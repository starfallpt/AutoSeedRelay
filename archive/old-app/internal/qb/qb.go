// Package qb implements a client for the qBittorrent WebUI API (v2),
// the download/seed side of the relay pipeline.
//
// Behaviors mirroring the Python original:
//   - Login via POST /api/v2/auth/login (qB5 returns HTTP 204, qB4
//     returns a text "Ok." body); the SID cookie is kept in the jar.
//   - Every request carries a Referer header to satisfy CSRF protection
//     (web_ui_csrf_protection_enabled=true).
//   - A 401/403 response triggers a single re-login and retry.
//   - Stop/start fall back from the qB5 verbs to qB4 pause/resume on 404.
//   - Delete always keeps data files unless delete_files is set.
//   - Adding a .torrent uses the multipart field name "torrents".
package qb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/autoseedrelay/go-relay/internal/bencode"
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

// completedSeedingStates is the set of qB states meaning "fully
// downloaded and actively seeding".
var completedSeedingStates = map[string]bool{
	"uploading": true,
	"stalledUP": true,
	"stoppedUP": true,
}

// QBittorrent is a qBittorrent WebUI API client.
type QBittorrent struct {
	host        string
	username    string
	password    string
	client      *http.Client
	loggedIn    atomic.Bool
	referer     string
	slowMu      sync.Mutex
	slowTracker map[string]*slowEntry
}

