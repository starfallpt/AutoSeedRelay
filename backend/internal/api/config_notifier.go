package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/autoseedrelay/relay/internal/notifier"
	"github.com/autoseedrelay/relay/internal/source"
	"github.com/autoseedrelay/relay/internal/store"
	"github.com/gin-gonic/gin"
)

// notifierDTO is the wire representation of a notifier instance. Config is the
// native JSON config object (the repo stores it as an encrypted JSON string;
// this layer converts between the two so the UI edits a plain object).
type notifierDTO struct {
	ID      int64           `json:"id"`
	Name    string          `json:"name"`
	Type    string          `json:"type"`
	Config  json.RawMessage `json:"config"`
	Enabled bool            `json:"enabled"`
}

// notifierInput is the create/update payload.
type notifierInput struct {
	Name    string          `json:"name" binding:"required"`
	Type    string          `json:"type" binding:"required"`
	Config  json.RawMessage `json:"config"`
	Enabled *bool           `json:"enabled"`
}

func validNotifierType(t string) bool {
	switch notifier.ProviderType(t) {
	case notifier.TypeWebhook, notifier.TypeTelegram, notifier.TypeSMTP,
		notifier.TypeNtfy, notifier.TypeGotify, notifier.TypeServerChan, notifier.TypePushPlus:
		return true
	}
	return false
}

func (in *notifierInput) toInstance() *store.NotifierInstance {
	enabled := int64(0)
	if in.Enabled != nil && *in.Enabled {
		enabled = 1
	}
	return &store.NotifierInstance{
		Name:    in.Name,
		Type:    in.Type,
		Config:  rawToJSONStringDefault(in.Config, "{}"),
		Enabled: enabled,
	}
}

func notifierDTOFromStore(n *store.NotifierInstance) notifierDTO {
	return notifierDTO{
		ID:      n.ID,
		Name:    n.Name,
		Type:    n.Type,
		Config:  jsonStringToRaw(n.Config),
		Enabled: n.Enabled != 0,
	}
}

func (h *handler) listNotifiers(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	list, err := repo.GetNotifierInstances(c.Request.Context(), false)
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	out := []notifierDTO{}
	for _, n := range list {
		out = append(out, notifierDTOFromStore(n))
	}
	c.JSON(http.StatusOK, out)
}

func (h *handler) createNotifier(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	var in notifierInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if !validNotifierType(in.Type) {
		writeError(c, http.StatusBadRequest, "invalid type")
		return
	}
	if len(in.Config) > 0 && !json.Valid(in.Config) {
		writeError(c, http.StatusBadRequest, "config 必须为合法 JSON")
		return
	}
	n := in.toInstance()
	if err := repo.UpsertNotifierInstance(c.Request.Context(), n); err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, notifierDTOFromStore(n))
}

func (h *handler) updateNotifier(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	id, ok := parseID(c)
	if !ok {
		return
	}
	var in notifierInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if !validNotifierType(in.Type) {
		writeError(c, http.StatusBadRequest, "invalid type")
		return
	}
	if len(in.Config) > 0 && !json.Valid(in.Config) {
		writeError(c, http.StatusBadRequest, "config 必须为合法 JSON")
		return
	}
	// The repo has no GetNotifierByID; confirm existence with a tiny raw query.
	var n int
	if err := repo.DB().QueryRowContext(c.Request.Context(),
		`SELECT COUNT(*) FROM notifier_instances WHERE id = ?`, id).Scan(&n); err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	if n == 0 {
		writeError(c, http.StatusNotFound, "notifier not found")
		return
	}
	inst := in.toInstance()
	inst.ID = id
	if err := repo.UpsertNotifierInstance(c.Request.Context(), inst); err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, notifierDTOFromStore(inst))
}

