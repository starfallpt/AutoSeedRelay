// Package store provides the SQLite storage layer for AutoSeedRelay:
// opening the database, running schema migrations, and health checks.
//
// The schema (see internal/store/migrations) is applied on first open using a
// minimal self-managed version loop keyed on PRAGMA user_version — no external
// migration tool is required.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// DriverName is the modernc SQLite driver name registered with database/sql.
const DriverName = "sqlite"

// Store is an open SQLite database handle. A single connection is used
// (SetMaxOpenConns(1)) so connection-scoped PRAGMAs stay effective and all
// writes are serialized.
type Store struct {
	db *sql.DB
}

// Open opens (creating parent directories and the file as needed) the SQLite
// database at path, configures WAL / busy-timeout / foreign-keys, applies any
// pending migrations, and verifies connectivity with a Ping.
func Open(path string) (*Store, error) {
	if path == "" {
		path = "data/relay.db"
	}
	if parent := filepath.Dir(path); parent != "" && parent != "." {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return nil, fmt.Errorf("store: create parent dir: %w", err)
		}
	}

	db, err := sql.Open(DriverName, path)
	if err != nil {
		return nil, fmt.Errorf("store: open sqlite: %w", err)
	}
	// One connection keeps connection-scoped PRAGMAs sticky and serializes
	// writes (SQLite permits a single writer anyway).
	db.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("store: %s: %w", pragma, err)
		}
	}

	if err := migrateEmbedded(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the underlying database. It is safe to call more than once.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// MigrateVersion returns the current schema version (PRAGMA user_version). It
// is used by health checks to confirm the schema has been applied.
func (s *Store) MigrateVersion() (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("store: closed")
	}
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("store: read user_version: %w", err)
	}
	return v, nil
}
