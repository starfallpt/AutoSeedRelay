package qb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// fake qB WebUI server
// ---------------------------------------------------------------------------

type fakeQB struct {
	srv *httptest.Server

	mu sync.Mutex

	// login behavior
	password     string // correct password; anything else is rejected
	loginStatus  int    // success status: 200 or 204
	loginBody    string // success body (used when status is 200)
	loginFail200 bool   // reject with 200 "Fails." instead of 401

	// add behavior: "Ok." / "Fails." / JSON / "" (empty 200 body)
	addBody string

	// stop/start fallback
	stopReturns404  bool
	startReturns404 bool

	// protected-endpoint auth
	requireAuth       bool
	failNextProtected int
	sid               string

	version    string
	exportBody string
	infoBody   string

	// counters
	loginCalls   int
	versionCalls int
	infoCalls    int
	addCalls     int
	stopCalls    int
	pauseCalls   int
	startCalls   int
	resumeCalls  int
	exportCalls  int

	lastAddHadTorrents    bool
	lastAddHadTorrentsSet bool
}

const torrentInfoJSON = `[{"hash":"abc123","name":"test.torrent","state":"uploading","progress":1,"completed":100,"completion_on":12345,"dlspeed":0,"upspeed":0,"size":100,"downloaded":100,"uploaded":200,"ratio":2.0,"added_on":1000,"category":"cat","save_path":"/downloads","num_complete":5,"num_leechs":0}]`

func newFake(t *testing.T, configure func(*fakeQB)) *fakeQB {
	t.Helper()
	f := &fakeQB{
		password:    "adminpass",
		loginStatus: http.StatusOK,
		loginBody:   "Ok.",
		addBody:     "Ok.",
		version:     "v4.6.0",
		exportBody:  "d4:infod4:name8:test.txtee",
		infoBody:    torrentInfoJSON,
	}
	if configure != nil {
		configure(f)
	}
	f.srv = httptest.NewServer(f.handler())
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeQB) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			f.handleLogin(w, r)
		case "/api/v2/app/version":
			f.handleVersion(w, r)
		case "/api/v2/torrents/add":
			f.handleAdd(w, r)
		case "/api/v2/torrents/stop":
			f.handleStop(w, r)
		case "/api/v2/torrents/pause":
			f.handlePause(w, r)
		case "/api/v2/torrents/start":
			f.handleStart(w, r)
		case "/api/v2/torrents/resume":
			f.handleResume(w, r)
		case "/api/v2/torrents/info":
			f.handleInfo(w, r)
		case "/api/v2/torrents/export":
			f.handleExport(w, r)
		case "/api/v2/sync/maindata":
			f.handleMaindata(w, r)
		case "/api/v2/transfer/info":
			f.handleTransfer(w, r)
		default:
			http.NotFound(w, r)
		}
	}
}

func (f *fakeQB) handleLogin(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.loginCalls++
	f.mu.Unlock()

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	password := f.password
	status := f.loginStatus
	body := f.loginBody
	fail200 := f.loginFail200
	f.mu.Unlock()

	if r.PostFormValue("password") != password {
		if fail200 {
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, "Fails.")
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "Fails.")
		return
	}

	f.mu.Lock()
	f.sid = fmt.Sprintf("sid-%d", f.loginCalls)
	sid := f.sid
	f.mu.Unlock()

	http.SetCookie(w, &http.Cookie{Name: "SID", Value: sid, Path: "/"})
	w.WriteHeader(status)
	if status == http.StatusOK {
		io.WriteString(w, body)
	}
}

func (f *fakeQB) authorize(w http.ResponseWriter, r *http.Request) bool {
	f.mu.Lock()
	require := f.requireAuth
	failNext := false
	if f.failNextProtected > 0 {
		f.failNextProtected--
		failNext = true
	}
	sid := f.sid
	f.mu.Unlock()

	if !require {
		return true
	}
	if failNext {
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	cookie, err := r.Cookie("SID")
	if err != nil || cookie.Value == "" || cookie.Value != sid {
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	return true
}

func (f *fakeQB) handleVersion(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	f.versionCalls++
	version := f.version
	f.mu.Unlock()
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, version)
}

func (f *fakeQB) handleAdd(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.addCalls++
	f.mu.Unlock()

	if !f.authorize(w, r) {
		return
	}

	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		_, _, err := r.FormFile("torrents")
		f.mu.Lock()
		f.lastAddHadTorrents = err == nil
		f.lastAddHadTorrentsSet = true
		f.mu.Unlock()
	}

	f.mu.Lock()
	addBody := f.addBody
	f.mu.Unlock()

	w.WriteHeader(http.StatusOK)
	if addBody != "" {
		io.WriteString(w, addBody)
	}
}

