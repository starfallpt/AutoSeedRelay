package engine_test

// Full-lifecycle closed-loop verification (BIZ-SPEC §3 nine-step flow) driven
// through the REAL engine + pipeline + adapters + store + notifier, with fake
// network endpoints (httptest) for the source site, the two target sites, the
// qBittorrent WebUI, and the notifier webhook. No product code is modified.
//
// Run: go test ./internal/engine/ -run Lifecycle -count=1
//
// Driving note: the engine's real background loops are exercised via Start/Stop
// (poll → worker → pipeline → monitor → retire → retry queue). The one place a
// test-infrastructure shortcut is taken is the retry re-run in step 5: the
// engine schedules retries with a 60s real-time backoff, so the convergence of
// the partially-failed seed is driven by calling pipeline.Relay directly —
// which is exactly the call the engine's retryLoop makes.

import (
	"context"
	"encoding/json"
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

	"github.com/autoseedrelay/relay/internal/bencode"
	"github.com/autoseedrelay/relay/internal/engine"
	"github.com/autoseedrelay/relay/internal/notifier"
	"github.com/autoseedrelay/relay/internal/pipeline"
	"github.com/autoseedrelay/relay/internal/qb"
	"github.com/autoseedrelay/relay/internal/source"
	"github.com/autoseedrelay/relay/internal/store"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func testKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

func newRepo(t *testing.T) *store.Repo {
	t.Helper()
	// Self-managed temp dir (not t.TempDir): the engine's Start/Stop churn
	// cancels in-flight SQLite queries, and on Windows modernc/sqlite can lag
	// releasing the WAL/SHM file handles past db.Close. Removal is best-effort
	// with retries so this environment quirk cannot fail the test.
	dir, err := os.MkdirTemp("", "asr-lifecycle-*")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "relay.db"))
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

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// fakeClock is a fixed, concurrency-safe time source.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func completedTorrent(hash string, seeders int, completionOn int64) *qb.TorrentInfo {
	return &qb.TorrentInfo{
		Hash: hash, Name: "origin", State: "uploading",
		Progress: 1, Completed: 100, CompletionOn: completionOn,
		Seeders: seeders, Ratio: 1.0,
	}
}

func buildTorrentBytes(t *testing.T, name string, size int64) ([]byte, string) {
	t.Helper()
	info := map[string]any{
		"name": name, "piece length": int64(16384),
		"pieces": string(make([]byte, 20)), "length": size,
	}
	d := map[string]any{"announce": "http://src.example/announce.php?passkey=srcpass", "info": info}
	raw, err := bencode.Encode(d)
	if err != nil {
		t.Fatalf("encode torrent: %v", err)
	}
	h, err := bencode.InfoHash(d)
	if err != nil {
		t.Fatalf("info hash: %v", err)
	}
	return raw, h
}

func torrentMeta(data []byte) (announce string, private any, src string, ok bool) {
	obj, err := bencode.Decode(data)
	if err != nil {
		return "", nil, "", false
	}
	d, ok := obj.(map[string]any)
	if !ok {
		return "", nil, "", false
	}
	announce, _ = d["announce"].(string)
	if info, _ := d["info"].(map[string]any); info != nil {
		private = info["private"]
		src, _ = info["source"].(string)
	}
	return announce, private, src, true
}

func permissiveFetchRSS(ctx context.Context, url string, client *http.Client) ([]source.RssItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rss: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return source.ParseRSS(body)
}

func lifecycleSourceFactory() pipeline.SourceFactory {
	return func(src *store.Source) pipeline.SourceProvider {
		return pipeline.NewSourceProvider(src, nil, pipeline.SourceConfig{
			URLChecker: func(string) error { return nil },
			HTTPClient: &http.Client{},
			FetchRSS:   permissiveFetchRSS,
		})
	}
}

type rssSpec struct {
	guid, title, id, desc string
}

type fakeSource struct {
	srv      *httptest.Server
	mu       sync.Mutex
	items    []rssSpec
	torrents map[string][]byte
}

