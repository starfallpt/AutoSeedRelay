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
	if ver != 2 {
		t.Fatalf("expected schema version 2, got %d", ver)
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
	} else if ver != 2 {
		t.Fatalf("expected schema version 2 after reopen, got %d", ver)
	}

	tables := []string{
		"sources", "targets", "qb_instances", "seeds", "relay_records",
		"seed_replicas", "activity_log", "notifier_instances",
		"notifier_routes", "strategies",
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
}
