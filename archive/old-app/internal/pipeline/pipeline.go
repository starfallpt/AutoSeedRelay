// Package pipeline 转种流水线编排 — 把源站 RSS 命中 → qB 做种 → 目标站上传串起来。
//
// 支持两种模式(mode 参数):
//   - "A"(默认):先下载做种再转。源站 .torrent 交 qB 完整下载做种(有上传量
//     收益),做种完成后 export 取回种子,清洗后上传目标站。
//   - "B":先转再辅。源站 .torrent 直接清洗上传目标站(不等下载),发布成功后
//     用目标站种子在 qB 交叉做种(指向已有数据目录,skip_checking 挂上)。
package pipeline

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/autoseedrelay/go-relay/internal/bencode"
	"github.com/autoseedrelay/go-relay/internal/parser"
	"github.com/autoseedrelay/go-relay/internal/qb"
	"github.com/autoseedrelay/go-relay/internal/source"
	"github.com/autoseedrelay/go-relay/internal/store"
	"github.com/autoseedrelay/go-relay/internal/targets"
)

// 轮询等待"已完成做种"的默认参数。
const (
	DefaultPollInterval = 5.0
	DefaultPollTimeout  = 600.0
)

// 标签约定:流水线进行中 / 已转发完成。
const (
	TagPending = "relay-pending"
	TagDone    = "relay-done"
)

// RelayOptions 单条转种的参数。
type RelayOptions struct {
	Store          *store.RelayStore
	Mode           string // "A"=先下载做种再转(默认) / "B"=先转再辅
	TargetAnnounce string
	TargetSiteName string
	TargetBaseURL  string
	TargetCfg      map[string]any
	Savepath       string
	Category       string
	Workdir        string
	PollInterval   float64
	PollTimeout    float64
}

type relayParams struct {
	item           *source.RssItem
	parsed         *parser.ParsedTorrent
	qbClient       *qb.QBittorrent
	meta           map[string]any
	cfg            map[string]any
	store          *store.RelayStore
	infoHash       string
	result         map[string]any
	targetAnnounce string
	targetSiteName string
	targetBaseURL  string
	cleanDir       string
	category       string
	savepath       string
	pollInterval   float64
	pollTimeout    float64
	qbTorrentBytes []byte
}

// record 封装 store.MarkStatus,失败仅警告不中断。
func record(s *store.RelayStore, infoHash, status string, extra map[string]any) {
	if s == nil {
		return
	}
	if err := s.MarkStatus(infoHash, status, extra); err != nil {
		slog.Warn("store.MarkStatus failed", "infoHash", infoHash, "status", status, "error", err)
	}
}

func fileListText(parsed *parser.ParsedTorrent) string {
	if p, ok := any(parsed).(interface{ FileListText() string }); ok {
		return p.FileListText()
	}
	var lines []string
	for _, f := range parsed.Files {
		lines = append(lines, fmt.Sprintf("%s %d", f.Path, f.Size))
	}
	return strings.Join(lines, "\n")
}

// buildMeta 构造传给 targets.Upload 的 meta(源站上下文)。
func buildMeta(item *source.RssItem, parsed *parser.ParsedTorrent) map[string]any {
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
		"file_list_text": fileListText(parsed),
	}
}

