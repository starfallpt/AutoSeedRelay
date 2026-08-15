package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/autoseedrelay/relay/internal/adapters"
	"github.com/autoseedrelay/relay/internal/source"
	"github.com/autoseedrelay/relay/internal/store"
	"github.com/gin-gonic/gin"
)

// targetDTO is the wire representation of a target. Credential fields are
// masked on read; on write they accept plaintext or "***" (= keep). The three
// JSON override columns are passed through as native JSON (the repo stores them
// as raw JSON strings).
type targetDTO struct {
	ID                 int64           `json:"id"`
	Name               string          `json:"name"`
	Type               string          `json:"type"`
	BaseURL            string          `json:"base_url"`
	Announce           string          `json:"announce"`
	Passkey            string          `json:"passkey"`
	Cookie             string          `json:"cookie"`
	APIToken           string          `json:"api_token"`
	TestMode           bool            `json:"test_mode"`
	CategoryOverrides  json.RawMessage `json:"category_overrides"`
	DimensionOverrides json.RawMessage `json:"dimension_overrides"`
	TagsMap            json.RawMessage `json:"tags_map"`
	FallbackCategory   *int            `json:"fallback_category"`
	Status             string          `json:"status"`
}

// targetInput is the create/update payload (id is server-owned).
type targetInput struct {
	Name               string          `json:"name" binding:"required"`
	Type               string          `json:"type"`
	BaseURL            string          `json:"base_url"`
	Announce           string          `json:"announce"`
	Passkey            string          `json:"passkey"`
	Cookie             string          `json:"cookie"`
	APIToken           string          `json:"api_token"`
	TestMode           bool            `json:"test_mode"`
	CategoryOverrides  json.RawMessage `json:"category_overrides"`
	DimensionOverrides json.RawMessage `json:"dimension_overrides"`
	TagsMap            json.RawMessage `json:"tags_map"`
	FallbackCategory   *int            `json:"fallback_category"`
	Status             string          `json:"status"`
}

func validTargetType(t string) bool {
	switch t {
	case "", "nexusphp", "nexusphp_classic", "mteam":
		return true
	}
	return false
}

// targetVersionForType derives the schema's version column from the v2 type.
// The pipeline maps type "nexusphp" → API when version != "classic", and the
// schema CHECK only allows api|classic, so: API/API-family types use "api",
// classic uses "classic".
func targetVersionForType(typ string) string {
	if typ == "nexusphp_classic" {
		return "classic"
	}
	return "api"
}

func (in *targetInput) toTarget() *store.Target {
	typ := in.Type
	if typ == "" {
		typ = "nexusphp"
	}
	status := in.Status
	if status == "" {
		status = "active"
	}
	var fallback string
	if in.FallbackCategory != nil {
		fallback = strconv.Itoa(*in.FallbackCategory)
	}
	return &store.Target{
		Name:               in.Name,
		Type:               typ,
		Version:            targetVersionForType(typ),
		BaseURL:            in.BaseURL,
		AnnounceURL:        in.Announce,
		TestMode:           boolToInt(in.TestMode),
		FallbackCategory:   fallback,
		CategoryOverrides:  rawToJSONStringDefault(in.CategoryOverrides, "{}"),
		DimensionOverrides: rawToJSONStringDefault(in.DimensionOverrides, "{}"),
		TagsMap:            rawToJSONStringDefault(in.TagsMap, "{}"),
		Passkey:            in.Passkey,
		Cookie:             in.Cookie,
		APIToken:           in.APIToken,
		Status:             status,
	}
}

func targetDTOFromStore(t *store.Target) targetDTO {
	var fallback *int
	if s := strings.TrimSpace(t.FallbackCategory); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			fallback = &n
		}
	}
	return targetDTO{
		ID:                 t.ID,
		Name:               t.Name,
		Type:               t.Type,
		BaseURL:            t.BaseURL,
		Announce:           t.AnnounceURL,
		Passkey:            mask(t.Passkey),
		Cookie:             mask(t.Cookie),
		APIToken:           mask(t.APIToken),
		TestMode:           t.TestMode != 0,
		CategoryOverrides:  jsonStringToRaw(t.CategoryOverrides),
		DimensionOverrides: jsonStringToRaw(t.DimensionOverrides),
		TagsMap:            jsonStringToRaw(t.TagsMap),
		FallbackCategory:   fallback,
		Status:             t.Status,
	}
}

