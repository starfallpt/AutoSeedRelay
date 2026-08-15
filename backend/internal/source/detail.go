package source

import (
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultUserAgent 默认 UA:贴近浏览器,减少被误伤概率。
const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// DetailFetchError 详情/文件列表抓取失败(网络错误、非 200、未登录、空响应等)。
type DetailFetchError struct {
	msg string
}

// Error implements the error interface.
func (e *DetailFetchError) Error() string { return e.msg }

func errf(format string, args ...any) error {
	return &DetailFetchError{msg: fmt.Sprintf(format, args...)}
}

var sizeUnits = map[string]int64{
	"B": 1, "KB": 1024, "MB": 1024 * 1024, "GB": 1024 * 1024 * 1024,
	"TB": 1024 * 1024 * 1024 * 1024, "PB": 1024 * 1024 * 1024 * 1024 * 1024,
}

var humanSizeRe = regexp.MustCompile(`(?i)([0-9]+(?:\.[0-9]+)?)\s*([KMGTP]?B)`)

// ParseHumanSize 把 '6.70 GB' / '918.08 MB' 之类的可读大小解析成字节;失败返回 nil。
func ParseHumanSize(text string) *int64 {
	m := humanSizeRe.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	value, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return nil
	}
	unit := strings.ToUpper(m[2])
	mult, ok := sizeUnits[unit]
	if !ok {
		mult = 1
	}
	out := int64(value * float64(mult))
	return &out
}

var (
	brSpaceRe    = regexp.MustCompile(`(?i)<br\s*/?>`)
	anyTagRe     = regexp.MustCompile(`</?[^>]+>`)
	wsCollapseRe = regexp.MustCompile(`\s+`)
)

func stripTags(s string) string {
	s = brSpaceRe.ReplaceAllString(s, " ")
	s = anyTagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.TrimSpace(wsCollapseRe.ReplaceAllString(s, " "))
}

