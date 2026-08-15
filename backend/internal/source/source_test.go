package source

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const sampleRSS = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
<channel>
  <item>
    <title>Test.Movie.2026.1080p.WEB-DL.x264-StarfallWeb</title>
    <link>https://dev.example.org/details.php?id=169620</link>
    <description>&lt;b&gt;片名:&lt;/b&gt;Test Movie&lt;br /&gt;&lt;b&gt;副标题:&lt;/b&gt;测试副标题&lt;br /&gt;IMDb tt1234567</description>
    <category domain="cat=401">Movies</category>
    <enclosure url="https://dev.example.org/download.php?id=169620&amp;downhash=abc" length="123456789" type="application/x-bittorrent"/>
    <guid>a1b2c3d4e5f6a7b8c9d0a1b2c3d4e5f6a7b8c9d0</guid>
    <author>uploader</author>
    <pubDate>Wed, 01 Aug 2026 00:00:00 +0800</pubDate>
  </item>
</channel>
</rss>`

func TestParseRSS(t *testing.T) {
	items, err := ParseRSS([]byte(sampleRSS))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	it := items[0]
	if it.ID != "169620" {
		t.Errorf("id = %q", it.ID)
	}
	if it.Title != "Test.Movie.2026.1080p.WEB-DL.x264-StarfallWeb" {
		t.Errorf("title = %q", it.Title)
	}
	if it.CategoryID != "401" {
		t.Errorf("category_id = %q", it.CategoryID)
	}
	if it.CategoryName != "Movies" {
		t.Errorf("category_name = %q", it.CategoryName)
	}
	if it.Size == nil || *it.Size != 123456789 {
		t.Errorf("size = %v", it.Size)
	}
	if it.EnclosureURL != "https://dev.example.org/download.php?id=169620&downhash=abc" {
		t.Errorf("enclosure_url = %q", it.EnclosureURL)
	}
	if it.GUID != "a1b2c3d4e5f6a7b8c9d0a1b2c3d4e5f6a7b8c9d0" {
		t.Errorf("guid = %q", it.GUID)
	}
	if it.IMDB != "tt1234567" {
		t.Errorf("imdb = %q", it.IMDB)
	}
	// 副标题在 HTML 标签内,regex 也匹配不到 → 空
	if it.SmallDescr != "" {
		t.Errorf("small_descr = %q, want empty", it.SmallDescr)
	}
}

func TestParseRSSNamespace(t *testing.T) {
	const nsRSS = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:atom="http://www.w3.org/2005/Atom" xmlns:content="http://purl.org/rss/1.0/modules/content/">
<channel>
  <item>
    <title>NS.Movie.2026.1080p</title>
    <link>https://ns.example.org/details.php?id=42</link>
    <atom:link href="https://ns.example.org/details.php?id=42" rel="alternate" type="text/html"/>
    <content:encoded><![CDATA[<p>简介</p>]]></content:encoded>
    <description>副标题: 命名空间副标题 IMDb tt9999999</description>
    <category domain="cat=7">TV</category>
    <enclosure url="https://ns.example.org/download.php?id=42&amp;downhash=x" length="1000" type="application/x-bittorrent"/>
    <guid>bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb</guid>
    <pubDate>Tue, 02 Aug 2026 00:00:00 +0800</pubDate>
  </item>
</channel>
</rss>`
	items, err := ParseRSS([]byte(nsRSS))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	it := items[0]
	// atom:link 不得覆盖标准 <link>
	if it.Link != "https://ns.example.org/details.php?id=42" {
		t.Errorf("link = %q", it.Link)
	}
	if it.ID != "42" {
		t.Errorf("id = %q", it.ID)
	}
	if it.Title != "NS.Movie.2026.1080p" {
		t.Errorf("title = %q", it.Title)
	}
	if it.IMDB != "tt9999999" {
		t.Errorf("imdb = %q", it.IMDB)
	}
	if it.CategoryID != "7" {
		t.Errorf("category_id = %q", it.CategoryID)
	}
}

func TestMatchesKeywords(t *testing.T) {
	it := RssItem{Title: "Test.Movie.2026.StarfallWeb"}
	if !it.MatchesKeywords([]string{"starfallweb"}) {
		t.Errorf("should match starfallweb case-insensitively")
	}
	if it.MatchesKeywords([]string{"LongWeb"}) {
		t.Errorf("should not match LongWeb")
	}
}

