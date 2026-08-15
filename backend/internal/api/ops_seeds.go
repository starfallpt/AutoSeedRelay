package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/autoseedrelay/relay/internal/qb"
	"github.com/autoseedrelay/relay/internal/source"
	"github.com/autoseedrelay/relay/internal/store"
	"github.com/gin-gonic/gin"
)

// Ops bundles the dependencies for the operations-domain v2 handlers
// (seeds / events / dashboard / backup). It is deliberately self-contained so
// the ops handlers compile and test independently of the config domain's wiring
// (deps.go); the wiring layer maps its Deps onto these fields.
type Ops struct {
	Repo    *store.Repo
	Engine  Engine    // nil-safe
	QB      QBManager // nil-safe
	DataDir string    // where restore-pending.zip is written (default: dir of store.DefaultDBPath)
}

// Engine is the minimal engine seam the ops handlers need. *engine.Engine
// satisfies it once the engine domain adds ResendSeed (contract:
//
//	func (e *Engine) ResendSeed(ctx context.Context, seedID int64, fullRerun bool) error
//
// ); Status is already provided by engine.Engine.
type Engine interface {
	ResendSeed(ctx context.Context, seedID int64, fullRerun bool) error
	Status() map[string]any
}

// QBManager is the minimal qB manager seam (satisfied by *qb.Manager).
type QBManager interface {
	AllHealthy(ctx context.Context) ([]qb.Status, error)
	Names() []string
	Get(name string) (*qb.Instance, bool)
}

// Register mounts the operations-domain v2 routes on rg (the "/api/v2" group).
func (o *Ops) Register(rg *gin.RouterGroup) {
	rg.GET("/seeds", o.listSeeds)
	rg.GET("/seeds/:id", o.getSeed)
	rg.POST("/seeds/:id/resend", o.resendSeed)
	rg.DELETE("/seeds/:id", o.deleteSeed)
	rg.GET("/events", o.listEvents)
	rg.GET("/dashboard", o.dashboard)
	rg.GET("/backup/export", o.exportBackup)
	rg.POST("/backup/restore", o.restoreBackup)
}

// requireRepo writes a 500 and returns false when the store is not wired.
func (o *Ops) requireRepo(c *gin.Context) bool {
	if o.Repo == nil {
		opsWriteError(c, http.StatusInternalServerError, "store not available")
		return false
	}
	return true
}

// opsWriteError writes the shared {error} body (see deps.go) after redacting any
// embedded URLs. The config-domain writeError is left unredacted; ops handlers
// route dynamic error strings through here so credentials never leak.
func opsWriteError(c *gin.Context, status int, msg string) {
	writeError(c, status, source.RedactError(msg))
}

// seedJSON is the v2 wire shape of a seeds row. Note the schema column is
// discovered_at, exposed here as created_at per the v2 contract.
type seedJSON struct {
	ID         int64  `json:"id"`
	SourceSite string `json:"source_site"`
	Title      string `json:"title"`
	InfoHash   string `json:"info_hash"`
	Status     string `json:"status"`
	Promotion  string `json:"promotion"`
	Size       int64  `json:"size"`
	RetryCount int64  `json:"retry_count"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
	Error      string `json:"error"`
}

func toSeedJSON(sd *store.Seed) seedJSON {
	title := sd.Title
	if title == "" {
		// The v2 contract falls back to a source_site+info_hash display when no
		// title is present.
		title = sd.SourceSite + "+" + sd.InfoHash
	}
	return seedJSON{
		ID:         sd.ID,
		SourceSite: sd.SourceSite,
		Title:      title,
		InfoHash:   sd.InfoHash,
		Status:     sd.Status,
		Promotion:  sd.Promotion,
		Size:       sd.Size,
		RetryCount: sd.RetryCount,
		CreatedAt:  sd.DiscoveredAt,
		UpdatedAt:  sd.UpdatedAt,
		Error:      source.RedactError(sd.Error),
	}
}

// recordJSON is the v2 wire shape of a relay_records row.
type recordJSON struct {
	ID              int64  `json:"id"`
	SeedID          int64  `json:"seed_id"`
	TargetID        int64  `json:"target_id"`
	Role            string `json:"role"`
	Status          string `json:"status"`
	TargetTorrentID string `json:"target_torrent_id"`
	Attempts        int64  `json:"attempts"`
	LastError       string `json:"last_error"`
	PublishedAt     int64  `json:"published_at"`
	RetiredAt       int64  `json:"retired_at"`
	RetireReason    string `json:"retire_reason"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
}

