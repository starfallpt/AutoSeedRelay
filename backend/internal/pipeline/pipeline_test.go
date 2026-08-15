package pipeline

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autoseedrelay/relay/internal/bencode"
	"github.com/autoseedrelay/relay/internal/notifier"
	"github.com/autoseedrelay/relay/internal/parser"
	"github.com/autoseedrelay/relay/internal/qb"
	"github.com/autoseedrelay/relay/internal/source"
	"github.com/autoseedrelay/relay/internal/store"
)

// ---------------------------------------------------------------------------
// test helpers
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
	path := filepath.Join(t.TempDir(), "relay.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return store.NewRepo(st.DB(), testKey())
}

// buildTorrent builds a minimal valid single-file .torrent and returns its
// raw bytes plus the computed info_hash.
func buildTorrent(t *testing.T, name string) ([]byte, string) {
	t.Helper()
	info := map[string]any{
		"name":         name,
		"piece length": int64(16384),
		"pieces":       string(make([]byte, 20)),
		"length":       int64(123456789),
	}
	d := map[string]any{
		"announce": "http://src.example/announce.php?passkey=srcpass",
		"info":     info,
	}
	raw, err := bencode.Encode(d)
	if err != nil {
		t.Fatalf("encode torrent: %v", err)
	}
	p, err := parser.ParseTorrent(raw)
	if err != nil {
		t.Fatalf("parse torrent: %v", err)
	}
	return raw, p.InfoHash
}

// permissiveFetchRSS bypasses source.FetchRSS's SSRF check so the pipeline can
// talk to a loopback httptest server.
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
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rss: HTTP %d", resp.StatusCode)
	}
	return source.ParseRSS(body)
}

func permissiveSourceFactory() SourceFactory {
	return func(src *store.Source) SourceProvider {
		return NewSourceProvider(src, nil, SourceConfig{
			URLChecker: func(string) error { return nil },
			HTTPClient: &http.Client{},
			FetchRSS:   permissiveFetchRSS,
		})
	}
}

// fakeSource serves RSS, detail pages and the .torrent download.
type fakeSource struct {
	srv        *httptest.Server
	torrent    []byte
	infoHash   string
	detailFail bool
}

func newFakeSource(t *testing.T, torrent []byte, infoHash string) *fakeSource {
	fs := &fakeSource{torrent: torrent, infoHash: infoHash}
	mux := http.NewServeMux()
	mux.HandleFunc("/rss.xml", func(w http.ResponseWriter, r *http.Request) {
		body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><item>
<title>Test.Movie.2026.2160p.WEB-DL.HEVC.DDP</title>
<link>%s/details.php?id=169620</link>
<description>Test Movie description IMDb tt1234567</description>
<category domain="cat=401">Movies</category>
<enclosure url="%s/download" length="123456789" type="application/x-bittorrent"/>
<guid>%s</guid>
<pubDate>Wed, 01 Aug 2026 00:00:00 +0800</pubDate>
</item></channel></rss>`, fs.srv.URL, fs.srv.URL, infoHash)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/details.php", func(w http.ResponseWriter, r *http.Request) {
		if fs.detailFail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`<table>
<tr><td class="rowhead">副标题</td><td class="rowfollow">测试副标题</td></tr>
<tr><td class="rowhead">标签</td><td class="rowfollow"><span>国语</span><span>中字</span></td></tr>
</table>
<small>IMDb tt1234567</small>`))
	})
	mux.HandleFunc("/viewfilelist.php", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<table><tr><td class="rowfollow">Test.Movie.2026.mkv</td><td class="rowfollow">6.70 GB</td></tr></table>`))
	})
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-bittorrent")
		_, _ = w.Write(fs.torrent)
	})
	fs.srv = httptest.NewServer(mux)
	t.Cleanup(fs.srv.Close)
	return fs
}

// fakeQB emulates the qB WebUI endpoints the pipeline touches. A URL add
// (form-encoded) fails so the source download exercises the direct fallback;
// a file add (multipart) succeeds and records skip_checking.
type fakeQB struct {
	srv *httptest.Server
	mu  sync.Mutex

	infoBody     string
	skipChecking string
	fileAdds     int
}

