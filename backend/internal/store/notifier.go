package store

import (
	"context"
	"database/sql"
	"fmt"
)

const notifierColumns = `id, type, name, enc_config, enabled, created_at, updated_at`

func (r *Repo) scanNotifier(s scanner) (*NotifierInstance, error) {
	var n NotifierInstance
	var encConfig sql.NullString
	err := s.Scan(&n.ID, &n.Type, &n.Name, &encConfig, &n.Enabled, &n.CreatedAt, &n.UpdatedAt)
	if err != nil {
		return nil, wrapScanErr("scan notifier instance", err)
	}
	if n.Config, err = r.decryptNull(encConfig); err != nil {
		return nil, err
	}
	return &n, nil
}

// GetNotifierInstances returns notifier instances; when enabledOnly is true it
// returns only enabled ones.
func (r *Repo) GetNotifierInstances(ctx context.Context, enabledOnly bool) ([]*NotifierInstance, error) {
	query := `SELECT ` + notifierColumns + ` FROM notifier_instances`
	if enabledOnly {
		query += ` WHERE enabled = 1`
	}
	query += ` ORDER BY id`

	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("store: list notifier instances: %w", err)
	}
	defer rows.Close()

	var out []*NotifierInstance
	for rows.Next() {
		n, err := r.scanNotifier(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list notifier instances: %w", err)
	}
	return out, nil
}

// GetRoutes returns the notifier_routes rows for one tier (all of them,
// including disabled, so the config matrix can read back its state; callers
// filter by Route.Enabled when dispatching).
func (r *Repo) GetRoutes(ctx context.Context, tier string) ([]*Route, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT instance_id, tier, enabled FROM notifier_routes WHERE tier = ? ORDER BY instance_id`, tier)
	if err != nil {
		return nil, fmt.Errorf("store: list notifier routes: %w", err)
	}
	defer rows.Close()

	var out []*Route
	for rows.Next() {
		var rt Route
		if err := rows.Scan(&rt.InstanceID, &rt.Tier, &rt.Enabled); err != nil {
			return nil, fmt.Errorf("store: scan notifier route: %w", err)
		}
		out = append(out, &rt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list notifier routes: %w", err)
	}
	return out, nil
}

// UpsertNotifierInstance inserts a new instance when n.ID == 0 (assigning the
// generated id back to n) or updates the row with n.ID otherwise. Config is
// encrypted on write.
func (r *Repo) UpsertNotifierInstance(ctx context.Context, n *NotifierInstance) error {
	if n == nil {
		return fmt.Errorf("store: upsert notifier instance: nil instance")
	}
	config, err := r.encrypt(n.Config)
	if err != nil {
		return err
	}

	if n.ID == 0 {
		res, err := r.db.ExecContext(ctx, `
			INSERT INTO notifier_instances (type, name, enc_config, enabled)
			VALUES (?, ?, ?, ?)`, n.Type, n.Name, config, n.Enabled)
		if err != nil {
			return fmt.Errorf("store: insert notifier instance: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: insert notifier instance id: %w", err)
		}
		n.ID = id
		return nil
	}

	_, err = r.db.ExecContext(ctx, `
		UPDATE notifier_instances SET type = ?, name = ?, enc_config = ?, enabled = ?,
			updated_at = unixepoch()
		WHERE id = ?`, n.Type, n.Name, config, n.Enabled, n.ID)
	if err != nil {
		return fmt.Errorf("store: update notifier instance %d: %w", n.ID, err)
	}
	return nil
}

// UpsertNotifierRoute inserts or updates the route keyed by UNIQUE
// (instance_id, tier).
func (r *Repo) UpsertNotifierRoute(ctx context.Context, rt *Route) error {
	if rt == nil {
		return fmt.Errorf("store: upsert notifier route: nil route")
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO notifier_routes (instance_id, tier, enabled)
		VALUES (?, ?, ?)
		ON CONFLICT(instance_id, tier) DO UPDATE SET enabled = excluded.enabled`,
		rt.InstanceID, rt.Tier, rt.Enabled)
	if err != nil {
		return fmt.Errorf("store: upsert notifier route: %w", err)
	}
	return nil
}
