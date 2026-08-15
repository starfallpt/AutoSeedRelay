package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/autoseedrelay/relay/internal/notifier"
	"github.com/autoseedrelay/relay/internal/qb"
	"github.com/autoseedrelay/relay/internal/store"
)

// monitorLoop periodically checks every enabled qB instance: health, torrent
// progress for seeds/replicas, and retire conditions (BIZ-SPEC §5).
func (e *Engine) monitorLoop(ctx context.Context) {
	defer e.wg.Done()

	e.monitor(ctx)

	ticker := time.NewTicker(e.cfg.MonitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.monitor(ctx)
		}
	}
}

// monitor performs one full monitoring pass.
func (e *Engine) monitor(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	statuses, err := e.qbMgr.AllHealthy(ctx)
	if err != nil {
		return // ctx cancelled/expired while pinging
	}
	e.observeHealth(ctx, statuses)

	instances, err := e.repo.GetEnabledQBInstances(ctx)
	if err != nil {
		e.log.Error("monitor: list enabled qb", "error", err)
		return
	}

	for _, qi := range instances {
		inst, ok := e.qbMgr.Get(qi.Name)
		if !ok {
			continue
		}
		torrents, err := inst.Info(ctx, "")
		if err != nil {
			e.log.Warn("monitor: qb info failed", "qb", qi.Name, "error", err)
			continue
		}
		e.monitorInstance(ctx, qi, torrents)
	}
}

// observeHealth emits critical/warning notifications for qB reachability.
// critical (all offline) is de-duplicated via e.qbAllOffline; per-instance
// warnings are aggregated by the router's 10-minute window.
func (e *Engine) observeHealth(ctx context.Context, statuses []qb.Status) {
	if len(statuses) == 0 {
		return
	}
	online := 0
	for _, s := range statuses {
		if s.Online {
			online++
		} else {
			e.notify(ctx, notifier.LevelWarning, "qB 断连",
				fmt.Sprintf("instance=%s error=%s", s.Name, s.LastError))
		}
	}
	if online == 0 {
		if e.qbAllOffline.CompareAndSwap(false, true) {
			e.notify(ctx, notifier.LevelCritical, "qB 全部离线", "所有启用 qB 实例均不可达")
		}
		return
	}
	e.qbAllOffline.Store(false)
}

// monitorInstance reconciles one qB instance's torrents against the seeds and
// replicas: it maintains origin replica rows with live progress and evaluates
// retire conditions for completed seeds. Ownership is replica-based
// (replica.info_hash × qb_id); a hash-only fallback handles historical seeds
// that predate the replica rows, and only when the hash is unambiguous.
func (e *Engine) monitorInstance(ctx context.Context, qi *store.QBInstance, torrents []*qb.TorrentInfo) {
	byHash := make(map[string]*qb.TorrentInfo, len(torrents))
	for _, t := range torrents {
		if t == nil || t.Hash == "" {
			continue
		}
		byHash[strings.ToLower(t.Hash)] = t
	}

	replicas, err := e.repo.ListReplicasByQB(ctx, qi.ID)
	if err != nil {
		e.log.Error("monitor: list replicas by qb", "qb", qi.Name, "error", err)
		return
	}

	processed := make(map[int64]bool, len(replicas))
	for _, rep := range replicas {
		sd, err := e.repo.GetSeedByID(ctx, rep.SeedID)
		if err != nil {
			continue // orphan replica
		}
		t, ok := byHash[strings.ToLower(rep.InfoHash)]
		if !ok {
			continue // torrent no longer on this qB
		}
		if rep.Progress != t.Progress {
			if err := e.repo.UpdateReplicaProgress(ctx, rep.ID, t.Progress); err != nil {
				e.log.Warn("monitor: update replica progress", "replica_id", rep.ID, "error", err)
			}
		}
		if !processed[sd.ID] {
			processed[sd.ID] = true
			e.retireCheck(ctx, sd, qi, t)
		}
	}

	e.fallbackHashMatch(ctx, qi, byHash, processed)
}

