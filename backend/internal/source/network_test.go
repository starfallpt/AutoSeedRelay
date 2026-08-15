package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// APIFetcher (Sanctum API) network-layer tests
// ---------------------------------------------------------------------------

// newTestAPIFetcher builds an APIFetcher pointed at an httptest server, with the
// SSRF pre-check relaxed so the loopback test server is reachable (the real
// safeURL rejects 127.0.0.1).
func newTestAPIFetcher(t *testing.T, token string, handler http.Handler) *APIFetcher {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	a := NewAPIFetcher(srv.URL, token, 30)
	a.checkURL = func(string) error { return nil }
	return a
}

const apiDetailBody = `{
  "data": {
    "id": 169620,
    "title": "Test.Movie.2026",
    "small_descr": "测试副标题",
    "tags": ["国语", {"name": "中字"}],
    "descr": "<p>desc</p>",
    "category": {"name": "Movies", "id": 401},
    "size": 123456789,
    "info_hash": "abc",
    "imdb": "tt1234567",
    "douban_id": "db1",
    "team": "StarfallWeb",
    "medium": "BluRay",
    "codec": "H264",
    "audiocodec": "DDP",
    "resolution": "1080p",
    "mediainfo": "mediainfo text"
  }
}`

func TestAPIFetcherFetchDetailAPI(t *testing.T) {
	a := newTestAPIFetcher(t, "tok", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/torrent/169620" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q, want Bearer tok", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(apiDetailBody))
	}))

	got, err := a.FetchDetailAPI(context.Background(), 169620)
	if err != nil {
		t.Fatalf("FetchDetailAPI: %v", err)
	}
	if got.ID != 169620 || got.Title != "Test.Movie.2026" || got.SmallDescr != "测试副标题" {
		t.Fatalf("basic fields mismatch: %+v", got)
	}
	// 标签 string/map 兼容:两种格式都要被归一化成 []string。
	if len(got.Tags) != 2 || got.Tags[0] != "国语" || got.Tags[1] != "中字" {
		t.Fatalf("tags = %v, want [国语 中字]", got.Tags)
	}
	if got.MediaInfo != "mediainfo text" || got.DescrHTML != "<p>desc</p>" {
		t.Fatalf("mediainfo/descr mismatch: %+v", got)
	}
	if got.CategoryName != "Movies" || got.CategoryID != 401 {
		t.Fatalf("category mismatch: %+v", got)
	}
	if got.Size != 123456789 || got.InfoHash != "abc" || got.IMDb != "tt1234567" ||
		got.DoubanID != "db1" || got.Medium != "BluRay" || got.Codec != "H264" ||
		got.AudioCodec != "DDP" || got.Resolution != "1080p" || got.Team != "StarfallWeb" {
		t.Fatalf("detail fields mismatch: %+v", got)
	}
}

func TestAPIFetcherFetchDetailAPIErrorResponse(t *testing.T) {
	a := newTestAPIFetcher(t, "tok", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	if _, err := a.FetchDetailAPI(context.Background(), 1); err == nil {
		t.Fatal("FetchDetailAPI = nil error, want HTTP 500 error")
	} else if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error should mention status 500, got %v", err)
	}
}

func TestAPIFetcherFetchDetailAPIInvalidJSON(t *testing.T) {
	a := newTestAPIFetcher(t, "tok", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	if _, err := a.FetchDetailAPI(context.Background(), 1); err == nil {
		t.Fatal("FetchDetailAPI = nil error, want JSON parse error")
	}
}

func TestAPIFetcherFetchDetailAPINoToken(t *testing.T) {
	a := NewAPIFetcher("http://unused", "", 30)
	if _, err := a.FetchDetailAPI(context.Background(), 1); err == nil {
		t.Fatal("FetchDetailAPI with empty token = nil error, want rejection")
	}
}

func TestAPIFetcherFetchFileListAPI(t *testing.T) {
	a := newTestAPIFetcher(t, "tok", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/torrent/42/files" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"name":"a.mkv","size":111},{"name":"b.nfo","size":222}]}`))
	}))

	files, err := a.FetchFileListAPI(context.Background(), 42)
	if err != nil {
		t.Fatalf("FetchFileListAPI: %v", err)
	}
	if len(files) != 2 || files[0].Name != "a.mkv" || files[0].Size != 111 ||
		files[1].Name != "b.nfo" || files[1].Size != 222 {
		t.Fatalf("files = %+v", files)
	}
}

