package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/autoseedrelay/relay/internal/qb"
	"github.com/autoseedrelay/relay/internal/store"
	"github.com/gin-gonic/gin"
)

// qbDTO is the wire representation of a qB instance. The password is masked on
// read; on write it accepts plaintext or "***" (= keep).
type qbDTO struct {
	ID       int64           `json:"id"`
	Name     string          `json:"name"`
	Host     string          `json:"host"`
	Port     int64           `json:"port"`
	Username string          `json:"username"`
	Password string          `json:"password"`
	Priority int64           `json:"priority"`
	Enabled  bool            `json:"enabled"`
	Extra    json.RawMessage `json:"extra"`
}

// qbInput is the create/update payload. Enabled is a pointer so an omitted
// value keeps the schema default (enabled); the repo has no GetQBByID, so
// list/get read the table directly and decrypt only when strictly needed.
type qbInput struct {
	Name     string          `json:"name" binding:"required"`
	Host     string          `json:"host" binding:"required"`
	Port     int64           `json:"port"`
	Username string          `json:"username"`
	Password string          `json:"password"`
	Priority int64           `json:"priority"`
	Enabled  *bool           `json:"enabled"`
	Extra    json.RawMessage `json:"extra"`
}

func (in *qbInput) toInstance() *store.QBInstance {
	enabled := int64(1)
	if in.Enabled != nil && !*in.Enabled {
		enabled = 0
	}
	port := in.Port
	if port == 0 {
		port = 8080
	}
	return &store.QBInstance{
		Name:     in.Name,
		Host:     in.Host,
		Port:     port,
		Username: in.Username,
		Password: in.Password,
		Priority: in.Priority,
		Enabled:  enabled,
		Extra:    rawToJSONStringDefault(in.Extra, "{}"),
	}
}

// qbRow is a raw qb_instances row (enc_password stays ciphertext; the repo only
// decrypts through GetEnabledQBInstances, which filters to enabled rows).
type qbRow struct {
	ID          int64
	Name        string
	Host        string
	Port        int64
	Username    string
	EncPassword sql.NullString
	Priority    int64
	Enabled     int64
	LastSeenAt  int64
	Extra       string
}

const qbColumnsRaw = `id, name, host, port, username, enc_password, priority, enabled, last_seen_at, extra`

func scanQBRow(row interface{ Scan(dest ...any) error }) (*qbRow, error) {
	var q qbRow
	var lastSeen sql.NullInt64
	if err := row.Scan(&q.ID, &q.Name, &q.Host, &q.Port, &q.Username, &q.EncPassword,
		&q.Priority, &q.Enabled, &lastSeen, &q.Extra); err != nil {
		return nil, err
	}
	q.LastSeenAt = lastSeen.Int64
	return &q, nil
}

func qbDTOFromRow(q *qbRow) qbDTO {
	return qbDTO{
		ID:       q.ID,
		Name:     q.Name,
		Host:     q.Host,
		Port:     q.Port,
		Username: q.Username,
		Password: maskIf(q.EncPassword),
		Priority: q.Priority,
		Enabled:  q.Enabled != 0,
		Extra:    jsonStringToRaw(q.Extra),
	}
}

func qbDTOFromInstance(q *store.QBInstance) qbDTO {
	return qbDTO{
		ID:       q.ID,
		Name:     q.Name,
		Host:     q.Host,
		Port:     q.Port,
		Username: q.Username,
		Password: mask(q.Password),
		Priority: q.Priority,
		Enabled:  q.Enabled != 0,
		Extra:    jsonStringToRaw(q.Extra),
	}
}

func (h *handler) getQBRow(ctx *gin.Context, id int64) (*qbRow, error) {
	row := h.deps.Repo.DB().QueryRowContext(ctx.Request.Context(),
		`SELECT `+qbColumnsRaw+` FROM qb_instances WHERE id = ?`, id)
	return scanQBRow(row)
}