// fallbackHashMatch reconciles seeding seeds that have no replica on this qB
// (historical data) by info_hash. It only acts when exactly one seed maps to a
// hash; a hash shared by multiple seeds is ambiguous and is skipped (never
// retired) with a warning.
func (e *Engine) fallbackHashMatch(ctx context.Context, qi *store.QBInstance, byHash map[string]*qb.TorrentInfo, processed map[int64]bool) {
	seeds := e.seedingSeeds(ctx)

	bySeedHash := map[string][]*store.Seed{}
	for _, sd := range seeds {
		if processed[sd.ID] {
			continue
		}
		h := strings.ToLower(sd.InfoHash)
		bySeedHash[h] = append(bySeedHash[h], sd)
	}

	for h, sds := range bySeedHash {
		t, ok := byHash[h]
		if !ok {
			continue
		}
		if len(sds) != 1 {
			e.log.Warn("monitor: hash matches multiple seeds on qb; skip retire",
				"qb", qi.Name, "hash", h, "seeds", len(sds))
			continue
		}
		sd := sds[0]
		e.ensureOriginReplica(ctx, sd, qi, t)
		e.retireCheck(ctx, sd, qi, t)
	}
}

// ensureOriginReplica upserts (or updates the progress of) the origin replica
// for a seed on this instance.
func (e *Engine) ensureOriginReplica(ctx context.Context, sd *store.Seed, qi *store.QBInstance, t *qb.TorrentInfo) {
	reps, err := e.repo.ListReplicas(ctx, sd.ID)
	if err != nil {
		e.log.Warn("monitor: list replicas", "seed_id", sd.ID, "error", err)
		return
	}
	for _, r := range reps {
		if r.QBID == qi.ID && r.Role == "origin" {
			if r.Progress != t.Progress {
				if err := e.repo.UpdateReplicaProgress(ctx, r.ID, t.Progress); err != nil {
					e.log.Warn("monitor: update replica progress", "replica_id", r.ID, "error", err)
				}
			}
			return
		}
	}

	rep := &store.Replica{
		SeedID:   sd.ID,
		QBID:     qi.ID,
		InfoHash: sd.InfoHash,
		Role:     "origin",
		Status:   "seeding",
		Progress: t.Progress,
	}
	if err := e.repo.UpsertReplica(ctx, rep); err != nil {
		e.log.Warn("monitor: upsert origin replica", "seed_id", sd.ID, "qb", qi.Name, "error", err)
	}
}

// retireCheck evaluates and applies retire conditions for a completed seed on
// this instance (BIZ-SPEC §5 / ARCHITECTURE-v4 §7).
func (e *Engine) retireCheck(ctx context.Context, sd *store.Seed, qi *store.QBInstance, t *qb.TorrentInfo) {
	if !qb.IsCompletedSeeding(t) {
		return
	}

	st := e.strategy(ctx)
	should, reason := shouldRetire(t, st, e.now())
	if !should {
		return
	}

	// Stop/delete the source torrent from this qB.
	if inst, ok := e.qbMgr.Get(qi.Name); ok {
		if err := inst.Delete(ctx, t.Hash, false); err != nil {
			e.log.Warn("monitor: retire delete failed", "hash", t.Hash, "error", err)
		}
	}

	// Mark every published record for this seed retired, per target.
	records, err := e.repo.ListRecordsBySeed(ctx, sd.ID)
	if err != nil {
		e.log.Warn("monitor: list records", "seed_id", sd.ID, "error", err)
		return
	}
	for _, rec := range records {
		if !isPublishedStatus(rec.Status) {
			continue
		}
		if err := e.repo.MarkRetired(ctx, sd.ID, rec.TargetID, reason); err != nil {
			e.log.Warn("monitor: mark retired", "seed_id", sd.ID, "target_id", rec.TargetID, "error", err)
			continue
		}
		if err := e.repo.UpdateRecordStatus(ctx, sd.ID, rec.TargetID, "retired", ""); err != nil {
			e.log.Warn("monitor: update record status", "seed_id", sd.ID, "target_id", rec.TargetID, "error", err)
		}
		if err := e.repo.AppendLog(ctx, "info", "retired",
			fmt.Sprintf("seed=%d target=%d reason=%s", sd.ID, rec.TargetID, reason)); err != nil {
			e.log.Warn("monitor: append retired log", "error", err)
		}
		e.notify(ctx, notifier.LevelInfo, "自动撤种",
			fmt.Sprintf("seed=%d target=%d %s", sd.ID, rec.TargetID, reason))
	}

	// Drop replicas and close out the seed's lifecycle.
	if reps, err := e.repo.ListReplicas(ctx, sd.ID); err == nil {
		for _, r := range reps {
			if err := e.repo.DeleteReplica(ctx, r.ID); err != nil {
				e.log.Warn("monitor: delete replica", "replica_id", r.ID, "error", err)
			}
		}
	}
	if err := e.repo.UpdateSeedStatus(ctx, sd.ID, "retired", reason); err != nil {
		e.log.Warn("monitor: update seed status retired", "seed_id", sd.ID, "error", err)
	}
	e.log.Info("monitor: seed retired", "seed_id", sd.ID, "reason", reason)
}

