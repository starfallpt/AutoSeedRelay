// Package web: QB proxy for session-passthrough to qBittorrent WebUI.
//
// GET/POST /qb/* -> reverse proxy to qBittorrent with automatic SID cookie
// injection. The relay logs into qB once, caches the SID, and injects it into
// every proxied request so the user never sees a qB login page.
package web

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// QBProxy is a reverse proxy that injects qBittorrent SID cookies so the
// browser-embedded qB WebUI is already authenticated.
type QBProxy struct {
	qbURL    string
	username string
	password string

	mu       sync.Mutex
	client   *http.Client // cookiejar-based, auto-manages SID
	loggedIn bool
}

// NewQBProxy creates a new qB proxy. qbURL is the full qB WebUI base URL
// (e.g. http://127.0.0.1:8080).
func NewQBProxy(qbURL, username, password string) *QBProxy {
	jar, _ := cookiejar.New(nil)
	return &QBProxy{
		qbURL:    strings.TrimRight(qbURL, "/"),
		username: username,
		password: password,
		client: &http.Client{
			Timeout: 30 * time.Second,
			Jar:     jar,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// ensureLogin logs into qB once. The cookiejar persists the SID cookie
// so all subsequent proxied requests are authenticated automatically.
func (p *QBProxy) ensureLogin() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.loggedIn {
		return nil
	}

	resp, err := p.client.PostForm(p.qbURL+"/api/v2/auth/login", url.Values{
		"username": {p.username},
		"password": {p.password},
	})
	if err != nil {
		return fmt.Errorf("qb proxy login: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	bodyStr := strings.TrimSpace(string(body))

	if strings.Contains(strings.ToLower(bodyStr), "fails") {
		return fmt.Errorf("qb proxy login: wrong credentials")
	}
	if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("qb proxy login: forbidden (IP banned or API disabled)")
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("qb proxy login: HTTP %d %s", resp.StatusCode, bodyStr)
	}

	p.loggedIn = true
	slog.Debug("qb proxy: logged in")
	return nil
}

// ServeHTTP handles proxying a request to qBittorrent. The cookiejar
// automatically attaches the SID cookie to every request.
func (p *QBProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := p.ensureLogin(); err != nil {
		http.Error(w, "QB proxy: login failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Build target URL: strip /qb prefix, forward to qB.
	targetPath := strings.TrimPrefix(r.URL.Path, "/qb")
	if targetPath == "" {
		targetPath = "/"
	}
	targetURL := p.qbURL + targetPath
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	// Create proxy request.
	proxyReq, err := http.NewRequest(r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, "QB proxy: bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Copy headers (except cookie, host).
	for k, vs := range r.Header {
		kl := strings.ToLower(k)
		if kl == "cookie" || kl == "host" {
			continue
		}
		for _, v := range vs {
			proxyReq.Header.Add(k, v)
		}
	}

	proxyReq.Header.Set("Referer", p.qbURL+"/")

	// Execute via cookiejar client — SID cookie attached automatically.
	resp, err := p.client.Do(proxyReq)
	if err != nil {
		http.Error(w, "QB proxy: upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// On 403, clear login state so we re-login next time.
	if resp.StatusCode == http.StatusForbidden {
		p.mu.Lock()
		p.loggedIn = false
		p.mu.Unlock()
	}

	// Copy headers back.
	for k, vs := range resp.Header {
		if strings.ToLower(k) == "set-cookie" {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}

	// WebSocket upgrade.
	if strings.ToLower(r.Header.Get("Upgrade")) == "websocket" {
		w.Header().Set("Upgrade", resp.Header.Get("Upgrade"))
		w.Header().Set("Connection", resp.Header.Get("Connection"))
	}

	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		slog.Debug("qb proxy: body copy error", "error", err)
	}
}

// Ensure httputil is available (used by reverse proxy if needed).
var _ = httputil.ReverseProxy{}
