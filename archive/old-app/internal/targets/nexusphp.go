package targets

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/autoseedrelay/go-relay/internal/parser"
)

// nexusphpExistingHints 服务端去重响应的特征关键词(按 response body 判定)。
var nexusphpExistingHints = []string{
	"torrent_existed",
	"torrent already exists",
	"already exists",
	"duplicate torrent",
	"existed",
}

// NexusPHPAPI NexusPHP 系目标站(Laravel API + Sanctum)。
type NexusPHPAPI struct {
	name         string
	baseURL      string
	apiToken     string // Sanctum token
	announceBase string
	siteName     string
	passkey      string
	targetType   string
	timeout      float64
	categories   map[string]int
	sections     map[string]any // 实例内存缓存
}

func newNexusPHPAPI() TargetSite {
	return &NexusPHPAPI{
		name:       "nexusphp",
		targetType: "nexusphp",
		timeout:    30,
		categories: map[string]int{},
	}
}

// ApplyConfig 从 cfg 覆盖配置。兼容两套命名(api_token/auth_token)。
func (n *NexusPHPAPI) ApplyConfig(cfg map[string]any) {
	if v := getStr(cfg, "name"); v != "" {
		n.name = v
	}
	if v := getStr(cfg, "base_url"); v != "" {
		n.baseURL = v
	}
	if v := getStr(cfg, "api_token", "auth_token"); v != "" {
		n.apiToken = v
	}
	if v := getStr(cfg, "announce_base"); v != "" {
		n.announceBase = v
	}
	if v := getStr(cfg, "site_name"); v != "" {
		n.siteName = v
	}
	if v := getStr(cfg, "passkey"); v != "" {
		n.passkey = v
	}
}

func (n *NexusPHPAPI) SiteType() string          { return n.targetType }
func (n *NexusPHPAPI) SiteName() string          { return n.name }
func (n *NexusPHPAPI) Categories() map[string]int { return n.categories }
func (n *NexusPHPAPI) SetCategories(cats map[string]int) {
	if cats == nil {
		cats = map[string]int{}
	}
	n.categories = cats
}

func (n *NexusPHPAPI) clientHeaders() map[string]string {
	return clientHeaders(nil)
}

func (n *NexusPHPAPI) makeClient() *http.Client {
	return newHTTPClient(n.timeout, true, n.clientHeaders())
}

// BuildAnnounce 目标站 announce URL。
func (n *NexusPHPAPI) BuildAnnounce() string {
	base := n.announceBase
	if base == "" {
		base = strings.TrimRight(n.baseURL, "/") + "/announce.php"
	}
	if strings.Contains(base, "{passkey}") {
		return strings.ReplaceAll(base, "{passkey}", n.passkey)
	}
	if n.passkey != "" && !strings.Contains(base, "passkey=") {
		sep := "&"
		if !strings.Contains(base, "?") {
			sep = "?"
		}
		return base + sep + "passkey=" + n.passkey
	}
	return base
}

// UploadURL 上传端点。
func (n *NexusPHPAPI) UploadURL() string {
	return strings.TrimRight(n.baseURL, "/") + "/api/v1/upload"
}

