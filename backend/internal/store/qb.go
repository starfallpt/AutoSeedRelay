package store

import (
	"context"
	"database/sql"
	"fmt"
)

const qbColumns = `id, name, host, port, username, enc_password, priority, enabled,
	last_seen_at, extra, created_at, updated_at`

func (r *Repo) scanQB(s scanner) (*QBInstance, error) {
	var q QBInstance
	var encPassword sql.NullString
	var lastSeen sql.NullInt64
	err := s.Scan(&q.ID, &q.Name, &q.Host, &q.Port, &q.Username, &encPassword,
		&q.Priority, &q.Enabled, &lastSeen, &q.Extra, &q.CreatedAt, &q.UpdatedAt)
	if err != nil {
		return nil, wrapScanErr("scan qb instance", err)
	}
	q.LastSeenAt = lastSeen.Int64
	if q.Password, err = r.decryptNull(encPassword); err != nil {
		return nil, err
	}
	return &q, nil
}

// GetEnabledQBInstances returns every enabled qB instance, highest priority
// first (priority DESC, id ASC as a stable tiebreak).
func (r *Repo) GetEnabledQBInstances(ctx context.Context) ([]*QBInstance, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+qbColumns+` FROM qb_instances WHERE enabled = 1 ORDER BY priority DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list enabled qb instances: %w", err)
	}
	defer rows.Close()

	var out []*QBInstance
	for rows.Next() {
		q, err := r.scanQB(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list enabled qb instances: %w", err)
	}
	return out, nil
}

// UpsertQBInstance inserts a new qB instance when q.ID == 0 (assigning the
// generated id back to q) or updates the row with q.ID otherwise. The password
// is encrypted on write.
func (r *Repo) UpsertQBInstance(ctx context.Context, q *QBInstance) error {
	if q == nil {
		return fmt.Errorf("store: upsert qb instance: nil qb instance")
	}
	password, err := r.encrypt(q.Password)
	if err != nil {
		return err
	}

	if q.ID == 0 {
		res, err := r.db.ExecContext(ctx, `
			INSERT INTO qb_instances
				(name, host, port, username, enc_password, priority, enabled, last_seen_at, extra)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			q.Name, q.Host, q.Port, q.Username, password, q.Priority, q.Enabled,
			intOrNull(q.LastSeenAt), q.Extra)
		if err != nil {
			return fmt.Errorf("store: insert qb instance: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: insert qb instance id: %w", err)
		}
		q.ID = id
		return nil
	}

	_, err = r.db.ExecContext(ctx, `
		UPDATE qb_instances SET
			name = ?, host = ?, port = ?, username = ?, enc_password = ?,
			priority = ?, enabled = ?, last_seen_at = ?, extra = ?,
			updated_at = unixepoch()
		WHERE id = ?`,
		q.Name, q.Host, q.Port, q.Username, password, q.Priority, q.Enabled,
		intOrNull(q.LastSeenAt), q.Extra, q.ID)
	if err != nil {
		return fmt.Errorf("store: update qb instance %d: %w", q.ID, err)
	}
	return nil
}

// TouchQBSeen stamps last_seen_at with the current Unix time.
func (r *Repo) TouchQBSeen(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE qb_instances SET last_seen_at = unixepoch() WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: touch qb instance %d: %w", id, err)
	}
	return nil
}
