package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/autoseedrelay/relay/internal/qb"
)

// 各端点 HTTP 响应体读取上限(加固:防止异常大的 body 耗尽内存)。
const (
	maxRSSBody     = 10 << 20 // RSS 10MB
	maxDetailBody  = 20 << 20 // 详情/API 20MB
	maxTorrentBody = 64 << 20 // .torrent 64MB
)

// maxDownloadAttempts 单个 URL 的最大尝试次数(含首次),防止 403/503 无限重试。
const maxDownloadAttempts = 5

// BackoffFunc 计算第 attempt 次(0 起)退避重试前的等待时长。可注入用于测试。
type BackoffFunc func(attempt int) time.Duration

// DefaultBackoff 默认指数退避:基数 60s、倍增、上限 900s。
func DefaultBackoff(attempt int) time.Duration {
	const base = 60 * time.Second
	const max = 900 * time.Second
	d := base
	for i := 0; i < attempt; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	return d
}

// safeURL 校验待请求的 URL,防 SSRF:仅允许 http/https,并把主机名解析成 IP 后
// 拒绝环回/私网/链路本地/保留地址段。解析失败时默认拒绝(fail-closed)。
// 错误信息中的 URL 一律经过 RedactURL 脱敏,避免把 query 里的 passkey 外泄。
func safeURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("非法 URL %q: %w", RedactURL(raw), err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("仅允许 http/https 协议,拒绝 %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL %q 缺少主机名", RedactURL(raw))
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("解析主机 %q 失败: %w", host, err)
	}
	return checkIPs(host, ips)
}

// checkIPs 校验一组解析出的 IP:任一落在 SSRF 敏感段即拒绝(fail-closed)。
// 供 safeURL 与拨号层复用,保证"预检"与"拨号"用同一套私有/环回判断。
func checkIPs(host string, ips []net.IP) error {
	if len(ips) == 0 {
		return fmt.Errorf("主机 %q 未解析到任何 IP", host)
	}
	for _, ip := range ips {
		if isUnsafeIP(ip) {
			return fmt.Errorf("拒绝访问内网/保留地址 %q(%s)", host, ip.String())
		}
	}
	return nil
}

// isUnsafeIP 判断 IP 是否落在 SSRF 敏感段(环回/私网/链路本地/组播/未指定/
// CGNAT 等)。IPv4-mapped IPv6 会被 To4 归一化后再判断。
func isUnsafeIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		// 0.0.0.0/8
		if ip4[0] == 0 {
			return true
		}
		// 100.64.0.0/10 (CGNAT,常被用于云元数据/内网代理)
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return true
		}
		// 192.0.0.0/24 (IETF 协议保留)
		if ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 0 {
			return true
		}
		// 198.18.0.0/15 (基准测试保留)
		if ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19) {
			return true
		}
	}
	return false
}

// safeDialContext 是自定义 Transport 的拨号钩子:解析 host 后校验每一枚 IP,再
// 直接用校验过的 IP 拨号(解析与拨号同一 IP,消除 DNS rebinding 的 TOCTOU)。
func safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("解析主机 %q 失败: %w", host, err)
	}
	plain := make([]net.IP, 0, len(ips))
	for _, ia := range ips {
		plain = append(plain, ia.IP)
	}
	if err := checkIPs(host, plain); err != nil {
		return nil, err
	}
	var d net.Dialer
	var lastErr error
	for _, ip := range plain {
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("主机 %q 无可拨号地址", host)
	}
	return nil, lastErr
}

// safeRedirectCheck 返回一个 CheckRedirect:对每一跳目标重跑 checkURL(默认
// safeURL),命中 SSRF 敏感地址时返回错误停止跟随,从而逐跳复检重定向。
func safeRedirectCheck(checkURL func(string) error) func(*http.Request, []*http.Request) error {
	if checkURL == nil {
		checkURL = safeURL
	}
	return func(req *http.Request, via []*http.Request) error {
		if err := checkURL(req.URL.String()); err != nil {
			return fmt.Errorf("拒绝跟随重定向到 %s: %w", RedactURL(req.URL.String()), err)
		}
		return nil
	}
}

// readBody 用 http.MaxBytesReader 限制响应体大小后读取;超限返回 *http.MaxBytesError。
func readBody(resp *http.Response, limit int64) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(nil, resp.Body, limit))
}

// isTorrentBody 判定响应是否真的是 .torrent(bencode 字典以 'd' 开头)而非拦截页。
func isTorrentBody(status int, content []byte, contentType string) bool {
	if status != http.StatusOK {
		return false
	}
	if len(content) > 0 && content[0] == 'd' {
		return true
	}
	return strings.Contains(strings.ToLower(contentType), "x-bittorrent")
}

