package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/autoseedrelay/relay/internal/secret"
)

// Repo is the query layer over an opened store. It wraps a *sql.DB plus the
// AES-256 master key used to transparently encrypt/decrypt the enc_* columns.
// All methods take a context, use bound parameters (never string concat), and
// return a clear error rather than panicking when credential decryption fails.
type Repo struct {
	db        *sql.DB
	masterKey []byte
}

// NewRepo builds a Repo over an open *sql.DB. masterKey must be a valid AES key
// (32 bytes), as produced by secret.LoadMasterKey; an empty key makes any
// credential write/read fail with an explicit error rather than panic.
func NewRepo(db *sql.DB, masterKey []byte) *Repo {
	return &Repo{db: db, masterKey: masterKey}
}

// DB exposes the underlying handle for callers that need raw access (e.g. health
// checks or transactions owned by higher layers).
func (r *Repo) DB() *sql.DB { return r.db }

// scanner is satisfied by both *sql.Row and *sql.Rows, so one scan helper can
// populate an entity from either a single-row query or a row in a result set.
type scanner interface {
	Scan(dest ...any) error
}

// encrypt seals a plaintext credential. An empty plaintext maps to SQL NULL so
// the round-trip (empty in → empty out) holds and no dummy ciphertext is stored.
func (r *Repo) encrypt(plaintext string) (any, error) {
	if plaintext == "" {
		return nil, nil
	}
	enc, err := secret.Encrypt(r.masterKey, []byte(plaintext))
	if err != nil {
		return nil, fmt.Errorf("store: encrypt credential: %w", err)
	}
	return enc, nil
}

// decrypt opens a ciphertext. Empty input (which is how SQL NULL is read back)
// returns an empty plaintext without touching the crypto layer.
func (r *Repo) decrypt(enc string) (string, error) {
	if enc == "" {
		return "", nil
	}
	plain, err := secret.Decrypt(r.masterKey, enc)
	if err != nil {
		return "", fmt.Errorf("store: decrypt credential: %w", err)
	}
	return string(plain), nil
}

// decryptNull decrypts a nullable credential column, treating NULL as empty.
func (r *Repo) decryptNull(ns sql.NullString) (string, error) {
	if !ns.Valid {
		return "", nil
	}
	return r.decrypt(ns.String)
}

// intOrNull converts the 0-means-NULL convention into a real SQL NULL.
func intOrNull(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// errNotWrapped lets scan helpers surface sql.ErrNoRows untouched while wrapping
// real driver errors with entity context.
func wrapScanErr(op string, err error) error {
	if err == sql.ErrNoRows {
		return err
	}
	return fmt.Errorf("store: %s: %w", op, err)
}

// execResult runs an ExecContext and returns (rowsAffected, error).
func (r *Repo) execResult(ctx context.Context, query string, args ...any) (int64, error) {
	res, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
