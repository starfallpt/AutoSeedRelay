// Package backup exports and restores the AutoSeedRelay database as a zip
// archive (docs/BIZ-SPEC.md §12).
//
// The archive always contains two entries:
//
//	relay.db   a self-contained SQLite snapshot produced by VACUUM INTO
//	meta.json  schema version, application version, and export timestamp
//
// Restore validates the archive, verifies the embedded database is healthy and
// not newer than this binary's schema, snapshots the current database into
// data/backups/auto-<timestamp>.db as a safety net, then atomically replaces
// the live database and re-opens it to confirm the swap worked.
//
// Callers that hold an open *store.Store must Close it before calling Restore:
// closing the store checkpoints WAL into the main file (so the auto-backup copy
// is self-contained) and releases the file lock that would otherwise make the
// final os.Rename fail on Windows.
package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/autoseedrelay/relay/internal/store"
	sqlite "modernc.org/sqlite"
)

const (
	// dbFileName is the name of the SQLite snapshot inside the archive.
	dbFileName = "relay.db"
	// metaFileName is the name of the metadata entry inside the archive.
	metaFileName = "meta.json"
	// maxMetaSize caps meta.json to guard against zip bombs in a restore path.
	maxMetaSize = 1 << 20 // 1 MiB
)

// AppVersion is the backend release version recorded in backup metadata.
//
// It must be kept in sync with internal/server.Version until the two are moved
// into a shared version package (the backup package cannot import the server
// package without creating a future import cycle once the web layer exposes
// backup endpoints).
const AppVersion = "0.1.0-m0"

// meta is the JSON metadata payload written to meta.json.
type meta struct {
	SchemaVersion int    `json:"schema_version"`
	AppVersion    string `json:"app_version"`
	ExportedAt    string `json:"exported_at"`
}

// Export writes a zip archive of the database pointed to by db to w. The
// snapshot is taken online (consistent even if other readers are active) and
// packaged together with meta.json.
func Export(ctx context.Context, db *sql.DB, w io.Writer) error {
	if db == nil {
		return errors.New("backup: nil database handle")
	}
	if w == nil {
		return errors.New("backup: nil writer")
	}

	// Record the source schema version before snapshotting.
	var schemaVersion int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&schemaVersion); err != nil {
		return fmt.Errorf("backup: read source schema version: %w", err)
	}

	// Snapshot the database into a temporary file (same temp dir is fine; it is
	// only read back into the archive).
	tmp, err := os.CreateTemp("", "relay-export-*.db")
	if err != nil {
		return fmt.Errorf("backup: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("backup: close temp file: %w", err)
	}
	defer os.Remove(tmpPath)

	if err := snapshotDB(ctx, db, tmpPath); err != nil {
		return err
	}

	zw := zip.NewWriter(w)
	if err := addFileToZip(zw, dbFileName, tmpPath); err != nil {
		zw.Close()
		return fmt.Errorf("backup: add %s: %w", dbFileName, err)
	}
	metaBytes, err := json.MarshalIndent(meta{
		SchemaVersion: schemaVersion,
		AppVersion:    AppVersion,
		ExportedAt:    time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		zw.Close()
		return fmt.Errorf("backup: marshal meta: %w", err)
	}
	if err := addBytesToZip(zw, metaFileName, metaBytes); err != nil {
		zw.Close()
		return fmt.Errorf("backup: add %s: %w", metaFileName, err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("backup: finalize archive: %w", err)
	}
	return nil
}

// Restore validates r as a backup archive and, if acceptable, replaces the
// database at dbPath with the snapshot inside it. The current database is
// copied to data/backups/auto-<timestamp>.db first.
func Restore(ctx context.Context, dbPath string, r io.Reader) error {
	if dbPath == "" {
		dbPath = store.DefaultDBPath
	}
	if r == nil {
		return errors.New("backup: nil reader")
	}

	// zip.Reader needs random access, so buffer the whole archive.
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("backup: read archive: %w", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("backup: invalid zip archive: %w", err)
	}

	var metaFile, dbFile *zip.File
	for _, f := range zr.File {
		switch f.Name {
		case metaFileName:
			metaFile = f
		case dbFileName:
			dbFile = f
		}
	}
	if metaFile == nil || dbFile == nil {
		return fmt.Errorf("backup: archive must contain %q and %q", metaFileName, dbFileName)
	}

	// Validate meta.json and its schema version before touching anything.
	var m meta
	if err := readZipJSON(metaFile, &m); err != nil {
		return err
	}
	current, err := store.CurrentVersion()
	if err != nil {
		return fmt.Errorf("backup: determine current schema version: %w", err)
	}
	if m.SchemaVersion < 1 {
		return fmt.Errorf("backup: archive schema version %d is invalid", m.SchemaVersion)
	}
	if m.SchemaVersion > current {
		return fmt.Errorf("backup: archive schema version %d is newer than supported %d", m.SchemaVersion, current)
	}

	// Extract relay.db into a temporary file inside the target directory so the
	// final rename stays on one volume (atomic on Windows).
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("backup: create db directory: %w", err)
	}
	tmpFile, err := os.CreateTemp(dir, ".relay-restore-*.db")
	if err != nil {
		return fmt.Errorf("backup: create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)
	if err := writeZipFile(dbFile, tmpFile); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("backup: close temp file: %w", err)
	}

	// Validate the extracted database before replacing the current one.
	if err := validateRestoredDB(ctx, tmpPath, current); err != nil {
		return err
	}

	// Safety net: snapshot the current database before overwriting it.
	if err := autoBackup(dbPath); err != nil {
		return err
	}

	// Atomically swap the validated snapshot into place. The caller must have
	// closed its store handle first (see package doc).
	if err := os.Rename(tmpPath, dbPath); err != nil {
		return fmt.Errorf("backup: replace database: %w", err)
	}

	// Re-open the restored database to confirm it is healthy.
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("backup: reopen restored database: %w", err)
	}
	defer st.Close()
	if ver, err := st.MigrateVersion(); err != nil {
		return fmt.Errorf("backup: read restored schema version: %w", err)
	} else if ver > current {
		return fmt.Errorf("backup: restored schema version %d is newer than supported %d", ver, current)
	}
	return nil
}

// snapshotDB writes a consistent online snapshot of db to destPath. It prefers
// VACUUM INTO (SQLite >= 3.27) and falls back to the driver's online backup
// API (the sqlite3_backup equivalent of the shell .backup command) when VACUUM
// INTO is unsupported or fails.
func snapshotDB(ctx context.Context, db *sql.DB, destPath string) error {
	var vacuumErr, backupErr error
	if err := vacuumInto(ctx, db, destPath); err == nil {
		return nil
	} else {
		vacuumErr = err
	}
	if err := backupViaDriver(ctx, db, destPath); err == nil {
		return nil
	} else {
		backupErr = err
	}
	return fmt.Errorf("backup: snapshot failed: VACUUM INTO: %v; online backup: %v", vacuumErr, backupErr)
}

// vacuumInto runs VACUUM INTO with destPath as the target. The path is a
// SQL string literal (VACUUM INTO takes an expression rather than a bound
// parameter in some SQLite builds), quoted against embedded single quotes.
func vacuumInto(ctx context.Context, db *sql.DB, destPath string) error {
	stmt := "VACUUM INTO " + quoteSQLString(destPath)
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("backup: VACUUM INTO: %w", err)
	}
	return nil
}

