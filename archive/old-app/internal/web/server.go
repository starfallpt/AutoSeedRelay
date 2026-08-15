// Package web implements the v3 HTTP server, REST API handlers, and qB proxy
// for the AutoSeedRelay management dashboard.
package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/autoseedrelay/go-relay/internal/config"
)

//go:embed templates/*
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Server is the v3 Web UI and API server.
type Server struct {
	addr   string
	engine EngineInterface
	srv    *http.Server
	mux    *http.ServeMux
	cfg    *config.AppConfig

	// Template cache (parsed once at startup).
	baseTmpl    *template.Template
	loginTmpl   *template.Template
	setupTmpl   *template.Template
	previewTmpl *template.Template
	tmplMu      sync.RWMutex

	// In-memory log buffer for the /api/logs endpoint.
	logBuf    []LogEntry
	logBufMu  sync.Mutex
	logBufMax int

	// QB proxy.
	qbProxy   *QBProxy
	qbProxyMu sync.RWMutex

	// Auth
	sessionStore *sessionStore
	rateLimiter  *rateLimiter

	// Setup wizard state.
	initialized atomic.Bool
	setupQBSet  bool

	started atomic.Bool
}

// EngineInterface is the interface the web server expects from the engine.
type EngineInterface interface {
	GetStats() EngineStats
	GetSeeds(filter SeedFilter) ([]SeedInfo, int, error)
	GetSeedDetail(id int64) (*SeedDetail, error)
	RetireSeed(id int64) error
	RetrySeed(id int64) error
	GetConfig() *config.AppConfig
	SaveConfig(cfg *config.AppConfig) error
	IsRunning() bool
}

// EngineStats is returned by the engine for the /api/status endpoint.
type EngineStats struct {
	TotalPublished   int64   `json:"total_published"`
	TotalCrossSeeded int64   `json:"total_cross_seeded"`
	CurrentSeeding   int     `json:"current_seeding"`
	DiskFreeGB       float64 `json:"disk_free_gb"`
	DiskTotalGB      float64 `json:"disk_total_gb"`
	TodayPublished   int     `json:"today_published"`
	TodayCrossSeeded int     `json:"today_cross_seeded"`
	QBConnected      bool    `json:"qb_connected"`
	UptimeSeconds    int64   `json:"uptime_seconds"`
}

// SeedFilter is passed to the engine to filter the seed list.
type SeedFilter struct {
	Status string `json:"status"`
	Target string `json:"target"`
	Query  string `json:"q"`
	Page   int    `json:"page"`
	Limit  int    `json:"limit"`
}

