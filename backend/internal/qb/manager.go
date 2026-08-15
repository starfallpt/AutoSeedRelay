package qb

import (
	"context"
	"sort"
	"sync"
)

// Manager is a concurrency-safe registry of named qB Instances.
type Manager struct {
	mu    sync.RWMutex
	insts map[string]*Instance
}

// NewManager returns an empty Manager.
func NewManager() *Manager {
	return &Manager{insts: make(map[string]*Instance)}
}

// Set registers (or replaces) the instance under name. A nil instance
// removes the entry, mirroring Remove.
func (m *Manager) Set(name string, inst *Instance) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if inst == nil {
		delete(m.insts, name)
		return
	}
	m.insts[name] = inst
}

// Get returns the instance registered under name and whether it exists.
func (m *Manager) Get(name string) (*Instance, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	inst, ok := m.insts[name]
	return inst, ok
}

// Remove deletes the instance registered under name.
func (m *Manager) Remove(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.insts, name)
}

// Names returns the registered instance names in sorted order.
func (m *Manager) Names() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.insts))
	for name := range m.insts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Status is a per-instance health snapshot.
type Status struct {
	Name      string `json:"name"`
	Online    bool   `json:"online"`
	Version   string `json:"version,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// AllHealthy concurrently pings every registered instance and returns one
// Status per instance (in name order). Per-instance failures are reported
// as Online=false with LastError set; the returned error is non-nil only
// when ctx was cancelled or expired while the checks were running.
func (m *Manager) AllHealthy(ctx context.Context) ([]Status, error) {
	m.mu.RLock()
	type pair struct {
		name string
		inst *Instance
	}
	pairs := make([]pair, 0, len(m.insts))
	for name, inst := range m.insts {
		pairs = append(pairs, pair{name, inst})
	}
	m.mu.RUnlock()
	sort.Slice(pairs, func(a, b int) bool { return pairs[a].name < pairs[b].name })

	results := make([]Status, len(pairs))
	var wg sync.WaitGroup
	for idx, p := range pairs {
		wg.Add(1)
		go func(idx int, p pair) {
			defer wg.Done()
			version, err := p.inst.Ping(ctx)
			if err != nil {
				results[idx] = Status{Name: p.name, Online: false, LastError: err.Error()}
				return
			}
			results[idx] = Status{Name: p.name, Online: true, Version: version}
		}(idx, p)
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return results, err
	}
	return results, nil
}
