-- AutoSeedRelay schema v1 — full initial schema (docs/ARCHITECTURE-v4.md §6).
-- Applied once by the store migration engine, tracked via PRAGMA user_version.

CREATE TABLE sources (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'source',
    base_url      TEXT NOT NULL DEFAULT '',
    rss_url       TEXT NOT NULL DEFAULT '',
    announce_url  TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','paused')),
    fail_count    INTEGER NOT NULL DEFAULT 0,
    enc_cookie    TEXT,
    enc_passkey   TEXT,
    enc_api_token TEXT,
    created_at    INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at    INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE TABLE targets (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    name                TEXT NOT NULL,
    type                TEXT NOT NULL DEFAULT 'nexusphp'
                            CHECK (type IN ('nexusphp','nexusphp_classic','mteam')),
    version             TEXT NOT NULL DEFAULT 'classic'
                            CHECK (version IN ('api','classic')),
    base_url            TEXT NOT NULL DEFAULT '',
    announce_url        TEXT NOT NULL DEFAULT '',
    test_mode           INTEGER NOT NULL DEFAULT 0,
    fallback_category   TEXT NOT NULL DEFAULT '',
    category_overrides  TEXT NOT NULL DEFAULT '{}',
    dimension_overrides TEXT NOT NULL DEFAULT '{}',
    enc_cookie          TEXT,
    enc_passkey         TEXT,
    enc_api_token       TEXT,
    status              TEXT NOT NULL DEFAULT 'active'
                            CHECK (status IN ('active','paused')),
    created_at          INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at          INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE TABLE qb_instances (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT NOT NULL,
    host         TEXT NOT NULL,
    port         INTEGER NOT NULL DEFAULT 8080,
    username     TEXT NOT NULL DEFAULT '',
    enc_password TEXT,
    priority     INTEGER NOT NULL DEFAULT 0,
    enabled      INTEGER NOT NULL DEFAULT 1,
    last_seen_at INTEGER,
    extra        TEXT NOT NULL DEFAULT '{}',
    created_at   INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at   INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE TABLE seeds (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    source_site   TEXT NOT NULL,
    info_hash     TEXT NOT NULL,
    title         TEXT NOT NULL DEFAULT '',
    size          INTEGER NOT NULL DEFAULT 0,
    category      TEXT NOT NULL DEFAULT '',
    promotion     TEXT NOT NULL DEFAULT '',
    source_id     INTEGER,
    status        TEXT NOT NULL DEFAULT 'discovered',
    error         TEXT NOT NULL DEFAULT '',
    retry_count   INTEGER NOT NULL DEFAULT 0,
    discovered_at INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at    INTEGER NOT NULL DEFAULT (unixepoch()),
    UNIQUE (source_site, info_hash)
);

CREATE TABLE relay_records (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    seed_id           INTEGER NOT NULL REFERENCES seeds (id) ON DELETE CASCADE,
    target_id         INTEGER NOT NULL REFERENCES targets (id) ON DELETE CASCADE,
    role              TEXT NOT NULL DEFAULT 'publisher'
                          CHECK (role IN ('publisher','seeder')),
    status            TEXT NOT NULL DEFAULT 'pending',
    target_torrent_id TEXT NOT NULL DEFAULT '',
    attempts          INTEGER NOT NULL DEFAULT 0,
    last_error        TEXT NOT NULL DEFAULT '',
    published_at      INTEGER,
    retired_at        INTEGER,
    retire_reason     TEXT NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at        INTEGER NOT NULL DEFAULT (unixepoch()),
    UNIQUE (seed_id, target_id)
);

CREATE TABLE seed_replicas (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    seed_id   INTEGER NOT NULL REFERENCES seeds (id) ON DELETE CASCADE,
    qb_id     INTEGER NOT NULL REFERENCES qb_instances (id) ON DELETE CASCADE,
    info_hash TEXT NOT NULL DEFAULT '',
    role      TEXT NOT NULL DEFAULT 'origin' CHECK (role IN ('origin','cross')),
    status    TEXT NOT NULL DEFAULT 'pending',
    progress  REAL NOT NULL DEFAULT 0,
    added_at  INTEGER NOT NULL DEFAULT (unixepoch()),
    UNIQUE (seed_id, qb_id, role)
);

CREATE TABLE activity_log (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    seed_id    INTEGER,
    level      TEXT NOT NULL DEFAULT 'info',
    action     TEXT NOT NULL DEFAULT '',
    detail     TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX idx_activity_log_created_at ON activity_log (created_at);

CREATE TABLE notifier_instances (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    type       TEXT NOT NULL
                   CHECK (type IN ('webhook','telegram','smtp','ntfy','gotify','serverchan','pushplus')),
    name       TEXT NOT NULL,
    enc_config TEXT,
    enabled    INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE TABLE notifier_routes (
    instance_id INTEGER NOT NULL REFERENCES notifier_instances (id) ON DELETE CASCADE,
    tier        TEXT NOT NULL CHECK (tier IN ('critical','warning','info')),
    enabled     INTEGER NOT NULL DEFAULT 0,
    UNIQUE (instance_id, tier)
);

CREATE TABLE strategies (
    id                   INTEGER PRIMARY KEY CHECK (id = 1),
    promotions           TEXT NOT NULL DEFAULT '[]',
    keywords             TEXT NOT NULL DEFAULT '[]',
    min_size             INTEGER NOT NULL DEFAULT 0,
    max_size             INTEGER NOT NULL DEFAULT 0,
    retire_seeders       INTEGER NOT NULL DEFAULT 10,
    retire_minutes       INTEGER NOT NULL DEFAULT 60,
    retire_ratio_enabled INTEGER NOT NULL DEFAULT 0,
    retire_ratio         REAL NOT NULL DEFAULT 0,
    retire_mode          TEXT NOT NULL DEFAULT 'and' CHECK (retire_mode IN ('and','or')),
    dispatch_mode        TEXT NOT NULL DEFAULT 'priority',
    timezone             TEXT NOT NULL DEFAULT 'UTC',
    image_host           TEXT NOT NULL DEFAULT '{}',
    image_cover_enabled  INTEGER NOT NULL DEFAULT 0,
    retry_max            INTEGER NOT NULL DEFAULT 3
);

INSERT INTO strategies (id) VALUES (1);