func (h *handler) listQB(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	rows, err := repo.DB().QueryContext(c.Request.Context(),
		`SELECT `+qbColumnsRaw+` FROM qb_instances ORDER BY priority DESC, id`)
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	out := []qbDTO{}
	for rows.Next() {
		q, err := scanQBRow(rows)
		if err != nil {
			writeError(c, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, qbDTOFromRow(q))
	}
	if err := rows.Err(); err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, out)
}

func (h *handler) createQB(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	var in qbInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	q := in.toInstance()
	if err := repo.UpsertQBInstance(c.Request.Context(), q); err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, qbDTOFromInstance(q))
}

func (h *handler) getQB(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	id, ok := parseID(c)
	if !ok {
		return
	}
	q, err := h.getQBRow(c, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "qb instance not found")
		return
	}
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, qbDTOFromRow(q))
}

func (h *handler) updateQB(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	id, ok := parseID(c)
	if !ok {
		return
	}
	var in qbInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	existing, err := h.getQBRow(c, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "qb instance not found")
		return
	}
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}

	q := in.toInstance()
	q.ID = id
	q.LastSeenAt = existing.LastSeenAt

	if in.Password == "***" {
		// Preserve the encrypted password: a raw UPDATE that leaves enc_password
		// untouched (UpsertQBInstance always re-encrypts, which would wipe a
		// "***" input to NULL).
		_, err = repo.DB().ExecContext(c.Request.Context(), `
			UPDATE qb_instances SET name = ?, host = ?, port = ?, username = ?,
				priority = ?, enabled = ?, extra = ?, updated_at = unixepoch()
			WHERE id = ?`,
			q.Name, q.Host, q.Port, q.Username, q.Priority, q.Enabled, q.Extra, id)
		if err != nil {
			writeError(c, http.StatusInternalServerError, err.Error())
			return
		}
	} else if err := repo.UpsertQBInstance(c.Request.Context(), q); err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}

	updated, err := h.getQBRow(c, id)
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, qbDTOFromRow(updated))
}

func (h *handler) deleteQB(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	id, ok := parseID(c)
	if !ok {
		return
	}
	// The repo has no DeleteQBInstance; use a direct DELETE (small query).
	res, err := repo.DB().ExecContext(c.Request.Context(), `DELETE FROM qb_instances WHERE id = ?`, id)
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(c, http.StatusNotFound, "qb instance not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *handler) testQB(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	id, ok := parseID(c)
	if !ok {
		return
	}

	// The repo decrypts qB passwords only through GetEnabledQBInstances (there
	// is no GetQBByID), so a live login test is possible for enabled instances
	// only; a disabled instance's password cannot be decrypted from this
	// package (the master key is repo-private).
	q, err := h.findQBDecrypted(c, repo, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "qb instance not found")
		return
	}
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	if q == nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "message": "instance disabled: 无法解密密码进行测试"})
		return
	}

	port := ""
	if q.Port > 0 {
		port = strconv.FormatInt(q.Port, 10)
	}
	inst := qb.NewInstance(q.Host, port, q.Username, q.Password)
	if err := inst.Login(c.Request.Context()); err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "message": err.Error()})
		return
	}
	ver, err := inst.Version(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "version": ver})
}

// findQBDecrypted returns the decrypted qB instance for id, or (nil, nil) when
// the instance exists but is disabled (and therefore absent from
// GetEnabledQBInstances). A missing row yields sql.ErrNoRows.
func (h *handler) findQBDecrypted(c *gin.Context, repo *store.Repo, id int64) (*store.QBInstance, error) {
	var n int
	if err := repo.DB().QueryRowContext(c.Request.Context(),
		`SELECT COUNT(*) FROM qb_instances WHERE id = ?`, id).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, sql.ErrNoRows
	}
	list, err := repo.GetEnabledQBInstances(c.Request.Context())
	if err != nil {
		return nil, err
	}
	for _, q := range list {
		if q.ID == id {
			return q, nil
		}
	}
	return nil, nil
}
