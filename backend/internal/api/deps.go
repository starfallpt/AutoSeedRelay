// Package api implements the config-domain HTTP API (M3): sources, targets,
// qB instances, strategy and notifiers, all under /api/v2. It exposes a single
// entry point — RegisterConfig — that the server assembly wires into a gin
// router group. Every route requires an authenticated, initialized session:
// when Deps.Auth is non-nil its middleware guards the whole group (401 not
// logged in / 403 not initialized); the handlers then deal only with validation
// (400) and data access.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/autoseedrelay/relay/internal/notifier"
	"github.com/autoseedrelay/relay/internal/qb"
	"github.com/autoseedrelay/relay/internal/store"
	"github.com/gin-gonic/gin"
)

// Auth is the minimal authentication surface the config API depends on. The
// full auth.Manager (developed in parallel by another agent) is expected to
// satisfy this interface; until it lands, any type exposing Middleware() works
// (tests use a no-op). The scheduler aligns the concrete type at integration
// time — the config API itself never blocks on the auth package.
type Auth interface {
	Middleware() gin.HandlerFunc
}

// Deps carries the runtime dependencies the config API needs.
//
//	Repo  is the query layer (credentials encrypted/decrypted transparently).
//	Store is the raw handle (used only for the small queries the repo lacks).
//	Auth  guards the whole group when non-nil (see Auth).
//	QB    is the live qB instance manager (kept for the shared contract; the
//	      qB test endpoint builds a fresh client from the stored config).
//	Notif is the notification router (kept for the shared contract; the
//	      notifier test endpoint drives providers directly).
type Deps struct {
	Repo  *store.Repo
	Store *store.Store
	Auth  Auth
	QB    *qb.Manager
	Notif *notifier.Router
}

// handler holds the deps shared by every config route.
type handler struct {
	deps Deps
}

// RegisterConfig registers every config-domain route on rg (already scoped to
// /api/v2). When deps.Auth is non-nil its middleware is applied to the whole
// group.
func RegisterConfig(rg *gin.RouterGroup, deps Deps) {
	if deps.Auth != nil {
		rg.Use(deps.Auth.Middleware())
	}
	h := &handler{deps: deps}

	rg.GET("/sources", h.listSources)
	rg.POST("/sources", h.createSource)
	rg.GET("/sources/:id", h.getSource)
	rg.PUT("/sources/:id", h.updateSource)
	rg.DELETE("/sources/:id", h.deleteSource)
	rg.POST("/sources/:id/test", h.testSource)

	rg.GET("/targets", h.listTargets)
	rg.POST("/targets", h.createTarget)
	rg.GET("/targets/:id", h.getTarget)
	rg.PUT("/targets/:id", h.updateTarget)
	rg.DELETE("/targets/:id", h.deleteTarget)
	rg.POST("/targets/:id/probe", h.probeTarget)
	rg.POST("/targets/:id/test", h.testTarget)

	rg.GET("/qb", h.listQB)
	rg.POST("/qb", h.createQB)
	rg.GET("/qb/:id", h.getQB)
	rg.PUT("/qb/:id", h.updateQB)
	rg.DELETE("/qb/:id", h.deleteQB)
	rg.POST("/qb/:id/test", h.testQB)

	rg.GET("/strategy", h.getStrategy)
	rg.PUT("/strategy", h.putStrategy)

	rg.GET("/notifiers", h.listNotifiers)
	rg.POST("/notifiers", h.createNotifier)
	rg.PUT("/notifiers/:id", h.updateNotifier)
	rg.DELETE("/notifiers/:id", h.deleteNotifier)
	rg.POST("/notifiers/:id/test", h.testNotifier)
	rg.GET("/notifiers/routes", h.getNotifierRoutes)
	rg.PUT("/notifiers/routes", h.putNotifierRoutes)
}

// --- shared helpers ---

// writeError renders the uniform {"error":"..."} error body.
func writeError(c *gin.Context, status int, msg string) {
	c.JSON(status, gin.H{"error": msg})
}

// parseID reads the :id path parameter as a positive int64; on failure it
// writes the 400 response and returns ok=false.
func parseID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

// repoOr500 returns the shared repo, writing a 500 when it is not wired.
func (h *handler) repoOr500(c *gin.Context) *store.Repo {
	if h.deps.Repo == nil {
		writeError(c, http.StatusInternalServerError, "store not wired")
		return nil
	}
	return h.deps.Repo
}

// boolToInt maps a bool to the store's INTEGER 0/1 convention.
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// mask renders a decrypted plaintext credential for read: non-empty → "***",
// empty → "". This mirrors secret.Mask; it is kept local so the list endpoints
// (which only see the encrypted columns via maskIf) share the same shape.
func mask(plain string) string {
	if plain == "" {
		return ""
	}
	return "***"
}

// maskIf renders a credential's presence from its encrypted column: a non-NULL
// (non-empty) ciphertext means a non-empty plaintext, so it is masked "***".
// This avoids decrypting on list endpoints (the repo's decrypt is unexported).
func maskIf(ns sql.NullString) string {
	if ns.Valid && ns.String != "" {
		return "***"
	}
	return ""
}

// mergeCredential applies the "***" = keep semantics on update.
func mergeCredential(existing, incoming string) string {
	if incoming == "***" {
		return existing
	}
	return incoming
}

// rawToJSONString converts an inbound json.RawMessage (object/array/scalar or
// JSON null) into the raw JSON string the repo stores. Absent or null becomes
// "" so the caller can substitute the column default.
func rawToJSONString(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	return s
}

// rawToJSONStringDefault is rawToJSONString with an explicit default for the
// absent/null case (JSON columns carry a schema default like "{}" or "[]").
func rawToJSONStringDefault(raw json.RawMessage, def string) string {
	if s := rawToJSONString(raw); s != "" {
		return s
	}
	return def
}

// jsonStringToRaw converts a stored JSON string back into a json.RawMessage so
// responses carry native JSON rather than a double-encoded string. An empty
// stored value is emitted as JSON null.
func jsonStringToRaw(s string) json.RawMessage {
	s = strings.TrimSpace(s)
	if s == "" {
		return json.RawMessage("null")
	}
	return json.RawMessage(s)
}

// apiHTTPClient is the shared outbound client for connectivity probes (target
// test, telegram getMe). 10s timeout; redirects are followed.
var apiHTTPClient = &http.Client{Timeout: 10 * time.Second}

// probeGet issues a GET and returns the response status code, draining a small
// prefix of the body. Used for the target connectivity checks.
func probeGet(ctx context.Context, rawURL string) (int, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return 0, fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := apiHTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return resp.StatusCode, nil
}
