// Package config provides the v3 app-level RelayConfig for relay.yaml.
// This extends the existing site-profile config (config.go) with QB settings,
// strategy parameters, and monitor thresholds used by the engine and web UI.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// AppConfig is the v3 app-level configuration corresponding to relay.yaml.
// It wraps site profiles, qB connection, strategy rules, and monitor thresholds.
type AppConfig struct {
	// Sources defines the RSS source sites (one or more).
	Sources []*SiteProfile `yaml:"sources" json:"sources"`

	// Targets defines the target sites where cleaned torrents are uploaded.
	Targets []*SiteProfile `yaml:"targets" json:"targets"`

	// QB holds qBittorrent connection settings.
	QB QBConfig `yaml:"qb" json:"qb"`

	// Strategy controls which RSS items are selected for relay.
	Strategy StrategyConfig `yaml:"strategy" json:"strategy"`

	// Retire controls when completed seeds should be removed.
	Retire RetireConfig `yaml:"retire" json:"retire"`

	// Monitor controls the monitoring loop intervals and thresholds.
	Monitor MonitorConfig `yaml:"monitor" json:"monitor"`

	// Web controls the web UI server.
	Web WebConfig `yaml:"web" json:"web"`

	// Global settings.
	PollInterval float64  `yaml:"poll_interval" json:"poll_interval"`
	Keywords     []string `yaml:"keywords" json:"keywords"`
	DBPath       string   `yaml:"db_path" json:"db_path"`
	WorkDir      string   `yaml:"workdir" json:"workdir"`
	TorrentsDir  string   `yaml:"torrents_dir" json:"torrents_dir"`
	LogLevel     string   `yaml:"log_level" json:"log_level"`
}

// QBConfig holds qBittorrent connection parameters.
type QBConfig struct {
	Host     string `yaml:"host" json:"host"`
	Port     int    `yaml:"port" json:"port"`
	Username string `yaml:"username" json:"username"`
		Password string `yaml:"password" json:"-"`
	UseSSL   bool   `yaml:"use_ssl" json:"use_ssl"`
}

// URL returns the full qB WebUI base URL (e.g. http://127.0.0.1:8080).
func (q QBConfig) URL() string {
	scheme := "http"
	if q.UseSSL {
		scheme = "https"
	}
	host := q.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := q.Port
	if port == 0 {
		port = 8080
	}
	return fmt.Sprintf("%s://%s:%d", scheme, host, port)
}

// StrategyConfig controls RSS item filtering.
type StrategyConfig struct {
	// Promotion filters: which discount types to match.
	Promotions []string `yaml:"promotions" json:"promotions"` // free, 2x_free, 50%, 30%, neutral

	// Keywords in the torrent title that must match (case-insensitive).
	Keywords []string `yaml:"keywords" json:"keywords"`

	// Size limits in bytes (0 = unlimited).
	MinSize int64 `yaml:"min_size" json:"min_size"`
	MaxSize int64 `yaml:"max_size" json:"max_size"`

	// Role: "publisher" or "cross_seeder".
	Role string `yaml:"role" json:"role"`

	// MaxConcurrent limits the number of simultaneous downloads.
	MaxConcurrent int `yaml:"max_concurrent" json:"max_concurrent"`

	// DownloadTimeoutSeconds: max time allowed for a download to complete.
	DownloadTimeoutSeconds float64 `yaml:"download_timeout" json:"download_timeout"`

	// RetryCount: number of retries after a download timeout.
	RetryCount int `yaml:"retry_count" json:"retry_count"`

	// RetryIntervalSeconds: delay between retries.
	RetryIntervalSeconds float64 `yaml:"retry_interval" json:"retry_interval"`

	// LowSpeedKBps: threshold for low-speed detection.
	LowSpeedKBps int `yaml:"low_speed_kbps" json:"low_speed_kbps"`

	// LowSpeedDurationSeconds: how long low speed must persist.
	LowSpeedDurationSeconds float64 `yaml:"low_speed_duration" json:"low_speed_duration"`

	// LowSpeedAction: "abort" or "continue".
	LowSpeedAction string `yaml:"low_speed_action" json:"low_speed_action"`
}

// RetireConfig controls when completed seeds are removed.
type RetireConfig struct {
	MinSeeders    int     `yaml:"min_seeders" json:"min_seeders"`
	MinRatio      float64 `yaml:"min_ratio" json:"min_ratio"`
	MinDays       int     `yaml:"min_days" json:"min_days"`
	DeleteFiles   bool    `yaml:"delete_files" json:"delete_files"`
}

// MonitorConfig controls the monitoring loop.
type MonitorConfig struct {
	IntervalSeconds float64 `yaml:"interval_seconds" json:"interval_seconds"`

	// Disk thresholds in GB.
	DiskLowGB      float64 `yaml:"disk_low_gb" json:"disk_low_gb"`
	DiskCriticalGB float64 `yaml:"disk_critical_gb" json:"disk_critical_gb"`

	// Site backoff on errors.
	SiteBackoffBase   float64 `yaml:"site_backoff_base" json:"site_backoff_base"`
	SiteBackoffMax    float64 `yaml:"site_backoff_max" json:"site_backoff_max"`
	SiteMaxFailures   int     `yaml:"site_max_failures" json:"site_max_failures"`
}

