package backup

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/autoseedrelay/relay/internal/store"
)

// sqliteHeader is the 16-byte magic every valid SQLite database file begins
// with (see https://www.sqlite.org/fileformat.html).
var sqliteHeader = []byte("SQLite format 3\x00")

// ValidateZip performs a *light* validation of a backup archive suitable for
// the restore upload path: the ops API calls it before persisting an upload to
// <dataDir>/restore-pending.zip so a clearly-broken archive is rejected up
// front, while the full integrity_check / auto-backup / atomic-replace work is
// deferred to backup.Restore (run by the startup hook on the next boot).
//
// The archive must:
//   - be a well-formed zip containing both "relay.db" and "meta.json";
//   - carry a meta.json whose schema_version is in [1, store.CurrentVersion()];
//   - contain a relay.db entry that begins with the SQLite file magic.
//
// It is intentionally lighter than Restore: no decompression of the whole
// database, no PRAGMA integrity_check, and no filesystem side effects.
func ValidateZip(r io.Reader) error {
	if r == nil {
		return errors.New("backup: nil reader")
	}

	data, err := io.ReadAll(io.LimitReader(r, MaxBackupBytes+1))
	if err != nil {
		return fmt.Errorf("backup: read archive: %w", err)
	}
	if int64(len(data)) > MaxBackupBytes {
		return fmt.Errorf("backup: archive exceeds %d bytes", MaxBackupBytes)
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

	if err := checkSQLiteHeader(dbFile); err != nil {
		return err
	}
	return nil
}

// checkSQLiteHeader verifies that a zip entry begins with the SQLite file
// magic, without decompressing it beyond the header.
func checkSQLiteHeader(f *zip.File) error {
	if f.UncompressedSize64 > maxDBBytes {
		return fmt.Errorf("backup: %s is too large (%d bytes)", f.Name, f.UncompressedSize64)
	}
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("backup: open %s: %w", f.Name, err)
	}
	defer rc.Close()

	hdr := make([]byte, len(sqliteHeader))
	if _, err := io.ReadFull(rc, hdr); err != nil {
		return fmt.Errorf("backup: read %s header: %w", f.Name, err)
	}
	if !bytes.Equal(hdr, sqliteHeader) {
		return fmt.Errorf("backup: %s is not a valid SQLite database", f.Name)
	}
	return nil
}