// defaultBrowserHeaders 一个"看起来像浏览器"的默认请求头。
var defaultBrowserHeaders = map[string]string{
	"User-Agent":                "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	"Accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
	"Accept-Language":           "zh-CN,zh;q=0.9,en;q=0.8",
	"Sec-Ch-Ua":                 `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`,
	"Sec-Ch-Ua-Mobile":          "?0",
	"Sec-Ch-Ua-Platform":        `"Windows"`,
	"Sec-Fetch-Dest":            "document",
	"Sec-Fetch-Mode":            "navigate",
	"Sec-Fetch-Site":            "same-origin",
	"Sec-Fetch-User":            "?1",
	"Upgrade-Insecure-Requests": "1",
}

// ClientOptions 源站客户端配置。
type ClientOptions struct {
	TimeoutSeconds  float64
	Headers         map[string]string
	FollowRedirects bool
	Passkey         string
	Cookie          string
	Proxy           string
	DownloadMode    string // direct | qb
	APIToken        string

	// qB 直拉(qb 模式)凭据。QBHost 形如 "http://host"(不含端口),QBPort 为端口。
	QBHost string
	QBPort string
	QBUser string
	QBPass string

	// 测试注入:退避策略、SSRF 校验、qB 实例、HTTP 客户端。nil 时用默认值。
	Backoff    BackoffFunc
	URLChecker func(string) error
	QB         *qb.Instance
	HTTPClient *http.Client
}

// Client 源站客户端:RSS 抓取 + 种子下载(含 qB 直拉)+ 详情抓取入口。
type Client struct {
	RSSURL       string
	Timeout      time.Duration
	BaseHeaders  map[string]string
	Passkey      string
	Cookie       string
	DownloadMode string
	Proxy        string

	apiToken   string
	cookieDict map[string]string
	httpClient *http.Client

	qbHost string
	qbPort string
	qbUser string
	qbPass string
	qb     *qb.Instance
	qbMu   sync.Mutex

	checkURL func(string) error
	backoff  BackoffFunc
}

