// Package engine implements the M2c orchestration layer: the four relay
// components (Poller, Monitor, Dispatcher, RetryQueue) plus the lifecycle that
// starts and stops them. It is deliberately decoupled from the concrete
// pipeline implementation: engine depends only on the Pipeline interface below,
// which main injects (see docs/ARCHITECTURE-v4.md §7).
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/autoseedrelay/relay/internal/notifier"
	"github.com/autoseedrelay/relay/internal/qb"
	"github.com/autoseedrelay/relay/internal/source"
	"github.com/autoseedrelay/relay/internal/store"
)

// Pipeline is the download → clean → publish / cross-seed unit the engine
// drives, one seed at a time. It is defined locally (not imported from package
// pipeline) so the engine depends only on this interface; the concrete
// implementation is injected by main.
type Pipeline interface {
	Relay(ctx context.Context, seedID int64) error
}

// Config holds the engine's tuning knobs. Zero values fall back to the defaults
// in withDefaults.
type Config struct {
	PollInterval    time.Duration // source RSS poll cadence
	MonitorInterval time.Duration // qB monitor / retire cadence
	Workers         int           // pipeline worker-pool concurrency
}

const (
	defaultPollInterval    = 300 * time.Second
	defaultMonitorInterval = 30 * time.Second
	defaultWorkers         = 4
)

// withDefaults normalizes a Config so every field has a usable value.
func (c Config) withDefaults() Config {
	if c.PollInterval <= 0 {
		c.PollInterval = defaultPollInterval
	}
	if c.MonitorInterval <= 0 {
		c.MonitorInterval = defaultMonitorInterval
	}
	if c.Workers <= 0 {
		c.Workers = defaultWorkers
	}
	return c
}

// Engine is the M2c relay orchestrator. It owns a worker pool plus four
// background loops (poller, monitor, retry drain, notifier flusher).
type Engine struct {
	cfg   Config
	repo  *store.Repo
	pl    Pipeline
	qbMgr *qb.Manager
	notif *notifier.Router
	log   *slog.Logger

	// lifecycle
	running     atomic.Bool
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	lifecycleMu sync.Mutex // serializes Start/Stop

	// worker pool: seed ids waiting for a pipeline run.
	jobs chan int64

	// components
	retry      *RetryQueue
	dispatcher *Dispatcher

	// poller per-source backoff state, keyed by source id.
	pollMu    sync.Mutex
	pollState map[int64]*sourceState

	// monitor: guards the "all qB offline" critical against re-notify spam.
	qbAllOffline atomic.Bool

	// monitor: per-instance disk state ("ok"/"low"/"critical"), keyed by qB
	// name. It de-duplicates the critical disk notification + activity log
	// across monitor rounds; after a restart the map is empty and a re-notify
	// is acceptable.
	diskMu    sync.Mutex
	diskState map[string]string

	// Injectable seams (used by tests; defaults are production values).
	now        func() time.Time
	backoff    func(attempt int) time.Duration
	httpClient *http.Client
	fetchRSS   func(ctx context.Context, url string, client *http.Client) ([]source.RssItem, error)
}

// New builds an Engine. pl may be nil while the pipeline package is not yet
// wired (main.go TODO(M2c)); a nil pipeline turns Relay into a logged no-op so
// the rest of the engine still runs and is observable via /health.
func New(cfg Config, repo *store.Repo, pl Pipeline, qbMgr *qb.Manager, notif *notifier.Router) *Engine {
	cfg = cfg.withDefaults()
	if qbMgr == nil {
		qbMgr = qb.NewManager()
	}
	e := &Engine{
		cfg:        cfg,
		repo:       repo,
		pl:         pl,
		qbMgr:      qbMgr,
		notif:      notif,
		log:        slog.Default(),
		jobs:       make(chan int64, cfg.Workers),
		pollState:  make(map[int64]*sourceState),
		diskState:  make(map[string]string),
		now:        time.Now,
		backoff:    source.DefaultBackoff,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		fetchRSS:   source.FetchRSS,
	}
	e.dispatcher = NewDispatcher(repo, qbMgr)
	e.retry = NewRetryQueue(e.now)
	return e
}

