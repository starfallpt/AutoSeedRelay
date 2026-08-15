// Package web: preview page handlers — fetch RSS and preview upload fields.
package web

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/autoseedrelay/go-relay/internal/config"
	"github.com/autoseedrelay/go-relay/internal/parser"
	"github.com/autoseedrelay/go-relay/internal/source"
	"github.com/autoseedrelay/go-relay/internal/targets"
)

// ---------------------------------------------------------------------------
// RSS item preview types
// ---------------------------------------------------------------------------

// previewRSSItem is a simplified RSS item for the preview UI.
type previewRSSItem struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	CategoryName string `json:"category_name"`
	CategoryID   string `json:"category_id"`
	Size         int64  `json:"size"`
	PubDate      string `json:"pub_date"`
	Link         string `json:"link"`
	GUID         string `json:"guid"`
}

// ---------------------------------------------------------------------------
// GET /api/preview/fetch
// ---------------------------------------------------------------------------

func (s *Server) handlePreviewFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	cfg := s.cfg
	if cfg == nil || len(cfg.Sources) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":  "没有配置源站",
			"items":  []previewRSSItem{},
		})
		return
	}

	srcSite := cfg.Sources[0]

	rssURL := srcSite.RSSURL
	if rssURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":  "源站没有配置 RSS URL",
			"items":  []previewRSSItem{},
		})
		return
	}

	client, err := source.NewSourceClient(rssURL, source.SourceClientOptions{
		Passkey:      srcSite.Passkey,
		Cookie:       srcSite.Cookie,
		APIToken:     srcSite.APIToken,
		DownloadMode: "direct",
	})
	if err != nil {
		slog.Error("preview: failed to create source client", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":  "创建源站客户端失败: " + err.Error(),
			"items":  []previewRSSItem{},
		})
		return
	}
	defer client.Close()

	items, err := client.FetchRSS()
	if err != nil {
		slog.Error("preview: failed to fetch RSS", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":  "RSS 抓取失败: " + err.Error(),
			"items":  []previewRSSItem{},
		})
		return
	}

	previewItems := make([]previewRSSItem, 0, len(items))
	for _, it := range items {
		var size int64
		if it.Size != nil {
			size = *it.Size
		}
		previewItems = append(previewItems, previewRSSItem{
			ID:           it.ID,
			Title:        it.Title,
			CategoryName: it.CategoryName,
			CategoryID:   it.CategoryID,
			Size:         size,
			PubDate:      it.PubDate,
			Link:         it.Link,
			GUID:         it.GUID,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"source_name":   srcSite.Name,
		"rss_url":       rssURL,
		"poll_interval": cfg.PollInterval,
		"fetched_at":    nowUTC(),
		"total":         len(items),
		"items":         previewItems,
	})
}

// ---------------------------------------------------------------------------
// GET /api/preview/seed?id=<id>&target=<target_type>
// ---------------------------------------------------------------------------