func TestAPIFetcherFetchFileListAPIErrorResponse(t *testing.T) {
	a := newTestAPIFetcher(t, "tok", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	if _, err := a.FetchFileListAPI(context.Background(), 1); err == nil {
		t.Fatal("FetchFileListAPI = nil error, want HTTP error")
	}
}

func TestAPIFetcherFetchAllAPI(t *testing.T) {
	a := newTestAPIFetcher(t, "tok", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/torrent/7":
			_, _ = w.Write([]byte(`{"data":{"id":7,"title":"All","tags":["x"]}}`))
		case "/api/v1/torrent/7/files":
			_, _ = w.Write([]byte(`{"data":[{"name":"f.mkv","size":5}]}`))
		default:
			http.NotFound(w, r)
		}
	}))

	got, err := a.FetchAllAPI(context.Background(), 7)
	if err != nil {
		t.Fatalf("FetchAllAPI: %v", err)
	}
	if got.ID != 7 || got.Title != "All" {
		t.Fatalf("detail mismatch: %+v", got)
	}
	if len(got.Files) != 1 || got.Files[0].Name != "f.mkv" {
		t.Fatalf("files not merged: %+v", got.Files)
	}
}

// ---------------------------------------------------------------------------
// DetailFetcher (HTML) network-layer tests
// ---------------------------------------------------------------------------

func newTestDetailFetcher(t *testing.T, handler http.Handler) *DetailFetcher {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewDetailFetcher(srv.URL, DetailFetcherOptions{
		URLChecker: func(string) error { return nil },
		HTTPClient: &http.Client{},
	})
}

const detailHTMLBody = `<html><body>
<table>
<tr><td class="rowhead">副标题</td><td class="rowfollow">测试副标题 1080p</td></tr>
<tr><td class="rowhead">标签</td><td class="rowfollow"><span>国语</span><span>中字</span></td></tr>
<tr><td class="rowhead">IMDb</td><td class="rowfollow"><a href="https://www.imdb.com/title/tt6485574/">tt6485574</a></td></tr>
<tr><td class="rowhead">MediaInfo</td><td class="rowfollow">Video: H264 1080p</td></tr>
<tr><td class="rowhead">促销</td><td class="rowfollow">免费</td></tr>
</table>
</body></html>`

const fileListHTMLBody = `<html><body>
<tr><td class=rowfollow>Test.Movie.2026.mkv</td><td class=rowfollow align="right">6.70 GB</td></tr>
<tr><td class=rowfollow>Sample/README.txt</td><td class=rowfollow align="right">918.08 MB</td></tr>
</body></html>`

func TestDetailFetcherFetchDetailPage(t *testing.T) {
	f := newTestDetailFetcher(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/details.php" || r.URL.Query().Get("id") != "169620" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(detailHTMLBody))
	}))

	body, err := f.FetchDetailPage(context.Background(), 169620)
	if err != nil {
		t.Fatalf("FetchDetailPage: %v", err)
	}
	if !strings.Contains(body, "tt6485574") {
		t.Fatalf("body missing IMDb id: %q", body)
	}
}

func TestDetailFetcherFetchDetailPageRedirect(t *testing.T) {
	f := newTestDetailFetcher(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login.php", http.StatusFound)
	}))
	if _, err := f.FetchDetailPage(context.Background(), 1); err == nil {
		t.Fatal("FetchDetailPage = nil error, want redirect rejection")
	} else if !strings.Contains(err.Error(), "重定向") {
		t.Fatalf("error should mention 重定向, got %v", err)
	}
}

func TestDetailFetcherFetchDetailPageHTTPError(t *testing.T) {
	f := newTestDetailFetcher(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusNotFound)
	}))
	if _, err := f.FetchDetailPage(context.Background(), 1); err == nil {
		t.Fatal("FetchDetailPage = nil error, want HTTP 404 error")
	}
}

