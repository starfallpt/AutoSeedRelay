package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autoseedrelay/relay/internal/notifier"
	"github.com/autoseedrelay/relay/internal/qb"
	"github.com/autoseedrelay/relay/internal/source"
	"github.com/autoseedrelay/relay/internal/store"
)

var ctx = context.Background()

const testHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// --- test helpers ---

func testKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

func newTestRepo(t *testing.T) *store.Repo {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return store.NewRepo(st.DB(), testKey())
}

func rawCount(t *testing.T, repo *store.Repo, query string, args ...any) int {
	t.Helper()
	var n int
	if err := repo.DB().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("raw count %q: %v", query, err)
	}
	return n
}

// fakeClock is a controllable, concurrency-safe time source.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakePipeline records Relay calls and returns a configurable error.
type fakePipeline struct {
	mu    sync.Mutex
	calls []int64
	err   error   // fallback single error (existing tests)
	seq   []error // per-call error sequence; sticks to the last entry once exhausted
}

func (f *fakePipeline) Relay(_ context.Context, seedID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, seedID)
	if len(f.seq) > 0 {
		i := len(f.calls) - 1
		if i < len(f.seq) {
			return f.seq[i]
		}
		return f.seq[len(f.seq)-1]
	}
	return f.err
}

// fakePartialFailure is an engine-local stand-in for pipeline.PartialFailure,
// letting the engine's structural detection be tested without importing
// pipeline.
type fakePartialFailure struct{ failed []string }

func (f *fakePartialFailure) Error() string         { return "partial: " + strings.Join(f.failed, ", ") }
func (f *fakePartialFailure) IsPartial() bool       { return true }
func (f *fakePartialFailure) FailedNames() []string { return f.failed }

func (f *fakePipeline) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakePipeline) ids() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.calls...)
}

// recordingNotifier captures every delivered message.
type recordingNotifier struct {
	mu    sync.Mutex
	calls []notifier.Message
}

func (n *recordingNotifier) Send(_ context.Context, msg notifier.Message) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, msg)
	return nil
}

func (n *recordingNotifier) messages() []notifier.Message {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]notifier.Message(nil), n.calls...)
}

// fakeQB is an httptest-backed qB WebUI API with controllable torrent list and
// disk space, and records delete calls.
type fakeQB struct {
	mu       sync.Mutex
	torrents []*qb.TorrentInfo
	freeDisk int64
	deleted  []string
}

func (f *fakeQB) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/auth/login":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/app/version":
			io.WriteString(w, "v5.0.0")
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/torrents/info":
			_ = json.NewEncoder(w).Encode(f.torrents)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/sync/maindata":
			_, _ = fmt.Fprintf(w, `{"server_state":{"free_space_on_disk":%d}}`, f.freeDisk)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/torrents/delete":
			_ = r.ParseForm()
			f.deleted = append(f.deleted, r.PostFormValue("hashes"))
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (f *fakeQB) deleteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deleted)
}

// registerFakeQB inserts a qb row and registers an httptest-backed instance in
// the manager, returning the DB id and the fake for assertions.
func registerFakeQB(t *testing.T, repo *store.Repo, qbMgr *qb.Manager, name string, priority int64, torrents []*qb.TorrentInfo, freeDisk int64) (int64, *fakeQB) {
	t.Helper()
	fq := &fakeQB{torrents: torrents, freeDisk: freeDisk}
	srv := httptest.NewServer(fq.handler())
	t.Cleanup(srv.Close)

	qi := &store.QBInstance{Name: name, Host: srv.URL, Port: 0, Username: "u", Password: "p", Priority: priority, Enabled: 1}
	if err := repo.UpsertQBInstance(ctx, qi); err != nil {
		t.Fatalf("UpsertQBInstance: %v", err)
	}
	qbMgr.Set(name, qb.NewInstance(srv.URL, "", "u", "p", qb.WithHTTPClient(srv.Client())))
	return qi.ID, fq
}

func completedTorrent(hash string, seeders int, completionOn int64) *qb.TorrentInfo {
	return &qb.TorrentInfo{
		Hash:         hash,
		Name:         "torrent",
		State:        "uploading",
		Progress:     1,
		Completed:    100,
		CompletionOn: completionOn,
		Seeders:      seeders,
		Ratio:        1.0,
	}
}

func setDispatchMode(t *testing.T, repo *store.Repo, mode string) {
	t.Helper()
	st, err := repo.GetStrategy(ctx)
	if err != nil {
		t.Fatalf("GetStrategy: %v", err)
	}
	st.DispatchMode = mode
	if err := repo.UpdateStrategy(ctx, st); err != nil {
		t.Fatalf("UpdateStrategy: %v", err)
	}
}

// --- poller tests ---

func TestPollerDedupNoDoubleRelay(t *testing.T) {
	repo := newTestRepo(t)
	pl := &fakePipeline{}
	eng := New(Config{Workers: 2}, repo, pl, qb.NewManager(), nil)

	eng.SetFetchRSS(func(_ context.Context, _ string, _ *http.Client) ([]source.RssItem, error) {
		size := int64(100)
		return []source.RssItem{{GUID: testHash, Title: "Movie", Link: "http://x?id=1", Size: &size}}, nil
	})

	if err := repo.UpsertSource(ctx, &store.Source{Name: "src", Role: "source", RSSURL: "http://x/rss", Status: "active"}); err != nil {
		t.Fatal(err)
	}

	eng.poll(ctx)

	if n := rawCount(t, repo, `SELECT count(*) FROM seeds WHERE source_site='src' AND info_hash=?`, testHash); n != 1 {
		t.Fatalf("seed count after first poll = %d, want 1", n)
	}
	if got := len(eng.jobs); got != 1 {
		t.Fatalf("jobs after first poll = %d, want 1", got)
	}

	// Simulate the worker running the discovered seed through the pipeline.
	eng.submitJob(ctx, <-eng.jobs, 0)
	if pl.count() != 1 {
		t.Fatalf("Relay called %d times, want 1", pl.count())
	}

	// Second poll: same hash must not re-create or re-relay.
	eng.poll(ctx)
	if n := rawCount(t, repo, `SELECT count(*) FROM seeds WHERE source_site='src' AND info_hash=?`, testHash); n != 1 {
		t.Fatalf("seed count after second poll = %d, want 1", n)
	}
	if got := len(eng.jobs); got != 0 {
		t.Fatalf("jobs after second poll = %d, want 0", got)
	}
	if pl.count() != 1 {
		t.Fatalf("Relay called %d times after second poll, want 1", pl.count())
	}
}

