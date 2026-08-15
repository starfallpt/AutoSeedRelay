// Package web implements cookie-based authentication for the web panel.
package web

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// sessionStore is an in-memory session store with 24-hour expiry.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time
}

func newSessionStore() *sessionStore {
	s := &sessionStore{sessions: make(map[string]time.Time)}
	go s.cleanupLoop()
	return s
}

func (s *sessionStore) create() string {
	b := make([]byte, 32)
	rand.Read(b)
	token := base64.URLEncoding.EncodeToString(b)
	s.mu.Lock()
	s.sessions[token] = time.Now().Add(24 * time.Hour)
	s.mu.Unlock()
	return token
}

func (s *sessionStore) validate(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expiry, ok := s.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(expiry) {
		delete(s.sessions, token)
		return false
	}
	return true
}

func (s *sessionStore) revoke(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

func (s *sessionStore) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		now := time.Now()
		for token, expiry := range s.sessions {
			if now.After(expiry) {
				delete(s.sessions, token)
			}
		}
		s.mu.Unlock()
	}
}

// rateLimiter tracks failed login attempts per IP address.
type rateLimiter struct {
	mu      sync.Mutex
	fails   map[string]int
	lastGC  time.Time
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{
		fails:  make(map[string]int),
		lastGC: time.Now(),
	}
}

const maxFailures = 5
const rateLimitDelay = 3 * time.Second

// check returns true if the request is rate-limited (should delay).
// It also increments the failure counter before returning.
func (rl *rateLimiter) checkAndIncrement(ip string) bool {
	rl.maybeGC()
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.fails[ip]++
	return rl.fails[ip] > maxFailures
}

// reset clears the failure counter for an IP (on successful login).
func (rl *rateLimiter) reset(ip string) {
	rl.mu.Lock()
	delete(rl.fails, ip)
	rl.mu.Unlock()
}

// maybeGC periodically purges old entries to prevent unbounded growth.
func (rl *rateLimiter) maybeGC() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if time.Since(rl.lastGC) < 1*time.Hour {
		return
	}
	rl.fails = make(map[string]int)
	rl.lastGC = time.Now()
}

// ---------------------------------------------------------------------------
// Auth middleware
// ---------------------------------------------------------------------------

// authWrap returns a HandlerFunc that requires a valid session cookie.
// Page requests are redirected to /login; API requests receive 401 JSON.
// During setup (before initialization), /qb/ proxy requests are allowed
// without authentication so the user can interact with qB WebUI.
func (s *Server) authWrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Allow /qb/ proxy without auth during setup wizard.
		if !s.initialized.Load() && strings.HasPrefix(r.URL.Path, "/qb/") {
			next(w, r)
			return
		}

		cookie, err := r.Cookie("session")
		if err != nil || !s.sessionStore.validate(cookie.Value) {
			if isAPIRequest(r) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]any{"error": "unauthorized"})
				return
			}
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next(w, r)
	}
}

func isAPIRequest(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		return true
	}
	// 前端 fetch 请求都带 Accept: application/json
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Login page handler
// ---------------------------------------------------------------------------

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	// If already logged in, redirect to dashboard.
	if cookie, err := r.Cookie("session"); err == nil && s.sessionStore.validate(cookie.Value) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	s.tmplMu.RLock()
	tmpl := s.loginTmpl
	s.tmplMu.RUnlock()

	if tmpl == nil {
		http.Error(w, "template not initialized", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, nil); err != nil {
		http.Error(w, "template render error", http.StatusInternalServerError)
	}
}

// ---------------------------------------------------------------------------
// POST /api/login
// ---------------------------------------------------------------------------

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}

	ip := clientIP(r)

	// Apply rate-limit delay if too many failures.
	if s.rateLimiter.checkAndIncrement(ip) {
		time.Sleep(rateLimitDelay)
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}

	if req.Password != s.cfg.Web.Password {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "密码错误"})
		return
	}

	// Successful login: reset rate-limit counter and create session.
	s.rateLimiter.reset(ip)

	token := s.sessionStore.create()
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   86400,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// POST /api/logout
// ---------------------------------------------------------------------------

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}

	if cookie, err := r.Cookie("session"); err == nil {
		s.sessionStore.revoke(cookie.Value)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// clientIP extracts the client IP from the request, stripping the port.
func clientIP(r *http.Request) string {
	// Check X-Forwarded-For header first (for reverse proxy setups).
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	// Fall back to RemoteAddr, strip port.
	ip := r.RemoteAddr
	if idx := strings.LastIndex(ip, ":"); idx != -1 {
		ip = ip[:idx]
	}
	return ip
}
