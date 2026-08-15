-- Add disk / low-speed monitoring thresholds to the single-row strategies table
-- (M3 engine-completion gap; see BIZ-SPEC §5). All columns carry safe defaults
-- so the seeded id=1 row is extended in place without touching existing values.
ALTER TABLE strategies ADD COLUMN disk_low_gb INTEGER NOT NULL DEFAULT 50;
ALTER TABLE strategies ADD COLUMN disk_critical_gb INTEGER NOT NULL DEFAULT 20;
ALTER TABLE strategies ADD COLUMN low_speed_kbps INTEGER NOT NULL DEFAULT 100;
ALTER TABLE strategies ADD COLUMN low_speed_duration_sec INTEGER NOT NULL DEFAULT 600;
ALTER TABLE strategies ADD COLUMN low_speed_action TEXT NOT NULL DEFAULT 'abort';