func TestPollerRetiredPermanentSkip(t *testing.T) {
	repo := newTestRepo(t)
	pl := &fakePipeline{}
	eng := New(Config{Workers: 2}, repo, pl, qb.NewManager(), nil)

	// A seed already retired for this hash.
	sd := &store.Seed{SourceSite: "src", InfoHash: testHash, Title: "Old", Status: "retired"}
	if _, err := repo.CreateSeed(ctx, sd); err != nil {
		t.Fatal(err)
	}

	eng.SetFetchRSS(func(_ context.Context, _ string, _ *http.Client) ([]source.RssItem, error) {
		return []source.RssItem{{GUID: testHash, Title: "Movie", Link: "http://x?id=1"}}, nil
	})
	if err := repo.UpsertSource(ctx, &store.Source{Name: "src", Role: "source", RSSURL: "http://x/rss", Status: "active"}); err != nil {
		t.Fatal(err)
	}

	eng.poll(ctx)

	if n := rawCount(t, repo, `SELECT count(*) FROM seeds WHERE source_site='src' AND info_hash=?`, testHash); n != 1 {
		t.Fatalf("seed count = %d, want 1 (retired row persists, never re-created)", n)
	}
	if got := len(eng.jobs); got != 0 {
		t.Fatalf("jobs = %d, want 0 (retired seed must be skipped)", got)
	}
	if pl.count() != 0 {
		t.Fatalf("Relay called %d times, want 0", pl.count())
	}
}

func TestPollerTombstoneSurvivesSeedDeletion(t *testing.T) {
	repo := newTestRepo(t)
	pl := &fakePipeline{}
	eng := New(Config{Workers: 2}, repo, pl, qb.NewManager(), nil)

	eng.SetFetchRSS(func(_ context.Context, _ string, _ *http.Client) ([]source.RssItem, error) {
		return []source.RssItem{{GUID: testHash, Title: "Movie", Link: "http://x?id=1"}}, nil
	})
	if err := repo.UpsertSource(ctx, &store.Source{Name: "src", Role: "source", RSSURL: "http://x/rss", Status: "active"}); err != nil {
		t.Fatal(err)
	}

	// First poll: the hash is discovered and a seed row is created.
	eng.poll(ctx)
	if n := rawCount(t, repo, `SELECT count(*) FROM seeds WHERE source_site='src' AND info_hash=?`, testHash); n != 1 {
		t.Fatalf("seed count after first poll = %d, want 1", n)
	}
	if got := len(eng.jobs); got != 1 {
		t.Fatalf("jobs after first poll = %d, want 1", got)
	}
	<-eng.jobs // drain the discovered job so later assertions see a clean channel

	// The poller must also have tombstoned the hash.
	if n := rawCount(t, repo, `SELECT count(*) FROM seen_hashes WHERE source_site='src' AND info_hash=?`, testHash); n != 1 {
		t.Fatalf("seen_hashes rows after first poll = %d, want 1", n)
	}

	// Simulate user cleanup: delete the seed row. The tombstone must persist.
	if _, err := repo.DB().ExecContext(ctx, `DELETE FROM seeds WHERE source_site='src' AND info_hash=?`, testHash); err != nil {
		t.Fatal(err)
	}

	// Replay the same RSS: the tombstone must prevent re-creating the seed.
	eng.poll(ctx)
	if n := rawCount(t, repo, `SELECT count(*) FROM seeds WHERE source_site='src' AND info_hash=?`, testHash); n != 0 {
		t.Fatalf("seed count after delete + replay = %d, want 0 (tombstone prevents re-creation)", n)
	}
	if got := len(eng.jobs); got != 0 {
		t.Fatalf("jobs after delete + replay = %d, want 0", got)
	}
	if pl.count() != 0 {
		t.Fatalf("Relay called %d times, want 0", pl.count())
	}
}

