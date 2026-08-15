package store

import (
	"context"
	"database/sql"
	"fmt"
)

const sourceColumns = `id, name, role, base_url, rss_url, announce_url, status,
	fail_count, enc_cookie, enc_passkey, enc_api_token, created_at, updated_at`

func (r *Repo) scanSource(s scanner) (*Source, error) {
	var src Source
	var encCookie, encPasskey, encAPIToken sql.NullString
	err := s.Scan(&src.ID, &src.Name, &src.Role, &src.BaseURL, &src.RSSURL,
		&src.AnnounceURL, &src.Status, &src.FailCount,
		&encCookie, &encPasskey, &encAPIToken, &src.CreatedAt, &src.UpdatedAt)
	if err != nil {
		return nil, wrapScanErr("scan source", err)
	}
	if src.Cookie, err = r.decryptNull(encCookie); err != nil {
		return nil, err
	}
	if src.Passkey, err = r.decryptNull(encPasskey); err != nil {
		return nil, err
	}
	if src.APIToken, err = r.decryptNull(encAPIToken); err != nil {
		return nil, err
	}
	return &src, nil
}

// GetActiveSources returns every source whose status is 'active'.
func (r *Repo) GetActiveSources(ctx context.Context) ([]*Source, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+sourceColumns+` FROM sources WHERE status = 'active' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list active sources: %w", err)
	}
	defer rows.Close()

	var out []*Source
	for rows.Next() {
		src, err := r.scanSource(rows)
		if err != nil {
			if warnBadCredential(err) {
				continue
			}
			return nil, err
		}
		out = append(out, src)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list active sources: %w", err)
	}
	return out, nil
}

// GetSourceByID returns a source by primary key, or sql.ErrNoRows if absent.
func (r *Repo) GetSourceByID(ctx context.Context, id int64) (*Source, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+sourceColumns+` FROM sources WHERE id = ?`, id)
	return r.scanSource(row)
}

// UpsertSource inserts a new source when s.ID == 0 (assigning the generated id
// back to s) or updates the row with s.ID otherwise. Credentials are encrypted
// on write. Timestamps are owned by the database (created_at defaults on insert,
// updated_at is refreshed on update).
func (r *Repo) UpsertSource(ctx context.Context, s *Source) error {
	if s == nil {
		return fmt.Errorf("store: upsert source: nil source")
	}
	cookie, err := r.encrypt(s.Cookie)
	if err != nil {
		return err
	}
	passkey, err := r.encrypt(s.Passkey)
	if err != nil {
		return err
	}
	apiToken, err := r.encrypt(s.APIToken)
	if err != nil {
		return err
	}

	if s.ID == 0 {
		res, err := r.db.ExecContext(ctx, `
			INSERT INTO sources
				(name, role, base_url, rss_url, announce_url, status, fail_count,
				 enc_cookie, enc_passkey, enc_api_token)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.Name, s.Role, s.BaseURL, s.RSSURL, s.AnnounceURL, s.Status, s.FailCount,
			cookie, passkey, apiToken)
		if err != nil {
			return fmt.Errorf("store: insert source: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: insert source id: %w", err)
		}
		s.ID = id
		return nil
	}

	_, err = r.db.ExecContext(ctx, `
		UPDATE sources SET
			name = ?, role = ?, base_url = ?, rss_url = ?, announce_url = ?,
			status = ?, fail_count = ?,
			enc_cookie = ?, enc_passkey = ?, enc_api_token = ?,
			updated_at = unixepoch()
		WHERE id = ?`,
		s.Name, s.Role, s.BaseURL, s.RSSURL, s.AnnounceURL, s.Status, s.FailCount,
		cookie, passkey, apiToken, s.ID)
	if err != nil {
		return fmt.Errorf("store: update source %d: %w", s.ID, err)
	}
	return nil
}

// SetSourceStatus changes a source's status.
func (r *Repo) SetSourceStatus(ctx context.Context, id int64, status string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE sources SET status = ?, updated_at = unixepoch() WHERE id = ?`, status, id)
	if err != nil {
		return fmt.Errorf("store: set source %d status: %w", id, err)
	}
	return nil
}

// IncSourceFail increments fail_count by one and returns the new value.
func (r *Repo) IncSourceFail(ctx context.Context, id int64) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE sources SET fail_count = fail_count + 1, updated_at = unixepoch() WHERE id = ?`, id)
	if err != nil {
		return 0, fmt.Errorf("store: inc source %d fail: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return 0, fmt.Errorf("store: inc source %d fail: %w", id, err)
	} else if n == 0 {
		return 0, sql.ErrNoRows
	}
	var fail int64
	if err := r.db.QueryRowContext(ctx, `SELECT fail_count FROM sources WHERE id = ?`, id).Scan(&fail); err != nil {
		return 0, fmt.Errorf("store: read source %d fail: %w", id, err)
	}
	return fail, nil
}

// PauseSource sets status='paused' and appends a warning activity_log row, both
// in a single transaction.
func (r *Repo) PauseSource(ctx context.Context, id int64, reason string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: pause source %d: %w", id, err)
	}
	defer tx.Rollback() // no-op after Commit

	res, err := tx.ExecContext(ctx, `
		UPDATE sources SET status = 'paused', updated_at = unixepoch() WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: pause source %d: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: pause source %d: %w", id, err)
	} else if n == 0 {
		return sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO activity_log (level, action, detail) VALUES ('warning', 'source_paused', ?)`, reason); err != nil {
		return fmt.Errorf("store: pause source %d log: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: pause source %d commit: %w", id, err)
	}
	return nil
}
