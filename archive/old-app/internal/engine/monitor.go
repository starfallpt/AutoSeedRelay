package engine

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/autoseedrelay/go-relay/internal/config"
	"github.com/autoseedrelay/go-relay/internal/qb"
	"github.com/autoseedrelay/go-relay/internal/strategy"
)

// monitorLoop periodically checks seeding status, disk/speed metrics,
// retire conditions, and handles the exception queue.
func (e *Engine) monitorLoop() {
	defer e.wg.Done()

	ticker := time.NewTicker(e.monitorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			e.runMonitor()
		}
	}
}

func (e *Engine) runMonitor() {
	if !e.running.Load() {
		return
	}

	e.checkQBConnection()
	e.checkSeedingStatus()
	e.checkDiskSpace()
	e.checkLowSpeed()
	e.checkRetireConditions()

	e.statsMu.Lock()
	e.stats.UptimeSeconds = int64(time.Since(e.started).Seconds())
	e.statsMu.Unlock()
}

func (e *Engine) checkQBConnection() {
	if e.qb == nil {
		e.stats.QBConnected = false
		return
	}
	err := e.qb.Login()
	e.stats.QBConnected = (err == nil)
	if !e.stats.QBConnected {
		slog.Warn("engine: qb connection lost", "error", err)
	}
}

func (e *Engine) checkSeedingStatus() {
	if e.qb == nil || !e.stats.QBConnected {
		return
	}

	infos, err := e.qb.Info()
	if err != nil {
		slog.Error("engine: qb info failed", "error", err)
		return
	}

	seeding := 0
	downloading := 0
	for _, t := range infos {
		state, _ := t["state"].(string)
		switch state {
		case "uploading", "stalledUP", "forcedUP":
			seeding++
		case "downloading", "stalledDL", "forcedDL":
			downloading++
		}

		if hash, ok := t["hash"].(string); ok {
			e.updateTorrentStats(hash, t)
		}
	}

	e.statsMu.Lock()
	e.stats.CurrentSeeding = seeding
	e.statsMu.Unlock()

	slog.Debug("engine: seeding status", "seeding", seeding, "downloading", downloading, "total", len(infos))
}

func (e *Engine) updateTorrentStats(hash string, t map[string]any) {
	has, err := e.store.Has(hash)
	if err != nil || !has {
		return
	}

	if qb.IsCompletedSeeding(t) {
		state, _ := t["state"].(string)
		if err := e.store.MarkStatus(hash, "seeded", map[string]any{
			"error": fmt.Sprintf("qb_state: %s", state),
		}); err != nil {
			slog.Warn("engine: mark status failed", "hash", hash[:12], "status", "seeded", "error", err)
		}
	}
}

func (e *Engine) checkDiskSpace() {
	if e.qb == nil || !e.stats.QBConnected {
		return
	}

	infos, err := e.qb.Info()
	if err != nil {
		return
	}

	var totalSize int64
	for _, t := range infos {
		if sz, ok := t["size"].(float64); ok {
			totalSize += int64(sz)
		}
	}

	e.cfgMu.RLock()
	diskLow := e.cfg.Monitor.DiskLowGB
	diskCritical := e.cfg.Monitor.DiskCriticalGB
	e.cfgMu.RUnlock()

	e.statsMu.Lock()
	e.stats.DiskTotalGB = 500.0
	e.stats.DiskFreeGB = e.stats.DiskTotalGB - float64(totalSize)/(1024*1024*1024)
	if e.stats.DiskFreeGB < 0 {
		e.stats.DiskFreeGB = 0
	}

	if e.stats.DiskFreeGB < diskCritical {
		slog.Error("engine: disk critical",
			"free_gb", fmt.Sprintf("%.1f", e.stats.DiskFreeGB),
			"critical_gb", diskCritical,
		)
	} else if e.stats.DiskFreeGB < diskLow {
		slog.Warn("engine: disk low",
			"free_gb", fmt.Sprintf("%.1f", e.stats.DiskFreeGB),
			"low_gb", diskLow,
		)
	}
	e.statsMu.Unlock()
}

