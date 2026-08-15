// Package qb implements a qBittorrent WebUI API client (v2) plus a
// multi-instance manager, the download/seed side of the relay pipeline.
//
// This is a port-and-improve of the legacy internal/qb package. Behaviors
// mirrored from the legacy client:
//   - Login via POST /api/v2/auth/login (qB5 returns HTTP 204 with an
//     empty body, qB4 returns HTTP 200 with a text "Ok." body); the SID
//     cookie is kept in a per-instance cookie jar.
//   - Every request carries a Referer header to satisfy qB's CSRF
//     protection (web_ui_csrf_protection_enabled=true).
//   - A 401/403 response triggers exactly one re-login and retry.
//   - Stop/start fall back from the qB5 verbs to the qB4 pause/resume on
//     a 404.
//   - Adding a .torrent uses the multipart field name "torrents".
//
// Improvements over the legacy client:
//   - Every method takes a context.Context (the legacy client dropped it).
//   - The constructor takes host and port separately and accepts an
//     injectable *http.Client (for tests and custom transports).
//   - Info returns a typed []*TorrentInfo instead of []map[string]any.
//   - The qB app version is probed once and cached.
package qb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// QBError is the base qB client error.
type QBError struct{ msg string }

// Error implements the error interface.
func (e *QBError) Error() string { return e.msg }

// QBConnectionError reports a network-layer failure (unable to reach qB).
type QBConnectionError struct{ QBError }

// QBAuthError reports a login failure or rejected authentication.
type QBAuthError struct{ QBError }

// QBRequestError reports a failed API call (HTTP status or invalid body).
type QBRequestError struct{ QBError }

func connErrf(format string, args ...any) error {
	return &QBConnectionError{QBError{fmt.Sprintf(format, args...)}}
}

func authErrf(format string, args ...any) error {
	return &QBAuthError{QBError{fmt.Sprintf(format, args...)}}
}

func reqErrf(format string, args ...any) error {
	return &QBRequestError{QBError{fmt.Sprintf(format, args...)}}
}

// DefaultTimeout is the HTTP client timeout used when no custom client is
// injected.
const DefaultTimeout = 30 * time.Second

// Instance is a single qBittorrent WebUI API client.
type Instance struct {
	host     string
	port     string
	username string
	password string

	baseURL string
	referer string

	client *http.Client
	jar    *cookiejar.Jar

	loggedIn atomic.Bool

	verMu     sync.Mutex
	version   string
	versionOK bool

	slowMu      sync.Mutex
	slowTracker map[string]*slowEntry
}

// Option configures an Instance at construction time.
type Option func(*Instance)

// WithHTTPClient injects a custom *http.Client (for tests or custom
// transports, e.g. a proxy or TLS skip-verify). The Instance still owns the
// SID cookie jar and always enforces a no-redirect policy, so the injected
// client's Jar and CheckRedirect are overwritten.
func WithHTTPClient(c *http.Client) Option {
	return func(i *Instance) {
		if c != nil {
			i.client = c
		}
	}
}