func (f *fakeQB) handleStop(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.stopCalls++
	stop404 := f.stopReturns404
	f.mu.Unlock()
	if !f.authorize(w, r) {
		return
	}
	if stop404 {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (f *fakeQB) handlePause(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.pauseCalls++
	f.mu.Unlock()
	if !f.authorize(w, r) {
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (f *fakeQB) handleStart(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.startCalls++
	start404 := f.startReturns404
	f.mu.Unlock()
	if !f.authorize(w, r) {
		return
	}
	if start404 {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (f *fakeQB) handleResume(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.resumeCalls++
	f.mu.Unlock()
	if !f.authorize(w, r) {
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (f *fakeQB) handleInfo(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.infoCalls++
	infoBody := f.infoBody
	f.mu.Unlock()
	if !f.authorize(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, infoBody)
}

func (f *fakeQB) handleExport(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.exportCalls++
	exportBody := f.exportBody
	f.mu.Unlock()
	if !f.authorize(w, r) {
		return
	}
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, exportBody)
}

func (f *fakeQB) handleMaindata(w http.ResponseWriter, r *http.Request) {
	if !f.authorize(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"server_state":{"free_space_on_disk":12345}}`)
}

func (f *fakeQB) handleTransfer(w http.ResponseWriter, r *http.Request) {
	if !f.authorize(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"dl_info_speed":1,"up_info_speed":2,"dl_info_data":3,"up_info_data":4}`)
}

// client builds an Instance pointed at the fake server with the given
// password.
func (f *fakeQB) client(t *testing.T, password string) *Instance {
	t.Helper()
	u, err := url.Parse(f.srv.URL)
	if err != nil {
		t.Fatalf("parse srv.URL: %v", err)
	}
	return NewInstance(u.Scheme+"://"+u.Hostname(), u.Port(), "admin", password, WithHTTPClient(f.srv.Client()))
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestLogin204(t *testing.T) {
	f := newFake(t, func(f *fakeQB) {
		f.loginStatus = http.StatusNoContent
		f.loginBody = ""
	})
	inst := f.client(t, "adminpass")
	if err := inst.Login(context.Background()); err != nil {
		t.Fatalf("Login() error = %v, want nil", err)
	}
	if inst.SID() == "" {
		t.Fatal("SID() empty after 204 login, want non-empty")
	}
}

func TestLogin200OK(t *testing.T) {
	f := newFake(t, nil)
	inst := f.client(t, "adminpass")
	if err := inst.Login(context.Background()); err != nil {
		t.Fatalf("Login() error = %v, want nil", err)
	}
	if inst.SID() == "" {
		t.Fatal("SID() empty after 200 Ok. login")
	}
}

func TestLoginWrongPassword401(t *testing.T) {
	f := newFake(t, nil)
	inst := f.client(t, "wrongpass")
	err := inst.Login(context.Background())
	if err == nil {
		t.Fatal("Login() = nil, want auth error")
	}
	var authErr *QBAuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("Login() error type = %T (%v), want *QBAuthError", err, err)
	}
}

func TestLoginFails200(t *testing.T) {
	f := newFake(t, func(f *fakeQB) { f.loginFail200 = true })
	inst := f.client(t, "wrongpass")
	err := inst.Login(context.Background())
	var authErr *QBAuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("Login() error type = %T, want *QBAuthError", err)
	}
}

func TestAddReturnsOK(t *testing.T) {
	f := newFake(t, nil)
	inst := f.client(t, "adminpass")
	res, err := inst.AddTorrentFile(context.Background(), "test.torrent", []byte("dummy-torrent-bytes"), AddOptions{})
	if err != nil {
		t.Fatalf("AddTorrentFile() error = %v, want nil", err)
	}
	if res["status"] != "Ok." {
		t.Fatalf("AddTorrentFile() = %v, want status Ok.", res)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.lastAddHadTorrents {
		t.Fatal(`multipart field "torrents" not present in upload`)
	}
}

func TestAddFails(t *testing.T) {
	f := newFake(t, func(f *fakeQB) { f.addBody = "Fails." })
	inst := f.client(t, "adminpass")
	_, err := inst.AddTorrentURL(context.Background(), "magnet:?xt=urn:btih:abc", AddOptions{})
	if err == nil {
		t.Fatal("AddTorrentURL() = nil, want error for Fails.")
	}
}

func TestAddJSON(t *testing.T) {
	f := newFake(t, func(f *fakeQB) { f.addBody = `{"added":1}` })
	inst := f.client(t, "adminpass")
	res, err := inst.AddMagnet(context.Background(), "magnet:?xt=urn:btih:abc", AddOptions{})
	if err != nil {
		t.Fatalf("AddMagnet() error = %v, want nil", err)
	}
	if res["added"] != float64(1) {
		t.Fatalf("AddMagnet() = %v, want added=1", res)
	}
}

func TestStop404FallbackPause(t *testing.T) {
	f := newFake(t, func(f *fakeQB) {
		f.stopReturns404 = true
		f.requireAuth = true
	})
	inst := f.client(t, "adminpass")
	if err := inst.Stop(context.Background(), "abc123"); err != nil {
		t.Fatalf("Stop() error = %v, want nil", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopCalls != 1 {
		t.Fatalf("stopCalls = %d, want 1", f.stopCalls)
	}
	if f.pauseCalls != 1 {
		t.Fatalf("pauseCalls = %d, want 1 (fallback)", f.pauseCalls)
	}
}

func TestReloginOn401(t *testing.T) {
	f := newFake(t, func(f *fakeQB) {
		f.requireAuth = true
		f.failNextProtected = 1
	})
	inst := f.client(t, "adminpass")
	info, err := inst.GetTorrent(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("GetTorrent() error = %v, want nil", err)
	}
	if info == nil || info.Hash != "abc123" {
		t.Fatalf("GetTorrent() = %+v, want hash abc123", info)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loginCalls != 2 {
		t.Fatalf("loginCalls = %d, want 2 (initial + relogin)", f.loginCalls)
	}
	if f.infoCalls != 2 {
		t.Fatalf("infoCalls = %d, want 2 (401 + retry)", f.infoCalls)
	}
}

func TestInfoTyped(t *testing.T) {
	f := newFake(t, func(f *fakeQB) { f.requireAuth = true })
	inst := f.client(t, "adminpass")
	list, err := inst.Info(context.Background(), "")
	if err != nil {
		t.Fatalf("Info() error = %v, want nil", err)
	}
	if len(list) != 1 {
		t.Fatalf("Info() len = %d, want 1", len(list))
	}
	if got := list[0]; got.Hash != "abc123" || got.State != "uploading" || got.Progress != 1 {
		t.Fatalf("Info()[0] = %+v", got)
	}
}

func TestExportTorrent(t *testing.T) {
	f := newFake(t, func(f *fakeQB) { f.requireAuth = true })
	inst := f.client(t, "adminpass")
	data, err := inst.ExportTorrent(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("ExportTorrent() error = %v, want nil", err)
	}
	if len(data) == 0 {
		t.Fatal("ExportTorrent() returned empty data")
	}
}

func TestExportTorrentInvalid(t *testing.T) {
	f := newFake(t, func(f *fakeQB) {
		f.requireAuth = true
		f.exportBody = "not a bencoded torrent"
	})
	inst := f.client(t, "adminpass")
	if _, err := inst.ExportTorrent(context.Background(), "abc123"); err == nil {
		t.Fatal("ExportTorrent() = nil error for invalid torrent body")
	}
}

func TestGetDiskSpace(t *testing.T) {
	f := newFake(t, func(f *fakeQB) { f.requireAuth = true })
	inst := f.client(t, "adminpass")
	d, err := inst.GetDiskSpace(context.Background())
	if err != nil {
		t.Fatalf("GetDiskSpace() error = %v, want nil", err)
	}
	if d.FreeOnDisk != 12345 {
		t.Fatalf("FreeOnDisk = %d, want 12345", d.FreeOnDisk)
	}
}

func TestGetTransferInfo(t *testing.T) {
	f := newFake(t, func(f *fakeQB) { f.requireAuth = true })
	inst := f.client(t, "adminpass")
	ti, err := inst.GetTransferInfo(context.Background())
	if err != nil {
		t.Fatalf("GetTransferInfo() error = %v, want nil", err)
	}
	if ti.DLSpeed != 1 || ti.UPSpeed != 2 || ti.DLTotal != 3 || ti.UPTotal != 4 {
		t.Fatalf("GetTransferInfo() = %+v", ti)
	}
}

func TestVersionCache(t *testing.T) {
	f := newFake(t, nil)
	inst := f.client(t, "adminpass")
	v1, err := inst.Version(context.Background())
	if err != nil {
		t.Fatalf("Version() error = %v, want nil", err)
	}
	if v1 != "v4.6.0" {
		t.Fatalf("Version() = %q, want v4.6.0", v1)
	}
	v2, err := inst.Version(context.Background())
	if err != nil || v2 != v1 {
		t.Fatalf("cached Version() = %q, %v; want %q, nil", v2, err, v1)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.versionCalls != 1 {
		t.Fatalf("versionCalls = %d, want 1 (cached)", f.versionCalls)
	}
}

func TestIsCompletedSeeding(t *testing.T) {
	cases := []struct {
		name string
		ti   *TorrentInfo
		want bool
	}{
		{"uploading", &TorrentInfo{Progress: 1, Completed: 100, CompletionOn: 12345, State: "uploading"}, true},
		{"stalledUP", &TorrentInfo{Progress: 1, Completed: 100, CompletionOn: 12345, State: "stalledUP"}, true},
		{"stoppedUP", &TorrentInfo{Progress: 1, Completed: 100, CompletionOn: 12345, State: "stoppedUP"}, true},
		{"incomplete", &TorrentInfo{Progress: 0.5, Completed: 50, CompletionOn: -1, State: "downloading"}, false},
		{"downloading full", &TorrentInfo{Progress: 1, Completed: 100, CompletionOn: 12345, State: "downloading"}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := IsCompletedSeeding(tc.ti); got != tc.want {
			t.Errorf("%s: IsCompletedSeeding() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsSlow(t *testing.T) {
	f := newFake(t, func(f *fakeQB) {
		f.requireAuth = true
		f.infoBody = `[{"hash":"slow1","name":"slow","state":"downloading","dlspeed":0,"progress":0.1,"completed":0,"completion_on":-1}]`
	})
	inst := f.client(t, "adminpass")
	ctx := context.Background()
	// First call: below threshold and downloading, starts the slow timer.
	if inst.IsSlow(ctx, "slow1", 10, time.Hour) {
		t.Fatal("IsSlow() = true on first call, want false")
	}
	// Let real time pass so the sliding timer can elapse past the duration.
	time.Sleep(20 * time.Millisecond)
	if !inst.IsSlow(ctx, "slow1", 10, 10*time.Millisecond) {
		t.Fatal("IsSlow() = false after delay, want true")
	}
}

func TestManagerCRUD(t *testing.T) {
	m := NewManager()
	if _, ok := m.Get("a"); ok {
		t.Fatal("Get() on empty manager = true, want false")
	}
	m.Set("a", &Instance{})
	m.Set("b", &Instance{})
	if _, ok := m.Get("a"); !ok {
		t.Fatal("Get(a) = false, want true")
	}
	if got := m.Names(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("Names() = %v, want [a b]", got)
	}
	m.Remove("a")
	if _, ok := m.Get("a"); ok {
		t.Fatal("Get(a) after Remove = true, want false")
	}
	if got := m.Names(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("Names() after Remove = %v, want [b]", got)
	}
	// Set with nil removes the entry.
	m.Set("b", nil)
	if got := m.Names(); len(got) != 0 {
		t.Fatalf("Names() after Set(b, nil) = %v, want empty", got)
	}
}

func TestManagerAllHealthy(t *testing.T) {
	good := newFake(t, nil)
	a := good.client(t, "adminpass")
	b := good.client(t, "adminpass")

	scheme, host, port := closedPortURL(t)
	c := NewInstance(scheme+"://"+host, port, "admin", "x")

	m := NewManager()
	m.Set("alpha", a)
	m.Set("beta", b)
	m.Set("offline", c)

	statuses, err := m.AllHealthy(context.Background())
	if err != nil {
		t.Fatalf("AllHealthy() error = %v, want nil", err)
	}
	if len(statuses) != 3 {
		t.Fatalf("AllHealthy() = %d statuses, want 3", len(statuses))
	}

	byName := make(map[string]Status, len(statuses))
	for _, s := range statuses {
		byName[s.Name] = s
	}

	if s := byName["alpha"]; !s.Online || s.Version != "v4.6.0" {
		t.Errorf("alpha = %+v, want online + version v4.6.0", s)
	}
	if s := byName["beta"]; !s.Online || s.Version != "v4.6.0" {
		t.Errorf("beta = %+v, want online + version v4.6.0", s)
	}
	if s := byName["offline"]; s.Online {
		t.Errorf("offline = %+v, want offline", s)
	} else if s.LastError == "" {
		t.Errorf("offline = %+v, want non-empty LastError", s)
	}
}

// closedPortURL returns the scheme/host/port of a just-closed TCP listener,
// i.e. an address that is very likely to refuse connections.
func closedPortURL(t *testing.T) (scheme, host, port string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	host, port, err = net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	return "http", host, port
}
