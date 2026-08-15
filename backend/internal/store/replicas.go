package store

import (
	"context"
	"fmt"
)

const replicaColumns = `id, seed_id, qb_id, info_hash, role, status, progress, added_at`

func (r *Repo) scanReplica(s scanner) (*Replica, error) {
	var rep Replica
	err := s.Scan(&rep.ID, &rep.SeedID, &rep.QBID, &rep.InfoHash, &rep.Role,
		&rep.Status, &rep.Progress, &rep.AddedAt)
	if err != nil {
		return nil, wrapScanErr("scan replica", err)
	}
	return &rep, nil
}

// ListReplicas returns every replica for a seed.
func (r *Repo) ListReplicas(ctx context.Context, seedID int64) ([]*Replica, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+replicaColumns+` FROM seed_replicas WHERE seed_id = ? ORDER BY id`, seedID)
	if err != nil {
		return nil, fmt.Errorf("store: list replicas: %w", err)
	}
	defer rows.Close()

	var out []*Replica
	for rows.Next() {
		rep, err := r.scanReplica(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rep)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list replicas: %w", err)
	}
	return out, nil
}

// UpsertReplica inserts or updates the replica keyed by UNIQUE
// (seed_id, qb_id, role), assigning the row id back to rep.ID.
func (r *Repo) UpsertReplica(ctx context.Context, rep *Replica) error {
	if rep == nil {
		return fmt.Errorf("store: upsert replica: nil replica")
	}
	var id int64
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO seed_replicas (seed_id, qb_id, info_hash, role, status, progress)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(seed_id, qb_id, role) DO UPDATE SET
			info_hash = excluded.info_hash,
			status = excluded.status,
			progress = excluded.progress
		RETURNING id`,
		rep.SeedID, rep.QBID, rep.InfoHash, rep.Role, rep.Status, rep.Progress).Scan(&id)
	if err != nil {
		return fmt.Errorf("store: upsert replica: %w", err)
	}
	rep.ID = id
	return nil
}

// UpdateReplicaProgress sets a replica's progress (0..1).
func (r *Repo) UpdateReplicaProgress(ctx context.Context, id int64, progress float64) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE seed_replicas SET progress = ? WHERE id = ?`, progress, id)
	if err != nil {
		return fmt.Errorf("store: update replica %d progress: %w", id, err)
	}
	return nil
}

// DeleteReplica removes a replica by id.
func (r *Repo) DeleteReplica(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM seed_replicas WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete replica %d: %w", id, err)
	}
	return nil
}
