package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autoseedrelay/relay/internal/auth"
	"github.com/autoseedrelay/relay/internal/store"
)

// newAuthServer returns a fully-wired server handler backed by a temp SQLite DB
// and a fresh (uninitialized) auth manager.
func newAuthServer(t *testing.T) http.Handler {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	m, err := auth.New(st.DB(), nil, auth.Options{})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	srv, err := New(nil, nil, Deps{Store: st, Auth: m})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv.Handler()
}

func doJSON(h http.Handler, method, path, body string, cookies []*http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func cookieByName(cookies []*http.Cookie, name string) *http.Cookie {
	for _, ck := range cookies {
		if ck.Name == name {
			return ck
		}
	}
	return nil
}

// login performs a full setup + login against h and returns the session/CSRF
// cookies plus the CSRF token issued by the server.
func setupAndLogin(t *testing.T, h http.Handler, pw string) ([]*http.Cookie, string) {
	t.Helper()
	if w := doJSON(h, "POST", "/api/v2/setup/complete", `{"password":"`+pw+`"}`, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("setup/complete = %d, want 200", w.Code)
	}
	w := doJSON(h, "POST", "/api/v2/auth/login", `{"password":"`+pw+`"}`, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("login = %d, want 200", w.Code)
	}
	cookies := w.Result().Cookies()
	tok := w.Header().Get("X-CSRF-Token")
	if cookieByName(cookies, "autoseed_session") == nil {
		t.Fatal("login response missing session cookie")
	}
	if cookieByName(cookies, "csrf_token") == nil {
		t.Fatal("login response missing CSRF cookie")
	}
	if tok == "" {
		t.Fatal("login response missing X-CSRF-Token header")
	}
	return cookies, tok
}

func TestSetupStatusCompleteFlow(t *testing.T) {
	h := newAuthServer(t)

	if w := doJSON(h, "GET", "/api/v2/setup/status", "", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("setup/status = %d, want 200", w.Code)
	} else if !strings.Contains(w.Body.String(), `"initialized":false`) {
		t.Fatalf("setup/status body = %q, want initialized:false", w.Body.String())
	}

	if w := doJSON(h, "POST", "/api/v2/setup/complete", `{"password":"pw"}`, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("setup/complete = %d, want 200", w.Code)
	}
	if w := doJSON(h, "GET", "/api/v2/setup/status", "", nil, nil); !strings.Contains(w.Body.String(), `"initialized":true`) {
		t.Fatalf("setup/status after complete = %q, want initialized:true", w.Body.String())
	}
	// Second completion is forbidden.
	if w := doJSON(h, "POST", "/api/v2/setup/complete", `{"password":"pw2"}`, nil, nil); w.Code != http.StatusForbidden {
		t.Fatalf("second setup/complete = %d, want 403", w.Code)
	}
}

func TestSetupCompleteIssuesSession(t *testing.T) {
	h := newAuthServer(t)

	w := doJSON(h, "POST", "/api/v2/setup/complete", `{"password":"pw"}`, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("setup/complete = %d, want 200", w.Code)
	}
	cookies := w.Result().Cookies()
	tok := w.Header().Get("X-CSRF-Token")
	if cookieByName(cookies, "autoseed_session") == nil {
		t.Fatal("setup/complete response missing session cookie")
	}
	if cookieByName(cookies, "csrf_token") == nil {
		t.Fatal("setup/complete response missing csrf_token cookie")
	}
	if tok == "" {
		t.Fatal("setup/complete response missing X-CSRF-Token header")
	}

	// No second login needed: the setup-issued session already authorizes /me.
	if w := doJSON(h, "GET", "/api/v2/auth/me", "", cookies, nil); w.Code != http.StatusOK {
		t.Fatalf("me after setup = %d, want 200", w.Code)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	h := newAuthServer(t)
	if w := doJSON(h, "POST", "/api/v2/setup/complete", `{"password":"pw"}`, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("setup/complete = %d", w.Code)
	}
	if w := doJSON(h, "POST", "/api/v2/auth/login", `{"password":"wrong"}`, nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-password login = %d, want 401", w.Code)
	}
}

func TestLoginRateLimited(t *testing.T) {
	h := newAuthServer(t)
	if w := doJSON(h, "POST", "/api/v2/setup/complete", `{"password":"pw"}`, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("setup/complete = %d", w.Code)
	}
	for i := 0; i < 5; i++ {
		if w := doJSON(h, "POST", "/api/v2/auth/login", `{"password":"wrong"}`, nil, nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("login attempt %d = %d, want 401", i+1, w.Code)
		}
	}
	w := doJSON(h, "POST", "/api/v2/auth/login", `{"password":"wrong"}`, nil, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("6th login = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 response missing Retry-After header")
	}
}

func TestUninitializedProtectedForbidden(t *testing.T) {
	h := newAuthServer(t)

	// Protected routes are 403 (not 401) while uninitialized.
	if w := doJSON(h, "GET", "/api/v2/auth/me", "", nil, nil); w.Code != http.StatusForbidden {
		t.Fatalf("uninitialized /auth/me = %d, want 403", w.Code)
	}
	// login is exempt from the middleware but its handler still 403s pre-setup.
	if w := doJSON(h, "POST", "/api/v2/auth/login", `{"password":"pw"}`, nil, nil); w.Code != http.StatusForbidden {
		t.Fatalf("uninitialized login = %d, want 403", w.Code)
	}
}

func TestMeLogoutFlow(t *testing.T) {
	h := newAuthServer(t)
	cookies, tok := setupAndLogin(t, h, "pw")

	// /me with valid session.
	w := doJSON(h, "GET", "/api/v2/auth/me", "", cookies, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("me = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("me body = %q, want ok:true", w.Body.String())
	}

	// logout with session + CSRF.
	w = doJSON(h, "POST", "/api/v2/auth/logout", "", cookies, map[string]string{"X-CSRF-Token": tok})
	if w.Code != http.StatusOK {
		t.Fatalf("logout = %d, want 200", w.Code)
	}

	// The logout response clears the session cookie. Replaying the cleared
	// cookies means /me no longer presents a valid session → 401.
	cleared := w.Result().Cookies()
	if cookieByName(cleared, "autoseed_session") == nil {
		t.Fatal("logout response did not clear the session cookie")
	}
	w = doJSON(h, "GET", "/api/v2/auth/me", "", cleared, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("me after logout = %d, want 401", w.Code)
	}
}

func TestCSRFMissingRejected(t *testing.T) {
	h := newAuthServer(t)
	cookies, _ := setupAndLogin(t, h, "pw")

	// Authenticated POST without a CSRF header must be rejected 403.
	if w := doJSON(h, "POST", "/api/v2/auth/logout", "", cookies, nil); w.Code != http.StatusForbidden {
		t.Fatalf("logout without CSRF = %d, want 403", w.Code)
	}
}

func TestCookieTamperedRejected(t *testing.T) {
	h := newAuthServer(t)
	cookies, _ := setupAndLogin(t, h, "pw")

	sess := cookieByName(cookies, "autoseed_session")
	tampered := *sess
	tampered.Value = sess.Value + "x" // malformed → rejected
	bad := []*http.Cookie{&tampered}

	if w := doJSON(h, "GET", "/api/v2/auth/me", "", bad, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("me with tampered cookie = %d, want 401", w.Code)
	}
}

func TestLoginAuthNil503(t *testing.T) {
	srv, err := New(nil, nil, Deps{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if w := doJSON(srv.Handler(), "POST", "/api/v2/auth/login", `{"password":"pw"}`, nil, nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("login with nil auth = %d, want 503", w.Code)
	}
}
