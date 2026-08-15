// Package pipeline implements the relay core chain (M2c): for a discovered
// seed it locates the source item, downloads the .torrent, fetches detail,
// cleans and adapts it for every enabled target, publishes, and cross-seeds
// when a target reports a duplicate. Business semantics follow
// docs/BIZ-SPEC.md §3 (nine-step flow), §4 (cross-seed) and §5 (state
// machine). Dependencies are injected through interfaces (store, source,
// adapters) so the whole chain is testable offline; qB and the notifier are
// the concrete *qb.Manager and *notifier.Router types per the module contract.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/autoseedrelay/relay/internal/adapters"
	"github.com/autoseedrelay/relay/internal/engine"
	"github.com/autoseedrelay/relay/internal/notifier"
	"github.com/autoseedrelay/relay/internal/parser"
	"github.com/autoseedrelay/relay/internal/qb"
	"github.com/autoseedrelay/relay/internal/source"
	"github.com/autoseedrelay/relay/internal/store"
)

// Pipeline is the contract the engine holds. The single method Relay drives a
// seed through the core chain; keeping it an interface pins the contract so
// the engine and pipeline cannot drift.
type Pipeline interface {
	Relay(ctx context.Context, seedID int64) error
}

// Repo is the store subset the pipeline needs. *store.Repo satisfies it.
type Repo interface {
	GetSeedByID(ctx context.Context, id int64) (*store.Seed, error)
	GetActiveSources(ctx context.Context) ([]*store.Source, error)
	GetSourceByID(ctx context.Context, id int64) (*store.Source, error)
	GetEnabledTargets(ctx context.Context) ([]*store.Target, error)
	GetEnabledQBInstances(ctx context.Context) ([]*store.QBInstance, error)
	GetStrategy(ctx context.Context) (*store.Strategy, error)
	GetRecord(ctx context.Context, seedID, targetID int64) (*store.RelayRecord, error)
	ListRecordsBySeed(ctx context.Context, seedID int64) ([]*store.RelayRecord, error)
	UpsertRecord(ctx context.Context, rec *store.RelayRecord) (inserted bool, err error)
	SetRecordRole(ctx context.Context, seedID, targetID int64, role string) error
	MarkPublished(ctx context.Context, seedID, targetID int64, publishedAt int64) error
	UpdateRecordStatus(ctx context.Context, seedID, targetID int64, status, errMsg string) error
	UpdateRecordAttempt(ctx context.Context, seedID, targetID int64, status, errMsg string) error
	ListReplicas(ctx context.Context, seedID int64) ([]*store.Replica, error)
	UpsertReplica(ctx context.Context, rep *store.Replica) error
	AppendLogSeed(ctx context.Context, seedID int64, level, action, detail string) error
	UpdateSeedStatus(ctx context.Context, id int64, status, errMsg string) error
	UpdateSeedPromotion(ctx context.Context, id int64, promotion string) error
}

// QBSelector chooses a qB instance for download / cross-seed work. It has the
// exact signature of engine.Dispatcher.SelectQB so main can inject the engine's
// dispatcher directly; the pipeline depends only on this interface, never on
// the engine implementation (the import of engine here is for the DispatchOpts
// type only and is acyclic — engine never imports pipeline).
type QBSelector interface {
	SelectQB(ctx context.Context, opts engine.DispatchOpts) (string, error)
}

// SourceFactory builds a SourceProvider for a specific source site. It exists
// so tests can inject a source client whose SSRF/HTTP behaviour is relaxed to
// talk to an httptest server.
type SourceFactory func(src *store.Source) SourceProvider

// AdapterFactory builds a target adapter from a SiteConfig. The default is
// adapters.New.
type AdapterFactory func(cfg adapters.SiteConfig) (adapters.Adapter, error)

// Deps carries every dependency RelayOne needs. QB and Notifier are concrete
// types; Repo/Source/Adapters are interfaces for testability.
type Deps struct {
	Repo     Repo
	QB       *qb.Manager
	Notifier *notifier.Router
	Source   SourceFactory
	Adapters AdapterFactory

	// QBSelector selects a qB instance for download / cross-seed work. When
	// nil the pipeline falls back to the first registered instance.
	QBSelector QBSelector

	// WorkDir is where temporary .torrent files are written. Defaults to the
	// OS temp directory.
	WorkDir string
	// Now is the injectable clock used for cross-seed polling deadlines.
	Now func() time.Time

	// MaxTargetConcurrency caps per-target publish goroutines (default 4).
	MaxTargetConcurrency int
	// CrossSeedTimeout bounds the skip_checking completion poll (default 5m).
	CrossSeedTimeout time.Duration
	// CrossSeedInterval is the poll cadence during cross-seed verification.
	CrossSeedInterval time.Duration
}

