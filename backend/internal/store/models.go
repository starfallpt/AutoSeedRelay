package store

// Entity structs mirror the columns of migrations/00001_init.sql one-to-one.
//
// Conventions:
//   - TEXT columns map to string; INTEGER columns map to int64; REAL maps to
//     float64. Boolean-ish INTEGER flags (enabled / test_mode / *_enabled) are
//     kept as int64 0/1 to stay faithful to the schema.
//   - JSON columns (category_overrides, extra, promotions, keywords, image_host,
//     notifier config) are stored and returned as raw JSON strings — this layer
//     never parses them.
//   - Timestamps are Unix seconds (schema defaults to unixepoch()).
//   - Nullable INTEGER columns use int64 with 0 meaning NULL on write and
//     NULL reading back as 0 (timestamps and auto-increment FKs never use 0).
//   - Credential fields (Cookie / Passkey / APIToken / Password / Config) hold
//     PLAINTEXT; the Repo encrypts them into the enc_* columns on write and
//     decrypts them on read. Empty plaintext is stored as NULL.

// Source is one upstream source site (row in sources). The json tags mirror the
// /api/v2 sources wire contract (snake_case) so the entity is directly
// serializable without a separate DTO where a handler needs it.
type Source struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Role        string `json:"role"`
	BaseURL     string `json:"base_url"`
	RSSURL      string `json:"rss_url"`
	AnnounceURL string `json:"announce_url"`
	Status      string `json:"status"` // active | paused
	FailCount   int64  `json:"fail_count"`

	// Credential fields (enc_cookie / enc_passkey / enc_api_token), plaintext.
	Cookie   string `json:"cookie"`
	Passkey  string `json:"passkey"`
	APIToken string `json:"api_token"`

	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

// Target is one downstream PT site (row in targets).
type Target struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"`    // nexusphp | nexusphp_classic | mteam
	Version     string `json:"version"` // api | classic
	BaseURL     string `json:"base_url"`
	AnnounceURL string `json:"announce_url"`
	TestMode    int64  `json:"test_mode"` // 0/1

	FallbackCategory   string `json:"fallback_category"`
	CategoryOverrides  string `json:"category_overrides"`  // JSON string
	DimensionOverrides string `json:"dimension_overrides"` // JSON string
	TagsMap            string `json:"tags_map"`            // JSON string

	// Credential fields (enc_cookie / enc_passkey / enc_api_token), plaintext.
	Cookie   string `json:"cookie"`
	Passkey  string `json:"passkey"`
	APIToken string `json:"api_token"`

	Status    string `json:"status"` // active | paused
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// QBInstance is one qBittorrent instance (row in qb_instances).
type QBInstance struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int64  `json:"port"`
	Username string `json:"username"`

	// Password is the plaintext qB password (enc_password column).
	Password string `json:"password"`

	Priority   int64  `json:"priority"`
	Enabled    int64  `json:"enabled"` // 0/1
	LastSeenAt int64  `json:"last_seen_at"` // Unix seconds; 0 = never seen
	Extra      string `json:"extra"`        // JSON string

	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

// Seed is one discovered torrent (row in seeds). Unique on (source_site, info_hash).
type Seed struct {
	ID         int64  `json:"id"`
	SourceSite string `json:"source_site"`
	InfoHash   string `json:"info_hash"`
	Title      string `json:"title"`
	Size       int64  `json:"size"`
	Category   string `json:"category"`
	Promotion  string `json:"promotion"`
	SourceID   int64  `json:"source_id"` // 0 = no source row
	Status     string `json:"status"`
	Error      string `json:"error"`
	RetryCount int64  `json:"retry_count"`

	DiscoveredAt int64 `json:"discovered_at"` // Unix seconds
	UpdatedAt    int64 `json:"updated_at"`
}

// RelayRecord is one per-target publish/seeding attempt for a seed
// (row in relay_records). Unique on (seed_id, target_id).
type RelayRecord struct {
	ID              int64  `json:"id"`
	SeedID          int64  `json:"seed_id"`
	TargetID        int64  `json:"target_id"`
	Role            string `json:"role"` // publisher | seeder
	Status          string `json:"status"`
	TargetTorrentID string `json:"target_torrent_id"`
	Attempts        int64  `json:"attempts"`
	LastError       string `json:"last_error"`
	PublishedAt     int64  `json:"published_at"` // Unix seconds; 0 = not published
	RetiredAt       int64  `json:"retired_at"`   // Unix seconds; 0 = not retired
	RetireReason    string `json:"retire_reason"`

	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

// Replica is one seed copy living on a qB instance (row in seed_replicas).
// Unique on (seed_id, qb_id, role).
type Replica struct {
	ID       int64   `json:"id"`
	SeedID   int64   `json:"seed_id"`
	QBID     int64   `json:"qb_id"`
	InfoHash string  `json:"info_hash"`
	Role     string  `json:"role"` // origin | cross
	Status   string  `json:"status"`
	Progress float64 `json:"progress"`
	AddedAt  int64   `json:"added_at"` // Unix seconds
}

// Activity is one activity_log row.
type Activity struct {
	ID        int64  `json:"id"`
	SeedID    int64  `json:"seed_id"` // 0 = not seed-scoped
	Level     string `json:"level"`
	Action    string `json:"action"`
	Detail    string `json:"detail"`
	CreatedAt int64  `json:"created_at"`
}

// NotifierInstance is one notifier provider instance (row in notifier_instances).
type NotifierInstance struct {
	ID   int64  `json:"id"`
	Type string `json:"type"` // webhook | telegram | smtp | ntfy | gotify | serverchan | pushplus
	Name string `json:"name"`

	// Config is the plaintext instance config JSON (enc_config column).
	Config string `json:"config"`

	Enabled   int64 `json:"enabled"` // 0/1
	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

// Route is one notifier_routes row (instance × tier matrix cell).
type Route struct {
	InstanceID int64  `json:"instance_id"`
	Tier       string `json:"tier"` // critical | warning | info
	Enabled    int64  `json:"enabled"` // 0/1
}

// Strategy is the single-row (id=1) strategies table.
type Strategy struct {
	ID                 int64   `json:"id"`
	Promotions         string  `json:"promotions"` // JSON string
	Keywords           string  `json:"keywords"`   // JSON string
	MinSize            int64   `json:"min_size"`
	MaxSize            int64   `json:"max_size"`
	RetireSeeders      int64   `json:"retire_seeders"`
	RetireMinutes      int64   `json:"retire_minutes"`
	RetireRatioEnabled int64   `json:"retire_ratio_enabled"` // 0/1
	RetireRatio        float64 `json:"retire_ratio"`
	RetireMode         string  `json:"retire_mode"` // and | or
	DispatchMode       string  `json:"dispatch_mode"`
	Timezone           string  `json:"timezone"`
	ImageHost          string  `json:"image_host"`          // JSON string
	ImageCoverEnabled  int64   `json:"image_cover_enabled"` // 0/1
	RetryMax           int64   `json:"retry_max"`

	// Disk / low-speed monitor thresholds (migrations/00005_strategy_monitor.sql).
	DiskLowGB           int64  `json:"disk_low_gb"`            // warn below this many free GB
	DiskCriticalGB      int64  `json:"disk_critical_gb"`       // critical below this many free GB
	LowSpeedKbps        int64  `json:"low_speed_kbps"`         // download speed considered "stalled"
	LowSpeedDurationSec int64  `json:"low_speed_duration_sec"` // how long below LowSpeedKbps before acting
	LowSpeedAction      string `json:"low_speed_action"`       // "abort" (delete + retry) or "" (observe only)
}