func newFakeQB(t *testing.T) *fakeQB {
	f := &fakeQB{infoBody: "[]"}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/torrents/add", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			_ = r.ParseMultipartForm(10 << 20)
			f.mu.Lock()
			if r.MultipartForm != nil {
				if vs := r.MultipartForm.Value["skip_checking"]; len(vs) > 0 {
					f.skipChecking = vs[0]
				}
			}
			f.fileAdds++
			f.mu.Unlock()
			_, _ = w.Write([]byte("Ok."))
			return
		}
		// URL add (source qB direct-pull) → fail to force the direct fallback.
		_, _ = w.Write([]byte("Fails."))
	})
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		body := f.infoBody
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/api/v2/torrents/delete", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("Ok."))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeQB) setInfo(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infoBody = body
}

func (f *fakeQB) addState() (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.skipChecking, f.fileAdds
}

// recordingNotifier captures delivered messages.
type recordingNotifier struct {
	mu   sync.Mutex
	msgs []notifier.Message
}

func (n *recordingNotifier) Send(_ context.Context, msg notifier.Message) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.msgs = append(n.msgs, msg)
	return nil
}

func (n *recordingNotifier) messages() []notifier.Message {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]notifier.Message, len(n.msgs))
	copy(out, n.msgs)
	return out
}

// insertSource + insertSeed + insertTarget helpers keep tests terse.
func insertSource(t *testing.T, repo *store.Repo, baseURL string) *store.Source {
	t.Helper()
	src := &store.Source{Name: "src", Role: "source", BaseURL: baseURL, RSSURL: baseURL + "/rss.xml", Status: "active"}
	if err := repo.UpsertSource(context.Background(), src); err != nil {
		t.Fatalf("upsert source: %v", err)
	}
	return src
}

func insertSeed(t *testing.T, repo *store.Repo, srcID int64, infoHash, title string) *store.Seed {
	t.Helper()
	seed := &store.Seed{SourceSite: "src", InfoHash: infoHash, Title: title, SourceID: srcID, Status: "downloaded"}
	if _, err := repo.CreateSeed(context.Background(), seed); err != nil {
		t.Fatalf("create seed: %v", err)
	}
	return seed
}

func insertTarget(t *testing.T, repo *store.Repo, tgt *store.Target) *store.Target {
	t.Helper()
	if err := repo.UpsertTarget(context.Background(), tgt); err != nil {
		t.Fatalf("upsert target: %v", err)
	}
	return tgt
}

