package store

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migration is a single SQL migration file: its version (parsed from the
// "NNNNN_" filename prefix) and its raw SQL body.
type migration struct {
	version int
	name    string
	sql     string
}

// migrate applies every pending migration found in dir, sorted by the numeric
// version encoded in each filename, and bumps PRAGMA user_version to the
// highest applied version. Each migration runs in its own transaction so a
// failure never leaves a half-applied schema — the version bump commits or
// rolls back together with the DDL.
//
// The binary embeds its migrations (see migrationsFS) and applies them through
// migrateFS; this directory-based entry point exists for tests and tooling
// that keep migrations on disk.
func migrate(dir string, db *sql.DB) error {
	return migrateFS(os.DirFS(dir), db)
}

// migrateEmbedded applies the migrations embedded in the binary at build time.
// The embed pattern keeps a "migrations/" prefix, so it is rooted at that
// subdirectory before handing off to migrateFS.
func migrateEmbedded(db *sql.DB) error {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("store: embedded migrations: %w", err)
	}
	return migrateFS(sub, db)
}

// migrateFS applies pending migrations from fsys (sorted by filename) whose
// version is greater than the current PRAGMA user_version.
func migrateFS(fsys fs.FS, db *sql.DB) error {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return fmt.Errorf("store: read migrations: %w", err)
	}
	var migs []migration
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		ver, err := parseVersion(name)
		if err != nil {
			return fmt.Errorf("store: migration filename %q: %w", name, err)
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("store: read migration %q: %w", name, err)
		}
		migs = append(migs, migration{version: ver, name: name, sql: string(body)})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })

	var current int
	if err := db.QueryRow("PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("store: read user_version: %w", err)
	}
	for _, m := range migs {
		if m.version <= current {
			continue
		}
		if err := applyMigration(db, m); err != nil {
			return fmt.Errorf("store: apply %s: %w", m.name, err)
		}
		current = m.version
	}
	return nil
}

// applyMigration runs one migration's statements and sets user_version in a
// single transaction.
func applyMigration(db *sql.DB, m migration) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after a successful Commit
	for _, stmt := range splitStatements(m.sql) {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return err
	}
	return tx.Commit()
}

// splitStatements splits a SQL script on top-level semicolons, ignoring
// semicolons inside single-quoted string literals and SQL line comments.
func splitStatements(script string) []string {
	var out []string
	var cur strings.Builder
	for i := 0; i < len(script); {
		c := script[i]
		// "--" line comment: copy verbatim up to (not including) the newline.
		if c == '-' && i+1 < len(script) && script[i+1] == '-' {
			j := strings.IndexByte(script[i:], '\n')
			if j < 0 {
				cur.WriteString(script[i:])
				i = len(script)
			} else {
				cur.WriteString(script[i : i+j])
				i += j
			}
			continue
		}
		// Single-quoted string literal, honoring '' escapes.
		if c == '\'' {
			cur.WriteByte(c)
			i++
			for i < len(script) {
				cur.WriteByte(script[i])
				if script[i] == '\'' {
					if i+1 < len(script) && script[i+1] == '\'' {
						cur.WriteByte(script[i+1])
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			continue
		}
		if c == ';' {
			if s := strings.TrimSpace(cur.String()); s != "" {
				out = append(out, s)
			}
			cur.Reset()
			i++
			continue
		}
		cur.WriteByte(c)
		i++
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// parseVersion returns the leading integer of a "NNNNN_name.sql" filename.
func parseVersion(name string) (int, error) {
	i := strings.IndexByte(name, '_')
	if i <= 0 {
		return 0, fmt.Errorf("missing \"NNNNN_\" version prefix")
	}
	v, err := strconv.Atoi(name[:i])
	if err != nil {
		return 0, fmt.Errorf("non-numeric version prefix %q", name[:i])
	}
	if v < 1 {
		return 0, fmt.Errorf("version must be >= 1")
	}
	return v, nil
}