// NewClient 构造源站客户端。环境变量兜底:
// AUTOSEED_PASSKEY / AUTOSEED_COOKIE / AUTOSEED_PROXY / AUTOSEED_DOWNLOAD_MODE。
func NewClient(rssURL string, opts ClientOptions) *Client {
	downloadMode := strings.ToLower(opts.DownloadMode)
	if downloadMode == "" {
		downloadMode = os.Getenv("AUTOSEED_DOWNLOAD_MODE")
	}
	if downloadMode == "" {
		downloadMode = "direct"
	}
	if downloadMode != "direct" && downloadMode != "qb" {
		downloadMode = "direct"
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

	baseHeaders := make(map[string]string, len(defaultBrowserHeaders)+len(opts.Headers))
	for k, v := range defaultBrowserHeaders {
		baseHeaders[k] = v
	}
	for k, v := range opts.Headers {
		baseHeaders[k] = v
	}

	timeout := time.Duration(opts.TimeoutSeconds * float64(time.Second))
	if opts.TimeoutSeconds <= 0 {
		timeout = 30 * time.Second
	}

	client := opts.HTTPClient
	if client == nil {
		transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
		if proxy != "" {
			if pu, err := url.Parse(proxy); err == nil {
				transport.Proxy = http.ProxyURL(pu)
			}
		} else {
			// 直连(无显式代理)时在拨号层复验 IP,消除 DNS rebinding TOCTOU。
			// 走代理时目标校验由 safeURL 预检完成,拨号目标是代理本身。
			transport.Proxy = nil
			transport.DialContext = safeDialContext
		}
		client = &http.Client{Timeout: timeout, Transport: transport}
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
	backoff := opts.Backoff
	if backoff == nil {
		backoff = DefaultBackoff
	}

	return &Client{
		RSSURL:       rssURL,
		Timeout:      timeout,
		BaseHeaders:  baseHeaders,
		Passkey:      passkey,
		Cookie:       cookie,
		DownloadMode: downloadMode,
		Proxy:        proxy,
		apiToken:     opts.APIToken,
		cookieDict:   parseCookie(cookie),
		httpClient:   client,
		qbHost:       opts.QBHost,
		qbPort:       opts.QBPort,
		qbUser:       opts.QBUser,
		qbPass:       opts.QBPass,
		qb:           opts.QB,
		checkURL:     checkURL,
		backoff:      backoff,
	}
}

// Close 释放底层 HTTP 连接。
func (c *Client) Close() { c.httpClient.CloseIdleConnections() }

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

func (c *Client) buildHeaders(referer string) map[string]string {
	h := make(map[string]string, len(c.BaseHeaders)+1)
	for k, v := range c.BaseHeaders {
		h[k] = v
	}
	if referer != "" {
		h["Referer"] = referer
	}
	return h
}

func (c *Client) siteBase() string {
	if m := regexp.MustCompile(`^(https?://[^/]+)`).FindStringSubmatch(c.RSSURL); m != nil {
		return m[1]
	}
	return ""
}

// torrentURLs 生成候选下载 URL,按优先级排序:
//  1. passkey + download.php?id={id}&passkey={passkey}(标准机制)
//  2. enclosure_url(downhash 直链)
//  3. cookie + download.php?id={id}(登录会话下载)
func (c *Client) torrentURLs(item *RssItem) []string {
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

// DownloadTorrent 下载 .torrent 到 outPath。返回是否成功。
//
// download_mode=direct:本进程直接请求(候选 URL 三级降级 + isTorrentBody 校验 +
// 403/503 退避);download_mode=qb:交给 qB 直拉再 export 取回。
func (c *Client) DownloadTorrent(ctx context.Context, item *RssItem, outPath string) (bool, error) {
	urls := c.torrentURLs(item)
	if len(urls) == 0 {
		return false, errors.New("没有可用的下载 URL(enclosure/passkey/cookie 均缺失)")
	}
	if c.DownloadMode == "qb" {
		return c.DownloadViaQB(ctx, item, outPath)
	}

	var lastErr error
	for _, u := range urls {
		if err := c.checkURL(u); err != nil {
			lastErr = err
			continue
		}
		data, err := c.downloadRaw(ctx, u)
		if err != nil {
			lastErr = err
			continue
		}
		if err := os.WriteFile(outPath, data, 0o600); err != nil {
			return false, err
		}
		return true, nil
	}
	if lastErr != nil {
		return false, lastErr
	}
	return false, errors.New("direct 模式下载失败:所有 URL 均被源站限制")
}

// downloadRaw 对单个 URL 做下载,403/503 走退避重试(有上限),拿到非 torrent
// body 或其它状态码直接失败。
func (c *Client) downloadRaw(ctx context.Context, u string) ([]byte, error) {
	var lastStatus int
	for attempt := 0; attempt < maxDownloadAttempts; attempt++ {
		body, status, ct, err := c.rawGet(ctx, u)
		if err != nil {
			return nil, err
		}
		lastStatus = status
		if isTorrentBody(status, body, ct) {
			return body, nil
		}
		if status == http.StatusForbidden || status == http.StatusServiceUnavailable {
			if err := c.waitBackoff(ctx, attempt); err != nil {
				return nil, err
			}
			continue
		}
		return nil, fmt.Errorf("下载 %s: HTTP %d 响应不是有效 .torrent(可能被 WAF/登录页拦截)", RedactURL(u), status)
	}
	return nil, fmt.Errorf("下载 %s: 连续 %d 次 HTTP %d(403/503)被限流,已停止重试", RedactURL(u), maxDownloadAttempts, lastStatus)
}

func (c *Client) waitBackoff(ctx context.Context, attempt int) error {
	wait := c.backoff(attempt)
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) rawGet(ctx context.Context, u string) ([]byte, int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, "", err
	}
	referer := c.siteBase() + "/details.php"
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

	client := *c.httpClient
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, "", fmt.Errorf("httpx GET %s: %w", RedactURL(u), err)
	}
	defer resp.Body.Close()
	body, err := readBody(resp, maxTorrentBody)
	if err != nil {
		return nil, resp.StatusCode, "", fmt.Errorf("httpx GET %s 读取失败: %w", RedactURL(u), err)
	}
	return body, resp.StatusCode, resp.Header.Get("Content-Type"), nil
}

// DownloadViaQB 经 qBittorrent 直拉种子(qB 服务器直连源站拉 .torrent),再
// export 取回写到 outPath。流程:before-hash 防误取 + 轮询 + export。
func (c *Client) DownloadViaQB(ctx context.Context, item *RssItem, outPath string) (bool, error) {
	inst, err := c.qbInstance()
	if err != nil {
		return false, err
	}
	urls := c.torrentURLs(item)
	if len(urls) == 0 {
		return false, errors.New("没有可用的下载 URL")
	}
	u := urls[0]
	if err := c.checkURL(u); err != nil {
		return false, err
	}

	// before-hash:记录添加前 relay-pending 分类下已有的 hash,用于识别"新增"
	// 种子,防止误取仍在 pending 分类里的旧种子。这些 hash 同时登记进共享注册
	// 表,严格排除"其他 worker 的 before 快照"里的 hash,避免并发直拉串扰。
	before := map[string]bool{}
	if infos, err := inst.Info(ctx, ""); err == nil {
		for _, t := range infos {
			if t.Category == "relay-pending" {
				before[t.Hash] = true
			}
		}
	}
	registerPendingBefore(before)

	added, err := inst.AddTorrentURL(ctx, u, qb.AddOptions{
		Cookie:   c.Cookie,
		Category: "relay-pending",
		Paused:   true,
	})
	if err != nil {
		return false, err
	}

	// qB 5.x 返回新增的 torrent id 列表;4.x 返回 "Ok."(added 为 nil)。
	hash := ""
	if len(added) > 0 {
		hash = added[0]
	}

	// qB 接受 URL 但 metadata 尚未解析时,轮询 relay-pending 直到出现新 hash。
	// 严格过滤:仅接受 (a) 不在本任务 before 快照中、(b) 不在任何其他 worker
	// before 快照中 的 hash,避免并发直拉时误取别的任务的种子。
	if hash == "" {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			var cands []string
			if infos, err := inst.Info(ctx, ""); err == nil {
				for _, t := range infos {
					if t.Category == "relay-pending" && t.Hash != "" &&
						!before[t.Hash] && !isForeignPending(t.Hash) {
						cands = append(cands, t.Hash)
					}
				}
			}
			if len(cands) > 0 {
				hash = cands[len(cands)-1]
				break
			}
			if err := sleepCtx(ctx, 3*time.Second); err != nil {
				return false, err
			}
		}
	}
	if hash == "" {
		return false, fmt.Errorf("qB 添加种子后未找到新增 hash(add 响应: %v)", added)
	}

	// 轮询 export 直到 metadata 就绪(qB 尚未取到 .torrent 时 export 报错)。
	deadline := time.Now().Add(120 * time.Second)
	var data []byte
	for time.Now().Before(deadline) {
		if d, err := inst.ExportTorrent(ctx, hash); err == nil {
			data = d
			break
		}
		if err := sleepCtx(ctx, 3*time.Second); err != nil {
			_ = inst.Delete(ctx, hash, false)
			return false, err
		}
	}
	if len(data) == 0 {
		_ = inst.Delete(ctx, hash, false)
		return false, fmt.Errorf("qB 120s 内未取到 .torrent 元数据(hash=%s, url=%s)", hash, RedactURL(u))
	}

	if err := os.WriteFile(outPath, data, 0o600); err != nil {
		return false, err
	}
	_ = inst.Delete(ctx, hash, false)
	return true, nil
}

