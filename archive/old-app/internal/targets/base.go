// Package targets 目标站上传适配器。
//
//   - base.go:统一清洗/字段映射工具
//   - nexusphp.go:NexusPHP 系(Laravel + Sanctum)API 上传
//   - nexusphp_classic.go:传统 NexusPHP(takeupload.php)表单上传
//   - mteam.go:M-Team(Spring)上传
//   - targets.go:顶层统一上传入口 + TargetSite 接口
//
// 设计约束:上传逻辑只做 --dry-run 自测;真实上传必须由调用方显式传入
// 有效凭据并负责遵守目标站规则。任何情况下本包都不落盘凭据。
package targets

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/autoseedrelay/go-relay/internal/descr"
	"github.com/autoseedrelay/go-relay/internal/mteam"
	"github.com/autoseedrelay/go-relay/internal/parser"
)

// UploadResult 统一上传结果结构(对应 Python upload_result dict)。
type UploadResult struct {
	OK       bool
	TargetID *int
	Detail   string
}

// UploadError 目标站上传失败。
// Existing 为是否为"种子已存在"类失败(命中服务端去重)。
type UploadError struct {
	Message     string
	StatusCode  int // 无则 0
	BodyPreview string
	Existing    bool
}

func (e *UploadError) Error() string { return e.Message }

func newUploadError(message string, statusCode int, bodyPreview string, existing bool) *UploadError {
	if len(bodyPreview) > 500 {
		bodyPreview = bodyPreview[:500]
	}
	return &UploadError{Message: message, StatusCode: statusCode, BodyPreview: bodyPreview, Existing: existing}
}

// IMDBRe IMDb:tt1234567 或 URL 形式 http.../title/tt1234567/ 里取 tt 号。
var IMDBRe = regexp.MustCompile(`tt\d{6,}`)

// ExtractIMDB 从候选文本里提取 IMDb tt 号;没有则空串。
func ExtractIMDB(candidates ...any) string {
	for _, c := range candidates {
		if c == nil {
			continue
		}
		s := fmt.Sprintf("%v", c)
		if s == "" {
			continue
		}
		if m := IMDBRe.FindString(s); m != "" {
			return m
		}
	}
	return ""
}

const defaultUserAgent = "AutoSeedRelay/0.1 (+relay script)"

// newHTTPClient 构建 http.Client。
func newHTTPClient(timeout float64, followRedirects bool, headers map[string]string) *http.Client {
	if timeout <= 0 {
		timeout = 30
	}
	client := &http.Client{Timeout: time.Duration(timeout * float64(time.Second))}
	if !followRedirects {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	return client
}

// clientHeaders 基础请求头。
func clientHeaders(extra map[string]string) map[string]string {
	h := map[string]string{"User-Agent": defaultUserAgent}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

// postMultipart multipart 表单 POST。fields 里 slice 值会展开为多个同名字段;
// filePath 作为 file 字段上传。convert 负责把字段值转成表单字符串。
func postMultipart(client *http.Client, url string, headers map[string]string, fields map[string]any, filePath string, convert func(v any) string) (*http.Response, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	// 排序字段名保证输出稳定(不影响服务端语义)
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := fields[k]
		if v == nil {
			continue
		}
		switch val := v.(type) {
		case []any:
			for _, item := range val {
				if item == nil {
					continue
				}
				_ = w.WriteField(k, convert(item))
			}
		case []string:
			for _, item := range val {
				_ = w.WriteField(k, convert(item))
			}
		default:
			_ = w.WriteField(k, convert(v))
		}
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, filepath.Base(filePath)))
	hdr.Set("Content-Type", "application/x-bittorrent")
	fw, err := w.CreatePart(hdr)
	if err != nil {
		return nil, err
	}
	if _, err := fw.Write(data); err != nil {
		return nil, err
	}
	_ = w.Close()

	req, err := http.NewRequest("POST", url, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return client.Do(req)
}

// readRespBody 读取响应体并关闭。
func readRespBody(resp *http.Response) (string, error) {
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func getStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
			if n, ok := v.(int); ok {
				return strconv.Itoa(n)
			}
		}
	}
	return ""
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func toIntVal(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i
		}
	}
	return 0
}

