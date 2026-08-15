package store

import (
	"context"
	"fmt"
)

// AppendLog appends one activity_log row (level/action/detail). The row is not
// seed-scoped (seed_id is NULL); the M2c method list only exposes this
// non-seed-scoped form.
func (r *Repo) AppendLog(ctx context.Context, level, action, detail string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO activity_log (level, action, detail) VALUES (?, ?, ?)`, level, action, detail)
	if err != nil {
		return fmt.Errorf("store: append activity log: %w", err)
	}
	return nil
}

// AppendLogSeed appends one seed-scoped activity_log row (level/action/detail)
// carrying seed_id.
func (r *Repo) AppendLogSeed(ctx context.Context, seedID int64, level, action, detail string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO activity_log (seed_id, level, action, detail) VALUES (?, ?, ?, ?)`,
		seedID, level, action, detail)
	if err != nil {
		return fmt.Errorf("store: append seed activity log: %w", err)
	}
	return nil
}