func TestGuidToInfohash(t *testing.T) {
	const hex40 = "a1b2c3d4e5f6a7b8c9d0a1b2c3d4e5f6a7b8c9d0"
	if got := GuidToInfohash(hex40); got != hex40 {
		t.Errorf("40-hex passthrough = %q, want %q", got, hex40)
	}
	if got := GuidToInfohash(strings.ToUpper(hex40)); got != hex40 {
		t.Errorf("uppercase 40-hex = %q, want lowercase %q", got, hex40)
	}
	if got := GuidToInfohash("not-a-hash"); len(got) != 40 {
		t.Errorf("non-hex guid hash len = %d, want 40", len(got))
	}

	sum := sha1.Sum([]byte(""))
	sha1Empty := hex.EncodeToString(sum[:])
	// 空 guid + link/title 兜底 → 不等于 sha1("")
	got := GuidToInfohash("", "https://dev.example.org/details.php?id=1", "Some.Title")
	if got == sha1Empty {
		t.Errorf("empty guid with fallback hashed to sha1(\"\") = %q, want distinct", got)
	}
	if len(got) != 40 {
		t.Errorf("empty guid hash len = %d, want 40", len(got))
	}
	// 不同 fallback 不应碰撞
	got2 := GuidToInfohash("", "https://dev.example.org/details.php?id=2", "Other.Title")
	if got == got2 {
		t.Errorf("different fallback collided: %q", got)
	}
}

func TestSafeURL(t *testing.T) {
	blocked := []string{
		"http://127.0.0.1/",
		"http://127.0.0.1:8080/x",
		"http://169.254.169.254/latest/meta-data",
		"http://[::1]/",
		"http://10.0.0.1/",
		"http://172.16.0.1/",
		"http://192.168.1.1/",
		"http://0.0.0.0/",
		"file:///etc/passwd",
		"ftp://example.com/x",
	}
	for _, u := range blocked {
		if err := safeURL(u); err == nil {
			t.Errorf("safeURL(%q) = nil, want error", u)
		}
	}

	allowed := []string{
		"https://8.8.8.8/",
		"https://1.1.1.1/x",
		"http://93.184.216.34/", // example.com 公网 IP
	}
	for _, u := range allowed {
		if err := safeURL(u); err != nil {
			t.Errorf("safeURL(%q) = %v, want nil", u, err)
		}
	}
}

func TestIsUnsafeIP(t *testing.T) {
	unsafe := []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "0.0.0.0", "224.0.0.1",
		"100.64.0.1", "198.18.0.1", "::1", "fe80::1", "fc00::1",
		"::ffff:127.0.0.1",
	}
	for _, s := range unsafe {
		if !isUnsafeIP(net.ParseIP(s)) {
			t.Errorf("isUnsafeIP(%s) = false, want true", s)
		}
	}
	safe := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"}
	for _, s := range safe {
		if isUnsafeIP(net.ParseIP(s)) {
			t.Errorf("isUnsafeIP(%s) = true, want false", s)
		}
	}
}

func TestFetchRSSRejectsLoopback(t *testing.T) {
	if _, err := FetchRSS(context.Background(), "http://127.0.0.1/", nil); err == nil {
		t.Fatal("FetchRSS(127.0.0.1) = nil error, want SSRF rejection")
	}
}

func TestReadBodyLimitExceeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, 2048)) // 2KB > 1KB 限制
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if _, err := readBody(resp, 1024); err == nil {
		t.Fatal("readBody() = nil error, want limit-exceeded error")
	} else {
		var mbe *http.MaxBytesError
		if !errors.As(err, &mbe) {
			t.Fatalf("readBody() error = %T %v, want *http.MaxBytesError", err, err)
		}
	}
}

func TestDownloadBackoff403(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, ClientOptions{
		DownloadMode: "direct",
		Backoff:      func(attempt int) time.Duration { return time.Millisecond },
		URLChecker:   func(string) error { return nil },
		HTTPClient:   &http.Client{Timeout: 5 * time.Second},
	})
	it := &RssItem{ID: "1", Link: srv.URL + "/details.php?id=1", EnclosureURL: srv.URL + "/dl"}
	ok, err := c.DownloadTorrent(context.Background(), it, filepath.Join(t.TempDir(), "x.torrent"))
	if ok {
		t.Fatal("DownloadTorrent() ok = true, want false")
	}
	if err == nil {
		t.Fatal("DownloadTorrent() err = nil, want error after retries")
	}

	mu.Lock()
	got := hits
	mu.Unlock()
	if got < 2 {
		t.Errorf("expected backoff retries (>=2 attempts), got %d", got)
	}
	if got > maxDownloadAttempts {
		t.Errorf("too many attempts: %d > %d", got, maxDownloadAttempts)
	}
}

func TestDefaultBackoff(t *testing.T) {
	if got := DefaultBackoff(0); got != 60*time.Second {
		t.Errorf("DefaultBackoff(0) = %v, want 60s", got)
	}
	if got := DefaultBackoff(1); got != 120*time.Second {
		t.Errorf("DefaultBackoff(1) = %v, want 120s", got)
	}
	if got := DefaultBackoff(10); got != 900*time.Second {
		t.Errorf("DefaultBackoff(10) = %v, want capped 900s", got)
	}
}