// WebConfig controls the web UI server.
type WebConfig struct {
	ListenAddr string `yaml:"listen_addr" json:"listen_addr"`
	Password   string `yaml:"password" json:"-"` // Web panel login password, default "admin"
}

// NewAppConfig returns an AppConfig with sensible defaults.
func NewAppConfig() *AppConfig {
	return &AppConfig{
		Sources:      []*SiteProfile{},
		Targets:      []*SiteProfile{},
		PollInterval: 300,
		Keywords:     []string{"StarfallWeb", "LongWeb"},
		DBPath:       "data/relay.db",
		WorkDir:      "data",
		TorrentsDir:  "data/torrents",
		LogLevel:     "info",
		QB: QBConfig{
			Host:     "127.0.0.1",
			Port:     8080,
			Username: "admin",
			Password: "adminadmin",
		},
		Strategy: StrategyConfig{
			Promotions:              []string{"free", "2x_free"},
			Keywords:                []string{"StarfallWeb", "LongWeb"},
			Role:                    "cross_seeder",
			MaxConcurrent:           3,
			DownloadTimeoutSeconds:  3600,
			RetryCount:              3,
			RetryIntervalSeconds:    300,
			LowSpeedKBps:            100,
			LowSpeedDurationSeconds: 600,
			LowSpeedAction:          "abort",
		},
		Retire: RetireConfig{
			MinSeeders:  5,
			MinRatio:    2.0,
			MinDays:     14,
			DeleteFiles: false,
		},
		Monitor: MonitorConfig{
			IntervalSeconds:  30,
			DiskLowGB:        50,
			DiskCriticalGB:   20,
			SiteBackoffBase:  60,
			SiteBackoffMax:   900,
			SiteMaxFailures:  5,
		},
		Web: WebConfig{
			ListenAddr: ":9020",
			Password:   "admin",
		},
	}
}

// LoadAppConfig loads an AppConfig from a YAML file. If path is empty,
// it tries config/relay.yaml then config/relay.json.
func LoadAppConfig(path string) (*AppConfig, error) {
	cfg := NewAppConfig()

	p, err := resolveConfigPath(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", p, err)
	}

	sub := substituteEnvInString(string(raw), os.Getenv)
	if err := yaml.Unmarshal([]byte(sub.(string)), cfg); err != nil {
		// Try JSON as fallback
		if err2 := yaml.Unmarshal(raw, cfg); err2 != nil {
			return nil, fmt.Errorf("config: parse %s: %w", p, err)
		}
	}

	// Apply environment variable overrides.
	applyEnvOverridesApp(cfg)

	// Apply defaults for zero values.
	applyDefaultsApp(cfg)

	return cfg, nil
}

// resolveConfigPath resolves the config file path.
func resolveConfigPath(path string) (string, error) {
	if path != "" {
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("config file not found: %s", path)
		}
		return path, nil
	}
	for _, candidate := range []string{"config/relay.yaml", "config/relay.json"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no config file found (tried config/relay.yaml, config/relay.json)")
}