// NewQBittorrent creates a client for the given WebUI host (e.g.
// http://127.0.0.1:8080). timeout is in seconds and defaults to 30.
func NewQBittorrent(host, username, password string, timeout ...float64) (*QBittorrent, error) {
	t := 30.0
	if len(timeout) > 0 {
		t = timeout[0]
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("qb: create cookie jar: %w", err)
	}
	host = strings.TrimRight(host, "/")
	q := &QBittorrent{
		host:        host,
		username:    username,
		password:    password,
		referer:     host + "/",
		slowTracker: make(map[string]*slowEntry),
		client: &http.Client{
			Timeout: time.Duration(t * float64(time.Second)),
			Jar:     jar,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	return q, nil
}

// Close releases idle connections held by the underlying HTTP client.
func (q *QBittorrent) Close() {
	q.client.CloseIdleConnections()
}

// Cookies returns the cookies stored for the qB host URL.
func (q *QBittorrent) Cookies(u *url.URL) []*http.Cookie {
	return q.client.Jar.Cookies(u)
}

// SID returns the SID cookie value after successful login.
func (q *QBittorrent) SID() string {
	u, _ := url.Parse(q.host)
	for _, c := range q.client.Jar.Cookies(u) {
		if c.Name == "SID" {
			return c.Value
		}
	}
	return ""
}

// Login authenticates with qB and keeps the SID cookie. It is safe to
// call repeatedly; already-logged-in clients return immediately.
func (q *QBittorrent) Login() error {
	if q.loggedIn.Load() {
		return nil
	}
	resp, err := q.client.PostForm(q.host+"/api/v2/auth/login", url.Values{
		"username": {q.username},
		"password": {q.password},
	})
	if err != nil {
		return connErrf("无法连接 qB(%s): %v", q.host, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	if (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent) &&
		!strings.Contains(strings.ToLower(text), "fails") {
		q.loggedIn.Store(true)
		return nil
	}

	q.loggedIn.Store(false)
	if resp.StatusCode == http.StatusForbidden {
		return authErrf("登录被拒绝(HTTP 403):WebUI 可能禁用了 API,或 IP 被临时封禁")
	}
	if strings.Contains(strings.ToLower(text), "fails") {
		return authErrf("登录失败:用户名或密码错误(返回 Fails.)")
	}
	return authErrf("登录失败:HTTP %d 响应 %s", resp.StatusCode, truncate(text, 200))
}

func (q *QBittorrent) ensureLogin() error {
	if !q.loggedIn.Load() {
		return q.Login()
	}
	return nil
}

// bodyBuilder produces an HTTP request body plus its Content-Type, and
// is re-invoked on retry so multipart bodies are rebuilt fresh.
type bodyBuilder func() (io.Reader, string, error)

// request is the unified request entry point. Every request carries the
// Referer header for CSRF; a 401/403 response triggers one re-login and
// retry (guarded against infinite loops).
func (q *QBittorrent) request(method, path string, params url.Values, retry bool, build bodyBuilder) (*http.Response, error) {
	if err := q.ensureLogin(); err != nil {
		return nil, err
	}

	fullURL := q.host + path
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

	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Referer", q.referer)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := q.client.Do(req)
	if err != nil {
		return nil, connErrf("请求失败(%s %s): %v", method, fullURL, err)
	}

	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && retry {
		resp.Body.Close()
		q.loggedIn.Store(false)
		return q.request(method, path, params, false, build)
	}
	return resp, nil
}

func (q *QBittorrent) get(path string, params url.Values) (*http.Response, error) {
	return q.request(http.MethodGet, path, params, true, nil)
}

func (q *QBittorrent) postForm(path string, form url.Values) (*http.Response, error) {
	return q.request(http.MethodPost, path, nil, true, func() (io.Reader, string, error) {
		return strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", nil
	})
}

// filePart is an in-memory file for a multipart upload.
type filePart struct {
	filename string
	content  []byte
	mimetype string
}

func (q *QBittorrent) postMultipart(path string, form url.Values, files map[string]filePart) (*http.Response, error) {
	return q.request(http.MethodPost, path, nil, true, func() (io.Reader, string, error) {
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
func (q *QBittorrent) expectOK(resp *http.Response, action string) error {
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

// parseAddResponse interprets a torrents/add response: qB5 returns JSON,
// qB4 returns a text "Ok." / "Fails." body.
func parseAddResponse(resp *http.Response) (map[string]any, error) {
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := strings.TrimSpace(string(body))
	if resp.StatusCode == http.StatusOK && text == "Ok." {
		return map[string]any{"status": "Ok."}, nil
	}
	if resp.StatusCode == http.StatusOK && text == "Fails." {
		return nil, reqErrf("qB 添加种子失败(返回 Fails.):可能种子已存在或非法")
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err == nil {
		return m, nil
	}
	status := text
	if status == "" {
		status = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return map[string]any{"status": status, "http": resp.StatusCode}, nil
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

// AddTorrentFile uploads a local .torrent file to qB. The multipart
// field name is "torrents" (not "file").
func (q *QBittorrent) AddTorrentFile(path string, opts AddOptions) (map[string]any, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	form := url.Values{}
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
	files := map[string]filePart{
		"torrents": {filename: filepath.Base(path), content: content, mimetype: "application/x-bittorrent"},
	}
	resp, err := q.postMultipart("/api/v2/torrents/add", form, files)
	if err != nil {
		return nil, err
	}
	return parseAddResponse(resp)
}

// AddTorrentURL lets qB download a .torrent from an http(s) link or a
// magnet. opts.Cookie is the site login cookie sent along with qB's HTTP
// download request (used to bypass a source-site WAF).
func (q *QBittorrent) AddTorrentURL(torrentURL string, opts AddOptions) (map[string]any, error) {
	form := url.Values{"urls": {torrentURL}}
	if opts.Cookie != "" {
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
	resp, err := q.postForm("/api/v2/torrents/add", form)
	if err != nil {
		return nil, err
	}
	return parseAddResponse(resp)
}

// AddMagnet adds a magnet link to qB; identical to AddTorrentURL.
func (q *QBittorrent) AddMagnet(torrentURL string, opts AddOptions) (map[string]any, error) {
	return q.AddTorrentURL(torrentURL, opts)
}

// Info lists torrents. hashes may hold a single v1 infohash, multiple
// `|`-joined hashes, or nothing for all torrents.
func (q *QBittorrent) Info(hashes ...string) ([]map[string]any, error) {
	params := url.Values{}
	if len(hashes) > 0 {
		params.Set("hashes", strings.Join(hashes, "|"))
	}
	resp, err := q.get("/api/v2/torrents/info", params)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := truncate(string(body), 200)
	if resp.StatusCode == http.StatusForbidden {
		return nil, reqErrf("info 被拒(HTTP 403): %s", text)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, reqErrf("info 失败(HTTP %d): %s", resp.StatusCode, text)
	}
	var lst []map[string]any
	if err := json.Unmarshal(body, &lst); err != nil {
		return nil, reqErrf("info 响应解析失败: %v", err)
	}
	return lst, nil
}

// GetTorrent returns a single torrent by v1 infohash, or nil when no
// such torrent exists.
func (q *QBittorrent) GetTorrent(hash string) (map[string]any, error) {
	lst, err := q.Info(hash)
	if err != nil {
		return nil, err
	}
	if len(lst) > 0 {
		return lst[0], nil
	}
	return nil, nil
}

// ExportTorrent downloads the .torrent bytes for a single v1 infohash
// and verifies they decode as a valid torrent (contain an info dict).
func (q *QBittorrent) ExportTorrent(hash string) ([]byte, error) {
	resp, err := q.get("/api/v2/torrents/export", url.Values{"hash": {hash}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := truncate(string(body), 200)
	if resp.StatusCode == http.StatusNotFound {
		return nil, reqErrf("导出失败(HTTP 404):hash %s 未找到或元数据缺失(has_metadata=false)", hash)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, reqErrf("导出失败(HTTP %d): %s", resp.StatusCode, text)
	}
	obj, err := bencode.Decode(body)
	if err != nil {
		return nil, reqErrf("导出的响应不是合法 .torrent(%v)", err)
	}
	d, ok := obj.(map[string]any)
	if !ok {
		return nil, reqErrf("导出的响应不是合法 .torrent(缺 info 字典)")
	}
	if _, ok := d["info"]; !ok {
		return nil, reqErrf("导出的响应不是合法 .torrent(缺 info 字典)")
	}
	return body, nil
}

func (q *QBittorrent) stopStart(verb, hashes, oldVerb string) error {
	form := url.Values{"hashes": {hashes}}
	resp, err := q.postForm("/api/v2/torrents/"+verb, form)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		resp, err = q.postForm("/api/v2/torrents/"+oldVerb, form)
		if err != nil {
			return err
		}
	}
	return q.expectOK(resp, "torrents/"+verb)
}

// Stop pauses seeding (qB5 "stop", falls back to qB4 "pause"). hashes
// supports `|`-joined values.
func (q *QBittorrent) Stop(hashes string) error {
	return q.stopStart("stop", hashes, "pause")
}

// Start resumes seeding (qB5 "start", falls back to qB4 "resume").
func (q *QBittorrent) Start(hashes string) error {
	return q.stopStart("start", hashes, "resume")
}

// Delete removes torrents. deleteFiles defaults to false so data files
// are preserved.
func (q *QBittorrent) Delete(hashes string, deleteFiles ...bool) error {
	del := false
	if len(deleteFiles) > 0 {
		del = deleteFiles[0]
	}
	flag := "false"
	if del {
		flag = "true"
	}
	resp, err := q.postForm("/api/v2/torrents/delete", url.Values{
		"hashes":      {hashes},
		"deleteFiles": {flag},
	})
	if err != nil {
		return err
	}
	return q.expectOK(resp, "torrents/delete")
}

// Recheck re-hashes the given torrents (`|`-joined hashes).
func (q *QBittorrent) Recheck(hashes string) error {
	resp, err := q.postForm("/api/v2/torrents/recheck", url.Values{"hashes": {hashes}})
	if err != nil {
		return err
	}
	return q.expectOK(resp, "torrents/recheck")
}

// SetTags replaces the tags of torrents; an empty tags string clears all
// tags.
func (q *QBittorrent) SetTags(hashes, tags string) error {
	resp, err := q.postForm("/api/v2/torrents/setTags", url.Values{
		"hashes": {hashes},
		"tags":   {tags},
	})
	if err != nil {
		return err
	}
	return q.expectOK(resp, "torrents/setTags")
}

// AddTags appends comma-separated tags while keeping existing ones.
func (q *QBittorrent) AddTags(hashes, tags string) error {
	resp, err := q.postForm("/api/v2/torrents/addTags", url.Values{
		"hashes": {hashes},
		"tags":   {tags},
	})
	if err != nil {
		return err
	}
	return q.expectOK(resp, "torrents/addTags")
}

// DeleteTags deletes tag definitions (comma-separated).
func (q *QBittorrent) DeleteTags(tags string) error {
	resp, err := q.postForm("/api/v2/torrents/deleteTags", url.Values{"tags": {tags}})
	if err != nil {
		return err
	}
	return q.expectOK(resp, "torrents/deleteTags")
}

// IsCompletedSeeding reports whether a torrent is fully downloaded and
// actively seeding: progress==1, completed>0, completion_on!=-1 and the
// state belongs to the completed-seeding set.
func IsCompletedSeeding(t map[string]any) bool {
	progress, _ := t["progress"].(float64)
	completed, _ := toInt64(t["completed"])
	completionOn, _ := toInt64(t["completion_on"])
	state, _ := t["state"].(string)
	return progress == 1 && completed > 0 && completionOn != -1 && completedSeedingStates[state]
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case float64:
		return int64(n), true
	case string:
		i, err := strconv.ParseInt(n, 10, 64)
		return i, err == nil
	default:
		return 0, false
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
