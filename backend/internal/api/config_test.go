package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/autoseedrelay/relay/internal/notifier"
	"github.com/autoseedrelay/relay/internal/store"
	"github.com/gin-gonic/gin"
)

// --- test doubles and setup ---

func configTestKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

type noopConfigAuth struct{}

func (noopConfigAuth) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) { c.Next() }
}

func newConfigAPI(t *testing.T) (*gin.Engine, *store.Repo, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	repo := store.NewRepo(st.DB(), configTestKey())

	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterConfig(r.Group("/api/v2"), Deps{Repo: repo, Store: st, Auth: noopConfigAuth{}})
	return r, repo, st
}

func configDoJSON(t *testing.T, r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func configDecode(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, w.Body.String())
	}
}

func configEncPassword(t *testing.T, repo *store.Repo, id int64) string {
	t.Helper()
	var enc sql.NullString
	if err := repo.DB().QueryRow(`SELECT enc_password FROM qb_instances WHERE id = ?`, id).Scan(&enc); err != nil {
		t.Fatalf("query enc_password: %v", err)
	}
	if !enc.Valid {
		return ""
	}
	return enc.String
}

// --- tests ---

func TestConfigSourceCRUD(t *testing.T) {
	r, repo, _ := newConfigAPI(t)

	w := configDoJSON(t, r, http.MethodPost, "/api/v2/sources", gin.H{
		"name": "src1", "base_url": "https://a.example", "rss_url": "https://a.example/rss",
		"passkey": "secret123", "api_token": "tok", "cookie": "ck",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", w.Code, w.Body.String())
	}
	var created map[string]any
	configDecode(t, w, &created)
	id := int64(created["id"].(float64))
	if created["passkey"] != "***" || created["api_token"] != "***" || created["cookie"] != "***" {
		t.Fatalf("create response not masked: %v", created)
	}

	s, err := repo.GetSourceByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetSourceByID: %v", err)
	}
	if s.Passkey != "secret123" {
		t.Fatalf("stored plaintext passkey = %q", s.Passkey)
	}

	w = configDoJSON(t, r, http.MethodGet, "/api/v2/sources", nil)
	var list []map[string]any
	configDecode(t, w, &list)
	if len(list) != 1 || list[0]["passkey"] != "***" {
		t.Fatalf("list: %v", list)
	}

	w = configDoJSON(t, r, http.MethodGet, fmt.Sprintf("/api/v2/sources/%d", id), nil)
	var got map[string]any
	configDecode(t, w, &got)
	if got["passkey"] != "***" {
		t.Fatalf("get passkey = %v", got["passkey"])
	}

	// "***" keeps the credential; name still updates.
	w = configDoJSON(t, r, http.MethodPut, fmt.Sprintf("/api/v2/sources/%d", id), gin.H{
		"name": "src1-renamed", "base_url": "https://a.example", "rss_url": "https://a.example/rss",
		"passkey": "***", "api_token": "***", "cookie": "***",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("update status = %d body=%s", w.Code, w.Body.String())
	}
	s, _ = repo.GetSourceByID(context.Background(), id)
	if s.Passkey != "secret123" {
		t.Fatalf("*** did not preserve passkey: %q", s.Passkey)
	}
	if s.Name != "src1-renamed" {
		t.Fatalf("name = %q", s.Name)
	}

	// empty clears.
	configDoJSON(t, r, http.MethodPut, fmt.Sprintf("/api/v2/sources/%d", id), gin.H{
		"name": "src1", "base_url": "https://a.example", "rss_url": "https://a.example/rss",
		"passkey": "",
	})
	s, _ = repo.GetSourceByID(context.Background(), id)
	if s.Passkey != "" {
		t.Fatalf("empty did not clear passkey: %q", s.Passkey)
	}

	w = configDoJSON(t, r, http.MethodDelete, fmt.Sprintf("/api/v2/sources/%d", id), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status = %d", w.Code)
	}
	w = configDoJSON(t, r, http.MethodGet, fmt.Sprintf("/api/v2/sources/%d", id), nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("get after delete = %d", w.Code)
	}

	w = configDoJSON(t, r, http.MethodPost, "/api/v2/sources", gin.H{"base_url": "x"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing name status = %d", w.Code)
	}
}

func TestConfigTargetProbeAndTest(t *testing.T) {
	r, repo, _ := newConfigAPI(t)

	siteSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/api/v1/sections" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"categories":[{"id":401,"name":"Movies"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer siteSrv.Close()

	w := configDoJSON(t, r, http.MethodPost, "/api/v2/targets", gin.H{
		"name": "t1", "type": "nexusphp", "base_url": siteSrv.URL,
		"passkey": "pk", "category_overrides": gin.H{"movie": 401},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}
	var created map[string]any
	configDecode(t, w, &created)
	id := int64(created["id"].(float64))
	if created["passkey"] != "***" {
		t.Fatalf("target passkey not masked: %v", created["passkey"])
	}

	w = configDoJSON(t, r, http.MethodPost, fmt.Sprintf("/api/v2/targets/%d/probe", id), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("probe status=%d body=%s", w.Code, w.Body.String())
	}
	var probe map[string]any
	configDecode(t, w, &probe)
	if probe["ok"] != true || probe["type"] != "nexusphp" {
		t.Fatalf("probe = %v", probe)
	}

	w = configDoJSON(t, r, http.MethodPost, fmt.Sprintf("/api/v2/targets/%d/test", id), nil)
	var tst map[string]any
	configDecode(t, w, &tst)
	if tst["ok"] != true {
		t.Fatalf("target test = %v", tst)
	}

	tg, err := repo.GetTargetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTargetByID: %v", err)
	}
	if tg.Passkey != "pk" {
		t.Fatalf("target passkey = %q", tg.Passkey)
	}
	if tg.CategoryOverrides != `{"movie":401}` {
		t.Fatalf("category_overrides = %q", tg.CategoryOverrides)
	}
}

func TestConfigQBCRUDAndTest(t *testing.T) {
	r, repo, _ := newConfigAPI(t)

	qbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/api/v2/auth/login":
			w.WriteHeader(http.StatusNoContent)
		case "/api/v2/app/version":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("v5.0.0"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer qbSrv.Close()

	u, _ := url.Parse(qbSrv.URL)
	host := u.Scheme + "://" + u.Hostname()
	port, _ := strconv.Atoi(u.Port())

	w := configDoJSON(t, r, http.MethodPost, "/api/v2/qb", gin.H{
		"name": "q1", "host": host, "port": port, "username": "admin", "password": "pw123",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}
	var created map[string]any
	configDecode(t, w, &created)
	id := int64(created["id"].(float64))
	if created["password"] != "***" {
		t.Fatalf("qb password not masked: %v", created["password"])
	}

	enc1 := configEncPassword(t, repo, id)
	if enc1 == "" {
		t.Fatalf("enc_password empty after create")
	}

	w = configDoJSON(t, r, http.MethodPost, fmt.Sprintf("/api/v2/qb/%d/test", id), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("test status=%d body=%s", w.Code, w.Body.String())
	}
	var tst map[string]any
	configDecode(t, w, &tst)
	if tst["ok"] != true || tst["version"] != "v5.0.0" {
		t.Fatalf("qb test = %v", tst)
	}

	// "***" keeps the password (ciphertext unchanged).
	w = configDoJSON(t, r, http.MethodPut, fmt.Sprintf("/api/v2/qb/%d", id), gin.H{
		"name": "q1", "host": host, "port": port, "username": "admin", "password": "***",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", w.Code, w.Body.String())
	}
	if enc2 := configEncPassword(t, repo, id); enc2 != enc1 {
		t.Fatalf("*** changed enc_password: %q -> %q", enc1, enc2)
	}

	// new password changes the ciphertext.
	configDoJSON(t, r, http.MethodPut, fmt.Sprintf("/api/v2/qb/%d", id), gin.H{
		"name": "q1", "host": host, "port": port, "username": "admin", "password": "newpw",
	})
	enc3 := configEncPassword(t, repo, id)
	if enc3 == "" || enc3 == enc1 {
		t.Fatalf("new password did not change enc_password")
	}

	w = configDoJSON(t, r, http.MethodDelete, fmt.Sprintf("/api/v2/qb/%d", id), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status=%d", w.Code)
	}
}

func TestConfigStrategyRoundtrip(t *testing.T) {
	r, _, _ := newConfigAPI(t)

	w := configDoJSON(t, r, http.MethodGet, "/api/v2/strategy", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get status=%d", w.Code)
	}
	var got map[string]any
	configDecode(t, w, &got)
	if got["id"] != float64(1) {
		t.Fatalf("strategy id = %v", got["id"])
	}

	w = configDoJSON(t, r, http.MethodPut, "/api/v2/strategy", gin.H{
		"min_size": 100, "max_size": 2000, "retire_mode": "or",
		"dispatch_mode": "round_robin", "timezone": "Asia/Shanghai",
		"retire_ratio_enabled": true, "retire_ratio": 1.5, "retry_max": 5,
		"promotions": []string{"free"}, "keywords": []string{"x264"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("put status=%d body=%s", w.Code, w.Body.String())
	}

	w = configDoJSON(t, r, http.MethodGet, "/api/v2/strategy", nil)
	configDecode(t, w, &got)
	if got["min_size"] != float64(100) || got["retire_mode"] != "or" || got["dispatch_mode"] != "round_robin" {
		t.Fatalf("strategy after put = %v", got)
	}
	if got["retire_ratio_enabled"] != true {
		t.Fatalf("retire_ratio_enabled = %v", got["retire_ratio_enabled"])
	}
}

func TestConfigNotifierCRUDRoutesAndTest(t *testing.T) {
	r, _, _ := newConfigAPI(t)

	var hits int32
	hookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer hookSrv.Close()

	w := configDoJSON(t, r, http.MethodPost, "/api/v2/notifiers", gin.H{
		"name": "n1", "type": "webhook", "config": notifier.Config{WebhookURL: hookSrv.URL}, "enabled": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}
	var created map[string]any
	configDecode(t, w, &created)
	id := int64(created["id"].(float64))
	if cfg, ok := created["config"].(map[string]any); !ok || cfg["webhook_url"] != hookSrv.URL {
		t.Fatalf("config roundtrip mismatch: %v", created["config"])
	}

	w = configDoJSON(t, r, http.MethodPost, fmt.Sprintf("/api/v2/notifiers/%d/test", id), nil)
	var tst map[string]any
	configDecode(t, w, &tst)
	if tst["ok"] != true {
		t.Fatalf("notifier test = %v", tst)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("webhook hits = %d, want 1", hits)
	}

	w = configDoJSON(t, r, http.MethodPut, "/api/v2/notifiers/routes", []gin.H{{"instance_id": id, "tier": "critical"}})
	if w.Code != http.StatusOK {
		t.Fatalf("routes put status=%d body=%s", w.Code, w.Body.String())
	}

	w = configDoJSON(t, r, http.MethodGet, "/api/v2/notifiers/routes", nil)
	var routes []map[string]any
	configDecode(t, w, &routes)
	if len(routes) != 1 || int64(routes[0]["instance_id"].(float64)) != id || routes[0]["tier"] != "critical" {
		t.Fatalf("routes = %v", routes)
	}

	w = configDoJSON(t, r, http.MethodDelete, fmt.Sprintf("/api/v2/notifiers/%d", id), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status=%d", w.Code)
	}
	w = configDoJSON(t, r, http.MethodGet, "/api/v2/notifiers", nil)
	var list []map[string]any
	configDecode(t, w, &list)
	if len(list) != 0 {
		t.Fatalf("notifiers after delete = %v", list)
	}
}

func TestConfigNotifierTelegramAndSMTP(t *testing.T) {
	r, _, _ := newConfigAPI(t)

	tgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/bottok/getMe" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer tgSrv.Close()

	w := configDoJSON(t, r, http.MethodPost, "/api/v2/notifiers", gin.H{
		"name": "tg", "type": "telegram",
		"config": notifier.Config{TelegramToken: "tok", TelegramChatID: "123", TelegramBaseURL: tgSrv.URL},
	})
	var created map[string]any
	configDecode(t, w, &created)
	tgID := int64(created["id"].(float64))

	w = configDoJSON(t, r, http.MethodPost, fmt.Sprintf("/api/v2/notifiers/%d/test", tgID), nil)
	var tst map[string]any
	configDecode(t, w, &tst)
	if tst["ok"] != true {
		t.Fatalf("telegram test = %v", tst)
	}

	// smtp: config-only validation (no network I/O).
	w = configDoJSON(t, r, http.MethodPost, "/api/v2/notifiers", gin.H{
		"name": "smtp", "type": "smtp",
		"config": notifier.Config{SMTPHost: "smtp.example.com", SMTPFrom: "a@b.c", SMTPTo: []string{"x@y.z"}},
	})
	configDecode(t, w, &created)
	smtpID := int64(created["id"].(float64))

	w = configDoJSON(t, r, http.MethodPost, fmt.Sprintf("/api/v2/notifiers/%d/test", smtpID), nil)
	configDecode(t, w, &tst)
	if tst["ok"] != true {
		t.Fatalf("smtp test = %v", tst)
	}
}
