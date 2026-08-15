package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// MarkSeen records that (sourceSite, infoHash) has been observed by the poller.
// It is a permanent tombstone: INSERT OR IGNORE makes it idempotent and never
// overwrites the original first_seen_at. Unlike a seeds row, a seen_hashes row
// is never deleted by user cleanup, so a hash whose seed row was removed — or
// lost to a stale backup restore — is never re-enqueued by the poller.
func (r *Repo) MarkSeen(ctx context.Context, sourceSite, infoHash string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO seen_hashes (source_site, info_hash) VALUES (?, ?)`,
		sourceSite, infoHash)
	if err != nil {
		return fmt.Errorf("store: mark seen: %w", err)
	}
	return nil
}

// HasSeen reports whether (sourceSite, infoHash) has already been tombstoned.
func (r *Repo) HasSeen(ctx context.Context, sourceSite, infoHash string) (bool, error) {
	var one int
	err := r.db.QueryRowContext(ctx,
		`SELECT 1 FROM seen_hashes WHERE source_site = ? AND info_hash = ?`,
		sourceSite, infoHash).Scan(&one)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	default:
		return false, fmt.Errorf("store: has seen: %w", err)
	}
}
