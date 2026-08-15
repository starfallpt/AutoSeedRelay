package api

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/autoseedrelay/relay/internal/source"
	"github.com/autoseedrelay/relay/internal/store"
	"github.com/gin-gonic/gin"
)

// sourceDTO is the wire representation of a source. Credential fields are
// always masked on read; on write they accept plaintext or "***" (= keep).
type sourceDTO struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	BaseURL   string `json:"base_url"`
	RSSURL    string `json:"rss_url"`
	Passkey   string `json:"passkey"`
	APIToken  string `json:"api_token"`
	Cookie    string `json:"cookie"`
	Status    string `json:"status"`
	FailCount int64  `json:"fail_count"`
}

// sourceInput is the create/update payload (id and fail_count are server-owned).
type sourceInput struct {
	Name     string `json:"name" binding:"required"`
	Role     string `json:"role"`
	BaseURL  string `json:"base_url"`
	RSSURL   string `json:"rss_url"`
	Passkey  string `json:"passkey"`
	APIToken string `json:"api_token"`
	Cookie   string `json:"cookie"`
	Status   string `json:"status"`
}

func (in *sourceInput) toSource() *store.Source {
	role := in.Role
	if role == "" {
		role = "source"
	}
	status := in.Status
	if status == "" {
		status = "active"
	}
	return &store.Source{
		Name:     in.Name,
		Role:     role,
		BaseURL:  in.BaseURL,
		RSSURL:   in.RSSURL,
		Passkey:  in.Passkey,
		APIToken: in.APIToken,
		Cookie:   in.Cookie,
		Status:   status,
	}
}

func sourceDTOFromStore(s *store.Source) sourceDTO {
	return sourceDTO{
		ID:        s.ID,
		Name:      s.Name,
		Role:      s.Role,
		BaseURL:   s.BaseURL,
		RSSURL:    s.RSSURL,
		Passkey:   mask(s.Passkey),
		APIToken:  mask(s.APIToken),
		Cookie:    mask(s.Cookie),
		Status:    s.Status,
		FailCount: s.FailCount,
	}
}

func (h *handler) listSources(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	// List-all is a small raw query: the repo only exposes GetActiveSources
	// (active subset) plus GetSourceByID. Credentials are masked directly from
	// the enc_* columns (non-NULL ⇒ "***"), so no decryption is needed here.
	rows, err := repo.DB().QueryContext(c.Request.Context(),
		`SELECT id, name, role, base_url, rss_url, status, fail_count,
		        enc_cookie, enc_passkey, enc_api_token
		   FROM sources ORDER BY id`)
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	out := []sourceDTO{}
	for rows.Next() {
		var s sourceDTO
		var encCookie, encPasskey, encAPIToken sql.NullString
		if err := rows.Scan(&s.ID, &s.Name, &s.Role, &s.BaseURL, &s.RSSURL, &s.Status, &s.FailCount,
			&encCookie, &encPasskey, &encAPIToken); err != nil {
			writeError(c, http.StatusInternalServerError, err.Error())
			return
		}
		s.Cookie = maskIf(encCookie)
		s.Passkey = maskIf(encPasskey)
		s.APIToken = maskIf(encAPIToken)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, out)
}

func (h *handler) createSource(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	var in sourceInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	s := in.toSource()
	if err := repo.UpsertSource(c.Request.Context(), s); err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, sourceDTOFromStore(s))
}

func (h *handler) getSource(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	id, ok := parseID(c)
	if !ok {
		return
	}
	s, err := repo.GetSourceByID(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "source not found")
		return
	}
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, sourceDTOFromStore(s))
}

func (h *handler) updateSource(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	id, ok := parseID(c)
	if !ok {
		return
	}
	var in sourceInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	existing, err := repo.GetSourceByID(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "source not found")
		return
	}
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}

	s := in.toSource()
	s.ID = existing.ID
	s.AnnounceURL = existing.AnnounceURL // not exposed by the v2 DTO; preserve
	s.FailCount = existing.FailCount     // server-managed
	s.Passkey = mergeCredential(existing.Passkey, in.Passkey)
	s.APIToken = mergeCredential(existing.APIToken, in.APIToken)
	s.Cookie = mergeCredential(existing.Cookie, in.Cookie)

	if err := repo.UpsertSource(c.Request.Context(), s); err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, sourceDTOFromStore(s))
}

func (h *handler) deleteSource(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	id, ok := parseID(c)
	if !ok {
		return
	}
	// The repo has no DeleteSource; use a direct DELETE (small query).
	res, err := repo.DB().ExecContext(c.Request.Context(), `DELETE FROM sources WHERE id = ?`, id)
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(c, http.StatusNotFound, "source not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *handler) testSource(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	id, ok := parseID(c)
	if !ok {
		return
	}
	s, err := repo.GetSourceByID(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "source not found")
		return
	}
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	if s.RSSURL == "" {
		c.JSON(http.StatusOK, gin.H{"ok": false, "message": "rss_url 未设置"})
		return
	}
	// FetchRSS performs a single-page fetch and applies its own SSRF guard.
	items, err := source.FetchRSS(c.Request.Context(), s.RSSURL, nil)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "message": source.RedactError(err.Error())})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "message": fmt.Sprintf("抓取成功：%d 条", len(items))})
}