// SetClock overrides the engine's time source (tests inject a controllable
// clock). It re-points the retry queue and the notifier router where relevant.
func (e *Engine) SetClock(fn func() time.Time) {
	if fn == nil {
		return
	}
	e.now = fn
	e.retry.SetClock(fn)
}

// SetBackoff overrides the source-poll failure backoff (tests).
func (e *Engine) SetBackoff(fn func(attempt int) time.Duration) {
	if fn != nil {
		e.backoff = fn
	}
}

// SetHTTPClient overrides the HTTP client used for RSS fetches (tests).
func (e *Engine) SetHTTPClient(c *http.Client) {
	if c != nil {
		e.httpClient = c
	}
}

// SetFetchRSS overrides the RSS fetch function (tests / offline).
func (e *Engine) SetFetchRSS(fn func(ctx context.Context, url string, client *http.Client) ([]source.RssItem, error)) {
	if fn != nil {
		e.fetchRSS = fn
	}
}

// Dispatcher exposes the qB selection strategy to callers (e.g. the pipeline).
func (e *Engine) Dispatcher() *Dispatcher { return e.dispatcher }

// SetPipeline replaces the pipeline after New, so main can build the engine
// first (to obtain its dispatcher) and then inject the concrete pipeline that
// depends on that dispatcher. It must be called before Start; calling it after
// Start races the worker pool's reads of the pipeline.
func (e *Engine) SetPipeline(pl Pipeline) { e.pl = pl }

// SelectQB is a convenience passthrough to the embedded Dispatcher.
func (e *Engine) SelectQB(ctx context.Context, opts DispatchOpts) (string, error) {
	return e.dispatcher.SelectQB(ctx, opts)
}

// Running reports whether the engine is currently started.
func (e *Engine) Running() bool { return e.running.Load() }

// Status returns a small snapshot used by the /health endpoint.
func (e *Engine) Status() map[string]any {
	return map[string]any{
		"running":          e.Running(),
		"workers":          e.cfg.Workers,
		"poll_interval":    e.cfg.PollInterval.String(),
		"monitor_interval": e.cfg.MonitorInterval.String(),
	}
}

// Start begins the background loops. It rebuilds the retry queue from the DB,
// launches the worker pool and the four loops, and returns once they are
// running. ctx is the parent lifetime; Stop cancels the derived context.
// Start/Stop are serialized by lifecycleMu; the jobs channel is rebuilt on
// every Start (old buffered jobs are discarded).
func (e *Engine) Start(ctx context.Context) error {
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()

	if e.running.Load() {
		return errAlreadyRunning
	}
	e.running.Store(true)

	// Rebuild the jobs channel so stale buffered ids from a previous run are
	// dropped rather than replayed into the new worker pool.
	e.jobs = make(chan int64, e.cfg.Workers)

	ectx, cancel := context.WithCancel(ctx)
	e.cancel = cancel

	e.rebuildRetryQueue(ectx)

	for i := 0; i < e.cfg.Workers; i++ {
		e.wg.Add(1)
		go e.workerLoop(ectx)
	}
	e.wg.Add(1)
	go e.pollLoop(ectx)
	e.wg.Add(1)
	go e.monitorLoop(ectx)
	e.wg.Add(1)
	go e.retryLoop(ectx)

	// Fourth loop: the notifier router's aggregation flusher. It manages its own
	// goroutine keyed on the derived context, so it exits on Stop's cancel.
	if e.notif != nil {
		e.notif.Start(ectx, 0)
	}

	e.log.Info("engine started",
		"workers", e.cfg.Workers,
		"poll_interval", e.cfg.PollInterval,
		"monitor_interval", e.cfg.MonitorInterval,
	)
	return nil
}

// Stop gracefully shuts the engine down: cancels the derived context, flushes
// pending notifications, and waits for all tracked goroutines to return. The
// running flag is cleared only after wg.Wait() returns, so a concurrent Start
// during shutdown is still refused; Start/Stop are serialized by lifecycleMu.
func (e *Engine) Stop(ctx context.Context) error {
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()

	if !e.running.Load() {
		return nil
	}
	if e.cancel != nil {
		e.cancel()
	}
	if e.notif != nil {
		e.notif.Flush(ctx)
		e.notif.Stop()
	}
	e.wg.Wait()
	e.running.Store(false)
	e.log.Info("engine stopped")
	return nil
}