// seedingStatuses are the seed statuses that mean "actively seeding in qB,
// awaiting retire judgment": BIZ-SPEC §5 uses "seeding"; the concrete pipeline
// marks success "relayed". Both are watched so the monitor works regardless of
// which the pipeline settles on.
var seedingStatuses = []string{"seeding", "relayed"}

// seedingSeeds returns the union of seeds in any actively-seeding status,
// deduplicated by id.
func (e *Engine) seedingSeeds(ctx context.Context) []*store.Seed {
	var out []*store.Seed
	seen := map[int64]bool{}
	for _, status := range seedingStatuses {
		seeds, err := e.repo.ListSeedsByStatus(ctx, status)
		if err != nil {
			e.log.Error("monitor: list seeding seeds", "status", status, "error", err)
			continue
		}
		for _, sd := range seeds {
			if !seen[sd.ID] {
				seen[sd.ID] = true
				out = append(out, sd)
			}
		}
	}
	return out
}

// isPublishedStatus reports whether a relay-record status means the seed is
// actively published/seeding on that target. Both BIZ-SPEC §5 ("cross_seeding")
// and the pipeline's spelling ("cross_seeded") are accepted.
func isPublishedStatus(status string) bool {
	switch status {
	case "published", "seeding", "cross_seeding", "cross_seeded":
		return true
	default:
		return false
	}
}

// shouldRetire evaluates the retire strategy against one completed torrent.
//
//	seedersOK:  num_complete >= retire_seeders
//	timeOK:     seeding duration > retire_minutes (measured from completion_on)
//	ratioOK:    ratio >= retire_ratio, only when retire_ratio_enabled
//
// retire_mode "and" requires every active condition; "or" requires any.
func shouldRetire(t *qb.TorrentInfo, st *store.Strategy, now time.Time) (bool, string) {
	if st == nil {
		st = defaultStrategy()
	}

	seedersOK := int64(t.Seeders) >= st.RetireSeeders

	minutes := 0.0
	if t.CompletionOn > 0 {
		minutes = now.Sub(time.Unix(t.CompletionOn, 0)).Minutes()
	}
	timeOK := minutes > float64(st.RetireMinutes)

	conds := []bool{seedersOK, timeOK}
	labels := []string{
		fmt.Sprintf("seeders=%d(>=%d)", t.Seeders, st.RetireSeeders),
		fmt.Sprintf("minutes=%.1f(>%d)", minutes, st.RetireMinutes),
	}
	if st.RetireRatioEnabled != 0 {
		ratioOK := t.Ratio >= st.RetireRatio
		conds = append(conds, ratioOK)
		labels = append(labels, fmt.Sprintf("ratio=%.2f(>=%.2f)", t.Ratio, st.RetireRatio))
	}

	met := conds[0]
	for _, c := range conds[1:] {
		if st.RetireMode == "or" {
			met = met || c
		} else {
			met = met && c
		}
	}

	return met, strings.Join(labels, " ")
}
