package store

import (
	"path/filepath"
	"testing"
)

func TestOpenMigrateIdempotent(t *testing.T) {
	// Parent directory does not exist yet — Open must create it.
	path := filepath.Join(t.TempDir(), "nested", "relay.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	defer s.Close() // safety net; Close is idempotent

	ver, err := s.MigrateVersion()
	if err != nil {
		t.Fatalf("MigrateVersion: %v", err)
	}
	if ver != 6 {
		t.Fatalf("expected schema version 6, got %d", ver)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-open the same file: migration must be a no-op (idempotent).
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()

	if ver, err := s2.MigrateVersion(); err != nil {
		t.Fatalf("MigrateVersion after reopen: %v", err)
	} else if ver != 6 {
		t.Fatalf("expected schema version 6 after reopen, got %d", ver)
	}

	tables := []string{
		"sources", "targets", "qb_instances", "seeds", "relay_records",
		"seed_replicas", "activity_log", "notifier_instances",
		"notifier_routes", "strategies", "seen_hashes",
	}
	for _, table := range tables {
		var name string
		err := s2.db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", table,
		).Scan(&name)
		if err != nil {
			t.Fatalf("table %q missing: %v", table, err)
		}
	}

	// targets must carry the tags_map column added by migration 00002.
	var col string
	if err := s2.db.QueryRow(
		"SELECT name FROM pragma_table_info('targets') WHERE name='tags_map'",
	).Scan(&col); err != nil {
		t.Fatalf("targets.tags_map column missing: %v", err)
	}

	// activity_log must carry the created_at index (§6).
	var idx string
	if err := s2.db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='index' AND name='idx_activity_log_created_at'",
	).Scan(&idx); err != nil {
		t.Fatalf("idx_activity_log_created_at missing: %v", err)
	}

	// strategies must be seeded with its single default row (id=1).
	var n int
	if err := s2.db.QueryRow("SELECT COUNT(*) FROM strategies WHERE id=1").Scan(&n); err != nil {
		t.Fatalf("strategies default row: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 strategies row, got %d", n)
	}

	// strategies must carry the monitor columns added by migration 00005.
	for _, col := range []string{"disk_low_gb", "disk_critical_gb", "low_speed_kbps", "low_speed_duration_sec", "low_speed_action"} {
		if err := s2.db.QueryRow(
			"SELECT name FROM pragma_table_info('strategies') WHERE name=?", col,
		).Scan(&col); err != nil {
			t.Fatalf("strategies.%s column missing: %v", col, err)
		}
	}

	// seen_hashes must carry the tombstone columns added by migration 00006.
	for _, col := range []string{"source_site", "info_hash", "first_seen_at"} {
		if err := s2.db.QueryRow(
			"SELECT name FROM pragma_table_info('seen_hashes') WHERE name=?", col,
		).Scan(&col); err != nil {
			t.Fatalf("seen_hashes.%s column missing: %v", col, err)
		}
	}
}

func TestForeignKeysEnforcedByDSN(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fk.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// A relay_records row referencing a non-existent seed must be rejected:
	// foreign_keys=1 must be in effect on the freshly-opened connection.
	if _, err := s.db.Exec(
		`INSERT INTO relay_records (seed_id, target_id) VALUES (1, 1)`); err == nil {
		t.Fatal("expected FK violation inserting relay_records with unknown seed_id")
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM relay_records`).Scan(&n); err != nil {
		t.Fatalf("count relay_records: %v", err)
	}
	if n != 0 {
		t.Fatalf("relay_records rows = %d, want 0 (FK must block the insert)", n)
	}
}