func newTestPipeline(repo *store.Repo, qbm *qb.Manager, rt *notifier.Router, srcFactory SourceFactory, now func() time.Time) *RelayOne {
	return New(Deps{
		Repo:                 repo,
		QB:                   qbm,
		Notifier:             rt,
		Source:               srcFactory,
		Adapters:             nil, // default adapters.New
		Now:                  now,
		MaxTargetConcurrency: 4,
		CrossSeedTimeout:     50 * time.Millisecond,
		CrossSeedInterval:    2 * time.Millisecond,
	})
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestRelayAllTargetsSuccess(t *testing.T) {
	repo := newRepo(t)
	raw, infoHash := buildTorrent(t, "Test.Movie.2026.mkv")
	fs := newFakeSource(t, raw, infoHash)

	src := insertSource(t, repo, fs.srv.URL)
	seed := insertSeed(t, repo, src.ID, infoHash, "Test.Movie.2026.2160p.WEB-DL.HEVC.DDP")

	nexus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/upload" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":123}}`))
	}))
	defer nexus.Close()

	mteamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/torrent/createOredit" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"id":456}}`))
	}))
	defer mteamSrv.Close()

	classic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/upload.php":
			_, _ = w.Write([]byte(`<form action="takeupload.php"></form>`))
		case "/takeupload.php":
			w.Header().Set("Location", "/details.php?id=789")
			w.WriteHeader(http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer classic.Close()

	insertTarget(t, repo, &store.Target{
		Name: "t1", Type: "nexusphp", Version: "api", BaseURL: nexus.URL,
		AnnounceURL: "http://t1/announce.php?passkey={passkey}", Passkey: "p1", APIToken: "tok",
		CategoryOverrides: `{"movie":401}`, DimensionOverrides: `{"standard":{"2160":4}}`, Status: "active",
	})
	insertTarget(t, repo, &store.Target{
		Name: "t2", Type: "mteam", Version: "api", BaseURL: mteamSrv.URL,
		AnnounceURL: "http://t2/announce?credential={credential}", APIToken: "key",
		CategoryOverrides: `{"movie":5}`, Status: "active",
	})
	insertTarget(t, repo, &store.Target{
		Name: "t3", Type: "nexusphp_classic", Version: "classic", BaseURL: classic.URL,
		AnnounceURL: "http://t3/announce.php?passkey={passkey}", Cookie: "uid=1",
		CategoryOverrides: `{"movie":401}`, Status: "active",
	})

	p := newTestPipeline(repo, qb.NewManager(), nil, permissiveSourceFactory(), nil)
	if err := p.Relay(context.Background(), seed.ID); err != nil {
		t.Fatalf("Relay: %v", err)
	}

	got, err := repo.GetSeedByID(context.Background(), seed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusRelayed {
		t.Fatalf("seed status = %q, want %q", got.Status, statusRelayed)
	}

	targets, _ := repo.GetEnabledTargets(context.Background())
	if len(targets) != 3 {
		t.Fatalf("targets = %d, want 3", len(targets))
	}
	for _, tgt := range targets {
		rec, err := repo.GetRecord(context.Background(), seed.ID, tgt.ID)
		if err != nil {
			t.Fatalf("record for %s: %v", tgt.Name, err)
		}
		if rec.Status != statusPublished || rec.Role != rolePublisher {
			t.Fatalf("record for %s = %+v, want published/publisher", tgt.Name, rec)
		}
	}
}

func TestRelayCrossSeed(t *testing.T) {
	repo := newRepo(t)
	raw, infoHash := buildTorrent(t, "Test.Movie.2026.mkv")
	fs := newFakeSource(t, raw, infoHash)

	src := insertSource(t, repo, fs.srv.URL)
	seed := insertSeed(t, repo, src.ID, infoHash, "Test.Movie.2026.2160p.WEB-DL.HEVC.DDP")

	// Target reports duplicate (HTTP 409 → ErrDuplicate).
	dupTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"torrent already exists"}`))
	}))
	defer dupTarget.Close()
	tgt := insertTarget(t, repo, &store.Target{
		Name: "t1", Type: "nexusphp", Version: "api", BaseURL: dupTarget.URL,
		AnnounceURL: "http://t1/announce.php?passkey={passkey}", Passkey: "p1", APIToken: "tok",
		CategoryOverrides: `{"movie":401}`, Status: "active",
	})

	qbfake := newFakeQB(t)
	qbfake.setInfo(`[{"hash":"` + infoHash + `","name":"t","state":"stalledUP","progress":1,"completed":100,"completion_on":12345}]`)

	// Register a QBInstance row (needed for the replica FK) and a manager
	// instance under the same name.
	qbinfo := &store.QBInstance{Name: "qb1", Host: qbfake.srv.URL, Port: 0, Username: "admin", Password: "adminpass", Enabled: 1}
	if err := repo.UpsertQBInstance(context.Background(), qbinfo); err != nil {
		t.Fatal(err)
	}
	mgr := qb.NewManager()
	mgr.Set("qb1", qb.NewInstance(qbfake.srv.URL, "", "admin", "adminpass"))

	p := newTestPipeline(repo, mgr, nil, permissiveSourceFactory(), nil)
	if err := p.Relay(context.Background(), seed.ID); err != nil {
		t.Fatalf("Relay: %v", err)
	}

	got, _ := repo.GetSeedByID(context.Background(), seed.ID)
	if got.Status != statusRelayed {
		t.Fatalf("seed status = %q, want %q", got.Status, statusRelayed)
	}

	rec, err := repo.GetRecord(context.Background(), seed.ID, tgt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != statusCrossSeeded || rec.Role != roleSeeder {
		t.Fatalf("record = %+v, want cross_seeding/seeder", rec)
	}

	replicas, err := repo.ListReplicas(context.Background(), seed.ID)
	if err != nil || len(replicas) != 1 {
		t.Fatalf("replicas = %+v, err=%v, want 1 cross replica", replicas, err)
	}
	if replicas[0].Role != replicaCross || replicas[0].InfoHash != infoHash || replicas[0].QBID != qbinfo.ID {
		t.Fatalf("replica = %+v, want cross/%s/qb=%d", replicas[0], infoHash, qbinfo.ID)
	}

	skip, adds := qbfake.addState()
	if adds != 1 || skip != "true" {
		t.Fatalf("file adds = %d, skip_checking = %q, want 1 add with skip_checking=true", adds, skip)
	}
}

func TestRelayCrossSeedTimeoutFallsBack(t *testing.T) {
	repo := newRepo(t)
	raw, infoHash := buildTorrent(t, "Test.Movie.2026.mkv")
	fs := newFakeSource(t, raw, infoHash)

	src := insertSource(t, repo, fs.srv.URL)
	seed := insertSeed(t, repo, src.ID, infoHash, "Test.Movie.2026.2160p.WEB-DL.HEVC.DDP")

	dupTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`already exists`))
	}))
	defer dupTarget.Close()
	tgt := insertTarget(t, repo, &store.Target{
		Name: "t1", Type: "nexusphp", Version: "api", BaseURL: dupTarget.URL,
		AnnounceURL: "http://t1/announce.php?passkey={passkey}", APIToken: "tok",
		CategoryOverrides: `{"movie":401}`, Status: "active",
	})

	// Never completes (progress stays 0) → verification times out → fallback
	// to a normal (re-checking) add.
	qbfake := newFakeQB(t)
	qbfake.setInfo(`[{"hash":"` + infoHash + `","name":"t","state":"downloading","progress":0}]`)

	qbinfo := &store.QBInstance{Name: "qb1", Host: qbfake.srv.URL, Port: 0, Username: "admin", Password: "adminpass", Enabled: 1}
	if err := repo.UpsertQBInstance(context.Background(), qbinfo); err != nil {
		t.Fatal(err)
	}
	mgr := qb.NewManager()
	mgr.Set("qb1", qb.NewInstance(qbfake.srv.URL, "", "admin", "adminpass"))

	p := newTestPipeline(repo, mgr, nil, permissiveSourceFactory(), nil)
	if err := p.Relay(context.Background(), seed.ID); err != nil {
		t.Fatalf("Relay: %v", err)
	}

	rec, _ := repo.GetRecord(context.Background(), seed.ID, tgt.ID)
	if rec.Status != statusCrossSeeded {
		t.Fatalf("record status = %q, want cross_seeding", rec.Status)
	}

	// Two file adds: skip_checking first, then the normal fallback.
	skip, adds := qbfake.addState()
	if adds != 2 {
		t.Fatalf("file adds = %d, want 2 (skip_checking + fallback)", adds)
	}
	_ = skip
}

func TestRelayDetailFailure(t *testing.T) {
	repo := newRepo(t)
	raw, infoHash := buildTorrent(t, "Test.Movie.2026.mkv")
	fs := newFakeSource(t, raw, infoHash)
	fs.detailFail = true

	src := insertSource(t, repo, fs.srv.URL)
	seed := insertSeed(t, repo, src.ID, infoHash, "Test.Movie.2026.2160p.WEB-DL.HEVC.DDP")

	// One target (never reached).
	tgtSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"id":1}}`))
	}))
	defer tgtSrv.Close()
	insertTarget(t, repo, &store.Target{Name: "t1", Type: "nexusphp", Version: "api", BaseURL: tgtSrv.URL, CategoryOverrides: `{"movie":401}`, Status: "active"})

	p := newTestPipeline(repo, qb.NewManager(), nil, permissiveSourceFactory(), nil)
	if err := p.Relay(context.Background(), seed.ID); err == nil {
		t.Fatal("Relay: expected detail-fetch error, got nil")
	}
}

