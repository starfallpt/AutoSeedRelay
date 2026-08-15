package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autoseedrelay/relay/internal/store"
	_ "modernc.org/sqlite"
)

// openRaw opens a raw *sql.DB handle for seeding/verifying data. The schema is
// created separately via store.Open (the store package owns migrations).
func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open(store.DriverName, path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	return db
}

func TestExportRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data", "relay.db")

	// Create and migrate the schema.
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	// Seed data through a raw handle (schema already migrated).
	db := openRaw(t, dbPath)
	if _, err := db.Exec(`INSERT INTO sources (name, base_url) VALUES ('s1','https://a'), ('s2','https://b')`); err != nil {
		db.Close()
		t.Fatalf("seed: %v", err)
	}

	// Export.
	var buf bytes.Buffer
	if err := Export(ctx, db, &buf); err != nil {
		db.Close()
		t.Fatalf("Export: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	// Verify archive contents.
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names[dbFileName] || !names[metaFileName] {
		t.Fatalf("archive entries = %v, want %q and %q", names, dbFileName, metaFileName)
	}
	var m meta
	for _, f := range zr.File {
		if f.Name != metaFileName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open meta.json: %v", err)
		}
		if err := json.NewDecoder(rc).Decode(&m); err != nil {
			rc.Close()
			t.Fatalf("decode meta.json: %v", err)
		}
		rc.Close()
	}
	current, err := store.CurrentVersion()
	if err != nil {
		t.Fatalf("store.CurrentVersion: %v", err)
	}
	if m.SchemaVersion != current {
		t.Fatalf("schema_version = %d, want %d", m.SchemaVersion, current)
	}

	// Corrupt the original data.
	db2 := openRaw(t, dbPath)
	if _, err := db2.Exec(`DELETE FROM sources`); err != nil {
		db2.Close()
		t.Fatalf("corrupt: %v", err)
	}
	if err := db2.Close(); err != nil {
		t.Fatalf("close corrupt db: %v", err)
	}

	// Restore from the archive.
	if err := Restore(ctx, dbPath, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Assert data recovered.
	db3 := openRaw(t, dbPath)
	var n int
	if err := db3.QueryRow(`SELECT COUNT(*) FROM sources`).Scan(&n); err != nil {
		db3.Close()
		t.Fatalf("count after restore: %v", err)
	}
	db3.Close()
	if n != 2 {
		t.Fatalf("expected 2 sources after restore, got %d", n)
	}

	// Auto-backup file must exist.
	backupsDir := filepath.Join(filepath.Dir(dbPath), "backups")
	entries, err := os.ReadDir(backupsDir)
	if err != nil {
		t.Fatalf("read backups dir: %v", err)
	}
	found := false
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "auto-") && strings.HasSuffix(e.Name(), ".db") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no auto-*.db backup found in %s", backupsDir)
	}
}

func TestRestoreRejectsCorruptArchive(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "relay.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.Close()

	if err := Restore(ctx, dbPath, bytes.NewReader([]byte("definitely not a zip"))); err == nil {
		t.Fatal("expected error for non-zip input, got nil")
	}

	// Valid zip but missing relay.db.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create(metaFileName)
	_, _ = w.Write([]byte(`{"schema_version":1,"app_version":"x","exported_at":"y"}`))
	_ = zw.Close()
	if err := Restore(ctx, dbPath, bytes.NewReader(buf.Bytes())); err == nil {
		t.Fatal("expected error for archive missing relay.db, got nil")
	}
}

func TestRestoreRejectsNewerVersion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "relay.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.Close()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create(metaFileName)
	_, _ = w.Write([]byte(`{"schema_version":999999,"app_version":"x","exported_at":"y"}`))
	w2, _ := zw.Create(dbFileName)
	_, _ = w2.Write([]byte("junk"))
	_ = zw.Close()

	if err := Restore(ctx, dbPath, bytes.NewReader(buf.Bytes())); err == nil {
		t.Fatal("expected error for too-new schema version, got nil")
	}
}