// NewInstance creates a client for the given WebUI host and port.
//
// host is a scheme + hostname (e.g. "http://127.0.0.1"); a missing scheme
// defaults to "http://". port is the WebUI port (e.g. "8080"); it may be
// empty when the host already carries an implicit default port.
func NewInstance(host, port, username, password string, opts ...Option) *Instance {
	jar, _ := cookiejar.New(nil)
	baseURL := buildBaseURL(host, port)
	i := &Instance{
		host:        host,
		port:        port,
		username:    username,
		password:    password,
		baseURL:     baseURL,
		referer:     baseURL + "/",
		jar:         jar,
		slowTracker: make(map[string]*slowEntry),
		client: &http.Client{
			Timeout: DefaultTimeout,
			Jar:     jar,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	for _, o := range opts {
		o(i)
	}
	// The SID cookie jar is owned by the Instance, and redirects must not
	// be followed (so 3xx responses surface to callers instead of being
	// transparently replayed). Enforce both regardless of the injected
	// client.
	i.client.Jar = i.jar
	i.client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return i
}

func buildBaseURL(host, port string) string {
	host = strings.TrimRight(strings.TrimSpace(host), "/")
	if host == "" {
		host = "http://127.0.0.1"
	}
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	port = strings.TrimSpace(port)
	if port == "" {
		return host
	}
	return host + ":" + port
}

// Close releases idle connections held by the underlying HTTP client.
func (i *Instance) Close() {
	i.client.CloseIdleConnections()
}

// SID returns the SID cookie value after a successful login ("" when not
// logged in).
func (i *Instance) SID() string {
	u, _ := url.Parse(i.baseURL)
	for _, c := range i.jar.Cookies(u) {
		if c.Name == "SID" {
			return c.Value
		}
	}
	return ""
}

// Login authenticates with qB and keeps the SID cookie. It is safe to call
// repeatedly; already-logged-in clients return immediately.
//
// qB5 answers 204 No Content (empty body) on success, qB4 answers 200 with
// a text "Ok." body ("Fails." on bad credentials).
func (i *Instance) Login(ctx context.Context) error {
	if i.loggedIn.Load() {
		return nil
	}

	form := url.Values{
		"username": {i.username},
		"password": {i.password},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, i.baseURL+"/api/v2/auth/login", strings.NewReader(form.Encode()))
	if err != nil {
		i.loggedIn.Store(false)
		return connErrf("构建登录请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", i.referer)

	resp, err := i.client.Do(req)
	if err != nil {
		i.loggedIn.Store(false)
		return connErrf("无法连接 qB(%s): %v", i.baseURL, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	text := strings.TrimSpace(string(body))
	lower := strings.ToLower(text)

	switch {
	case resp.StatusCode == http.StatusNoContent:
		// qB5 success (empty body).
		i.loggedIn.Store(true)
		return nil
	case resp.StatusCode == http.StatusOK && !strings.Contains(lower, "fails"):
		// qB4 success ("Ok." or empty).
		i.loggedIn.Store(true)
		return nil
	case resp.StatusCode == http.StatusOK:
		// qB4 bad credentials ("Fails.").
		i.loggedIn.Store(false)
		return authErrf("登录失败:用户名或密码错误(返回 Fails.)")
	case resp.StatusCode == http.StatusUnauthorized:
		i.loggedIn.Store(false)
		return authErrf("登录失败:用户名或密码错误(HTTP 401)")
	case resp.StatusCode == http.StatusForbidden:
		i.loggedIn.Store(false)
		return authErrf("登录被拒绝(HTTP 403):WebUI 可能禁用了 API,或 IP 被临时封禁")
	default:
		i.loggedIn.Store(false)
		return authErrf("登录失败:HTTP %d 响应 %s", resp.StatusCode, truncate(text, 200))
	}
}

func (i *Instance) ensureLogin(ctx context.Context) error {
	if !i.loggedIn.Load() {
		return i.Login(ctx)
	}
	return nil
}

// bodyBuilder produces an HTTP request body plus its Content-Type, and is
// re-invoked on retry so multipart bodies are rebuilt fresh.
type bodyBuilder func() (io.Reader, string, error)

// request is the unified request entry point. Every request carries the
// Referer header for CSRF; a 401/403 response triggers exactly one re-login
// and retry (guarded against infinite loops by the retry flag).
func (i *Instance) request(ctx context.Context, method, path string, params url.Values, retry bool, build bodyBuilder) (*http.Response, error) {
	if err := i.ensureLogin(ctx); err != nil {
		return nil, err
	}

	fullURL := i.baseURL + path
	if len(params) > 0 {
		fullURL += "?" + params.Encode()
	}

	var body io.Reader
	var contentType string
	if build != nil {
		var err error
		body, contentType, err = build()
		if err != nil {
			return nil, err
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Referer", i.referer)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := i.client.Do(req)
	if err != nil {
		return nil, connErrf("请求失败(%s %s): %v", method, fullURL, err)
	}

	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && retry {
		resp.Body.Close()
		i.loggedIn.Store(false)
		return i.request(ctx, method, path, params, false, build)
	}
	return resp, nil
}

func (i *Instance) get(ctx context.Context, path string, params url.Values) (*http.Response, error) {
	return i.request(ctx, http.MethodGet, path, params, true, nil)
}

func (i *Instance) postForm(ctx context.Context, path string, form url.Values) (*http.Response, error) {
	return i.request(ctx, http.MethodPost, path, nil, true, func() (io.Reader, string, error) {
		return strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", nil
	})
}

// filePart is an in-memory file for a multipart upload.
type filePart struct {
	filename string
	content  []byte
	mimetype string
}

func (i *Instance) postMultipart(ctx context.Context, path string, form url.Values, files map[string]filePart) (*http.Response, error) {
	return i.request(ctx, http.MethodPost, path, nil, true, func() (io.Reader, string, error) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		for k, vs := range form {
			for _, v := range vs {
				if err := w.WriteField(k, v); err != nil {
					return nil, "", err
				}
			}
		}
		for fieldName, fp := range files {
			hdr := make(textproto.MIMEHeader)
			hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, fieldName, fp.filename))
			hdr.Set("Content-Type", fp.mimetype)
			part, err := w.CreatePart(hdr)
			if err != nil {
				return nil, "", err
			}
			if _, err := part.Write(fp.content); err != nil {
				return nil, "", err
			}
		}
		if err := w.Close(); err != nil {
			return nil, "", err
		}
		return &buf, w.FormDataContentType(), nil
	})
}

// expectOK validates a write response, consuming and closing the body.
func (i *Instance) expectOK(resp *http.Response, action string) error {
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := truncate(string(body), 200)
	if resp.StatusCode == http.StatusForbidden {
		return reqErrf("%s 被拒(HTTP 403,可能是 CSRF 或登录态失效): %s", action, text)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return reqErrf("%s 失败(HTTP %d): %s", action, resp.StatusCode, text)
	}
	return nil
}

// parseAddResponse interprets a torrents/add response: qB5 returns an empty
// body or JSON, qB4 returns a text "Ok." / "Fails." body.
func parseAddResponse(resp *http.Response) (map[string]any, error) {
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := strings.TrimSpace(string(body))

	switch {
	case resp.StatusCode == http.StatusOK && text == "Fails.":
		return nil, reqErrf("qB 添加种子失败(返回 Fails.):可能种子已存在或非法")
	case resp.StatusCode >= http.StatusBadRequest:
		return nil, reqErrf("qB 添加种子失败(HTTP %d): %s", resp.StatusCode, truncate(text, 200))
	}

	if text == "Ok." || text == "" {
		return map[string]any{"status": "Ok."}, nil
	}

	var m map[string]any
	if err := json.Unmarshal(body, &m); err == nil {
		return m, nil
	}
	return map[string]any{"status": text, "http": resp.StatusCode}, nil
}

// AddOptions carries the optional form fields for torrents/add.
type AddOptions struct {
	Savepath     string
	Category     string
	Tags         string
	Cookie       string
	SkipChecking bool
	Paused       bool
}

func addForm(opts AddOptions, includeCookie bool) url.Values {
	form := url.Values{}
	if includeCookie && opts.Cookie != "" {
		form.Set("cookie", opts.Cookie)
	}
	if opts.Savepath != "" {
		form.Set("savepath", opts.Savepath)
	}
	if opts.Category != "" {
		form.Set("category", opts.Category)
	}
	if opts.Tags != "" {
		form.Set("tags", opts.Tags)
	}
	if opts.SkipChecking {
		form.Set("skip_checking", "true")
	}
	if opts.Paused {
		form.Set("paused", "true")
	}
	return form
}

// AddTorrentFile uploads a .torrent file to qB. The multipart field name is
// "torrents" (not "file"). filename is used as the upload's file name.
func (i *Instance) AddTorrentFile(ctx context.Context, filename string, data []byte, opts AddOptions) (map[string]any, error) {
	files := map[string]filePart{
		"torrents": {filename: filename, content: data, mimetype: "application/x-bittorrent"},
	}
	resp, err := i.postMultipart(ctx, "/api/v2/torrents/add", addForm(opts, false), files)
	if err != nil {
		return nil, err
	}
	return parseAddResponse(resp)
}

// AddTorrentURL lets qB download a .torrent from an http(s) link or a
// magnet. opts.Cookie is the site login cookie sent along with qB's HTTP
// download request (used to bypass a source-site WAF).
func (i *Instance) AddTorrentURL(ctx context.Context, torrentURL string, opts AddOptions) (map[string]any, error) {
	form := addForm(opts, true)
	form.Set("urls", torrentURL)
	resp, err := i.postForm(ctx, "/api/v2/torrents/add", form)
	if err != nil {
		return nil, err
	}
	return parseAddResponse(resp)
}

// AddMagnet adds a magnet link to qB; identical to AddTorrentURL.
func (i *Instance) AddMagnet(ctx context.Context, magnet string, opts AddOptions) (map[string]any, error) {
	return i.AddTorrentURL(ctx, magnet, opts)
}

// Delete removes torrents. deleteFiles controls whether the downloaded data
// files are deleted too (false preserves them).
func (i *Instance) Delete(ctx context.Context, hashes string, deleteFiles bool) error {
	flag := "false"
	if deleteFiles {
		flag = "true"
	}
	resp, err := i.postForm(ctx, "/api/v2/torrents/delete", url.Values{
		"hashes":      {hashes},
		"deleteFiles": {flag},
	})
	if err != nil {
		return err
	}
	return i.expectOK(resp, "torrents/delete")
}

// stopStart performs a start/stop action, falling back from the qB5 verb to
// the qB4 verb on a 404.
func (i *Instance) stopStart(ctx context.Context, verb, hashes, fallback string) error {
	form := url.Values{"hashes": {hashes}}
	resp, err := i.postForm(ctx, "/api/v2/torrents/"+verb, form)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		resp, err = i.postForm(ctx, "/api/v2/torrents/"+fallback, form)
		if err != nil {
			return err
		}
	}
	return i.expectOK(resp, "torrents/"+verb)
}

// Stop pauses seeding (qB5 "stop", falls back to qB4 "pause"). hashes
// supports `|`-joined values.
func (i *Instance) Stop(ctx context.Context, hashes string) error {
	return i.stopStart(ctx, "stop", hashes, "pause")
}

// Start resumes seeding (qB5 "start", falls back to qB4 "resume").
func (i *Instance) Start(ctx context.Context, hashes string) error {
	return i.stopStart(ctx, "start", hashes, "resume")
}

// Version returns the qB app version, probing once and caching the result.
func (i *Instance) Version(ctx context.Context) (string, error) {
	i.verMu.Lock()
	if i.versionOK {
		v := i.version
		i.verMu.Unlock()
		return v, nil
	}
	i.verMu.Unlock()

	v, err := i.probeVersion(ctx)
	if err != nil {
		return "", err
	}

	i.verMu.Lock()
	i.version = v
	i.versionOK = true
	i.verMu.Unlock()
	return v, nil
}

// Ping performs a live health check: a fresh version probe (which also
// authenticates on first use). It returns the qB app version on success.
func (i *Instance) Ping(ctx context.Context) (string, error) {
	return i.probeVersion(ctx)
}

func (i *Instance) probeVersion(ctx context.Context) (string, error) {
	resp, err := i.get(ctx, "/api/v2/app/version", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := truncate(string(body), 200)
	if resp.StatusCode != http.StatusOK {
		return "", reqErrf("获取 qB 版本失败(HTTP %d): %s", resp.StatusCode, text)
	}
	v := strings.TrimSpace(string(body))
	if v == "" {
		return "", reqErrf("获取 qB 版本失败:空响应")
	}
	return v, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
