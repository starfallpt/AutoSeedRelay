package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/autoseedrelay/relay/internal/backup"
	"github.com/autoseedrelay/relay/internal/qb"
	"github.com/autoseedrelay/relay/internal/store"
	"github.com/gin-gonic/gin"
)

// --- test doubles and setup ---

type resendCall struct {
	id   int64
	full bool
}

type fakeEngine struct {
	status map[string]any
	calls  []resendCall
}

func (f *fakeEngine) ResendSeed(_ context.Context, seedID int64, fullRerun bool) error {
	f.calls = append(f.calls, resendCall{id: seedID, full: fullRerun})
	return nil
}

func (f *fakeEngine) Status() map[string]any { return f.status }

func newTestOps(t *testing.T) (*Ops, *gin.Engine, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	repo := store.NewRepo(st.DB(), nil)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	ops := &Ops{
		Repo:    repo,
		Engine:  &fakeEngine{status: map[string]any{"running": true, "workers": 4}},
		QB:      qb.NewManager(),
		DataDir: dir,
	}
	ops.Register(r.Group("/api/v2"))
	return ops, r, dir
}

func doReq(r *gin.Engine, method, path string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func postMultipart(r *gin.Engine, path, field, filename string, content []byte) *httptest.ResponseRecorder {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, err := w.CreateFormFile(field, filename)
	if err != nil {
		panic(err)
	}
	if _, err := fw.Write(content); err != nil {
		panic(err)
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// --- fixture helpers (direct SQL for full control over timestamps/status) ---

func insertSeed(t *testing.T, repo *store.Repo, source, status string, discoveredAt int64) int64 {
	t.Helper()
	if discoveredAt == 0 {
		discoveredAt = time.Now().Unix()
	}
	res, err := repo.DB().Exec(
		`INSERT INTO seeds (source_site, info_hash, title, size, promotion, status, discovered_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		source, "hash-"+source, "title-"+source, 1024, "free", status, discoveredAt)
	if err != nil {
		t.Fatalf("insert seed: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func insertTarget(t *testing.T, repo *store.Repo) int64 {
	t.Helper()
	res, err := repo.DB().Exec(`INSERT INTO targets (name) VALUES (?)`, "target")
	if err != nil {
		t.Fatalf("insert target: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func insertQB(t *testing.T, repo *store.Repo) int64 {
	t.Helper()
	res, err := repo.DB().Exec(`INSERT INTO qb_instances (name, host) VALUES (?, ?)`, "qb", "http://127.0.0.1")
	if err != nil {
		t.Fatalf("insert qb: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func insertRecord(t *testing.T, repo *store.Repo, seedID, targetID int64, role, status string) {
	t.Helper()
	_, err := repo.DB().Exec(
		`INSERT INTO relay_records (seed_id, target_id, role, status) VALUES (?, ?, ?, ?)`,
		seedID, targetID, role, status)
	if err != nil {
		t.Fatalf("insert record: %v", err)
	}
}

func insertReplica(t *testing.T, repo *store.Repo, seedID, qbID int64) {
	t.Helper()
	_, err := repo.DB().Exec(
		`INSERT INTO seed_replicas (seed_id, qb_id, role) VALUES (?, ?, 'origin')`,
		seedID, qbID)
	if err != nil {
		t.Fatalf("insert replica: %v", err)
	}
}

func insertLog(t *testing.T, repo *store.Repo, seedID int64, level string) {
	t.Helper()
	var sid any
	if seedID != 0 {
		sid = seedID
	}
	_, err := repo.DB().Exec(
		`INSERT INTO activity_log (seed_id, level, action, detail) VALUES (?, ?, ?, ?)`,
		sid, level, "test", "detail")
	if err != nil {
		t.Fatalf("insert log: %v", err)
	}
}

func countRows(t *testing.T, repo *store.Repo, query string, arg any) int64 {
	t.Helper()
	var n int64
	if err := repo.DB().QueryRow(query, arg).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// --- tests ---

func TestOpsListSeeds(t *testing.T) {
	ops, r, _ := newTestOps(t)
	for i := 0; i < 3; i++ {
		insertSeed(t, ops.Repo, fmt.Sprintf("site%d", i), "seeding", 0)
	}
	for i := 0; i < 2; i++ {
		insertSeed(t, ops.Repo, fmt.Sprintf("failsite%d", i), "failed", 0)
	}

	// Unfiltered page 1 size 2 → total 5, 2 rows, newest first.
	var body struct {
		Items []seedJSON `json:"items"`
		Total int64      `json:"total"`
		Page  int        `json:"page"`
		Size  int        `json:"size"`
	}
	rec := doReq(r, http.MethodGet, "/api/v2/seeds?page=1&size=2", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Total != 5 {
		t.Fatalf("total = %d, want 5", body.Total)
	}
	if body.Page != 1 || body.Size != 2 {
		t.Fatalf("page/size = %d/%d, want 1/2", body.Page, body.Size)
	}
	if len(body.Items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(body.Items))
	}
	if body.Items[0].ID < body.Items[1].ID {
		t.Fatalf("expected descending ids, got %d then %d", body.Items[0].ID, body.Items[1].ID)
	}

	// Filter status=failed → total 2, all failed.
	rec = doReq(r, http.MethodGet, "/api/v2/seeds?status=failed", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Total != 2 {
		t.Fatalf("total(failed) = %d, want 2", body.Total)
	}
	if len(body.Items) != 2 {
		t.Fatalf("len(items failed) = %d, want 2", len(body.Items))
	}
	for _, s := range body.Items {
		if s.Status != "failed" {
			t.Fatalf("seed %d status = %q, want failed", s.ID, s.Status)
		}
	}
}

func TestOpsGetSeed(t *testing.T) {
	ops, r, _ := newTestOps(t)
	id := insertSeed(t, ops.Repo, "site", "seeding", 0)
	tid := insertTarget(t, ops.Repo)
	qid := insertQB(t, ops.Repo)
	insertRecord(t, ops.Repo, id, tid, "publisher", "published")
	insertReplica(t, ops.Repo, id, qid)
	insertLog(t, ops.Repo, id, "info")

	rec := doReq(r, http.MethodGet, fmt.Sprintf("/api/v2/seeds/%d", id), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Seed     seedJSON      `json:"seed"`
		Records  []recordJSON  `json:"records"`
		Replicas []replicaJSON `json:"replicas"`
		Logs     []logJSON     `json:"logs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Seed.ID != id {
		t.Fatalf("seed id = %d, want %d", body.Seed.ID, id)
	}
	if body.Seed.Title != "title-site" {
		t.Fatalf("title = %q, want title-site", body.Seed.Title)
	}
	if len(body.Records) != 1 || len(body.Replicas) != 1 || len(body.Logs) != 1 {
		t.Fatalf("records/replicas/logs = %d/%d/%d, want 1/1/1", len(body.Records), len(body.Replicas), len(body.Logs))
	}

	// Missing id → 404.
	rec = doReq(r, http.MethodGet, "/api/v2/seeds/99999", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing seed status = %d, want 404", rec.Code)
	}
}

func TestOpsResendSeed(t *testing.T) {
	ops, r, _ := newTestOps(t)
	id := insertSeed(t, ops.Repo, "site", "failed", 0)
	eng := ops.Engine.(*fakeEngine)

	rec := doReq(r, http.MethodPost, fmt.Sprintf("/api/v2/seeds/%d/resend", id), strings.NewReader(`{"full":true}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["ok"] != true {
		t.Fatalf("ok = %v, want true", body["ok"])
	}
	if len(eng.calls) != 1 || eng.calls[0].id != id || !eng.calls[0].full {
		t.Fatalf("engine calls = %+v, want one full resend of %d", eng.calls, id)
	}

	// Empty body → full defaults false.
	doReq(r, http.MethodPost, fmt.Sprintf("/api/v2/seeds/%d/resend", id), nil)
	if len(eng.calls) != 2 || eng.calls[1].full {
		t.Fatalf("engine calls = %+v, want second non-full resend", eng.calls)
	}
}

func TestOpsDeleteSeed(t *testing.T) {
	ops, r, _ := newTestOps(t)

	// Downloading → 409.
	dl := insertSeed(t, ops.Repo, "dl", "downloading", 0)
	rec := doReq(r, http.MethodDelete, fmt.Sprintf("/api/v2/seeds/%d", dl), nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("downloading delete status = %d, want 409", rec.Code)
	}

	// Seeding seed with dependent rows → 200 and cascade cleanup.
	s := insertSeed(t, ops.Repo, "seed", "seeding", 0)
	tid := insertTarget(t, ops.Repo)
	qid := insertQB(t, ops.Repo)
	insertRecord(t, ops.Repo, s, tid, "publisher", "published")
	insertReplica(t, ops.Repo, s, qid)
	insertLog(t, ops.Repo, s, "info")

	rec = doReq(r, http.MethodDelete, fmt.Sprintf("/api/v2/seeds/%d", s), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("seeding delete status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if n := countRows(t, ops.Repo, `SELECT COUNT(*) FROM seeds WHERE id = ?`, s); n != 0 {
		t.Fatalf("seed still present: %d", n)
	}
	if n := countRows(t, ops.Repo, `SELECT COUNT(*) FROM relay_records WHERE seed_id = ?`, s); n != 0 {
		t.Fatalf("records still present: %d", n)
	}
	if n := countRows(t, ops.Repo, `SELECT COUNT(*) FROM seed_replicas WHERE seed_id = ?`, s); n != 0 {
		t.Fatalf("replicas still present: %d", n)
	}
	if n := countRows(t, ops.Repo, `SELECT COUNT(*) FROM activity_log WHERE seed_id = ?`, s); n != 0 {
		t.Fatalf("logs still present: %d", n)
	}
}

func TestOpsEventsCursor(t *testing.T) {
	ops, r, _ := newTestOps(t)
	for i := 0; i < 3; i++ {
		insertLog(t, ops.Repo, 0, "info")
	}

	var body struct {
		Events []logJSON `json:"events"`
		Latest int64     `json:"latest"`
	}
	rec := doReq(r, http.MethodGet, "/api/v2/events?since=0", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Events) != 3 {
		t.Fatalf("len(events) = %d, want 3", len(body.Events))
	}
	if body.Events[0].ID != 3 || body.Events[1].ID != 2 || body.Events[2].ID != 1 {
		t.Fatalf("events not newest-first: %+v", body.Events)
	}
	if body.Latest != 3 {
		t.Fatalf("latest = %d, want 3", body.Latest)
	}

	// since=3 → nothing newer, latest stays at the cursor.
	rec = doReq(r, http.MethodGet, "/api/v2/events?since=3", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Events) != 0 || body.Latest != 3 {
		t.Fatalf("since=3 → events=%d latest=%d, want 0/3", len(body.Events), body.Latest)
	}

	// level filter.
	insertLog(t, ops.Repo, 0, "warning")
	rec = doReq(r, http.MethodGet, "/api/v2/events?since=0&level=warning", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Events) != 1 || body.Events[0].Level != "warning" {
		t.Fatalf("warning filter → %+v, want exactly one warning", body.Events)
	}
}

func TestOpsDashboard(t *testing.T) {
	ops, r, _ := newTestOps(t)
	// 2 seeding, 1 retry, 1 failed → 4 seeds discovered today.
	insertSeed(t, ops.Repo, "s1", "seeding", 0)
	insertSeed(t, ops.Repo, "s2", "seeding", 0)
	insertSeed(t, ops.Repo, "r1", "retry", 0)
	sid := insertSeed(t, ops.Repo, "f1", "failed", 0)

	// One published + one cross-seeded record (both "today" via updated_at).
	tid := insertTarget(t, ops.Repo)
	insertRecord(t, ops.Repo, sid, tid, "publisher", "published")
	tid2 := insertTarget(t, ops.Repo)
	insertRecord(t, ops.Repo, sid, tid2, "seeder", "cross_seeding")
	insertLog(t, ops.Repo, 0, "info")

	rec := doReq(r, http.MethodGet, "/api/v2/dashboard", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Status struct {
			QBS     []map[string]any `json:"qbs"`
			Sources []map[string]any `json:"sources"`
			Disk    map[string]any   `json:"disk"`
			Uptime  int64            `json:"uptime_seconds"`
			Engine  map[string]any   `json:"engine"`
		} `json:"status"`
		Stats  map[string]int64 `json:"stats"`
		Tasks  []seedJSON       `json:"tasks"`
		Events []logJSON        `json:"events"`
		Trend  []map[string]any `json:"trend"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got := body.Stats["current_seeding"]; got != 2 {
		t.Fatalf("current_seeding = %d, want 2", got)
	}
	if got := body.Stats["retry"]; got != 1 {
		t.Fatalf("retry = %d, want 1", got)
	}
	if got := body.Stats["failed"]; got != 1 {
		t.Fatalf("failed = %d, want 1", got)
	}
	if got := body.Stats["total_published"]; got != 1 {
		t.Fatalf("total_published = %d, want 1", got)
	}
	if got := body.Stats["total_cross_seeded"]; got != 1 {
		t.Fatalf("total_cross_seeded = %d, want 1", got)
	}
	if got := body.Stats["today_published"]; got != 1 {
		t.Fatalf("today_published = %d, want 1", got)
	}
	if got := body.Stats["today_cross_seeded"]; got != 1 {
		t.Fatalf("today_cross_seeded = %d, want 1", got)
	}

	if len(body.Tasks) != 4 {
		t.Fatalf("len(tasks) = %d, want 4", len(body.Tasks))
	}
	if len(body.Events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(body.Events))
	}
	if len(body.Trend) != 7 {
		t.Fatalf("len(trend) = %d, want 7", len(body.Trend))
	}
	// All 4 seeds were discovered today → the last (today) bucket holds them.
	if got := body.Trend[len(body.Trend)-1]["count"].(float64); int64(got) != 4 {
		t.Fatalf("today trend count = %v, want 4", body.Trend[len(body.Trend)-1]["count"])
	}
	if body.Status.Uptime < 0 {
		t.Fatalf("uptime_seconds = %d, want >= 0", body.Status.Uptime)
	}
	if body.Status.Engine["running"] != true || body.Status.Engine["workers"].(float64) != 4 {
		t.Fatalf("engine status = %+v, want running=true workers=4", body.Status.Engine)
	}
	if body.Status.Disk["free_gb"].(float64) != 0 || body.Status.Disk["total_gb"].(float64) != 0 {
		t.Fatalf("disk = %+v, want zeros (no qb instances)", body.Status.Disk)
	}
}

func TestOpsExportBackup(t *testing.T) {
	ops, r, _ := newTestOps(t)
	insertSeed(t, ops.Repo, "site", "seeding", 0)

	rec := doReq(r, http.MethodGet, "/api/v2/backup/export", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("content-type = %q, want application/zip", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("content-disposition = %q, want attachment", cd)
	}

	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("body is not a valid zip: %v", err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["relay.db"] || !names["meta.json"] {
		t.Fatalf("zip entries = %v, want relay.db + meta.json", names)
	}
}

func TestOpsRestoreBackup(t *testing.T) {
	ops, r, dir := newTestOps(t)

	var buf bytes.Buffer
	if err := backup.Export(context.Background(), ops.Repo.DB(), &buf); err != nil {
		t.Fatalf("export: %v", err)
	}

	rec := postMultipart(r, "/api/v2/backup/restore", "file", "backup.zip", buf.Bytes())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["ok"] != true || body["restart_required"] != true {
		t.Fatalf("body = %v, want ok=true restart_required=true", body)
	}

	got, err := os.ReadFile(filepath.Join(dir, "restore-pending.zip"))
	if err != nil {
		t.Fatalf("restore-pending.zip missing: %v", err)
	}
	if !bytes.Equal(got, buf.Bytes()) {
		t.Fatalf("restore-pending.zip differs from upload")
	}

	// Invalid archive → 400.
	rec = postMultipart(r, "/api/v2/backup/restore", "file", "bad.zip", []byte("not a zip"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid restore status = %d, want 400", rec.Code)
	}
}