func (h *handler) deleteNotifier(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	id, ok := parseID(c)
	if !ok {
		return
	}
	// The repo has no DeleteNotifierInstance; use a direct DELETE (small query).
	res, err := repo.DB().ExecContext(c.Request.Context(), `DELETE FROM notifier_instances WHERE id = ?`, id)
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(c, http.StatusNotFound, "notifier not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *handler) testNotifier(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	id, ok := parseID(c)
	if !ok {
		return
	}
	n, err := h.notifierByID(c.Request.Context(), repo, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "notifier not found")
		return
	}
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}

	var cfg notifier.Config
	if err := json.Unmarshal([]byte(n.Config), &cfg); err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "message": source.RedactError("config 解析失败: " + err.Error())})
		return
	}
	cfg.Type = notifier.ProviderType(n.Type)

	// notifier.New validates config completeness for every provider (including
	// smtp, whose "test" is config-only and performs no network I/O).
	if _, err := notifier.New(cfg); err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "message": source.RedactError(err.Error())})
		return
	}

	switch cfg.Type {
	case notifier.TypeSMTP:
		c.JSON(http.StatusOK, gin.H{"ok": true, "message": "config 完整"})
		return
	case notifier.TypeTelegram:
		if err := telegramGetMe(c.Request.Context(), cfg); err != nil {
			c.JSON(http.StatusOK, gin.H{"ok": false, "message": source.RedactError(err.Error())})
			return
		}
	default:
		// webhook / ntfy / gotify / serverchan / pushplus: the provider's Send
		// performs the minimal authenticated request with a test payload.
		prov, err := notifier.New(cfg)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"ok": false, "message": source.RedactError(err.Error())})
			return
		}
		if err := prov.Send(c.Request.Context(), notifier.Message{
			Title: "AutoSeedRelay 测试",
			Body:  "连接正常",
			Level: notifier.LevelInfo,
		}); err != nil {
			c.JSON(http.StatusOK, gin.H{"ok": false, "message": source.RedactError(err.Error())})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "message": "ok"})
}

// telegramGetMe performs the minimal telegram auth check (getMe).
func telegramGetMe(ctx context.Context, cfg notifier.Config) error {
	base := strings.TrimRight(cfg.TelegramBaseURL, "/")
	if base == "" {
		base = "https://api.telegram.org"
	}
	target := base + "/bot" + cfg.TelegramToken + "/getMe"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := apiHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram getMe 失败: HTTP %d", resp.StatusCode)
	}
	var r struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal(body, &r) == nil && !r.OK {
		return fmt.Errorf("telegram getMe 失败: not ok")
	}
	return nil
}

// notifierByID returns the decrypted notifier instance for id (the repo only
// exposes a list + decrypt path, so this lists and filters).
func (h *handler) notifierByID(ctx context.Context, repo *store.Repo, id int64) (*store.NotifierInstance, error) {
	list, err := repo.GetNotifierInstances(ctx, false)
	if err != nil {
		return nil, err
	}
	for _, n := range list {
		if n.ID == id {
			return n, nil
		}
	}
	return nil, sql.ErrNoRows
}

// routeDTO is one enabled (instance_id, tier) cell of the routing matrix.
type routeDTO struct {
	InstanceID int64  `json:"instance_id"`
	Tier       string `json:"tier"`
}

func (h *handler) getNotifierRoutes(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	out := []routeDTO{}
	for _, tier := range []string{"critical", "warning", "info"} {
		routes, err := repo.GetRoutes(c.Request.Context(), tier)
		if err != nil {
			writeError(c, http.StatusInternalServerError, err.Error())
			return
		}
		for _, rt := range routes {
			if rt.Enabled != 0 {
				out = append(out, routeDTO{InstanceID: rt.InstanceID, Tier: rt.Tier})
			}
		}
	}
	c.JSON(http.StatusOK, out)
}

func (h *handler) putNotifierRoutes(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	var in []routeDTO
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request body")
		return
	}

	validTiers := map[string]bool{"critical": true, "warning": true, "info": true}
	insts, err := repo.GetNotifierInstances(c.Request.Context(), false)
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	validIDs := map[int64]bool{}
	for _, n := range insts {
		validIDs[n.ID] = true
	}

	seen := map[string]bool{}
	rows := []routeDTO{}
	for _, rt := range in {
		if !validTiers[rt.Tier] {
			writeError(c, http.StatusBadRequest, "invalid tier")
			return
		}
		if rt.InstanceID <= 0 {
			writeError(c, http.StatusBadRequest, "invalid instance_id")
			return
		}
		if !validIDs[rt.InstanceID] {
			writeError(c, http.StatusBadRequest, "unknown instance_id")
			return
		}
		key := strconv.FormatInt(rt.InstanceID, 10) + "|" + rt.Tier
		if seen[key] {
			continue
		}
		seen[key] = true
		rows = append(rows, rt)
	}

	// Replace the whole matrix in one transaction (the repo has no clear-all).
	tx, err := repo.DB().BeginTx(c.Request.Context(), nil)
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback() // no-op after Commit
	if _, err := tx.ExecContext(c.Request.Context(), `DELETE FROM notifier_routes`); err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	for _, rt := range rows {
		if _, err := tx.ExecContext(c.Request.Context(),
			`INSERT INTO notifier_routes (instance_id, tier, enabled) VALUES (?, ?, 1)
			 ON CONFLICT(instance_id, tier) DO UPDATE SET enabled = 1`,
			rt.InstanceID, rt.Tier); err != nil {
			writeError(c, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, rows)
}
