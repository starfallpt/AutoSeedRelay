package targets

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/autoseedrelay/go-relay/internal/parser"
)

// NexusPHPClassic 传统 NexusPHP(表单上传)目标站,如北洋园 TJUPT。
type NexusPHPClassic struct {
	name         string
	baseURL      string
	cookie       string
	siteName     string
	announceBase string
	passkey      string
	uploadPath   string
	categories   map[string]int
	targetType   string
	timeout      float64
}

func newNexusPHPClassic() TargetSite {
	return &NexusPHPClassic{
		name:       "nexusphp-classic",
		targetType: "nexusphp_classic",
		uploadPath: "takeupload.php",
		timeout:    30,
		categories: map[string]int{},
	}
}

// ApplyConfig 从 cfg 覆盖配置。
func (n *NexusPHPClassic) ApplyConfig(cfg map[string]any) {
	if v := getStr(cfg, "name"); v != "" {
		n.name = v
	}
	if v := getStr(cfg, "base_url"); v != "" {
		n.baseURL = v
	}
	if v := getStr(cfg, "cookie"); v != "" {
		n.cookie = v
	}
	if v := getStr(cfg, "site_name"); v != "" {
		n.siteName = v
	}
	if v := getStr(cfg, "announce_base"); v != "" {
		n.announceBase = v
	}
	if v := getStr(cfg, "passkey"); v != "" {
		n.passkey = v
	}
	if v := getStr(cfg, "upload_path"); v != "" {
		n.uploadPath = v
	}
	if cats, ok := cfg["categories"]; ok && cats != nil {
		if m, ok := cats.(map[string]any); ok {
			parsed := map[string]int{}
			for k, v := range m {
				if id, err := strconv.Atoi(fmt.Sprintf("%v", v)); err == nil {
					parsed[k] = id
				}
			}
			n.categories = parsed
		}
	}
}

func (n *NexusPHPClassic) SiteType() string           { return n.targetType }
func (n *NexusPHPClassic) SiteName() string           { return n.name }
func (n *NexusPHPClassic) Categories() map[string]int { return n.categories }
func (n *NexusPHPClassic) SetCategories(cats map[string]int) {
	if cats == nil {
		cats = map[string]int{}
	}
	n.categories = cats
}

func (n *NexusPHPClassic) clientHeaders() map[string]string {
	return clientHeaders(nil)
}

func (n *NexusPHPClassic) makeClient() *http.Client {
	return newHTTPClient(n.timeout, true, n.clientHeaders())
}

// BuildAnnounce 目标站 announce URL。
func (n *NexusPHPClassic) BuildAnnounce() string {
	if strings.Contains(n.announceBase, "{passkey}") {
		return strings.ReplaceAll(n.announceBase, "{passkey}", n.passkey)
	}
	return n.announceBase
}

// UploadURL 上传入口。
func (n *NexusPHPClassic) UploadURL() string {
	return strings.TrimRight(n.baseURL, "/") + "/" + strings.TrimLeft(n.uploadPath, "/")
}

// LoadCategories 实现 CategoriesLoader(返回配置的分类枚举)。
func (n *NexusPHPClassic) LoadCategories() map[string]int {
	return n.categories
}

// Teams 传统 NexusPHP 通常无 team 枚举,返回空。
func (n *NexusPHPClassic) Teams() map[string]int {
	return map[string]int{}
}

// ParseFieldsFromTorrent 由 ParsedTorrent 生成基础字段。
func (n *NexusPHPClassic) ParseFieldsFromTorrent(parsed *parser.ParsedTorrent) map[string]any {
	return BuildUploadFields(parsed, n, nil)
}

var detailsIDRe = regexp.MustCompile(`details\.php\?id=(\d+)`)

// classicConvert 布尔转 'yes',列表 join,其余转字符串。
func classicConvert(v any) string {
	switch b := v.(type) {
	case bool:
		if b {
			return "yes"
		}
		return ""
	case []any:
		var parts []string
		for _, x := range b {
			parts = append(parts, fmt.Sprintf("%v", x))
		}
		return strings.Join(parts, ",")
	case []string:
		return strings.Join(b, ",")
	default:
		return fmt.Sprintf("%v", v)
	}
}

// UploadTorrent 通过 takeupload.php 表单上传。
func (n *NexusPHPClassic) UploadTorrent(torrentPath string, fields map[string]any) (UploadResult, error) {
	missing := []string{}
	for _, k := range []string{"name", "descr", "type"} {
		if _, ok := fields[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return UploadResult{}, newUploadError("TJUPT 必填字段缺失: "+strings.Join(missing, ", "), 0, "", false)
	}

	url := n.UploadURL()
	headers := n.clientHeaders()
	if n.cookie != "" {
		headers["Cookie"] = n.cookie
	} else {
		return UploadResult{}, newUploadError("TJUPT 表单上传需要登录 cookie(cookie 字段)", 0, "", false)
	}

	client := n.makeClient()
	resp, err := postMultipart(client, url, headers, fields, torrentPath, classicConvert)
	if err != nil {
		return UploadResult{}, newUploadError("TJUPT 请求失败: "+err.Error(), 0, err.Error(), false)
	}
	body, err := readRespBody(resp)
	if err != nil {
		return UploadResult{}, newUploadError("TJUPT 读取响应失败: "+err.Error(), resp.StatusCode, "", false)
	}

	// 成功判定:传统 NexusPHP 上传成功会 302 到 details.php?id=N
	loc := resp.Header.Get("Location")
	m := detailsIDRe.FindStringSubmatch(loc)
	if m == nil {
		m = detailsIDRe.FindStringSubmatch(body)
	}
	if m != nil {
		id, _ := strconv.Atoi(m[1])
		return UploadResult{OK: true, TargetID: &id, Detail: fmt.Sprintf("TJUPT 上传成功 id=%s", m[1])}, nil
	}
	if strings.Contains(body, "种子已存在") || strings.Contains(strings.ToLower(body), "already exists") || strings.Contains(body, "重复") {
		return UploadResult{}, newUploadError("TJUPT 上传:种子已存在", 0, truncateStr(body, 200), true)
	}
	return UploadResult{}, newUploadError(
		fmt.Sprintf("TJUPT 上传失败: HTTP %d loc=%s", resp.StatusCode, truncateStr(loc, 60)),
		resp.StatusCode, truncateStr(body, 200), false,
	)
}