// enqueue hands a seed id to the worker pool, blocking (with ctx awareness)
// until a worker is available or the engine shuts down.
func (e *Engine) enqueue(ctx context.Context, seedID int64) {
	select {
	case e.jobs <- seedID:
	case <-ctx.Done():
	}
}

// workerLoop consumes the jobs channel and runs each seed through the pipeline.
func (e *Engine) workerLoop(ctx context.Context) {
	defer e.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-e.jobs:
			e.submitJob(ctx, id, 0)
		}
	}
}

// strategy returns the single strategy row, falling back to defaults on any
// error so the engine degrades to safe, documented values.
func (e *Engine) strategy(ctx context.Context) *store.Strategy {
	st, err := e.repo.GetStrategy(ctx)
	if err != nil || st == nil {
		return defaultStrategy()
	}
	return st
}

// defaultStrategy mirrors the schema defaults in migrations/00001_init.sql.
func defaultStrategy() *store.Strategy {
	return &store.Strategy{
		ID:                  1,
		RetireSeeders:       10,
		RetireMinutes:       60,
		RetireRatioEnabled:  0,
		RetireRatio:         0,
		RetireMode:          "and",
		DispatchMode:        "priority",
		RetryMax:            3,
		DiskLowGB:           50,
		DiskCriticalGB:      20,
		LowSpeedKbps:        100,
		LowSpeedDurationSec: 600,
		LowSpeedAction:      "abort",
	}
}

// notify delivers one notification through the router, if one is wired. event
// is the action tag (e.g. "retire", "disk", "low_speed") the router uses to
// aggregate warning/info notifications per (instance, tier, event).
func (e *Engine) notify(ctx context.Context, level notifier.Level, title, body, event string) {
	if e.notif == nil {
		return
	}
	_ = e.notif.Notify(ctx, level, notifier.Message{Title: title, Body: body, Event: event})
}

// ResendSeed re-queues a seed for relay. fullRerun=false resets the seed's retry
// state (retry_count=0, status=retry, error cleared) and re-enters the retry
// queue; fullRerun=true additionally deletes every relay record and replica for
// the seed so it is re-run from scratch (a fresh publish + cross-seed, not an
// idempotent retry).
//
// This signature is a contract depended on by the API domain and must not
// change.
func (e *Engine) ResendSeed(ctx context.Context, seedID int64, fullRerun bool) error {
	if _, err := e.repo.GetSeedByID(ctx, seedID); err != nil {
		return fmt.Errorf("engine: resend seed %d: %w", seedID, err)
	}

	if fullRerun {
		// The store domain owns relay_records and exposes no dedicated bulk-delete
		// method, so the engine deletes the seed's records directly through the
		// repo's raw handle. Replicas are removed via the existing DeleteReplica.
		if _, err := e.repo.DB().ExecContext(ctx,
			`DELETE FROM relay_records WHERE seed_id = ?`, seedID); err != nil {
			return fmt.Errorf("engine: resend seed %d: delete records: %w", seedID, err)
		}
		reps, err := e.repo.ListReplicas(ctx, seedID)
		if err != nil {
			return fmt.Errorf("engine: resend seed %d: list replicas: %w", seedID, err)
		}
		for _, rep := range reps {
			if err := e.repo.DeleteReplica(ctx, rep.ID); err != nil {
				return fmt.Errorf("engine: resend seed %d: delete replica %d: %w", seedID, rep.ID, err)
			}
		}
	}

	// Reset retry state and re-enter the retry queue (retry number 1 → the first
	// backoff step, 60s). retry_count is reset directly since no repo method
	// sets it to an absolute value.
	if _, err := e.repo.DB().ExecContext(ctx,
		`UPDATE seeds SET retry_count = 0, status = 'retry', error = '', updated_at = unixepoch() WHERE id = ?`,
		seedID); err != nil {
		return fmt.Errorf("engine: resend seed %d: reset retry state: %w", seedID, err)
	}
	e.retry.Enqueue(seedID, 1)
	return nil
}

// errAlreadyRunning is returned by Start when the engine is already running.
var errAlreadyRunning = errAlreadyRunningError{}

type errAlreadyRunningError struct{}

func (errAlreadyRunningError) Error() string { return "engine: already running" }