func TestRelayHashMismatch(t *testing.T) {
	repo := newRepo(t)
	rawA, hashA := buildTorrent(t, "MovieA.mkv")
	rawB, _ := buildTorrent(t, "MovieB.mkv")

	// RSS announces hashA, but /download serves torrent B.
	fs := newFakeSource(t, rawB, hashA)

	src := insertSource(t, repo, fs.srv.URL)
	seed := insertSeed(t, repo, src.ID, hashA, "Test.Movie.2026.2160p.WEB-DL.HEVC.DDP")
	_ = rawA

	tgtSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"id":1}}`))
	}))
	defer tgtSrv.Close()
	insertTarget(t, repo, &store.Target{Name: "t1", Type: "nexusphp", Version: "api", BaseURL: tgtSrv.URL, CategoryOverrides: `{"movie":401}`, Status: "active"})

	p := newTestPipeline(repo, qb.NewManager(), nil, permissiveSourceFactory(), nil)
	err := p.Relay(context.Background(), seed.ID)
	if err == nil || !strings.Contains(err.Error(), "info_hash mismatch") {
		t.Fatalf("Relay: expected info_hash mismatch error, got %v", err)
	}
}

func TestRelayOneTargetFailsOtherSucceeds(t *testing.T) {
	repo := newRepo(t)
	raw, infoHash := buildTorrent(t, "Test.Movie.2026.mkv")
	fs := newFakeSource(t, raw, infoHash)

	src := insertSource(t, repo, fs.srv.URL)
	seed := insertSeed(t, repo, src.ID, infoHash, "Test.Movie.2026.2160p.WEB-DL.HEVC.DDP")

	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"id":111}}`))
	}))
	defer okSrv.Close()

	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
	}))
	defer failSrv.Close()

	okTgt := insertTarget(t, repo, &store.Target{Name: "ok", Type: "nexusphp", Version: "api", BaseURL: okSrv.URL, CategoryOverrides: `{"movie":401}`, Status: "active"})
	failTgt := insertTarget(t, repo, &store.Target{Name: "fail", Type: "mteam", Version: "api", BaseURL: failSrv.URL, CategoryOverrides: `{"movie":5}`, Status: "active"})

	p := newTestPipeline(repo, qb.NewManager(), nil, permissiveSourceFactory(), nil)
	if err := p.Relay(context.Background(), seed.ID); err != nil {
		t.Fatalf("Relay: %v", err)
	}

	got, _ := repo.GetSeedByID(context.Background(), seed.ID)
	if got.Status != statusRelayed {
		t.Fatalf("seed status = %q, want seeding", got.Status)
	}

	okRec, _ := repo.GetRecord(context.Background(), seed.ID, okTgt.ID)
	if okRec.Status != statusPublished {
		t.Fatalf("ok record = %+v, want published", okRec)
	}
	failRec, _ := repo.GetRecord(context.Background(), seed.ID, failTgt.ID)
	if failRec.Status != statusFailed {
		t.Fatalf("fail record = %+v, want failed", failRec)
	}
}