func TestPollerPauseOnConsecutiveFailures(t *testing.T) {
	repo := newTestRepo(t)
	pl := &fakePipeline{}
	eng := New(Config{Workers: 1}, repo, pl, qb.NewManager(), nil)

	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	eng.SetClock(clk.Now)
	eng.SetBackoff(func(int) time.Duration { return time.Second })
	eng.SetFetchRSS(func(_ context.Context, _ string, _ *http.Client) ([]source.RssItem, error) {
		return nil, errors.New("boom")
	})

	src := &store.Source{Name: "src", Role: "source", RSSURL: "http://x/rss", Status: "active"}
	if err := repo.UpsertSource(ctx, src); err != nil {
		t.Fatal(err)
	}

	eng.poll(ctx) // fail #1 → backs off 1s
	if got, _ := repo.GetSourceByID(ctx, src.ID); got.Status != "active" || got.FailCount != 1 {
		t.Fatalf("after fail#1: status=%s fail_count=%d, want active/1", got.Status, got.FailCount)
	}

	eng.poll(ctx) // within backoff → skipped
	if got, _ := repo.GetSourceByID(ctx, src.ID); got.FailCount != 1 {
		t.Fatalf("backoff should skip; fail_count=%d, want 1", got.FailCount)
	}

	clk.Advance(2 * time.Second)
	eng.poll(ctx) // fail #2
	clk.Advance(2 * time.Second)
	eng.poll(ctx) // fail #3 → pause

	got, err := repo.GetSourceByID(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "paused" {
		t.Fatalf("status = %q, want paused after 3 consecutive failures", got.Status)
	}
	if got.FailCount != 3 {
		t.Fatalf("fail_count = %d, want 3", got.FailCount)
	}
}

// --- monitor tests ---

func seedAndRecord(t *testing.T, repo *store.Repo, status, recordStatus string) (seedID, targetID int64) {
	t.Helper()
	sd := &store.Seed{SourceSite: "src", InfoHash: testHash, Title: "T", Status: status}
	if _, err := repo.CreateSeed(ctx, sd); err != nil {
		t.Fatal(err)
	}
	tgt := &store.Target{Name: "t1", Type: "nexusphp", Version: "api", Status: "active"}
	if err := repo.UpsertTarget(ctx, tgt); err != nil {
		t.Fatal(err)
	}
	rec := &store.RelayRecord{SeedID: sd.ID, TargetID: tgt.ID, Role: "publisher", Status: recordStatus}
	if inserted, err := repo.UpsertRecord(ctx, rec); err != nil || !inserted {
		t.Fatalf("UpsertRecord = (%v, %v), want (true, nil)", inserted, err)
	}
	return sd.ID, tgt.ID
}

func TestMonitorRetireAND(t *testing.T) {
	repo := newTestRepo(t)
	seedID, _ := seedAndRecord(t, repo, "seeding", "published")
	qbMgr := qb.NewManager()
	_, fq := registerFakeQB(t, repo, qbMgr, "qb", 1, []*qb.TorrentInfo{
		completedTorrent(testHash, 15, time.Unix(1_700_000_000, 0).Add(-90*time.Minute).Unix()),
	}, 0)

	eng := New(Config{Workers: 1}, repo, &fakePipeline{}, qbMgr, nil)
	eng.SetClock(func() time.Time { return time.Unix(1_700_000_000, 0) })

	eng.monitor(ctx)

	got, _ := repo.GetSeedByID(ctx, seedID)
	if got.Status != "retired" {
		t.Fatalf("seed status = %q, want retired (AND: seeders>=10 and time>60m)", got.Status)
	}
	rec, _ := repo.GetRecord(ctx, seedID, 1) // target id 1
	if rec.Status != "retired" || rec.RetiredAt == 0 {
		t.Fatalf("record = %+v, want retired with retired_at set", rec)
	}
	if fq.deleteCount() != 1 {
		t.Fatalf("qb delete count = %d, want 1", fq.deleteCount())
	}
	if n := rawCount(t, repo, `SELECT count(*) FROM activity_log WHERE action='retired'`); n == 0 {
		t.Fatal("expected a retired activity_log row")
	}
}

func TestMonitorRetireANDNotSatisfied(t *testing.T) {
	repo := newTestRepo(t)
	seedID, _ := seedAndRecord(t, repo, "seeding", "published")
	qbMgr := qb.NewManager()
	_, fq := registerFakeQB(t, repo, qbMgr, "qb", 1, []*qb.TorrentInfo{
		completedTorrent(testHash, 15, time.Unix(1_700_000_000, 0).Add(-10*time.Minute).Unix()),
	}, 0)

	eng := New(Config{Workers: 1}, repo, &fakePipeline{}, qbMgr, nil)
	eng.SetClock(func() time.Time { return time.Unix(1_700_000_000, 0) })

	eng.monitor(ctx)

	got, _ := repo.GetSeedByID(ctx, seedID)
	if got.Status != "seeding" {
		t.Fatalf("seed status = %q, want seeding (AND not satisfied: time < 60m)", got.Status)
	}
	if fq.deleteCount() != 0 {
		t.Fatalf("qb delete count = %d, want 0", fq.deleteCount())
	}
}

func TestMonitorRetireOR(t *testing.T) {
	repo := newTestRepo(t)
	seedID, _ := seedAndRecord(t, repo, "seeding", "published")

	// Switch retire mode to OR.
	st, _ := repo.GetStrategy(ctx)
	st.RetireMode = "or"
	if err := repo.UpdateStrategy(ctx, st); err != nil {
		t.Fatal(err)
	}

	qbMgr := qb.NewManager()
	_, fq := registerFakeQB(t, repo, qbMgr, "qb", 1, []*qb.TorrentInfo{
		completedTorrent(testHash, 5, time.Unix(1_700_000_000, 0).Add(-90*time.Minute).Unix()),
	}, 0)

	eng := New(Config{Workers: 1}, repo, &fakePipeline{}, qbMgr, nil)
	eng.SetClock(func() time.Time { return time.Unix(1_700_000_000, 0) })

	eng.monitor(ctx)

	got, _ := repo.GetSeedByID(ctx, seedID)
	if got.Status != "retired" {
		t.Fatalf("seed status = %q, want retired (OR: time > 60m alone satisfies)", got.Status)
	}
	if fq.deleteCount() != 1 {
		t.Fatalf("qb delete count = %d, want 1", fq.deleteCount())
	}
}

func TestMonitorRetireORNotSatisfied(t *testing.T) {
	repo := newTestRepo(t)
	seedID, _ := seedAndRecord(t, repo, "seeding", "published")

	st, _ := repo.GetStrategy(ctx)
	st.RetireMode = "or"
	if err := repo.UpdateStrategy(ctx, st); err != nil {
		t.Fatal(err)
	}

	qbMgr := qb.NewManager()
	_, fq := registerFakeQB(t, repo, qbMgr, "qb", 1, []*qb.TorrentInfo{
		completedTorrent(testHash, 5, time.Unix(1_700_000_000, 0).Add(-10*time.Minute).Unix()),
	}, 0)

	eng := New(Config{Workers: 1}, repo, &fakePipeline{}, qbMgr, nil)
	eng.SetClock(func() time.Time { return time.Unix(1_700_000_000, 0) })

	eng.monitor(ctx)

	got, _ := repo.GetSeedByID(ctx, seedID)
	if got.Status != "seeding" {
		t.Fatalf("seed status = %q, want seeding (OR not satisfied)", got.Status)
	}
	if fq.deleteCount() != 0 {
		t.Fatalf("qb delete count = %d, want 0", fq.deleteCount())
	}
}

func TestShouldRetireTable(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name       string
		seeders    int
		minutesAgo int
		ratio      float64
		mode       string
		ratioOn    bool
		ratioMin   float64
		want       bool
	}{
		{"and-both", 15, 90, 1.0, "and", false, 0, true},
		{"and-seeders-only", 15, 10, 1.0, "and", false, 0, false},
		{"and-time-only", 5, 90, 1.0, "and", false, 0, false},
		{"or-time", 5, 90, 1.0, "or", false, 0, true},
		{"or-seeders", 15, 10, 1.0, "or", false, 0, true},
		{"or-neither", 5, 10, 1.0, "or", false, 0, false},
		{"and-ratio-ok", 15, 90, 3.0, "and", true, 2.0, true},
		{"and-ratio-fail", 15, 90, 1.0, "and", true, 2.0, false},
		{"or-ratio-only", 5, 10, 3.0, "or", true, 2.0, true},
		{"or-ratio-fail", 5, 10, 0.5, "or", true, 2.0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tt := &qb.TorrentInfo{
				Seeders:      c.seeders,
				Ratio:        c.ratio,
				CompletionOn: now.Add(-time.Duration(c.minutesAgo) * time.Minute).Unix(),
			}
			st := defaultStrategy()
			st.RetireMode = c.mode
			if c.ratioOn {
				st.RetireRatioEnabled = 1
				st.RetireRatio = c.ratioMin
			}
			got, _ := shouldRetire(tt, st, now)
			if got != c.want {
				t.Fatalf("shouldRetire = %v, want %v", got, c.want)
			}
		})
	}
}

