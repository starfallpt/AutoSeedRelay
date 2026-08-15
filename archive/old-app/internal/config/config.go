// Package config implements the site-profile / configuration layer for
// the relay: loading SiteProfile source/target definitions from YAML or
// JSON, substituting <PUT_ENV_XXX> placeholders, applying AUTOSEED_*
// environment overrides, building a config purely from environment
// variables, and writing an example config.
//
// Design notes matching the Python original:
//   - All per-site differences (domain, RSS, announce, auth fields) are
//     converged into SiteProfile; downstream code consumes only profiles.
//   - Config files carry only placeholders or empty values; real
//     credentials come from environment variables. Environment values
//     take precedence over file values.
//   - YAML is preferred, JSON is the fallback (default search order:
//     config/relay.yaml then config/relay.json).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// RoleSource marks a site as a relay source.
	RoleSource = "source"
	// RoleTarget marks a site as a relay target.
	RoleTarget = "target"
)

// ConfigError is a user-facing configuration load/validation error.
type ConfigError struct{ msg string }

// Error implements the error interface.
func (e *ConfigError) Error() string { return e.msg }

func configErrf(format string, args ...any) error {
	return &ConfigError{fmt.Sprintf(format, args...)}
}

var validRoles = map[string]bool{RoleSource: true, RoleTarget: true}

// defaultConfigPaths are the config files tried, in order, when no
// explicit path is given.
var defaultConfigPaths = []string{"config/relay.yaml", "config/relay.json"}

// globalEnvKeys are the global (non-site) AUTOSEED_* environment keys.
var globalEnvKeys = []string{"POLL_INTERVAL", "KEYWORDS", "DB_PATH", "OUT_DIR", "TARGET_ANNOUNCE"}

type envAlias struct {
	alias string
	field string
}

// envFieldAliases maps environment-variable short aliases to SiteProfile
// fields, under the AUTOSEED_<SITE>_<FIELD> convention. Later aliases for
// the same field take precedence (the longer/more explicit spelling
// wins). Order is significant and is preserved from the Python original.
var envFieldAliases = []envAlias{
	{"PASSKEY", "passkey"},
	{"COOKIE", "cookie"},
	{"ROLE", "role"},
	{"NAME", "name"},
	{"BASEURL", "base_url"},
	{"BASE_URL", "base_url"},
	{"RSS", "rss_url"},
	{"RSS_URL", "rss_url"},
	{"ANNOUNCE", "announce_url"},
	{"ANNOUNCE_URL", "announce_url"},
	{"TOKEN", "api_token"},
	{"API_TOKEN", "api_token"},
	{"AUTH", "mteam_auth"},
}

// siteFields are the known per-site config keys; any other key becomes
// part of SiteProfile.Extra.
var siteFields = map[string]bool{
	"name":         true,
	"role":         true,
	"base_url":     true,
	"rss_url":      true,
	"announce_url": true,
	"api_token":    true,
	"mteam_auth":   true,
	"cookie":       true,
}

var (
	putEnvRE   = regexp.MustCompile(`<PUT_ENV_([A-Za-z0-9_]+)>`)
	nonAlnumRE = regexp.MustCompile(`[^A-Za-z0-9]+`)
	passkeyRE  = regexp.MustCompile(`(?i)([?&]passkey=)[^&]*`)
)

// SiteProfile holds all configuration for one site (source or target).
type SiteProfile struct {
	Name        string         `yaml:"name" json:"name"`
	Role        string         `yaml:"role" json:"role"`
	BaseURL     string         `yaml:"base_url" json:"base_url"`
	RSSURL      string         `yaml:"rss_url,omitempty" json:"rss_url,omitempty"`
	AnnounceURL string         `yaml:"announce_url,omitempty" json:"announce_url,omitempty"`
	APIToken    string         `yaml:"api_token,omitempty" json:"api_token,omitempty"`
	MTeamAuth   string         `yaml:"mteam_auth,omitempty" json:"mteam_auth,omitempty"`
	Cookie      string         `yaml:"cookie,omitempty" json:"cookie,omitempty"`
	Passkey     string         `yaml:"passkey,omitempty" json:"passkey,omitempty"`
	Extra       map[string]any `yaml:"-" json:"-"`
}

