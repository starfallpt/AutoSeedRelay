// Package engine implements the v3 core relay engine: the main scheduling loop,
// monitoring, and state management. It wires together config, store, qB client,
// source clients, target adapters, and strategy.
package engine

import (
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/autoseedrelay/go-relay/internal/config"
	"github.com/autoseedrelay/go-relay/internal/qb"
	"github.com/autoseedrelay/go-relay/internal/source"
	"github.com/autoseedrelay/go-relay/internal/store"
	"github.com/autoseedrelay/go-relay/internal/strategy"
	"github.com/autoseedrelay/go-relay/internal/targets"
	"github.com/autoseedrelay/go-relay/internal/web"
)

// Engine is the core relay engine that orchestrates RSS polling, seed
// processing, monitoring, and retire decisions.
type Engine struct {
	cfg      *config.AppConfig
	store    *store.RelayStore
	qb       *qb.QBittorrent
	filter   *strategy.Filter
	retire   *strategy.RetirePolicy
	cfgMu    sync.RWMutex // protects cfg, filter, retire

	// Source clients keyed by site name.
	sources map[string]*source.SourceClient

	// Running state.
	running atomic.Bool
	started time.Time

	// Stats (atomic for lock-free reads).
	stats   Stats
	statsMu sync.RWMutex

	// Web server (in-process).
	webServer *web.Server

	// Control channels.
	stopCh chan struct{}
	wg     sync.WaitGroup
	sem    chan struct{} // limits concurrent processSeed goroutines

	// Monitor interval.
	monitorInterval time.Duration
}

// Stats holds accumulated engine statistics.
type Stats struct {
	TotalPublished   int64 `json:"total_published"`
	TotalCrossSeeded int64 `json:"total_cross_seeded"`
	CurrentSeeding   int   `json:"current_seeding"`
	TodayPublished   int   `json:"today_published"`
	TodayCrossSeeded int   `json:"today_cross_seeded"`
	DiskFreeGB       float64 `json:"disk_free_gb"`
	DiskTotalGB      float64 `json:"disk_total_gb"`
	QBConnected      bool    `json:"qb_connected"`
	UptimeSeconds    int64   `json:"uptime_seconds"`
	CyclesCompleted  int64   `json:"cycles_completed"`
	LastCycleTime    string  `json:"last_cycle_time"`
	Errors           int64   `json:"errors"`
}

// New creates a new Engine from the given config.
func New(cfg *config.AppConfig) (*Engine, error) {
	// Open store.
	s, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("engine: open store: %w", err)
	}

	// Create qB client.
	qbClient, err := qb.NewQBittorrent(cfg.QB.URL(), cfg.QB.Username, cfg.QB.Password, 30)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("engine: create qb client: %w", err)
	}

	// Create filter.
	f := strategy.NewFilter(
		cfg.Strategy.Promotions,
		cfg.Strategy.Keywords,
		cfg.Strategy.MinSize,
		cfg.Strategy.MaxSize,
	)

	// Create retire policy.
	r := strategy.NewRetirePolicy(
		cfg.Retire.MinSeeders,
		cfg.Retire.MinRatio,
		cfg.Retire.MinDays,
		cfg.Retire.DeleteFiles,
	)

	// Create source clients from config.
	srcs := make(map[string]*source.SourceClient)
	for _, sp := range cfg.Sources {
		passkey := ""
		if sp.Extra != nil {
			if pk, ok := sp.Extra["passkey"].(string); ok {
				passkey = pk
			}
		}
		sc, err := source.NewSourceClient(sp.RSSURL, source.SourceClientOptions{
			Passkey:        passkey,
			Cookie:         sp.Cookie,
			APIToken:       sp.APIToken,
			TimeoutSeconds: 30,
		})
		if err != nil {
			slog.Warn("engine: failed to create source client", "site", sp.Name, "error", err)
			continue
		}
		srcs[sp.Name] = sc
	}

	interval := time.Duration(cfg.Monitor.IntervalSeconds) * time.Second

	maxConc := cfg.Strategy.MaxConcurrent
	if maxConc <= 0 {
		maxConc = 3
	}

	return &Engine{
		cfg:             cfg,
		store:           s,
		qb:              qbClient,
		filter:          f,
		retire:          r,
		sources:         srcs,
		started:         time.Now(),
		stopCh:          make(chan struct{}),
		sem:             make(chan struct{}, maxConc),
		monitorInterval: interval,
	}, nil
}

