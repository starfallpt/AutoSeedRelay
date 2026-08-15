package engine

import (
	"log/slog"
	"time"

	"github.com/autoseedrelay/go-relay/internal/source"
)

// cycleLoop is the main polling loop: fetch RSS from each source, filter
// items, check dedup, and queue new seeds for processing.
func (e *Engine) cycleLoop() {
	defer e.wg.Done()

	e.cfgMu.RLock()
	pollInterval := time.Duration(e.cfg.PollInterval) * time.Second
	e.cfgMu.RUnlock()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// Run one cycle immediately on start.
	e.runCycle()

	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			e.runCycle()
		}
	}
}

func (e *Engine) runCycle() {
	if !e.running.Load() {
		return
	}

	slog.Debug("engine: cycle start", "sources", len(e.sources))

	for name, sc := range e.sources {
		e.processSource(name, sc)
	}

	e.statsMu.Lock()
	e.stats.CyclesCompleted++
	e.stats.LastCycleTime = time.Now().UTC().Format(time.RFC3339)
	e.statsMu.Unlock()

	slog.Debug("engine: cycle complete")
}

func (e *Engine) processSource(name string, sc *source.SourceClient) {
	items, err := sc.FetchRSS()
	if err != nil {
		slog.Error("engine: RSS fetch failed", "source", name, "error", err)
		e.statsMu.Lock()
		e.stats.Errors++
		e.statsMu.Unlock()
		return
	}

	slog.Debug("engine: RSS fetched", "source", name, "count", len(items))

	e.cfgMu.RLock()
	filter := e.filter
	e.cfgMu.RUnlock()

	for i := range items {
		item := &items[i]

		// Apply filter rules.
		if !filter.MatchAll(item.CategoryName, item.Title, sizeOrZero(item.Size)) {
			continue
		}

		// Dedup check.
		ih := source.GuidToInfohash(item.GUID)
		has, err := e.store.Has(ih)
		if err != nil {
			slog.Error("engine: dedup check failed", "hash", ih, "error", err)
			continue
		}
		if has {
			continue
		}

		// Insert new seed.
		var srcSize int64
		if item.Size != nil {
			srcSize = *item.Size
		}

		_, err = e.store.Add(map[string]any{
			"info_hash":   ih,
			"rss_id":      item.ID,
			"title":       item.Title,
			"source_site": name,
			"source_size": srcSize,
		})
		if err != nil {
			slog.Error("engine: insert seed failed", "hash", ih, "error", err)
			continue
		}

		slog.Info("engine: new seed discovered",
			"source", name,
			"title", truncateStr(item.Title, 60),
			"hash", ih[:12],
		)

		// Process seed asynchronously, limited by max_concurrent semaphore.
		e.sem <- struct{}{} // acquire
		go func(ih string, item *source.RssItem, name string) {
			defer func() { <-e.sem }() // release
			e.processSeed(ih, item, name)
		}(ih, item, name)
	}
}

// processSeed handles a single seed through the pipeline: download, parse,
// add to qB, upload to target, cross-seed.
func (e *Engine) processSeed(infoHash string, item *source.RssItem, sourceName string) {
	sc, ok := e.sources[sourceName]
	if !ok {
		slog.Error("engine: source not found", "source", sourceName)
		return
	}

	// Mark as downloaded.
	if err := e.store.MarkStatus(infoHash, "downloaded", nil); err != nil {
		slog.Warn("engine: mark status failed", "hash", infoHash[:12], "status", "downloaded", "error", err)
	}

	e.cfgMu.RLock()
	torrentsDir := e.cfg.TorrentsDir
	cfgSnapshot := e.cfg
	e.cfgMu.RUnlock()

	// Download .torrent from source.
	torrentPath := torrentsDir + "/" + item.ID + ".torrent"
	okDownload, err := sc.DownloadTorrent(item, torrentPath)
	if err != nil || !okDownload {
		slog.Error("engine: download failed", "hash", infoHash[:12], "error", err)
		if err2 := e.store.MarkStatus(infoHash, "failed", map[string]any{
			"error": "download: " + errStr(err),
		}); err2 != nil {
			slog.Warn("engine: mark status failed", "hash", infoHash[:12], "status", "failed", "error", err2)
		}
		return
	}

	// Add to qB for seeding.
	if e.qb != nil && e.stats.QBConnected {
		_, err := e.qb.AddTorrentFile(torrentPath, qbAddOptions(cfgSnapshot))
		if err != nil {
			slog.Error("engine: qb add failed", "hash", infoHash[:12], "error", err)
			if err2 := e.store.MarkStatus(infoHash, "failed", map[string]any{
				"error": "qb_add: " + err.Error(),
			}); err2 != nil {
				slog.Warn("engine: mark status failed", "hash", infoHash[:12], "status", "failed", "error", err2)
			}
			return
		}
		if err := e.store.MarkStatus(infoHash, "added_to_qb", map[string]any{
			"qb_hash": infoHash,
		}); err != nil {
			slog.Warn("engine: mark status failed", "hash", infoHash[:12], "status", "added_to_qb", "error", err)
		}
	}

	slog.Info("engine: seed processed", "hash", infoHash[:12], "title", truncateStr(item.Title, 60))
}

func sizeOrZero(s *int64) int64 {
	if s == nil {
		return 0
	}
	return *s
}

func errStr(err error) string {
	if err == nil {
		return "unknown"
	}
	return err.Error()
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
