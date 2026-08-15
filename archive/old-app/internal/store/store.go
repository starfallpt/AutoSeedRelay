// Package store implements SQLite-backed dedup and status storage for
// relay jobs.
//
// A single relay_jobs table is keyed on the source info_hash for dedup;
// each job records its position in the relay pipeline via target_status.
// All writes are idempotent (repeated Add never creates duplicate rows)
// and serialized with a mutex. WAL journal mode is enabled to allow
// concurrent cross-process reads without blocking.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// StatusEnum lists the allowed relay_jobs.target_status values.
var StatusEnum = map[string]bool{
	"pending":          true,
	"downloaded":       true,
	"added_to_qb":      true,
	"seeded":           true,
	"uploaded":         true,
	"cross_seeded":     true,
	"skipped_existing": true,
	"failed":           true,
	"skipped":          true,
}

// DefaultStatus is the initial target_status for a new job.
const DefaultStatus = "pending"

// Updatable lists the business fields that Add / MarkStatus may write.
// info_hash, created_at and updated_at are managed internally.
var Updatable = []string{
	"rss_id",
	"title",
	"source_site",
	"source_size",
	"qb_hash",
	"target_status",
	"target_id",
	"target_site",
	"error",
}

// columns is the full relay_jobs column list (definition order).
var columns = []string{
	"info_hash", "rss_id", "title", "source_site", "source_size",
	"qb_hash", "target_status", "target_id", "target_site",
	"error", "created_at", "updated_at",
}

var updatableSet = func() map[string]bool {
	m := make(map[string]bool, len(Updatable))
	for _, f := range Updatable {
		m[f] = true
	}
	return m
}()

// RelayStore is a SQLite dedup/status store.
type RelayStore struct {
	dbPath string
	db     *sql.DB
	mu     sync.Mutex
}

// Open opens (creating if needed) the SQLite database at dbPath. It
// enables WAL mode and ensures the relay_jobs table exists.
func Open(dbPath string) (*RelayStore, error) {
	if dbPath == "" {
		dbPath = "data/relay.db"
	}
	parent := filepath.Dir(dbPath)
	if parent != "" && parent != "." {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return nil, fmt.Errorf("store: create parent dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("store: open sqlite: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: enable WAL: %w", err)
	}
	const createTable = `CREATE TABLE IF NOT EXISTS relay_jobs (
		info_hash      TEXT PRIMARY KEY,
		rss_id         INTEGER,
		title          TEXT,
		source_site    TEXT,
		source_size    INTEGER,
		qb_hash        TEXT,
		target_status  TEXT NOT NULL DEFAULT 'pending',
		target_id      INTEGER,
		target_site    TEXT,
		error          TEXT,
		created_at     TEXT,
		updated_at     TEXT
	)`
	if _, err := db.Exec(createTable); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: create table: %w", err)
	}
	s := &RelayStore{dbPath: dbPath, db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return s, nil
}

func (s *RelayStore) requireOpen() (*sql.DB, error) {
	if s.db == nil {
		return nil, fmt.Errorf("store: RelayStore 已关闭")
	}
	return s.db, nil
}

// scanRow scans one row into a map keyed by column name. The value type
// comes from the driver: int64 for INTEGER, string for TEXT, nil for
// NULL.
func scanRow(sc interface{ Scan(dest ...any) error }) (map[string]any, error) {
	dest := make([]any, len(columns))
	ptrs := make([]any, len(columns))
	for i := range dest {
		ptrs[i] = &dest[i]
	}
	if err := sc.Scan(ptrs...); err != nil {
		return nil, err
	}
	m := make(map[string]any, len(columns))
	for i, col := range columns {
		m[col] = dest[i]
	}
	return m, nil
}

func (s *RelayStore) updateRow(db *sql.DB, infoHash string, updates map[string]any) error {
	sets := make([]string, 0, len(updates))
	vals := make([]any, 0, len(updates)+1)
	for k, v := range updates {
		sets = append(sets, k+"=?")
		vals = append(vals, v)
	}
	vals = append(vals, infoHash)
	query := fmt.Sprintf("UPDATE relay_jobs SET %s WHERE info_hash=?", strings.Join(sets, ", "))
	if _, err := db.Exec(query, vals...); err != nil {
		return fmt.Errorf("store: update: %w", err)
	}
	return nil
}

func utcNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}

func isEmpty(v any) bool {
	if v == nil {
		return true
	}
	if s, ok := v.(string); ok && s == "" {
		return true
	}
	return false
}

