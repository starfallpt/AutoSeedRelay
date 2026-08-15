// Package server assembles the Gin engine: middleware chain skeleton, route
// registration, and request handlers. M0 ships only health + login placeholder.
package server

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/autoseedrelay/relay/internal/auth"
	"github.com/autoseedrelay/relay/internal/config"
	"github.com/autoseedrelay/relay/internal/engine"
	"github.com/autoseedrelay/relay/internal/store"
	"github.com/autoseedrelay/relay/internal/webfs"
	"github.com/gin-gonic/gin"
)

// Version is the backend release version for the M0 milestone.
const Version = "0.1.0-m0"

// Deps carries the runtime dependencies the health endpoint reports on. Every
// field is optional (nil-safe), so M0-only wiring still works. RegisterAPI (if
// set) mounts the full v2 API surface (config + ops domains) on the /api/v2
// group; it is a closure injected by the wiring layer (cmd/relay) so the
// server package stays decoupled from the api package.
type Deps struct {
	Store       *store.Store
	Engine      *engine.Engine
	Auth        *auth.Manager
	RegisterAPI func(rg *gin.RouterGroup)
}

// Server holds the wired Gin engine and deployment config.
type Server struct {
	cfg    *config.Config
	logger *slog.Logger
	engine *gin.Engine
	deps   Deps
}

// New builds the Gin engine with the middleware chain and routes.
func New(cfg *config.Config, logger *slog.Logger, deps Deps) (*Server, error) {
	if cfg == nil {
		cfg = config.Default()
	}
	if logger == nil {
		logger = slog.Default()
	}

	engine := gin.New()
	// Middleware chain (see docs/ARCHITECTURE-v4.md §10): Recovery → RequestID → slog.
	engine.Use(Recovery(logger), RequestID(), SlogLogger(logger))
	if deps.Auth != nil {
		engine.Use(deps.Auth.Middleware())
	}

	s := &Server{cfg: cfg, logger: logger, engine: engine, deps: deps}
	s.routes()
	return s, nil
}

// Run starts the HTTP server and blocks until ctx is cancelled, at which point
// it gracefully shuts down: it stops accepting new connections, drains in-flight
// requests for up to 10s, and returns nil once the listener (and thus the port)
// is released. It uses an explicit http.Server with read/write/idle/header
// timeouts (replacing the bare ListenAndServe) and disables proxy-header trust
// so X-Forwarded-For cannot spoof client IPs.
func (s *Server) Run(ctx context.Context) error {
	_ = s.engine.SetTrustedProxies(nil)
	srv := &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           s.engine,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Bind synchronously so Run returns a bind error immediately and so a
	// shutdown always has a listener to close (no bind/serve race).
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return err
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		// The server exited on its own (Serve failed).
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		// Graceful shutdown: stop accepting new conns and drain in-flight
		// requests, bounded by a 10s timeout.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		// Drain the Serve goroutine so the listener is fully closed (port
		// released) before Run returns.
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// Handler exposes the engine for embedding in tests or a supervisor.
func (s *Server) Handler() http.Handler {
	return s.engine
}

func (s *Server) routes() {
	api := s.engine.Group("/api/v2")
	{
		api.GET("/health", s.handleHealth)
		api.POST("/auth/login", s.handleLogin)
		api.POST("/auth/logout", s.handleLogout)
		api.GET("/auth/me", s.handleMe)
		api.GET("/setup/status", s.handleSetupStatus)
		api.POST("/setup/complete", s.handleSetupComplete)
		if s.deps.RegisterAPI != nil {
			s.deps.RegisterAPI(api)
		}
	}

	webfs.Register(s.engine)
	// All other routes fall through to Gin's default 404.
}

func (s *Server) handleHealth(c *gin.Context) {
	resp := gin.H{
		"status":  "ok",
		"version": Version,
		"time":    time.Now().UTC().Format(time.RFC3339),
	}
	if s.deps.Store != nil {
		if v, err := s.deps.Store.MigrateVersion(); err == nil {
			resp["db_version"] = v
		}
	}
	if s.deps.Engine != nil {
		resp["engine"] = s.deps.Engine.Status()
	}
	c.JSON(http.StatusOK, resp)
}

// handleLogin verifies the web password against the stored bcrypt hash and, on
// success, issues a session cookie and a CSRF token. It is rate-limited per IP
// (see auth.Manager.AllowLogin). When deps.Auth is nil (auth not yet wired) it
// returns 503 so the server can boot without auth during gradual wiring.
func (s *Server) handleLogin(c *gin.Context) {
	if s.deps.Auth == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"ok": false, "error": "auth not configured"})
		return
	}
	a := s.deps.Auth

	if allowed, retry := a.AllowLogin(c.ClientIP()); !allowed {
		c.Header("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		c.JSON(http.StatusTooManyRequests, gin.H{"ok": false, "error": "too many attempts"})
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "invalid request body"})
		return
	}

	if !a.SetupState() {
		c.JSON(http.StatusForbidden, gin.H{"ok": false, "error": "setup required"})
		return
	}

	ok, err := a.Login(c.Request.Context(), req.Password)
	if err != nil {
		s.logger.Error("login", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false})
		return
	}
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"ok": false})
		return
	}

	a.StartSession(c)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleLogout clears the session and CSRF cookies. Auth + CSRF are enforced by
