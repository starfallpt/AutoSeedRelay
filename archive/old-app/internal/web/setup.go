// Package web: setup wizard API handlers for first-run initialization.
package web

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/autoseedrelay/go-relay/internal/config"
	"github.com/autoseedrelay/go-relay/internal/qb"
)

// ---------------------------------------------------------------------------
// Preset target site definitions
// ---------------------------------------------------------------------------

type presetTargetDef struct {
	Name        string
	Type        string
	BaseURL     string
	AnnounceURL string
}

var presetTargets = []presetTargetDef{
	{
		Name:    "dev.internal-source.org",
		Type:    "classic",
		BaseURL: "https://dev.internal-source.org",
	},
	{
		Name:    "luckpt.de",
		Type:    "nexusphp",
		BaseURL: "https://luckpt.de",
	},
	{
		Name:        "M-Team",
		Type:        "mteam",
		BaseURL:     "https://api.m-team.cc/api",
		AnnounceURL: "https://tracker.m-team.cc/announce?credential={credential}",
	},
}

// lookupPreset returns the preset definition for a target name, or nil.
func lookupPreset(name string) *presetTargetDef {
	for i := range presetTargets {
		if presetTargets[i].Name == name {
			return &presetTargets[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Setup state types
// ---------------------------------------------------------------------------

// SetupStatus represents the current initialization state.
type SetupStatus struct {
	Initialized bool       `json:"initialized"`
	Steps       SetupSteps `json:"steps"`
}

// SetupSteps tracks progress through the three required steps.
type SetupSteps struct {
	QB      QBStepStatus      `json:"qb"`
	Source  SiteStepStatus    `json:"source"`
	Targets TargetsStepStatus `json:"targets"`
}

// QBStepStatus tracks the qBittorrent connection step.
type QBStepStatus struct {
	Done  bool   `json:"done"`
	Error string `json:"error,omitempty"`
}

// SiteStepStatus tracks the source site configuration step.
type SiteStepStatus struct {
	Done  bool   `json:"done"`
	Error string `json:"error,omitempty"`
}

// TargetsStepStatus tracks the target sites configuration step.
type TargetsStepStatus struct {
	Done  bool   `json:"done"`
	Count int    `json:"count"`
}

// ---------------------------------------------------------------------------
// GET /api/setup/status
// ---------------------------------------------------------------------------

func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	status := s.buildSetupStatus()
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) buildSetupStatus() SetupStatus {
	status := SetupStatus{
		Initialized: s.initialized.Load(),
	}

	// QB step.
	if s.setupQBSet {
		status.Steps.QB.Done = true
	}

	// Source step.
	if s.cfg != nil && len(s.cfg.Sources) > 0 {
		status.Steps.Source.Done = true
	}

	// Targets step.
	if s.cfg != nil && len(s.cfg.Targets) > 0 {
		status.Steps.Targets.Done = true
		status.Steps.Targets.Count = len(s.cfg.Targets)
	}

	return status
}

// ---------------------------------------------------------------------------
// POST /api/setup/qb — save qB connection info + test connection
// ---------------------------------------------------------------------------

type setupQBRequest struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	UseSSL      bool   `json:"use_ssl"`
	NewPassword string `json:"new_password,omitempty"`
}

type setupQBResponse struct {
	OK         bool    `json:"ok"`
	Error      string  `json:"error,omitempty"`
	Version    string  `json:"version,omitempty"`
	DiskFreeGB float64 `json:"disk_free_gb,omitempty"`
	DiskTotalGB float64 `json:"disk_total_gb,omitempty"`
}

func (s *Server) handleSetupQB(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	var req setupQBRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求格式无效: " + err.Error()})
		return
	}

	// Apply defaults for fixed fields.
	if req.Host == "" {
		req.Host = "qbittorrent"
	}
	if req.Port <= 0 {
		req.Port = 8080
	}
	if req.Username == "" {
		req.Username = "admin"
	}
	if req.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "密码不能为空"})
		return
	}

	// Test the qB connection.
	version, freeGB, totalGB, err := testQBConnection(req.Host, req.Port, req.Username, req.Password, req.UseSSL)
	if err != nil {
		writeJSON(w, http.StatusOK, setupQBResponse{
			OK:    false,
			Error: err.Error(),
		})
		return
	}

	// Save to in-memory config.
	if s.cfg != nil {
		s.cfg.QB.Host = req.Host
		s.cfg.QB.Port = req.Port
		s.cfg.QB.Username = req.Username
		s.cfg.QB.Password = req.Password
		s.cfg.QB.UseSSL = req.UseSSL

		// Initialize the qB proxy now so /qb/ routes work during setup.
		s.qbProxyMu.Lock()
		s.qbProxy = NewQBProxy(s.cfg.QB.URL(), s.cfg.QB.Username, s.cfg.QB.Password)
		s.qbProxyMu.Unlock()
	}

	slog.Info("setup: qB connection verified",
		"host", req.Host,
		"port", req.Port,
		"version", version,
	)

	s.setupQBSet = true

	writeJSON(w, http.StatusOK, setupQBResponse{
		OK:         true,
		Version:    version,
		DiskFreeGB: freeGB,
		DiskTotalGB: totalGB,
	})
}

