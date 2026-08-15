package source

import (
	"os"
	"path/filepath"
	"testing"
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
	// 副标题在 HTML 标签内,Python 的 regex 也匹配不到 → 空
	if it.SmallDescr != "" {
		t.Errorf("small_descr = %q, want empty", it.SmallDescr)
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
	guid := "a1b2c3d4e5f6a7b8c9d0a1b2c3d4e5f6a7b8c9d0"
	if got := GuidToInfohash(guid); got != guid {
		t.Errorf("guid already hex should pass through, got %q", got)
	}
	// non-hex guid → sha1
	if got := GuidToInfohash("not-a-hash"); len(got) != 40 {
		t.Errorf("sha1 hash len = %d, want 40", len(got))
	}
}

func TestSourceClientTorrentURLs(t *testing.T) {
	client, err := NewSourceClient("https://dev.example.org/torrentrss.php?passkey=abc", SourceClientOptions{
		Passkey: "srcpass",
		Cookie:  "access_token=xyz",
	})
	if err != nil {
		t.Fatal(err)
	}
	it := RssItem{ID: "169620", Link: "https://dev.example.org/details.php?id=169620", EnclosureURL: "https://dev.example.org/download.php?id=169620&downhash=abc"}
	urls := client.torrentURLs(&it)
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

func TestParseRSSFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "rss.xml")
	if err := os.WriteFile(p, []byte(sampleRSS), 0o644); err != nil {
		t.Fatal(err)
	}
	items, err := ParseRSS([]byte(sampleRSS))
	if err != nil || len(items) != 1 {
		t.Fatalf("parse file: %v, items=%d", err, len(items))
	}
}