func toRecordJSON(r *store.RelayRecord) recordJSON {
	return recordJSON{
		ID:              r.ID,
		SeedID:          r.SeedID,
		TargetID:        r.TargetID,
		Role:            r.Role,
		Status:          r.Status,
		TargetTorrentID: r.TargetTorrentID,
		Attempts:        r.Attempts,
		LastError:       source.RedactError(r.LastError),
		PublishedAt:     r.PublishedAt,
		RetiredAt:       r.RetiredAt,
		RetireReason:    r.RetireReason,
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
	}
}

// replicaJSON is the v2 wire shape of a seed_replicas row.
type replicaJSON struct {
	ID       int64   `json:"id"`
	SeedID   int64   `json:"seed_id"`
	QBID     int64   `json:"qb_id"`
	InfoHash string  `json:"info_hash"`
	Role     string  `json:"role"`
	Status   string  `json:"status"`
	Progress float64 `json:"progress"`
	AddedAt  int64   `json:"added_at"`
}

func toReplicaJSON(r *store.Replica) replicaJSON {
	return replicaJSON{
		ID:       r.ID,
		SeedID:   r.SeedID,
		QBID:     r.QBID,
		InfoHash: r.InfoHash,
		Role:     r.Role,
		Status:   r.Status,
		Progress: r.Progress,
		AddedAt:  r.AddedAt,
	}
}

// listSeeds handles GET /seeds?status=&page=&size= → {items:[...], total, page, size}.
// The Repo has no combined filter+count+page method, so the query is issued
// directly through Repo.DB() (bound parameters, never string concat).
func (o *Ops) listSeeds(c *gin.Context) {
	if !o.requireRepo(c) {
		return
	}
	ctx := c.Request.Context()
	db := o.Repo.DB()

	status := c.Query("status")
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("size", "20"))
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 20
	}
	if size > 100 {
		size = 100
	}

	where := ""
	var args []any
	if status != "" {
		where = " WHERE status = ?"
		args = append(args, status)
	}

	var total int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM seeds"+where, args...).Scan(&total); err != nil {
		opsWriteError(c, http.StatusInternalServerError, "count seeds: "+err.Error())
		return
	}

	q := "SELECT id, source_site, info_hash, title, size, promotion, status, error, retry_count, discovered_at, updated_at" +
		" FROM seeds" + where + " ORDER BY id DESC LIMIT ? OFFSET ?"
	rows, err := db.QueryContext(ctx, q, append(args, size, (page-1)*size)...)
	if err != nil {
		opsWriteError(c, http.StatusInternalServerError, "list seeds: "+err.Error())
		return
	}
	defer rows.Close()

	seeds := make([]seedJSON, 0)
	for rows.Next() {
		var sd store.Seed
		if err := rows.Scan(&sd.ID, &sd.SourceSite, &sd.InfoHash, &sd.Title, &sd.Size,
			&sd.Promotion, &sd.Status, &sd.Error, &sd.RetryCount, &sd.DiscoveredAt, &sd.UpdatedAt); err != nil {
			opsWriteError(c, http.StatusInternalServerError, "scan seed: "+err.Error())
			return
		}
		seeds = append(seeds, toSeedJSON(&sd))
	}
	if err := rows.Err(); err != nil {
		opsWriteError(c, http.StatusInternalServerError, "list seeds: "+err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{"items": seeds, "total": total, "page": page, "size": size})
}

// getSeed handles GET /seeds/{id} → {seed, records:[], replicas:[], logs:[]}.
func (o *Ops) getSeed(c *gin.Context) {
	if !o.requireRepo(c) {
		return
	}
	id, ok := parseID(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()

	sd, err := o.Repo.GetSeedByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		opsWriteError(c, http.StatusNotFound, "seed not found")
		return
	}
	if err != nil {
		opsWriteError(c, http.StatusInternalServerError, "get seed: "+err.Error())
		return
	}

	records, err := o.Repo.ListRecordsBySeed(ctx, id)
	if err != nil {
		opsWriteError(c, http.StatusInternalServerError, "list records: "+err.Error())
		return
	}
	replicas, err := o.Repo.ListReplicas(ctx, id)
	if err != nil {
		opsWriteError(c, http.StatusInternalServerError, "list replicas: "+err.Error())
		return
	}
	logs, err := o.seedLogs(ctx, id)
	if err != nil {
		opsWriteError(c, http.StatusInternalServerError, "list logs: "+err.Error())
		return
	}

	recs := make([]recordJSON, 0, len(records))
	for _, r := range records {
		recs = append(recs, toRecordJSON(r))
	}
	reps := make([]replicaJSON, 0, len(replicas))
	for _, r := range replicas {
		reps = append(reps, toReplicaJSON(r))
	}

	c.JSON(http.StatusOK, gin.H{
		"seed":     toSeedJSON(sd),
		"records":  recs,
		"replicas": reps,
		"logs":     logs,
	})
}

