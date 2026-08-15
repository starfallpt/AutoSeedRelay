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
	GetRecord(ctx context.Context, seedID, targetID int64) (*store.RelayRecord, error)
	UpsertRecord(ctx context.Context, rec *store.RelayRecord) error
	ListReplicas(ctx context.Context, seedID int64) ([]*store.Replica, error)
	UpsertReplica(ctx context.Context, rep *store.Replica) error
	AppendLog(ctx context.Context, level, action, detail string) error
	UpdateSeedStatus(ctx context.Context, id int64, status, errMsg string) error
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
	statusRelayed     = "seeding"
	statusPublished   = "published"
	statusCrossSeeded = "cross_seeding"
	statusFailed      = "failed"
	rolePublisher     = "publisher"
	roleSeeder        = "seeder"
	replicaCross      = "cross"
	replicaSeeding    = "seeding"
)

// RelayOne is the concrete Pipeline implementation.
type RelayOne struct {
	repo     Repo
	qb       *qb.Manager
	notifier *notifier.Router
	source   SourceFactory
	adapters AdapterFactory
	workDir  string
	now      func() time.Time

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
			return NewSourceProvider(src, p.firstQB, SourceConfig{})
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
// returned as an error; the engine owns the retry queue.
func (p *RelayOne) Relay(ctx context.Context, seedID int64) error {
	seed, err := p.repo.GetSeedByID(ctx, seedID)
	if err != nil {
		return fmt.Errorf("pipeline: load seed %d: %w", seedID, err)
	}

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

	tmp := filepath.Join(p.workDir, fmt.Sprintf("relay-%d-%s.torrent", seedID, strings.ToLower(seed.InfoHash)))
	tor, err := p.downloadAndParse(ctx, sp, item, tmp, seed)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)

	targets, err := p.repo.GetEnabledTargets(ctx)
	if err != nil {
		return fmt.Errorf("pipeline: list targets: %w", err)
	}
	if len(targets) == 0 {
		return errors.New("pipeline: no enabled targets")
	}

	results := p.publishToTargets(ctx, seed, src, detail, tor, targets)

	var failed int
	var errs []error
	for _, r := range results {
		if r.err != nil {
			failed++
			errs = append(errs, fmt.Errorf("target %s: %w", r.target.Name, r.err))
		}
	}
	if failed < len(results) {
		_ = p.repo.UpdateSeedStatus(ctx, seed.ID, statusRelayed, "")
		return nil
	}
	return fmt.Errorf("pipeline: relay failed for all %d targets: %w", failed, errors.Join(errs...))
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

// firstQB returns the first registered qB instance (name order). It is the
// pipeline's default source-download instance; the dispatcher owns the richer
// strategy (priority/least_jobs/disk/round_robin) in the engine.
func (p *RelayOne) firstQB() *qb.Instance {
	if p.qb == nil {
		return nil
	}
	if names := p.qb.Names(); len(names) > 0 {
		if inst, ok := p.qb.Get(names[0]); ok {
			return inst
		}
	}
	return nil
}

// pickQB returns a qB instance plus its DB id for cross-seeding, preferring
// the origin replica's qB (BIZ-SPEC §7), then the highest-priority enabled
// instance, then the first manager-registered instance.
func (p *RelayOne) pickQB(ctx context.Context, seed *store.Seed) (*qb.Instance, int64) {
	if p.qb == nil {
		return nil, 0
	}
	instances, _ := p.repo.GetEnabledQBInstances(ctx)
	byID := make(map[int64]*store.QBInstance, len(instances))
	for _, qi := range instances {
		byID[qi.ID] = qi
	}

	if seed != nil {
		if reps, err := p.repo.ListReplicas(ctx, seed.ID); err == nil {
			for _, r := range reps {
				if r.Role == "origin" {
					if qi := byID[r.QBID]; qi != nil {
						if inst, ok := p.qb.Get(qi.Name); ok {
							return inst, qi.ID
						}
					}
				}
			}
		}
	}
	for _, qi := range instances {
		if inst, ok := p.qb.Get(qi.Name); ok {
			return inst, qi.ID
		}
	}
	if names := p.qb.Names(); len(names) > 0 {
		if inst, ok := p.qb.Get(names[0]); ok {
			return inst, 0
		}
	}
	return nil, 0
}

// notify delivers a best-effort notification through the router.
func (p *RelayOne) notify(ctx context.Context, level notifier.Level, title, body string) {
	if p.notifier == nil {
		return
	}
	_ = p.notifier.Notify(ctx, level, notifier.Message{Title: title, Body: body})
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
