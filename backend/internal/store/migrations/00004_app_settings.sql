-- app_settings is a generic single-row-per-key store for application-level
-- settings that do not belong to a business-domain table. M3 (web auth) is the
-- first consumer: it persists the bcrypt web-password hash (web_password_hash)
-- and the HMAC session secret (session_secret) here. The auth package reads and
-- writes this table directly through *sql.DB; it deliberately does not extend
-- the store package with a dedicated repo.
CREATE TABLE app_settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