func toIntPtr(v any) *int {
	n := toIntVal(v)
	return &n
}

func toBool(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return strings.EqualFold(b, "true") || b == "1" || strings.EqualFold(b, "yes")
	}
	return false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parsedCategoryName / parsedSmallDescr / parsedIMDB:Python 的 ParsedTorrent
// 没有这些字段(base.py 用 getattr(parsed, name, "") 取默认空),Go 侧同样
// 返回空串。若后续 parser 包补充了对应字段/方法,可在此扩展。
func parsedCategoryName(parsed *parser.ParsedTorrent) string {
	if p, ok := any(parsed).(interface{ CategoryName() string }); ok {
		return p.CategoryName()
	}
	return ""
}

func parsedSmallDescr(parsed *parser.ParsedTorrent) string {
	if p, ok := any(parsed).(interface{ SmallDescr() string }); ok {
		return p.SmallDescr()
	}
	return ""
}

func parsedIMDB(parsed *parser.ParsedTorrent) string {
	if p, ok := any(parsed).(interface{ IMDB() string }); ok {
		return p.IMDB()
	}
	return ""
}

// ---------------------------------------------------------------------------
// 分类映射相关数据
// ---------------------------------------------------------------------------

// MTeamCategoryID M-Team 官方 category id —— 实测自生产 API。
var MTeamCategoryID = map[string]int{
	"movie": 100, "tv": 105, "doc": 404, "anime": 405, "music": 110, "other": 409,
	"movie_hd": 419, "movie_bluray": 421, "movie_remux": 439, "movie_sd": 401, "movie_dvdiso": 420,
	"tv_series": 448, "tv_hd": 402, "tv_bluray": 438, "tv_sd": 403, "tv_dvdiso": 435,
}

// MTeamCategoryAlias 中文/英文别名。
var MTeamCategoryAlias = map[string]string{
	"电影": "movie", "影片": "movie", "电影类": "movie",
	"剧集": "tv", "电视剧": "tv", "剧集类": "tv", "连续剧": "tv", "综艺": "tv",
	"纪录片": "doc", "记录片": "doc",
	"动漫": "anime", "动画": "anime", "动漫类": "anime",
	"音乐": "music", "无损音乐": "music",
	"其它": "other", "其他": "other", "软件": "other", "学习": "other", "教程": "other",
	"movies": "movie", "movi": "movie", "video": "movie",
	"tv shows": "tv", "tvs": "tv", "tvseries": "tv", "episode": "tv",
	"documentary": "doc", "anime": "anime", "animation": "anime",
	"music": "music", "audio": "music", "other": "other", "misc": "other",
}

// CATEGORY_FIELD_BY_TARGET 目标站分类键 → 目标站表单字段名。
var CATEGORY_FIELD_BY_TARGET = map[string]string{"nexusphp": "type", "mteam": "category"}

// normalizeCatKey 分类键统一小写、去空白,便于匹配。
func normalizeCatKey(k string) string {
	return wsAllRe.ReplaceAllString(strings.ToLower(k), "")
}

var wsAllRe = regexp.MustCompile(`\s+`)

// ParseCategoriesMapping 从站点返回的分类枚举提取 {分类名: id} 映射。
// 递归遍历带 children 的结构。
func ParseCategoriesMapping(rows any) map[string]int {
	out := map[string]int{}
	walkCategories(rows, out)
	return out
}

func walkCategories(items any, out map[string]int) {
	switch v := items.(type) {
	case map[string]any:
		if idRaw, ok := v["id"]; ok {
			nm := ""
			if s, ok := v["name"].(string); ok {
				nm = s
			} else if s, ok := v["label"].(string); ok {
				nm = s
			}
			if nm != "" {
				if id, err := strconv.Atoi(fmt.Sprintf("%v", idRaw)); err == nil {
					out[nm] = id
				}
			}
		}
		for _, val := range v {
			walkCategories(val, out)
		}
	case []any:
		for _, it := range v {
			walkCategories(it, out)
		}
	}
}

