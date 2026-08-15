package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/autoseedrelay/relay/internal/notifier"
	"github.com/autoseedrelay/relay/internal/source"
	"github.com/autoseedrelay/relay/internal/store"
)

// maxSourceFailures is the number of consecutive RSS failures after which a
// source is auto-paused (BIZ-SPEC §3: cookie-expiry auto-pause). Transient
// failures below this threshold are retried with injected backoff.
const maxSourceFailures = 3

// sourceState tracks one source's consecutive-failure count and the earliest
// time its next poll is allowed (the injected backoff gate).
type sourceState struct {
	fails       int
	nextAllowed time.Time
}

// pollLoop polls every active source on cfg.PollInterval, running one cycle
// immediately on start (BIZ-SPEC §3: "启动立即一轮").
func (e *Engine) pollLoop(ctx context.Context) {
	defer e.wg.Done()

	e.poll(ctx)

	ticker := time.NewTicker(e.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.poll(ctx)
		}
	}
}

// poll performs one full cycle over every active source.
func (e *Engine) poll(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	sources, err := e.repo.GetActiveSources(ctx)
	if err != nil {
		e.log.Error("poll: list active sources", "error", err)
		return
	}
	for _, src := range sources {
		e.pollSource(ctx, src)
	}
}

// pollSource fetches and ingests one source's RSS, enforcing per-source
// backoff and auto-pause on consecutive failure.
func (e *Engine) pollSource(ctx context.Context, src *store.Source) {
	st := e.sourceState(src.ID)
	now := e.now()
	if st.nextAllowed.After(now) {
		// Still backing off from a previous failure.
		return
	}

	items, err := e.fetchRSS(ctx, src.RSSURL, e.httpClient)
	if err != nil {
		e.sourceFailed(ctx, src, st, err)
		return
	}

	// Success: reset the failure window.
	e.pollMu.Lock()
	st.fails = 0
	st.nextAllowed = time.Time{}
	e.pollMu.Unlock()

	for i := range items {
		e.ingestItem(ctx, src, &items[i])
	}
}

// ingestItem dedups a single RSS item against the seeds table and, when new,
// creates the seed and submits it to the pipeline worker pool.
func (e *Engine) ingestItem(ctx context.Context, src *store.Source, item *source.RssItem) {
	// Strategy filter (keywords + size) runs before dedup/create so filtered
	// items never create a seed row. Promotion filtering happens later, in the
	// pipeline (it needs the fetched detail).
	if !itemMatchesStrategy(item, e.strategy(ctx)) {
		e.log.Debug("poll: item filtered out by strategy",
			"source", src.Name, "title", truncate(item.Title, 60))
		return
	}

	ih := source.GuidToInfohash(item.GUID, item.Link, item.Title)

	// Permanent-tombstone dedup (M5): a hash that has ever been seen is skipped
	// even if its seeds row was later deleted by cleanup or lost to a stale
	// backup restore.
	seen, err := e.repo.HasSeen(ctx, src.Name, ih)
	if err != nil {
		e.log.Error("poll: dedup seen lookup failed", "source", src.Name, "hash", ih, "error", err)
		return
	}
	if seen {
		return
	}

	// Not seen yet: tombstone it permanently before deciding whether to create a
	// seed row. MarkSeen runs even when the seed row already exists, so a later
	// manual delete cannot resurrect the hash.
	if err := e.repo.MarkSeen(ctx, src.Name, ih); err != nil {
		e.log.Error("poll: mark seen failed", "source", src.Name, "hash", ih, "error", err)
		return
	}

	existing, err := e.repo.GetSeedByHash(ctx, src.Name, ih)
	switch {
	case err == nil && existing != nil:
		// Already known. This covers the "retired → permanently skip" rule: a
		// retired seed keeps its (source_site, info_hash) row, so it is never
		// re-created here. Only an explicit manual republish re-opens it.
		return
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		e.log.Error("poll: dedup lookup failed", "source", src.Name, "hash", ih, "error", err)
		return
	}

	size := int64(0)
	if item.Size != nil {
		size = *item.Size
	}
	seed := &store.Seed{
		SourceSite: src.Name,
		InfoHash:   ih,
		Title:      item.Title,
		Size:       size,
		Category:   item.CategoryName,
		Promotion:  "",
		SourceID:   src.ID,
		Status:     "discovered",
	}

	id, err := e.repo.CreateSeed(ctx, seed)
	if err != nil {
		e.log.Error("poll: create seed", "source", src.Name, "hash", ih, "error", err)
		return
	}

	if err := e.repo.AppendLog(ctx, "info", "discovered",
		fmt.Sprintf("source=%s hash=%s title=%q", src.Name, ih, truncate(item.Title, 80))); err != nil {
		e.log.Warn("poll: append discovered log", "error", err)
	}

	e.log.Info("poll: new seed discovered", "source", src.Name, "seed_id", id, "hash", ih)
	e.enqueue(ctx, id)
}

