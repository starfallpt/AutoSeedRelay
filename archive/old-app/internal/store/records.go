package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// ---- record statuses ----

const (
	RecStatusPending     = "pending"
	RecStatusPublishing  = "publishing"
	RecStatusPublished   = "published"
	RecStatusCrossSeeding = "cross_seeding"
	RecStatusSeeding     = "seeding"
	RecStatusMonitoring  = "monitoring"
	RecStatusRetired     = "retired"
	RecStatusFailed      = "failed"
)

// ---- retire reasons ----

const (
	RetireSeededEnough = "seeded_enough"
	RetireTimeout      = "timeout"
	RetireManual       = "manual"
	RetireDiskEmergency = "disk_emergency"
)

// ---- roles ----

const (
	RolePublisher = "publisher"
	RoleSeeder    = "seeder"
)

// RelayRecord represents one row in the relay_records table.
type RelayRecord struct {
	ID            int64
	SeedID        int64
	TargetSite    string
	TargetID      *int64
	TargetHash    *string
	Role          string
	PublishedAt   *string
	CrossSeededAt *string
	Status        string
	Seeders       int
	Leechers      int
	LastCheckAt   *string
	RetiredAt     *string
	RetireReason  *string
	CreatedAt     string
	UpdatedAt     string
}

var recCols = []struct {
	name string
	dst  func(r *RelayRecord) interface{}
}{
	{"id", func(r *RelayRecord) interface{} { return &r.ID }},
	{"seed_id", func(r *RelayRecord) interface{} { return &r.SeedID }},
	{"target_site", func(r *RelayRecord) interface{} { return &r.TargetSite }},
	{"target_id", func(r *RelayRecord) interface{} { return &r.TargetID }},
	{"target_hash", func(r *RelayRecord) interface{} { return &r.TargetHash }},
	{"role", func(r *RelayRecord) interface{} { return &r.Role }},
	{"published_at", func(r *RelayRecord) interface{} { return &r.PublishedAt }},
	{"cross_seeded_at", func(r *RelayRecord) interface{} { return &r.CrossSeededAt }},
	{"status", func(r *RelayRecord) interface{} { return &r.Status }},
	{"seeders", func(r *RelayRecord) interface{} { return &r.Seeders }},
	{"leechers", func(r *RelayRecord) interface{} { return &r.Leechers }},
	{"last_check_at", func(r *RelayRecord) interface{} { return &r.LastCheckAt }},
	{"retired_at", func(r *RelayRecord) interface{} { return &r.RetiredAt }},
	{"retire_reason", func(r *RelayRecord) interface{} { return &r.RetireReason }},
	{"created_at", func(r *RelayRecord) interface{} { return &r.CreatedAt }},
	{"updated_at", func(r *RelayRecord) interface{} { return &r.UpdatedAt }},
}

func scanRecord(scanner interface{ Scan(dest ...interface{}) error }) (*RelayRecord, error) {
	r := &RelayRecord{}
	dests := make([]interface{}, len(recCols))
	for i, c := range recCols {
		dests[i] = c.dst(r)
	}
	if err := scanner.Scan(dests...); err != nil {
		return nil, err
	}
	return r, nil
}

// ---- CRUD ----