// ResolveCategoryID 解析分类 id:候选键 → 站点 API 分类 → 默认映射 → fallback。
func ResolveCategoryID(catKey string, siteCategories, defaultCategories map[string]int, fallback *int) *int {
	if catKey != "" {
		trimmed := strings.TrimLeft(catKey, "-")
		if isDigits(trimmed) {
			return toIntPtr(trimmed)
		}
	}
	if catKey == "" {
		return fallback
	}
	k := normalizeCatKey(catKey)
	// 1) 站点 API 分类名(兼容大小写/空白)
	for cidStr, cid := range siteCategories {
		if normalizeCatKey(cidStr) == k || strconv.Itoa(cid) == k {
			return toIntPtr(cid)
		}
	}
	// 2) 默认映射(先别名后键名)
	alias := MTeamCategoryAlias[k]
	if alias == "" {
		alias = MTeamCategoryAlias[strings.ToLower(catKey)]
	}
	if alias != "" {
		if cid, ok := defaultCategories[alias]; ok {
			return toIntPtr(cid)
		}
	}
	if cid, ok := defaultCategories[k]; ok {
		return toIntPtr(cid)
	}
	return fallback
}

// resolveTeamID 把 team 解析为 ID。
func resolveTeamID(val any, teams map[string]int) string {
	s := strings.TrimSpace(fmt.Sprintf("%v", val))
	if s == "" {
		return s
	}
	if isDigits(s) {
		return s
	}
	if teams != nil {
		for name, tid := range teams {
			if strings.EqualFold(name, s) {
				return strconv.Itoa(tid)
			}
		}
	}
	return s
}

// targetDefaultCategories 返回该站点的默认分类 id 映射(未拉取 API 时的兜底)。
func targetDefaultCategories(site TargetSite) map[string]int {
	if site.SiteType() == "mteam" {
		return MTeamCategoryID
	}
	return map[string]int{}
}

// ---------------------------------------------------------------------------
// M-Team 维度枚举(实测自生产 API)
// ---------------------------------------------------------------------------

var MTEAM_STANDARD = map[string]int{
	"1080p": 1, "1080i": 2, "720p": 3, "SD": 5, "4K": 6, "2160p": 6, "8K": 7,
}
var MTEAM_VIDEO_CODEC = map[string]int{
	"H.264": 1, "H264": 1, "X264": 1, "AVC": 1,
	"H.265": 16, "H265": 16, "HEVC": 16, "X265": 16,
	"VC-1": 2, "MPEG-2": 4, "XVID": 3, "AV1": 19, "VP8": 21, "VP9": 21, "AVS": 22,
}
var MTEAM_AUDIO_CODEC = map[string]int{
	"AAC": 6, "AC3": 8, "DD": 8, "DTS": 3, "DTS-HD MA": 11, "DTSHDMA": 11,
	"DDP": 12, "E-AC3": 12, "EAC3": 12, "DDP ATMOS": 13, "E-AC3 ATOMS": 13,
	"TRUEHD": 9, "TRUEHD ATMOS": 10, "LPCM": 14, "PCM": 14, "FLAC": 1,
	"APE": 2, "MP2": 4, "MP3": 4, "OGG": 5, "WAV": 15, "OTHER": 7,
}

