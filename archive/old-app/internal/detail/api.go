// Package detail Sanctum API 方式的种子详情/文件列表获取。
//
// NexusPHP 系列站点(如星云阁)可通过 Sanctum API:
//
//	GET /api/v1/torrent/{id}         -- 种子详情
//	GET /api/v1/torrent/{id}/files   -- 文件列表
//
// 需要 Bearer token 认证。与 HTML 爬取方式互为降级:
//
//	有 token → 先调 API，失败回退 Cookie+HTML
//	无 token → 直接 HTML 爬取(detail.go)
package detail

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"
)

// --------------------------------------------------------------------------
// API 响应结构体(JSON)
// --------------------------------------------------------------------------

type apiTorrentResponse struct {
	Data apiTorrentData `json:"data"`
}

type apiTorrentData struct {
	ID         int         `json:"id"`
	Title      string      `json:"title"`
	SmallDescr string      `json:"small_descr"`
	Tags       []any       `json:"tags"` // string 或 {"name":"..."} 两种格式都支持
	Descr      string      `json:"descr"`
	Category   apiCategory `json:"category"`
	Size       int64       `json:"size"`
	InfoHash   string      `json:"info_hash"`
	IMDb       string      `json:"imdb"`
	DoubanID   string      `json:"douban_id"`
	Team       string      `json:"team"`
	Medium     string      `json:"medium"`
	Codec      string      `json:"codec"`
	AudioCodec string      `json:"audiocodec"`
	Resolution string      `json:"resolution"`
	MediaInfo  string      `json:"mediainfo"`
}

type apiTag struct {
	Name string `json:"name"`
}

type apiCategory struct {
	Name string `json:"name"`
	ID   int    `json:"id"`
}

type apiFileListResponse struct {
	Data []apiFileEntry `json:"data"`
}

type apiFileEntry struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// --------------------------------------------------------------------------
// 公开返回值类型
// --------------------------------------------------------------------------

// FileEntry API 方式返回的单个文件条目。
type FileEntry struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// SeedDetail 种子详情(API 或 HTML 解析结果)。
type SeedDetail struct {
	ID           int         `json:"id"`
	Title        string      `json:"title"`
	SmallDescr   string      `json:"small_descr"`
	Tags         []string    `json:"tags"`
	MediaInfo    string      `json:"mediainfo"`
	DescrHTML    string      `json:"descr"`
	CategoryName string      `json:"category_name"`
	CategoryID   int         `json:"category_id"`
	Size         int64       `json:"size"`
	InfoHash     string      `json:"info_hash"`
	IMDb         string      `json:"imdb"`
	DoubanID     string      `json:"douban_id"`
	Files        []FileEntry `json:"files"`
	Medium       string      `json:"medium"`
	Codec        string      `json:"codec"`
	AudioCodec   string      `json:"audiocodec"`
	Resolution   string      `json:"resolution"`
	Team         string      `json:"team"`
}

// --------------------------------------------------------------------------
// APIFetcher
// --------------------------------------------------------------------------

// APIFetcher 通过 Sanctum API 获取种子详情和文件列表。
type APIFetcher struct {
	BaseURL  string
	APIToken string
	client   *http.Client
}

// NewAPIFetcher 构造 API 抓取客户端。token 为空字符串时后续调用直接返回错误。
func NewAPIFetcher(baseURL string, token string, timeoutSeconds float64) *APIFetcher {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 30
	}
	return &APIFetcher{
		BaseURL:  baseURL,
		APIToken: token,
		client:   &http.Client{Timeout: time.Duration(timeoutSeconds * float64(time.Second))},
	}
}

// --------------------------------------------------------------------------
// API 请求方法
// --------------------------------------------------------------------------

// FetchDetailAPI 通过 API 获取种子详情。
func (a *APIFetcher) FetchDetailAPI(torrentID int) (*SeedDetail, error) {
	if a.APIToken == "" {
		return nil, errf("API token 未设置")
	}
	url := a.BaseURL + "/api/v1/torrent/" + strconv.Itoa(torrentID)
	body, err := a.doGet(url)
	if err != nil {
		return nil, err
	}
	var resp apiTorrentResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, errf("API 响应 JSON 解析失败(torrent %d): %v", torrentID, err)
	}

	// 提取标签:支持 string 和 {"name":"..."} 两种格式
	tags := make([]string, 0, len(resp.Data.Tags))
	for _, t := range resp.Data.Tags {
		switch v := t.(type) {
		case string:
			if v != "" {
				tags = append(tags, v)
			}
		case map[string]any:
			if name, ok := v["name"].(string); ok && name != "" {
				tags = append(tags, name)
			}
		}
	}

	return &SeedDetail{
		ID:           resp.Data.ID,
		Title:        resp.Data.Title,
		SmallDescr:   resp.Data.SmallDescr,
		Tags:         tags,
		MediaInfo:    resp.Data.MediaInfo,
		DescrHTML:    resp.Data.Descr,
		CategoryName: resp.Data.Category.Name,
		CategoryID:   resp.Data.Category.ID,
		Size:         resp.Data.Size,
		InfoHash:     resp.Data.InfoHash,
		IMDb:         resp.Data.IMDb,
		DoubanID:     resp.Data.DoubanID,
		Medium:       resp.Data.Medium,
		Codec:        resp.Data.Codec,
		AudioCodec:   resp.Data.AudioCodec,
		Resolution:   resp.Data.Resolution,
		Team:         resp.Data.Team,
	}, nil
}

// FetchFileListAPI 通过 API 获取文件列表。
func (a *APIFetcher) FetchFileListAPI(torrentID int) ([]FileEntry, error) {
	if a.APIToken == "" {
		return nil, errf("API token 未设置")
	}
	url := a.BaseURL + "/api/v1/torrent/" + strconv.Itoa(torrentID) + "/files"
	body, err := a.doGet(url)
	if err != nil {
		return nil, err
	}
	var resp apiFileListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, errf("API 文件列表 JSON 解析失败(torrent %d): %v", torrentID, err)
	}
	files := make([]FileEntry, 0, len(resp.Data))
	for _, f := range resp.Data {
		files = append(files, FileEntry{
			Name: f.Name,
			Size: f.Size,
		})
	}
	return files, nil
}

// FetchAllAPI 一次获取全部:详情+文件+mediainfo。
func (a *APIFetcher) FetchAllAPI(torrentID int) (*SeedDetail, error) {
	if a.APIToken == "" {
		return nil, errf("API token 未设置")
	}
	detail, err := a.FetchDetailAPI(torrentID)
	if err != nil {
		return nil, err
	}
	files, err := a.FetchFileListAPI(torrentID)
	if err != nil {
		// 文件列表获取失败不致命,继续返回详情(无文件列表)
	}
	detail.Files = files
	return detail, nil
}

// doGet 发起带 Bearer token 认证的 GET 请求,返回响应 body。
func (a *APIFetcher) doGet(url string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, errf("构造 API 请求失败(%s): %v", url, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.APIToken)
	req.Header.Set("User-Agent", DefaultUserAgent)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, errf("API 请求网络错误(%s): %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		snippet := string(body)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, errf("API 请求失败(%s): HTTP %d body=%q", url, resp.StatusCode, snippet)
	}

	return io.ReadAll(resp.Body)
}
