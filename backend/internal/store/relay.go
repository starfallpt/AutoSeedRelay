package store

import (
	"context"
	"database/sql"
	"fmt"
)

const relayColumns = `id, seed_id, target_id, role, status, target_torrent_id, attempts,
	last_error, published_at, retired_at, retire_reason, created_at, updated_at`

func (r *Repo) scanRelay(s scanner) (*RelayRecord, error) {
	var rec RelayRecord
	var publishedAt, retiredAt sql.NullInt64
	err := s.Scan(&rec.ID, &rec.SeedID, &rec.TargetID, &rec.Role, &rec.Status,
		&rec.TargetTorrentID, &rec.Attempts, &rec.LastError, &publishedAt, &retiredAt,
		&rec.RetireReason, &rec.CreatedAt, &rec.UpdatedAt)
	if err != nil {
		return nil, wrapScanErr("scan relay record", err)
	}
	rec.PublishedAt = publishedAt.Int64
	rec.RetiredAt = retiredAt.Int64
	return &rec, nil
}

// GetRecord returns the relay record for a (seed, target) pair, or
// sql.ErrNoRows if absent.
func (r *Repo) GetRecord(ctx context.Context, seedID, targetID int64) (*RelayRecord, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+relayColumns+` FROM relay_records WHERE seed_id = ? AND target_id = ?`, seedID, targetID)
	return r.scanRelay(row)
}

// UpsertRecord atomically claims the relay record keyed by UNIQUE
// (seed_id, target_id). It is a pure insert: on a conflict it performs
// ON CONFLICT ... DO NOTHING and reports inserted=false with no error, so
// concurrent callers racing for the same (seed, target) pair get exactly one
// winner. A successful claim assigns the row id back to rec.ID and returns
// inserted=true.
//
// retired_at / retire_reason are intentionally not written here — a freshly
// claimed record is never pre-retired; only MarkRetired stamps them.
func (r *Repo) UpsertRecord(ctx context.Context, rec *RelayRecord) (inserted bool, err error) {
	if rec == nil {
		return false, fmt.Errorf("store: upsert relay record: nil record")
	}
	if err := validateRecordStatus(rec.Status); err != nil {
		return false, fmt.Errorf("store: upsert relay record: %w", err)
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO relay_records
			(seed_id, target_id, role, status, target_torrent_id, attempts,
			 last_error, published_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(seed_id, target_id) DO NOTHING`,
		rec.SeedID, rec.TargetID, rec.Role, rec.Status, rec.TargetTorrentID,
		rec.Attempts, rec.LastError, intOrNull(rec.PublishedAt))
	if err != nil {
		return false, fmt.Errorf("store: upsert relay record: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: upsert relay record: %w", err)
	}
	if n == 0 {
		// (seed_id, target_id) already claimed by someone else.
		return false, nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return false, fmt.Errorf("store: upsert relay record id: %w", err)
	}
	rec.ID = id
	return true, nil
}

// SetRecordRole changes only a record's role column for a (seed, target) pair.
// It is used after a failed claim to degrade the loser to a cross seeder.
// Nothing else (status, attempts, retired_at, retire_reason) is touched.
func (r *Repo) SetRecordRole(ctx context.Context, seedID, targetID int64, role string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE relay_records SET role = ? WHERE seed_id = ? AND target_id = ?`,
		role, seedID, targetID)
	if err != nil {
		return fmt.Errorf("store: set record role: %w", err)
	}
	return nil
}

// MarkPublished marks a record published and stamps published_at with the given
// Unix-second timestamp. published_at is only written when it is not yet set
// (first publish time is preserved), mirroring the once-only publish success
// path. It never touches retired_at / retire_reason — only MarkRetired may
// write those columns.
func (r *Repo) MarkPublished(ctx context.Context, seedID, targetID int64, publishedAt int64) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE relay_records SET
			status = 'published',
			last_error = '',
			published_at = CASE WHEN published_at IS NULL OR published_at = 0 THEN ? ELSE published_at END,
			updated_at = unixepoch()
		WHERE seed_id = ? AND target_id = ?`, publishedAt, seedID, targetID)
	if err != nil {
		return fmt.Errorf("store: mark record published: %w", err)
	}
	return nil
}

// UpdateRecordStatus sets a record's status and error text for a (seed, target)
// pair. It never touches retired_at / retire_reason — only MarkRetired may
// write those columns.
func (r *Repo) UpdateRecordStatus(ctx context.Context, seedID, targetID int64, status, errMsg string) error {
	if err := validateRecordStatus(status); err != nil {
		return fmt.Errorf("store: update record status: %w", err)
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE relay_records SET status = ?, last_error = ?, updated_at = unixepoch()
		WHERE seed_id = ? AND target_id = ?`, status, errMsg, seedID, targetID)
	if err != nil {
		return fmt.Errorf("store: update record status: %w", err)
	}
	return nil
}

// UpdateRecordAttempt bumps a record's attempt counter and records the latest
// status / error in one atomic statement. It never touches retired_at /
// retire_reason — only MarkRetired may write those columns.
func (r *Repo) UpdateRecordAttempt(ctx context.Context, seedID, targetID int64, status, errMsg string) error {
	if err := validateRecordStatus(status); err != nil {
		return fmt.Errorf("store: update record attempt: %w", err)
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE relay_records SET
			attempts = attempts + 1,
			status = ?,
			last_error = ?,
			updated_at = unixepoch()
		WHERE seed_id = ? AND target_id = ?`, status, errMsg, seedID, targetID)
	if err != nil {
		return fmt.Errorf("store: update record attempt: %w", err)
	}
	return nil
}

// MarkRetired stamps retired_at and records the reason for a (seed, target)
// pair. It does not change status; callers that also want a status transition
// can follow up with UpdateRecordStatus.
func (r *Repo) MarkRetired(ctx context.Context, seedID, targetID int64, reason string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE relay_records SET retired_at = unixepoch(), retire_reason = ?, updated_at = unixepoch()
		WHERE seed_id = ? AND target_id = ?`, reason, seedID, targetID)
	if err != nil {
		return fmt.Errorf("store: mark retired: %w", err)
	}
	return nil
}

// ListRecordsBySeed returns every relay record for a seed.
func (r *Repo) ListRecordsBySeed(ctx context.Context, seedID int64) ([]*RelayRecord, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+relayColumns+` FROM relay_records WHERE seed_id = ? ORDER BY id`, seedID)
	if err != nil {
		return nil, fmt.Errorf("store: list records by seed: %w", err)
	}
	defer rows.Close()

	var out []*RelayRecord
	for rows.Next() {
		rec, err := r.scanRelay(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list records by seed: %w", err)
	}
	return out, nil
}
