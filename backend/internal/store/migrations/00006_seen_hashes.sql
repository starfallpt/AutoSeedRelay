-- Permanent dedup tombstone (M5). Once a (source_site, info_hash) has been
-- observed by the poller it is recorded here and never removed by user cleanup,
-- so a hash whose seeds row was deleted (or lost to a stale backup restore) is
-- never re-enqueued. MarkSeen uses INSERT OR IGNORE, so first_seen_at is never
-- overwritten by a re-mark.
CREATE TABLE seen_hashes (source_site TEXT NOT NULL, info_hash TEXT NOT NULL, first_seen_at TEXT NOT NULL DEFAULT (datetime('now')), PRIMARY KEY (source_site, info_hash));