// applyDefaultsApp fills zero values with defaults from NewAppConfig.
func applyDefaultsApp(cfg *AppConfig) {
	def := NewAppConfig()

	if cfg.PollInterval <= 0 {
		cfg.PollInterval = def.PollInterval
	}
	if len(cfg.Keywords) == 0 {
		cfg.Keywords = def.Keywords
	}
	if cfg.DBPath == "" {
		cfg.DBPath = def.DBPath
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = def.WorkDir
	}
	if cfg.TorrentsDir == "" {
		cfg.TorrentsDir = def.TorrentsDir
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = def.LogLevel
	}

	// QB defaults.
	if cfg.QB.Host == "" {
		cfg.QB.Host = def.QB.Host
	}
	if cfg.QB.Port == 0 {
		cfg.QB.Port = def.QB.Port
	}
	if cfg.QB.Username == "" {
		cfg.QB.Username = def.QB.Username
	}

	// Strategy defaults.
	if len(cfg.Strategy.Promotions) == 0 {
		cfg.Strategy.Promotions = def.Strategy.Promotions
	}
	if len(cfg.Strategy.Keywords) == 0 {
		cfg.Strategy.Keywords = def.Strategy.Keywords
	}
	if cfg.Strategy.Role == "" {
		cfg.Strategy.Role = def.Strategy.Role
	}
	if cfg.Strategy.MaxConcurrent <= 0 {
		cfg.Strategy.MaxConcurrent = def.Strategy.MaxConcurrent
	}
	if cfg.Strategy.DownloadTimeoutSeconds <= 0 {
		cfg.Strategy.DownloadTimeoutSeconds = def.Strategy.DownloadTimeoutSeconds
	}
	if cfg.Strategy.RetryCount <= 0 {
		cfg.Strategy.RetryCount = def.Strategy.RetryCount
	}
	if cfg.Strategy.RetryIntervalSeconds <= 0 {
		cfg.Strategy.RetryIntervalSeconds = def.Strategy.RetryIntervalSeconds
	}
	if cfg.Strategy.LowSpeedKBps <= 0 {
		cfg.Strategy.LowSpeedKBps = def.Strategy.LowSpeedKBps
	}
	if cfg.Strategy.LowSpeedDurationSeconds <= 0 {
		cfg.Strategy.LowSpeedDurationSeconds = def.Strategy.LowSpeedDurationSeconds
	}
	if cfg.Strategy.LowSpeedAction == "" {
		cfg.Strategy.LowSpeedAction = def.Strategy.LowSpeedAction
	}

	// Retire defaults.
	if cfg.Retire.MinSeeders <= 0 {
		cfg.Retire.MinSeeders = def.Retire.MinSeeders
	}
	if cfg.Retire.MinRatio <= 0 {
		cfg.Retire.MinRatio = def.Retire.MinRatio
	}
	if cfg.Retire.MinDays <= 0 {
		cfg.Retire.MinDays = def.Retire.MinDays
	}

	// Monitor defaults.
	if cfg.Monitor.IntervalSeconds <= 0 {
		cfg.Monitor.IntervalSeconds = def.Monitor.IntervalSeconds
	}
	if cfg.Monitor.DiskLowGB <= 0 {
		cfg.Monitor.DiskLowGB = def.Monitor.DiskLowGB
	}
	if cfg.Monitor.DiskCriticalGB <= 0 {
		cfg.Monitor.DiskCriticalGB = def.Monitor.DiskCriticalGB
	}
	if cfg.Monitor.SiteBackoffBase <= 0 {
		cfg.Monitor.SiteBackoffBase = def.Monitor.SiteBackoffBase
	}
	if cfg.Monitor.SiteBackoffMax <= 0 {
		cfg.Monitor.SiteBackoffMax = def.Monitor.SiteBackoffMax
	}
	if cfg.Monitor.SiteMaxFailures <= 0 {
		cfg.Monitor.SiteMaxFailures = def.Monitor.SiteMaxFailures
	}

	// Web defaults.
	if cfg.Web.ListenAddr == "" {
		cfg.Web.ListenAddr = def.Web.ListenAddr
	}
	if cfg.Web.Password == "" {
		cfg.Web.Password = def.Web.Password
	}
}

// applyEnvOverridesApp applies AUTOSEED_* and QB_* environment variable overrides.
func applyEnvOverridesApp(cfg *AppConfig) {
	if v := os.Getenv("AUTOSEED_POLL_INTERVAL"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.PollInterval = f
		}
	}
	if v := os.Getenv("AUTOSEED_DB"); v != "" {
		cfg.DBPath = v
	}
	if v := os.Getenv("AUTOSEED_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("AUTOSEED_LISTEN_ADDR"); v != "" {
		cfg.Web.ListenAddr = v
	}
	if v := os.Getenv("AUTOSEED_WEB_PASSWORD"); v != "" {
		cfg.Web.Password = v
	}

	// QB env overrides.
	if v := os.Getenv("QBHOST"); v != "" {
		cfg.QB.Host = v
	}
	if v := os.Getenv("QBUSER"); v != "" {
		cfg.QB.Username = v
	}
	if v := os.Getenv("QBPASS"); v != "" {
		cfg.QB.Password = v
	}
	if v := os.Getenv("QB_WEBUI_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.QB.Port = n
		}
	}
}

// SaveAppConfig writes the current configuration to a YAML file.
func SaveAppConfig(cfg *AppConfig, path string) error {
	if path == "" {
		path = "config/relay.yaml"
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("config: create dir: %w", err)
	}

	// Mask sensitive fields before saving.
	saved := *cfg
	saved.QB.Password = "***"

	b, err := yaml.Marshal(&saved)
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	return os.WriteFile(path, b, 0o644)
}

// substituteEnvInString replaces <PUT_ENV_XXX> placeholders in a string.
func substituteEnvInString(s string, getenv func(string) string) any {
	for {
		start := strings.Index(s, "<PUT_ENV_")
		if start < 0 {
			break
		}
		end := strings.Index(s[start:], ">")
		if end < 0 {
			break
		}
		varName := s[start+9 : start+end]
		val := getenv(varName)
		s = s[:start] + val + s[start+end+1:]
	}
	return s
}

// AllSites returns all source and target SiteProfiles concatenated.
func (c *AppConfig) AllSites() []*SiteProfile {
	out := make([]*SiteProfile, 0, len(c.Sources)+len(c.Targets))
	out = append(out, c.Sources...)
	out = append(out, c.Targets...)
	return out
}
