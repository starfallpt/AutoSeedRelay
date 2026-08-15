package adapters

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/autoseedrelay/relay/internal/bencode"
	"github.com/autoseedrelay/relay/internal/parser"
)

// sampleParsedTorrent builds a ParsedTorrent (with RawDict) from bencoded
// bytes so Publish can re-encode it into the multipart "file" field.
func sampleParsedTorrent(t *testing.T, name string) *parser.ParsedTorrent {
	t.Helper()
	info := map[string]any{
		"name":         name,
		"piece length": int64(16384),
		"pieces":       string(make([]byte, 20)),
		"length":       int64(0),
	}
	d := map[string]any{
		"announce": "https://src.example/announce.php?passkey=srcpass",
		"info":     info,
	}
	raw, err := bencode.Encode(d)
	if err != nil {
		t.Fatalf("encode sample torrent: %v", err)
	}
	p, err := parser.ParseTorrent(raw)
	if err != nil {
		t.Fatalf("parse sample torrent: %v", err)
	}
	return p
}

// uploadCapture records what an httptest upload handler received.
type uploadCapture struct {
	auth   string
	cookie string
	xapi   string
	values map[string][]string
	file   int // count of "file" multipart parts
}

func val(m map[string][]string, k string) string {
	if vs := m[k]; len(vs) > 0 {
		return vs[0]
	}
	return ""
}

func TestNexusPHPAPIPublish(t *testing.T) {
	var got uploadCapture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/upload" {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseMultipartForm(10 << 20)
		got = uploadCapture{
			auth:   r.Header.Get("Authorization"),
			cookie: r.Header.Get("Cookie"),
			xapi:   r.Header.Get("x-api-key"),
			values: r.MultipartForm.Value,
			file:   len(r.MultipartForm.File["file"]),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":123}}`))
	}))
	defer srv.Close()

	cfg := SiteConfig{
		Name:              "luckpt",
		Type:              TypeNexusPHPAPI,
		BaseURL:           srv.URL,
		APIToken:          "test-token",
		CategoryOverrides: map[string]int{"movie": 401},
		DimensionOverrides: map[string]map[string]int{
			"standard": {"2160": 4},
			"codec":    {"HEVC": 6},
		},
		TagsMap: map[string]string{"国语": "5", "中字": "6"},
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tor := sampleParsedTorrent(t, "Example.Movie.2026.2160p.HEVC")

	res, err := a.Publish(context.Background(), tor, PublishParams{
		Title:       "Example Movie 2026 2160p HEVC",
		Description: "<b>desc</b>",
		Category:    "movie",
		Tags:        []string{"国语", "中字"},
		IMDb:        "tt1234567",
		Dimensions:  map[string]string{"standard": "2160", "codec": "HEVC"},
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !res.OK || res.TargetID != 123 {
		t.Fatalf("result = %+v, want OK with id 123", res)
	}
	if got.auth != "Bearer test-token" {
		t.Errorf("Authorization = %q", got.auth)
	}
	if got.values["name"][0] != "Example Movie 2026 2160p HEVC" {
		t.Errorf("name = %v", got.values["name"])
	}
	if got.values["type"][0] != "401" {
		t.Errorf("type = %v, want 401", got.values["type"])
	}
	if got.values["url"][0] != "1234567" {
		t.Errorf("url = %v, want 1234567 (tt stripped)", got.values["url"])
	}
	if got.values["standard"][0] != "4" || got.values["codec"][0] != "6" {
		t.Errorf("dims standard/codec = %v / %v, want 4 / 6", got.values["standard"], got.values["codec"])
	}
	tags := got.values["tags[]"]
	if len(tags) != 2 || tags[0] != "5" || tags[1] != "6" {
		t.Errorf("tags[] = %v, want [5 6]", tags)
	}
	if _, ok := got.values["file"]; ok {
		t.Errorf("file should be in the multipart file part, not a value")
	}
	if got.file != 1 {
		t.Errorf("file part count = %d, want 1", got.file)
	}
}

func TestNexusPHPAPIDuplicate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"torrent_existed"}`))
	}))
	defer srv.Close()

	a, _ := New(SiteConfig{Name: "x", Type: TypeNexusPHPAPI, BaseURL: srv.URL, CategoryOverrides: map[string]int{"m": 1}})
	tor := sampleParsedTorrent(t, "x")
	_, err := a.Publish(context.Background(), tor, PublishParams{Title: "t", Description: "d", Category: "m"})
	if !IsDuplicate(err) {
		t.Fatalf("err = %v, want ErrDuplicate", err)
	}
}

func TestNexusPHPAPIAuthExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Unauthenticated."}`))
	}))
	defer srv.Close()

	a, _ := New(SiteConfig{Name: "x", Type: TypeNexusPHPAPI, BaseURL: srv.URL, APIToken: "bad", CategoryOverrides: map[string]int{"m": 1}})
	tor := sampleParsedTorrent(t, "x")
	_, err := a.Publish(context.Background(), tor, PublishParams{Title: "t", Description: "d", Category: "m"})
	if !IsAuthExpired(err) {
		t.Fatalf("err = %v, want ErrAuthExpired", err)
	}
}

