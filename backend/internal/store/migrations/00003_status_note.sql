-- Status whitelists for seeds.status and relay_records.status are enforced in
-- the application layer (see status.go) because SQLite cannot ALTER an existing
-- column to add a CHECK constraint. This migration therefore performs no schema
-- change; it only advances PRAGMA user_version to 3 so the whitelist
-- enforcement is recorded as a versioned step.
SELECT 1;