func (h *handler) listTargets(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	// List-all is a small raw query: the repo only exposes GetEnabledTargets
	// (active subset) plus GetTargetByID. Credentials are masked directly from
	// the enc_* columns (non-NULL ⇒ "***"), so no decryption is needed here.
	rows, err := repo.DB().QueryContext(c.Request.Context(),
		`SELECT id, name, type, base_url, announce_url, test_mode,
		        fallback_category, category_overrides, dimension_overrides, tags_map,
		        status, enc_cookie, enc_passkey, enc_api_token
		   FROM targets ORDER BY id`)
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	out := []targetDTO{}
	for rows.Next() {
		var d targetDTO
		var fallback, catOver, dimOver, tagsMap string
		var testMode int64
		var encCookie, encPasskey, encAPIToken sql.NullString
		if err := rows.Scan(&d.ID, &d.Name, &d.Type, &d.BaseURL, &d.Announce, &testMode,
			&fallback, &catOver, &dimOver, &tagsMap, &d.Status,
			&encCookie, &encPasskey, &encAPIToken); err != nil {
			writeError(c, http.StatusInternalServerError, err.Error())
			return
		}
		d.TestMode = testMode != 0
		d.FallbackCategory = atoiPtr(fallback)
		d.CategoryOverrides = jsonStringToRaw(catOver)
		d.DimensionOverrides = jsonStringToRaw(dimOver)
		d.TagsMap = jsonStringToRaw(tagsMap)
		d.Cookie = maskIf(encCookie)
		d.Passkey = maskIf(encPasskey)
		d.APIToken = maskIf(encAPIToken)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, out)
}

func (h *handler) createTarget(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	var in targetInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if !validTargetType(in.Type) {
		writeError(c, http.StatusBadRequest, "invalid type")
		return
	}
	t := in.toTarget()
	if err := repo.UpsertTarget(c.Request.Context(), t); err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, targetDTOFromStore(t))
}

func (h *handler) getTarget(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	id, ok := parseID(c)
	if !ok {
		return
	}
	t, err := repo.GetTargetByID(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "target not found")
		return
	}
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, targetDTOFromStore(t))
}

func (h *handler) updateTarget(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	id, ok := parseID(c)
	if !ok {
		return
	}
	var in targetInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if !validTargetType(in.Type) {
		writeError(c, http.StatusBadRequest, "invalid type")
		return
	}
	existing, err := repo.GetTargetByID(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "target not found")
		return
	}
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}

	t := in.toTarget()
	t.ID = existing.ID
	t.Passkey = mergeCredential(existing.Passkey, in.Passkey)
	t.Cookie = mergeCredential(existing.Cookie, in.Cookie)
	t.APIToken = mergeCredential(existing.APIToken, in.APIToken)

	if err := repo.UpsertTarget(c.Request.Context(), t); err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, targetDTOFromStore(t))
}

func (h *handler) deleteTarget(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	id, ok := parseID(c)
	if !ok {
		return
	}
	// The repo has no DeleteTarget; use a direct DELETE (small query).
	res, err := repo.DB().ExecContext(c.Request.Context(), `DELETE FROM targets WHERE id = ?`, id)
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(c, http.StatusNotFound, "target not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *handler) probeTarget(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	id, ok := parseID(c)
	if !ok {
		return
	}
	t, err := repo.GetTargetByID(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "target not found")
		return
	}
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	if t.BaseURL == "" {
		c.JSON(http.StatusOK, gin.H{"ok": false, "message": "base_url 未设置"})
		return
	}
	res, err := adapters.Probe(c.Request.Context(), t.BaseURL, nil)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "message": source.RedactError(err.Error())})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":         true,
		"type":       res.Type,
		"sections":   res.Sections,
		"categories": res.Categories,
		"tags":       res.Tags,
		"codec_list": res.Codecs,
	})
}

func (h *handler) testTarget(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	id, ok := parseID(c)
	if !ok {
		return
	}
	t, err := repo.GetTargetByID(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "target not found")
		return
	}
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	if t.BaseURL == "" {
		c.JSON(http.StatusOK, gin.H{"ok": false, "message": "base_url 未设置"})
		return
	}

	base := strings.TrimRight(t.BaseURL, "/")
	var endpoint string
	switch t.Type {
	case "nexusphp":
		endpoint = base + "/api/v1/sections"
	case "nexusphp_classic":
		endpoint = base + "/upload.php"
	case "mteam":
		endpoint = base + "/"
	default:
		c.JSON(http.StatusOK, gin.H{"ok": false, "message": "未知类型 " + t.Type})
		return
	}

	status, err := probeGet(c.Request.Context(), endpoint)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "message": source.RedactError(err.Error())})
		return
	}
	if status >= http.StatusInternalServerError {
		c.JSON(http.StatusOK, gin.H{"ok": false, "message": "HTTP " + strconv.Itoa(status)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "message": "HTTP " + strconv.Itoa(status)})
}

// atoiPtr parses a numeric string into *int (nil when empty or non-numeric).
func atoiPtr(s string) *int {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return &n
	}
	return nil
}
