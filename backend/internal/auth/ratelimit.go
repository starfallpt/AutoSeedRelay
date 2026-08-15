package auth

import (
	"sync"
	"time"
)

const (
	loginLimitPerMinute = 5
	loginWindow         = time.Minute
)

// rateLimiter is a fixed-window, per-key limiter. It is safe for concurrent use
// and takes its clock via injection so tests can advance time deterministically.
type rateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	now     func() time.Time
	windows map[string]*rateWindow
}

type rateWindow struct {
	start time.Time
	count int
}

func newRateLimiter(limit int, window time.Duration, now func() time.Time) *rateLimiter {
	return &rateLimiter{
		limit:   limit,
		window:  window,
		now:     now,
		windows: make(map[string]*rateWindow),
	}
}

// allow records an attempt for key and reports whether it is under the limit.
// When the limit is exceeded it also returns the time remaining until the window
// resets, so the caller can emit a Retry-After header.
func (r *rateLimiter) allow(key string) (allowed bool, retryAfter time.Duration) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.windows[key]
	if !ok || now.Sub(w.start) >= r.window {
		w = &rateWindow{start: now}
		r.windows[key] = w
	}
	if w.count >= r.limit {
		return false, r.window - now.Sub(w.start)
	}
	w.count++
	return true, 0
}
