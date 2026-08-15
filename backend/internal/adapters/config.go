// Package adapters implements the target-site upload layer for AutoSeedRelay.
//
// Every site is configuration-driven: base URL, announce template, credential
// (passkey / cookie / api token), category overrides, dimension overrides and
// test mode all come from SiteConfig. No real site is hard-coded — the three
// adapters only encode the *architecture* (NexusPHP Laravel API, classic
// NexusPHP form, M-Team Spring API), never a concrete site's taxonomy IDs.
package adapters

import (
	"strings"
	"time"
)

// Adapter type identifiers (the architecture, not the site).
const (
	// TypeNexusPHPAPI is NexusPHP >= 1.9 with the Laravel/Sanctum JSON API.
	TypeNexusPHPAPI = "nexusphp"
	// TypeNexusPHPClassic is a legacy NexusPHP form-based site
	// (upload.php -> takeupload.php).
	TypeNexusPHPClassic = "nexusphp_classic"
	// TypeMTeam is the M-Team Spring Boot API (x-api-key auth).
	TypeMTeam = "mteam"
)

// DefaultTimeout is used when SiteConfig.Timeout is zero.
const DefaultTimeout = 30 * time.Second

// SiteConfig is the complete configuration for one target site. It is the
// single source of truth for how the adapter talks to the site; nothing about
// a concrete site's IDs or URLs is embedded in the adapter code.
type SiteConfig struct {
	// Name is the site identifier used in logs and by the tag mapper.
	Name string `json:"name" yaml:"name"`
	// Type is one of TypeNexusPHPAPI / TypeNexusPHPClassic / TypeMTeam.
	// Empty defaults to TypeNexusPHPAPI.
	Type string `json:"type" yaml:"type"`
	// BaseURL is the site root (or API root for M-Team, e.g. ".../api").
	BaseURL string `json:"base_url" yaml:"base_url"`
	// Announce is the announce URL template. For the NexusPHP family it may
	// contain "{passkey}"; for M-Team it may contain "{credential}".
	Announce string `json:"announce" yaml:"announce"`
	// Passkey is the personal passkey substituted into the announce template.
	Passkey string `json:"passkey,omitempty" yaml:"passkey,omitempty"`
	// Cookie is the login cookie for classic form sites.
	Cookie string `json:"cookie,omitempty" yaml:"cookie,omitempty"`
	// APIToken is the Sanctum bearer token (nexusphp) or the x-api-key (mteam).
	APIToken string `json:"api_token,omitempty" yaml:"api_token,omitempty"`

	// CategoryOverrides maps a category name (any language/case) to the site's
	// numeric category ID. It is the config-driven replacement for the legacy
	// per-site hard-coded category tables.
	CategoryOverrides map[string]int `json:"category_overrides,omitempty" yaml:"category_overrides,omitempty"`
	// DimensionOverrides maps a dimension kind ("standard", "codec",
	// "audiocodec", "source", "medium", "team", "processing") to a
	// {token: id} table. The token follows titler.StandardKeys canonical
	// values (e.g. "2160", "HEVC", "DDP", "WEB-DL"). Matching is
	// case-insensitive and ignores ".", "-", "_" and spaces.
	DimensionOverrides map[string]map[string]int `json:"dimension_overrides,omitempty" yaml:"dimension_overrides,omitempty"`
	// TagsMap maps a source tag name to the target site's tag value
	// (an ID for checkbox/API sites, a preset label for M-Team).
	TagsMap map[string]string `json:"tags_map,omitempty" yaml:"tags_map,omitempty"`

	// FallbackCategory is used when PublishParams.Category is empty and no
	// mapping resolves it. Nil means "no fallback".
	FallbackCategory *int `json:"fallback_category,omitempty" yaml:"fallback_category,omitempty"`

	// TestMode makes Publish a no-op: no network request is sent, and the
	// adapter returns an error wrapping ErrTestMode with the would-be request.
	TestMode bool `json:"test_mode,omitempty" yaml:"test_mode,omitempty"`
	// Timeout is the per-request timeout; zero means DefaultTimeout.
	Timeout time.Duration `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	// UploadPath is the classic-form upload endpoint, default "takeupload.php".
	UploadPath string `json:"upload_path,omitempty" yaml:"upload_path,omitempty"`
}

// normalizeType canonicalizes a site type string and applies the default.
func normalizeType(t string) string {
	switch strings.ToLower(strings.NewReplacer("-", "_", " ", "").Replace(strings.TrimSpace(t))) {
	case "nexusphp", "nexusphp_api", "nexusphpapi", "":
		return TypeNexusPHPAPI
	case "nexusphp_classic", "nexusphpclassic", "classic":
		return TypeNexusPHPClassic
	case "mteam", "m_team", "mteam_api", "mteamapi":
		return TypeMTeam
	default:
		// Return as-is so New can report the unknown type verbatim.
		return strings.TrimSpace(t)
	}
}

// Normalize applies defaults and lower-cases map keys for stable lookup.
func (c *SiteConfig) Normalize() {
	c.Type = normalizeType(c.Type)
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.Type == TypeNexusPHPClassic && strings.TrimSpace(c.UploadPath) == "" {
		c.UploadPath = "takeupload.php"
	}
	c.CategoryOverrides = normalizeIntMap(c.CategoryOverrides)
	c.DimensionOverrides = normalizeDimOverrides(c.DimensionOverrides)
}

// normalizeIntMap trims/keys an int map (used for category overrides).
func normalizeIntMap(m map[string]int) map[string]int {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[normToken(k)] = v
	}
	return out
}

// normalizeDimOverrides normalizes the outer kind keys and inner token keys.
func normalizeDimOverrides(m map[string]map[string]int) map[string]map[string]int {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]map[string]int, len(m))
	for kind, table := range m {
		out[canonicalDimKind(kind)] = normalizeIntMap(table)
	}
	return out
}

// normToken upper-cases a token and strips ".", "-", "_" and whitespace so
// "H.265", "H265" and "h-265" compare equal for override lookup.
func normToken(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.NewReplacer(".", "", "-", "", "_", "", " ", "", "\t", "").Replace(s)
	return s
}

// canonicalDimKind collapses dimension-kind aliases ("codec" / "video_codec" /
// "videoCodec") into one canonical key so PublishParams and DimensionOverrides
// agree regardless of naming convention.
func canonicalDimKind(kind string) string {
	switch normToken(kind) {
	case "VIDEOCODEC", "CODEC", "VIDEOENCODING":
		return "codec"
	case "AUDIOCODEC", "AUDIOENCODING":
		return "audiocodec"
	default:
		return normToken(kind) // STANDARD / SOURCE / MEDIUM / TEAM / PROCESSING
	}
}

// dimKindAliases returns the candidate lookup keys for a canonical dimension
// kind, used to find overrides configured under any of its aliases.
func dimKindAliases(kind string) []string {
	c := canonicalDimKind(kind)
	switch c {
	case "codec":
		return []string{"codec", "video_codec", "videocodec"}
	case "audiocodec":
		return []string{"audiocodec", "audio_codec", "audiocodec"}
	default:
		return []string{c}
	}
}

// BuildAnnounce renders the target announce URL from SiteConfig, substituting
// credentials. For M-Team the {credential} placeholder is replaced by a
// harmless PLACEHOLDER token (the server rewrites it on upload). For the
// NexusPHP family {passkey} is substituted, or appended as ?passkey=.
func BuildAnnounce(cfg SiteConfig) string {
	cfg.Normalize()
	base := strings.TrimSpace(cfg.Announce)
	if cfg.Type == TypeMTeam {
		if base == "" {
			return ""
		}
		return strings.ReplaceAll(base, "{credential}", "PLACEHOLDER")
	}
	if base == "" {
		if strings.TrimSpace(cfg.BaseURL) == "" {
			return ""
		}
		base = strings.TrimRight(cfg.BaseURL, "/") + "/announce.php"
	}
	if strings.Contains(base, "{passkey}") {
		return strings.ReplaceAll(base, "{passkey}", cfg.Passkey)
	}
	if cfg.Passkey != "" && !strings.Contains(base, "passkey=") {
		sep := "&"
		if !strings.Contains(base, "?") {
			sep = "?"
		}
		return base + sep + "passkey=" + cfg.Passkey
	}
	return base
}
