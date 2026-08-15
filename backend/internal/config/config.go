// Package config loads deployment-level configuration (YAML + env).
//
// M0 skeleton: only listen_addr / log_level / db_path. Business config moves
// into the DB in later milestones (see docs/ARCHITECTURE-v4.md §4).
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Deployment-level defaults.
const (
	DefaultListenAddr = ":9020"
	DefaultLogLevel   = "info"
	DefaultDBPath     = "data/relay.db"
)

// Config is the read-only deployment-level configuration.
type Config struct {
	// ListenAddr is the HTTP listen address (host:port).
	ListenAddr string `yaml:"listen_addr"`
	// LogLevel is one of debug|info|warn|error.
	LogLevel string `yaml:"log_level"`
	// DBPath is the SQLite database path (data/relay.db by default).
	DBPath string `yaml:"db_path"`
}

// Default returns a Config populated with defaults.
func Default() *Config {
	return &Config{
		ListenAddr: DefaultListenAddr,
		LogLevel:   DefaultLogLevel,
		DBPath:     DefaultDBPath,
	}
}

// Load builds a Config from an optional YAML file plus environment overrides.
// An empty path skips file loading (defaults only). Environment variables are
// applied last and win over both defaults and file values.
func Load(path string) (*Config, error) {
	cfg := Default()

	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("config file %q not found", path)
			}
			return nil, fmt.Errorf("read config %q: %w", path, err)
		}
		if err := yaml.Unmarshal(b, cfg); err != nil {
			return nil, fmt.Errorf("parse config %q: %w", path, err)
		}
	}

	applyEnv(cfg)
	return cfg, nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("AUTOSEED_LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("AUTOSEED_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("AUTOSEED_DB_PATH"); v != "" {
		cfg.DBPath = v
	}
}

// SlogLevel maps LogLevel to a slog.Level, defaulting to Info for unknown values.
func (c *Config) SlogLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(c.LogLevel)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