// sourceFailed records a source failure, applies injected backoff, and pauses
// the source (with a critical notification) once the consecutive threshold is
// reached.
func (e *Engine) sourceFailed(ctx context.Context, src *store.Source, st *sourceState, err error) {
	e.pollMu.Lock()
	st.fails++
	fails := st.fails
	e.pollMu.Unlock()

	// Persist the failure counter for the dashboard/health surface.
	if _, ierr := e.repo.IncSourceFail(ctx, src.ID); ierr != nil {
		e.log.Warn("poll: inc source fail", "source", src.Name, "error", ierr)
	}

	if fails >= maxSourceFailures {
		reason := fmt.Sprintf("连续 %d 次 RSS 抓取失败(可能 cookie 过期): %s", fails, source.RedactError(err.Error()))
		if perr := e.repo.PauseSource(ctx, src.ID, reason); perr != nil {
			e.log.Error("poll: pause source", "source", src.Name, "error", perr)
		}
		e.notify(ctx, notifier.LevelCritical, "源站已自动暂停",
			fmt.Sprintf("source=%s %s", src.Name, reason), "source_pause")

		e.pollMu.Lock()
		delete(e.pollState, src.ID)
		e.pollMu.Unlock()
		return
	}

	// Back off before the next attempt (attempt 0 → base, 1 → base*2, …).
	e.pollMu.Lock()
	st.nextAllowed = e.now().Add(e.backoff(fails - 1))
	e.pollMu.Unlock()

	e.log.Warn("poll: source rss failed (will retry)",
		"source", src.Name, "fails", fails, "next_allowed", st.nextAllowed, "error", source.RedactError(err.Error()))
}

// sourceState returns (creating on first use) the poll state for a source id.
func (e *Engine) sourceState(id int64) *sourceState {
	e.pollMu.Lock()
	defer e.pollMu.Unlock()
	st, ok := e.pollState[id]
	if !ok {
		st = &sourceState{}
		e.pollState[id] = st
	}
	return st
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// itemMatchesStrategy applies the strategy keyword + size filters to one RSS
// item (BIZ-SPEC §2/§6). An empty keyword list accepts all; min/max size are
// only enforced when the RSS item carries a known size.
func itemMatchesStrategy(item *source.RssItem, st *store.Strategy) bool {
	if item == nil || st == nil {
		return true
	}
	if kws := parseJSONStringList(st.Keywords); len(kws) > 0 {
		if !keywordHits(item.Title+"\n"+item.Description, kws) {
			return false
		}
	}
	if item.Size != nil {
		if st.MinSize > 0 && *item.Size < st.MinSize {
			return false
		}
		if st.MaxSize > 0 && *item.Size > st.MaxSize {
			return false
		}
	}
	return true
}

// keywordHits reports whether any keyword is a case-insensitive substring of
// haystack.
func keywordHits(haystack string, keywords []string) bool {
	low := strings.ToLower(haystack)
	for _, k := range keywords {
		if k = strings.TrimSpace(k); k != "" && strings.Contains(low, strings.ToLower(k)) {
			return true
		}
	}
	return false
}

// parseJSONStringList decodes a JSON string-array column (keywords / promotions).
func parseJSONStringList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" || raw == "null" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}