// NEXUSPHP_STANDARD 星陨阁(nexusphp-api)taxonomy ID。
var NEXUSPHP_STANDARD = map[string]int{
	"1080p": 3, "1080i": 3, "720p": 2, "2160p": 4, "4K": 4, "SD": 6, "480p": 6,
}
var NEXUSPHP_VIDEO_CODEC = map[string]int{
	"H.264": 1, "H264": 1, "X264": 1, "AVC": 1,
	"H.265": 2, "H265": 2, "HEVC": 2, "X265": 2,
	"AV1": 16, "VC-1": 6, "XVID": 6, "MPEG-2": 6,
}
var NEXUSPHP_AUDIO_CODEC = map[string]int{
	"AAC": 14, "AC3": 10, "DD": 10, "DD5.1": 10,
	"E-AC3": 11, "EAC3": 11, "DDP": 11, "DDP5.1": 11, "DD+": 11,
	"TRUEHD": 8, "DTS": 6, "DTS-HD MA": 6, "FLAC": 6, "LPCM": 6,
}
var NEXUSPHP_SOURCE = map[string]int{
	"BLURAY": 1, "BD": 1, "BLU-RAY": 1, "BDRIP": 1,
	"WEB": 4, "WEB-DL": 4, "WEBRIP": 4,
	"HDTV": 5, "DVDRIP": 6, "DVD": 6,
}
var NEXUSPHP_MEDIUM = map[string]int{
	"BLURAY": 1, "BD": 1, "BDRIP": 1,
	"WEB": 4, "WEB-DL": 4, "WEBRIP": 4,
	"REMUX": 3, "ENCODE": 7, "HDTV": 7, "DVDRIP": 10, "DVD": 10,
}

