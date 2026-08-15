package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/autoseedrelay/relay/internal/notifier"
	"github.com/autoseedrelay/relay/internal/source"
)

// retryBackoffs is the fixed exponential backoff schedule between retries
// (60s / 300s / 900s). The number of retries is bounded by strategy.RetryMax
// (default 3), so the schedule is at most RetryMax entries long; a larger
// RetryMax reuses the final entry.
var retryBackoffs = []time.Duration{
	60 * time.Second,
	300 * time.Second,
	900 * time.Second,
}

// retryItem is one scheduled pipeline retry.
type retryItem struct {
	seedID  int64
	retryNo int // 1-based retry number (1 .. RetryMax)
	dueAt   time.Time
}

// RetryQueue is an in-memory delay queue of failed seeds awaiting re-submission
// to the pipeline. It is driven by an injectable clock so tests can advance
// time deterministically.
type RetryQueue struct {
	now     func() time.Time
	backoff []time.Duration

	mu    sync.Mutex
	items []retryItem
	wake  chan struct{}
}

// NewRetryQueue builds an empty queue with the default backoff schedule.
func NewRetryQueue(now func() time.Time) *RetryQueue {
	if now == nil {
		now = time.Now
	}
	return &RetryQueue{
		now:     now,
		backoff: retryBackoffs,
		wake:    make(chan struct{}, 1),
	}
}

// SetClock re-points the queue's time source.
func (q *RetryQueue) SetClock(fn func() time.Time) {
	if fn == nil {
		return
	}
	q.mu.Lock()
	q.now = fn
	q.mu.Unlock()
}

// backoffFor returns the delay before retry number retryNo (1-based).
func (q *RetryQueue) backoffFor(retryNo int) time.Duration {
	if retryNo < 1 {
		retryNo = 1
	}
	idx := retryNo - 1
	if idx >= len(q.backoff) {
		idx = len(q.backoff) - 1
	}
	return q.backoff[idx]
}

// Enqueue schedules a retry for seedID at now + backoffFor(retryNo).
func (q *RetryQueue) Enqueue(seedID int64, retryNo int) {
	q.mu.Lock()
	q.items = append(q.items, retryItem{
		seedID:  seedID,
		retryNo: retryNo,
		dueAt:   q.now().Add(q.backoffFor(retryNo)),
	})
	q.mu.Unlock()
	q.signal()
}

func (q *RetryQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Due removes and returns every item whose dueAt is not after now, preserving
// insertion order.
func (q *RetryQueue) Due() []retryItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	var due, rest []retryItem
	for _, it := range q.items {
		if it.dueAt.After(now) {
			rest = append(rest, it)
		} else {
			due = append(due, it)
		}
	}
	q.items = rest
	return due
}

// NextWait returns how long until the earliest due item (0 when the queue is
// empty or an item is already due).
func (q *RetryQueue) NextWait() time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return 0
	}
	now := q.now()
	min := q.items[0].dueAt.Sub(now)
	for _, it := range q.items[1:] {
		if d := it.dueAt.Sub(now); d < min {
			min = d
		}
	}
	if min < 0 {
		return 0
	}
	return min
}