// RelayConfig is the full relay configuration: sources + targets +
// global parameters.
type RelayConfig struct {
	Sources        []*SiteProfile `yaml:"sources" json:"sources"`
	Targets        []*SiteProfile `yaml:"targets" json:"targets"`
	PollInterval   float64        `yaml:"poll_interval" json:"poll_interval"`
	Keywords       []string       `yaml:"keywords" json:"keywords"`
	DBPath         string         `yaml:"db_path" json:"db_path"`
	OutDir         string         `yaml:"out_dir" json:"out_dir"`
	TargetAnnounce string         `yaml:"target_announce" json:"target_announce"`
}

// AllSites returns sources and targets concatenated for unified
// traversal.
func (c *RelayConfig) AllSites() []*SiteProfile {
	out := make([]*SiteProfile, 0, len(c.Sources)+len(c.Targets))
	out = append(out, c.Sources...)
	out = append(out, c.Targets...)
	return out
}

func newRelayConfig() *RelayConfig {
	return &RelayConfig{
		Sources:      []*SiteProfile{},
		Targets:      []*SiteProfile{},
		PollInterval: 300.0,
		Keywords:     []string{"StarfallWeb", "LongWeb"},
		DBPath:       "data/relay.db",
		OutDir:       "data/out",
	}
}

// findPlaceholder returns the variable name of the first <PUT_ENV_XXX>
// placeholder in s, or "" when there is none.
func findPlaceholder(s string) string {
	m := putEnvRE.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return m[1]
}

// substituteEnvPlaceholders recursively replaces <PUT_ENV_XXX>
// placeholders in strings with the corresponding environment variable
// value. When the variable is unset or empty, the placeholder is kept so
// later validation can report which variable is missing.
func substituteEnvPlaceholders(obj any, getenv func(string) string) any {
	switch v := obj.(type) {
	case string:
		return putEnvRE.ReplaceAllStringFunc(v, func(m string) string {
			sub := putEnvRE.FindStringSubmatch(m)
			if sub == nil {
				return m
			}
			if val := getenv(sub[1]); val != "" {
				return val
			}
			return m
		})
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = substituteEnvPlaceholders(item, getenv)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = substituteEnvPlaceholders(item, getenv)
		}
		return out
	default:
		return obj
	}
}

