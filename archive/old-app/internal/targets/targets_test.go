package targets

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/autoseedrelay/go-relay/internal/bencode"
	"github.com/autoseedrelay/go-relay/internal/parser"
)

func makeSampleTorrent(t *testing.T, dir, name string) string {
	t.Helper()
	info := map[string]any{
		"name":         name,
		"piece length": 256 * 1024,
		"pieces":       string(make([]byte, 20)),
		"length":       0,
		"private":      0,
	}
	d := map[string]any{
		"announce":      "https://src.example/announce.php?passkey=srcpass",
		"creation date": int64(time.Now().Unix()) - 3600,
		"info":          info,
	}
	path := filepath.Join(dir, "sample.torrent")
	if err := bencode.WriteTorrent(path, d); err != nil {
		t.Fatalf("write sample torrent: %v", err)
	}
	return path
}

func TestNexusPHPBuildFields(t *testing.T) {
	dir := t.TempDir()
	tp := makeSampleTorrent(t, dir, "Example.Movie.2026.1080p.WEB-DL.x264")
	parsed, err := parser.ParseTorrent(tp)
	if err != nil {
		t.Fatal(err)
	}
	site, err := NewTargetSite("nexusphp", map[string]any{
		"base_url":      "https://target.example",
		"api_token":     "<sanctum-token>",
		"announce_base": "https://target.example/announce.php?passkey={passkey}",
		"passkey":       "<passkey>",
		"site_name":     "TargetSite",
	})
	if err != nil {
		t.Fatal(err)
	}
	site.SetCategories(map[string]int{"Movies": 401, "TV Series": 402, "Documentaries": 403, "Anime": 404})
	fields := BuildUploadFields(parsed, site, map[string]any{"category": "Movies"})

	if fields["name"] != "Example Movie 2026 1080p WEB-DL x264" {
		t.Errorf("name = %v", fields["name"])
	}
	if fields["descr"] != "" {
		t.Errorf("descr = %v, want empty", fields["descr"])
	}
	if fields["type"] != 401 {
		t.Errorf("type = %v, want 401", fields["type"])
	}
	if fields["source"] != 4 {
		t.Errorf("source = %v, want 4", fields["source"])
	}
	if fields["medium"] != 4 {
		t.Errorf("medium = %v, want 4", fields["medium"])
	}
	if fields["standard"] != 3 {
		t.Errorf("standard = %v, want 3", fields["standard"])
	}
	if fields["codec"] != 1 {
		t.Errorf("codec = %v, want 1", fields["codec"])
	}

	// BuildAnnounce
	np := site.(*NexusPHPAPI)
	if got := np.BuildAnnounce(); got != "https://target.example/announce.php?passkey=<passkey>" {
		t.Errorf("announce = %q", got)
	}
	if got := np.UploadURL(); got != "https://target.example/api/v1/upload" {
		t.Errorf("upload_url = %q", got)
	}
}

func TestMTeamBuildFields(t *testing.T) {
	dir := t.TempDir()
	tp := makeSampleTorrent(t, dir, "Example.Movie.2026.1080p.WEB-DL.x264")
	parsed, err := parser.ParseTorrent(tp)
	if err != nil {
		t.Fatal(err)
	}
	site, err := NewTargetSite("mteam", map[string]any{
		"base_url":      "https://api.m-team.io/api",
		"auth_token":    "<mteam-token>",
		"announce_base": "https://tracker.m-team.cc/announce?credential={credential}",
		"site_name":     "M-Team",
	})
	if err != nil {
		t.Fatal(err)
	}
	site.SetCategories(map[string]int{"电影": 100, "剧集": 105, "纪录片": 404, "动漫": 405, "音乐": 110, "其他": 409})
	fields := BuildUploadFields(parsed, site, map[string]any{"category": "电影"})

	if fields["name"] != "Example Movie 2026 1080p WEB-DL.x264" {
		t.Errorf("name = %v", fields["name"])
	}
	if fields["category"] != 100 {
		t.Errorf("category = %v, want 100", fields["category"])
	}
	if fields["anonymous"] != false {
		t.Errorf("anonymous = %v, want false", fields["anonymous"])
	}
	if fields["descr"] != "" {
		t.Errorf("descr = %v, want empty", fields["descr"])
	}
	// M-Team 维度 ID
	if fields["standard"] != 1 {
		t.Errorf("standard = %v, want 1", fields["standard"])
	}
	if fields["video_codec"] != 1 {
		t.Errorf("video_codec = %v, want 1", fields["video_codec"])
	}

	mt := site.(*MTeamAPI)
	if got := mt.UploadURL(); got != "https://api.m-team.io/api/torrent/createOredit" {
		t.Errorf("upload_url = %q", got)
	}
	if got := mt.BuildAnnounce(); !strings.Contains(got, "tracker.m-team.cc/announce?credential=") {
		t.Errorf("announce = %q", got)
	}
}

