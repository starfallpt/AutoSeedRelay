package engine

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/autoseedrelay/relay/internal/qb"
	"github.com/autoseedrelay/relay/internal/store"
)

// ErrNoQB is returned when no enabled, registered qB instance is available.
var ErrNoQB = errors.New("engine: no enabled qB instance available")

// DispatchOpts carries optional selection hints into SelectQB.
type DispatchOpts struct {
	// PreferName, when set and registered, is returned directly. It implements
	// the cross-seed rule "prefer the qB already hosting the origin torrent"
	// (BIZ-SPEC §7).
	PreferName string
}

// Dispatcher selects a qB instance for download / cross-seed work using one of
// the four strategies in BIZ-SPEC §7: manual priority, most free disk, least
// jobs, or round robin.
type Dispatcher struct {
	repo  *store.Repo
	qbMgr *qb.Manager
	rr    atomic.Uint64 // round-robin cursor
}

// NewDispatcher builds a Dispatcher over the repo (for strategy + instance
// metadata) and the manager (for live instances).
func NewDispatcher(repo *store.Repo, qbMgr *qb.Manager) *Dispatcher {
	if qbMgr == nil {
		qbMgr = qb.NewManager()
	}
	return &Dispatcher{repo: repo, qbMgr: qbMgr}
}

// SelectQB returns the name of the qB instance to use. Names key the
// *qb.Manager, so the caller resolves the instance with qbMgr.Get(name).
func (d *Dispatcher) SelectQB(ctx context.Context, opts DispatchOpts) (string, error) {
	instances, err := d.repo.GetEnabledQBInstances(ctx)
	if err != nil {
		return "", err
	}
	if len(instances) == 0 {
		return "", ErrNoQB
	}

	// Cross-seed: prefer the qB that already hosts the origin torrent.
	if opts.PreferName != "" {
		if _, ok := d.qbMgr.Get(opts.PreferName); ok {
			return opts.PreferName, nil
		}
	}

	switch d.mode(ctx) {
	case "most_free_disk":
		return d.mostFreeDisk(ctx, instances)
	case "least_jobs":
		return d.leastJobs(ctx, instances)
	case "round_robin":
		return d.roundRobin(instances)
	default: // "manual" / "priority" / unknown → highest priority first
		return d.byPriority(instances)
	}
}

// mode reads the dispatch strategy, defaulting to manual priority.
func (d *Dispatcher) mode(ctx context.Context) string {
	st, err := d.repo.GetStrategy(ctx)
	if err != nil || st == nil || st.DispatchMode == "" {
		return "priority"
	}
	return st.DispatchMode
}

// byPriority returns the first registered instance in priority order. The repo
// already returns them sorted by priority DESC, id ASC.
func (d *Dispatcher) byPriority(instances []*store.QBInstance) (string, error) {
	for _, qi := range instances {
		if _, ok := d.qbMgr.Get(qi.Name); ok {
			return qi.Name, nil
		}
	}
	return "", ErrNoQB
}

// mostFreeDisk returns the registered instance with the largest reported free
// disk space. Instances whose disk query fails report 0 (not preferred).
func (d *Dispatcher) mostFreeDisk(ctx context.Context, instances []*store.QBInstance) (string, error) {
	best := ""
	var bestFree int64 = -1
	for _, qi := range instances {
		inst, ok := d.qbMgr.Get(qi.Name)
		if !ok {
			continue
		}
		var free int64
		if info, err := inst.GetDiskSpace(ctx); err == nil && info != nil {
			free = info.FreeOnDisk
		}
		if best == "" || free > bestFree {
			best, bestFree = qi.Name, free
		}
	}
	if best == "" {
		return "", ErrNoQB
	}
	return best, nil
}

// leastJobs returns the registered instance with the fewest torrents. An
// instance whose Info query fails is treated as maximally loaded (not chosen).
func (d *Dispatcher) leastJobs(ctx context.Context, instances []*store.QBInstance) (string, error) {
	best := ""
	bestCount := int(^uint(0) >> 1) // max int
	for _, qi := range instances {
		inst, ok := d.qbMgr.Get(qi.Name)
		if !ok {
			continue
		}
		count := int(^uint(0) >> 1)
		if infos, err := inst.Info(ctx, ""); err == nil {
			count = len(infos)
		}
		if best == "" || count < bestCount {
			best, bestCount = qi.Name, count
		}
	}
	if best == "" {
		return "", ErrNoQB
	}
	return best, nil
}

// roundRobin cycles through the registered instances in stable priority order.
func (d *Dispatcher) roundRobin(instances []*store.QBInstance) (string, error) {
	var names []string
	for _, qi := range instances {
		if _, ok := d.qbMgr.Get(qi.Name); ok {
			names = append(names, qi.Name)
		}
	}
	if len(names) == 0 {
		return "", ErrNoQB
	}
	idx := d.rr.Add(1) - 1
	return names[idx%uint64(len(names))], nil
}