// parseKeywords accepts a YAML list or a comma-separated string; empty
// input yields the default keywords.
func parseKeywords(v any) ([]string, error) {
	switch val := v.(type) {
	case nil:
		return []string{"StarfallWeb", "LongWeb"}, nil
	case string:
		if val == "" {
			return []string{"StarfallWeb", "LongWeb"}, nil
		}
		var out []string
		for _, part := range strings.Split(val, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
		return out, nil
	case []any:
		out := make([]string, 0, len(val))
		for _, item := range val {
			out = append(out, fmt.Sprintf("%v", item))
		}
		return out, nil
	default:
		return nil, configErrf("keywords 类型非法: %T(应为列表或逗号分隔字符串)", v)
	}
}

// siteToken converts a site name into its uppercase environment-variable
// token: "m-team" -> "M_TEAM", "SOURCE" -> "SOURCE".
func siteToken(name string) string {
	t := nonAlnumRE.ReplaceAllString(name, "_")
	t = strings.Trim(t, "_")
	return strings.ToUpper(t)
}

// applyPasskey injects a passkey into rss_url and records it in Extra.
// It supports {passkey} / <passkey> placeholders and also rewrites an
// existing passkey= query parameter (env value wins).
func applyPasskey(site *SiteProfile, value string) {
	if site.RSSURL != "" {
		url := site.RSSURL
		url = strings.ReplaceAll(url, "{passkey}", value)
		url = strings.ReplaceAll(url, "<passkey>", value)
		url = passkeyRE.ReplaceAllString(url, "${1}"+value)
		site.RSSURL = url
	}
	if site.Extra == nil {
		site.Extra = map[string]any{}
	}
	site.Extra["passkey"] = value
}

// applyEnvOverrides overrides site fields from environment variables
// using the AUTOSEED_<SITE>_<FIELD> convention (default prefix
// AUTOSEED_). Present environment variables take precedence over file
// values.
func applyEnvOverrides(cfg *RelayConfig, prefix string, lookup func(string) (string, bool)) error {
	for _, site := range cfg.AllSites() {
		token := siteToken(site.Name)
		effective := map[string]string{}
		var order []string
		for _, a := range envFieldAliases {
			key := prefix + token + "_" + a.alias
			if value, ok := lookup(key); ok {
				if _, seen := effective[a.field]; !seen {
					order = append(order, a.field)
				}
				effective[a.field] = value
			}
		}
		for _, fieldName := range order {
			value := effective[fieldName]
			switch fieldName {
			case "passkey":
				applyPasskey(site, value)
			case "role":
				site.Role = strings.ToLower(strings.TrimSpace(value))
			case "name":
				site.Name = strings.TrimSpace(value)
			default:
				setSiteField(site, fieldName, value)
			}
		}
	}
	return nil
}

func setSiteField(site *SiteProfile, field, value string) {
	switch field {
	case "base_url":
		site.BaseURL = value
	case "rss_url":
		site.RSSURL = value
	case "announce_url":
		site.AnnounceURL = value
	case "api_token":
		site.APIToken = value
	case "mteam_auth":
		site.MTeamAuth = value
	case "cookie":
		site.Cookie = value
	}
}

// collectUnresolved recursively gathers the names of any <PUT_ENV_XXX>
// placeholders that remain in a value.
func collectUnresolved(v any) []string {
	switch val := v.(type) {
	case string:
		var out []string
		for _, m := range putEnvRE.FindAllStringSubmatch(val, -1) {
			out = append(out, m[1])
		}
		return out
	case []any:
		var out []string
		for _, item := range val {
			out = append(out, collectUnresolved(item)...)
		}
		return out
	case map[string]any:
		var out []string
		for _, item := range val {
			out = append(out, collectUnresolved(item)...)
		}
		return out
	default:
		return nil
	}
}

func uniqueSorted(items []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, it := range items {
		if !seen[it] {
			seen[it] = true
			out = append(out, it)
		}
	}
	sort.Strings(out)
	return out
}

func siteFieldValue(site *SiteProfile, field string) string {
	switch field {
	case "name":
		return site.Name
	case "role":
		return site.Role
	case "base_url":
		return site.BaseURL
	case "rss_url":
		return site.RSSURL
	case "announce_url":
		return site.AnnounceURL
	case "api_token":
		return site.APIToken
	case "mteam_auth":
		return site.MTeamAuth
	case "cookie":
		return site.Cookie
	}
	return ""
}

// validate runs the centralized checks: at least one site, valid roles,
// base_url present without placeholders, and no unresolved placeholders
// anywhere in a site's fields or Extra.
func validate(cfg *RelayConfig) error {
	if len(cfg.Sources) == 0 && len(cfg.Targets) == 0 {
		return configErrf("配置为空:sources 与 targets 都没有站点")
	}
	sections := []struct {
		name  string
		sites []*SiteProfile
	}{
		{"sources", cfg.Sources},
		{"targets", cfg.Targets},
	}
	for _, sec := range sections {
		for _, site := range sec.sites {
			if !validRoles[site.Role] {
				return configErrf("%s 站点 %q 的 role 非法: %q,允许取值 source|target", sec.name, site.Name, site.Role)
			}
			if strings.TrimSpace(site.BaseURL) == "" {
				return configErrf("%s 站点 %q 缺少 base_url", sec.name, site.Name)
			}
			if ph := findPlaceholder(site.BaseURL); ph != "" {
				return configErrf("%s 站点 %q 的 base_url 仍含未替换的环境变量占位符,请设置环境变量 %s", sec.name, site.Name, ph)
			}
			var missing []string
			for _, fname := range []string{"name", "role", "base_url", "rss_url", "announce_url", "api_token", "mteam_auth", "cookie"} {
				missing = append(missing, collectUnresolved(siteFieldValue(site, fname))...)
			}
			missing = append(missing, collectUnresolved(site.Extra)...)
			missing = uniqueSorted(missing)
			if len(missing) > 0 {
				return configErrf("%s 站点 %q 含未替换的环境变量占位符,请先设置: %s", sec.name, site.Name, strings.Join(missing, ", "))
			}
		}
	}
	return nil
}

func toFloat(v any) (float64, error) {
	switch n := v.(type) {
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case float64:
		return n, nil
	case float32:
		return float64(n), nil
	case string:
		return strconv.ParseFloat(n, 64)
	default:
		return 0, fmt.Errorf("not a number: %v", v)
	}
}

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	case bool:
		return t
	default:
		return true
	}
}

