package store

import (
	"context"
	"database/sql"
	"fmt"
)

const seedColumns = `id, source_site, info_hash, title, size, category, promotion,
	source_id, status, error, retry_count, discovered_at, updated_at`

func (r *Repo) scanSeed(s scanner) (*Seed, error) {
	var sd Seed
	var sourceID sql.NullInt64
	err := s.Scan(&sd.ID, &sd.SourceSite, &sd.InfoHash, &sd.Title, &sd.Size,
		&sd.Category, &sd.Promotion, &sourceID, &sd.Status, &sd.Error,
		&sd.RetryCount, &sd.DiscoveredAt, &sd.UpdatedAt)
	if err != nil {
		return nil, wrapScanErr("scan seed", err)
	}
	sd.SourceID = sourceID.Int64
	return &sd, nil
}

// GetSeedByHash returns a seed by its dedup key (source_site, info_hash), or
// sql.ErrNoRows if absent.
func (r *Repo) GetSeedByHash(ctx context.Context, sourceSite, infoHash string) (*Seed, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+seedColumns+` FROM seeds WHERE source_site = ? AND info_hash = ?`, sourceSite, infoHash)
	return r.scanSeed(row)
}

// GetSeedByID returns a seed by primary key, or sql.ErrNoRows if absent.
// Added for the pipeline: Relay(ctx, seedID) loads the seed by id before
// resolving its source site and driving the nine-step flow.
func (r *Repo) GetSeedByID(ctx context.Context, id int64) (*Seed, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+seedColumns+` FROM seeds WHERE id = ?`, id)
	return r.scanSeed(row)
}

// CreateSeed inserts a seed and returns its id. It is idempotent on the
// (source_site, info_hash) UNIQUE key: if the row already exists the existing id
// is returned without modifying the row. The generated (or existing) id is also
// assigned back to s.ID.
func (r *Repo) CreateSeed(ctx context.Context, s *Seed) (int64, error) {
	if s == nil {
		return 0, fmt.Errorf("store: create seed: nil seed")
	}
	if err := validateSeedStatus(s.Status); err != nil {
		return 0, fmt.Errorf("store: create seed: %w", err)
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO seeds
			(source_site, info_hash, title, size, category, promotion,
			 source_id, status, error, retry_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_site, info_hash) DO NOTHING`,
		s.SourceSite, s.InfoHash, s.Title, s.Size, s.Category, s.Promotion,
		intOrNull(s.SourceID), s.Status, s.Error, s.RetryCount)
	if err != nil {
		return 0, fmt.Errorf("store: create seed: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return 0, fmt.Errorf("store: create seed: %w", err)
	} else if n > 0 {
		id, err := res.LastInsertId()
		if err != nil {
			return 0, fmt.Errorf("store: create seed id: %w", err)
		}
		s.ID = id
		return id, nil
	}

	// Conflict → return the existing row's id without touching it.
	var existing int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT id FROM seeds WHERE source_site = ? AND info_hash = ?`,
		s.SourceSite, s.InfoHash).Scan(&existing); err != nil {
		return 0, fmt.Errorf("store: create seed existing id: %w", err)
	}
	s.ID = existing
	return existing, nil
}

// UpdateSeedStatus sets a seed's status and error text.
func (r *Repo) UpdateSeedStatus(ctx context.Context, id int64, status, errMsg string) error {
	if err := validateSeedStatus(status); err != nil {
		return fmt.Errorf("store: update seed %d status: %w", id, err)
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE seeds SET status = ?, error = ?, updated_at = unixepoch() WHERE id = ?`, status, errMsg, id)
	if err != nil {
		return fmt.Errorf("store: update seed %d status: %w", id, err)
	}
	return nil
}

// BumpRetry increments retry_count by one.
func (r *Repo) BumpRetry(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE seeds SET retry_count = retry_count + 1, updated_at = unixepoch() WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: bump seed %d retry: %w", id, err)
	}
	return nil
}

// ListSeedsByStatus returns all seeds with the given status.
func (r *Repo) ListSeedsByStatus(ctx context.Context, status string) ([]*Seed, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+seedColumns+` FROM seeds WHERE status = ? ORDER BY id`, status)
	if err != nil {
		return nil, fmt.Errorf("store: list seeds by status: %w", err)
	}
	defer rows.Close()

	var out []*Seed
	for rows.Next() {
		s, err := r.scanSeed(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list seeds by status: %w", err)
	}
	return out, nil
}

// ListRecentSeeds returns the most recent limit seeds (highest id first).
func (r *Repo) ListRecentSeeds(ctx context.Context, limit int) ([]*Seed, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+seedColumns+` FROM seeds ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list recent seeds: %w", err)
	}
	defer rows.Close()

	var out []*Seed
	for rows.Next() {
		s, err := r.scanSeed(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list recent seeds: %w", err)
	}
	return out, nil
}