// ParseCookie 接受 map[string]string 或 'k=v; k2=v2' 字符串,统一成 map。
func ParseCookie(cookie any) map[string]string {
	if cookie == nil {
		return map[string]string{}
	}
	if m, ok := cookie.(map[string]string); ok {
		out := make(map[string]string, len(m))
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	if m, ok := cookie.(map[string]any); ok {
		out := make(map[string]string, len(m))
		for k, v := range m {
			out[k] = fmt.Sprintf("%v", v)
		}
		return out
	}
	out := map[string]string{}
	for _, part := range strings.Split(fmt.Sprintf("%v", cookie), ";") {
		if idx := strings.Index(part, "="); idx >= 0 {
			k := strings.TrimSpace(part[:idx])
			v := strings.TrimSpace(part[idx+1:])
			if k != "" {
				out[k] = v
			}
		}
	}
	return out
}

// detailRowRe 详情页信息行:<tr><td class="rowhead ...">标签</td><td class="rowfollow ...">值</td></tr>
var detailRowRe = regexp.MustCompile(
	`(?s)<tr>\s*<td[^>]*class="?rowhead"?[^>]*>(.*?)</td>\s*<td[^>]*class="?rowfollow"?[^>]*>(.*?)</td>\s*</tr>`,
)

func rowValue(htmlStr, label string) (string, bool) {
	for _, m := range detailRowRe.FindAllStringSubmatch(htmlStr, -1) {
		if stripTags(m[1]) == label {
			return m[2], true
		}
	}
	return "", false
}

// fileRowRe viewfilelist 文件行:<tr><td class=rowfollow>文件名</td><td class=rowfollow ...>大小</td></tr>
var fileRowRe = regexp.MustCompile(
	`(?s)<tr>\s*<td[^>]*class="?rowfollow"?[^>]*>(.*?)</td>\s*<td[^>]*class="?rowfollow"?[^>]*>(.*?)</td>\s*</tr>`,
)

// FileInfo 单个文件(与 SeedDetail.Files 对齐)。
type FileInfo struct {
	Name      string
	Path      string
	Size      *int64
	SizeHuman string
}

// xmlFileList 部分 NexusPHP 版本 viewfilelist 可能输出 XML。
type xmlFileList struct {
	Files []struct {
		Name string `xml:"name,attr"`
		Size string `xml:"size,attr"`
	} `xml:"file"`
}

// ParseFileListHTML 解析 viewfilelist 返回体,得到 []FileInfo。
func ParseFileListHTML(htmlStr string) []FileInfo {
	var files []FileInfo
	for _, m := range fileRowRe.FindAllStringSubmatch(htmlStr, -1) {
		name := strings.TrimSpace(html.UnescapeString(anyTagRe.ReplaceAllString(m[1], "")))
		if name == "" {
			continue
		}
		sizeHuman := stripTags(m[2])
		files = append(files, FileInfo{
			Name:      name,
			Path:      name,
			Size:      ParseHumanSize(sizeHuman),
			SizeHuman: sizeHuman,
		})
	}
	if len(files) == 0 && strings.Contains(htmlStr, "<FileList>") {
		var xl xmlFileList
		if err := xml.Unmarshal([]byte(htmlStr), &xl); err == nil {
			for _, f := range xl.Files {
				size := int64(0)
				if n, err := strconv.ParseInt(f.Size, 10, 64); err == nil {
					size = n
				}
				files = append(files, FileInfo{
					Name:      html.UnescapeString(f.Name),
					Path:      html.UnescapeString(f.Name),
					Size:      &size,
					SizeHuman: f.Size,
				})
			}
		}
	}
	return files
}

// ParseSmallDescr 从详情页提取小描述(副标题行)。
func ParseSmallDescr(htmlStr string) string {
	if value, ok := rowValue(htmlStr, "副标题"); ok {
		return stripTags(value)
	}
	return ""
}

var spanRe = regexp.MustCompile(`(?s)<span[^>]*>(.*?)</span>`)

// ParseTags 从详情页提取标签(标签行内 <span>标签</span>)。
func ParseTags(htmlStr string) []string {
	row, ok := rowValue(htmlStr, "标签")
	if !ok {
		return nil
	}
	var tags []string
	seen := map[string]bool{}
	for _, m := range spanRe.FindAllStringSubmatch(row, -1) {
		text := stripTags(m[1])
		if text != "" && !seen[text] {
			seen[text] = true
			tags = append(tags, text)
		}
	}
	return tags
}

var ttRe = regexp.MustCompile(`(tt\d{6,})`)

// ParseIMDB 从详情页提取 IMDb ID(页面内首个 tt\d{6,})。
func ParseIMDB(htmlStr string) string {
	if m := ttRe.FindStringSubmatch(htmlStr); m != nil {
		return m[1]
	}
	return ""
}

// DetailFetcher 详情/文件列表抓取客户端(HTML 正则 + API 两条路径)。
type DetailFetcher struct {
	BaseURL  string
	Referer  string
	headers  map[string]string
	client   *http.Client
	apiToken string
	checkURL func(string) error
}

// DetailFetcherOptions 可选配置。
type DetailFetcherOptions struct {
	Cookie          any
	Referer         string
	UserAgent       string
	Headers         map[string]string
	TimeoutSeconds  float64
	FollowRedirects bool
	APIToken        string
	HTTPClient      *http.Client
	URLChecker      func(string) error
}

// NewDetailFetcher 构造抓取客户端。
func NewDetailFetcher(baseURL string, opts DetailFetcherOptions) *DetailFetcher {
	baseURL = strings.TrimRight(baseURL, "/")
	cookieMap := ParseCookie(opts.Cookie)
	referer := opts.Referer
	if referer == "" {
		referer = baseURL + "/"
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	hdrs := map[string]string{
		"User-Agent":      ua,
		"Accept-Encoding": "gzip, deflate",
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Referer":         referer,
	}
	for k, v := range opts.Headers {
		hdrs[k] = v
	}
	if len(cookieMap) > 0 {
		var parts []string
		for k, v := range cookieMap {
			parts = append(parts, k+"="+v)
		}
		hdrs["Cookie"] = strings.Join(parts, "; ")
	}

	timeout := opts.TimeoutSeconds
	if timeout <= 0 {
		timeout = 30
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: time.Duration(timeout * float64(time.Second))}
	}
	if !opts.FollowRedirects {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	checkURL := opts.URLChecker
	if checkURL == nil {
		checkURL = safeURL
	}

	return &DetailFetcher{
		BaseURL:  baseURL,
		Referer:  referer,
		headers:  hdrs,
		client:   client,
		apiToken: opts.APIToken,
		checkURL: checkURL,
	}
}

// Close 释放底层连接。
func (f *DetailFetcher) Close() { f.client.CloseIdleConnections() }

func (f *DetailFetcher) get(ctx context.Context, u string) ([]byte, int, error) {
	if err := f.checkURL(u); err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range f.headers {
		req.Header.Set(k, v)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := readBody(resp, maxDetailBody)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// FetchDetailPage 抓取 details.php 原始 HTML(未做解析)。需登录 cookie。
func (f *DetailFetcher) FetchDetailPage(ctx context.Context, torrentID int) (string, error) {
	u := f.BaseURL + "/details.php?id=" + strconv.Itoa(torrentID)
	body, status, err := f.get(ctx, u)
	if err != nil {
		return "", errf("details.php?id=%d 网络/读取错误: %v", torrentID, err)
	}
	if status == 301 || status == 302 || status == 303 || status == 307 || status == 308 {
		return "", errf("details.php?id=%d 重定向:需要登录(cookie 缺失或无效)", torrentID)
	}
	if status != 200 {
		return "", errf("details.php?id=%d: HTTP %d", torrentID, status)
	}
	return string(body), nil
}

// FetchFileListPage 抓取 viewfilelist.php 原始返回体(HTML table / XML)。
func (f *DetailFetcher) FetchFileListPage(ctx context.Context, torrentID int) (string, error) {
	u := f.BaseURL + "/viewfilelist.php?id=" + strconv.Itoa(torrentID)
	body, status, err := f.get(ctx, u)
	if err != nil {
		return "", errf("viewfilelist.php?id=%d 网络/读取错误: %v", torrentID, err)
	}
	if status != 200 {
		return "", errf("viewfilelist.php?id=%d: HTTP %d", torrentID, status)
	}
	return string(body), nil
}

// FetchFileList 抓取并解析文件列表。
func (f *DetailFetcher) FetchFileList(ctx context.Context, torrentID int) ([]FileInfo, error) {
	htmlStr, err := f.FetchFileListPage(ctx, torrentID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(htmlStr) == "" {
		return nil, errf("viewfilelist.php?id=%d 返回空 body:需登录 cookie 或该种子无文件列表", torrentID)
	}
	return ParseFileListHTML(htmlStr), nil
}

// FetchAllDetail 获取种子全部详情:有 token 优先走 API,失败回退 Cookie+HTML;
// 无 token 直接 HTML 爬取。
func (f *DetailFetcher) FetchAllDetail(ctx context.Context, torrentID int) (*SeedDetail, error) {
	if f.apiToken != "" {
		api := NewAPIFetcher(f.BaseURL, f.apiToken, 30)
		api.checkURL = f.checkURL
		api.client = f.client
		if sd, apiErr := api.FetchAllAPI(ctx, torrentID); apiErr == nil && sd != nil {
			return sd, nil
		}
	}
	return f.fetchDetailHTML(ctx, torrentID)
}

// fetchDetailHTML 通过 Cookie+HTML 爬取种子详情。
func (f *DetailFetcher) fetchDetailHTML(ctx context.Context, torrentID int) (*SeedDetail, error) {
	files, err := f.FetchFileList(ctx, torrentID)
	if err != nil {
		return nil, err
	}
	detail, err := f.FetchDetailPage(ctx, torrentID)
	if err != nil {
		return nil, err
	}

	entries := make([]FileEntry, 0, len(files))
	for _, fi := range files {
		entry := FileEntry{Name: fi.Name}
		if fi.Size != nil {
			entry.Size = *fi.Size
		}
		entries = append(entries, entry)
	}

	return &SeedDetail{
		ID:         torrentID,
		SmallDescr: ParseSmallDescr(detail),
		Tags:       ParseTags(detail),
		IMDb:       ParseIMDB(detail),
		DescrHTML:  detail,
		Files:      entries,
	}, nil
}
