package targets

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/autoseedrelay/go-relay/internal/parser"
)

// mteamExistingHints 服务端去重响应的特征关键词(含中文——M-Team 返回繁体"種子已存在")。
var mteamExistingHints = []string{
	"duplicate",
	"exists",
	"existed",
	"repeat upload",
	"already uploaded",
	"種子已存在",
	"种子已存在",
	"重复发布",
	"重複發布",
	"已存在",
	"已上传过",
	"已上傳過",
}

// MTeamAPI M-Team 目标站(Spring Boot)。
type MTeamAPI struct {
	name         string
	apiBase      string
	authToken    string // M-Team API key(x-api-key 头)
	announceBase string
	siteName     string
	passkey      string
	targetType   string
	timeout      float64
	categories   map[string]int
	teams        map[string]int
}

func newMTeamAPI() TargetSite {
	return &MTeamAPI{
		name:       "mteam",
		targetType: "mteam",
		timeout:    30,
		categories: map[string]int{},
		teams:      map[string]int{},
	}
}

// ApplyConfig 从 cfg 覆盖配置。兼容 base_url → api_base 命名。
func (m *MTeamAPI) ApplyConfig(cfg map[string]any) {
	if v := getStr(cfg, "name"); v != "" {
		m.name = v
	}
	if v := getStr(cfg, "base_url", "api_base"); v != "" {
		m.apiBase = v
	}
	if v := getStr(cfg, "auth_token", "api_token"); v != "" {
		m.authToken = v
	}
	if v := getStr(cfg, "announce_base"); v != "" {
		m.announceBase = v
	}
	if v := getStr(cfg, "site_name"); v != "" {
		m.siteName = v
	}
	if v := getStr(cfg, "passkey"); v != "" {
		m.passkey = v
	}
}

func (m *MTeamAPI) SiteType() string           { return m.targetType }
func (m *MTeamAPI) SiteName() string           { return m.name }
func (m *MTeamAPI) Categories() map[string]int { return m.categories }
func (m *MTeamAPI) SetCategories(cats map[string]int) {
	if cats == nil {
		cats = map[string]int{}
	}
	m.categories = cats
}
func (m *MTeamAPI) Teams() map[string]int { return m.teams }

func (m *MTeamAPI) clientHeaders() map[string]string {
	return clientHeaders(nil)
}

func (m *MTeamAPI) makeClient() *http.Client {
	return newHTTPClient(m.timeout, true, m.clientHeaders())
}

// BuildAnnounce 返回目标站 tracker announce。M-Team 无 passkey,上传时服务端
// 会自行改写 announce,此处只做模板占位。
func (m *MTeamAPI) BuildAnnounce() string {
	base := m.announceBase
	if base == "" {
		base = "https://tracker.m-team.cc/announce?credential={credential}"
	}
	if strings.Contains(base, "{credential}") {
		return strings.ReplaceAll(base, "{credential}", "PLACEHOLDER")
	}
	return base
}

// UploadURL 上传端点。
func (m *MTeamAPI) UploadURL() string {
	return strings.TrimRight(m.apiBase, "/") + "/torrent/createOredit"
}

func (m *MTeamAPI) categoryListURL() string {
	return strings.TrimRight(m.apiBase, "/") + "/torrent/categoryList"
}

func (m *MTeamAPI) teamListURL() string {
	return strings.TrimRight(m.apiBase, "/") + "/torrent/teamList"
}

// requestJSON 发起带鉴权头的请求并解析 JSON(M-Team 只读端点用)。
func (m *MTeamAPI) requestJSON(url, method string) (map[string]any, error) {
	headers := m.clientHeaders()
	if m.authToken != "" {
		headers["x-api-key"] = m.authToken
	}
	var req *http.Request
	var err error
	if strings.ToUpper(method) == "POST" {
		req, err = http.NewRequest("POST", url, nil)
	} else {
		req, err = http.NewRequest("GET", url, nil)
	}
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := newHTTPClient(m.timeout, true, headers)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("M-Team 请求失败: HTTP %d %s", resp.StatusCode, truncateStr(string(body), 200))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	return data, nil
}

// GetTeams 拉取制作组枚举并解析为 {制作组名: id},实例内存缓存。失败静默返回空。
func (m *MTeamAPI) GetTeams() map[string]int {
	if len(m.teams) > 0 {
		return m.teams
	}
	if m.apiBase == "" {
		return map[string]int{}
	}
	teams := map[string]int{}
	data, err := m.requestJSON(m.teamListURL(), "POST")
	if err == nil {
		rows := data["data"]
		if list, ok := rows.([]any); ok {
			for _, t := range list {
				tm, ok := t.(map[string]any)
				if !ok {
					continue
				}
				tid, err := strconv.Atoi(fmt.Sprintf("%v", tm["id"]))
				if err != nil {
					continue
				}
				if nm, ok := tm["name"].(string); ok && nm != "" {
					teams[nm] = tid
				}
			}
		}
	}
	m.teams = teams
	return teams
}