// Start begins the engine's main loops and optionally the web server.
func (e *Engine) Start() error {
	if e.running.Swap(true) {
		return fmt.Errorf("engine: already running")
	}

	// Login to qB.
	if err := e.qb.Login(); err != nil {
		slog.Warn("engine: qb login failed, will retry", "error", err)
	} else {
		e.stats.QBConnected = true
	}

	e.started = time.Now()

	// Start monitor loop.
	e.wg.Add(1)
	go e.monitorLoop()

	// Start main cycle loop.
	e.wg.Add(1)
	go e.cycleLoop()

	slog.Info("engine started",
		"poll_interval", e.cfg.PollInterval,
		"monitor_interval", e.monitorInterval,
		"sources", len(e.sources),
		"targets", len(e.cfg.Targets),
	)

	return nil
}

// Stop gracefully shuts down the engine.
func (e *Engine) Stop() error {
	if !e.running.Swap(false) {
		return nil
	}

	slog.Info("engine stopping...")
	close(e.stopCh)
	e.wg.Wait()

	if e.qb != nil {
		e.qb.Close()
	}
	if e.store != nil {
		e.store.Close()
	}
	for _, sc := range e.sources {
		sc.Close()
	}

	slog.Info("engine stopped")
	return nil
}

// IsRunning reports whether the engine is running.
func (e *Engine) IsRunning() bool {
	return e.running.Load()
}

// GetStats returns a snapshot of engine statistics.
func (e *Engine) GetStats() web.EngineStats {
	e.statsMu.RLock()
	defer e.statsMu.RUnlock()

	return web.EngineStats{
		TotalPublished:   e.stats.TotalPublished,
		TotalCrossSeeded: e.stats.TotalCrossSeeded,
		CurrentSeeding:   e.stats.CurrentSeeding,
		DiskFreeGB:       e.stats.DiskFreeGB,
		DiskTotalGB:      e.stats.DiskTotalGB,
		TodayPublished:   e.stats.TodayPublished,
		TodayCrossSeeded: e.stats.TodayCrossSeeded,
		QBConnected:      e.stats.QBConnected,
		UptimeSeconds:    int64(time.Since(e.started).Seconds()),
	}
}

// GetSeeds returns a paginated, filtered list of seeds.
func (e *Engine) GetSeeds(filter web.SeedFilter) ([]web.SeedInfo, int, error) {
	if filter.Limit <= 0 {
		filter.Limit = 50
	}
	if filter.Page <= 0 {
		filter.Page = 1
	}

	// Get all rows and filter in memory for simplicity.
	// In production, this should use SQL WHERE clauses.
	all, err := e.store.All()
	if err != nil {
		return nil, 0, fmt.Errorf("engine: get seeds: %w", err)
	}

	var filtered []map[string]any
	for _, row := range all {
		status, _ := row["target_status"].(string)
		if filter.Status != "" && status != filter.Status {
			continue
		}
		title, _ := row["title"].(string)
		if filter.Query != "" && !containsFold(title, filter.Query) {
			continue
		}
		target, _ := row["target_site"].(string)
		if filter.Target != "" && target != filter.Target {
			continue
		}
		filtered = append(filtered, row)
	}

	total := len(filtered)

	// Paginate.
	offset := (filter.Page - 1) * filter.Limit
	if offset > len(filtered) {
		return []web.SeedInfo{}, total, nil
	}
	end := offset + filter.Limit
	if end > len(filtered) {
		end = len(filtered)
	}
	page := filtered[offset:end]

	// Convert to SeedInfo.
	seeds := make([]web.SeedInfo, 0, len(page))
	for _, row := range page {
		seeds = append(seeds, rowToSeedInfo(row))
	}

	return seeds, total, nil
}

// GetSeedDetail returns a single seed with its relay records.
func (e *Engine) GetSeedDetail(id int64) (*web.SeedDetail, error) {
	// Since the current store doesn't have numeric IDs, we look up by scanning.
	// In production, add an integer primary key to the table.
	all, err := e.store.All()
	if err != nil {
		return nil, fmt.Errorf("engine: get seed detail: %w", err)
	}

	// For now, use row index as ID.
	if id < 0 || int(id) >= len(all) {
		return nil, fmt.Errorf("engine: seed %d not found", id)
	}

	row := all[id]
	info := rowToSeedInfo(row)
	info.ID = id

	return &web.SeedDetail{
		SeedInfo: info,
		Records:  []web.RecordEntry{}, // TODO: implement record log
	}, nil
}