func TestNexusPHPAPITestMode(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	a, _ := New(SiteConfig{Name: "x", Type: TypeNexusPHPAPI, BaseURL: srv.URL, TestMode: true, CategoryOverrides: map[string]int{"m": 1}})
	tor := sampleParsedTorrent(t, "x")
	res, err := a.Publish(context.Background(), tor, PublishParams{Title: "t", Description: "d", Category: "m"})
	if !IsTestMode(err) {
		t.Fatalf("err = %v, want ErrTestMode", err)
	}
	if !res.TestMode {
		t.Errorf("result.TestMode = false, want true")
	}
	if hits.Load() != 0 {
		t.Errorf("test mode made %d network requests, want 0", hits.Load())
	}
}

func TestClassicPublish(t *testing.T) {
	var got uploadCapture
	mux := http.NewServeMux()
	mux.HandleFunc("/upload.php", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><form method="post" action="takeupload.php" enctype="multipart/form-data">
<input type="hidden" name="auth_token" value="csrf123"/>
<select name="type"><option value="401">电影</option><option value="402">剧集</option></select>
<input type="text" name="name"/>
<textarea name="descr"></textarea>
</form></body></html>`))
	})
	mux.HandleFunc("/takeupload.php", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(10 << 20)
		got = uploadCapture{
			auth:   r.Header.Get("Authorization"),
			cookie: r.Header.Get("Cookie"),
			values: r.MultipartForm.Value,
			file:   len(r.MultipartForm.File["file"]),
		}
		w.Header().Set("Location", "details.php?id=42")
		w.WriteHeader(http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := SiteConfig{
		Name:              "classic",
		Type:              TypeNexusPHPClassic,
		BaseURL:           srv.URL,
		Cookie:            "access_token=secret",
		CategoryOverrides: map[string]int{"movie": 401},
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tor := sampleParsedTorrent(t, "Example.Movie.2026.1080p.H.264")
	res, err := a.Publish(context.Background(), tor, PublishParams{
		Title:       "Example.Movie.2026.1080p.H.264",
		Description: "body",
		Category:    "movie",
		Tags:        []string{"国语", "中字"},
		Dimensions:  map[string]string{"standard": "1080p", "video_codec": "H.264"},
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !res.OK || res.TargetID != 42 {
		t.Fatalf("result = %+v, want OK id 42", res)
	}
	if !strings.Contains(got.cookie, "access_token=secret") {
		t.Errorf("Cookie = %q, want access_token=secret", got.cookie)
	}
	if got.values["auth_token"][0] != "csrf123" {
		t.Errorf("hidden field auth_token = %v, want csrf123 (echoed from upload.php)", got.values["auth_token"])
	}
	if got.values["type"][0] != "401" {
		t.Errorf("type = %v, want 401", got.values["type"])
	}
	if got.file != 1 {
		t.Errorf("file part count = %d, want 1", got.file)
	}
	descr := val(got.values, "descr")
	if !strings.Contains(descr, "[标签:国语,中字]") {
		t.Errorf("descr missing tags: %q", descr)
	}
	if !strings.Contains(descr, "[参数:1080p,H.264]") {
		t.Errorf("descr missing params: %q", descr)
	}
}

func TestClassicDuplicate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/upload.php", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<form action="takeupload.php"><input type="hidden" name="a" value="b"/></form>`))
	})
	mux.HandleFunc("/takeupload.php", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`种子已存在`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a, _ := New(SiteConfig{Name: "c", Type: TypeNexusPHPClassic, BaseURL: srv.URL, Cookie: "c=1", CategoryOverrides: map[string]int{"m": 1}})
	tor := sampleParsedTorrent(t, "x")
	_, err := a.Publish(context.Background(), tor, PublishParams{Title: "t", Description: "d", Category: "m"})
	if !IsDuplicate(err) {
		t.Fatalf("err = %v, want ErrDuplicate", err)
	}
}

