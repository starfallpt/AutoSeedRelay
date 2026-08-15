package store

import (
	"context"
	"database/sql"
	"fmt"
)

const targetColumns = `id, name, type, version, base_url, announce_url, test_mode,
	fallback_category, category_overrides, dimension_overrides, tags_map,
	enc_cookie, enc_passkey, enc_api_token, status, created_at, updated_at`

func (r *Repo) scanTarget(s scanner) (*Target, error) {
	var t Target
	var encCookie, encPasskey, encAPIToken sql.NullString
	err := s.Scan(&t.ID, &t.Name, &t.Type, &t.Version, &t.BaseURL, &t.AnnounceURL,
		&t.TestMode, &t.FallbackCategory, &t.CategoryOverrides, &t.DimensionOverrides,
		&t.TagsMap, &encCookie, &encPasskey, &encAPIToken, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, wrapScanErr("scan target", err)
	}
	if t.Cookie, err = r.decryptNull(encCookie); err != nil {
		return nil, err
	}
	if t.Passkey, err = r.decryptNull(encPasskey); err != nil {
		return nil, err
	}
	if t.APIToken, err = r.decryptNull(encAPIToken); err != nil {
		return nil, err
	}
	return &t, nil
}

// GetEnabledTargets returns every target whose status is 'active'.
func (r *Repo) GetEnabledTargets(ctx context.Context) ([]*Target, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+targetColumns+` FROM targets WHERE status = 'active' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list enabled targets: %w", err)
	}
	defer rows.Close()

	var out []*Target
	for rows.Next() {
		t, err := r.scanTarget(rows)
		if err != nil {
			if warnBadCredential(err) {
				continue
			}
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list enabled targets: %w", err)
	}
	return out, nil
}

// GetTargetByID returns a target by primary key, or sql.ErrNoRows if absent.
func (r *Repo) GetTargetByID(ctx context.Context, id int64) (*Target, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+targetColumns+` FROM targets WHERE id = ?`, id)
	return r.scanTarget(row)
}

// UpsertTarget inserts a new target when t.ID == 0 (assigning the generated id
// back to t) or updates the row with t.ID otherwise. Credentials are encrypted
// on write.
func (r *Repo) UpsertTarget(ctx context.Context, t *Target) error {
	if t == nil {
		return fmt.Errorf("store: upsert target: nil target")
	}
	cookie, err := r.encrypt(t.Cookie)
	if err != nil {
		return err
	}
	passkey, err := r.encrypt(t.Passkey)
	if err != nil {
		return err
	}
	apiToken, err := r.encrypt(t.APIToken)
	if err != nil {
		return err
	}

	if t.ID == 0 {
		res, err := r.db.ExecContext(ctx, `
			INSERT INTO targets
				(name, type, version, base_url, announce_url, test_mode,
				 fallback_category, category_overrides, dimension_overrides, tags_map,
				 enc_cookie, enc_passkey, enc_api_token, status)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			t.Name, t.Type, t.Version, t.BaseURL, t.AnnounceURL, t.TestMode,
			t.FallbackCategory, t.CategoryOverrides, t.DimensionOverrides, t.TagsMap,
			cookie, passkey, apiToken, t.Status)
		if err != nil {
			return fmt.Errorf("store: insert target: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: insert target id: %w", err)
		}
		t.ID = id
		return nil
	}

	_, err = r.db.ExecContext(ctx, `
		UPDATE targets SET
			name = ?, type = ?, version = ?, base_url = ?, announce_url = ?,
			test_mode = ?, fallback_category = ?, category_overrides = ?,
			dimension_overrides = ?, tags_map = ?, enc_cookie = ?, enc_passkey = ?, enc_api_token = ?,
			status = ?, updated_at = unixepoch()
		WHERE id = ?`,
		t.Name, t.Type, t.Version, t.BaseURL, t.AnnounceURL, t.TestMode,
		t.FallbackCategory, t.CategoryOverrides, t.DimensionOverrides, t.TagsMap,
		cookie, passkey, apiToken, t.Status, t.ID)
	if err != nil {
		return fmt.Errorf("store: update target %d: %w", t.ID, err)
	}
	return nil
}