// RetireSeed removes a seed from qB and marks it retired in the store.
func (e *Engine) RetireSeed(id int64) error {
	detail, err := e.GetSeedDetail(id)
	if err != nil {
		return err
	}

	e.cfgMu.RLock()
	deleteFiles := e.cfg.Retire.DeleteFiles
	e.cfgMu.RUnlock()

	// Stop and delete from qB.
	if e.qb != nil && e.stats.QBConnected {
		if err := e.qb.Stop(detail.InfoHash); err != nil {
			slog.Warn("engine: retire stop failed", "hash", detail.InfoHash, "error", err)
		}
		if err := e.qb.Delete(detail.InfoHash, deleteFiles); err != nil {
			slog.Warn("engine: retire delete failed", "hash", detail.InfoHash, "error", err)
		}
	}

	// Mark in store.
	if err := e.store.MarkStatus(detail.InfoHash, "skipped", map[string]any{
		"error": "retired by user",
	}); err != nil {
		return fmt.Errorf("engine: mark retired: %w", err)
	}

	slog.Info("engine: seed retired", "id", id, "hash", detail.InfoHash)
	return nil
}

// RetrySeed re-enables a failed seed for re-processing.
func (e *Engine) RetrySeed(id int64) error {
	detail, err := e.GetSeedDetail(id)
	if err != nil {
		return err
	}

	// Reset status to pending for re-processing.
	if err := e.store.MarkStatus(detail.InfoHash, "pending", map[string]any{
		"error": nil,
	}); err != nil {
		return fmt.Errorf("engine: mark retry: %w", err)
	}

	slog.Info("engine: seed marked for retry", "id", id, "hash", detail.InfoHash)
	return nil
}

// GetConfig returns the current engine configuration.
func (e *Engine) GetConfig() *config.AppConfig {
	e.cfgMu.RLock()
	defer e.cfgMu.RUnlock()
	return e.cfg
}

// SaveConfig applies a new configuration. Some changes require a restart.
func (e *Engine) SaveConfig(cfg *config.AppConfig) error {
	e.cfgMu.Lock()
	// Update the in-memory config.
	e.cfg = cfg

	// Persist to file.
	if err := config.SaveAppConfig(cfg, ""); err != nil {
		e.cfgMu.Unlock()
		return fmt.Errorf("engine: save config: %w", err)
	}

	// Update filter rules live.
	e.filter = strategy.NewFilter(
		cfg.Strategy.Promotions,
		cfg.Strategy.Keywords,
		cfg.Strategy.MinSize,
		cfg.Strategy.MaxSize,
	)

	// Update retire rules live.
	e.retire = strategy.NewRetirePolicy(
		cfg.Retire.MinSeeders,
		cfg.Retire.MinRatio,
		cfg.Retire.MinDays,
		cfg.Retire.DeleteFiles,
	)
	e.cfgMu.Unlock()

	slog.Info("engine: config updated (restart may be needed for source/target changes)")
	return nil
}

// rowToSeedInfo converts a store row map to a web.SeedInfo.
func rowToSeedInfo(row map[string]any) web.SeedInfo {
	info := web.SeedInfo{
		Status:   "unknown",
		Progress: 1.0,
	}

	if v, ok := row["info_hash"].(string); ok {
		info.InfoHash = v
	}
	if v, ok := row["title"].(string); ok {
		info.Title = v
	}
	if v, ok := row["source_site"].(string); ok {
		info.SourceSite = v
	}
	if v, ok := row["source_size"].(int64); ok {
		info.SourceSize = v
	}
	if v, ok := row["target_status"].(string); ok {
		info.Status = v
	}
	if v, ok := row["target_site"].(string); ok {
		info.TargetSite = v
	}
	if v, ok := row["target_id"].(int64); ok {
		info.TargetID = v
	}
	if v, ok := row["created_at"].(string); ok {
		info.CreatedAt = v
	}
	if v, ok := row["updated_at"].(string); ok {
		info.UpdatedAt = v
	}
	if v, ok := row["error"].(string); ok {
		info.ErrorMsg = v
	}

	// Map store status to web status.
	switch info.Status {
	case "downloaded", "added_to_qb":
		info.Status = "downloading"
		info.Progress = 0.5
	case "seeded", "cross_seeded", "uploaded":
		info.Status = "seeding"
		info.Progress = 1.0
	case "pending":
		info.Progress = 0.0
	}

	return info
}

// Ensure implementation satisfies the interface.
var _ web.EngineInterface = (*Engine)(nil)
var _ = targets.Upload // reference targets package
var _ = source.ParseRSS // reference source package

func containsFold(s, substr string) bool {
	return len(s) >= len(substr) && len(substr) > 0 &&
		// Simple case-insensitive contains.
		containsFoldImpl(s, substr)
}

func containsFoldImpl(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	if len(s) < len(substr) {
		return false
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			c1 := s[i+j]
			c2 := substr[j]
			if c1 >= 'A' && c1 <= 'Z' {
				c1 += 32
			}
			if c2 >= 'A' && c2 <= 'Z' {
				c2 += 32
			}
			if c1 != c2 {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