func TestClassicAuthExpired(t *testing.T) {
	// upload.php redirects to login.php -> auth expired before any POST.
	mux := http.NewServeMux()
	mux.HandleFunc("/upload.php", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/login.php")
		w.WriteHeader(http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a, _ := New(SiteConfig{Name: "c", Type: TypeNexusPHPClassic, BaseURL: srv.URL, Cookie: "c=1", CategoryOverrides: map[string]int{"m": 1}})
	tor := sampleParsedTorrent(t, "x")
	_, err := a.Publish(context.Background(), tor, PublishParams{Title: "t", Description: "d", Category: "m"})
	if !IsAuthExpired(err) {
		t.Fatalf("err = %v, want ErrAuthExpired", err)
	}
}

func TestMTeamPublish(t *testing.T) {
	var got uploadCapture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/torrent/createOredit" {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseMultipartForm(10 << 20)
		got = uploadCapture{
			auth:   r.Header.Get("Authorization"),
			cookie: r.Header.Get("Cookie"),
			xapi:   r.Header.Get("x-api-key"),
			values: r.MultipartForm.Value,
			file:   len(r.MultipartForm.File["file"]),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"id":7}}`))
	}))
	defer srv.Close()

	cfg := SiteConfig{
		Name:              "mteam",
		Type:              TypeMTeam,
		BaseURL:           srv.URL,
		APIToken:          "mteam-key",
		CategoryOverrides: map[string]int{"movie": 100},
		DimensionOverrides: map[string]map[string]int{
			"standard": {"2160": 6},
			"codec":    {"HEVC": 16},
		},
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tor := sampleParsedTorrent(t, "Example.Movie.2026.2160p.HEVC")
	res, err := a.Publish(context.Background(), tor, PublishParams{
		Title:       "Example Movie 2026 2160p HEVC",
		SubTitle:    "副标题",
		Description: "desc",
		Category:    "movie",
		IMDb:        "tt1234567",
		Dimensions:  map[string]string{"standard": "2160", "codec": "HEVC"},
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !res.OK || res.TargetID != 7 {
		t.Fatalf("result = %+v, want OK id 7", res)
	}
	if got.xapi != "mteam-key" {
		t.Errorf("x-api-key = %q", got.xapi)
	}
	if got.values["smallDescr"][0] != "副标题" {
		t.Errorf("smallDescr = %v", got.values["smallDescr"])
	}
	if got.values["imdb"][0] != "tt1234567" {
		t.Errorf("imdb = %v, want tt1234567 (tt kept)", got.values["imdb"])
	}
	if got.values["category"][0] != "100" {
		t.Errorf("category = %v, want 100", got.values["category"])
	}
	if got.values["standard"][0] != "6" || got.values["videoCodec"][0] != "16" {
		t.Errorf("dims = %v / %v, want 6 / 16", got.values["standard"], got.values["videoCodec"])
	}
	if got.values["anonymous"][0] != "false" {
		t.Errorf("anonymous = %v, want false", got.values["anonymous"])
	}
	if got.file != 1 {
		t.Errorf("file part count = %d, want 1", got.file)
	}
}

func TestMTeamDuplicateAndAuth(t *testing.T) {
	t.Run("duplicate", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"code":1,"message":"種子已存在"}`))
		}))
		defer srv.Close()
		a, _ := New(SiteConfig{Name: "m", Type: TypeMTeam, BaseURL: srv.URL, CategoryOverrides: map[string]int{"m": 1}})
		_, err := a.Publish(context.Background(), sampleParsedTorrent(t, "x"), PublishParams{Title: "t", Description: "d", Category: "m"})
		if !IsDuplicate(err) {
			t.Fatalf("err = %v, want ErrDuplicate", err)
		}
	})
	t.Run("auth", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Unauthorized"}`))
		}))
		defer srv.Close()
		a, _ := New(SiteConfig{Name: "m", Type: TypeMTeam, BaseURL: srv.URL, APIToken: "bad", CategoryOverrides: map[string]int{"m": 1}})
		_, err := a.Publish(context.Background(), sampleParsedTorrent(t, "x"), PublishParams{Title: "t", Description: "d", Category: "m"})
		if !IsAuthExpired(err) {
			t.Fatalf("err = %v, want ErrAuthExpired", err)
		}
	})
}

