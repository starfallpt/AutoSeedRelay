package notifier

import (
	"errors"
	"sync"
	"time"
)

// ErrBreakerOpen is returned by Breaker.Do when the circuit is open and the
// wrapped call is skipped without executing fn.
var ErrBreakerOpen = errors.New("notifier: circuit breaker open")

type breakerState uint8

const (
	stateClosed breakerState = iota
	stateOpen
	stateHalfOpen
)

const (
	defaultMaxFailures  = 5
	defaultOpenDuration = 10 * time.Minute
)

// Breaker is a per-instance circuit breaker. After maxFailures consecutive
// failures it opens for openDuration, skipping (and counting) every call during
// that window. Once the window elapses it allows a single half-open probe: a
// success closes the circuit and resets the failure count, a failure re-opens
// it immediately.
//
// The time source is injectable so tests can advance time deterministically.
type Breaker struct {
	mu           sync.Mutex
	state        breakerState
	failures     int
	skips        int64
	openedAt     time.Time
	probing      bool
	maxFailures  int
	openDuration time.Duration
	now          func() time.Time
}

// BreakerOption configures a Breaker.
type BreakerOption func(*Breaker)

// WithBreakerClock sets the time source used to decide open-window expiry.
func WithBreakerClock(fn func() time.Time) BreakerOption {
	return func(b *Breaker) { b.now = fn }
}

// WithBreakerThreshold overrides the consecutive-failure threshold.
func WithBreakerThreshold(n int) BreakerOption {
	return func(b *Breaker) { b.maxFailures = n }
}

// WithBreakerOpenDuration overrides how long the breaker stays open.
func WithBreakerOpenDuration(d time.Duration) BreakerOption {
	return func(b *Breaker) { b.openDuration = d }
}

// NewBreaker builds a Breaker with defaults (5 consecutive failures, 10m open).
func NewBreaker(opts ...BreakerOption) *Breaker {
	b := &Breaker{
		state:        stateClosed,
		maxFailures:  defaultMaxFailures,
		openDuration: defaultOpenDuration,
		now:          time.Now,
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

// Do runs fn under the breaker's state machine. It returns fn's error, or
// ErrBreakerOpen when the circuit is open and fn was skipped. A nil error from
// fn counts as a success and resets the consecutive-failure count.
func (b *Breaker) Do(fn func() error) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	switch b.state {
	case stateOpen:
		if now.Sub(b.openedAt) < b.openDuration {
			b.skips++
			return ErrBreakerOpen
		}
		b.state = stateHalfOpen
		b.probing = false
	case stateHalfOpen:
		if b.probing {
			// A probe is already in flight; keep skipping.
			b.skips++
			return ErrBreakerOpen
		}
		b.probing = true
	}

	err := fn()
	if err != nil {
		if b.state == stateHalfOpen {
			// The probe failed: re-open immediately with a fresh window.
			b.state = stateOpen
			b.openedAt = now
			b.probing = false
		} else {
			b.failures++
			if b.failures >= b.maxFailures {
				b.state = stateOpen
				b.openedAt = now
			}
		}
		return err
	}

	// Success closes the circuit (from closed or half-open) and resets the count.
	b.failures = 0
	b.state = stateClosed
	b.probing = false
	return nil
}

// SkipCount reports how many calls were skipped while the circuit was open.
func (b *Breaker) SkipCount() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.skips
}

// Failures reports the current consecutive-failure count.
func (b *Breaker) Failures() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failures
}

// State reports the breaker state as "closed", "open" or "half_open".
func (b *Breaker) State() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case stateOpen:
		return "open"
	case stateHalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}