func resolvePath(path string) (string, error) {
	if path != "" {
		if _, err := os.Stat(path); err != nil {
			return "", configErrf("配置文件不存在: %s\n请先创建(模板: config/sites.example.yaml,或运行 SaveExample())。", path)
		}
		return path, nil
	}
	for _, candidate := range defaultConfigPaths {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", configErrf("未找到配置文件:默认查找 config/relay.yaml 或 config/relay.json。\n请创建其一,或显式调用 LoadConfig(path=...)。\n模板见 config/sites.example.yaml(敏感值走环境变量)。")
}

func readRaw(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, configErrf("读取配置文件 %s 失败: %v", path, err)
	}
	suffix := strings.ToLower(filepath.Ext(path))
	var data map[string]any
	switch suffix {
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(raw, &data); err != nil {
			return nil, configErrf("解析 YAML 配置 %s 失败: %v", path, err)
		}
	case ".json":
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, configErrf("解析 JSON 配置 %s 失败: %v", path, err)
		}
	default:
		// No extension: try YAML first, fall back to JSON.
		if err := yaml.Unmarshal(raw, &data); err != nil {
			if err2 := json.Unmarshal(raw, &data); err2 != nil {
				return nil, configErrf("解析配置 %s 失败(YAML/JSON): %v", path, err)
			}
		}
	}
	return data, nil
}

func siteList(raw map[string]any, key string) ([]any, error) {
	v, ok := raw[key]
	if !ok || v == nil {
		return nil, nil
	}
	lst, ok := v.([]any)
	if !ok {
		return nil, configErrf("配置段 %q 必须是列表,得到: %T", key, v)
	}
	return lst, nil
}

func siteFromDict(section string, idx int, d any, defaultRole string) (*SiteProfile, error) {
	data, ok := d.(map[string]any)
	if !ok {
		return nil, configErrf("%s[%d] 必须是映射(dict),得到: %T", section, idx, d)
	}
	name := ""
	if v, ok := data["name"].(string); ok {
		name = v
	}
	if strings.TrimSpace(name) == "" {
		return nil, configErrf("%s[%d] 缺少 name 字段", section, idx)
	}
	role := defaultRole
	if v, ok := data["role"]; ok && v != nil {
		if r := strings.ToLower(strings.TrimSpace(fmt.Sprintf("%v", v))); r != "" {
			role = r
		}
	}

	site := &SiteProfile{
		Name:  name,
		Role:  role,
		Extra: map[string]any{},
	}
	if v, ok := data["base_url"]; ok && v != nil {
		site.BaseURL = fmt.Sprintf("%v", v)
	}
	if v, ok := data["rss_url"]; ok && v != nil {
		site.RSSURL = fmt.Sprintf("%v", v)
	}
	if v, ok := data["announce_url"]; ok && v != nil {
		site.AnnounceURL = fmt.Sprintf("%v", v)
	}
	if v, ok := data["api_token"]; ok && v != nil {
		site.APIToken = fmt.Sprintf("%v", v)
	}
	if v, ok := data["mteam_auth"]; ok && v != nil {
		site.MTeamAuth = fmt.Sprintf("%v", v)
	}
	if v, ok := data["cookie"]; ok && v != nil {
		site.Cookie = fmt.Sprintf("%v", v)
	}
	if v, ok := data["extra"]; ok && v != nil {
		if em, ok := v.(map[string]any); ok {
			for k, val := range em {
				site.Extra[k] = val
			}
		}
	}
	for k, val := range data {
		if k != "extra" && !siteFields[k] {
			site.Extra[k] = val
		}
	}
	if pk, ok := site.Extra["passkey"]; ok && pk != nil {
		applyPasskey(site, fmt.Sprintf("%v", pk))
	}
	return site, nil
}

func fromDict(raw any) (*RelayConfig, error) {
	if raw == nil {
		raw = map[string]any{}
	}
	rawMap, ok := raw.(map[string]any)
	if !ok {
		return nil, configErrf("配置顶层必须是映射(dict),得到: %T", raw)
	}
	cfg := newRelayConfig()

	srcs, err := siteList(rawMap, "sources")
	if err != nil {
		return nil, err
	}
	for i, s := range srcs {
		site, err := siteFromDict("sources", i, s, RoleSource)
		if err != nil {
			return nil, err
		}
		cfg.Sources = append(cfg.Sources, site)
	}

	tgts, err := siteList(rawMap, "targets")
	if err != nil {
		return nil, err
	}
	for i, s := range tgts {
		site, err := siteFromDict("targets", i, s, RoleTarget)
		if err != nil {
			return nil, err
		}
		cfg.Targets = append(cfg.Targets, site)
	}

	if v, ok := rawMap["poll_interval"]; ok && v != nil {
		f, err := toFloat(v)
		if err != nil {
			return nil, configErrf("poll_interval 必须是数字,得到: %v", v)
		}
		cfg.PollInterval = f
	}
	if v, ok := rawMap["keywords"]; ok {
		kw, err := parseKeywords(v)
		if err != nil {
			return nil, err
		}
		cfg.Keywords = kw
	}
	if v, ok := rawMap["db_path"]; ok && truthy(v) {
		cfg.DBPath = fmt.Sprintf("%v", v)
	}
	if v, ok := rawMap["out_dir"]; ok && truthy(v) {
		cfg.OutDir = fmt.Sprintf("%v", v)
	}
	if v, ok := rawMap["target_announce"]; ok && truthy(v) {
		cfg.TargetAnnounce = fmt.Sprintf("%v", v)
	}
	return cfg, nil
}

