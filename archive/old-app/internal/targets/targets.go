package targets

import (
	"fmt"
	"sort"
	"strings"

	"github.com/autoseedrelay/go-relay/internal/parser"
)

// TargetSite 目标站上传适配器抽象接口。
// 对应 Python targets/base.py 的 TargetSite(ABC)。
type TargetSite interface {
	// SiteType 目标站类型标识:"nexusphp" / "nexusphp_classic" / "mteam"。
	SiteType() string
	// SiteName 站点标识(如 "SOURCE-nexus" / "mteam")。
	SiteName() string
	// UploadTorrent 上传清洗后的 .torrent。fields 为目标站表单字段(已含 category)。
	UploadTorrent(torrentPath string, fields map[string]any) (UploadResult, error)
	// ParseFieldsFromTorrent 由 ParsedTorrent 生成可覆盖的字段基础(不含 category 映射)。
	ParseFieldsFromTorrent(parsed *parser.ParsedTorrent) map[string]any
	// BuildAnnounce 目标站 announce URL。
	BuildAnnounce() string
	// Categories 当前分类映射缓存 {分类名: id}。
	Categories() map[string]int
	// SetCategories 注入分类映射缓存。
	SetCategories(cats map[string]int)
}

// CategoriesLoader 可选接口:能从站点拉取分类映射。
type CategoriesLoader interface {
	LoadCategories() map[string]int
}

// TeamProvider 可选接口:能提供制作组枚举 {制作组名: id}。
type TeamProvider interface {
	Teams() map[string]int
}

// Configurable 可选接口:从 cfg 覆盖适配器配置字段。
type Configurable interface {
	ApplyConfig(cfg map[string]any)
}

// 目标站类型 → 适配器构造器。
var targetRegistry = map[string]func() TargetSite{
	"nexusphp":         newNexusPHPAPI,
	"nexusphp_classic": newNexusPHPClassic,
	"mteam":            newMTeamAPI,
}

// extraKeys 从 cfg 提取的覆盖字段。
var extraKeys = []string{
	"category", "fallback_category", "small_descr", "smallDescr",
	"url", "imdb", "douban", "source", "medium", "codec", "standard",
	"processing", "team", "audiocodec", "tags", "uplver", "anonymous",
	"countries", "labels", "mediainfo",
}

// metaOverrideKeys meta 里的覆盖字段(优先于 cfg)。
var metaOverrideKeys = []string{
	"name", "descr", "small_descr", "imdb", "url", "category_name",
	"category", "anonymous", "countries", "labels", "mediainfo", "tags",
}

// Upload 顶层统一上传入口(pipeline / CLI 的对接约定)。
//
//	torrentPath: 清洗后 .torrent 文件路径
//	meta: 种子上文信息(可选),含 "parsed"(*parser.ParsedTorrent)或平铺字段
//	      (name/descr/small_descr/imdb/category_name 等)
//	cfg: 目标站配置 dict(可选),含 "target"(nexusphp|mteam)、"base_url"、
//	      "api_token"/"auth_token"、"announce_base"、"site_name"、"passkey"、
//	      "fallback_category" 等
//
// 返回 (ok, targetID, targetSite, err)。失败返回 *UploadError(带 existing 标记),
// 由调用方决定降级策略。
func Upload(torrentPath string, meta, cfg map[string]any) (bool, int, string, error) {
	if meta == nil {
		meta = map[string]any{}
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	target := getStr(cfg, "target", "target_type")
	if target == "" {
		target = "nexusphp"
	}
	factory, ok := targetRegistry[target]
	if !ok {
		keys := make([]string, 0, len(targetRegistry))
		for k := range targetRegistry {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return false, 0, "", newUploadError(
			fmt.Sprintf("未知目标站类型: %q(可选: %s)", target, strings.Join(keys, ", ")), 0, "", false)
	}
	site := factory()
	if c, ok := site.(Configurable); ok {
		c.ApplyConfig(cfg)
	}

	// 从 cfg 取 extra 覆盖字段(分类名 → ID 的映射在 BuildUploadFields 内完成)
	extra := map[string]any{}
	for _, k := range extraKeys {
		if v, ok := cfg[k]; ok {
			extra[k] = v
		}
	}
	// meta 里的覆盖字段(优先于 cfg;含小写别名与 category_name)
	for _, k := range metaOverrideKeys {
		if v, ok := meta[k]; ok && v != nil {
			if s, isStr := v.(string); !isStr || s != "" {
				extra[k] = v
			}
		}
	}

	// 尝试加载分类映射(失败静默,走 fallback)
	if loader, ok := site.(CategoriesLoader); ok {
		cats := loader.LoadCategories()
		site.SetCategories(cats)
	}

	// 构造上传字段(必填字段齐全;缺失分类由 fallback 兜底)
	parsed := metaParsed(meta)
	if parsed == nil {
		p, err := parser.ParseTorrent(torrentPath)
		if err != nil {
			return false, 0, site.SiteName(), newUploadError("解析种子失败: "+err.Error(), 0, "", false)
		}
		parsed = p
	}

	fields := BuildUploadFields(parsed, site, extra)
	if _, hasType := fields["type"]; !hasType {
		if _, hasCat := fields["category"]; !hasCat {
			return false, 0, site.SiteName(), newUploadError(
				"无法确定分类:缺少 category/fallback_category,且未能从站点映射", 0, "", false)
		}
	}

	result, err := site.UploadTorrent(torrentPath, fields)
	if err != nil {
		return false, 0, site.SiteName(), err
	}
	targetID := 0
	if result.TargetID != nil {
		targetID = *result.TargetID
	}
	return result.OK, targetID, site.SiteName(), nil
}

func metaParsed(meta map[string]any) *parser.ParsedTorrent {
	if p, ok := meta["parsed"].(*parser.ParsedTorrent); ok && p != nil {
		return p
	}
	return nil
}

// TargetTypes 返回支持的目标站类型列表(排序后)。
func TargetTypes() []string {
	keys := make([]string, 0, len(targetRegistry))
	for k := range targetRegistry {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// NewTargetSite 按类型构造目标站实例并应用 cfg(供 CLI / dry-run / 测试使用)。
func NewTargetSite(target string, cfg map[string]any) (TargetSite, error) {
	factory, ok := targetRegistry[target]
	if !ok {
		return nil, newUploadError(
			fmt.Sprintf("未知目标站类型: %q(可选: %s)", target, strings.Join(TargetTypes(), ", ")), 0, "", false)
	}
	site := factory()
	if c, ok := site.(Configurable); ok {
		c.ApplyConfig(cfg)
	}
	return site, nil
}