// seedLogs lists activity_log rows scoped to a seed, newest first. The Repo has
// no seed-scoped log reader (activity.go only exposes AppendLog / AppendLogSeed),
// so this queries the table directly through Repo.DB().
func (o *Ops) seedLogs(ctx context.Context, seedID int64) ([]logJSON, error) {
	rows, err := o.Repo.DB().QueryContext(ctx,
		`SELECT id, seed_id, level, action, detail, created_at
		   FROM activity_log WHERE seed_id = ? ORDER BY id DESC LIMIT 200`, seedID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]logJSON, 0)
	for rows.Next() {
		var l logJSON
		var sid sql.NullInt64
		if err := rows.Scan(&l.ID, &sid, &l.Level, &l.Action, &l.Detail, &l.CreatedAt); err != nil {
			return nil, err
		}
		l.SeedID = sid.Int64
		out = append(out, l)
	}
	return out, rows.Err()
}

// resendSeed handles POST /seeds/{id}/resend {full:false} → {ok}. It delegates
// to engine.ResendSeed; the engine method is added in parallel by the engine
// domain (contract: ResendSeed(ctx, seedID, fullRerun) error).
func (o *Ops) resendSeed(c *gin.Context) {
	if !o.requireRepo(c) {
		return
	}
	id, ok := parseID(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()

	// Resending a missing seed is a client error, not an engine failure.
	if _, err := o.Repo.GetSeedByID(ctx, id); errors.Is(err, sql.ErrNoRows) {
		opsWriteError(c, http.StatusNotFound, "seed not found")
		return
	} else if err != nil {
		opsWriteError(c, http.StatusInternalServerError, "get seed: "+err.Error())
		return
	}

	var req struct {
		Full bool `json:"full"`
	}
	_ = c.ShouldBindJSON(&req) // tolerate an empty/invalid body: full stays false

	if o.Engine == nil {
		opsWriteError(c, http.StatusInternalServerError, "engine not available")
		return
	}
	if err := o.Engine.ResendSeed(ctx, id, req.Full); err != nil {
		opsWriteError(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// deleteSeed handles DELETE /seeds/{id} → {ok}. It refuses (409) while a seed is
// actively downloading or processing, then cascade-deletes records / replicas /
// seed-scoped logs before the seed itself.
func (o *Ops) deleteSeed(c *gin.Context) {
	if !o.requireRepo(c) {
		return
	}
	id, ok := parseID(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()

	sd, err := o.Repo.GetSeedByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		opsWriteError(c, http.StatusNotFound, "seed not found")
		return
	}
	if err != nil {
		opsWriteError(c, http.StatusInternalServerError, "get seed: "+err.Error())
		return
	}
	if sd.Status == "downloading" || sd.Status == "processing" {
		c.JSON(http.StatusConflict, gin.H{"error": "seed is currently " + sd.Status + "; wait for it to settle"})
		return
	}

	db := o.Repo.DB()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		opsWriteError(c, http.StatusInternalServerError, "begin delete: "+err.Error())
		return
	}
	defer tx.Rollback() // no-op after Commit

	// relay_records and seed_replicas also carry ON DELETE CASCADE on seed_id,
	// but the explicit deletes make the intent clear and cover activity_log
	// (whose seed_id has no foreign key).
	for _, q := range []string{
		`DELETE FROM relay_records WHERE seed_id = ?`,
		`DELETE FROM seed_replicas WHERE seed_id = ?`,
		`DELETE FROM activity_log WHERE seed_id = ?`,
		`DELETE FROM seeds WHERE id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			opsWriteError(c, http.StatusInternalServerError, "delete seed: "+err.Error())
			return
		}
	}
	if err := tx.Commit(); err != nil {
		opsWriteError(c, http.StatusInternalServerError, "commit delete: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
