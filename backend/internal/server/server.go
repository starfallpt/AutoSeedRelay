// Package server assembles the Gin engine: middleware chain skeleton, route
// registration, and request handlers. M0 ships only health + login placeholder.
package server

import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/autoseedrelay/relay/internal/config"
	"github.com/autoseedrelay/relay/internal/webfs"
	"github.com/gin-gonic/gin"
)

// Version is the backend release version for the M0 milestone.
const Version = "0.1.0-m0"

// Server holds the wired Gin engine and deployment config.
type Server struct {
	cfg    *config.Config
	logger *slog.Logger
	engine *gin.Engine
}

// New builds the Gin engine with the middleware chain and routes.
func New(cfg *config.Config, logger *slog.Logger) (*Server, error) {
	if cfg == nil {
		cfg = config.Default()
	}
	if logger == nil {
		logger = slog.Default()
	}

	engine := gin.New()
	// Middleware chain (see docs/ARCHITECTURE-v4.md §10): Recovery → RequestID → slog.
	engine.Use(Recovery(logger), RequestID(), SlogLogger(logger))

	s := &Server{cfg: cfg, logger: logger, engine: engine}
	s.routes()
	return s, nil
}

// Run starts the HTTP server and blocks until it stops.
func (s *Server) Run() error {
	return s.engine.Run(s.cfg.ListenAddr)
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
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"version": Version,
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
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