// Seed / record statuses written by the pipeline. The seed terminal is
// "seeding" (engine consumes it once at least one target succeeded); records
// use "published" / "cross_seeding" / "failed". All values follow the
// BIZ-SPEC §5 state machine.
const (
	statusSeeding      = "seeding"
	statusPending      = "pending"
	statusPublished    = "published"
	statusCrossSeeding = "cross_seeding"
	statusFailed      = "failed"
	statusRetired     = "retired"
	statusSkipped     = "skipped"
	rolePublisher     = "publisher"
	roleSeeder        = "seeder"
	replicaCross      = "cross"
	replicaSeeding    = "seeding"
)

// doneRecordStatuses are the relay_record.status values that mean a target is
// finished (published / cross-seeding / seeding / retired) and must be skipped
// on retry (target-level idempotency).
var doneRecordStatuses = map[string]bool{
	statusPublished:    true,
	statusCrossSeeding: true,
	statusSeeding:      true, // "seeding" at the record level
	statusRetired:      true,
}

// TargetFailure records one target that failed during a relay attempt.
type TargetFailure struct {
	TargetID   int64
	TargetName string
	Err        error
}

// PartialFailure is returned by Relay when at least one target succeeded but
// one or more targets failed. It is retryable: the engine re-runs Relay, which
// re-processes only the still-failed targets (idempotent via relay_records).
type PartialFailure struct {
	SeedID int64
	Failed []TargetFailure
}

// Error implements the error interface.
func (p *PartialFailure) Error() string {
	if p == nil || len(p.Failed) == 0 {
		return "pipeline: partial failure"
	}
	names := make([]string, 0, len(p.Failed))
	for _, f := range p.Failed {
		names = append(names, f.TargetName)
	}
	return fmt.Sprintf("pipeline: partial failure for seed %d (failed targets: %s)", p.SeedID, strings.Join(names, ", "))
}

// IsPartial marks this error as a partial (retryable) failure so the engine can
// detect it structurally (errors.As) without importing the pipeline package.
func (p *PartialFailure) IsPartial() bool { return true }

// FailedNames returns human-readable "name: err" lines for the failed targets,
// used by the engine's retry-exhaustion critical notification.
func (p *PartialFailure) FailedNames() []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.Failed))
	for _, f := range p.Failed {
		out = append(out, fmt.Sprintf("%s: %s", f.TargetName, f.Err))
	}
	return out
}

// RelayOne is the concrete Pipeline implementation.
type RelayOne struct {
	repo     Repo
	qb       *qb.Manager
	notifier *notifier.Router
	source   SourceFactory
	adapters AdapterFactory
	workDir  string
	now      func() time.Time

	qbSelector QBSelector

	maxTargetConcurrency int
	crossSeedTimeout     time.Duration
	crossSeedInterval    time.Duration
}