// ---------------------------------------------------------------------------
// POST /api/setup/source — save source site configuration
// ---------------------------------------------------------------------------

type setupSourceRequest struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	RSSURL  string `json:"rss_url"`
	APIToken string `json:"api_token"`
	Token   string `json:"token"`
	Cookie  string `json:"cookie"`
	Passkey string `json:"passkey"`
}

func (s *Server) handleSetupSource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	var req setupSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求格式无效: " + err.Error()})
		return
	}

	// Auto-fill name and base_url to fixed values for 星陨阁.
	req.Name = "星陨阁"
	req.BaseURL = "https://pt.internal-source.org"

	// Build RSS URL from passkey if provided.
	if req.RSSURL == "" && req.Passkey != "" {
		req.RSSURL = "https://pt.internal-source.org/torrentrss.php?passkey=" + req.Passkey
	}

	// Build site profile.
	apiToken := req.APIToken
	if apiToken == "" {
		apiToken = req.Token
	}
	site := &config.SiteProfile{
		Name:     req.Name,
		Role:     config.RoleSource,
		BaseURL:  req.BaseURL,
		RSSURL:   req.RSSURL,
		APIToken: apiToken,
		Cookie:   req.Cookie,
		Passkey:  req.Passkey,
	}

	// Replace the source list with this single source.
	s.cfg.Sources = []*config.SiteProfile{site}

	slog.Info("setup: source site configured", "name", req.Name)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"name": req.Name,
	})
}

// ---------------------------------------------------------------------------
// POST /api/setup/targets — save target sites configuration
// ---------------------------------------------------------------------------