// GetSections 拉取上传表单 schema(分类/子分类/tags ID),实例内存缓存。
func (n *NexusPHPAPI) GetSections() map[string]any {
	if n.sections != nil {
		return n.sections
	}
	if n.baseURL == "" {
		n.sections = map[string]any{}
		return n.sections
	}
	url := strings.TrimRight(n.baseURL, "/") + "/api/v1/sections"
	headers := n.clientHeaders()
	if n.apiToken != "" {
		headers["Authorization"] = "Bearer " + n.apiToken
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return map[string]any{}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := newHTTPClient(n.timeout, true, headers)
	resp, err := client.Do(req)
	if err != nil {
		return map[string]any{}
	}
	if resp.StatusCode != 200 {
		_ = resp.Body.Close()
		return map[string]any{}
	}
	body, err := readRespBody(resp)
	if err != nil {
		return map[string]any{}
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		data = map[string]any{}
	}
	n.sections = data
	n.cacheCategories(data)
	return data
}

func (n *NexusPHPAPI) cacheCategories(data map[string]any) {
	if cats, ok := data["categories"]; ok && cats != nil {
		n.categories = ParseCategoriesMapping(cats)
	} else {
		// 兼容嵌套结构:任意位置找含 id+name 的对象
		n.categories = ParseCategoriesMapping(data)
	}
}

// LoadCategories 实现 CategoriesLoader。
func (n *NexusPHPAPI) LoadCategories() map[string]int {
	n.GetSections()
	return n.categories
}

// ParseFieldsFromTorrent 由 ParsedTorrent 生成基础字段。
func (n *NexusPHPAPI) ParseFieldsFromTorrent(parsed *parser.ParsedTorrent) map[string]any {
	return BuildUploadFields(parsed, n, nil)
}

// pythonStr 对应 Python 的 str(v)(bool → "True"/"False")。
func pythonStr(v any) string {
	if b, ok := v.(bool); ok {
		if b {
			return "True"
		}
		return "False"
	}
	return fmt.Sprintf("%v", v)
}

// UploadTorrent 上传清洗后的种子到目标站。
func (n *NexusPHPAPI) UploadTorrent(torrentPath string, fields map[string]any) (UploadResult, error) {
	// 必填校验
	var missing []string
	for _, k := range []string{"name", "descr", "type"} {
		if _, ok := fields[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return UploadResult{}, newUploadError("NexusPHP 必填字段缺失: "+strings.Join(missing, ", "), 0, "", false)
	}

	url := n.UploadURL()
	headers := n.clientHeaders()
	if n.apiToken != "" {
		headers["Authorization"] = "Bearer " + n.apiToken
	}

	// multipart 表单(先读入字节,避免 Windows 上占用句柄)
	form := map[string]any{}
	for k, v := range fields {
		if k == "tags" {
			continue
		}
		form[k] = v
	}
	if tags, ok := fields["tags"]; ok && tags != nil {
		form["tags[]"] = tags
	}

	client := n.makeClient()
	resp, err := postMultipart(client, url, headers, form, torrentPath, pythonStr)
	if err != nil {
		return UploadResult{}, newUploadError("NexusPHP 请求失败: "+err.Error(), 0, err.Error(), false)
	}
	body, err := readRespBody(resp)
	if err != nil {
		return UploadResult{}, newUploadError("NexusPHP 读取响应失败: "+err.Error(), resp.StatusCode, "", false)
	}

	// 去重识别:HTTP 409 或返回体含 existing 关键词
	existing := resp.StatusCode == 409
	lowBody := strings.ToLower(body)
	for _, hint := range nexusphpExistingHints {
		if strings.Contains(lowBody, hint) {
			existing = true
			break
		}
	}
	if resp.StatusCode >= 400 || existing {
		return UploadResult{}, newUploadError(
			fmt.Sprintf("NexusPHP 上传失败: HTTP %d", resp.StatusCode),
			resp.StatusCode, body, existing,
		)
	}

	var dataResp map[string]any
	if err := json.Unmarshal([]byte(body), &dataResp); err != nil {
		dataResp = map[string]any{"ret": body}
	}
	targetID := extractTargetID(dataResp)
	ok := targetID != nil
	detail := fmt.Sprintf("NexusPHP 上传完成 target_id=%v resp=%s", targetID, truncateStr(body, 200))
	return UploadResult{OK: ok, TargetID: targetID, Detail: detail}, nil
}

// extractTargetID 从响应 data.id 提取 target_id。
func extractTargetID(dataResp map[string]any) *int {
	d, _ := dataResp["data"]
	switch v := d.(type) {
	case map[string]any:
		if id, ok := v["id"]; ok && id != nil {
			return toIntPtr(id)
		}
	case int:
		return toIntPtr(v)
	case int64:
		return toIntPtr(int(v))
	case float64:
		return toIntPtr(int(v))
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return &n
		}
	}
	return nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
