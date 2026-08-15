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

// Source is one upstream source site (row in sources).
type Source struct {
	ID          int64
	Name        string
	Role        string
	BaseURL     string
	RSSURL      string
	AnnounceURL string
	Status      string // active | paused
	FailCount   int64

	// Credential fields (enc_cookie / enc_passkey / enc_api_token), plaintext.
	Cookie   string
	Passkey  string
	APIToken string

	CreatedAt int64
	UpdatedAt int64
}

// Target is one downstream PT site (row in targets).
type Target struct {
	ID          int64
	Name        string
	Type        string // nexusphp | nexusphp_classic | mteam
	Version     string // api | classic
	BaseURL     string
	AnnounceURL string
	TestMode    int64 // 0/1

	FallbackCategory   string
	CategoryOverrides  string // JSON string
	DimensionOverrides string // JSON string
	TagsMap            string // JSON string

	// Credential fields (enc_cookie / enc_passkey / enc_api_token), plaintext.
	Cookie   string
	Passkey  string
	APIToken string

	Status    string // active | paused
	CreatedAt int64
	UpdatedAt int64
}

// QBInstance is one qBittorrent instance (row in qb_instances).
type QBInstance struct {
	ID       int64
	Name     string
	Host     string
	Port     int64
	Username string

	// Password is the plaintext qB password (enc_password column).
	Password string

	Priority   int64
	Enabled    int64  // 0/1
	LastSeenAt int64  // Unix seconds; 0 = never seen
	Extra      string // JSON string

	CreatedAt int64
	UpdatedAt int64
}

// Seed is one discovered torrent (row in seeds). Unique on (source_site, info_hash).
type Seed struct {
	ID         int64
	SourceSite string
	InfoHash   string
	Title      string
	Size       int64
	Category   string
	Promotion  string
	SourceID   int64 // 0 = no source row
	Status     string
	Error      string
	RetryCount int64

	DiscoveredAt int64 // Unix seconds
	UpdatedAt    int64
}

// RelayRecord is one per-target publish/seeding attempt for a seed
// (row in relay_records). Unique on (seed_id, target_id).
type RelayRecord struct {
	ID              int64
	SeedID          int64
	TargetID        int64
	Role            string // publisher | seeder
	Status          string
	TargetTorrentID string
	Attempts        int64
	LastError       string
	PublishedAt     int64 // Unix seconds; 0 = not published
	RetiredAt       int64 // Unix seconds; 0 = not retired
	RetireReason    string

	CreatedAt int64
	UpdatedAt int64
}

// Replica is one seed copy living on a qB instance (row in seed_replicas).
// Unique on (seed_id, qb_id, role).
type Replica struct {
	ID       int64
	SeedID   int64
	QBID     int64
	InfoHash string
	Role     string // origin | cross
	Status   string
	Progress float64
	AddedAt  int64 // Unix seconds
}

// Activity is one activity_log row.
type Activity struct {
	ID        int64
	SeedID    int64 // 0 = not seed-scoped
	Level     string
	Action    string
	Detail    string
	CreatedAt int64
}

// NotifierInstance is one notifier provider instance (row in notifier_instances).
type NotifierInstance struct {
	ID   int64
	Type string // webhook | telegram | smtp | ntfy | gotify | serverchan | pushplus
	Name string

	// Config is the plaintext instance config JSON (enc_config column).
	Config string

	Enabled   int64 // 0/1
	CreatedAt int64
	UpdatedAt int64
}

// Route is one notifier_routes row (instance × tier matrix cell).
type Route struct {
	InstanceID int64
	Tier       string // critical | warning | info
	Enabled    int64  // 0/1
}

// Strategy is the single-row (id=1) strategies table.
type Strategy struct {
	ID                 int64
	Promotions         string // JSON string
	Keywords           string // JSON string
	MinSize            int64
	MaxSize            int64
	RetireSeeders      int64
	RetireMinutes      int64
	RetireRatioEnabled int64 // 0/1
	RetireRatio        float64
	RetireMode         string // and | or
	DispatchMode       string
	Timezone           string
	ImageHost          string // JSON string
	ImageCoverEnabled  int64  // 0/1
	RetryMax           int64
}
