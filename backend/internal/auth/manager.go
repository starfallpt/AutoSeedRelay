// Package auth implements the M3 web-authentication layer: bcrypt password
// hashing, stateless HMAC-SHA256 session cookies, CSRF double-submit tokens,
// per-IP login rate limiting, and a Gin middleware that enforces them.
//
// All persisted state lives in the store's app_settings table (key/value), read
// and written directly through *sql.DB — the store package is not extended.
package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// app_settings keys owned by this package.
const (
	keyWebPasswordHash = "web_password_hash"
	keySessionSecret   = "session_secret"
)

// secretBytes is the HMAC session-secret length in bytes.
const secretBytes = 32

// Sentinel errors returned by CompleteSetup.
var (
	// ErrAlreadyInitialized is returned when a password hash already exists.
	ErrAlreadyInitialized = errors.New("auth: already initialized")
	// ErrEmptyPassword is returned when an empty password is supplied.
	ErrEmptyPassword = errors.New("auth: password must not be empty")
)

// Options configures a Manager. All fields are optional.
type Options struct {
	// SessionSecret, when non-empty, is used verbatim as the HMAC-SHA256 key
	// for signing session cookies. When empty, the manager reads a persisted
	// secret from app_settings, or generates and persists a fresh 32-byte one.
	SessionSecret []byte
	// WebPasswordEnv names an environment variable holding the initial web
	// password. When set at construction time and no password hash is stored
	// yet, the manager auto-initializes by hashing this value. Empty disables
	// auto-initialization.
	WebPasswordEnv string
	// Now supplies the clock used for session expiry and the login rate-limit
	// window. Nil defaults to time.Now; injectable for deterministic tests.
	Now func() time.Time
}

// Manager owns web-auth state: the password hash, the HMAC session secret, the
// in-memory per-IP login rate limiter, and the middleware/handlers enforcing
// them.
type Manager struct {
	db     *sql.DB
	logger *slog.Logger
	secret []byte
	now    func() time.Time
	limits *rateLimiter
}

// New builds a Manager, resolving the session secret and auto-initializing from
// WebPasswordEnv when applicable. It returns an error only when the database is
// unusable or secret generation/persistence fails.
func New(db *sql.DB, logger *slog.Logger, opts Options) (*Manager, error) {
	if db == nil {
		return nil, errors.New("auth: nil *sql.DB")
	}
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	m := &Manager{
		db:     db,
		logger: logger,
		now:    now,
		limits: newRateLimiter(loginLimitPerMinute, loginWindow, now),
	}

	if len(opts.SessionSecret) > 0 {
		m.secret = opts.SessionSecret
	} else {
		secret, err := m.loadOrCreateSecret()
		if err != nil {
			return nil, err
		}
		m.secret = secret
	}

	// Auto-initialize from the env password when no hash is stored yet.
	if opts.WebPasswordEnv != "" {
		if pw := os.Getenv(opts.WebPasswordEnv); pw != "" && !m.SetupState() {
			if err := m.CompleteSetup(pw); err != nil {
				return nil, fmt.Errorf("auth: auto-initialize from %s: %w", opts.WebPasswordEnv, err)
			}
			m.logger.Info("auth: auto-initialized web password from env", "env", opts.WebPasswordEnv)
		}
	}

	return m, nil
}

// loadOrCreateSecret returns the persisted session secret, generating and
// persisting a fresh 32-byte one (hex-encoded) when none exists yet.
func (m *Manager) loadOrCreateSecret() ([]byte, error) {
	var stored string
	err := m.db.QueryRow("SELECT value FROM app_settings WHERE key = ?", keySessionSecret).Scan(&stored)
	if err == nil && stored != "" {
		b, derr := hex.DecodeString(stored)
		if derr != nil || len(b) != secretBytes {
			return nil, fmt.Errorf("auth: stored session secret invalid: %w", derr)
		}
		return b, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("auth: read session secret: %w", err)
	}

	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("auth: generate session secret: %w", err)
	}
	if _, err := m.db.Exec(
		"INSERT INTO app_settings(key, value) VALUES(?, ?)", keySessionSecret, hex.EncodeToString(b)); err != nil {
		return nil, fmt.Errorf("auth: persist session secret: %w", err)
	}
	return b, nil
}

// SetupState reports whether the web password has been initialized (a stored
// bcrypt hash exists). It fails closed: a database error is logged and reported
// as "not initialized".
func (m *Manager) SetupState() bool {
	ok, err := m.setupState()
	if err != nil {
		m.logger.Error("auth: read setup state", "err", err)
		return false
	}
	return ok
}

func (m *Manager) setupState() (bool, error) {
	var v string
	err := m.db.QueryRow("SELECT value FROM app_settings WHERE key = ?", keyWebPasswordHash).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("auth: read password hash: %w", err)
	}
	return v != "", nil
}

// CompleteSetup stores the bcrypt hash of pw as the web password. It fails with
// ErrEmptyPassword for an empty password and ErrAlreadyInitialized when a hash
// already exists.
func (m *Manager) CompleteSetup(pw string) error {
	if pw == "" {
		return ErrEmptyPassword
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("auth: hash password: %w", err)
	}
	if _, err := m.db.Exec(
		"INSERT INTO app_settings(key, value) VALUES(?, ?)", keyWebPasswordHash, string(hash)); err != nil {
		if isUniqueViolation(err) {
			return ErrAlreadyInitialized
		}
		return fmt.Errorf("auth: store password hash: %w", err)
	}
	return nil
}

// Login verifies pw against the stored bcrypt hash using the constant-time
// bcrypt comparison. It returns (true, nil) on success, (false, nil) for a wrong
// or missing password, and a non-nil error only for internal failures.
func (m *Manager) Login(ctx context.Context, pw string) (bool, error) {
	var hash string
	err := m.db.QueryRowContext(ctx, "SELECT value FROM app_settings WHERE key = ?", keyWebPasswordHash).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("auth: read password hash: %w", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)); err != nil {
		return false, nil
	}
	return true, nil
}

// AllowLogin applies the per-IP login rate limit. It returns false together
// with the time to wait when the IP has exhausted its window budget.
func (m *Manager) AllowLogin(ip string) (allowed bool, retryAfter time.Duration) {
	return m.limits.allow(ip)
}

// isUniqueViolation reports whether err is a SQLite UNIQUE/PK constraint
// violation (modernc.org/sqlite surfaces these as a constraint-failed error).
func isUniqueViolation(err error) bool {
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
