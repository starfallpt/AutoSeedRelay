package api

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// logJSON is the v2 wire shape of an activity_log row (shared by the events
// endpoint and the seed-detail "logs" list).
type logJSON struct {
	ID        int64  `json:"id"`
	SeedID    int64  `json:"seed_id"`
	Level     string `json:"level"`
	Action    string `json:"action"`
	Detail    string `json:"detail"`
	CreatedAt int64  `json:"created_at"`
}

// listEvents handles GET /events?since=&level=&seed_id= → {events:[...], latest}.
//
// It returns activity_log rows newer than the `since` id cursor, newest first.
// `level` and `seed_id` optionally narrow the stream. `latest` is the highest id
// in the returned page (or `since` when nothing newer exists) so the client can
// carry it forward as the next `since`.
func (o *Ops) listEvents(c *gin.Context) {
	if !o.requireRepo(c) {
		return
	}
	ctx := c.Request.Context()

	since, _ := strconv.ParseInt(c.DefaultQuery("since", "0"), 10, 64)
	if since < 0 {
		since = 0
	}
	size := 50
	if v, err := strconv.Atoi(c.Query("size")); err == nil && v > 0 && v <= 200 {
		size = v
	}

	conds := []string{"id > ?"}
	args := []any{since}
	if lvl := c.Query("level"); lvl != "" {
		conds = append(conds, "level = ?")
		args = append(args, lvl)
	}
	if sid := c.Query("seed_id"); sid != "" {
		if v, err := strconv.ParseInt(sid, 10, 64); err == nil {
			conds = append(conds, "seed_id = ?")
			args = append(args, v)
		}
	}

	q := "SELECT id, seed_id, level, action, detail, created_at FROM activity_log" +
		" WHERE " + strings.Join(conds, " AND ") + " ORDER BY id DESC LIMIT ?"
	rows, err := o.Repo.DB().QueryContext(ctx, q, append(args, size)...)
	if err != nil {
		opsWriteError(c, http.StatusInternalServerError, "list events: "+err.Error())
		return
	}
	defer rows.Close()

	events := make([]logJSON, 0)
	latest := since
	for rows.Next() {
		var l logJSON
		var sid sql.NullInt64
		if err := rows.Scan(&l.ID, &sid, &l.Level, &l.Action, &l.Detail, &l.CreatedAt); err != nil {
			opsWriteError(c, http.StatusInternalServerError, "scan event: "+err.Error())
			return
		}
		l.SeedID = sid.Int64
		events = append(events, l)
		if l.ID > latest {
			latest = l.ID
		}
	}
	if err := rows.Err(); err != nil {
		opsWriteError(c, http.StatusInternalServerError, "list events: "+err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{"events": events, "latest": latest})
}