func TestRelayAuthExpired(t *testing.T) {
	repo := newRepo(t)
	raw, infoHash := buildTorrent(t, "Test.Movie.2026.mkv")
	fs := newFakeSource(t, raw, infoHash)

	src := insertSource(t, repo, fs.srv.URL)
	seed := insertSeed(t, repo, src.ID, infoHash, "Test.Movie.2026.2160p.WEB-DL.HEVC.DDP")

	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`unauthorized`))
	}))
	defer authSrv.Close()
	tgt := insertTarget(t, repo, &store.Target{Name: "t1", Type: "nexusphp", Version: "api", BaseURL: authSrv.URL, APIToken: "tok", CategoryOverrides: `{"movie":401}`, Status: "active"})

	rec := &recordingNotifier{}
	router := notifier.NewRouter()
	router.Add("test", rec, notifier.LevelInfo, notifier.LevelWarning, notifier.LevelCritical)

	p := newTestPipeline(repo, qb.NewManager(), router, permissiveSourceFactory(), nil)
	if err := p.Relay(context.Background(), seed.ID); err == nil {
		t.Fatal("Relay: expected auth-expired error, got nil")
	}

	recState, _ := repo.GetRecord(context.Background(), seed.ID, tgt.ID)
	if recState.Status != statusFailed {
		t.Fatalf("record status = %q, want failed", recState.Status)
	}

	router.Flush(context.Background())
	var warned bool
	for _, m := range rec.messages() {
		if m.Level == notifier.LevelWarning {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("expected a warning notification, got %+v", rec.messages())
	}
}

func TestSiteConfigFromTargetTagsMap(t *testing.T) {
	// A valid tags_map JSON parses into SiteConfig.TagsMap.
	cfg, err := siteConfigFromTarget(&store.Target{
		Name: "t", Type: "nexusphp", Version: "api",
		TagsMap: `{"国语":"1","中字":"2"}`,
	})
	if err != nil {
		t.Fatalf("siteConfigFromTarget: %v", err)
	}
	if cfg.TagsMap["国语"] != "1" || cfg.TagsMap["中字"] != "2" {
		t.Fatalf("TagsMap = %+v, want 国语=1 中字=2", cfg.TagsMap)
	}

	// A malformed tags_map must degrade to an empty map (no error).
	cfg, err = siteConfigFromTarget(&store.Target{
		Name: "t", Type: "nexusphp", Version: "api",
		TagsMap: `{invalid`,
	})
	if err != nil {
		t.Fatalf("siteConfigFromTarget (invalid tags_map) must not error: %v", err)
	}
	if len(cfg.TagsMap) != 0 {
		t.Fatalf("TagsMap = %+v, want empty on parse failure", cfg.TagsMap)
	}
}