func (s *Server) handlePreviewSeed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	seedID := r.URL.Query().Get("id")
	if seedID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少种子 id 参数"})
		return
	}

	cfg := s.cfg
	if cfg == nil || len(cfg.Sources) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "没有配置源站"})
		return
	}

	srcSite := cfg.Sources[0]
	rssURL := srcSite.RSSURL
	if rssURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "源站没有配置 RSS URL"})
		return
	}

	client, err := source.NewSourceClient(rssURL, source.SourceClientOptions{
		Passkey:      srcSite.Passkey,
		Cookie:       srcSite.Cookie,
		APIToken:     srcSite.APIToken,
		DownloadMode: "direct",
		QBHost:       cfg.QB.URL(),
		QBUser:       cfg.QB.Username,
		QBPass:       cfg.QB.Password,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "创建源站客户端失败: " + err.Error()})
		return
	}
	defer client.Close()

	// Fetch RSS to find the item.
	items, err := client.FetchRSS()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "RSS 抓取失败: " + err.Error()})
		return
	}

	var targetItem *source.RssItem
	for i := range items {
		if items[i].ID == seedID {
			targetItem = &items[i]
			break
		}
	}
	if targetItem == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "未找到种子 id=" + seedID})
		return
	}

	// Download the torrent.
	workdir := filepath.Join(os.TempDir(), "autoseedrelay_preview")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "创建工作目录失败"})
		return
	}
	torrentPath := filepath.Join(workdir, seedID+".torrent")
	defer os.Remove(torrentPath)

	ok, err := client.DownloadTorrent(targetItem, torrentPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "种子下载失败: " + err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "种子下载被拦截或内容非 torrent"})
		return
	}

	// Parse the torrent.
	parsed, err := parser.ParseTorrent(torrentPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "种子解析失败: " + err.Error()})
		return
	}

	// Build item metadata (same as pipeline buildMeta).
	itemMeta := buildPreviewMeta(targetItem, parsed)

	// Build preview for each configured target site.
	type targetPreview struct {
		Name        string         `json:"name"`
		Type        string         `json:"type"`
		BaseURL     string         `json:"base_url"`
		AnnounceURL string         `json:"announce_url"`
		Fields      map[string]any `json:"fields"`
		Error       string         `json:"error,omitempty"`
	}

	var targetPreviews []targetPreview

	for _, t := range cfg.Targets {
		if t == nil || t.Name == "" {
			continue
		}

		tp := targetPreview{
			Name:        t.Name,
			BaseURL:     t.BaseURL,
			AnnounceURL: t.AnnounceURL,
		}

		// Determine target type from site config.
		preset := lookupPreset(t.Name)
		targetType := "nexusphp"
		if preset != nil {
			tp.Type = preset.Type
			targetType = preset.Type
		} else if strings.Contains(strings.ToLower(t.Name), "m-team") || strings.Contains(t.BaseURL, "m-team") {
			tp.Type = "mteam"
			targetType = "mteam"
		} else {
			tp.Type = "nexusphp"
		}

		// Build site config dict.
		siteCfg := buildSiteCfg(t, preset, targetType)
		if t.AnnounceURL != "" {
			siteCfg["announce_base"] = t.AnnounceURL
		}

		site, err := targets.NewTargetSite(targetType, siteCfg)
		if err != nil {
			tp.Error = "创建适配器失败: " + err.Error()
			targetPreviews = append(targetPreviews, tp)
			continue
		}

		if loader, ok := site.(targets.CategoriesLoader); ok {
			site.SetCategories(loader.LoadCategories())
		}

		extra := make(map[string]any)
		for k, v := range itemMeta {
			extra[k] = v
		}

		fields := targets.BuildUploadFields(parsed, site, extra)

		// Add announce URL that would be set.
		announce := site.BuildAnnounce()
		if announce != "" {
			tp.AnnounceURL = announce
		}

		tp.Fields = fields
		targetPreviews = append(targetPreviews, tp)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"item": map[string]any{
			"id":            targetItem.ID,
			"title":         targetItem.Title,
			"category_name": targetItem.CategoryName,
			"category_id":   targetItem.CategoryID,
			"size":          valueOrZero(targetItem.Size),
			"info_hash":     parsed.InfoHash,
			"file_count":    parsed.FileCount,
			"total_size":    parsed.TotalSize,
			"parsed_name":   parsed.Name,
		},
		"targets": targetPreviews,
	})
}

// buildPreviewMeta builds the meta dict from RSS item, matching pipeline.buildMeta.
func buildPreviewMeta(item *source.RssItem, parsed *parser.ParsedTorrent) map[string]any {
	var size any
	if item.Size != nil {
		size = *item.Size
	} else {
		size = parsed.TotalSize
	}
	return map[string]any{
		"title":          item.Title,
		"descr":          item.Description,
		"category_id":    item.CategoryID,
		"category_name":  item.CategoryName,
		"small_descr":    item.SmallDescr,
		"imdb":           item.IMDB,
		"size":           size,
		"info_hash":      parsed.InfoHash,
		"rss_id":         item.ID,
		"file_count":     parsed.FileCount,
		"file_list_text": fileListTextVal(parsed),
	}
}

func fileListTextVal(parsed *parser.ParsedTorrent) string {
	if p, ok := any(parsed).(interface{ FileListText() string }); ok {
		return p.FileListText()
	}
	var lines []string
	for _, f := range parsed.Files {
		lines = append(lines, f.Path+" "+strconv.FormatInt(f.Size, 10))
	}
	return strings.Join(lines, "\n")
}

func valueOrZero(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func buildSiteCfg(site *config.SiteProfile, preset *presetTargetDef, targetType string) map[string]any {
	cfg := map[string]any{
		"target":   targetType,
		"base_url": site.BaseURL,
	}
	if site.APIToken != "" {
		cfg["api_token"] = site.APIToken
	}
	if site.MTeamAuth != "" {
		cfg["api_token"] = site.MTeamAuth
		cfg["auth_token"] = site.MTeamAuth
	}
	if site.Cookie != "" {
		cfg["cookie"] = site.Cookie
	}
	if site.Passkey != "" {
		cfg["passkey"] = site.Passkey
	}
	if site.AnnounceURL != "" {
		cfg["announce_base"] = site.AnnounceURL
	}
	if site.Name != "" {
		cfg["site_name"] = site.Name
	}
	return cfg
}