func TestDetailFetcherFetchFileList(t *testing.T) {
	f := newTestDetailFetcher(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(fileListHTMLBody))
	}))

	files, err := f.FetchFileList(context.Background(), 169620)
	if err != nil {
		t.Fatalf("FetchFileList: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %d, want 2", len(files))
	}
	if files[0].Name != "Test.Movie.2026.mkv" || files[0].Size == nil || *files[0].Size != 7194070220 {
		t.Fatalf("files[0] = %+v", files[0])
	}
	if files[1].Name != "Sample/README.txt" || files[1].Size == nil || *files[1].Size != 962676654 {
		t.Fatalf("files[1] = %+v", files[1])
	}
}

func TestDetailFetcherFetchFileListEmpty(t *testing.T) {
	f := newTestDetailFetcher(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(nil) // empty body
	}))
	if _, err := f.FetchFileList(context.Background(), 1); err == nil {
		t.Fatal("FetchFileList = nil error, want empty-body error")
	} else if !strings.Contains(err.Error(), "空 body") {
		t.Fatalf("error should mention empty body, got %v", err)
	}
}

func TestDetailFetcherFetchAllDetail(t *testing.T) {
	f := newTestDetailFetcher(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/viewfilelist.php":
			_, _ = w.Write([]byte(fileListHTMLBody))
		case "/details.php":
			_, _ = w.Write([]byte(detailHTMLBody))
		default:
			http.NotFound(w, r)
		}
	}))

	got, err := f.FetchAllDetail(context.Background(), 169620)
	if err != nil {
		t.Fatalf("FetchAllDetail: %v", err)
	}
	if got.ID != 169620 || got.SmallDescr != "测试副标题 1080p" {
		t.Fatalf("small_descr/id mismatch: %+v", got)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "国语" || got.Tags[1] != "中字" {
		t.Fatalf("tags = %v", got.Tags)
	}
	if got.IMDb != "tt6485574" {
		t.Fatalf("imdb = %q", got.IMDb)
	}
	if got.Promotion != "免费" {
		t.Fatalf("promotion = %q, want 免费", got.Promotion)
	}
	if len(got.Files) != 2 || got.Files[0].Name != "Test.Movie.2026.mkv" || got.Files[0].Size != 7194070220 {
		t.Fatalf("files = %+v", got.Files)
	}
}

func TestDetailFetcherFetchAllDetailMalformedHTMLNoPanic(t *testing.T) {
	// 畸形 HTML(不闭合标签、乱序、垃圾字节)不得 panic,应静默解析为空。
	f := newTestDetailFetcher(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<table><tr><td class=\"rowhead\">x</table><<<>>><b>unclosed 标签"))
	}))

	got, err := f.FetchAllDetail(context.Background(), 1)
	if err != nil {
		t.Fatalf("FetchAllDetail on malformed HTML = %v, want nil", err)
	}
	if got == nil {
		t.Fatal("FetchAllDetail returned nil detail on malformed HTML")
	}
	// 畸形 HTML 不应解析出任何结构化字段,但也不能 panic。
	if len(got.Tags) != 0 || got.IMDb != "" || len(got.Files) != 0 {
		t.Fatalf("malformed HTML produced unexpected fields: %+v", got)
	}
}

func TestDetailFetcherFetchAllDetailAPIPath(t *testing.T) {
	// 有 APIToken 时优先走 API:Title/MediaInfo 只有 API 路径会填充,HTML 路径
	// 不会。据此可证明 FetchAllDetail 命中了 API 分支。
	f := newTestDetailFetcher(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/torrent/7":
			_, _ = w.Write([]byte(`{"data":{"id":7,"title":"API Title","mediainfo":"MI","tags":["x"]}}`))
		case "/api/v1/torrent/7/files":
			_, _ = w.Write([]byte(`{"data":[{"name":"api.mkv","size":9}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	f.apiToken = "tok"

	got, err := f.FetchAllDetail(context.Background(), 7)
	if err != nil {
		t.Fatalf("FetchAllDetail (API): %v", err)
	}
	if got.Title != "API Title" || got.MediaInfo != "MI" {
		t.Fatalf("API path not used: %+v", got)
	}
	if len(got.Files) != 1 || got.Files[0].Name != "api.mkv" {
		t.Fatalf("files = %+v", got.Files)
	}
}
