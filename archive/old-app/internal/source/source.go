// Package source 源站客户端:RSS 抓取 + .torrent 下载。
//
//   - RSS 解析:兼容 NexusPHP 系 `torrentrss.php` 输出(标准 RSS 2.0)
//   - 种子下载两种显式模式:
//     1. download_mode="direct"(默认):服务端代理模式,本进程直接请求,
//        URL 优先 passkey 拼接(download.php?id=&passkey=,NexusPHP 标准机制),
//        可配 HTTP 代理(AUTOSEED_PROXY)。
//     2. download_mode="qb":qB 直拉模式,把下载交给 qBittorrent
//        (qB 服务器直连源站拉 .torrent),再从 qB export 取回。
//
// cf_mode 仅作为内部 HTTP 后端参数保留(默认 auto)。Go 版只有 net/http
// (httpx)后端;Python 的 curl_cffi / cloudscraper 为可选增强,Go 中不存在,
// 显式指定时回退到 httpx。
package source

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/autoseedrelay/go-relay/internal/detail"
	"github.com/autoseedrelay/go-relay/internal/qb"
)

// RssItem RSS 单条种子的解析结果。
type RssItem struct {
	ID           string // torrent id(从 link 解析)
	Title        string
	Link         string
	Description  string // descr 全量 HTML
	CategoryName string
	CategoryID   string
	Size         *int64 // enclosure length
	EnclosureURL string
	GUID         string // info_hash hex
	Author       string
	PubDate      string

	// 清洗出的结构字段
	IMDB       string // tt\d{6,}
	SmallDescr string
}

// MatchesKeywords 标题后缀/关键词匹配(不区分大小写)。命中任一即返回 True。
func (it *RssItem) MatchesKeywords(keywords []string) bool {
	low := strings.ToLower(it.Title)
	for _, k := range keywords {
		if k != "" && strings.Contains(low, strings.ToLower(k)) {
			return true
		}
	}
	return false
}

// --------------------------------------------------------------------------
// RSS 解析
// --------------------------------------------------------------------------

type rssEnvelope struct {
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Items []rssItemXML `xml:"item"`
}

type rssItemXML struct {
	Title       string      `xml:"title"`
	Link        string      `xml:"link"`
	Description string      `xml:"description"`
	Category    rssCategory `xml:"category"`
	Enclosure   rssEncXML   `xml:"enclosure"`
	GUID        string      `xml:"guid"`
	Author      string      `xml:"author"`
	PubDate     string      `xml:"pubDate"`
}

type rssCategory struct {
	Text   string `xml:",chardata"`
	Domain string `xml:"domain,attr"`
}

type rssEncXML struct {
	URL    string `xml:"url,attr"`
	Length string `xml:"length,attr"`
}

var imdbRssRe = regexp.MustCompile(`(tt\d{6,})`)
var smallDescrRssRe = regexp.MustCompile(`副标题[:：]\s*([^\n<]+)`)

// ParseRSS 解析 NexusPHP 系 RSS 输出。
func ParseRSS(xmlBytes []byte) ([]RssItem, error) {
	var env rssEnvelope
	if err := xml.Unmarshal(xmlBytes, &env); err != nil {
		return nil, fmt.Errorf("解析 RSS XML 失败: %v", err)
	}
	var items []RssItem
	for _, el := range env.Channel.Items {
		link := strings.TrimSpace(el.Link)
		m := regexp.MustCompile(`id=(\d+)`).FindStringSubmatch(link)
		tid := ""
		if m != nil {
			tid = m[1]
		}

		catID := ""
		if cm := regexp.MustCompile(`cat=(\d+)`).FindStringSubmatch(el.Category.Domain); cm != nil {
			catID = cm[1]
		}

		var size *int64
		if el.Enclosure.Length != "" {
			if n, err := strconv.ParseInt(el.Enclosure.Length, 10, 64); err == nil {
				size = &n
			}
		}

		description := el.Description
		imdb := ""
		if im := imdbRssRe.FindStringSubmatch(description); im != nil {
			imdb = im[1]
		}
		smallDescr := ""
		if sm := smallDescrRssRe.FindStringSubmatch(description); sm != nil {
			smallDescr = strings.TrimSpace(sm[1])
		}

		items = append(items, RssItem{
			ID:           tid,
			Title:        strings.TrimSpace(el.Title),
			Link:         link,
			Description:  description,
			CategoryName: strings.TrimSpace(el.Category.Text),
			CategoryID:   catID,
			Size:         size,
			EnclosureURL: el.Enclosure.URL,
			GUID:         strings.TrimSpace(el.GUID),
			Author:       strings.TrimSpace(el.Author),
			PubDate:      strings.TrimSpace(el.PubDate),
			IMDB:         imdb,
			SmallDescr:   smallDescr,
		})
	}
	return items, nil
}