func TestRestoreRejectsNewerDBVersion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "relay.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.Close()

	// Build a standalone, structurally valid SQLite db with an inflated
	// user_version (meta.json advertises a compatible version, so the db-level
	// check is what must reject it).
	srcPath := filepath.Join(dir, "toohigh.db")
	db, err := sql.Open(store.DriverName, srcPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE t (x)`); err != nil {
		db.Close()
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 999999`); err != nil {
		db.Close()
		t.Fatalf("set user_version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	data, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read db: %v", err)
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create(metaFileName)
	_, _ = w.Write([]byte(`{"schema_version":1,"app_version":"x","exported_at":"y"}`))
	w2, _ := zw.Create(dbFileName)
	_, _ = w2.Write(data)
	_ = zw.Close()

	if err := Restore(ctx, dbPath, bytes.NewReader(buf.Bytes())); err == nil {
		t.Fatal("expected error for too-new relay.db user_version, got nil")
	}
}

func TestWriteZipFileRejectsOversizedEntry(t *testing.T) {
	// Forge an entry whose declared uncompressed size exceeds the cap, without
	// allocating a real multi-hundred-MB file.
	f := &zip.File{}
	f.Name = dbFileName
	f.UncompressedSize64 = maxDBBytes + 1

	dst, err := os.Create(filepath.Join(t.TempDir(), "out.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	if err := writeZipFile(f, dst); err == nil {
		t.Fatal("writeZipFile() = nil, want oversized-entry error")
	}
}

// buildStandaloneDB creates a plain (non-WAL) SQLite file with a table, some
// payload blobs, and user_version=1, and returns its raw bytes. It is used to
// forge relay.db entries whose header stays valid while their body is later
// corrupted or truncated.
func buildStandaloneDB(t *testing.T, dir, name string, blobs int) []byte {
	t.Helper()
	path := filepath.Join(dir, name)
	db, err := sql.Open(store.DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE t (x)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	for i := 0; i < blobs; i++ {
		if _, err := db.Exec(`INSERT INTO t (x) VALUES (randomblob(4000))`); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRestoreRejectsIntegrityCheckFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "relay.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.Close()

	// A structurally valid header with a corrupted body page: user_version reads
	// fine (header untouched), but PRAGMA integrity_check must fail.
	bad := buildStandaloneDB(t, dir, "bad.db", 50)
	start := len(bad) / 2
	for i := start; i < len(bad) && i < start+512; i++ {
		bad[i] ^= 0xFF
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create(metaFileName)
	_, _ = w.Write([]byte(`{"schema_version":1,"app_version":"x","exported_at":"y"}`))
	w2, _ := zw.Create(dbFileName)
	_, _ = w2.Write(bad)
	_ = zw.Close()

	err = Restore(ctx, dbPath, bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Fatal("expected integrity-check rejection for corrupt relay.db, got nil")
	}
	if !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("expected integrity-check error, got: %v", err)
	}
}

func TestRestoreRejectsTruncatedDB(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "relay.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.Close()

	// meta.json is valid, but relay.db is truncated to just past its header.
	trunc := buildStandaloneDB(t, dir, "trunc.db", 0)
	if len(trunc) > 600 {
		trunc = trunc[:600]
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create(metaFileName)
	_, _ = w.Write([]byte(`{"schema_version":1,"app_version":"x","exported_at":"y"}`))
	w2, _ := zw.Create(dbFileName)
	_, _ = w2.Write(trunc)
	_ = zw.Close()

	if err := Restore(ctx, dbPath, bytes.NewReader(buf.Bytes())); err == nil {
		t.Fatal("expected rejection for truncated relay.db, got nil")
	}
}

func TestAutoBackupSkipsWhenNoExistingDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "relay.db")

	if err := autoBackup(dbPath); err != nil {
		t.Fatalf("autoBackup(no existing db) = %v, want nil", err)
	}
	// The backups dir must not even be created when there is nothing to back up.
	if _, err := os.Stat(filepath.Join(dir, "backups")); !os.IsNotExist(err) {
		t.Fatalf("backups dir should not exist, stat err = %v", err)
	}
}