// cleanTorrent 清洗种子并写临时文件,返回路径。
func cleanTorrent(parsed *parser.ParsedTorrent, targetAnnounce, targetSiteName, targetBaseURL, outDir string) (string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	cleaned, err := parser.CleanTorrentForTarget(parsed.RawDict, targetAnnounce, targetSiteName, targetBaseURL)
	if err != nil {
		return "", err
	}
	path := filepath.Join(outDir, fmt.Sprintf("clean_%s.torrent", truncateStr(parsed.InfoHash, 12)))
	if err := bencode.WriteTorrent(path, cleaned); err != nil {
		return "", err
	}
	return path, nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// CrossSeedInQB 目标站种子在 qB 交叉做种:指向源站已下载的数据目录,skip_checking 挂上。
func CrossSeedInQB(qbClient *qb.QBittorrent, targetTorrentPath, dataPath string, s *store.RelayStore, infoHash, category string) bool {
	if category == "" {
		category = "relay"
	}
	if _, err := qbClient.AddTorrentFile(targetTorrentPath, qb.AddOptions{
		Savepath:      dataPath,
		Category:      category,
		Tags:          TagPending,
		SkipChecking:  true,
	}); err != nil {
		slog.Warn("交叉做种失败(不影响主流程)", "error", err)
		return false
	}
	record(s, infoHash, "cross_seeded", nil)
	return true
}

// RelayOne 执行单条转种(模式 A 或 B)。
func RelayOne(item *source.RssItem, parsed *parser.ParsedTorrent, qbClient *qb.QBittorrent, opts RelayOptions) map[string]any {
	mode := opts.Mode
	if mode == "" {
		mode = "A"
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.PollTimeout <= 0 {
		opts.PollTimeout = DefaultPollTimeout
	}

	// store:未传入则按默认路径创建(缺依赖时降级跳过)
	s := opts.Store
	if s == nil {
		if created, err := store.Open("data/relay.db"); err == nil {
			s = created
			defer created.Close()
		} else {
			slog.Warn("store 模块未就绪,跳过去重/状态记录", "error", err)
		}
	}

	infoHash := parsed.InfoHash
	result := map[string]any{"info_hash": infoHash, "name": parsed.Name, "status": "done", "mode": mode}

	// ---- 1. store 去重记录 ----
	if s != nil {
		if has, err := s.Has(infoHash); err == nil && has {
			slog.Info("info_hash 已记录过,跳过", "infoHash", infoHash)
			return map[string]any{"status": "duplicate", "info_hash": infoHash, "mode": mode}
		}
		job := map[string]any{
			"info_hash":     infoHash,
			"rss_id":        item.ID,
			"title":         item.Title,
			"source_size":   item.Size,
			"target_status": "downloaded",
		}
		if item.Size == nil {
			job["source_size"] = parsed.TotalSize
		}
		if _, err := s.Add(job); err != nil {
			slog.Warn("store 记录失败(降级继续)", "error", err)
			result["store"] = fmt.Sprintf("error: %v", err)
		} else {
			result["store"] = "recorded"
		}
	}

	meta := buildMeta(item, parsed)
	cfg := opts.TargetCfg
	if cfg == nil {
		cfg = map[string]any{}
	}
	workdir := opts.Workdir
	if workdir == "" {
		workdir = os.TempDir()
	}
	cleanDir := filepath.Join(workdir, "cleaned")
	if err := os.MkdirAll(cleanDir, 0o755); err != nil {
		slog.Warn("创建清洗目录失败", "error", err)
	}

	p := &relayParams{
		item:           item,
		parsed:         parsed,
		qbClient:       qbClient,
		meta:           meta,
		cfg:            cfg,
		store:          s,
		infoHash:       infoHash,
		result:         result,
		targetAnnounce: opts.TargetAnnounce,
		targetSiteName: opts.TargetSiteName,
		targetBaseURL:  opts.TargetBaseURL,
		cleanDir:       cleanDir,
		category:       opts.Category,
		savepath:       opts.Savepath,
		pollInterval:   opts.PollInterval,
		pollTimeout:    opts.PollTimeout,
	}

	if strings.ToUpper(mode) == "B" {
		return relayModeB(p)
	}
	return relayModeA(p)
}

func relayModeA(p *relayParams) map[string]any {
	// ---- 2. 交 qB 完整下载做种(不做 skip_checking,数据会真实下载) ----
	category := p.category
	if category == "" {
		category = "relay"
	}
	if _, err := p.qbClient.AddTorrentFile(p.parsed.Path, qb.AddOptions{
		Savepath:      p.savepath,
		Category:      category,
		Tags:          TagPending,
		SkipChecking:  false, // 模式 A:真实下载数据
	}); err != nil {
		slog.Error("qB 添加失败", "error", err)
		record(p.store, p.infoHash, "failed", map[string]any{"error": "add_to_qb: " + err.Error()})
		return map[string]any{"status": "error", "info_hash": p.infoHash, "step": "add", "error": err.Error(), "mode": "A"}
	}
	p.result["add"] = "ok"
	record(p.store, p.infoHash, "added_to_qb", map[string]any{"qb_hash": p.infoHash})

	// ---- 3. 轮询等待"已完成做种" ----
	deadline := time.Now().Add(time.Duration(p.pollTimeout * float64(time.Second)))
	completed := false
	for time.Now().Before(deadline) {
		t, err := p.qbClient.GetTorrent(p.infoHash)
		if err != nil {
			slog.Warn("轮询 qB 失败(稍后重试)", "error", err)
			t = nil
		}
		if t != nil && qb.IsCompletedSeeding(t) {
			completed = true
			if state, ok := t["state"]; ok {
				p.result["qB_state"] = state
			}
			break
		}
		time.Sleep(time.Duration(p.pollInterval * float64(time.Second)))
	}
	if !completed {
		slog.Warn("模式A 轮询超时: 未进入已完成做种", "infoHash", p.infoHash)
		p.result["status"] = "timeout"
		record(p.store, p.infoHash, "failed", map[string]any{"error": "poll timeout (mode A)"})
		return p.result
	}
	record(p.store, p.infoHash, "seeded", nil)

	// ---- 4. 从 qB 取回 .torrent ----
	torrentBytes, err := p.qbClient.ExportTorrent(p.infoHash)
	if err != nil {
		slog.Error("导出种子失败", "error", err)
		record(p.store, p.infoHash, "failed", map[string]any{"error": "export: " + err.Error()})
		return map[string]any{"status": "error", "info_hash": p.infoHash, "step": "export", "error": err.Error(), "mode": "A"}
	}
	p.result["export_bytes"] = len(torrentBytes)
	p.qbTorrentBytes = torrentBytes

	return uploadCleaned(p)
}

func relayModeB(p *relayParams) map[string]any {
	if !(p.targetAnnounce != "" && p.targetSiteName != "" && p.targetBaseURL != "") {
		slog.Warn("模式B 缺 target_* 配置,跳过上传")
		p.result["status"] = "cleaned_no_upload"
		return p.result
	}
	return uploadCleaned(p)
}

func uploadCleaned(p *relayParams) map[string]any {
	// ---- 5. 清洗种子(改 announce 为目标站) ----
	cleanPath, err := cleanTorrent(p.parsed, p.targetAnnounce, p.targetSiteName, p.targetBaseURL, p.cleanDir)
	if err != nil {
		slog.Error("种子清洗失败", "error", err)
		p.result["cleaned"] = false
		p.result["error"] = err.Error()
		record(p.store, p.infoHash, "failed", map[string]any{"error": "clean: " + err.Error()})
		return p.result
	}
	p.result["cleaned"] = true
	p.result["clean_path"] = cleanPath

	// ---- 6. 上传目标站 ----
	ok, targetID, targetSite, err := targets.Upload(cleanPath, p.meta, p.cfg)
	if err != nil {
		existing := false
		if ue, isUE := err.(*targets.UploadError); isUE {
			existing = ue.Existing
		}
		status := "skipped_existing"
		if !existing {
			status = "error"
		}
		slog.Error("目标站上传失败", "status", status, "error", err)
		errStr := err.Error()
		if existing {
			errStr = "existing: " + errStr
		}
		record(p.store, p.infoHash, status, map[string]any{"error": errStr})
		return map[string]any{"status": status, "info_hash": p.infoHash, "step": "upload", "error": errStr, "mode": p.result["mode"]}
	}
	if !ok {
		msg := fmt.Sprintf("upload rejected target_id=%d", targetID)
		record(p.store, p.infoHash, "failed", map[string]any{"error": msg})
		return map[string]any{"status": "error", "info_hash": p.infoHash, "step": "upload", "error": msg, "mode": p.result["mode"]}
	}

	p.result["target_id"] = targetID
	p.result["target_site"] = targetSite
	record(p.store, p.infoHash, "uploaded", map[string]any{"target_id": targetID, "target_site": targetSite})

	// ---- 7. 交叉做种:目标站种子回 qB(指向源站数据目录,skip_checking) ----
	dataPath := ""
	if v, ok := p.cfg["savepath"].(string); ok {
		dataPath = v
	}
	crossOK := CrossSeedInQB(p.qbClient, cleanPath, dataPath, p.store, p.infoHash, p.category)
	p.result["cross_seeded"] = crossOK

	// ---- 8. 标记 relay-done ----
	if err := p.qbClient.SetTags(p.infoHash, TagDone); err != nil {
		slog.Warn("打 relay-done 标签失败", "error", err)
	} else {
		p.result["tags"] = TagDone
	}

	p.result["status"] = "done"
	return p.result
}