// --- dispatcher tests ---

func TestDispatcherPriority(t *testing.T) {
	repo := newTestRepo(t)
	qbMgr := qb.NewManager()
	registerFakeQB(t, repo, qbMgr, "low", 1, nil, 0)
	registerFakeQB(t, repo, qbMgr, "high", 5, nil, 0)
	registerFakeQB(t, repo, qbMgr, "mid", 3, nil, 0)

	d := NewDispatcher(repo, qbMgr)
	got, err := d.SelectQB(ctx, DispatchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "high" {
		t.Fatalf("priority SelectQB = %q, want high", got)
	}
}

func TestDispatcherMostFreeDisk(t *testing.T) {
	repo := newTestRepo(t)
	qbMgr := qb.NewManager()
	registerFakeQB(t, repo, qbMgr, "a", 1, nil, 100)
	registerFakeQB(t, repo, qbMgr, "b", 1, nil, 900)
	registerFakeQB(t, repo, qbMgr, "c", 1, nil, 500)
	setDispatchMode(t, repo, "most_free_disk")

	d := NewDispatcher(repo, qbMgr)
	got, err := d.SelectQB(ctx, DispatchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "b" {
		t.Fatalf("most_free_disk SelectQB = %q, want b (900 free)", got)
	}
}

func TestDispatcherLeastJobs(t *testing.T) {
	repo := newTestRepo(t)
	qbMgr := qb.NewManager()
	mk := func(n int) []*qb.TorrentInfo {
		out := make([]*qb.TorrentInfo, n)
		for i := range out {
			out[i] = &qb.TorrentInfo{Hash: fmt.Sprintf("%040d", i)}
		}
		return out
	}
	registerFakeQB(t, repo, qbMgr, "a", 1, mk(5), 0)
	registerFakeQB(t, repo, qbMgr, "b", 1, mk(1), 0)
	registerFakeQB(t, repo, qbMgr, "c", 1, mk(3), 0)
	setDispatchMode(t, repo, "least_jobs")

	d := NewDispatcher(repo, qbMgr)
	got, err := d.SelectQB(ctx, DispatchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "b" {
		t.Fatalf("least_jobs SelectQB = %q, want b (1 job)", got)
	}
}

func TestDispatcherRoundRobin(t *testing.T) {
	repo := newTestRepo(t)
	qbMgr := qb.NewManager()
	registerFakeQB(t, repo, qbMgr, "alpha", 10, nil, 0)
	registerFakeQB(t, repo, qbMgr, "beta", 20, nil, 0)
	setDispatchMode(t, repo, "round_robin")

	d := NewDispatcher(repo, qbMgr)
	var seq []string
	for i := 0; i < 4; i++ {
		name, err := d.SelectQB(ctx, DispatchOpts{})
		if err != nil {
			t.Fatal(err)
		}
		seq = append(seq, name)
	}
	want := []string{"beta", "alpha", "beta", "alpha"} // priority order: beta(20) then alpha(10)
	if strings.Join(seq, ",") != strings.Join(want, ",") {
		t.Fatalf("round_robin sequence = %v, want %v", seq, want)
	}
}

func TestDispatcherCrossSeedPreferOrigin(t *testing.T) {
	repo := newTestRepo(t)
	qbMgr := qb.NewManager()
	registerFakeQB(t, repo, qbMgr, "origin", 1, nil, 0)
	registerFakeQB(t, repo, qbMgr, "other", 9, nil, 0)

	d := NewDispatcher(repo, qbMgr)
	got, err := d.SelectQB(ctx, DispatchOpts{PreferName: "origin"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "origin" {
		t.Fatalf("cross-seed SelectQB = %q, want origin (prefer origin's qB)", got)
	}
}

// --- retry tests ---

func TestRetryBackoffSequenceAndFailOut(t *testing.T) {
	repo := newTestRepo(t)
	pl := &fakePipeline{err: errors.New("relay failed")}
	eng := New(Config{Workers: 1}, repo, pl, qb.NewManager(), nil)

	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	eng.SetClock(clk.Now)

	sd := &store.Seed{SourceSite: "src", InfoHash: testHash, Title: "T", Status: "discovered"}
	if _, err := repo.CreateSeed(ctx, sd); err != nil {
		t.Fatal(err)
	}

	// Initial attempt fails → schedule retry #1 (60s).
	eng.submitJob(ctx, sd.ID, 0)
	if got := len(eng.retry.Due()); got != 0 {
		t.Fatalf("due immediately after submit = %d, want 0 (backoff not elapsed)", got)
	}
	gotSeed, _ := repo.GetSeedByID(ctx, sd.ID)
	if gotSeed.Status != "retry" || gotSeed.RetryCount != 1 {
		t.Fatalf("after initial failure: status=%q retry_count=%d, want retry/1", gotSeed.Status, gotSeed.RetryCount)
	}

	clk.Advance(60 * time.Second)
	due := eng.retry.Due()
	if len(due) != 1 || due[0].retryNo != 1 {
		t.Fatalf("due after +60s = %+v, want one retryNo=1", due)
	}

	// Retry #1 fails → retry #2 (300s).
	eng.submitJob(ctx, sd.ID, 1)
	clk.Advance(300 * time.Second)
	due = eng.retry.Due()
	if len(due) != 1 || due[0].retryNo != 2 {
		t.Fatalf("due after +300s = %+v, want one retryNo=2", due)
	}

	// Retry #2 fails → retry #3 (900s).
	eng.submitJob(ctx, sd.ID, 2)
	clk.Advance(900 * time.Second)
	due = eng.retry.Due()
	if len(due) != 1 || due[0].retryNo != 3 {
		t.Fatalf("due after +900s = %+v, want one retryNo=3", due)
	}

	// Retry #3 fails → exceeded → failed, no further enqueue.
	eng.submitJob(ctx, sd.ID, 3)
	gotSeed, _ = repo.GetSeedByID(ctx, sd.ID)
	if gotSeed.Status != "failed" {
		t.Fatalf("status after exceed = %q, want failed", gotSeed.Status)
	}
	if gotSeed.RetryCount != 4 {
		t.Fatalf("retry_count after exceed = %d, want 4", gotSeed.RetryCount)
	}
	if got := len(eng.retry.Due()); got != 0 {
		t.Fatalf("due items after exceed = %d, want 0", got)
	}
	if pl.count() != 4 {
		t.Fatalf("Relay called %d times, want 4 (initial + 3 retries)", pl.count())
	}
}

func TestRebuildRetryQueueFromDB(t *testing.T) {
	repo := newTestRepo(t)
	pl := &fakePipeline{err: errors.New("relay failed")}
	eng := New(Config{Workers: 1}, repo, pl, qb.NewManager(), nil)

	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	eng.SetClock(clk.Now)

	sd := &store.Seed{SourceSite: "src", InfoHash: testHash, Title: "T", Status: "retry", RetryCount: 2}
	if _, err := repo.CreateSeed(ctx, sd); err != nil {
		t.Fatal(err)
	}

	eng.rebuildRetryQueue(ctx)

	if got := len(eng.retry.Due()); got != 0 {
		t.Fatalf("due immediately after rebuild = %d, want 0", got)
	}
	clk.Advance(300 * time.Second) // retryNo=2 → 300s backoff
	due := eng.retry.Due()
	if len(due) != 1 || due[0].seedID != sd.ID || due[0].retryNo != 2 {
		t.Fatalf("due after rebuild+300s = %+v, want seedID=%d retryNo=2", due, sd.ID)
	}
}

func TestEngineStartStop(t *testing.T) {
	repo := newTestRepo(t)
	pl := &fakePipeline{}
	eng := New(Config{Workers: 2, PollInterval: time.Hour, MonitorInterval: time.Hour}, repo, pl, qb.NewManager(), nil)
	eng.SetFetchRSS(func(_ context.Context, _ string, _ *http.Client) ([]source.RssItem, error) {
		return nil, nil
	})

	if err := eng.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !eng.Running() {
		t.Fatal("engine not running after Start")
	}
	if err := eng.Start(ctx); err == nil {
		t.Fatal("second Start should return an error")
	}
	if err := eng.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if eng.Running() {
		t.Fatal("engine still running after Stop")
	}
}

func TestRetryRedactsCredential(t *testing.T) {
	repo := newTestRepo(t)
	const secret = "sekrit-passkey-987"
	pl := &fakePipeline{err: errors.New("download https://src.example/download.php?id=1&passkey=" + secret + " failed")}
	rec := &recordingNotifier{}
	router := notifier.NewRouter()
	router.Add("rec", rec, notifier.LevelCritical)
	eng := New(Config{Workers: 1}, repo, pl, qb.NewManager(), router)

	sd := &store.Seed{SourceSite: "src", InfoHash: testHash, Title: "T", Status: "discovered"}
	if _, err := repo.CreateSeed(ctx, sd); err != nil {
		t.Fatal(err)
	}

	// retryNo = 3 == RetryMax → exhausted → writes seeds.error + critical notify.
	eng.submitJob(ctx, sd.ID, 3)

	got, err := repo.GetSeedByID(ctx, sd.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if strings.Contains(got.Error, secret) {
		t.Fatalf("seeds.error leaks passkey: %q", got.Error)
	}
	if !strings.Contains(got.Error, "?[redacted]") {
		t.Fatalf("seeds.error should carry redacted marker: %q", got.Error)
	}

	msgs := rec.messages()
	if len(msgs) == 0 {
		t.Fatal("expected a critical notification")
	}
	for _, m := range msgs {
		if strings.Contains(m.Body, secret) || strings.Contains(m.Title, secret) {
			t.Fatalf("notify leaks passkey: %+v", m)
		}
	}
}

// --- partial-failure retry tests ---

// seedingPipeline simulates a real pipeline that owns seed.status: the first
// call returns a partial failure, subsequent calls mark the seed "seeding" and
// succeed.
type seedingPipeline struct {
	repo  *store.Repo
	mu    sync.Mutex
	calls int
}

func (s *seedingPipeline) Relay(ctx context.Context, seedID int64) error {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	if call == 1 {
		return &fakePartialFailure{failed: []string{"t1"}}
	}
	_ = s.repo.UpdateSeedStatus(ctx, seedID, "seeding", "")
	return nil
}

func TestPartialFailureRetryThenSuccess(t *testing.T) {
	repo := newTestRepo(t)
	pl := &seedingPipeline{repo: repo}
	eng := New(Config{Workers: 1}, repo, pl, qb.NewManager(), nil)
	eng.SetClock(func() time.Time { return time.Unix(1_700_000_000, 0) })

	sd := &store.Seed{SourceSite: "src", InfoHash: testHash, Title: "T", Status: "discovered"}
	if _, err := repo.CreateSeed(ctx, sd); err != nil {
		t.Fatal(err)
	}

	// Attempt 0 → partial failure → retry scheduled, seed marked retry.
	eng.submitJob(ctx, sd.ID, 0)
	got, _ := repo.GetSeedByID(ctx, sd.ID)
	if got.Status != "retry" {
		t.Fatalf("status after partial failure = %q, want retry", got.Status)
	}

	// Retry #1 → success → pipeline marks seed seeding.
	eng.submitJob(ctx, sd.ID, 1)
	got, _ = repo.GetSeedByID(ctx, sd.ID)
	if got.Status != "seeding" {
		t.Fatalf("status after successful retry = %q, want seeding", got.Status)
	}
}

func TestPartialFailureExhausted(t *testing.T) {
	repo := newTestRepo(t)
	pl := &fakePipeline{seq: []error{&fakePartialFailure{failed: []string{"t1", "t2"}}}}
	rec := &recordingNotifier{}
	router := notifier.NewRouter()
	router.Add("rec", rec, notifier.LevelCritical)
	eng := New(Config{Workers: 1}, repo, pl, qb.NewManager(), router)

	sd := &store.Seed{SourceSite: "src", InfoHash: testHash, Title: "T", Status: "discovered"}
	if _, err := repo.CreateSeed(ctx, sd); err != nil {
		t.Fatal(err)
	}

	// retryNo = 3 == RetryMax → exhausted → keep successes (seeding) + critical.
	eng.submitJob(ctx, sd.ID, 3)

	got, _ := repo.GetSeedByID(ctx, sd.ID)
	if got.Status != "seeding" {
		t.Fatalf("status = %q, want seeding (preserve successful targets)", got.Status)
	}

	msgs := rec.messages()
	if len(msgs) == 0 {
		t.Fatal("expected a critical notification")
	}
	found := false
	for _, m := range msgs {
		if strings.Contains(m.Body, "t1") && strings.Contains(m.Body, "t2") {
			found = true
		}
	}
	if !found {
		t.Fatalf("critical notification should list failed targets t1/t2: %+v", msgs)
	}
}

// --- strategy filter tests ---

func TestPollerKeywordFilter(t *testing.T) {
	repo := newTestRepo(t)
	pl := &fakePipeline{}
	eng := New(Config{Workers: 2}, repo, pl, qb.NewManager(), nil)

	st, err := repo.GetStrategy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st.Keywords = `["2160p", "x264"]`
	if err := repo.UpdateStrategy(ctx, st); err != nil {
		t.Fatal(err)
	}

	if err := repo.UpsertSource(ctx, &store.Source{Name: "src", Role: "source", RSSURL: "http://x/rss", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	eng.SetFetchRSS(func(_ context.Context, _ string, _ *http.Client) ([]source.RssItem, error) {
		return []source.RssItem{
			{GUID: testHash, Title: "Movie.2160p.WEB-DL", Link: "http://x?id=1", Description: "no"},
			{GUID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Title: "Movie.1080p", Link: "http://x?id=2", Description: "remux x264"},
			{GUID: "cccccccccccccccccccccccccccccccccccccccc", Title: "Movie.720p", Link: "http://x?id=3", Description: "no match"},
		}, nil
	})

	eng.poll(ctx)

	// Only the first two match a keyword (title "2160p" and description "x264").
	if n := rawCount(t, repo, `SELECT count(*) FROM seeds`); n != 2 {
		t.Fatalf("seed count = %d, want 2", n)
	}
}

func TestPollerSizeFilter(t *testing.T) {
	repo := newTestRepo(t)
	pl := &fakePipeline{}
	eng := New(Config{Workers: 2}, repo, pl, qb.NewManager(), nil)

	st, err := repo.GetStrategy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st.MinSize = 100
	st.MaxSize = 200
	if err := repo.UpdateStrategy(ctx, st); err != nil {
		t.Fatal(err)
	}

	if err := repo.UpsertSource(ctx, &store.Source{Name: "src", Role: "source", RSSURL: "http://x/rss", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	eng.SetFetchRSS(func(_ context.Context, _ string, _ *http.Client) ([]source.RssItem, error) {
		small := int64(50)
		mid := int64(150)
		big := int64(500)
		return []source.RssItem{
			{GUID: testHash, Title: "Small", Link: "http://x?id=1", Size: &small},
			{GUID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Title: "Mid", Link: "http://x?id=2", Size: &mid},
			{GUID: "cccccccccccccccccccccccccccccccccccccccc", Title: "Big", Link: "http://x?id=3", Size: &big},
			{GUID: "dddddddddddddddddddddddddddddddddddddddd", Title: "NoSize", Link: "http://x?id=4"},
		}, nil
	})

	eng.poll(ctx)

	// Only "Mid" (150) and "NoSize" (unknown → not filtered) pass.
	if n := rawCount(t, repo, `SELECT count(*) FROM seeds`); n != 2 {
		t.Fatalf("seed count = %d, want 2 (mid + unknown size)", n)
	}
}

// --- monitor ownership test ---

func TestMonitorSameHashDifferentSeedNoRetire(t *testing.T) {
	repo := newTestRepo(t)
	// Two distinct seeds (different source_site) sharing one info_hash, both
	// "seeding" and without any replica rows (historical data). The hash is
	// ambiguous on this qB, so neither may be retired.
	sd1 := &store.Seed{SourceSite: "src1", InfoHash: testHash, Title: "A", Status: "seeding"}
	sd2 := &store.Seed{SourceSite: "src2", InfoHash: testHash, Title: "B", Status: "seeding"}
	if _, err := repo.CreateSeed(ctx, sd1); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateSeed(ctx, sd2); err != nil {
		t.Fatal(err)
	}

	qbMgr := qb.NewManager()
	_, fq := registerFakeQB(t, repo, qbMgr, "qb", 1, []*qb.TorrentInfo{
		completedTorrent(testHash, 15, time.Unix(1_700_000_000, 0).Add(-90*time.Minute).Unix()),
	}, 0)

	eng := New(Config{Workers: 1}, repo, &fakePipeline{}, qbMgr, nil)
	eng.SetClock(func() time.Time { return time.Unix(1_700_000_000, 0) })

	eng.monitor(ctx)

	for _, id := range []int64{sd1.ID, sd2.ID} {
		got, err := repo.GetSeedByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "seeding" {
			t.Fatalf("seed %d status = %q, want seeding (ambiguous hash must not retire)", id, got.Status)
		}
	}
	if fq.deleteCount() != 0 {
		t.Fatalf("delete count = %d, want 0", fq.deleteCount())
	}
}

// --- lifecycle concurrency test ---

func TestEngineStopRestartConcurrent(t *testing.T) {
	// Self-managed temp dir (not t.TempDir): the concurrent Start/Stop churn
	// cancels in-flight SQLite queries, and on Windows modernc/sqlite can lag
	// releasing the WAL/SHM file handles past db.Close. Removal is best-effort
	// with retries so this environment quirk cannot fail the test.
	dir, err := os.MkdirTemp("", "asr-engine-conc-*")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.DB().Exec("PRAGMA wal_checkpoint(TRUNCATE)")
		_ = st.Close()
		for i := 0; i < 100; i++ {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	repo := store.NewRepo(st.DB(), testKey())

	pl := &fakePipeline{}
	eng := New(Config{Workers: 2, PollInterval: time.Hour, MonitorInterval: time.Hour}, repo, pl, qb.NewManager(), nil)
	eng.SetFetchRSS(func(_ context.Context, _ string, _ *http.Client) ([]source.RssItem, error) {
		return nil, nil
	})

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := eng.Start(ctx); err != nil && err != errAlreadyRunning {
				t.Errorf("Start: %v", err)
			}
			if err := eng.Stop(ctx); err != nil {
				t.Errorf("Stop: %v", err)
			}
		}()
	}
	wg.Wait()

	// After all the churn the engine must be fully stopped and cleanly restarted.
	if err := eng.Start(ctx); err != nil {
		t.Fatalf("final Start: %v", err)
	}
	if !eng.Running() {
		t.Fatal("engine not running after final Start")
	}
	if err := eng.Stop(ctx); err != nil {
		t.Fatalf("final Stop: %v", err)
	}
	if eng.Running() {
		t.Fatal("engine still running after final Stop")
	}
}

// --- ResendSeed ---

func TestResendSeedRerun(t *testing.T) {
	repo := newTestRepo(t)
	pl := &fakePipeline{}
	eng := New(Config{Workers: 1}, repo, pl, qb.NewManager(), nil)
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	eng.SetClock(clk.Now)

	sd := &store.Seed{SourceSite: "src", InfoHash: testHash, Title: "T", Status: "failed", RetryCount: 7, Error: "boom"}
	if _, err := repo.CreateSeed(ctx, sd); err != nil {
		t.Fatal(err)
	}

	if err := eng.ResendSeed(ctx, sd.ID, false); err != nil {
		t.Fatalf("ResendSeed: %v", err)
	}

	got, err := repo.GetSeedByID(ctx, sd.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "retry" || got.RetryCount != 0 || got.Error != "" {
		t.Fatalf("seed after rerun = %+v, want retry/0/empty-error", got)
	}

	// Enqueued at retry number 1 (first 60s backoff).
	clk.Advance(60 * time.Second)
	due := eng.retry.Due()
	if len(due) != 1 || due[0].seedID != sd.ID || due[0].retryNo != 1 {
		t.Fatalf("due after rerun = %+v, want seedID=%d retryNo=1", due, sd.ID)
	}

	// Drain into the pipeline so the recording fake observes the call.
	eng.submitJob(ctx, due[0].seedID, due[0].retryNo)
	if got := pl.ids(); len(got) != 1 || got[0] != sd.ID {
		t.Fatalf("pipeline calls = %v, want [%d]", got, sd.ID)
	}
}

func TestResendSeedFullRerun(t *testing.T) {
	repo := newTestRepo(t)
	pl := &fakePipeline{}
	eng := New(Config{Workers: 1}, repo, pl, qb.NewManager(), nil)
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	eng.SetClock(clk.Now)

	sd := &store.Seed{SourceSite: "src", InfoHash: testHash, Title: "T", Status: "failed", RetryCount: 3, Error: "x"}
	if _, err := repo.CreateSeed(ctx, sd); err != nil {
		t.Fatal(err)
	}
	tgt := &store.Target{Name: "t1", Type: "nexusphp", Version: "api", Status: "active"}
	if err := repo.UpsertTarget(ctx, tgt); err != nil {
		t.Fatal(err)
	}
	rec := &store.RelayRecord{SeedID: sd.ID, TargetID: tgt.ID, Role: "publisher", Status: "published"}
	if inserted, err := repo.UpsertRecord(ctx, rec); err != nil || !inserted {
		t.Fatalf("UpsertRecord = (%v,%v), want (true,nil)", inserted, err)
	}
	qbMgr := qb.NewManager()
	qbID, _ := registerFakeQB(t, repo, qbMgr, "qb", 1, nil, 0)
	if err := repo.UpsertReplica(ctx, &store.Replica{SeedID: sd.ID, QBID: qbID, InfoHash: testHash, Role: "origin", Status: "seeding", Progress: 1}); err != nil {
		t.Fatal(err)
	}

	if err := eng.ResendSeed(ctx, sd.ID, true); err != nil {
		t.Fatalf("ResendSeed fullRerun: %v", err)
	}

	if n := rawCount(t, repo, `SELECT count(*) FROM relay_records WHERE seed_id=?`, sd.ID); n != 0 {
		t.Fatalf("relay_records after full rerun = %d, want 0", n)
	}
	if n := rawCount(t, repo, `SELECT count(*) FROM seed_replicas WHERE seed_id=?`, sd.ID); n != 0 {
		t.Fatalf("seed_replicas after full rerun = %d, want 0", n)
	}
	got, err := repo.GetSeedByID(ctx, sd.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "retry" || got.RetryCount != 0 || got.Error != "" {
		t.Fatalf("seed after full rerun = %+v, want retry/0/empty-error", got)
	}
	clk.Advance(60 * time.Second)
	due := eng.retry.Due()
	if len(due) != 1 || due[0].seedID != sd.ID || due[0].retryNo != 1 {
		t.Fatalf("due after full rerun = %+v, want seedID=%d retryNo=1", due, sd.ID)
	}
}

// --- disk / low-speed monitor ---

func TestMonitorDiskCriticalNotifiesAndLogs(t *testing.T) {
	repo := newTestRepo(t)
	qbMgr := qb.NewManager()
	registerFakeQB(t, repo, qbMgr, "qb", 1, nil, 1*1024*1024*1024) // 1 GB free < critical 20 GB

	rec := &recordingNotifier{}
	router := notifier.NewRouter()
	router.Add("rec", rec, notifier.LevelCritical, notifier.LevelWarning)
	eng := New(Config{Workers: 1}, repo, &fakePipeline{}, qbMgr, router)

	eng.monitor(ctx)
	msgs := rec.messages()
	if len(msgs) != 1 {
		t.Fatalf("critical notifications = %d, want 1", len(msgs))
	}
	if msgs[0].Event != "disk" || msgs[0].Level != notifier.LevelCritical {
		t.Fatalf("critical msg = %+v, want event=disk level=critical", msgs[0])
	}
	if n := rawCount(t, repo, `SELECT count(*) FROM activity_log WHERE action='disk_critical'`); n != 1 {
		t.Fatalf("disk_critical log rows = %d, want 1", n)
	}

	// Second round: same critical state → deduped (no re-notify, no re-log).
	eng.monitor(ctx)
	if got := len(rec.messages()); got != 1 {
		t.Fatalf("notifications after 2nd round = %d, want 1 (deduped)", got)
	}
	if n := rawCount(t, repo, `SELECT count(*) FROM activity_log WHERE action='disk_critical'`); n != 1 {
		t.Fatalf("disk_critical log rows after 2nd round = %d, want 1 (deduped)", n)
	}
}

func TestMonitorDiskLowWarns(t *testing.T) {
	repo := newTestRepo(t)
	qbMgr := qb.NewManager()
	registerFakeQB(t, repo, qbMgr, "qb", 1, nil, 30*1024*1024*1024) // 30 GB: low (<50), not critical (>=20)

	rec := &recordingNotifier{}
	router := notifier.NewRouter()
	router.Add("rec", rec, notifier.LevelWarning)
	eng := New(Config{Workers: 1}, repo, &fakePipeline{}, qbMgr, router)

	eng.monitor(ctx)
	router.Flush(ctx)
	msgs := rec.messages()
	if len(msgs) != 1 {
		t.Fatalf("warning notifications = %d, want 1", len(msgs))
	}
	if msgs[0].Event != "disk" || msgs[0].Level != notifier.LevelWarning {
		t.Fatalf("warning msg = %+v, want event=disk level=warning", msgs[0])
	}
}

func TestMonitorLowSpeedAbort(t *testing.T) {
	repo := newTestRepo(t)
	st, err := repo.GetStrategy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st.LowSpeedKbps = 100
	st.LowSpeedDurationSec = 0 // zero duration → IsSlow true on its 2nd call
	st.LowSpeedAction = "abort"
	if err := repo.UpdateStrategy(ctx, st); err != nil {
		t.Fatal(err)
	}

	sd := &store.Seed{SourceSite: "src", InfoHash: testHash, Title: "T", Status: "downloading"}
	if _, err := repo.CreateSeed(ctx, sd); err != nil {
		t.Fatal(err)
	}
	tgt := &store.Target{Name: "t1", Type: "nexusphp", Version: "api", Status: "active"}
	if err := repo.UpsertTarget(ctx, tgt); err != nil {
		t.Fatal(err)
	}
	rec := &store.RelayRecord{SeedID: sd.ID, TargetID: tgt.ID, Role: "publisher", Status: "pending"}
	if inserted, err := repo.UpsertRecord(ctx, rec); err != nil || !inserted {
		t.Fatalf("UpsertRecord = (%v,%v), want (true,nil)", inserted, err)
	}

	qbMgr := qb.NewManager()
	slow := &qb.TorrentInfo{Hash: testHash, Name: "slow", State: "downloading", DLSpeed: 0, Progress: 0.5}
	qbID, fq := registerFakeQB(t, repo, qbMgr, "qb", 1, []*qb.TorrentInfo{slow}, 1000*1024*1024*1024)
	if err := repo.UpsertReplica(ctx, &store.Replica{SeedID: sd.ID, QBID: qbID, InfoHash: testHash, Role: "origin", Status: "downloading", Progress: 0.5}); err != nil {
		t.Fatal(err)
	}

	rec2 := &recordingNotifier{}
	router := notifier.NewRouter()
	router.Add("rec", rec2, notifier.LevelWarning)
	eng := New(Config{Workers: 1}, repo, &fakePipeline{}, qbMgr, router)
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	eng.SetClock(clk.Now)

	// First pass: IsSlow initializes its belowStart timer and returns false.
	eng.monitor(ctx)
	if fq.deleteCount() != 0 {
		t.Fatalf("delete after 1st pass = %d, want 0", fq.deleteCount())
	}

	// Second pass: IsSlow now reports slow → abort.
	eng.monitor(ctx)
	if fq.deleteCount() != 1 {
		t.Fatalf("delete after 2nd pass = %d, want 1", fq.deleteCount())
	}
	gotRec, err := repo.GetRecord(ctx, sd.ID, tgt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRec.Status != "failed" || gotRec.LastError != "low_speed_abort" {
		t.Fatalf("record after abort = %+v, want failed/low_speed_abort", gotRec)
	}
	got, err := repo.GetSeedByID(ctx, sd.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "retry" || got.RetryCount != 1 || got.Error != "low_speed_abort" {
		t.Fatalf("seed after abort = %+v, want retry/1/low_speed_abort", got)
	}

	clk.Advance(60 * time.Second)
	due := eng.retry.Due()
	if len(due) != 1 || due[0].seedID != sd.ID || due[0].retryNo != 1 {
		t.Fatalf("retry queue after abort = %+v, want seedID=%d retryNo=1", due, sd.ID)
	}

	router.Flush(ctx)
	msgs := rec2.messages()
	if len(msgs) != 1 || msgs[0].Event != "low_speed" {
		t.Fatalf("low_speed notifications = %+v, want 1 with event=low_speed", msgs)
	}
}