// SeedInfo is a single seed in the list view.
type SeedInfo struct {
	ID           int64   `json:"id"`
	InfoHash     string  `json:"info_hash"`
	Title        string  `json:"title"`
	SourceSite   string  `json:"source_site"`
	SourceSize   int64   `json:"source_size"`
	Status       string  `json:"status"`
	TargetSite   string  `json:"target_site"`
	TargetID     int64   `json:"target_id"`
	Progress     float64 `json:"progress"`
	DLSpeed      int64   `json:"dl_speed"`
	UPSpeed      int64   `json:"up_speed"`
	Ratio        float64 `json:"ratio"`
	Seeders      int     `json:"seeders"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
	RetryCount   int     `json:"retry_count"`
	ErrorMsg     string  `json:"error,omitempty"`
}

// SeedDetail is the full detail view for a single seed.
type SeedDetail struct {
	SeedInfo
	Records []RecordEntry `json:"records"`
}

// RecordEntry is a single relay record / timeline entry.
type RecordEntry struct {
	ID        int64  `json:"id"`
	SeedID    int64  `json:"seed_id"`
	Timestamp string  `json:"timestamp"`
	Action    string  `json:"action"`
	Detail    string  `json:"detail"`
}

// LogEntry is a buffered log line.
type LogEntry struct {
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Message   string `json:"message"`
}

// NewServer creates a new web server.
func NewServer(addr string, engine EngineInterface, cfg *config.AppConfig) *Server {
	s := &Server{
		addr:         addr,
		engine:       engine,
		cfg:          cfg,
		mux:          http.NewServeMux(),
		logBufMax:    1000,
		sessionStore: newSessionStore(),
		rateLimiter:  newRateLimiter(),
	}

	// Initialize templates.
	if err := s.initTemplates(); err != nil {
		slog.Error("failed to initialize templates", "error", err)
	}

	// Create QB proxy if config has credentials.
	if cfg != nil && cfg.QB.Host != "" {
		s.qbProxy = NewQBProxy(cfg.QB.URL(), cfg.QB.Username, cfg.QB.Password)
	}

	// Determine if the config is meaningful (loaded from a real file).
	if cfg != nil && len(cfg.Sources) > 0 && len(cfg.Targets) > 0 {
		s.initialized.Store(true)
	}

	s.registerRoutes()
	return s
}

// initTemplates parses layout.html as the base template (page templates
// are cloned and added per-request to avoid {{define}} collisions).
// It also parses the standalone login page template.
func (s *Server) initTemplates() error {
	s.tmplMu.Lock()
	defer s.tmplMu.Unlock()

	// Read static asset content for inline embedding.
	styleCSS, _ := fs.ReadFile(staticFS, "static/style.css")
	appJS, _ := fs.ReadFile(staticFS, "static/app.js")

	funcMap := template.FuncMap{
		"FetchStyleCSS": func() template.CSS {
			return template.CSS(string(styleCSS))
		},
		"FetchAppJS": func() template.JS {
			return template.JS(string(appJS))
		},
	}

	// Parse layout.html as the base template.
	layoutBytes, err := fs.ReadFile(templatesFS, "templates/layout.html")
	if err != nil {
		return fmt.Errorf("read layout.html: %w", err)
	}

	tmpl := template.New("layout").Funcs(funcMap)
	tmpl, err = tmpl.Parse(string(layoutBytes))
	if err != nil {
		return fmt.Errorf("parse layout.html: %w", err)
	}

	s.baseTmpl = tmpl

	// Parse the standalone login page template.
	loginBytes, err := fs.ReadFile(templatesFS, "templates/login.html")
	if err != nil {
		return fmt.Errorf("read login.html: %w", err)
	}

	loginTmpl, err := template.New("login").Parse(string(loginBytes))
	if err != nil {
		return fmt.Errorf("parse login.html: %w", err)
	}
	s.loginTmpl = loginTmpl

	// Parse the standalone setup wizard page template.
	setupBytes, err := fs.ReadFile(templatesFS, "templates/setup.html")
	if err != nil {
		// setup.html is optional; warn but don't fail.
		slog.Warn("setup.html template not found; setup wizard UI will be unavailable", "error", err)
		return nil
	}

	setupTmpl, err := template.New("setup").Parse(string(setupBytes))
	if err != nil {
		return fmt.Errorf("parse setup.html: %w", err)
	}
	s.setupTmpl = setupTmpl

	// Parse the preview page template (clone base layout + preview content).
	previewBytes, err := fs.ReadFile(templatesFS, "templates/preview.html")
	if err != nil {
		slog.Warn("preview.html template not found; preview page will be unavailable", "error", err)
	} else {
		previewTmpl, err := s.baseTmpl.Clone()
		if err != nil {
			slog.Warn("failed to clone base template for preview", "error", err)
		} else {
			if _, err := previewTmpl.Parse(string(previewBytes)); err != nil {
				slog.Warn("failed to parse preview.html", "error", err)
			} else {
				s.previewTmpl = previewTmpl
			}
		}
	}

	return nil
}

func (s *Server) registerRoutes() {
	// Static files (no auth required).
	staticSub, _ := fs.Sub(staticFS, "static")
	s.mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	// Setup wizard routes (no auth required, accessible before initialization).
	s.mux.HandleFunc("/setup", s.handleSetupPage)
	s.mux.HandleFunc("/api/setup/status", s.handleSetupStatus)
	s.mux.HandleFunc("/api/setup/qb", s.handleSetupQB)
	s.mux.HandleFunc("/api/setup/source", s.handleSetupSource)
	s.mux.HandleFunc("/api/setup/targets", s.handleSetupTargets)
	s.mux.HandleFunc("/api/setup/complete", s.handleSetupComplete)

	// Public routes (no auth required).
	s.mux.HandleFunc("/login", s.handleLoginPage)
	s.mux.HandleFunc("/api/login", s.handleLogin)
	s.mux.HandleFunc("/api/logout", s.handleLogout)

	// Page routes (auth + init check required).
	s.mux.HandleFunc("/", s.setupWrap(s.authWrap(s.handlePage("dashboard.html"))))
	s.mux.HandleFunc("/seeds", s.setupWrap(s.authWrap(s.handlePage("seeds.html"))))
	s.mux.HandleFunc("/config", s.setupWrap(s.authWrap(s.handlePage("config.html"))))
	s.mux.HandleFunc("/logs", s.setupWrap(s.authWrap(s.handlePage("logs.html"))))
	s.mux.HandleFunc("/preview", s.setupWrap(s.authWrap(s.handlePreviewPage)))

	// API routes (auth + init check required).
	s.mux.HandleFunc("/api/status", s.setupWrap(s.authWrap(s.handleAPIStatus)))
	s.mux.HandleFunc("/api/seeds", s.setupWrap(s.authWrap(s.handleAPISeeds)))
	s.mux.HandleFunc("/api/seeds/", s.setupWrap(s.authWrap(s.handleAPISeedAction)))
	s.mux.HandleFunc("/api/config", s.setupWrap(s.authWrap(s.handleAPIConfig)))
	s.mux.HandleFunc("/api/logs", s.setupWrap(s.authWrap(s.handleAPILogs)))

	// Preview API routes (auth + init check required).
	s.mux.HandleFunc("/api/preview/fetch", s.setupWrap(s.authWrap(s.handlePreviewFetch)))
	s.mux.HandleFunc("/api/preview/seed", s.setupWrap(s.authWrap(s.handlePreviewSeed)))

	// QB proxy — no auth required (qBittorrent has its own authentication).
	// setupWrap allows access during initialization; after init, it passes
	// through normally (no authWrap needed — qB WebUI handles its own auth).
	s.mux.HandleFunc("/qb/", s.setupWrap(s.authWrap(s.handleQBProxy)))
}

// handleQBProxy forwards requests to the current qB proxy instance. When the
// proxy hasn't been configured yet it returns 503.
func (s *Server) handleQBProxy(w http.ResponseWriter, r *http.Request) {
	s.qbProxyMu.RLock()
	proxy := s.qbProxy
	s.qbProxyMu.RUnlock()
	if proxy == nil {
		http.Error(w, "QB proxy: not configured yet", http.StatusServiceUnavailable)
		return
	}
	proxy.ServeHTTP(w, r)
}

// setupWrap redirects uninitialized clients to /setup, or returns 503 JSON
// for API requests. After initialization is complete, the request passes
// through to the next handler.
// /qb/ proxy paths are allowed through during setup so the user can interact
// with qB WebUI while configuring.
func (s *Server) setupWrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.initialized.Load() {
			// Allow /qb/ proxy access during setup wizard.
			if strings.HasPrefix(r.URL.Path, "/qb/") {
				next(w, r)
				return
			}
			if isAPIRequest(r) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusServiceUnavailable)
				json.NewEncoder(w).Encode(map[string]any{
					"error":          "系统未初始化，请先完成设置向导",
					"setup_required": true,
				})
				return
			}
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}
		next(w, r)
	}
}

// handleSetupPage renders the standalone setup wizard page (no auth/login
// required — first-run workflow pre-dates any password).
func (s *Server) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	// If already initialized, redirect to dashboard.
	if s.initialized.Load() {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	s.tmplMu.RLock()
	tmpl := s.setupTmpl
	s.tmplMu.RUnlock()

	if tmpl == nil {
		// Fallback: serve a minimal HTML page if the template isn't embedded.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`<!DOCTYPE html>
<html lang="zh-CN">
<head><meta charset="UTF-8"><title>AutoSeedRelay Setup</title></head>
<body>
<h1>AutoSeedRelay Setup</h1>
<p>设置向导模板未加载。请确认 templates/setup.html 已内嵌到二进制文件中。</p>
<p>你可以通过 API 手动完成配置：</p>
<ol>
<li>POST /api/setup/qb</li>
<li>POST /api/setup/source</li>
<li>POST /api/setup/targets</li>
<li>POST /api/setup/complete</li>
</ol>
</body>
</html>`))
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, nil); err != nil {
		slog.Error("setup template render failed", "error", err)
		http.Error(w, "template render error", http.StatusInternalServerError)
	}
}

// StartServer is the convenience entry point: creates and starts the server.
func StartServer(addr string, engine EngineInterface, cfg *config.AppConfig) error {
	srv := NewServer(addr, engine, cfg)
	return srv.Start()
}

func (s *Server) handlePage(templateName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.tmplMu.RLock()
		base := s.baseTmpl
		s.tmplMu.RUnlock()

		if base == nil {
			http.Error(w, "templates not initialized", http.StatusInternalServerError)
			return
		}

		// Clone the base layout template and add the page-specific template.
		tmpl, err := base.Clone()
		if err != nil {
			slog.Error("template clone failed", "error", err)
			http.Error(w, "template error", http.StatusInternalServerError)
			return
		}

		pageContent, err := fs.ReadFile(templatesFS, "templates/"+templateName)
		if err != nil {
			http.Error(w, "page not found", http.StatusNotFound)
			return
		}

		if _, err := tmpl.Parse(string(pageContent)); err != nil {
			slog.Error("template parse failed", "template", templateName, "error", err)
			http.Error(w, "template parse error", http.StatusInternalServerError)
			return
		}

		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, "layout", nil); err != nil {
			slog.Error("template render failed", "template", templateName, "error", err)
			http.Error(w, "template render error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.Copy(w, &buf)
	}
}

// handlePreviewPage renders the preview page from the pre-parsed template.
// Falls back to the generic handlePage if previewTmpl wasn't parsed.
func (s *Server) handlePreviewPage(w http.ResponseWriter, r *http.Request) {
	s.tmplMu.RLock()
	tmpl := s.previewTmpl
	s.tmplMu.RUnlock()

	if tmpl == nil {
		// Fallback: try on-the-fly parsing.
		s.handlePage("preview.html")(w, r)
		return
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "layout", nil); err != nil {
		slog.Error("preview template render failed", "error", err)
		http.Error(w, "template render error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.Copy(w, &buf)
}

// Start begins listening.
func (s *Server) Start() error {
	s.srv = &http.Server{
		Addr:         s.addr,
		Handler:      s.mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	s.started.Store(true)
	slog.Info("web server starting", "addr", s.addr)
	return s.srv.ListenAndServe()
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown() error {
	s.started.Store(false)
	if s.srv != nil {
		return s.srv.Close()
	}
	return nil
}

// addLog appends a log entry to the in-memory buffer.
func (s *Server) AddLog(level, message string) {
	s.logBufMu.Lock()
	defer s.logBufMu.Unlock()

	entry := LogEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Level:     level,
		Message:   message,
	}

	s.logBuf = append(s.logBuf, entry)
	if len(s.logBuf) > s.logBufMax {
		s.logBuf = s.logBuf[len(s.logBuf)-s.logBufMax:]
	}
}

// ---------------------------------------------------------------------------
// API handlers
// ---------------------------------------------------------------------------

func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	stats := s.engine.GetStats()
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleAPISeeds(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	filter := SeedFilter{
		Status: q.Get("status"),
		Target: q.Get("target"),
		Query:  q.Get("q"),
		Page:   parseIntParam(q.Get("page"), 1),
		Limit:  parseIntParam(q.Get("limit"), 50),
	}

	seeds, total, err := s.engine.GetSeeds(filter)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": err.Error(),
		})
		return
	}

	if seeds == nil {
		seeds = []SeedInfo{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"seeds": seeds,
		"total": total,
		"page":  filter.Page,
		"limit": filter.Limit,
	})
}

func (s *Server) handleAPISeedAction(w http.ResponseWriter, r *http.Request) {
	// Parse /api/seeds/{id}/{action}
	path := strings.TrimPrefix(r.URL.Path, "/api/seeds/")
	parts := strings.Split(path, "/")

	if len(parts) == 1 && r.Method == http.MethodGet {
		// GET /api/seeds/:id
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid seed id"})
			return
		}
		detail, err := s.engine.GetSeedDetail(id)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, detail)
		return
	}

	if len(parts) == 2 && r.Method == http.MethodPost {
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid seed id"})
			return
		}

		switch parts[1] {
		case "retire":
			if err := s.engine.RetireSeed(id); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"status": "retired"})
			return
		case "retry":
			if err := s.engine.RetrySeed(id); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"status": "retrying"})
			return
		}
	}

	writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
}

func (s *Server) handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.engine.GetConfig()
		writeJSON(w, http.StatusOK, cfg)
	case http.MethodPost:
		var cfg config.AppConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid config JSON: " + err.Error()})
			return
		}
		if err := s.engine.SaveConfig(&cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "saved"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleAPILogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	level := q.Get("level")
	n := parseIntParam(q.Get("n"), 100)

	s.logBufMu.Lock()
	defer s.logBufMu.Unlock()

	var filtered []LogEntry
	// Iterate from the end for most recent.
	start := len(s.logBuf) - n
	if start < 0 {
		start = 0
	}
	for i := len(s.logBuf) - 1; i >= start; i-- {
		entry := s.logBuf[i]
		if level != "" && !strings.EqualFold(entry.Level, level) {
			continue
		}
		filtered = append(filtered, entry)
		if len(filtered) >= n {
			break
		}
	}

	if filtered == nil {
		filtered = []LogEntry{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"logs": filtered,
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("write json failed", "error", err)
	}
}

func parseIntParam(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	if n < 1 {
		return def
	}
	return n
}