// --------------------------------------------------------------------------
// 常量与判定
// --------------------------------------------------------------------------

// defaultBrowserHeaders 一个"看起来像浏览器"的默认头。
var defaultBrowserHeaders = map[string]string{
	"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	"Accept":     "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
	"Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
	"Sec-Ch-Ua":          `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`,
	"Sec-Ch-Ua-Mobile":   "?0",
	"Sec-Ch-Ua-Platform": `"Windows"`,
	"Sec-Fetch-Dest":     "document",
	"Sec-Fetch-Mode":     "navigate",
	"Sec-Fetch-Site":     "same-origin",
	"Sec-Fetch-User":     "?1",
	"Upgrade-Insecure-Requests": "1",
}

// isTorrentBody 判定响应是否真的是 .torrent(bencode 以 d 开头)而非拦截页。
func isTorrentBody(status int, content []byte, contentType string) bool {
	if status != 200 {
		return false
	}
	if len(content) > 0 && content[0] == 'd' { // bencode dict 总是以 d 开头
		return true
	}
	return strings.Contains(strings.ToLower(contentType), "x-bittorrent")
}

// --------------------------------------------------------------------------
// SourceClient
// --------------------------------------------------------------------------

// SourceClientOptions 源站客户端配置。
type SourceClientOptions struct {
	TimeoutSeconds  float64
	Headers         map[string]string
	FollowRedirects bool
	Passkey         string
	Cookie          string
	CFMode          string // auto | curl_cffi | cloudscraper | httpx
	QBHost          string
	QBUser          string
	QBPass          string
	DownloadMode    string // direct | qb
	Proxy           string
	APIToken        string // Sanctum Bearer token,用于详情 API 获取
}

// SourceClient 源站客户端:RSS 抓取 + 种子下载。
type SourceClient struct {
	RSSURL         string
	Timeout        float64
	BaseHeaders    map[string]string
	Passkey        string
	Cookie         string
	CFMode         string
	DownloadMode   string
	Proxy          string
	QBHost         string
	QBUser         string
	QBPass         string
	apiToken       string
	cookieDict     map[string]string
	httpClient     *http.Client
	qbClient       *qb.QBittorrent
	qbMu           sync.Mutex
	backend        string
}