func TestClassicBuildFields(t *testing.T) {
	dir := t.TempDir()
	tp := makeSampleTorrent(t, dir, "Example.Movie.2026.1080p.WEB-DL.x264")
	parsed, err := parser.ParseTorrent(tp)
	if err != nil {
		t.Fatal(err)
	}
	site, err := NewTargetSite("nexusphp_classic", map[string]any{
		"base_url":      "https://tjupt.org",
		"cookie":        "<cookie>",
		"site_name":     "北洋园PT",
		"announce_base": "https://tracker-public.tjupt.org/announce.php?passkey={passkey}",
		"passkey":       "<passkey>",
		"categories":    map[string]any{"电影": 401, "动漫": 405, "剧集": 402},
	})
	if err != nil {
		t.Fatal(err)
	}
	fields := BuildUploadFields(parsed, site, map[string]any{"category": "电影", "tags": []string{"国语", "中字"}})

	if fields["name"] != "Example.Movie.2026.1080p.WEB-DL.x264" {
		t.Errorf("name = %v", fields["name"])
	}
	if fields["type"] != 401 {
		t.Errorf("type = %v, want 401", fields["type"])
	}
	descr := fields["descr"].(string)
	if !strings.Contains(descr, "[标签:国语,中字]") {
		t.Errorf("descr missing tags: %q", descr)
	}
	if !strings.Contains(descr, "[参数:H.264]") {
		t.Errorf("descr missing params: %q", descr)
	}
}

func TestParseCategoriesMapping(t *testing.T) {
	rows := map[string]any{
		"categories": []any{
			map[string]any{"id": "401", "name": "Movies"},
			map[string]any{"id": 402, "name": "TV Series"},
		},
	}
	m := ParseCategoriesMapping(rows)
	if m["Movies"] != 401 || m["TV Series"] != 402 {
		t.Errorf("parse mapping = %v", m)
	}
}

func TestResolveCategoryID(t *testing.T) {
	siteCats := map[string]int{"Movies": 401}
	defaults := map[string]int{"movie": 100}
	// 纯数字
	if id := ResolveCategoryID("401", nil, nil, nil); id == nil || *id != 401 {
		t.Errorf("numeric cat: %v", id)
	}
	// 站点 API 分类名
	if id := ResolveCategoryID("Movies", siteCats, nil, nil); id == nil || *id != 401 {
		t.Errorf("site cat: %v", id)
	}
	// 默认映射别名
	if id := ResolveCategoryID("电影", nil, defaults, nil); id == nil || *id != 100 {
		t.Errorf("alias cat: %v", id)
	}
	// fallback
	fallback := 999
	if id := ResolveCategoryID("", nil, nil, &fallback); id == nil || *id != 999 {
		t.Errorf("fallback cat: %v", id)
	}
}

func TestExtractIMDB(t *testing.T) {
	if got := ExtractIMDB("https://www.imdb.com/title/tt6485574/"); got != "tt6485574" {
		t.Errorf("imdb = %q", got)
	}
	if got := ExtractIMDB("", nil, "abc"); got != "" {
		t.Errorf("imdb = %q, want empty", got)
	}
}