func newFakeSource(t *testing.T) *fakeSource {
	fs := &fakeSource{torrents: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/rss.xml", fs.handleRSS)
	mux.HandleFunc("/details.php", fs.handleDetail)
	mux.HandleFunc("/viewfilelist.php", fs.handleFileList)
	mux.HandleFunc("/dl", fs.handleDownload)
	fs.srv = httptest.NewServer(mux)
	t.Cleanup(fs.srv.Close)
	return fs
}

func (fs *fakeSource) setItems(items []rssSpec) {
	fs.mu.Lock()
	fs.items = items
	fs.mu.Unlock()
}

func (fs *fakeSource) registerTorrent(guid string, data []byte) {
	fs.mu.Lock()
	fs.torrents[guid] = data
	fs.mu.Unlock()
}

func (fs *fakeSource) handleRSS(w http.ResponseWriter, r *http.Request) {
	fs.mu.Lock()
	items := fs.items
	base := fs.srv.URL
	fs.mu.Unlock()
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><rss version="2.0"><channel>`)
	for _, it := range items {
		fmt.Fprintf(&b,
			`<item><title>%s</title><link>%s/details.php?id=%s</link><description>%s</description><category domain="cat=401">Movies</category><enclosure url="%s/dl?h=%s" length="123456789" type="application/x-bittorrent"/><guid>%s</guid><pubDate>Wed, 01 Aug 2026 00:00:00 +0800</pubDate></item>`,
			it.title, base, it.id, it.desc, base, it.guid, it.guid)
	}
	b.WriteString(`</channel></rss>`)
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(b.String()))
}

func (fs *fakeSource) handleDetail(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte(`<table>
<tr><td class="rowhead">副标题</td><td class="rowfollow">测试副标题</td></tr>
<tr><td class="rowhead">标签</td><td class="rowfollow"><span>国语</span><span>中字</span></td></tr>
<tr><td class="rowhead">促销</td><td class="rowfollow">免费</td></tr>
</table>
<div>大小: 6.70 GB</div>
<div>MediaInfo: H.264 / AVC</div>
<small>IMDb tt1234567</small>`))
}

func (fs *fakeSource) handleFileList(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte(`<table><tr><td class="rowfollow">Movie.mkv</td><td class="rowfollow">6.70 GB</td></tr></table>`))
}

func (fs *fakeSource) handleDownload(w http.ResponseWriter, r *http.Request) {
	h := r.URL.Query().Get("h")
	fs.mu.Lock()
	data := fs.torrents[h]
	fs.mu.Unlock()
	if data == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/x-bittorrent")
	_, _ = w.Write(data)
}

// fakeNP is a NexusPHP >= 1.9 Laravel API target (POST /api/v1/upload).
type fakeNP struct {
	srv *httptest.Server
	mu  sync.Mutex

	fail, dup bool
	uploads   int
	announce  string
	private   any
	sourceTag string
}

func newFakeNP(t *testing.T) *fakeNP {
	f := &fakeNP{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/upload", f.handleUpload)
	mux.HandleFunc("/api/v1/sections", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeNP) handleUpload(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.uploads++
	fail, dup := f.fail, f.dup
	f.mu.Unlock()

	_ = r.ParseMultipartForm(32 << 20)
	if file, _, err := r.FormFile("file"); err == nil {
		data, _ := io.ReadAll(file)
		if a, p, s, ok := torrentMeta(data); ok {
			f.mu.Lock()
			f.announce, f.private, f.sourceTag = a, p, s
			f.mu.Unlock()
		}
	}

	if fail {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	if dup {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"torrent already exists"}`))
		return
	}
	f.mu.Lock()
	id := f.uploads
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"data":{"id":%d}}`, 1000+id)
}

func (f *fakeNP) set(fail, dup bool) {
	f.mu.Lock()
	f.fail, f.dup = fail, dup
	f.mu.Unlock()
}

// fakeClassic is a legacy NexusPHP form target (upload.php -> takeupload.php).
type fakeClassic struct {
	srv *httptest.Server
	mu  sync.Mutex

	fail, dup bool
	uploads   int
	announce  string
	private   any
	sourceTag string
}

func newFakeClassic(t *testing.T) *fakeClassic {
	f := &fakeClassic{}
	mux := http.NewServeMux()
	mux.HandleFunc("/upload.php", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<form action="takeupload.php"><input type="hidden" name="auth_token" value="tok123"></form>`))
	})
	mux.HandleFunc("/takeupload.php", f.handleTakeupload)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeClassic) handleTakeupload(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.uploads++
	fail, dup := f.fail, f.dup
	f.mu.Unlock()

	_ = r.ParseMultipartForm(32 << 20)
	if file, _, err := r.FormFile("file"); err == nil {
		data, _ := io.ReadAll(file)
		if a, p, s, ok := torrentMeta(data); ok {
			f.mu.Lock()
			f.announce, f.private, f.sourceTag = a, p, s
			f.mu.Unlock()
		}
	}

	if fail {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	if dup {
		_, _ = w.Write([]byte("种子已存在"))
		return
	}
	w.Header().Set("Location", "/details.php?id=789")
	w.WriteHeader(http.StatusFound)
}

func (f *fakeClassic) set(fail, dup bool) {
	f.mu.Lock()
	f.fail, f.dup = fail, dup
	f.mu.Unlock()
}

// lifecycleQB emulates the qB WebUI surface the pipeline + monitor touch.
type lifecycleQB struct {
	srv *httptest.Server
	mu  sync.Mutex

	torrents     []*qb.TorrentInfo
	freeDisk     int64
	deleted      []string
	fileAdds     int
	skipChecking string
}

func newLifecycleQB(t *testing.T) *lifecycleQB {
	f := &lifecycleQB{freeDisk: 1 << 40}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/api/v2/app/version", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("v5.0.0")) })
	mux.HandleFunc("/api/v2/torrents/info", f.handleInfo)
	mux.HandleFunc("/api/v2/torrents/add", f.handleAdd)
	mux.HandleFunc("/api/v2/torrents/delete", f.handleDelete)
	mux.HandleFunc("/api/v2/sync/maindata", f.handleMaindata)
	mux.HandleFunc("/api/v2/transfer/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"dl_info_speed":0,"up_info_speed":0,"dl_info_data":0,"up_info_data":0}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *lifecycleQB) handleInfo(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	hashes := r.URL.Query().Get("hashes")
	out := f.torrents
	if hashes != "" {
		var filtered []*qb.TorrentInfo
		for _, t := range f.torrents {
			if strings.EqualFold(t.Hash, hashes) {
				filtered = append(filtered, t)
			}
		}
		out = filtered
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (f *lifecycleQB) handleAdd(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		_, _ = w.Write([]byte("Fails."))
		return
	}
	_ = r.ParseMultipartForm(32 << 20)
	f.mu.Lock()
	if r.MultipartForm != nil {
		if vs := r.MultipartForm.Value["skip_checking"]; len(vs) > 0 {
			f.skipChecking = vs[0]
		}
	}
	f.fileAdds++
	f.mu.Unlock()

	if file, _, err := r.FormFile("torrents"); err == nil {
		data, _ := io.ReadAll(file)
		if obj, err := bencode.Decode(data); err == nil {
			if d, ok := obj.(map[string]any); ok {
				if h, err := bencode.InfoHash(d); err == nil {
					f.mu.Lock()
					f.torrents = append(f.torrents, &qb.TorrentInfo{
						Hash: h, Name: "cross", State: "downloading",
						Progress: 1, Completed: 0, CompletionOn: -1,
					})
					f.mu.Unlock()
				}
			}
		}
	}
	_, _ = w.Write([]byte("Ok."))
}

func (f *lifecycleQB) handleDelete(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, r.PostFormValue("hashes"))
	w.WriteHeader(http.StatusOK)
}

func (f *lifecycleQB) handleMaindata(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	fd := f.freeDisk
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"server_state":{"free_space_on_disk":%d}}`, fd)
}

func (f *lifecycleQB) addTorrent(t *qb.TorrentInfo) {
	f.mu.Lock()
	f.torrents = append(f.torrents, t)
	f.mu.Unlock()
}

func (f *lifecycleQB) deleteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deleted)
}

func (f *lifecycleQB) fileAddCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fileAdds
}

func (f *lifecycleQB) skipCheckingFlag() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.skipChecking
}

// webhookRecorder captures the JSON bodies the webhook notifier posts.
type webhookRecorder struct {
	srv *httptest.Server
	mu  sync.Mutex
	raw []string
}

func newWebhookRecorder(t *testing.T) *webhookRecorder {
	w := &webhookRecorder{}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.mu.Lock()
		w.raw = append(w.raw, string(body))
		w.mu.Unlock()
		rw.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *webhookRecorder) joined() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.raw, "\n")
}

func (w *webhookRecorder) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.raw)
}

func setStrategy(t *testing.T, repo *store.Repo, mutate func(*store.Strategy)) {
	t.Helper()
	st, err := repo.GetStrategy(context.Background())
	if err != nil {
		t.Fatalf("GetStrategy: %v", err)
	}
	mutate(st)
	if err := repo.UpdateStrategy(context.Background(), st); err != nil {
		t.Fatalf("UpdateStrategy: %v", err)
	}
}

func targetByName(t *testing.T, repo *store.Repo, name string) *store.Target {
	t.Helper()
	targets, err := repo.GetEnabledTargets(context.Background())
	if err != nil {
		t.Fatalf("GetEnabledTargets: %v", err)
	}
	for _, tg := range targets {
		if tg.Name == name {
			return tg
		}
	}
	t.Fatalf("target %q not found", name)
	return nil
}

// harness carries every piece shared across the lifecycle steps.
type harness struct {
	repo *store.Repo
	fs   *fakeSource
	np   *fakeNP
	cls  *fakeClassic
	qbf  *lifecycleQB
	hook *webhookRecorder

	qbMgr  *qb.Manager
	router *notifier.Router
	eng    *engine.Engine
	pl     *pipeline.RelayOne
	clk    *fakeClock

	hashA, hashB, hashC, hashD, hashE string
}

func newHarness(t *testing.T) *harness {
	h := &harness{}
	h.repo = newRepo(t)

	rawA, hashA := buildTorrentBytes(t, "RelayTarget.Movie.2026.mkv", 123456789)
	rawB, hashB := buildTorrentBytes(t, "Other.Movie.2026.mkv", 123456789)
	rawC, hashC := buildTorrentBytes(t, "RelayTarget.Series.2026.mkv", 123456789)
	rawD, hashD := buildTorrentBytes(t, "RelayTarget.Doc.2026.mkv", 123456789)
	rawE, hashE := buildTorrentBytes(t, "RelayTarget.Show.2026.mkv", 123456789)
	h.hashA, h.hashB, h.hashC, h.hashD, h.hashE = hashA, hashB, hashC, hashD, hashE

	h.fs = newFakeSource(t)
	h.fs.registerTorrent(hashA, rawA)
	h.fs.registerTorrent(hashB, rawB)
	h.fs.registerTorrent(hashC, rawC)
	h.fs.registerTorrent(hashD, rawD)
	h.fs.registerTorrent(hashE, rawE)

	h.np = newFakeNP(t)
	h.cls = newFakeClassic(t)
	h.qbf = newLifecycleQB(t)
	h.hook = newWebhookRecorder(t)

	src := &store.Source{Name: "src", Role: "source", BaseURL: h.fs.srv.URL, RSSURL: h.fs.srv.URL + "/rss.xml", Status: "active"}
	if err := h.repo.UpsertSource(context.Background(), src); err != nil {
		t.Fatalf("upsert source: %v", err)
	}

	t1 := &store.Target{
		Name: "t1", Type: "nexusphp", Version: "api", BaseURL: h.np.srv.URL,
		AnnounceURL: "http://t1.example/announce.php?passkey={passkey}", Passkey: "np-passkey",
		APIToken: "np-token", CategoryOverrides: `{"movies":401}`, FallbackCategory: "401", Status: "active",
	}
	if err := h.repo.UpsertTarget(context.Background(), t1); err != nil {
		t.Fatalf("upsert t1: %v", err)
	}
	t2 := &store.Target{
		Name: "t2", Type: "nexusphp_classic", Version: "classic", BaseURL: h.cls.srv.URL,
		AnnounceURL: "http://t2.example/announce.php?passkey={passkey}", Passkey: "classic-passkey",
		Cookie: "uid=1; pass=abc", CategoryOverrides: `{"movies":401}`, FallbackCategory: "401", Status: "active",
	}
	if err := h.repo.UpsertTarget(context.Background(), t2); err != nil {
		t.Fatalf("upsert t2: %v", err)
	}

	setStrategy(t, h.repo, func(st *store.Strategy) {
		st.Keywords = `["RelayTarget"]`
		st.Promotions = `["free"]`
		st.MinSize = 0
		st.MaxSize = 0
		st.RetireSeeders = 0
		st.RetireMinutes = 0
		st.RetireRatioEnabled = 0
		st.RetireMode = "and"
		st.RetryMax = 3
		st.DispatchMode = "priority"
		st.DiskLowGB = 0
		st.DiskCriticalGB = 0
		st.LowSpeedKbps = 100
		st.LowSpeedDurationSec = 600
		st.LowSpeedAction = "" // observe only
	})

	qbinfo := &store.QBInstance{Name: "qb1", Host: h.qbf.srv.URL, Port: 0, Username: "admin", Password: "adminpass", Priority: 1, Enabled: 1}
	if err := h.repo.UpsertQBInstance(context.Background(), qbinfo); err != nil {
		t.Fatalf("upsert qb: %v", err)
	}
	h.qbMgr = qb.NewManager()
	h.qbMgr.Set("qb1", qb.NewInstance(h.qbf.srv.URL, "", "admin", "adminpass", qb.WithHTTPClient(h.qbf.srv.Client())))

	wh, err := notifier.New(notifier.Config{Type: notifier.TypeWebhook, WebhookURL: h.hook.srv.URL})
	if err != nil {
		t.Fatalf("notifier.New: %v", err)
	}
	h.router = notifier.NewRouter()
	h.router.Add("hook", wh, notifier.LevelInfo, notifier.LevelWarning, notifier.LevelCritical)

	h.clk = &fakeClock{t: time.Unix(1_700_000_000, 0)}
	h.eng = engine.New(engine.Config{Workers: 2, PollInterval: 200 * time.Millisecond, MonitorInterval: 100 * time.Millisecond}, h.repo, nil, h.qbMgr, h.router)
	h.eng.SetClock(h.clk.Now)
	h.eng.SetFetchRSS(permissiveFetchRSS)

	h.pl = pipeline.New(pipeline.Deps{
		Repo:                 h.repo,
		QB:                   h.qbMgr,
		Notifier:             h.router,
		Source:               lifecycleSourceFactory(),
		Adapters:             nil,
		QBSelector:           h.eng.Dispatcher(),
		WorkDir:              t.TempDir(),
		Now:                  h.clk.Now,
		MaxTargetConcurrency: 4,
		CrossSeedTimeout:     5 * time.Second,
		CrossSeedInterval:    10 * time.Millisecond,
	})
	h.eng.SetPipeline(h.pl)

	return h
}

// start starts the engine and registers a cleanup that stops it (idempotent),
// so a mid-scenario assertion failure never leaks the background loops.
func (h *harness) start(t *testing.T) {
	t.Helper()
	if err := h.eng.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	t.Cleanup(func() { _ = h.eng.Stop(context.Background()) })
}

// waitSeed blocks until a seed with the given info_hash exists, then returns it.
func waitSeed(t *testing.T, repo *store.Repo, hash string) *store.Seed {
	t.Helper()
	var sd *store.Seed
	waitFor(t, 10*time.Second, "seed created hash="+hash, func() bool {
		s, err := repo.GetSeedByHash(context.Background(), "src", hash)
		if err != nil {
			return false
		}
		sd = s
		return true
	})
	return sd
}

func listRecords(t *testing.T, repo *store.Repo, seedID int64) []*store.RelayRecord {
	t.Helper()
	recs, err := repo.ListRecordsBySeed(context.Background(), seedID)
	if err != nil {
		t.Fatalf("ListRecordsBySeed: %v", err)
	}
	return recs
}

// ---------------------------------------------------------------------------
// the lifecycle
// ---------------------------------------------------------------------------

func TestLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// ---- step 1 + 2: poll → keyword filter → discover → worker → publish ----
	t.Log("STEP 1: poll / keyword filter / discover / enqueue")
	h.fs.setItems([]rssSpec{
		{guid: h.hashA, title: "RelayTarget.Movie.2026.2160p.WEB-DL.H264", id: "169620", desc: "hit"},
		{guid: h.hashB, title: "Other.Movie.2026.1080p", id: "169621", desc: "no keyword"},
	})
	h.start(t)

	waitFor(t, 10*time.Second, "hit seed created", func() bool {
		_, err := h.repo.GetSeedByHash(ctx, "src", h.hashA)
		return err == nil
	})
	sdA, err := h.repo.GetSeedByHash(ctx, "src", h.hashA)
	if err != nil {
		t.Fatalf("GetSeedByHash(hit): %v", err)
	}
	if sdA.DiscoveredAt == 0 {
		t.Fatalf("hit seed discovered_at = 0, want set (created via discovered flow)")
	}
	if n := rawCount(t, h.repo, `SELECT count(*) FROM seeds WHERE info_hash=?`, h.hashB); n != 0 {
		t.Fatalf("non-hit seed = %d, want 0 (keyword filter)", n)
	}
	if n := rawCount(t, h.repo, `SELECT count(*) FROM seen_hashes WHERE info_hash=?`, h.hashA); n != 1 {
		t.Fatalf("hit tombstone = %d, want 1", n)
	}
	if n := rawCount(t, h.repo, `SELECT count(*) FROM seen_hashes WHERE info_hash=?`, h.hashB); n != 0 {
		t.Fatalf("non-hit tombstone = %d, want 0 (filtered before dedup)", n)
	}
	t.Log("STEP 1: PASS (hit seed discovered; non-hit filtered; tombstone written)")

	t.Log("STEP 2: download / parse / clean / publish to both targets")
	waitFor(t, 15*time.Second, "seed A seeding", func() bool {
		s, err := h.repo.GetSeedByID(ctx, sdA.ID)
		return err == nil && s.Status == "seeding"
	})
	sdA, _ = h.repo.GetSeedByID(ctx, sdA.ID)
	if sdA.Promotion != "免费" {
		t.Fatalf("seed promotion = %q, want 免费 (persisted from detail)", sdA.Promotion)
	}
	recs := listRecords(t, h.repo, sdA.ID)
	if len(recs) != 2 {
		t.Fatalf("relay records = %d, want 2", len(recs))
	}
	for _, rec := range recs {
		if rec.Status != "published" || rec.Role != "publisher" || rec.PublishedAt == 0 {
			t.Fatalf("record = %+v, want published/publisher with published_at set", rec)
		}
	}
	if !strings.Contains(h.np.announce, "passkey=np-passkey") {
		t.Fatalf("t1 announce = %q, want passkey=np-passkey", h.np.announce)
	}
	if !strings.Contains(h.cls.announce, "passkey=classic-passkey") {
		t.Fatalf("t2 announce = %q, want passkey=classic-passkey", h.cls.announce)
	}
	if fmt.Sprint(h.np.private) != "1" || fmt.Sprint(h.cls.private) != "1" {
		t.Fatalf("private = %v/%v, want 1/1", h.np.private, h.cls.private)
	}
	if h.np.sourceTag != "[src]" || h.cls.sourceTag != "[src]" {
		t.Fatalf("source = %q/%q, want [src]/[src]", h.np.sourceTag, h.cls.sourceTag)
	}
	t.Log("STEP 2: PASS (seed seeding; both records published; announce/private/source cleaned)")
	_ = h.eng.Stop(ctx)

	// ---- step 3: cross-seed on duplicate ----
	t.Log("STEP 3: cross-seed path on target duplicate")
	h.np.set(false, false)
	h.cls.set(false, true) // classic reports "already exists"
	h.fs.setItems([]rssSpec{
		{guid: h.hashC, title: "RelayTarget.Series.2026.2160p", id: "169630", desc: "cross-seed"},
	})
	h.start(t)

	sdC := waitSeed(t, h.repo, h.hashC)
	waitFor(t, 15*time.Second, "seed C seeding", func() bool {
		s, err := h.repo.GetSeedByID(ctx, sdC.ID)
		return err == nil && s.Status == "seeding"
	})
	t1, t2 := targetByName(t, h.repo, "t1"), targetByName(t, h.repo, "t2")
	recT1, _ := h.repo.GetRecord(ctx, sdC.ID, t1.ID)
	recT2, _ := h.repo.GetRecord(ctx, sdC.ID, t2.ID)
	if recT1.Status != "published" || recT1.Role != "publisher" {
		t.Fatalf("t1 record = %+v, want published/publisher", recT1)
	}
	if recT2.Status != "cross_seeding" || recT2.Role != "seeder" {
		t.Fatalf("t2 record = %+v, want cross_seeding/seeder", recT2)
	}
	reps, err := h.repo.ListReplicas(ctx, sdC.ID)
	if err != nil || len(reps) != 1 || reps[0].Role != "cross" || !strings.EqualFold(reps[0].InfoHash, h.hashC) {
		t.Fatalf("replicas = %+v err=%v, want 1 cross replica", reps, err)
	}
	if h.qbf.fileAddCount() != 1 || h.qbf.skipCheckingFlag() != "true" {
		t.Fatalf("qB file adds = %d skip_checking=%q, want 1/true", h.qbf.fileAddCount(), h.qbf.skipCheckingFlag())
	}
	t.Log("STEP 3: PASS (t1 published; t2 cross_seeding/seeder; cross replica + qB skip_checking add)")
	_ = h.eng.Stop(ctx)
	h.cls.set(false, false)

	// ---- step 4: monitor retire ----
	t.Log("STEP 4: monitor triggers retire")
	seedA, err := h.repo.GetSeedByHash(ctx, "src", h.hashA)
	if err != nil {
		t.Fatalf("GetSeedByHash(A): %v", err)
	}
	h.qbf.addTorrent(completedTorrent(h.hashA, 15, h.clk.Now().Add(-2*time.Minute).Unix()))
	h.fs.setItems(nil)
	h.start(t)

	waitFor(t, 15*time.Second, "seed A retired", func() bool {
		s, err := h.repo.GetSeedByID(ctx, seedA.ID)
		return err == nil && s.Status == "retired"
	})
	for _, rec := range listRecords(t, h.repo, seedA.ID) {
		if rec.Status != "retired" || rec.RetiredAt == 0 || rec.RetireReason == "" {
			t.Fatalf("record = %+v, want retired with retired_at/reason", rec)
		}
	}
	if h.qbf.deleteCount() != 1 {
		t.Fatalf("qB delete count = %d, want 1", h.qbf.deleteCount())
	}
	t.Log("STEP 4: PASS (seed + records retired; qB delete issued)")
	_ = h.eng.Stop(ctx)

	// ---- step 5: partial failure → retry → converge ----
	t.Log("STEP 5: partial failure → engine retry → converge")
	h.np.set(false, false)
	h.cls.set(true, false) // classic -> HTTP 500 on first attempt
	h.fs.setItems([]rssSpec{
		{guid: h.hashD, title: "RelayTarget.Doc.2026.2160p", id: "169640", desc: "retry"},
	})
	h.start(t)

	sdD := waitSeed(t, h.repo, h.hashD)
	waitFor(t, 15*time.Second, "seed D retry", func() bool {
		s, err := h.repo.GetSeedByID(ctx, sdD.ID)
		return err == nil && s.Status == "retry"
	})
	gotD, _ := h.repo.GetSeedByID(ctx, sdD.ID)
	if gotD.RetryCount != 1 {
		t.Fatalf("retry_count = %d, want 1", gotD.RetryCount)
	}
	recT1, _ = h.repo.GetRecord(ctx, sdD.ID, t1.ID)
	recT2, _ = h.repo.GetRecord(ctx, sdD.ID, t2.ID)
	if recT1.Status != "published" {
		t.Fatalf("t1 record = %+v, want published", recT1)
	}
	if recT2.Status != "failed" {
		t.Fatalf("t2 record = %+v, want failed", recT2)
	}
	_ = h.eng.Stop(ctx)

	// Target recovers. The engine would re-run after a 60s backoff; the re-run
	// is invoked directly here (the exact call the retryLoop makes) so the test
	// does not sleep for the real backoff.
	h.cls.set(false, false)
	if err := h.pl.Relay(ctx, sdD.ID); err != nil {
		t.Fatalf("retry Relay: %v", err)
	}
	gotD, _ = h.repo.GetSeedByID(ctx, sdD.ID)
	if gotD.Status != "seeding" {
		t.Fatalf("seed status after retry = %q, want seeding", gotD.Status)
	}
	for _, rec := range listRecords(t, h.repo, sdD.ID) {
		if rec.Status != "published" {
			t.Fatalf("record after retry = %+v, want published", rec)
		}
	}
	t.Log("STEP 5: PASS (partial failure detected, retry_count=1, converge to seeding with both published)")

	// ---- step 6: notifications ----
	t.Log("STEP 6: notification webhook received events")
	h.router.Flush(ctx)
	joined := h.hook.joined()
	for _, want := range []string{"发布成功", "交叉辅种", "自动撤种", "发布失败"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("webhook output missing %q; got:\n%s", want, joined)
		}
	}
	if h.hook.count() == 0 {
		t.Fatal("webhook received no posts")
	}
	t.Log("STEP 6: PASS (publish / cross-seed / retire / publish-failure events all delivered)")

	// ---- step 7: tombstone survives replay ----
	t.Log("STEP 7: retired seed is not re-created on RSS replay")
	h.np.set(false, false)
	h.cls.set(false, false)
	h.fs.setItems([]rssSpec{
		{guid: h.hashA, title: "RelayTarget.Movie.2026.2160p.WEB-DL.H264", id: "169620", desc: "replay"},
		{guid: h.hashE, title: "RelayTarget.Show.2026.2160p", id: "169650", desc: "fresh"},
	})
	h.start(t)

	// hashE is fresh and must be ingested (proving the poll ran); hashA is
	// tombstoned and must NOT be re-created.
	waitFor(t, 15*time.Second, "fresh seed E created", func() bool {
		_, err := h.repo.GetSeedByHash(ctx, "src", h.hashE)
		return err == nil
	})
	if n := rawCount(t, h.repo, `SELECT count(*) FROM seeds WHERE info_hash=?`, h.hashA); n != 1 {
		t.Fatalf("hashA seeds after replay = %d, want 1 (no re-create)", n)
	}
	sdA, _ = h.repo.GetSeedByHash(ctx, "src", h.hashA)
	if sdA.Status != "retired" {
		t.Fatalf("hashA status after replay = %q, want retired (not reset)", sdA.Status)
	}
	t.Log("STEP 7: PASS (tombstone holds; retired seed not re-created nor re-enqueued)")
	_ = h.eng.Stop(ctx)
}
