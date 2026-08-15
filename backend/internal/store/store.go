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
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// DriverName is the modernc SQLite driver name registered with database/sql.
const DriverName = "sqlite"

// DefaultDBPath is the database path used when none is configured.
const DefaultDBPath = "data/relay.db"

// Store is an open SQLite database handle. A single connection is used
// (SetMaxOpenConns(1)) so connection-scoped PRAGMAs stay effective and all
// writes are serialized.
type Store struct {
	db *sql.DB
}

// dsn builds the modernc SQLite data-source name for path. It encodes the path
// (folding Windows separators to '/', percent-encoding characters that would
// otherwise break URI parsing) and folds the per-connection PRAGMAs
// busy_timeout / foreign_keys into _pragma query parameters so that every
// freshly-opened connection carries them, not just the one current connection.
// journal_mode is deliberately NOT folded in here: it is database-scoped and is
// set once, explicitly, after Open (see Open).
func dsn(path string) string {
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	return "file:" + u.EscapedPath() + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
}

// Open opens (creating parent directories and the file as needed) the SQLite
// database at path, configures WAL / busy-timeout / foreign-keys, applies any
// pending migrations, and verifies connectivity with a Ping.
func Open(path string) (*Store, error) {
	if path == "" {
		path = DefaultDBPath
	}
	if parent := filepath.Dir(path); parent != "" && parent != "." {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return nil, fmt.Errorf("store: create parent dir: %w", err)
		}
	}

	db, err := sql.Open(DriverName, dsn(path))
	if err != nil {
		return nil, fmt.Errorf("store: open sqlite: %w", err)
	}
	// One connection keeps connection-scoped PRAGMAs sticky and serializes
	// writes (SQLite permits a single writer anyway).
	db.SetMaxOpenConns(1)

	// journal_mode is database-scoped and sticky; set it once after opening.
	// busy_timeout and foreign_keys are per-connection and are carried by the
	// DSN (see dsn) so every connection — including any future replacement —
	// gets them.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: PRAGMA journal_mode=WAL: %w", err)
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

// DB exposes the raw *sql.DB so a query-layer Repo can be constructed over the
// same handle (see NewRepo). It returns nil once the store is closed.
func (s *Store) DB() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
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
