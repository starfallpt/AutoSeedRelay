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
	"os"
	"time"

	"github.com/autoseedrelay/relay/internal/config"
	"github.com/autoseedrelay/relay/internal/engine"
	"github.com/autoseedrelay/relay/internal/store"
	"github.com/autoseedrelay/relay/internal/webfs"
	"github.com/gin-gonic/gin"
)

// Version is the backend release version for the M0 milestone.
const Version = "0.1.0-m0"

// Deps carries the runtime dependencies the health endpoint reports on. Every
// field is optional (nil-safe), so M0-only wiring still works.
type Deps struct {
	Store  *store.Store
	Engine *engine.Engine
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

// handleLogin is a placeholder for M0. It accepts {"password": "..."} and
// compares it against the AUTOSEED_WEB_PASSWORD environment variable.
//
// TODO(M1): replace plaintext compare with bcrypt against a DB-stored hash and
// issue a session cookie (see docs/ARCHITECTURE-v4.md §5/§10).
func (s *Server) handleLogin(c *gin.Context) {
	var req struct {
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "invalid request body"})
		return
	}

	expected := os.Getenv("AUTOSEED_WEB_PASSWORD")
	if expected == "" {
		s.logger.Warn("login rejected: AUTOSEED_WEB_PASSWORD is not set")
		c.JSON(http.StatusServiceUnavailable, gin.H{"ok": false, "error": "password not configured"})
		return
	}

	if req.Password != expected {
		c.JSON(http.StatusUnauthorized, gin.H{"ok": false})
		return
	}
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