// GetCategories 拉取分类枚举并解析为 {分类名: id},实例内存缓存。
// 未配置 api_base(dry-run)则返回内置默认映射。
func (m *MTeamAPI) GetCategories() map[string]int {
	if len(m.categories) > 0 {
		return m.categories
	}
	if m.apiBase == "" {
		// 无凭据/dry-run:回退官方默认分类 id
		out := make(map[string]int, len(MTeamCategoryID))
		for k, v := range MTeamCategoryID {
			out[k] = v
		}
		m.categories = out
		return out
	}
	data, err := m.requestJSON(m.categoryListURL(), "POST")
	if err == nil {
		rows := data["data"]
		m.categories = parseMTeamCategories(rows)
	}
	return m.categories
}

// LoadCategories 实现 CategoriesLoader。
func (m *MTeamAPI) LoadCategories() map[string]int {
	return m.GetCategories()
}

// ParseFieldsFromTorrent 由 ParsedTorrent 生成基础字段。
func (m *MTeamAPI) ParseFieldsFromTorrent(parsed *parser.ParsedTorrent) map[string]any {
	return BuildUploadFields(parsed, m, nil)
}

// parseMTeamCategories 解析 M-Team categoryList 真实返回结构。
// 把 简体名/繁体名/英文名 都映射到同一 id(便于按任意语言匹配)。
func parseMTeamCategories(rows any) map[string]int {
	out := map[string]int{}
	var lst []any
	switch v := rows.(type) {
	case map[string]any:
		if l, ok := v["list"].([]any); ok {
			lst = l
		} else if l, ok := v["categories"].([]any); ok {
			lst = l
		}
	case []any:
		lst = v
	default:
		lst = nil
	}
	for _, c := range lst {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		cid, err := strconv.Atoi(fmt.Sprintf("%v", cm["id"]))
		if err != nil {
			continue
		}
		for _, key := range []string{"nameChs", "nameCht", "nameEng", "name", "label"} {
			if nm, ok := cm[key].(string); ok && nm != "" {
				out[nm] = cid
			}
		}
	}
	return out
}

// mteamConvert 布尔转小写字符串,列表 join,其余转字符串。
func mteamConvert(v any) string {
	switch b := v.(type) {
	case bool:
		if b {
			return "true"
		}
		return "false"
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

// UploadTorrent 上传清洗后的种子到 M-Team。
func (m *MTeamAPI) UploadTorrent(torrentPath string, fields map[string]any) (UploadResult, error) {
	missing := []string{}
	for _, k := range []string{"name", "descr", "anonymous"} {
		if _, ok := fields[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return UploadResult{}, newUploadError("M-Team 必填字段缺失: "+strings.Join(missing, ", "), 0, "", false)
	}

	url := m.UploadURL()
	headers := m.clientHeaders()
	if m.authToken != "" {
		headers["x-api-key"] = m.authToken
	} else {
		// dry-run / 无凭据:占位避免漏头
		headers["x-api-key"] = ""
	}

	client := m.makeClient()
	resp, err := postMultipart(client, url, headers, fields, torrentPath, mteamConvert)
	if err != nil {
		return UploadResult{}, newUploadError("M-Team 请求失败: "+err.Error(), 0, err.Error(), false)
	}
	body, err := readRespBody(resp)
	if err != nil {
		return UploadResult{}, newUploadError("M-Team 读取响应失败: "+err.Error(), resp.StatusCode, "", false)
	}

	existing := false
	lowBody := strings.ToLower(body)
	for _, hint := range mteamExistingHints {
		if strings.Contains(lowBody, hint) {
			existing = true
			break
		}
	}
	if resp.StatusCode >= 400 || existing {
		return UploadResult{}, newUploadError(
			fmt.Sprintf("M-Team 上传失败: HTTP %d", resp.StatusCode),
			resp.StatusCode, body, existing,
		)
	}

	var dataResp map[string]any
	if err := json.Unmarshal([]byte(body), &dataResp); err != nil {
		dataResp = map[string]any{"raw": body}
	}

	// Result{code,message,data};code=0 表示成功
	code := -1
	if c, ok := dataResp["code"]; ok && c != nil {
		if n, err := strconv.Atoi(fmt.Sprintf("%v", c)); err == nil {
			code = n
		}
	}
	targetID := extractTargetID(dataResp)
	ok := code == 0 || targetID != nil
	detail := fmt.Sprintf("M-Team 上传完成 code=%d target_id=%v resp=%s", code, targetID, truncateStr(body, 200))
	return UploadResult{OK: ok, TargetID: targetID, Detail: detail}, nil
}