// retryLoop drains the retry queue, re-submitting due seeds to the pipeline.
func (e *Engine) retryLoop(ctx context.Context) {
	defer e.wg.Done()
	for {
		for _, it := range e.retry.Due() {
			e.submitJob(ctx, it.seedID, it.retryNo)
		}

		wait := e.retry.NextWait()
		if wait <= 0 {
			// Queue empty: block until a wake or shutdown.
			select {
			case <-ctx.Done():
				return
			case <-e.retry.wake:
			}
			continue
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-e.retry.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// rebuildRetryQueue re-enqueues every seed whose status is "retry" so a restart
// recovers in-flight retries (ARCHITECTURE-v4 §7). The seed's retry_count is
// the retry number to run next, and the backoff is re-applied from now.
func (e *Engine) rebuildRetryQueue(ctx context.Context) {
	seeds, err := e.repo.ListSeedsByStatus(ctx, "retry")
	if err != nil {
		e.log.Error("retry: rebuild list failed", "error", err)
		return
	}
	for _, sd := range seeds {
		retryNo := int(sd.RetryCount)
		if retryNo < 1 {
			retryNo = 1
		}
		e.retry.Enqueue(sd.ID, retryNo)
		e.log.Info("retry: rebuilt from db", "seed_id", sd.ID, "retry_no", retryNo)
	}
}

// partialFailure is the structural shape of pipeline.PartialFailure. The engine
// detects it with errors.As so it never imports the pipeline package.
type partialFailure interface {
	error
	IsPartial() bool
	FailedNames() []string
}

// submitJob runs one pipeline attempt (retryNo == 0 is the initial attempt,
// 1..RetryMax are retries) and routes failures into the retry queue or, once
// RetryMax is exceeded, into the terminal state with a critical notification.
// A partial failure (some targets succeeded) is retryable; on exhaustion the
// seed is kept in "seeding" (preserving the successes) and only the failed
// targets are listed in the critical notification.
func (e *Engine) submitJob(ctx context.Context, seedID int64, retryNo int) {
	if e.pl == nil {
		e.log.Warn("pipeline not wired; seed left pending", "seed_id", seedID)
		return
	}

	err := e.pl.Relay(ctx, seedID)
	if err == nil {
		e.log.Info("seed relayed", "seed_id", seedID)
		return
	}

	var pf partialFailure
	isPartial := errors.As(err, &pf) && pf.IsPartial()

	st := e.strategy(ctx)
	maxRetries := int(st.RetryMax)
	if maxRetries <= 0 {
		maxRetries = 3
	}

	if berr := e.repo.BumpRetry(ctx, seedID); berr != nil {
		e.log.Warn("retry: bump retry count", "seed_id", seedID, "error", berr)
	}

	next := retryNo + 1
	// 统一脱敏:进入 seeds.error / 通知 / slog 的错误串必须剥离凭据(如 passkey)。
	errMsg := source.RedactError(err.Error())
	if next <= maxRetries {
		if serr := e.repo.UpdateSeedStatus(ctx, seedID, "retry", errMsg); serr != nil {
			e.log.Warn("retry: mark seed retry", "seed_id", seedID, "error", serr)
		}
		e.retry.Enqueue(seedID, next)
		e.log.Warn("retry: scheduled", "seed_id", seedID, "retry_no", next, "error", errMsg)
		return
	}

	if isPartial {
		// Partial failure exhausted: keep the successful targets (seed stays
		// "seeding") and surface only the failed targets critically.
		if ferr := e.repo.UpdateSeedStatus(ctx, seedID, "seeding", errMsg); ferr != nil {
			e.log.Warn("retry: mark seed seeding (partial)", "seed_id", seedID, "error", ferr)
		}
		e.notify(ctx, notifier.LevelCritical, "部分目标重试耗尽",
			fmt.Sprintf("seed_id=%d 重试 %d 次后仍有目标失败(已保留成功目标): %s", seedID, maxRetries, strings.Join(pf.FailedNames(), "; ")))
		e.log.Error("retry: partial exhausted", "seed_id", seedID, "retries", maxRetries, "error", errMsg)
		return
	}

	if ferr := e.repo.UpdateSeedStatus(ctx, seedID, "failed", errMsg); ferr != nil {
		e.log.Warn("retry: mark seed failed", "seed_id", seedID, "error", ferr)
	}
	e.notify(ctx, notifier.LevelCritical, "种子重试耗尽",
		fmt.Sprintf("seed_id=%d 重试 %d 次仍失败,已进入失败队列待手动重试: %s", seedID, maxRetries, errMsg))
	e.log.Error("retry: exhausted", "seed_id", seedID, "retries", maxRetries, "error", errMsg)
}