// Has reports whether the info_hash already exists (dedup check).
func (s *RelayStore) Has(infoHash string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.requireOpen()
	if err != nil {
		return false, err
	}
	var one int
	err = db.QueryRow("SELECT 1 FROM relay_jobs WHERE info_hash=?", infoHash).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: has: %w", err)
	}
	return true, nil
}

// Get returns a single job as a map, or nil when not found.
func (s *RelayStore) Get(infoHash string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.requireOpen()
	if err != nil {
		return nil, err
	}
	row := db.QueryRow("SELECT * FROM relay_jobs WHERE info_hash=?", infoHash)
	m, err := scanRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get: %w", err)
	}
	return m, nil
}

// All returns every job ordered by creation time.
func (s *RelayStore) All() ([]map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.requireOpen()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query("SELECT * FROM relay_jobs ORDER BY created_at, info_hash")
	if err != nil {
		return nil, fmt.Errorf("store: all: %w", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		m, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// PendingJobs returns up to limit jobs whose target_status is 'pending'.
func (s *RelayStore) PendingJobs(limit ...int) ([]map[string]any, error) {
	l := 50
	if len(limit) > 0 {
		l = limit[0]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.requireOpen()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(
		"SELECT * FROM relay_jobs WHERE target_status='pending' ORDER BY created_at LIMIT ?", l)
	if err != nil {
		return nil, fmt.Errorf("store: pending_jobs: %w", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		m, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Add inserts or updates a job. It returns true when a new row was
// inserted and false when an existing row was updated. Empty (nil or "")
// fields never overwrite existing values. job must contain info_hash;
// every other field is optional.
func (s *RelayStore) Add(job map[string]any) (bool, error) {
	ih, _ := job["info_hash"].(string)
	if ih == "" {
		return false, fmt.Errorf("store: job 必须包含 info_hash 字段")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.requireOpen()
	if err != nil {
		return false, err
	}

	now := utcNow()
	var one int
	err = db.QueryRow("SELECT 1 FROM relay_jobs WHERE info_hash=?", ih).Scan(&one)
	exists := err == nil
	if err != nil && err != sql.ErrNoRows {
		return false, fmt.Errorf("store: add check: %w", err)
	}

	if !exists {
		fields := map[string]any{}
		for _, k := range Updatable {
			if v, ok := job[k]; ok {
				fields[k] = v
			}
		}
		fields["info_hash"] = ih
		if _, ok := fields["target_status"]; !ok {
			fields["target_status"] = DefaultStatus
		}
		fields["created_at"] = now
		fields["updated_at"] = now

		cols := make([]string, 0, len(fields))
		vals := make([]any, 0, len(fields))
		for k, v := range fields {
			cols = append(cols, k)
			vals = append(vals, v)
		}
		ph := strings.Repeat("?,", len(cols)-1) + "?"
		query := fmt.Sprintf("INSERT INTO relay_jobs (%s) VALUES (%s)", strings.Join(cols, ", "), ph)
		if _, err := db.Exec(query, vals...); err != nil {
			return false, fmt.Errorf("store: add insert: %w", err)
		}
		return true, nil
	}

	updates := map[string]any{}
	for _, k := range Updatable {
		if v, ok := job[k]; ok && !isEmpty(v) {
			updates[k] = v
		}
	}
	updates["updated_at"] = now
	if len(updates) > 0 {
		if err := s.updateRow(db, ih, updates); err != nil {
			return false, err
		}
	}
	return false, nil
}

// MarkStatus updates the status and optional Updatable fields (qb_hash,
// target_id, target_site, error, ...). Nil extra values are ignored.
// updated_at is refreshed.
func (s *RelayStore) MarkStatus(infoHash, status string, extra map[string]any) error {
	if !StatusEnum[status] {
		keys := make([]string, 0, len(StatusEnum))
		for k := range StatusEnum {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return fmt.Errorf("store: 非法状态: %q(允许: %s)", status, strings.Join(keys, ", "))
	}
	updates := map[string]any{"target_status": status, "updated_at": utcNow()}
	for k, v := range extra {
		if !updatableSet[k] {
			return fmt.Errorf("store: 未知字段: %q", k)
		}
		if v != nil {
			updates[k] = v
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.requireOpen()
	if err != nil {
		return err
	}
	return s.updateRow(db, infoHash, updates)
}

// Close closes the underlying SQLite connection (idempotent).
func (s *RelayStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}