// sortByLenDesc 按 token 长度降序稳定排序(优先具体 token)。
func sortByLenDesc(keys []string) []string {
	sorted := append([]string(nil), keys...)
	sort.SliceStable(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	return sorted
}

var mteamStandardOrder = sortByLenDesc([]string{"1080p", "1080i", "720p", "SD", "4K", "2160p", "8K"})
var mteamVideoOrder = []string{"H.264", "H264", "X264", "AVC", "H.265", "H265", "HEVC", "X265", "VC-1", "MPEG-2", "XVID", "AV1", "VP8", "VP9", "AVS"}
var mteamAudioOrder = sortByLenDesc([]string{
	"AAC", "AC3", "DD", "DTS", "DTS-HD MA", "DTSHDMA", "DDP", "E-AC3", "EAC3",
	"DDP ATMOS", "E-AC3 ATOMS", "TRUEHD", "TRUEHD ATMOS", "LPCM", "PCM", "FLAC",
	"APE", "MP2", "MP3", "OGG", "WAV", "OTHER",
})

var nexusphpStandardOrder = sortByLenDesc([]string{"1080p", "1080i", "720p", "2160p", "4K", "SD", "480p"})
var nexusphpVideoOrder = []string{"H.264", "H264", "X264", "AVC", "H.265", "H265", "HEVC", "X265", "AV1", "VC-1", "XVID", "MPEG-2"}
var nexusphpAudioOrder = sortByLenDesc([]string{
	"AAC", "AC3", "DD", "DD5.1", "E-AC3", "EAC3", "DDP", "DDP5.1", "DD+",
	"TRUEHD", "DTS", "DTS-HD MA", "FLAC", "LPCM",
})
var nexusphpSourceOrder = sortByLenDesc([]string{"BLURAY", "BD", "BLU-RAY", "BDRIP", "WEB", "WEB-DL", "WEBRIP", "HDTV", "DVDRIP", "DVD"})
var nexusphpMediumOrder = sortByLenDesc([]string{"BLURAY", "BD", "BDRIP", "WEB", "WEB-DL", "WEBRIP", "REMUX", "ENCODE", "HDTV", "DVDRIP", "DVD"})

func matchFirst(up string, order []string, m map[string]int) (int, bool) {
	for _, tok := range order {
		if strings.Contains(up, strings.ToUpper(tok)) {
			return m[tok], true
		}
	}
	return 0, false
}

func resolveMTeamDimensions(title string) map[string]any {
	out := map[string]any{}
	up := strings.ToUpper(title)
	if id, ok := matchFirst(up, mteamStandardOrder, MTEAM_STANDARD); ok {
		out["standard"] = id
	}
	if id, ok := matchFirst(up, mteamVideoOrder, MTEAM_VIDEO_CODEC); ok {
		out["video_codec"] = id
	}
	if id, ok := matchFirst(up, mteamAudioOrder, MTEAM_AUDIO_CODEC); ok {
		out["audio_codec"] = id
	}
	return out
}

func resolveNexusPHPDimensions(title string) map[string]any {
	out := map[string]any{}
	up := strings.ToUpper(title)
	if id, ok := matchFirst(up, nexusphpStandardOrder, NEXUSPHP_STANDARD); ok {
		out["standard"] = id
	}
	if id, ok := matchFirst(up, nexusphpVideoOrder, NEXUSPHP_VIDEO_CODEC); ok {
		out["codec"] = id
	}
	if id, ok := matchFirst(up, nexusphpAudioOrder, NEXUSPHP_AUDIO_CODEC); ok {
		out["audiocodec"] = id
	}
	if id, ok := matchFirst(up, nexusphpSourceOrder, NEXUSPHP_SOURCE); ok {
		out["source"] = id
	}
	if id, ok := matchFirst(up, nexusphpMediumOrder, NEXUSPHP_MEDIUM); ok {
		out["medium"] = id
	}
	return out
}

func resolveClassicDimensions(title string) map[string]any {
	up := strings.ToUpper(title)
	out := map[string]any{}
	for _, tok := range []string{"2160p", "4K", "1080p", "720p", "480p", "SD"} {
		if strings.Contains(up, tok) {
			if tok == "4K" {
				out["standard"] = "2160p"
			} else {
				out["standard"] = tok
			}
			break
		}
	}
	videoPairs := [][2]string{
		{"H.265", "H.265"}, {"H265", "H.265"}, {"HEVC", "H.265"}, {"X265", "H.265"},
		{"H.264", "H.264"}, {"H264", "H.264"}, {"X264", "H.264"}, {"AVC", "H.264"},
	}
	for _, p := range videoPairs {
		if strings.Contains(up, p[0]) {
			out["video_codec"] = p[1]
			break
		}
	}
	audioPairs := [][2]string{
		{"E-AC3", "E-AC3"}, {"EAC3", "E-AC3"}, {"DDP", "E-AC3"}, {"DDP5.1", "E-AC3"},
		{"DD+5.1", "AC3"}, {"AC3", "AC3"}, {"DD5.1", "AC3"},
		{"TRUEHD", "TrueHD"}, {"DTS-HD MA", "DTS-HD MA"}, {"DTS", "DTS"},
		{"AAC", "AAC"}, {"FLAC", "FLAC"}, {"LPCM", "LPCM"},
	}
	for _, p := range audioPairs {
		if strings.Contains(up, p[0]) {
			out["audio_codec"] = p[1]
			break
		}
	}
	return out
}

// ResolveDimensions 从标题解析维度(所有适配器;按站点类型映射到各自枚举/可读值)。
func ResolveDimensions(title, siteType string) map[string]any {
	switch siteType {
	case "mteam":
		return resolveMTeamDimensions(title)
	case "nexusphp":
		return resolveNexusPHPDimensions(title)
	default:
		return resolveClassicDimensions(title)
	}
}

// ---------------------------------------------------------------------------
// 标题规范化
// ---------------------------------------------------------------------------

// splitProtectRe 空格化时保护的复合 token(WEB-DL / H.265 / S01E01 / 音频 2.0 等)。
// Python 原式最后一项为 (?<!\d)\d\.\d(负向后行断言),RE2 不支持,这里用
// 捕获组 + 手工校验(见 spaceSplitTitle)。
var splitProtectRe = regexp.MustCompile(
	`(?i)(WEB-?DL|WEBRip|BluRay|REMUX|HEVC|AVC|X26[45]|H26[45]|H\.26[45]|` +
		`S\d+E\d+[-–]?S?\d*E?\d*|` +
		`(?:AAC|AC3|DDP|DD|TrueHD|DTS|FLAC|LPCM)\s*\.?\s*\d\.\d|` +
		`(\d\.\d))`,
)

var splitSepRe = regexp.MustCompile(`[\s._]+`)
var placeholderRe = regexp.MustCompile(`\x01\d+\x01`)

// spaceSplitTitle 点/下划线分隔 → 空格分隔;保护 WEB-DL/H.265/S01E01 等复合
// token,保留 `-制作组` 后缀(nexusphp 用;不转编码名)。
func spaceSplitTitle(name string) string {
	var protected []string
	locs := splitProtectRe.FindAllStringSubmatchIndex(name, -1)
	var sb strings.Builder
	last := 0
	for _, loc := range locs {
		s, e := loc[0], loc[1]
		var matched string
		isChannel := false
		if loc[4] >= 0 { // group 2:裸声道 token \d\.\d
			isChannel = true
			matched = name[loc[4]:loc[5]]
		} else { // group 1:其余(含 音频编码+声道 的整段)
			matched = name[loc[2]:loc[3]]
		}
		if isChannel {
			// 模拟 (?<!\d):前一字符不能是数字(避免 "2024.1080p" 的 "4.1" 被保护)
			if s > 0 && name[s-1] >= '0' && name[s-1] <= '9' {
				continue
			}
		}
		sb.WriteString(name[last:s])
		sb.WriteString("\x01" + strconv.Itoa(len(protected)) + "\x01")
		protected = append(protected, matched)
		last = e
	}
	sb.WriteString(name[last:])
	held := sb.String()

	var out []string
	for _, t := range splitSepRe.Split(held, -1) {
		if t == "" {
			continue
		}
		var pieces []string
		pos := 0
		for _, m := range placeholderRe.FindAllStringIndex(t, -1) {
			if m[0] > pos {
				pieces = append(pieces, t[pos:m[0]])
			}
			idx, _ := strconv.Atoi(t[m[0]+1 : m[1]-1])
			if idx >= 0 && idx < len(protected) {
				pieces = append(pieces, protected[idx])
			}
			pos = m[1]
		}
		if pos < len(t) {
			pieces = append(pieces, t[pos:])
		}
		out = append(out, strings.Join(pieces, ""))
	}
	return strings.Join(out, " ")
}

// NormalizeTitle 按站点类型规范化标题(所有适配器)。
func NormalizeTitle(name, siteType string) string {
	switch siteType {
	case "mteam":
		t := mteam.CleanMTteamTitle(name)
		if t.Name != "" {
			return t.Name
		}
		return name
	case "nexusphp":
		if out := spaceSplitTitle(name); out != "" {
			return out
		}
		return name
	default:
		return name
	}
}

// ---------------------------------------------------------------------------
// 副标题构造
// ---------------------------------------------------------------------------

var smallDescrSeasonRe = regexp.MustCompile(`(?i)S(\d{1,2})(?:E(\d{1,2}))?`)

func appendDescrLine(descr, line string) string {
	if descr == "" {
		return line
	}
	return descr + "\n" + line
}

// BuildSmallDescr 构造副标题(所有适配器):中文前缀 + 已有副标题 + 简介提取。
func BuildSmallDescr(parsed *parser.ParsedTorrent, extra map[string]any, siteType string) string {
	name := parsed.Name
	descrText := getStr(extra, "descr")
	var parts []string
	// 中文前缀(仅 mteam)
	if siteType == "mteam" {
		cnPref := mteam.CleanMTteamTitle(name).SmallDescrCN
		if cnPref != "" {
			parts = append(parts, cnPref)
		}
	}
	// 已有副标题(extra 用户覆盖 / parsed)
	sd := getStr(extra, "smallDescr", "small_descr")
	if sd == "" {
		sd = parsedSmallDescr(parsed)
	}
	if sd != "" && !containsStr(parts, sd) {
		parts = append(parts, sd)
	}
	// 从简介提取 片名/季集/类型
	if descrText != "" {
		sec := descr.ExtractSections(descrText)
		chTitle := getStr(sec, "chinese_title", "title")
		if chTitle != "" && !containsStr(parts, chTitle) {
			parts = append(parts, chTitle)
		}
		sm := smallDescrSeasonRe.FindStringSubmatch(name)
		if sm != nil {
			season, _ := strconv.Atoi(sm[1])
			sub := fmt.Sprintf("第%d季", season)
			if len(sm) > 2 && sm[2] != "" {
				ep, _ := strconv.Atoi(sm[2])
				sub += fmt.Sprintf(" 第%d集", ep)
			}
			if !containsStr(parts, sub) {
				parts = append(parts, sub)
			}
		}
		genre := getStr(sec, "genre")
		if genre != "" && !strings.Contains(genre, "类型") && !strings.Contains(genre, "类别") {
			genre = "类型：" + genre
		}
		if genre != "" && !containsStr(parts, genre) {
			parts = append(parts, genre)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " | ")
}

// ---------------------------------------------------------------------------
// 字段映射
// ---------------------------------------------------------------------------

func joinTags(tags any) string {
	switch v := tags.(type) {
	case []any:
		var parts []string
		for _, t := range v {
			parts = append(parts, fmt.Sprintf("%v", t))
		}
		return strings.Join(parts, ",")
	case []string:
		return strings.Join(v, ",")
	default:
		return fmt.Sprintf("%v", tags)
	}
}

// mapByType 按目标站类型把公共字段映射为具体表单字段名。
func mapByType(typ string, parsed *parser.ParsedTorrent, name, descr, smallDescr string, dims map[string]any, extra map[string]any, site TargetSite) map[string]any {
	if typ == "nexusphp" || typ == "nexusphp_classic" {
		fields := map[string]any{"name": name, "descr": descr}
		if smallDescr != "" {
			fields["small_descr"] = smallDescr
		}
		imdb := ExtractIMDB(extra["url"], parsedIMDB(parsed))
		if imdb != "" {
			fields["url"] = strings.TrimPrefix(imdb, "tt") // NexusPHP 存纯数字
		}

		if typ == "nexusphp_classic" {
			// 传统 NexusPHP 无独立 tags/维度字段 → 并入 descr
			descrOut := fields["descr"].(string)
			if tags, ok := extra["tags"]; ok && tags != nil {
				tagStr := joinTags(tags)
				descrOut = appendDescrLine(descrOut, "[标签:"+tagStr+"]")
			}
			var paramParts []string
			for _, k := range []string{"standard", "video_codec", "audio_codec"} {
				if v, ok := dims[k]; ok {
					paramParts = append(paramParts, fmt.Sprintf("%v", v))
				}
			}
			if team, ok := extra["team"]; ok && team != nil {
				paramParts = append(paramParts, fmt.Sprintf("%v", team))
			}
			if len(paramParts) > 0 {
				descrOut = appendDescrLine(descrOut, "[参数:"+strings.Join(paramParts, ",")+"]")
			}
			fields["descr"] = descrOut
		} else {
			// nexusphp(API):独立 taxonomy 字段
			for _, key := range []string{"source", "medium", "codec", "standard", "processing", "team", "audiocodec"} {
				if v, ok := extra[key]; ok && v != nil {
					if s, ok := v.(string); ok && s == "" {
						continue
					}
					fields[key] = fmt.Sprintf("%v", v)
				}
			}
			for _, pair := range [][2]string{
				{"source", "source"}, {"medium", "medium"}, {"standard", "standard"},
				{"codec", "codec"}, {"audiocodec", "audiocodec"},
			} {
				fieldKey := pair[1]
				if _, ok := fields[fieldKey]; ok {
					continue
				}
				dimKey := pair[0]
				if v, ok := dims[dimKey]; ok {
					fields[fieldKey] = v
				}
			}
		}

		// tags:nexusphp(API)有独立 tags 数组;classic 无 → 仅并入 descr
		if typ == "nexusphp" {
			if tags, ok := extra["tags"]; ok && tags != nil {
				fields["tags"] = tags
			}
		}
		if up, ok := extra["uplver"]; ok && up != nil {
			fields["uplver"] = fmt.Sprintf("%v", up)
		}

		siteCats := site.Categories()
		catKey := getStr(extra, "category")
		if catKey == "" {
			catKey = parsedCategoryName(parsed)
		}
		cid := ResolveCategoryID(catKey, siteCats, map[string]int{}, nil)
		if cid != nil {
			fields["type"] = *cid
		} else if fc, ok := extra["fallback_category"]; ok && fc != nil {
			fields["type"] = toIntVal(fc)
		}
		return fields
	}

	// ---- M-Team ----
	fields := map[string]any{"name": name, "descr": descr}
	if smallDescr != "" {
		fields["smallDescr"] = smallDescr
	}
	imdb := ExtractIMDB(extra["imdb"], extra["url"], parsedIMDB(parsed))
	if imdb != "" {
		fields["imdb"] = imdb
	}
	if db, ok := extra["douban"]; ok && db != nil {
		fields["douban"] = fmt.Sprintf("%v", db)
	}
	for _, key := range []string{"countries", "labels", "tags", "mediainfo"} {
		if v, ok := extra[key]; ok && v != nil {
			fields[key] = v
		}
	}
	// 维度 ID 映射:team 支持名字→ID 解析
	for _, pair := range [][2]string{
		{"source", "source"}, {"medium", "medium"}, {"standard", "standard"},
		{"videoCodec", "video_codec"}, {"video_codec", "video_codec"},
		{"audioCodec", "audio_codec"}, {"audio_codec", "audio_codec"},
		{"team", "team"}, {"processing", "processing"},
	} {
		k, v := pair[0], pair[1]
		val, ok := extra[k]
		if !ok || val == nil {
			continue
		}
		if s, isStr := val.(string); isStr && s == "" {
			continue
		}
		if k == "team" {
			teams := map[string]int{}
			if tp, ok := site.(TeamProvider); ok {
				teams = tp.Teams()
			}
			val = resolveTeamID(val, teams)
		}
		if isDigits(fmt.Sprintf("%v", val)) {
			fields[v] = toIntVal(val)
		} else {
			fields[v] = fmt.Sprintf("%v", val)
		}
	}
	// 未显式给的维度:从标题自动解析
	for _, pair := range [][2]string{
		{"standard", "standard"}, {"video_codec", "video_codec"}, {"audio_codec", "audio_codec"},
	} {
		fieldKey := pair[1]
		if _, ok := fields[fieldKey]; ok {
			continue
		}
		dimKey := pair[0]
		if v, ok := dims[dimKey]; ok {
			fields[fieldKey] = v
		}
	}
	// M-Team 有独立 codec 维度名
	if codec, ok := extra["codec"]; ok && codec != nil {
		if _, has := fields["video_codec"]; !has {
			fields["video_codec"] = fmt.Sprintf("%v", codec)
		}
	}

	siteCats := site.Categories()
	defaultCats := targetDefaultCategories(site)
	catKey := getStr(extra, "category")
	if catKey == "" {
		catKey = parsedCategoryName(parsed)
	}
	cid := ResolveCategoryID(catKey, siteCats, defaultCats, nil)
	if cid == nil {
		if fc, ok := extra["fallback_category"]; ok && fc != nil {
			cid = toIntPtr(fc)
		}
	}
	if cid != nil {
		fields["category"] = *cid
	}
	// M-Team 必填 anonymous(bool)
	if anon, ok := extra["anonymous"]; ok {
		fields["anonymous"] = toBool(anon)
	} else {
		fields["anonymous"] = false
	}
	return fields
}

// BuildUploadFields 把 ParsedTorrent 映射成目标站上传字段(统一核心入口)。
func BuildUploadFields(parsed *parser.ParsedTorrent, site TargetSite, extra map[string]any) map[string]any {
	if extra == nil {
		extra = map[string]any{}
	}
	typ := site.SiteType()
	if typ == "" {
		typ = "nexusphp"
	}

	// 标题:extra 覆盖优先;否则按站点类型规范化
	nameRaw := getStr(extra, "name")
	if nameRaw == "" {
		nameRaw = parsed.Name
	}
	name := NormalizeTitle(nameRaw, typ)
	// descr:extra 覆盖优先;否则留空
	descrText := getStr(extra, "descr")

	// 公共:smallDescr(所有适配器)
	smallDescr := BuildSmallDescr(parsed, extra, typ)
	// 公共:维度解析
	dims := ResolveDimensions(name, typ)

	// 差异:字段名映射
	return mapByType(typ, parsed, name, descrText, smallDescr, dims, extra, site)
}
