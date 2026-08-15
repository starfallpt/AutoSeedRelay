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

// UpsertRecord inserts or updates the relay record keyed by UNIQUE
// (seed_id, target_id), assigning the row id back to rec.ID.
func (r *Repo) UpsertRecord(ctx context.Context, rec *RelayRecord) error {
	if rec == nil {
		return fmt.Errorf("store: upsert relay record: nil record")
	}
	var id int64
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO relay_records
			(seed_id, target_id, role, status, target_torrent_id, attempts,
			 last_error, published_at, retired_at, retire_reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(seed_id, target_id) DO UPDATE SET
			role = excluded.role,
			status = excluded.status,
			target_torrent_id = excluded.target_torrent_id,
			attempts = excluded.attempts,
			last_error = excluded.last_error,
			published_at = excluded.published_at,
			retired_at = excluded.retired_at,
			retire_reason = excluded.retire_reason,
			updated_at = unixepoch()
		RETURNING id`,
		rec.SeedID, rec.TargetID, rec.Role, rec.Status, rec.TargetTorrentID,
		rec.Attempts, rec.LastError, intOrNull(rec.PublishedAt), intOrNull(rec.RetiredAt),
		rec.RetireReason).Scan(&id)
	if err != nil {
		return fmt.Errorf("store: upsert relay record: %w", err)
	}
	rec.ID = id
	return nil
}

// UpdateRecordStatus sets a record's status and error text for a (seed, target)
// pair.
func (r *Repo) UpdateRecordStatus(ctx context.Context, seedID, targetID int64, status, errMsg string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE relay_records SET status = ?, last_error = ?, updated_at = unixepoch()
		WHERE seed_id = ? AND target_id = ?`, status, errMsg, seedID, targetID)
	if err != nil {
		return fmt.Errorf("store: update record status: %w", err)
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