// InsertRecord inserts a new relay record. Returns the new row id.
func (s *RelayStore) InsertRecord(rec *RelayRecord) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.requireOpen()
	if err != nil {
		return 0, err
	}
	if rec.Role == "" {
		rec.Role = RoleSeeder
	}
	if rec.Status == "" {
		rec.Status = RecStatusPending
	}
	now := utcNow()
	if rec.CreatedAt == "" {
		rec.CreatedAt = now
	}
	if rec.UpdatedAt == "" {
		rec.UpdatedAt = now
	}

	const query = `INSERT INTO relay_records (
		seed_id, target_site, target_id, target_hash, role,
		published_at, cross_seeded_at, status, seeders, leechers,
		last_check_at, retired_at, retire_reason, created_at, updated_at
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

	res, err := db.Exec(query,
		rec.SeedID, rec.TargetSite, nilPtr(rec.TargetID), nilPtr(rec.TargetHash),
		rec.Role, nilPtr(rec.PublishedAt), nilPtr(rec.CrossSeededAt),
		rec.Status, rec.Seeders, rec.Leechers,
		nilPtr(rec.LastCheckAt), nilPtr(rec.RetiredAt), nilPtr(rec.RetireReason),
		rec.CreatedAt, rec.UpdatedAt,
	)
	if err != nil {
		return 0, fmt.Errorf("store: insert record: %w", err)
	}
	return res.LastInsertId()
}

// GetRecordBySeedTarget returns a record by (seed_id, target_site), or nil
// when not found.
func (s *RelayStore) GetRecordBySeedTarget(seedID int64, targetSite string) (*RelayRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.requireOpen()
	if err != nil {
		return nil, err
	}
	row := db.QueryRow("SELECT * FROM relay_records WHERE seed_id=? AND target_site=?", seedID, targetSite)
	rec, scanErr := scanRecord(row)
	if scanErr == sql.ErrNoRows {
		return nil, nil
	}
	if scanErr != nil {
		return nil, fmt.Errorf("store: get record: %w", scanErr)
	}
	return rec, nil
}

// UpdateRecordStatus updates a relay record's status. updated_at is
// always refreshed.
func (s *RelayStore) UpdateRecordStatus(id int64, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.requireOpen()
	if err != nil {
		return err
	}
	now := utcNow()
	if _, err := db.Exec(
		"UPDATE relay_records SET status=?, updated_at=? WHERE id=?",
		status, now, id,
	); err != nil {
		return fmt.Errorf("store: update record status: %w", err)
	}
	return nil
}

// UpdateRecordStats updates the seeder / leecher counts for a relay
// record. updated_at is always refreshed.
func (s *RelayStore) UpdateRecordStats(id int64, seeders, leechers int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.requireOpen()
	if err != nil {
		return err
	}
	now := utcNow()
	if _, err := db.Exec(
		"UPDATE relay_records SET seeders=?, leechers=?, last_check_at=?, updated_at=? WHERE id=?",
		seeders, leechers, now, now, id,
	); err != nil {
		return fmt.Errorf("store: update record stats: %w", err)
	}
	return nil
}

// ClaimPublish atomically claims the publisher role for a (seed_id,
// target_site) pair. It returns true when the claim succeeded (a new row
// was inserted), or false when the pair already exists (someone else
// already claimed it).
func (s *RelayStore) ClaimPublish(seedID int64, targetSite string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.requireOpen()
	if err != nil {
		return false, err
	}
	now := utcNow()
	res, err := db.Exec(
		`INSERT OR IGNORE INTO relay_records (seed_id, target_site, role, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		seedID, targetSite, RolePublisher, RecStatusPending, now, now,
	)
	if err != nil {
		return false, fmt.Errorf("store: claim publish: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// GetActiveRecords returns records that are currently seeding /
// cross-seeding / monitoring (not yet retired).
func (s *RelayStore) GetActiveRecords() ([]*RelayRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.requireOpen()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(
		`SELECT * FROM relay_records
		 WHERE status IN (?,?,?) AND retired_at IS NULL
		 ORDER BY created_at`,
		RecStatusSeeding, RecStatusCrossSeeding, RecStatusMonitoring,
	)
	if err != nil {
		return nil, fmt.Errorf("store: get active records: %w", err)
	}
	defer rows.Close()
	var out []*RelayRecord
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// RetireRecord marks a relay record as retired with a reason.
func (s *RelayStore) RetireRecord(id int64, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.requireOpen()
	if err != nil {
		return err
	}
	now := utcNow()
	if _, err := db.Exec(
		"UPDATE relay_records SET status=?, retired_at=?, retire_reason=?, updated_at=? WHERE id=?",
		RecStatusRetired, now, reason, now, id,
	); err != nil {
		return fmt.Errorf("store: retire record: %w", err)
	}
	return nil
}

// ListRecordsBySeed returns all relay records for a given seed.
func (s *RelayStore) ListRecordsBySeed(seedID int64) ([]*RelayRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.requireOpen()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query("SELECT * FROM relay_records WHERE seed_id=? ORDER BY id", seedID)
	if err != nil {
		return nil, fmt.Errorf("store: list records by seed: %w", err)
	}
	defer rows.Close()
	var out []*RelayRecord
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// setRecordColumn is a helper that updates a single nullable column
// (keyed by name) on a relay record.  Used internally by higher-level
// orchestration code.
func (s *RelayStore) setRecordColumn(id int64, col string, val interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.requireOpen()
	if err != nil {
		return err
	}
	now := utcNow()
	q := fmt.Sprintf("UPDATE relay_records SET %s=?, updated_at=? WHERE id=?", strings.TrimSpace(col))
	if _, err := db.Exec(q, val, now, id); err != nil {
		return fmt.Errorf("store: set %s on record %d: %w", col, id, err)
	}
	return nil
}