// the middleware before this handler runs.
func (s *Server) handleLogout(c *gin.Context) {
	if s.deps.Auth == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"ok": false, "error": "auth not configured"})
		return
	}
	s.deps.Auth.EndSession(c)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleMe reports the current session is valid (the middleware already checked
// it) and refreshes the CSRF token.
func (s *Server) handleMe(c *gin.Context) {
	if s.deps.Auth == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"ok": false, "error": "auth not configured"})
		return
	}
	s.deps.Auth.IssueCSRF(c)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleSetupStatus reports whether the web password has been initialized.
func (s *Server) handleSetupStatus(c *gin.Context) {
	if s.deps.Auth == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"ok": false, "error": "auth not configured"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"initialized": s.deps.Auth.SetupState()})
}

// handleSetupComplete sets the initial web password. It only works while the
// system is uninitialized; afterwards it returns 403.
func (s *Server) handleSetupComplete(c *gin.Context) {
	if s.deps.Auth == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"ok": false, "error": "auth not configured"})
		return
	}
	if s.deps.Auth.SetupState() {
		c.JSON(http.StatusForbidden, gin.H{"ok": false, "error": "already initialized"})
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "invalid request body"})
		return
	}
	if err := s.deps.Auth.CompleteSetup(req.Password); err != nil {
		switch {
		case errors.Is(err, auth.ErrAlreadyInitialized):
			c.JSON(http.StatusForbidden, gin.H{"ok": false, "error": "already initialized"})
		case errors.Is(err, auth.ErrEmptyPassword):
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "password required"})
		default:
			s.logger.Error("setup complete", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false})
		}
		return
	}
	// Setup success immediately issues a session + CSRF token so the client can
	// enter the dashboard without a second login.
	s.deps.Auth.StartSession(c)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// --- middleware ---

// Recovery recovers from panics and logs them with the structured logger.
func Recovery(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic recovered",
					"error", r,
					"method", c.Request.Method,
					"path", c.Request.URL.Path,
				)
				c.AbortWithStatus(http.StatusInternalServerError)
			}
		}()
		c.Next()
	}
}

// RequestID assigns a per-request ID (from X-Request-ID or generated).
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if id == "" {
			id = newRequestID()
		}
		c.Set("request_id", id)
		c.Header("X-Request-ID", id)
		c.Next()
	}
}

// SlogLogger logs one structured line per request.
func SlogLogger(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		c.Next()

		status := c.Writer.Status()
		attrs := []any{
			"method", c.Request.Method,
			"path", path,
			"status", status,
			"latency", time.Since(start).String(),
			"client_ip", c.ClientIP(),
		}
		if id, ok := c.Get("request_id"); ok {
			attrs = append(attrs, "request_id", id)
		}
		if len(c.Errors) > 0 {
			attrs = append(attrs, "errors", c.Errors.String())
		}

		level := slog.LevelInfo
		switch {
		case status >= 500:
			level = slog.LevelError
		case status >= 400:
			level = slog.LevelWarn
		}
		logger.Log(c.Request.Context(), level, "http_request", attrs...)
	}
}

func newRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b)
}