func TestProbeDetection(t *testing.T) {
	t.Run("nexusphp", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/sections" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"categories":[{"id":401,"name":"电影"},{"id":402,"name":"剧集"}],
				"tags":[{"id":5,"name":"国语"}],
				"codec_list":[{"id":6,"name":"H.265/HEVC"}]
			}`))
		}))
		defer srv.Close()
		res, err := Probe(context.Background(), srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.Type != TypeNexusPHPAPI {
			t.Errorf("type = %q", res.Type)
		}
		if res.Categories["电影"] != 401 || res.Categories["剧集"] != 402 {
			t.Errorf("categories = %v", res.Categories)
		}
		if len(res.Tags) != 1 || res.Tags[0].Name != "国语" {
			t.Errorf("tags = %v", res.Tags)
		}
		if opts := res.Codecs["codec"]; len(opts) != 1 || opts[0].Name != "H.265/HEVC" {
			t.Errorf("codecs = %v", res.Codecs)
		}
	})

	t.Run("classic", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/upload.php" {
				_, _ = w.Write([]byte(`<form method="post" action="takeupload.php" enctype="multipart/form-data">
<select name="type"><option value="401">电影</option><option value="402">剧集</option></select>
<input type="checkbox" name="tags[4][]" value="5" id="t5"><label for="t5">国语</label>
</form>`))
				return
			}
			http.NotFound(w, r)
		}))
		defer srv.Close()
		res, err := Probe(context.Background(), srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.Type != TypeNexusPHPClassic {
			t.Errorf("type = %q", res.Type)
		}
		if res.Categories["电影"] != 401 || res.Categories["剧集"] != 402 {
			t.Errorf("categories = %v", res.Categories)
		}
		if len(res.Tags) != 1 || res.Tags[0].ID != 5 || res.Tags[0].Name != "国语" {
			t.Errorf("tags = %v", res.Tags)
		}
	})

	t.Run("mteam", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/torrent/categoryList" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"code":0,"data":[
					{"id":100,"nameChs":"电影","nameCht":"電影","nameEng":"Movie"},
					{"id":105,"nameChs":"剧集"}
				]}`))
				return
			}
			if r.URL.Path == "/torrent/teamList" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"code":0,"data":[{"id":59,"name":"StarfallWeb"}]}`))
				return
			}
			http.NotFound(w, r)
		}))
		defer srv.Close()
		res, err := Probe(context.Background(), srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.Type != TypeMTeam {
			t.Errorf("type = %q", res.Type)
		}
		if res.Categories["电影"] != 100 || res.Categories["Movie"] != 100 {
			t.Errorf("categories = %v", res.Categories)
		}
	})
}

func TestRegistry(t *testing.T) {
	if len(Types()) != 3 {
		t.Errorf("Types() = %v, want 3", Types())
	}
	if _, err := New(SiteConfig{Type: "bogus"}); err == nil {
		t.Errorf("New(bogus) = nil error, want error")
	}
	a, err := New(SiteConfig{Name: "d", BaseURL: "https://x.example"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Type() != TypeNexusPHPAPI {
		t.Errorf("empty type defaults to %q, got %q", TypeNexusPHPAPI, a.Type())
	}
}

func TestBuildAnnounce(t *testing.T) {
	got := BuildAnnounce(SiteConfig{
		Type:     TypeNexusPHPAPI,
		BaseURL:  "https://x.example",
		Announce: "https://x.example/announce.php?passkey={passkey}",
		Passkey:  "abc",
	})
	if got != "https://x.example/announce.php?passkey=abc" {
		t.Errorf("announce = %q", got)
	}

	got = BuildAnnounce(SiteConfig{
		Type:     TypeMTeam,
		Announce: "https://tracker.example/announce?credential={credential}",
	})
	if !strings.Contains(got, "credential=PLACEHOLDER") {
		t.Errorf("mteam announce = %q", got)
	}
}

// TestCategoryMismatch verifies the ErrCategoryMismatch classification when a
// name cannot be resolved and no fallback is configured.
func TestCategoryMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	a, _ := New(SiteConfig{Name: "x", Type: TypeNexusPHPAPI, BaseURL: srv.URL})
	_, err := a.Publish(context.Background(), sampleParsedTorrent(t, "x"), PublishParams{Title: "t", Description: "d", Category: "unknown"})
	if !IsCategoryMismatch(err) {
		t.Fatalf("err = %v, want ErrCategoryMismatch", err)
	}
}