type setupTargetItem struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`

	// Classic (dev.internal-source.org) fields.
	Cookie  string `json:"cookie,omitempty"`
	Passkey string `json:"passkey,omitempty"`

	// NexusPHP / M-Team fields.
	APIToken string `json:"api_token,omitempty"`
}

type setupTargetsRequest struct {
	Targets []setupTargetItem `json:"targets"`
}

func (s *Server) handleSetupTargets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	var req setupTargetsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求格式无效: " + err.Error()})
		return
	}

	// Build SiteProfiles from enabled targets.
	var profiles []*config.SiteProfile
	for _, t := range req.Targets {
		if !t.Enabled {
			continue
		}

		preset := lookupPreset(t.Name)
		if preset == nil {
			slog.Warn("setup: unknown preset target, skipping", "name", t.Name)
			continue
		}

		site := &config.SiteProfile{
			Name:        t.Name,
			Role:        config.RoleTarget,
			BaseURL:     preset.BaseURL,
			AnnounceURL: preset.AnnounceURL,
		}

		switch preset.Type {
		case "classic":
			site.Cookie = t.Cookie
			site.Passkey = t.Passkey
		case "nexusphp":
			site.APIToken = t.APIToken
		case "mteam":
			site.MTeamAuth = t.APIToken
		}

		profiles = append(profiles, site)
	}

	if len(profiles) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请至少启用一个目标站并填写凭据"})
		return
	}

	s.cfg.Targets = profiles

	slog.Info("setup: target sites configured", "count", len(profiles))

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"count": len(profiles),
	})
}

// ---------------------------------------------------------------------------
// POST /api/setup/complete — finish initialization, write relay.yaml
// ---------------------------------------------------------------------------

type setupCompleteRequest struct {
	WebPassword    string `json:"web_password"`
	SyncQBPassword bool   `json:"sync_qb_password"`
}

func (s *Server) handleSetupComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	// Validate: all three steps must be done.
	status := s.buildSetupStatus()
	if !status.Steps.QB.Done {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请先完成 qBittorrent 连接配置"})
		return
	}
	if !status.Steps.Source.Done {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请先完成源站配置"})
		return
	}
	if !status.Steps.Targets.Done {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请先完成目标站配置"})
		return
	}

	var req setupCompleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// Web password is optional; ignore parse errors for the body.
		req.WebPassword = ""
	}

	// Set web panel password. If empty, keep the existing default.
	if req.WebPassword != "" {
		s.cfg.Web.Password = req.WebPassword
	}

	// Sync qBittorrent password if requested.
	if req.SyncQBPassword && req.WebPassword != "" && req.WebPassword != s.cfg.QB.Password {
		if err := changeQBPassword(
			s.cfg.QB.Host, s.cfg.QB.Port, s.cfg.QB.Username,
			s.cfg.QB.Password, req.WebPassword, s.cfg.QB.UseSSL,
		); err != nil {
			slog.Warn("setup: failed to sync qB password", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"ok":    false,
				"error": "同步 qBittorrent 密码失败: " + err.Error(),
			})
			return
		}
		s.cfg.QB.Password = req.WebPassword
		slog.Info("setup: qB password synced")
	}

	// Write config to disk.
	if err := config.SaveAppConfig(s.cfg, ""); err != nil {
		slog.Error("setup: failed to save config", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":    false,
			"error": "保存配置文件失败: " + err.Error(),
		})
		return
	}

	// Reload QB proxy with new credentials.
	if s.cfg.QB.Host != "" {
		s.qbProxyMu.Lock()
		s.qbProxy = NewQBProxy(s.cfg.QB.URL(), s.cfg.QB.Username, s.cfg.QB.Password)
		s.qbProxyMu.Unlock()
	}

	// Mark as initialized.
	s.initialized.Store(true)

	slog.Info("setup: initialization completed",
		"sources", len(s.cfg.Sources),
		"targets", len(s.cfg.Targets),
		"qb_host", s.cfg.QB.Host,
	)

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// QB connection test (standalone, not using QBProxy)
// ---------------------------------------------------------------------------

func testQBConnection(host string, port int, username, password string, useSSL bool) (version string, freeGB float64, totalGB float64, err error) {
	scheme := "http"
	if useSSL {
		scheme = "https"
	}
	baseURL := fmt.Sprintf("%s://%s:%d", scheme, host, port)

	// Use qb client which has proper cookiejar + 204 handling
	qc, err := qb.NewQBittorrent(baseURL, username, password, 10)
	if err != nil {
		return "", 0, 0, fmt.Errorf("无法连接到 qBittorrent (%s)", baseURL)
	}
	defer qc.Close()

	if err := qc.Login(); err != nil {
		return "", 0, 0, err
	}

	disk, err := qc.GetDiskSpace()
	if err == nil {
		freeGB = float64(disk.FreeOnDisk) / (1024 * 1024 * 1024)
		totalGB = float64(disk.Total) / (1024 * 1024 * 1024)
	}
	return "qBittorrent", freeGB, totalGB, nil
}

// qbAPIGet performs an authenticated GET request to the qB API and returns
// the trimmed response body as a string.
func qbAPIGet(client *http.Client, baseURL, sid, path string) (string, error) {
	body, err := qbAPIGetRaw(client, baseURL, sid, path)
	if err != nil {
		return "", err
	}
	// Trim newlines / whitespace from simple values like version strings.
	trimmed := ""
	for _, b := range []byte(body) {
		c := rune(b)
		if c != '\n' && c != '\r' && c != ' ' && c != '\t' {
			trimmed += string(b)
		}
	}
	// If the trimmed result is empty, return the raw body.
	if trimmed == "" {
		return body, nil
	}
	return trimmed, nil
}

// qbAPIGetRaw performs an authenticated GET request to the qB API and returns
// the raw response body.
func qbAPIGetRaw(client *http.Client, baseURL, sid, path string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
	if err != nil {
		return "", fmt.Errorf("构建请求失败: %w", err)
	}
	req.Header.Set("Cookie", "SID="+sid)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求 qB API 失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("读取 qB 响应失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("qB API 返回 HTTP %d: %s", resp.StatusCode, string(body))
	}

	return string(body), nil
}

// changeQBPassword changes the qBittorrent WebUI password via the setPreferences API.
// It first logs in with the current credentials, then POSTs the new password.
func changeQBPassword(host string, port int, username, password, newPassword string, useSSL bool) error {
	scheme := "http"
	if useSSL {
		scheme = "https"
	}
	baseURL := fmt.Sprintf("%s://%s:%d", scheme, host, port)

	client := &http.Client{Timeout: 10 * time.Second}

	// Step 1: Login with current credentials.
	resp, err := client.PostForm(baseURL+"/api/v2/auth/login", url.Values{
		"username": {username},
		"password": {password},
	})
	if err != nil {
		return fmt.Errorf("login to change password: %w", err)
	}
	defer resp.Body.Close()

	var sid string
	for _, c := range resp.Cookies() {
		if c.Name == "SID" && c.Value != "" {
			sid = c.Value
			break
		}
	}
	if sid == "" {
		if m := regexp.MustCompile(`SID=([^;]+)`).FindStringSubmatch(resp.Header.Get("Set-Cookie")); m != nil {
			sid = m[1]
		}
	}
	if sid == "" {
		return fmt.Errorf("no SID from login when changing password")
	}

	// Step 2: Change password via setPreferences.
	body := fmt.Sprintf(`{"web_ui_password":"%s"}`, newPassword)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v2/app/setPreferences", strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("build setPreferences request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", "SID="+sid)

	resp2, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("setPreferences: %w", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp2.Body)
		return fmt.Errorf("setPreferences returned HTTP %d: %s", resp2.StatusCode, string(respBody))
	}

	return nil
}