// LoadConfig loads configuration from a file (YAML preferred, JSON
// fallback). An empty path selects the default search order
// (config/relay.yaml then config/relay.json). The load flow is:
// read file, substitute <PUT_ENV_XXX> placeholders, apply AUTOSEED_*
// environment overrides (env wins), then validate.
func LoadConfig(path string) (*RelayConfig, error) {
	p, err := resolvePath(path)
	if err != nil {
		return nil, err
	}
	raw, err := readRaw(p)
	if err != nil {
		return nil, err
	}
	sub := substituteEnvPlaceholders(raw, os.Getenv)
	cfg, err := fromDict(sub)
	if err != nil {
		return nil, err
	}
	if err := applyEnvOverrides(cfg, "AUTOSEED_", os.LookupEnv); err != nil {
		return nil, err
	}
	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// FromEnv builds a RelayConfig purely from environment variables using
// the AUTOSEED_* convention (prefix defaults to "AUTOSEED_"). Global
// keys are POLL_INTERVAL/KEYWORDS/DB_PATH/OUT_DIR/TARGET_ANNOUNCE; sites
// are discovered from AUTOSEED_<SITE>_BASE_URL and related aliases. A
// site's role is inferred from SOURCE/TARGET in its token when ROLE is
// not set.
func FromEnv(prefix string) (*RelayConfig, error) {
	if prefix == "" {
		prefix = "AUTOSEED_"
	}
	cfg := newRelayConfig()

	get := func(name string) string { return os.Getenv(prefix + name) }

	if poll := get("POLL_INTERVAL"); poll != "" {
		f, err := strconv.ParseFloat(poll, 64)
		if err != nil {
			return nil, configErrf("%sPOLL_INTERVAL 必须是数字,得到: %q", prefix, poll)
		}
		cfg.PollInterval = f
	}
	if kw := get("KEYWORDS"); kw != "" {
		kws, err := parseKeywords(kw)
		if err != nil {
			return nil, err
		}
		cfg.Keywords = kws
	}
	if v := get("DB_PATH"); v != "" {
		cfg.DBPath = v
	}
	if v := get("OUT_DIR"); v != "" {
		cfg.OutDir = v
	}
	if v := get("TARGET_ANNOUNCE"); v != "" {
		cfg.TargetAnnounce = v
	}

	reserved := map[string]bool{}
	for _, k := range globalEnvKeys {
		reserved[prefix+k] = true
	}

	// Aliases matched longest-first (stable for equal lengths).
	aliasByLength := make([]string, 0, len(envFieldAliases))
	for _, a := range envFieldAliases {
		aliasByLength = append(aliasByLength, a.alias)
	}
	sort.SliceStable(aliasByLength, func(i, j int) bool {
		return len(aliasByLength[i]) > len(aliasByLength[j])
	})

	sites := map[string]map[string]string{}
	for _, kv := range os.Environ() {
		key, value, _ := strings.Cut(kv, "=")
		if reserved[key] || !strings.HasPrefix(key, prefix) {
			continue
		}
		rest := key[len(prefix):]
		for _, alias := range aliasByLength {
			if strings.HasSuffix(rest, "_"+alias) {
				token := rest[:len(rest)-len(alias)-1]
				if token == "" {
					continue
				}
				if sites[token] == nil {
					sites[token] = map[string]string{}
				}
				sites[token][alias] = value
				break
			}
		}
	}

	for token, data := range sites {
		baseURL := firstNonEmpty(data["BASE_URL"], data["BASEURL"])
		if baseURL == "" {
			// Only enhancement fields (e.g. PASSKEY) without BASE_URL do
			// not form a site.
			continue
		}
		role := strings.ToLower(strings.TrimSpace(data["ROLE"]))
		if role == "" {
			switch {
			case strings.Contains(strings.ToUpper(token), "SOURCE"):
				role = RoleSource
			case strings.Contains(strings.ToUpper(token), "TARGET"):
				role = RoleTarget
			default:
				return nil, configErrf("环境变量 %s%s_BASE_URL 标识的站点 %s 无法推断 role,请显式设置 %s%s_ROLE=source|target", prefix, token, token, prefix, token)
			}
		}
		name := strings.TrimSpace(data["NAME"])
		if name == "" {
			name = strings.ToLower(token)
		}
		site := &SiteProfile{
			Name:    name,
			Role:    role,
			BaseURL: baseURL,
			Extra:   map[string]any{},
		}
		site.RSSURL = firstNonEmpty(data["RSS"], data["RSS_URL"])
		site.AnnounceURL = firstNonEmpty(data["ANNOUNCE"], data["ANNOUNCE_URL"])
		site.APIToken = firstNonEmpty(data["TOKEN"], data["API_TOKEN"])
		site.MTeamAuth = data["AUTH"]
		site.Cookie = data["COOKIE"]
		if pk := data["PASSKEY"]; pk != "" {
			applyPasskey(site, pk)
		}
		if role == RoleSource {
			cfg.Sources = append(cfg.Sources, site)
		} else {
			cfg.Targets = append(cfg.Targets, site)
		}
	}

	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// SaveExample writes the example config (placeholders only; real
// credentials come from the environment) and returns the written path.
func SaveExample(path string) (string, error) {
	if path == "" {
		path = "config/sites.example.yaml"
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("config: create dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(exampleYAML), 0o644); err != nil {
		return "", fmt.Errorf("config: write example: %w", err)
	}
	return path, nil
}

const exampleYAML = `# AutoSeedRelay 站点配置示例(提交到 git,只含占位符,无真实凭据)
#
# 用法:
#   1. 复制为 config/relay.yaml(或 config/relay.json)后按需修改;
#   2. 敏感值(passkey/token/cookie)不要填在文件里 —— 用环境变量:
#      - 方式一(推荐):值里写占位符 <PUT_ENV_<环境变量名>>,加载时自动替换;
#        例如 rss_url 里的 <PUT_ENV_AUTOSEED_SOURCE_PASSKEY>。
#      - 方式二:字段留空占位,再设环境变量 AUTOSEED_<SITE>_<FIELD> 覆盖,
#        环境变量值优先于文件值。
#        常用: AUTOSEED_SOURCE_PASSKEY / AUTOSEED_MTEAM_AUTH / AUTOSEED_SOURCE_COOKIE
#
# 注意:
#   - .gitignore 已忽略 config/local.* 与 .env;
#     真实凭据只放环境变量,真实配置(非敏感部分)可放 config/local.yaml;
#   - 站点名不要含下划线(环境变量命名约定 AUTOSEED_<SITE>_<FIELD> 以 _ 分隔)。

# 源站(负责抓取 RSS / 下载种子)
sources:
  - name: SOURCE
    base_url: https://dev.internal-source.org
    rss_url: https://dev.internal-source.org/torrentrss.php?passkey=<PUT_ENV_AUTOSEED_SOURCE_PASSKEY>&rows=20&linktype=dl
    # api_token: <PUT_ENV_AUTOSEED_SOURCE_TOKEN>   # NexusPHP API,可选
    # cookie: <PUT_ENV_AUTOSEED_SOURCE_COOKIE>     # 可选

# 目标站(负责清洗后上传)
targets:
  - name: mteam
    base_url: https://api.m-team.cc/api
    announce_url: https://tracker.m-team.cc/announce?credential={credential}
    mteam_auth: <PUT_ENV_AUTOSEED_MTEAM_AUTH>
    # api_token: <PUT_ENV_AUTOSEED_MTEAM_TOKEN>       # 可选
    # cookie: <PUT_ENV_AUTOSEED_MTEAM_COOKIE>         # 可选

# 全局参数(可选,均为默认值)
# poll_interval: 300               # 轮询间隔(秒)
# keywords: [StarfallWeb, LongWeb] # 标题命中关键词;也可写 "StarfallWeb, LongWeb"
# db_path: data/relay.db           # 去重/状态 SQLite 路径
# out_dir: data/out                # 种子输出目录
# target_announce: ""              # 清洗时写入的默认目标 announce(一般留空)
`