func (c *Client) qbInstance() (*qb.Instance, error) {
	c.qbMu.Lock()
	defer c.qbMu.Unlock()
	if c.qb != nil {
		return c.qb, nil
	}
	if c.qbHost == "" || c.qbUser == "" || c.qbPass == "" {
		return nil, errors.New("qB 中转缺少凭据:设置 QBHost/QBPort/QBUser/QBPass(或注入 qb.Instance)")
	}
	c.qb = qb.NewInstance(c.qbHost, c.qbPort, c.qbUser, c.qbPass)
	return c.qb, nil
}

// pendingBefore 是跨 Client 实例共享的 before-hash 注册表:每个 qB 直拉任务
// 在开始前把 relay-pending 里已存在的 hash 登记进来,轮询识别"新增"时一并排除,
// 从而严格避免并发直拉任务互相误取(串扰)。该表只增不减——一个曾存在于
// pending 分类的 hash 永不应被后续任务当作"新增"。
var (
	pendingBeforeMu sync.Mutex
	pendingBefore   = map[string]bool{}
)

func registerPendingBefore(hashes map[string]bool) {
	pendingBeforeMu.Lock()
	for h := range hashes {
		if h != "" {
			pendingBefore[strings.ToLower(h)] = true
		}
	}
	pendingBeforeMu.Unlock()
}

func isForeignPending(hash string) bool {
	pendingBeforeMu.Lock()
	defer pendingBeforeMu.Unlock()
	return pendingBefore[strings.ToLower(hash)]
}

// DetailFetcher 构造详情/文件列表抓取客户端(复用源站 base URL 和认证信息)。
func (c *Client) DetailFetcher() *DetailFetcher {
	return NewDetailFetcher(c.siteBase(), DetailFetcherOptions{
		Cookie:     c.Cookie,
		APIToken:   c.apiToken,
		HTTPClient: c.httpClient,
		URLChecker: c.checkURL,
	})
}

// APIToken 返回源站 API token(可能为空)。
func (c *Client) APIToken() string { return c.apiToken }

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