func (e *Engine) checkLowSpeed() {
	if e.qb == nil || !e.stats.QBConnected {
		return
	}

	e.cfgMu.RLock()
	threshold := e.cfg.Strategy.LowSpeedKBps
	duration := e.cfg.Strategy.LowSpeedDurationSeconds
	action := e.cfg.Strategy.LowSpeedAction
	e.cfgMu.RUnlock()

	if threshold <= 0 || action != "abort" {
		return
	}

	infos, err := e.qb.Info()
	if err != nil {
		return
	}

	for _, t := range infos {
		state, _ := t["state"].(string)
		if state != "downloading" && state != "stalledDL" && state != "forcedDL" {
			continue
		}

		dlSpeed, _ := t["dlspeed"].(float64)
		dlSpeedKBps := int64(dlSpeed) / 1024

		if dlSpeedKBps < int64(threshold) {
			hash, _ := t["hash"].(string)
			if addedOn, ok := t["added_on"].(float64); ok {
				elapsed := time.Now().Unix() - int64(addedOn)
				if float64(elapsed) > duration {
					slog.Warn("engine: low speed detected, aborting",
						"hash", hash[:12],
						"speed_kbps", dlSpeedKBps,
						"threshold", threshold,
						"elapsed", elapsed,
					)
					_ = e.qb.Delete(hash, false)
					if err := e.store.MarkStatus(hash, "failed", map[string]any{
						"error": fmt.Sprintf("low_speed_abort: %d KB/s < %d KB/s for %ds", dlSpeedKBps, threshold, elapsed),
					}); err != nil {
						slog.Warn("engine: mark status failed", "hash", hash[:12], "status", "failed", "error", err)
					}
				}
			}
		}
	}
}

func (e *Engine) checkRetireConditions() {
	if e.qb == nil || !e.stats.QBConnected {
		return
	}

	infos, err := e.qb.Info()
	if err != nil {
		return
	}

	e.cfgMu.RLock()
	retire := e.retire
	deleteFiles := e.cfg.Retire.DeleteFiles
	e.cfgMu.RUnlock()

	for _, t := range infos {
		if !qb.IsCompletedSeeding(t) {
			continue
		}

		hash, _ := t["hash"].(string)
		has, err := e.store.Has(hash)
		if err != nil || !has {
			continue
		}

		ratio, _ := t["ratio"].(float64)
		numSeeds, _ := t["num_seeds"].(float64)
		addedOn, _ := t["added_on"].(float64)

		rec := &strategy.SeedRecord{
			InfoHash:  hash,
			Seeders:   int(numSeeds),
			Ratio:     ratio,
			AddedOn:   time.Unix(int64(addedOn), 0),
			Completed: true,
		}

		decision := retire.ShouldRetire(rec)
		if decision.ShouldRetire {
			slog.Info("engine: retiring seed",
				"hash", hash[:12],
				"reason", decision.Reason,
			)

			if err := e.qb.Stop(hash); err != nil {
				slog.Warn("engine: retire stop failed", "hash", hash[:12], "error", err)
			}
			if err := e.qb.Delete(hash, deleteFiles); err != nil {
				slog.Warn("engine: retire delete failed", "hash", hash[:12], "error", err)
			}
			if err := e.store.MarkStatus(hash, "skipped", map[string]any{
				"error": "auto_retired: " + decision.Reason,
			}); err != nil {
				slog.Warn("engine: mark status failed", "hash", hash[:12], "status", "skipped", "error", err)
			}
		}
	}
}

// qbAddOptions builds qB add options from engine config.
func qbAddOptions(cfg *config.AppConfig) qb.AddOptions {
	return qb.AddOptions{
		Savepath: cfg.WorkDir,
		Category: "relay",
		Paused:   false,
	}
}