// backuper matches the online-backup capability exposed by the modernc sqlite
// driver connection (see modernc.org/sqlite (*conn).NewBackup).
type backuper interface {
	NewBackup(string) (*sqlite.Backup, error)
}

// backupViaDriver copies the database to destPath using the driver's online
// backup API. It is the fallback when VACUUM INTO is unavailable.
func backupViaDriver(ctx context.Context, db *sql.DB, destPath string) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("backup: acquire connection: %w", err)
	}
	defer conn.Close()
	return conn.Raw(func(driverConn any) error {
		bu, ok := driverConn.(backuper)
		if !ok {
			return errors.New("driver does not expose sqlite3 online backup")
		}
		b, err := bu.NewBackup(destPath)
		if err != nil {
			return err
		}
		for {
			more, err := b.Step(-1)
			if err != nil {
				b.Finish()
				return err
			}
			if !more {
				break
			}
		}
		return b.Finish()
	})
}

// validateRestoredDB opens path and checks that it passes PRAGMA
// integrity_check and that its user_version is not newer than current.
func validateRestoredDB(ctx context.Context, path string, current int) error {
	db, err := sql.Open(store.DriverName, path)
	if err != nil {
		return fmt.Errorf("backup: open restored database: %w", err)
	}
	defer db.Close()

	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("backup: read restored schema version: %w", err)
	}
	if version > current {
		return fmt.Errorf("backup: restored schema version %d is newer than supported %d", version, current)
	}

	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return fmt.Errorf("backup: integrity check: %w", err)
	}
	defer rows.Close()
	var problems []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return fmt.Errorf("backup: integrity check scan: %w", err)
		}
		if s != "ok" {
			problems = append(problems, s)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("backup: integrity check rows: %w", err)
	}
	if len(problems) > 0 {
		return fmt.Errorf("backup: integrity check failed: %s", strings.Join(problems, "; "))
	}
	return nil
}

// autoBackup copies the current database file into
// <db-dir>/backups/auto-<timestamp>.db. A missing current database (fresh
// install) is not an error.
func autoBackup(dbPath string) error {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("backup: stat current db: %w", err)
	}

	dir := filepath.Join(filepath.Dir(dbPath), "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("backup: create backups dir: %w", err)
	}
	dst := filepath.Join(dir, "auto-"+time.Now().UTC().Format("20060102-150405.000000000")+".db")
	if err := copyFile(dbPath, dst); err != nil {
		return fmt.Errorf("backup: auto-backup copy: %w", err)
	}
	return nil
}

// --- small helpers ---

func addFileToZip(zw *zip.Writer, name, path string) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, src)
	return err
}

func addBytesToZip(zw *zip.Writer, name string, data []byte) error {
	dst, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = dst.Write(data)
	return err
}

func readZipJSON(f *zip.File, v any) error {
	if f.UncompressedSize64 > maxMetaSize {
		return fmt.Errorf("backup: %s is too large (%d bytes)", f.Name, f.UncompressedSize64)
	}
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("backup: open %s: %w", f.Name, err)
	}
	defer rc.Close()
	if err := json.NewDecoder(io.LimitReader(rc, maxMetaSize+1)).Decode(v); err != nil {
		return fmt.Errorf("backup: parse %s: %w", f.Name, err)
	}
	return nil
}

func writeZipFile(f *zip.File, dst *os.File) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("backup: open %s: %w", f.Name, err)
	}
	defer rc.Close()
	if _, err := io.Copy(dst, rc); err != nil {
		return fmt.Errorf("backup: extract %s: %w", f.Name, err)
	}
	return nil
}

func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	_, err = io.Copy(out, in)
	return err
}

func quoteSQLString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