// New builds a RelayOne with sensible defaults for any zero-valued tunables.
func New(deps Deps) *RelayOne {
	p := &RelayOne{
		repo:                 deps.Repo,
		qb:                   deps.QB,
		notifier:             deps.Notifier,
		source:               deps.Source,
		adapters:             deps.Adapters,
		workDir:              deps.WorkDir,
		now:                  deps.Now,
		qbSelector:           deps.QBSelector,
		maxTargetConcurrency: deps.MaxTargetConcurrency,
		crossSeedTimeout:     deps.CrossSeedTimeout,
		crossSeedInterval:    deps.CrossSeedInterval,
	}
	if p.workDir == "" {
		p.workDir = os.TempDir()
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.maxTargetConcurrency <= 0 {
		p.maxTargetConcurrency = 4
	}
	if p.crossSeedTimeout <= 0 {
		p.crossSeedTimeout = 5 * time.Minute
	}
	if p.crossSeedInterval <= 0 {
		p.crossSeedInterval = 2 * time.Second
	}
	if p.source == nil {
		p.source = func(src *store.Source) SourceProvider {
			return NewSourceProvider(src, p.downloadQB, SourceConfig{})
		}
	}
	if p.adapters == nil {
		p.adapters = adapters.New
	}
	return p
}

var torrentIDFromLinkRe = regexp.MustCompile(`id=(\d+)`)

// Relay drives one seed through the core chain and returns nil once at least
// one target succeeded (seed.status becomes "seeding"). A total failure is
// returned as a plain error; a partial failure (some targets succeeded, some
// failed) is returned as *PartialFailure, which the engine treats as retryable.
func (p *RelayOne) Relay(ctx context.Context, seedID int64) error {
	seed, err := p.repo.GetSeedByID(ctx, seedID)
	if err != nil {
		return fmt.Errorf("pipeline: load seed %d: %w", seedID, err)
	}

	_ = p.repo.UpdateSeedStatus(ctx, seedID, "processing", "")

	src := p.resolveSource(ctx, seed)
	if src == nil {
		return fmt.Errorf("pipeline: no active source %q for seed %d", seed.SourceSite, seedID)
	}

	sp := p.source(src)

	// Locate the RSS item: it carries the torrent id (for detail fetch) and
	// the download URL (for the .torrent fetch).
	item, err := sp.Locate(ctx, seed)
	if err != nil {
		return fmt.Errorf("pipeline: locate source item: %w", err)
	}
	torrentID := itemTorrentID(item)
	if torrentID == 0 {
		return fmt.Errorf("pipeline: source item has no torrent id (link=%q)", item.Link)
	}

	detail, err := sp.FetchDetail(ctx, torrentID)
	if err != nil {
		return fmt.Errorf("pipeline: fetch detail: %w", err)
	}
	// Persist the promotion marker as soon as it is known (best-effort metadata;
	// empty means the detail path yielded no promotion, so keep whatever we had).
	if detail != nil && detail.Promotion != "" {
		_ = p.repo.UpdateSeedPromotion(ctx, seedID, detail.Promotion)
	}

	// Promotion filter (after detail fetch): skip when the strategy whitelists
	// promotions and the seed's promotion is not among them.
	if skip, err := p.checkPromotionFilter(ctx, seed, detail); err != nil {
		return err
	} else if skip {
		return nil
	}

	_ = p.repo.UpdateSeedStatus(ctx, seedID, "downloading", "")
	tmp := filepath.Join(p.workDir, fmt.Sprintf("relay-%d-%s.torrent", seedID, strings.ToLower(seed.InfoHash)))
	tor, err := p.downloadAndParse(ctx, sp, item, tmp, seed)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	_ = p.repo.UpdateSeedStatus(ctx, seedID, "downloaded", "")

	targets, err := p.repo.GetEnabledTargets(ctx)
	if err != nil {
		return fmt.Errorf("pipeline: list targets: %w", err)
	}
	if len(targets) == 0 {
		return errors.New("pipeline: no enabled targets")
	}

	// Target-level idempotent retry: only process targets whose record status
	// is not already finished (published / cross_seeding / seeding / retired).
	pending := p.pendingTargets(ctx, seed.ID, targets)
	if len(pending) == 0 {
		_ = p.repo.UpdateSeedStatus(ctx, seed.ID, statusSeeding, "")
		return nil
	}

	results := p.publishToTargets(ctx, seed, src, detail, tor, pending)

	var failed []TargetFailure
	for _, r := range results {
		if r.err != nil {
			failed = append(failed, TargetFailure{TargetID: r.target.ID, TargetName: r.target.Name, Err: r.err})
		}
	}
	if len(failed) == 0 {
		_ = p.repo.UpdateSeedStatus(ctx, seed.ID, statusSeeding, "")
		return nil
	}
	if len(failed) < len(results) {
		return &PartialFailure{SeedID: seed.ID, Failed: failed}
	}
	errs := make([]error, 0, len(failed))
	for _, f := range failed {
		errs = append(errs, fmt.Errorf("target %s: %w", f.TargetName, f.Err))
	}
	return fmt.Errorf("pipeline: relay failed for all %d targets: %w", len(failed), errors.Join(errs...))
}

// pendingTargets returns the targets whose relay record is not yet finished,
// i.e. the targets Relay should (re-)process.
func (p *RelayOne) pendingTargets(ctx context.Context, seedID int64, targets []*store.Target) []*store.Target {
	records, err := p.repo.ListRecordsBySeed(ctx, seedID)
	if err != nil {
		// On error, degrade to processing all targets (safe: the atomic claim
		// still prevents double-publish).
		return targets
	}
	done := make(map[int64]bool, len(records))
	for _, rec := range records {
		if doneRecordStatuses[rec.Status] {
			done[rec.TargetID] = true
		}
	}
	out := make([]*store.Target, 0, len(targets))
	for _, t := range targets {
		if !done[t.ID] {
			out = append(out, t)
		}
	}
	return out
}

// checkPromotionFilter applies the promotion whitelist after detail fetch. It
// returns skip=true (seed marked skipped + logged) when the strategy whitelists
// promotions and the seed's promotion is not among them.
func (p *RelayOne) checkPromotionFilter(ctx context.Context, seed *store.Seed, detail *source.SeedDetail) (bool, error) {
	st, err := p.repo.GetStrategy(ctx)
	if err != nil || st == nil {
		return false, nil // strategy missing → no filter (accept all)
	}
	promos := parseStringList(st.Promotions)
	if len(promos) == 0 {
		return false, nil // 未配置 = 全收
	}

	promo := ""
	if detail != nil {
		seed.Promotion = detail.Promotion
		promo = source.NormalizePromotion(detail.Promotion)
	}
	if promo == "" {
		// 未知/缺失促销名 → 按不匹配处理并记日志 (BIZ-SPEC §6).
		p.skipSeed(ctx, seed, "promotion_filter", "未知/缺失促销标记,不在白名单内")
		return true, nil
	}
	for _, wp := range promos {
		if source.NormalizePromotion(wp) == promo {
			return false, nil
		}
	}
	p.skipSeed(ctx, seed, "promotion_filter", fmt.Sprintf("促销 %q 不在白名单内", promo))
	return true, nil
}

// skipSeed marks a seed skipped and appends a seed-scoped log line.
func (p *RelayOne) skipSeed(ctx context.Context, seed *store.Seed, action, detail string) {
	_ = p.repo.UpdateSeedStatus(ctx, seed.ID, statusSkipped, detail)
	_ = p.repo.AppendLogSeed(ctx, seed.ID, "info", action, detail)
}

// parseStringList decodes a JSON string-array column (keywords / promotions).
func parseStringList(raw string) []string {
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

// resolveSource finds the active source row for a seed: SourceID wins, then
// a name match against the active sources.
func (p *RelayOne) resolveSource(ctx context.Context, seed *store.Seed) *store.Source {
	if seed.SourceID != 0 {
		if s, err := p.repo.GetSourceByID(ctx, seed.SourceID); err == nil && s.Status == "active" {
			return s
		}
	}
	sources, err := p.repo.GetActiveSources(ctx)
	if err != nil {
		return nil
	}
	for _, s := range sources {
		if s.Name == seed.SourceSite {
			return s
		}
	}
	return nil
}

// downloadAndParse downloads the .torrent (qB direct-pull preferred, direct
// HTTP fallback), parses it, and verifies the info_hash matches the seed.
func (p *RelayOne) downloadAndParse(ctx context.Context, sp SourceProvider, item *source.RssItem, tmp string, seed *store.Seed) (*parser.ParsedTorrent, error) {
	if err := sp.Download(ctx, item, tmp); err != nil {
		return nil, fmt.Errorf("pipeline: download torrent: %w", err)
	}
	data, err := os.ReadFile(tmp)
	if err != nil {
		return nil, fmt.Errorf("pipeline: read torrent: %w", err)
	}
	tor, err := parser.ParseTorrent(data)
	if err != nil {
		return nil, fmt.Errorf("pipeline: parse torrent: %w", err)
	}
	if !strings.EqualFold(tor.InfoHash, seed.InfoHash) {
		return nil, fmt.Errorf("pipeline: info_hash mismatch: got %s want %s", tor.InfoHash, seed.InfoHash)
	}
	return tor, nil
}

// downloadQB selects the qB instance for source direct-pull via the injected
// QBSelector (falling back to the first registered instance).
func (p *RelayOne) downloadQB(ctx context.Context) *qb.Instance {
	if p.qb == nil {
		return nil
	}
	if inst, _ := p.selectQB(ctx, ""); inst != nil {
		return inst
	}
	if names := p.qb.Names(); len(names) > 0 {
		if inst, ok := p.qb.Get(names[0]); ok {
			return inst
		}
	}
	return nil
}

// selectQB returns a qB instance plus its DB id for download / cross-seed work,
// using the injected QBSelector (cross-seed prefers the origin replica's qB via
// PreferName). When no selector is wired (or it fails), it falls back to the
// highest-priority enabled DB instance (so the returned id is a real
// qb_instances.id for replica FKs), then the first manager-registered instance.
func (p *RelayOne) selectQB(ctx context.Context, preferName string) (*qb.Instance, int64) {
	if p.qb == nil {
		return nil, 0
	}
	if p.qbSelector != nil {
		if name, err := p.qbSelector.SelectQB(ctx, engine.DispatchOpts{PreferName: preferName}); err == nil && name != "" {
			if inst, ok := p.qb.Get(name); ok {
				return inst, p.qbIDByName(ctx, name)
			}
		}
	}
	if instances, err := p.repo.GetEnabledQBInstances(ctx); err == nil {
		for _, qi := range instances {
			if inst, ok := p.qb.Get(qi.Name); ok {
				return inst, qi.ID
			}
		}
	}
	if names := p.qb.Names(); len(names) > 0 {
		if inst, ok := p.qb.Get(names[0]); ok {
			return inst, 0
		}
	}
	return nil, 0
}

// originQBName returns the name of the qB hosting the seed's origin replica
// (for cross-seed "prefer the origin's qB", BIZ-SPEC §7).
func (p *RelayOne) originQBName(ctx context.Context, seed *store.Seed) string {
	if seed == nil {
		return ""
	}
	reps, err := p.repo.ListReplicas(ctx, seed.ID)
	if err != nil {
		return ""
	}
	for _, r := range reps {
		if r.Role == "origin" {
			if qi := p.qbInstanceByID(ctx, r.QBID); qi != nil {
				return qi.Name
			}
		}
	}
	return ""
}

func (p *RelayOne) qbIDByName(ctx context.Context, name string) int64 {
	instances, _ := p.repo.GetEnabledQBInstances(ctx)
	for _, qi := range instances {
		if qi.Name == name {
			return qi.ID
		}
	}
	return 0
}

func (p *RelayOne) qbInstanceByID(ctx context.Context, id int64) *store.QBInstance {
	instances, _ := p.repo.GetEnabledQBInstances(ctx)
	for _, qi := range instances {
		if qi.ID == id {
			return qi
		}
	}
	return nil
}

// notify delivers a best-effort notification through the router. The body is
// redacted so credential-bearing URLs never leak into notification text. The
// event tag feeds the router's per-(instance, tier, event) aggregation.
func (p *RelayOne) notify(ctx context.Context, level notifier.Level, title, body, event string) {
	if p.notifier == nil {
		return
	}
	_ = p.notifier.Notify(ctx, level, notifier.Message{Title: title, Body: source.RedactError(body), Event: event})
}

func itemTorrentID(item *source.RssItem) int {
	if item == nil {
		return 0
	}
	if item.ID != "" {
		if n, err := strconv.Atoi(item.ID); err == nil {
			return n
		}
	}
	if m := torrentIDFromLinkRe.FindStringSubmatch(item.Link); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return n
		}
	}
	return 0
}

// targetResult is the per-target outcome of a publish goroutine.
type targetResult struct {
	target *store.Target
	err    error
}

// publishToTargets runs one publish goroutine per target, capped at
// maxTargetConcurrency, and collects the outcomes (a single target failure
// never cancels the others).
func (p *RelayOne) publishToTargets(ctx context.Context, seed *store.Seed, src *store.Source, detail *source.SeedDetail, tor *parser.ParsedTorrent, targets []*store.Target) []targetResult {
	sem := make(chan struct{}, p.maxTargetConcurrency)
	results := make([]targetResult, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t *store.Target) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = targetResult{target: t, err: p.publishToOneTarget(ctx, seed, src, detail, tor, t)}
		}(i, t)
	}
	wg.Wait()
	return results
}