func TestIsTorrentBody(t *testing.T) {
	if !isTorrentBody(200, []byte("d4:infodee"), "application/octet-stream") {
		t.Error("bencode dict should be torrent")
	}
	if !isTorrentBody(200, []byte("xxx"), "application/x-bittorrent") {
		t.Error("x-bittorrent content-type should be torrent")
	}
	if isTorrentBody(200, []byte("<html>"), "text/html") {
		t.Error("html should not be torrent")
	}
	if isTorrentBody(403, []byte("d4:infodee"), "application/octet-stream") {
		t.Error("non-200 should not be torrent")
	}
}

func TestClientTorrentURLs(t *testing.T) {
	c := NewClient("https://dev.example.org/torrentrss.php?passkey=abc", ClientOptions{
		Passkey: "srcpass",
		Cookie:  "access_token=xyz",
	})
	it := &RssItem{ID: "169620", Link: "https://dev.example.org/details.php?id=169620", EnclosureURL: "https://dev.example.org/download.php?id=169620&downhash=abc"}
	urls := c.torrentURLs(it)
	if len(urls) != 3 {
		t.Fatalf("urls = %d, want 3: %v", len(urls), urls)
	}
	if urls[0] != "https://dev.example.org/download.php?id=169620&passkey=srcpass" {
		t.Errorf("urls[0] = %q", urls[0])
	}
	if urls[1] != "https://dev.example.org/download.php?id=169620&downhash=abc" {
		t.Errorf("urls[1] = %q", urls[1])
	}
	if urls[2] != "https://dev.example.org/download.php?id=169620" {
		t.Errorf("urls[2] = %q", urls[2])
	}
}

func TestNormalizeTags(t *testing.T) {
	got := normalizeTags([]any{"国语", map[string]any{"name": "中字"}, "", map[string]any{"name": ""}})
	if len(got) != 2 || got[0] != "国语" || got[1] != "中字" {
		t.Errorf("normalizeTags = %v, want [国语 中字]", got)
	}
}

const sampleFileListHTML = `<html><body>
<tr><td class=rowfollow>Test.Movie.2026.mkv</td><td class=rowfollow align="right">6.70 GB</td></tr>
<tr><td class=rowfollow>Sample/README.txt</td><td class=rowfollow align="right">918.08 MB</td></tr>
</body></html>`

const sampleDetailsHTML = `<html><body>
<table>
<tr><td class="rowhead">副标题</td><td class="rowfollow">测试副标题 1080p</td></tr>
<tr><td class="rowhead">标签</td><td class="rowfollow"><span style="background:yellow">国语</span><span style="background:red">中字</span></td></tr>
<tr><td class="rowhead">IMDb</td><td class="rowfollow"><a href="https://www.imdb.com/title/tt6485574/">tt6485574</a></td></tr>
</table>
</body></html>`

func TestParseHumanSize(t *testing.T) {
	if got := ParseHumanSize("6.70 GB"); got == nil || *got != 7194070220 {
		t.Errorf("6.70 GB = %v, want 7194070220", got)
	}
	if got := ParseHumanSize("918.08 MB"); got == nil || *got != 962676654 {
		t.Errorf("918.08 MB = %v, want 962676654", got)
	}
	if ParseHumanSize("abc") != nil {
		t.Errorf("invalid size should be nil")
	}
}

func TestParseFileListHTML(t *testing.T) {
	files := ParseFileListHTML(sampleFileListHTML)
	if len(files) != 2 {
		t.Fatalf("files = %d, want 2", len(files))
	}
	f0 := files[0]
	if f0.Name != "Test.Movie.2026.mkv" || f0.Path != "Test.Movie.2026.mkv" {
		t.Errorf("f0 = %+v", f0)
	}
	if f0.Size == nil || *f0.Size != 7194070220 {
		t.Errorf("f0 size = %v, want 7194070220", f0.Size)
	}
	if f0.SizeHuman != "6.70 GB" {
		t.Errorf("f0 size_human = %q", f0.SizeHuman)
	}
}

func TestParseSmallDescr(t *testing.T) {
	if got := ParseSmallDescr(sampleDetailsHTML); got != "测试副标题 1080p" {
		t.Errorf("small_descr = %q", got)
	}
}

func TestParseTags(t *testing.T) {
	tags := ParseTags(sampleDetailsHTML)
	if len(tags) != 2 || tags[0] != "国语" || tags[1] != "中字" {
		t.Errorf("tags = %v", tags)
	}
}

func TestParseIMDB(t *testing.T) {
	if got := ParseIMDB(sampleDetailsHTML); got != "tt6485574" {
		t.Errorf("imdb = %q", got)
	}
}

func TestParseCookie(t *testing.T) {
	c := ParseCookie("access_token=abc; csrftoken=xyz")
	if c["access_token"] != "abc" || c["csrftoken"] != "xyz" {
		t.Errorf("cookie = %v", c)
	}
}
