package source

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

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

// normalizeTags 把 API 的 tags(字符串或 {"name":"..."})统一成 []string。
func normalizeTags(raw []any) []string {
	tags := make([]string, 0, len(raw))
	for _, t := range raw {
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
	return tags
}

// APIFetcher 通过 Sanctum API 获取种子详情和文件列表。
type APIFetcher struct {
	BaseURL  string
	APIToken string
	client   *http.Client
	checkURL func(string) error
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
		checkURL: safeURL,
	}
}

// FetchDetailAPI 通过 API 获取种子详情。
func (a *APIFetcher) FetchDetailAPI(ctx context.Context, torrentID int) (*SeedDetail, error) {
	if a.APIToken == "" {
		return nil, errf("API token 未设置")
	}
	u := a.BaseURL + "/api/v1/torrent/" + strconv.Itoa(torrentID)
	body, err := a.doGet(ctx, u)
	if err != nil {
		return nil, err
	}
	var resp apiTorrentResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, errf("API 响应 JSON 解析失败(torrent %d): %v", torrentID, err)
	}
	return &SeedDetail{
		ID:           resp.Data.ID,
		Title:        resp.Data.Title,
		SmallDescr:   resp.Data.SmallDescr,
		Tags:         normalizeTags(resp.Data.Tags),
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
func (a *APIFetcher) FetchFileListAPI(ctx context.Context, torrentID int) ([]FileEntry, error) {
	if a.APIToken == "" {
		return nil, errf("API token 未设置")
	}
	u := a.BaseURL + "/api/v1/torrent/" + strconv.Itoa(torrentID) + "/files"
	body, err := a.doGet(ctx, u)
	if err != nil {
		return nil, err
	}
	var resp apiFileListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, errf("API 文件列表 JSON 解析失败(torrent %d): %v", torrentID, err)
	}
	files := make([]FileEntry, 0, len(resp.Data))
	for _, f := range resp.Data {
		files = append(files, FileEntry{Name: f.Name, Size: f.Size})
	}
	return files, nil
}

// FetchAllAPI 一次获取全部:详情+文件列表。
func (a *APIFetcher) FetchAllAPI(ctx context.Context, torrentID int) (*SeedDetail, error) {
	if a.APIToken == "" {
		return nil, errf("API token 未设置")
	}
	detail, err := a.FetchDetailAPI(ctx, torrentID)
	if err != nil {
		return nil, err
	}
	if files, err := a.FetchFileListAPI(ctx, torrentID); err == nil {
		detail.Files = files
	}
	return detail, nil
}

// doGet 发起带 Bearer token 认证的 GET 请求,返回响应 body。
func (a *APIFetcher) doGet(ctx context.Context, u string) ([]byte, error) {
	if err := a.checkURL(u); err != nil {
		return nil, errf("API 请求(%s): %v", u, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, errf("构造 API 请求失败(%s): %v", u, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.APIToken)
	req.Header.Set("User-Agent", DefaultUserAgent)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, errf("API 请求网络错误(%s): %v", u, err)
	}
	defer resp.Body.Close()

	body, err := readBody(resp, maxDetailBody)
	if err != nil {
		return nil, errf("API 读取失败(%s): %v", u, err)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := string(body)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, errf("API 请求失败(%s): HTTP %d body=%q", u, resp.StatusCode, snippet)
	}
	return body, nil
}
