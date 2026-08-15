package auth

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/autoseedrelay/relay/internal/store"
	"github.com/gin-gonic/gin"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s.DB()
}

func newTestManager(t *testing.T, opts Options) *Manager {
	t.Helper()
	m, err := New(newTestDB(t), nil, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

// validSessionCookie forges a correctly-signed, unexpired session cookie using
// the manager's own secret (test lives in-package).
func validSessionCookie(m *Manager, ttl time.Duration) string {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(m.now().Add(ttl).Unix()))
	return base64.RawURLEncoding.EncodeToString(append(b, m.sign(b)...))
}

func doReq(r http.Handler, method, path string, cookies []*http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestNewPersistsSecretAndSetupState(t *testing.T) {
	db := newTestDB(t)
	m, err := New(db, nil, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m.SetupState() {
		t.Fatal("fresh manager must report uninitialized")
	}

	var v string
	if err := db.QueryRow("SELECT value FROM app_settings WHERE key = ?", keySessionSecret).Scan(&v); err != nil {
		t.Fatalf("session secret not persisted: %v", err)
	}
	if v == "" {
		t.Fatal("session secret persisted empty")
	}

	// Re-opening with the persisted secret as an explicit option must reuse it.
	m2, err := New(db, nil, Options{SessionSecret: m.secret})
	if err != nil {
		t.Fatalf("New with explicit secret: %v", err)
	}
	if string(m2.secret) != string(m.secret) {
		t.Fatal("explicit secret not honored")
	}
}

func TestCompleteSetupLogin(t *testing.T) {
	m := newTestManager(t, Options{})

	if err := m.CompleteSetup("s3cret-pw"); err != nil {
		t.Fatalf("CompleteSetup: %v", err)
	}
	if !m.SetupState() {
		t.Fatal("expected initialized after CompleteSetup")
	}

	ok, err := m.Login(context.Background(), "s3cret-pw")
	if err != nil || !ok {
		t.Fatalf("Login(correct): ok=%v err=%v", ok, err)
	}
	ok, err = m.Login(context.Background(), "wrong")
	if err != nil || ok {
		t.Fatalf("Login(wrong): ok=%v err=%v", ok, err)
	}

	if err := m.CompleteSetup("other"); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("second CompleteSetup err = %v, want ErrAlreadyInitialized", err)
	}
	if err := m.CompleteSetup(""); !errors.Is(err, ErrEmptyPassword) {
		t.Fatalf("empty CompleteSetup err = %v, want ErrEmptyPassword", err)
	}
}

func TestAutoInitFromEnv(t *testing.T) {
	const env = "AUTOSEED_TEST_WEB_PASSWORD"
	t.Setenv(env, "env-pw")

	m := newTestManager(t, Options{WebPasswordEnv: env})
	if !m.SetupState() {
		t.Fatal("expected auto-initialized from env")
	}
	ok, err := m.Login(context.Background(), "env-pw")
	if err != nil || !ok {
		t.Fatalf("Login(env-pw): ok=%v err=%v", ok, err)
	}
}

func TestRateLimitWindow(t *testing.T) {
	var now time.Time
	m := newTestManager(t, Options{Now: func() time.Time { return now }})

	for i := 0; i < loginLimitPerMinute; i++ {
		allowed, _ := m.AllowLogin("1.2.3.4")
		if !allowed {
			t.Fatalf("attempt %d unexpectedly blocked", i+1)
		}
	}
	allowed, retry := m.AllowLogin("1.2.3.4")
	if allowed {
		t.Fatal("6th attempt must be blocked")
	}
	if retry <= 0 {
		t.Fatalf("retryAfter = %v, want > 0", retry)
	}

	// A different IP has its own budget.
	if allowed, _ := m.AllowLogin("5.6.7.8"); !allowed {
		t.Fatal("different IP must not share the first IP's budget")
	}

	// Advance the clock past the window → the original IP is allowed again.
	now = now.Add(loginWindow + time.Second)
	if allowed, _ := m.AllowLogin("1.2.3.4"); !allowed {
		t.Fatal("window must reset after the clock advances")
	}
}

func TestMiddleware(t *testing.T) {
	m := newTestManager(t, Options{})
	r := newAuthEngine(m)

	// Uninitialized: protected routes are 403, exempt routes pass.
	if w := doReq(r, "GET", "/api/v2/protected", nil, nil); w.Code != http.StatusForbidden {
		t.Fatalf("uninitialized protected GET = %d, want 403", w.Code)
	}
	if w := doReq(r, "GET", "/api/v2/health", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("health = %d, want 200", w.Code)
	}
	if w := doReq(r, "GET", "/api/v2/setup/status", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("setup/status = %d, want 200", w.Code)
	}

	if err := m.CompleteSetup("pw"); err != nil {
		t.Fatalf("CompleteSetup: %v", err)
	}

	// Initialized but no session → 401.
	if w := doReq(r, "GET", "/api/v2/protected", nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET = %d, want 401", w.Code)
	}

	sess := validSessionCookie(m, time.Hour)
	sessCookie := []*http.Cookie{{Name: sessionCookieName, Value: sess}}

	// Authenticated GET passes.
	if w := doReq(r, "GET", "/api/v2/protected", sessCookie, nil); w.Code != http.StatusOK {
		t.Fatalf("authenticated GET = %d, want 200", w.Code)
	}

	// Authenticated POST without CSRF → 403.
	if w := doReq(r, "POST", "/api/v2/protected", sessCookie, nil); w.Code != http.StatusForbidden {
		t.Fatalf("POST without CSRF = %d, want 403", w.Code)
	}

	// Authenticated POST with matching CSRF cookie + header → 200.
	csrfCookie := []*http.Cookie{
		{Name: sessionCookieName, Value: sess},
		{Name: csrfCookieName, Value: "tok"},
	}
	if w := doReq(r, "POST", "/api/v2/protected", csrfCookie, map[string]string{csrfHeaderName: "tok"}); w.Code != http.StatusOK {
		t.Fatalf("POST with CSRF = %d, want 200", w.Code)
	}
	// Mismatched CSRF header → 403.
	if w := doReq(r, "POST", "/api/v2/protected", csrfCookie, map[string]string{csrfHeaderName: "other"}); w.Code != http.StatusForbidden {
		t.Fatalf("POST with wrong CSRF = %d, want 403", w.Code)
	}

	// Tampered session cookie → 401.
	tampered := sess[:len(sess)/2] + string(flipBase64(sess[len(sess)/2])) + sess[len(sess)/2+1:]
	if w := doReq(r, "GET", "/api/v2/protected", []*http.Cookie{{Name: sessionCookieName, Value: tampered}}, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("tampered cookie GET = %d, want 401", w.Code)
	}
}

func newAuthEngine(m *Manager) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(m.Middleware())
	r.GET("/api/v2/health", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/api/v2/auth/login", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/api/v2/setup/status", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/api/v2/protected", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.POST("/api/v2/protected", func(c *gin.Context) { c.Status(http.StatusOK) })
	return r
}

// flipBase64 maps one base64url alphabet character to another, preserving length
// and validity so the tampered value still decodes to a same-length payload.
func flipBase64(c byte) byte {
	if c == 'A' {
		return 'B'
	}
	return 'A'
}
