package notifier

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const defaultAggregationWindow = 10 * time.Minute

// routedInstance is one registered notifier instance plus its tier
// subscriptions and its dedicated circuit breaker.
type routedInstance struct {
	name     string
	notifier Notifier
	breaker  *Breaker
	tiers    map[Level]bool
}

// aggregate collects warning/info events for one (instance, tier) inside a
// single aggregation window.
type aggregate struct {
	instance string
	level    Level
	count    int
	lines    []string
	deadline time.Time
}

func (a *aggregate) toMessage() Message {
	return Message{
		Title: fmt.Sprintf("%d 条 %s", a.count, string(a.level)),
		Body:  strings.Join(a.lines, "\n"),
		Level: a.level,
	}
}

// Router holds the instance × tier subscription matrix and drives delivery:
// critical is dispatched immediately, warning/info are buffered into a 10
// minute per-(instance, tier) aggregation window and flushed in batch.
type Router struct {
	mu        sync.Mutex
	instances map[string]*routedInstance
	window    time.Duration
	now       func() time.Time
	pending   map[string]*aggregate

	// flusher lifecycle (Start/Stop).
	wg      sync.WaitGroup
	started bool
	cancel  context.CancelFunc
}

// RouterOption configures a Router.
type RouterOption func(*Router)

// WithRouterClock sets the time source used for the aggregation window.
func WithRouterClock(fn func() time.Time) RouterOption {
	return func(r *Router) { r.now = fn }
}

// WithAggregationWindow overrides the aggregation window duration.
func WithAggregationWindow(d time.Duration) RouterOption {
	return func(r *Router) { r.window = d }
}

// NewRouter builds an empty Router with a 10-minute aggregation window.
func NewRouter(opts ...RouterOption) *Router {
	r := &Router{
		instances: map[string]*routedInstance{},
		window:    defaultAggregationWindow,
		now:       time.Now,
		pending:   map[string]*aggregate{},
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Add registers a notifier instance under a name and the tiers it subscribes
// to. Every instance gets its own circuit breaker sharing the router clock.
func (r *Router) Add(name string, n Notifier, levels ...Level) {
	inst := &routedInstance{
		name:     name,
		notifier: n,
		breaker:  NewBreaker(WithBreakerClock(r.now)),
		tiers:    map[Level]bool{},
	}
	for _, l := range levels {
		inst.tiers[l] = true
	}
	r.mu.Lock()
	r.instances[name] = inst
	r.mu.Unlock()
}

// Notify delivers a message according to its level. Critical is sent directly
// to every subscribed instance and returns any delivery errors joined
// together. Warning/info are merged into the current aggregation window (one
// per instance × tier) and return nil; expired windows are flushed lazily.
func (r *Router) Notify(ctx context.Context, level Level, msg Message) error {
	msg.Level = level
	if level == LevelCritical {
		return r.dispatch(ctx, level, msg)
	}
	return r.buffer(ctx, level, msg)
}

// Flush immediately flushes every pending aggregation window. It is used on
// shutdown and in tests.
func (r *Router) Flush(ctx context.Context) {
	r.mu.Lock()
	var all []*aggregate
	for key, agg := range r.pending {
		all = append(all, agg)
		delete(r.pending, key)
	}
	r.mu.Unlock()

	sort.Slice(all, func(i, j int) bool { return all[i].instance < all[j].instance })
	for _, agg := range all {
		if inst := r.lookup(agg.instance); inst != nil {
			r.send(ctx, inst, agg.toMessage())
		}
	}
}

// Start runs a background flusher until Stop is called (or ctx is cancelled).
// interval defaults to half the aggregation window. Start is idempotent: a
// second call while the flusher is already running is a no-op. The flusher
// goroutine is tracked in the router's WaitGroup; Stop cancels it and waits
// for it to exit.
func (r *Router) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = r.window / 2
	}

	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return
	}
	r.started = true
	fctx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.wg.Add(1)
	r.mu.Unlock()

	go func() {
		defer r.wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-fctx.Done():
				return
			case <-t.C:
				r.flushExpired(fctx)
			}
		}
	}()
}

// Stop cancels the flusher goroutine (if any) and waits for it to exit. It is
// safe to call when Start was never called, and after Stop a subsequent Start
// launches a fresh flusher.
func (r *Router) Stop() {
	r.mu.Lock()
	if !r.started {
		r.mu.Unlock()
		return
	}
	cancel := r.cancel
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	r.wg.Wait()

	r.mu.Lock()
	r.started = false
	r.cancel = nil
	r.mu.Unlock()
}

func (r *Router) dispatch(ctx context.Context, level Level, msg Message) error {
	r.mu.Lock()
	var targets []*routedInstance
	for _, inst := range r.instances {
		if inst.tiers[level] {
			targets = append(targets, inst)
		}
	}
	r.mu.Unlock()

	var errs []error
	for _, inst := range targets {
		if err := r.send(ctx, inst, msg); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", inst.name, err))
		}
	}
	return errors.Join(errs...)
}

func (r *Router) buffer(ctx context.Context, level Level, msg Message) error {
	r.mu.Lock()
	now := r.now()
	for _, inst := range r.instances {
		if !inst.tiers[level] {
			continue
		}
		key := inst.name + "|" + string(level)
		agg := r.pending[key]
		if agg == nil {
			agg = &aggregate{
				instance: inst.name,
				level:    level,
				deadline: now.Add(r.window),
			}
			r.pending[key] = agg
		}
		agg.count++
		agg.lines = append(agg.lines, msg.String())
	}
	r.mu.Unlock()

	r.flushExpired(ctx)
	return nil
}

func (r *Router) flushExpired(ctx context.Context) {
	r.mu.Lock()
	now := r.now()
	var due []*aggregate
	for key, agg := range r.pending {
		if !now.Before(agg.deadline) {
			due = append(due, agg)
			delete(r.pending, key)
		}
	}
	r.mu.Unlock()

	for _, agg := range due {
		if inst := r.lookup(agg.instance); inst != nil {
			r.send(ctx, inst, agg.toMessage())
		}
	}
}

func (r *Router) send(ctx context.Context, inst *routedInstance, msg Message) error {
	return inst.breaker.Do(func() error {
		return inst.notifier.Send(ctx, msg)
	})
}

func (r *Router) lookup(name string) *routedInstance {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.instances[name]
}
