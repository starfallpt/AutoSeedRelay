package store

import (
	"fmt"
)

// ---- types ----

// ActivityEntry represents one row in the activity_log table.
type ActivityEntry struct {
	ID         int64
	SeedID     *int64
	TargetSite *string
	Action     string
	Detail     *string
	CreatedAt  string
}

var actCols = []struct {
	name string
	dst  func(a *ActivityEntry) interface{}
}{
	{"id", func(a *ActivityEntry) interface{} { return &a.ID }},
	{"seed_id", func(a *ActivityEntry) interface{} { return &a.SeedID }},
	{"target_site", func(a *ActivityEntry) interface{} { return &a.TargetSite }},
	{"action", func(a *ActivityEntry) interface{} { return &a.Action }},
	{"detail", func(a *ActivityEntry) interface{} { return &a.Detail }},
	{"created_at", func(a *ActivityEntry) interface{} { return &a.CreatedAt }},
}

func scanActivity(scanner interface{ Scan(dest ...interface{}) error }) (*ActivityEntry, error) {
	a := &ActivityEntry{}
	dests := make([]interface{}, len(actCols))
	for i, c := range actCols {
		dests[i] = c.dst(a)
	}
	if err := scanner.Scan(dests...); err != nil {
		return nil, err
	}
	return a, nil
}

// ---- CRUD ----

// LogActivity writes an audit entry to activity_log. seedID may be 0
// when there is no associated seed; targetSite may be "" likewise.
// detail is an optional JSON string.
func (s *RelayStore) LogActivity(seedID int64, targetSite, action, detail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.requireOpen()
	if err != nil {
		return err
	}
	now := utcNow()
	var sid interface{}
	if seedID == 0 {
		sid = nil
	} else {
		sid = seedID
	}
	var ts interface{}
	if targetSite == "" {
		ts = nil
	} else {
		ts = targetSite
	}
	var det interface{}
	if detail == "" {
		det = nil
	} else {
		det = detail
	}
	if _, err := db.Exec(
		"INSERT INTO activity_log (seed_id, target_site, action, detail, created_at) VALUES (?,?,?,?,?)",
		sid, ts, action, det, now,
	); err != nil {
		return fmt.Errorf("store: log activity: %w", err)
	}
	return nil
}

// QueryActivity returns recent activity log entries, newest first.
func (s *RelayStore) QueryActivity(limit, offset int) ([]*ActivityEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.requireOpen()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(
		"SELECT * FROM activity_log ORDER BY id DESC LIMIT ? OFFSET ?", limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: query activity: %w", err)
	}
	defer rows.Close()
	var out []*ActivityEntry
	for rows.Next() {
		entry, err := scanActivity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// QueryActivityBySeed returns activity log entries for a specific seed.
func (s *RelayStore) QueryActivityBySeed(seedID int64, limit, offset int) ([]*ActivityEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.requireOpen()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(
		"SELECT * FROM activity_log WHERE seed_id=? ORDER BY id DESC LIMIT ? OFFSET ?",
		seedID, limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("store: query activity by seed: %w", err)
	}
	defer rows.Close()
	var out []*ActivityEntry
	for rows.Next() {
		entry, err := scanActivity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// QueryActivityBySite returns activity log entries for a specific target site.
func (s *RelayStore) QueryActivityBySite(targetSite string, limit, offset int) ([]*ActivityEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.requireOpen()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(
		"SELECT * FROM activity_log WHERE target_site=? ORDER BY id DESC LIMIT ? OFFSET ?",
		targetSite, limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("store: query activity by site: %w", err)
	}
	defer rows.Close()
	var out []*ActivityEntry
	for rows.Next() {
		entry, err := scanActivity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}