// NewSourceClient 构造源站客户端。环境变量兜底:
// AUTOSEED_PASSKEY / AUTOSEED_COOKIE / AUTOSEED_PROXY / QBHOST / QBUSER / QBPASS
// / AUTOSEED_DOWNLOAD_MODE。
func NewSourceClient(rssURL string, opts SourceClientOptions) (*SourceClient, error) {
	cfMode := strings.ToLower(opts.CFMode)
	if cfMode == "" {
		cfMode = "auto"
	}
	if cfMode != "auto" && cfMode != "curl_cffi" && cfMode != "cloudscraper" && cfMode != "httpx" {
		return nil, fmt.Errorf("未知 cf_mode: %q", cfMode)
	}
	downloadMode := strings.ToLower(opts.DownloadMode)
	if downloadMode == "" {
		downloadMode = os.Getenv("AUTOSEED_DOWNLOAD_MODE")
	}
	if downloadMode == "" {
		downloadMode = "direct"
	}
	if downloadMode != "direct" && downloadMode != "qb" {
		return nil, fmt.Errorf("未知 download_mode: %q", downloadMode)
	}

	passkey := opts.Passkey
	if passkey == "" {
		passkey = os.Getenv("AUTOSEED_PASSKEY")
	}
	cookie := opts.Cookie
	if cookie == "" {
		cookie = os.Getenv("AUTOSEED_COOKIE")
	}
	proxy := opts.Proxy
	if proxy == "" {
		proxy = os.Getenv("AUTOSEED_PROXY")
	}
	qbHost := opts.QBHost
	if qbHost == "" {
		qbHost = os.Getenv("QBHOST")
	}
	qbUser := opts.QBUser
	if qbUser == "" {
		qbUser = os.Getenv("QBUSER")
	}
	qbPass := opts.QBPass
	if qbPass == "" {
		qbPass = os.Getenv("QBPASS")
	}

	baseHeaders := make(map[string]string, len(defaultBrowserHeaders)+len(opts.Headers))
	for k, v := range defaultBrowserHeaders {
		baseHeaders[k] = v
	}
	for k, v := range opts.Headers {
		baseHeaders[k] = v
	}

	timeout := opts.TimeoutSeconds
	if timeout <= 0 {
		timeout = 30
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
	}
	if proxy != "" {
		if pu, err := url.Parse(proxy); err == nil {
			transport.Proxy = http.ProxyURL(pu)
		}
	}
	client := &http.Client{
		Timeout:   time.Duration(timeout * float64(time.Second)),
		Transport: transport,
	}
	if !opts.FollowRedirects {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	return &SourceClient{
		RSSURL:       rssURL,
		Timeout:      timeout,
		BaseHeaders:  baseHeaders,
		Passkey:      passkey,
		Cookie:       cookie,
		CFMode:       cfMode,
		DownloadMode: downloadMode,
		Proxy:        proxy,
		QBHost:       qbHost,
		QBUser:       qbUser,
		QBPass:       qbPass,
		apiToken:     opts.APIToken,
		cookieDict:   parseCookie(cookie),
		httpClient:   client,
	}, nil
}

func parseCookie(cookie string) map[string]string {
	out := map[string]string{}
	if cookie == "" {
		return out
	}
	for _, part := range strings.Split(cookie, ";") {
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

func (c *SourceClient) buildHeaders(referer string) map[string]string {
	h := make(map[string]string, len(c.BaseHeaders)+1)
	for k, v := range c.BaseHeaders {
		h[k] = v
	}
	if referer != "" {
		h["Referer"] = referer
	}
	return h
}

func (c *SourceClient) siteBase() string {
	m := regexp.MustCompile(`^(https?://[^/]+)`).FindStringSubmatch(c.RSSURL)
	if m != nil {
		return m[1]
	}
	return ""
}

// torrentURLs 生成候选下载 URL,按可用性排序:
//  1. passkey + download.php?id={id}&passkey={passkey}(首选,标准机制)
//  2. enclosure_url(downhash 直链,JWT 仍有效时)
//  3. cookie + download.php?id={id}(无 passkey,登录会话下载)
func (c *SourceClient) torrentURLs(item *RssItem) []string {
	var urls []string
	site := c.siteBase()
	if item.ID != "" {
		if m := regexp.MustCompile(`(https?://[^/]+)`).FindStringSubmatch(item.Link); m != nil {
			site = m[1]
		}
		if c.Passkey != "" {
			urls = append(urls, fmt.Sprintf("%s/download.php?id=%s&passkey=%s", site, item.ID, c.Passkey))
		}
	}
	if item.EnclosureURL != "" {
		urls = append(urls, item.EnclosureURL)
	}
	if item.ID != "" && c.Cookie != "" {
		urls = append(urls, fmt.Sprintf("%s/download.php?id=%s", site, item.ID))
	}
	return urls
}

// FetchRSS 抓取 RSS 并解析。
func (c *SourceClient) FetchRSS() ([]RssItem, error) {
	resp, err := c.httpClient.Get(c.RSSURL)
	if err != nil {
		return nil, fmt.Errorf("RSS 抓取失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("RSS 抓取失败: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("RSS 读取失败: %v", err)
	}
	return ParseRSS(body)
}

// DownloadTorrent 下载 .torrent 到 outPath。返回是否成功。
//
// download_mode=direct:本进程直接请求(net/http 后端,可配 HTTP 代理);
// download_mode=qb:交给 qBittorrent 拉种子再 export 取回。
//
// 返回 false 表示未下载成功且无网络错误(比如所有 URL 均被源站限制);
// 返回 error 表示网络/校验失败。
func (c *SourceClient) DownloadTorrent(item *RssItem, outPath string) (bool, error) {
	urls := c.torrentURLs(item)
	if len(urls) == 0 {
		return false, errors.New("没有可用的下载 URL(enclosure/passkey/cookie 均缺失)")
	}

	if c.DownloadMode == "qb" {
		return c.DownloadViaQB(item, outPath)
	}

	// direct 模式:服务端代理(本进程请求 + 可选 HTTP 代理)
	var lastErr error
	for _, u := range urls {
		ok, err := c.downloadWith(u, outPath)
		if err != nil {
			lastErr = err
			continue // 网络/校验失败 → 试下一个 URL
		}
		return ok, nil // ok=false 表示拿到了非 torrent 内容(被拦截),不再试
	}
	if lastErr != nil {
		return false, lastErr
	}
	return false, errors.New("direct 模式下载失败:所有 URL 均被源站限制。可改用 download_mode=qb(qB 直拉)或配置 HTTP 代理 AUTOSEED_PROXY")
}

func (c *SourceClient) downloadWith(url, outPath string) (bool, error) {
	ok, content, err := c.rawGet(url)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	if err := os.WriteFile(outPath, content, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// rawGet 发起 GET,返回 (是否拿到 torrent, 内容, 错误)。
func (c *SourceClient) rawGet(u string) (bool, []byte, error) {
	referer := c.siteBase() + "/details.php"
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return false, nil, err
	}
	for k, v := range c.buildHeaders(referer) {
		req.Header.Set(k, v)
	}
	if len(c.cookieDict) > 0 {
		var parts []string
		for k, v := range c.cookieDict {
			parts = append(parts, k+"="+v)
		}
		req.Header.Set("Cookie", strings.Join(parts, "; "))
	}

	// 单次请求用独立连接(follow_redirects=False)
	client := *c.httpClient
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, nil, fmt.Errorf("httpx GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, nil, fmt.Errorf("httpx GET %s 读取失败: %v", u, err)
	}
	ct := resp.Header.Get("Content-Type")
	if isTorrentBody(resp.StatusCode, body, ct) {
		return true, body, nil
	}
	return false, nil, errors.New(describeFail(u, "httpx", resp.StatusCode, resp.Header, body))
}

func isCFBlock(status int, header http.Header, body []byte) bool {
	if status == 404 && strings.Contains(strings.ToLower(header.Get("Server")), "cloudflare") {
		return true
	}
	switch header.Get("Cf-Mitigated") {
	case "challenge", "managed_challenge", "block":
		return true
	}
	if status == 403 || status == 503 {
		lower := strings.ToLower(string(body))
		if len(lower) > 400 {
			lower = lower[:400]
		}
		if strings.Contains(lower, "cloudflare") {
			return true
		}
	}
	return false
}

func describeFail(u, backend string, status int, header http.Header, body []byte) string {
	ctype := header.Get("Content-Type")
	server := header.Get("Server")
	snippet := string(body)
	if len(snippet) > 120 {
		snippet = snippet[:120]
	}
	tag := ""
	if isCFBlock(status, header, body) {
		tag = "被 CF/WAF 拦截"
	}
	return fmt.Sprintf("[%s] download %s: HTTP %d ct=%q server=%q (%s body=%dB, %q)",
		backend, u, status, ctype, server, tag, len(body), snippet)
}

// DownloadViaQB 经 qBittorrent 直拉种子(qB 直拉模式,download_mode="qb")。
func (c *SourceClient) DownloadViaQB(item *RssItem, outPath string) (bool, error) {
	if c.QBHost == "" || c.QBUser == "" || c.QBPass == "" {
		return false, errors.New("qB 中转缺少凭据:设置 QBHOST/QBUSER/QBPASS(或构造参数)")
	}
	urls := c.torrentURLs(item)
	if len(urls) == 0 {
		return false, errors.New("没有可用的下载 URL")
	}
	url := urls[0]

	c.qbMu.Lock()
	if c.qbClient == nil {
		client, err := qb.NewQBittorrent(c.QBHost, c.QBUser, c.QBPass, c.Timeout)
		if err != nil {
			c.qbMu.Unlock()
			return false, err
		}
		c.qbClient = client
	}
	c.qbMu.Unlock()
	client := c.qbClient
	if err := client.Login(); err != nil {
		return false, err
	}

	cookieArg := c.Cookie

	// 记录添加前该分类下已有的 hash(用于识别"新增"的种子,防止误取旧种子)
	before := map[string]bool{}
	if infos, err := client.Info(); err == nil {
		for _, t := range infos {
			if cat, _ := t["category"].(string); cat == "relay-pending" {
				if h, ok := t["hash"].(string); ok {
					before[h] = true
				}
			}
		}
	}

	add, err := client.AddTorrentURL(url, qb.AddOptions{
		Cookie:   cookieArg,
		Category: "relay-pending",
		Paused:   true,
	})
	if err != nil {
		return false, err
	}

	// qB 5.x 返回 {"added_torrent_ids":[...],"success_count":N,...};4.x 返回 Ok.
	var hashes []string
	if ids, ok := add["added_torrent_ids"].([]any); ok {
		for _, h := range ids {
			if s, ok := h.(string); ok {
				hashes = append(hashes, s)
			}
		}
	}

	// pending 场景:qB 接受 URL 但 metadata 尚未解析。轮询该分类直到出现新种子,最多 30s。
	if len(hashes) == 0 {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			cands := []string{}
			if infos, err := client.Info(); err == nil {
				for _, t := range infos {
					cat, _ := t["category"].(string)
					h, _ := t["hash"].(string)
					if cat == "relay-pending" && h != "" && !before[h] {
						cands = append(cands, h)
					}
				}
			}
			if len(cands) > 0 {
				hashes = []string{cands[len(cands)-1]}
				break
			}
			time.Sleep(3 * time.Second)
		}
	}
	if len(hashes) == 0 {
		return false, fmt.Errorf("qB 添加种子后未找到新增 hash(add 响应: %v)", add)
	}

	h := hashes[0]
	deadline := time.Now().Add(120 * time.Second)
	metaOK := false
	for time.Now().Before(deadline) {
		t, err := client.GetTorrent(h)
		if err == nil && t != nil {
			if hm, ok := t["has_metadata"].(bool); ok && hm {
				metaOK = true
				break
			}
		}
		time.Sleep(3 * time.Second)
	}
	if !metaOK {
		_ = client.Delete(h, false)
		return false, fmt.Errorf("qB 120s 内未取到 .torrent 元数据(hash=%s, url=%s)。请确认 qB 服务器出口能访问源站,或已配 cookie/代理", h, truncate(url, 90))
	}

	data, err := client.ExportTorrent(h)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		return false, err
	}
	_ = client.Delete(h, false)
	return true, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Close 释放底层连接。
func (c *SourceClient) Close() {}

// DetailFetcher 构造详情/文件列表抓取客户端(复用源站 base URL 和认证信息)。
// 有 apiToken 时优先走 Sanctum API,失败回退 Cookie+HTML;无 token 时仅 HTML 爬取。
func (c *SourceClient) DetailFetcher() *detail.DetailFetcher {
	baseURL := c.siteBase()
	return detail.NewDetailFetcher(baseURL, detail.DetailFetcherOptions{
		Cookie:   c.Cookie,
		APIToken: c.apiToken,
	})
}

// APIToken 返回源站 API token(可能为空)。
func (c *SourceClient) APIToken() string { return c.apiToken }

// GuidToInfohash guid 字段可能是 info_hash hex,规范化小写。
func GuidToInfohash(guid string) string {
	if len(guid) == 40 && isHexString(guid) {
		return strings.ToLower(guid)
	}
	sum := sha1.Sum([]byte(guid))
	return hex.EncodeToString(sum[:])
}

func isHexString(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
