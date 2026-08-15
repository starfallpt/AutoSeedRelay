package store

import (
	"fmt"
)

// migrate applies pending schema migrations keyed on PRAGMA user_version.
//
// Version history:
//
//	0  –  initial: relay_jobs only
//	1  –  v3: seeds, relay_records, activity_log
//
// Each migration is idempotent (CREATE TABLE IF NOT EXISTS /
// CREATE INDEX IF NOT EXISTS). To add a future migration wrap it in a
// "if version < N" block and bump PRAGMA user_version.
func (s *RelayStore) migrate() error {
	db := s.db
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("store: read user_version: %w", err)
	}

	if version < 1 {
		for _, ddl := range []string{
			`CREATE TABLE IF NOT EXISTS seeds (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				info_hash TEXT NOT NULL,
				source_site TEXT NOT NULL DEFAULT 'SOURCE',
				source_id INTEGER,
				title TEXT,
				size INTEGER,
				category TEXT,
				promotion TEXT,
				publish_time TEXT,
				discovered_at TEXT,
				downloaded_at TEXT,
				status TEXT NOT NULL DEFAULT 'discovered',
				error_msg TEXT,
				created_at TEXT DEFAULT (datetime('now')),
				updated_at TEXT DEFAULT (datetime('now'))
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_info_hash ON seeds(source_site, info_hash)`,
			`CREATE TABLE IF NOT EXISTS relay_records (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				seed_id INTEGER REFERENCES seeds(id),
				target_site TEXT NOT NULL,
				target_id INTEGER,
				target_hash TEXT,
				role TEXT NOT NULL DEFAULT 'seeder',
				published_at TEXT,
				cross_seeded_at TEXT,
				status TEXT NOT NULL DEFAULT 'pending',
				seeders INTEGER DEFAULT 0,
				leechers INTEGER DEFAULT 0,
				last_check_at TEXT,
				retired_at TEXT,
				retire_reason TEXT,
				created_at TEXT DEFAULT (datetime('now')),
				updated_at TEXT DEFAULT (datetime('now')),
				UNIQUE(seed_id, target_site)
			)`,
			`CREATE TABLE IF NOT EXISTS activity_log (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				seed_id INTEGER,
				target_site TEXT,
				action TEXT NOT NULL,
				detail TEXT,
				created_at TEXT DEFAULT (datetime('now'))
			)`,
			`CREATE INDEX IF NOT EXISTS idx_log_seed ON activity_log(seed_id)`,
		} {
			if _, err := db.Exec(ddl); err != nil {
				return fmt.Errorf("store: migrate v0->v1: %w", err)
			}
		}
		if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
			return fmt.Errorf("store: set user_version=1: %w", err)
		}
	}

	return nil
}
