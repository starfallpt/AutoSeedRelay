package store

import (
	"context"
	"fmt"
)

const strategyColumns = `id, promotions, keywords, min_size, max_size, retire_seeders,
	retire_minutes, retire_ratio_enabled, retire_ratio, retire_mode, dispatch_mode,
	timezone, image_host, image_cover_enabled, retry_max,
	disk_low_gb, disk_critical_gb, low_speed_kbps, low_speed_duration_sec, low_speed_action`

func (r *Repo) scanStrategy(s scanner) (*Strategy, error) {
	var st Strategy
	err := s.Scan(&st.ID, &st.Promotions, &st.Keywords, &st.MinSize, &st.MaxSize,
		&st.RetireSeeders, &st.RetireMinutes, &st.RetireRatioEnabled, &st.RetireRatio,
		&st.RetireMode, &st.DispatchMode, &st.Timezone, &st.ImageHost,
		&st.ImageCoverEnabled, &st.RetryMax,
		&st.DiskLowGB, &st.DiskCriticalGB, &st.LowSpeedKbps,
		&st.LowSpeedDurationSec, &st.LowSpeedAction)
	if err != nil {
		return nil, wrapScanErr("scan strategy", err)
	}
	return &st, nil
}

// GetStrategy returns the single strategy row (id=1), or sql.ErrNoRows if the
// seed row inserted by the migration is missing.
func (r *Repo) GetStrategy(ctx context.Context) (*Strategy, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+strategyColumns+` FROM strategies WHERE id = 1`)
	return r.scanStrategy(row)
}

// UpdateStrategy overwrites the strategy row (id=1). Only the id is ignored;
// every other column is written.
func (r *Repo) UpdateStrategy(ctx context.Context, st *Strategy) error {
	if st == nil {
		return fmt.Errorf("store: update strategy: nil strategy")
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE strategies SET
			promotions = ?, keywords = ?, min_size = ?, max_size = ?,
			retire_seeders = ?, retire_minutes = ?, retire_ratio_enabled = ?,
			retire_ratio = ?, retire_mode = ?, dispatch_mode = ?, timezone = ?,
			image_host = ?, image_cover_enabled = ?, retry_max = ?,
			disk_low_gb = ?, disk_critical_gb = ?, low_speed_kbps = ?,
			low_speed_duration_sec = ?, low_speed_action = ?
		WHERE id = 1`,
		st.Promotions, st.Keywords, st.MinSize, st.MaxSize,
		st.RetireSeeders, st.RetireMinutes, st.RetireRatioEnabled,
		st.RetireRatio, st.RetireMode, st.DispatchMode, st.Timezone,
		st.ImageHost, st.ImageCoverEnabled, st.RetryMax,
		st.DiskLowGB, st.DiskCriticalGB, st.LowSpeedKbps,
		st.LowSpeedDurationSec, st.LowSpeedAction)
	if err != nil {
		return fmt.Errorf("store: update strategy: %w", err)
	}
	return nil
}
