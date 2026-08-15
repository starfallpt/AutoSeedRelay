// Command relay 是 AutoSeedRelay 统一 CLI 入口。
//
// 子命令:
//
//	relay preview  --target nexusphp|mteam|nexusphp_classic --keyword ... [--download-mode direct|qb]
//	relay run      --target nexusphp|mteam|nexusphp_classic --keyword ... [--once] [--interval N]
//	relay fetch    --keyword ... [--out ...]
//	relay upload   --torrent <path> --target nexusphp|mteam|nexusphp_classic ...
//	relay probe    --url <url> [--cookie ...] [--token ...] [--out ...]
//	relay qb       <login|add-torrent|export|info|stop|delete|...>
//
// 环境变量统一: AUTOSEED_DOWNLOAD_MODE / AUTOSEED_PROXY / AUTOSEED_RSS /
// AUTOSEED_PASSKEY / AUTOSEED_DB / AUTOSEED_TARGET / AUTOSEED_BASE_URL /
// AUTOSEED_API_TOKEN / AUTOSEED_AUTH_TOKEN / AUTOSEED_ANNOUNCE_BASE /
// AUTOSEED_SITE_NAME / QBHOST / QBUSER / QBPASS / SITE_URL / SITE_COOKIE / SITE_TOKEN。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/autoseedrelay/go-relay/internal/parser"
	"github.com/autoseedrelay/go-relay/internal/pipeline"
	"github.com/autoseedrelay/go-relay/internal/qb"
	"github.com/autoseedrelay/go-relay/internal/source"
	"github.com/autoseedrelay/go-relay/internal/store"
	"github.com/autoseedrelay/go-relay/internal/targets"
)

var targetChoices = []string{"nexusphp", "mteam", "nexusphp_classic"}
var downloadModes = []string{"direct", "qb"}

var rootCmd = &cobra.Command{
	Use:   "relay",
	Short: "AutoSeedRelay 统一 CLI:preview / run / fetch / upload / probe / qb",
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// targetCfgFromEnv 从环境变量构造目标站配置 dict(敏感信息走环境变量)。
func targetCfgFromEnv() map[string]any {
	cfg := map[string]any{}
	if v := os.Getenv("AUTOSEED_TARGET"); v != "" {
		cfg["target"] = v
	}
	if v := os.Getenv("AUTOSEED_BASE_URL"); v != "" {
		cfg["base_url"] = v
	}
	if v := os.Getenv("AUTOSEED_API_TOKEN"); v != "" {
		cfg["api_token"] = v
	}
	if v := os.Getenv("AUTOSEED_AUTH_TOKEN"); v != "" {
		cfg["auth_token"] = v
	}
	if v := os.Getenv("AUTOSEED_ANNOUNCE_BASE"); v != "" {
		cfg["announce_base"] = v
	}
	if v := os.Getenv("AUTOSEED_SITE_NAME"); v != "" {
		cfg["site_name"] = v
	}
	if v := os.Getenv("AUTOSEED_PASSKEY"); v != "" {
		cfg["passkey"] = v
	}
	if v := os.Getenv("AUTOSEED_CATEGORY"); v != "" {
		cfg["category"] = v
	}
	return cfg
}

func addCommonDownloadFlags(cmd *cobra.Command) {
	cmd.Flags().String("download-mode", envOr("AUTOSEED_DOWNLOAD_MODE", "direct"),
		"种子下载模式:direct=服务端代理(passkey 拼接+可选 HTTP 代理,默认) / qb=qB 直拉(qB 服务器拉种子再 export 取回)")
	cmd.Flags().String("proxy", os.Getenv("AUTOSEED_PROXY"), "HTTP 代理(direct 模式用,如 http://127.0.0.1:7890)")
}

func resolveKeywords(flags []string) []string {
	if len(flags) > 0 {
		return flags
	}
	return []string{"StarfallWeb", "LongWeb"}
}

func init() {
	rootCmd.AddCommand(newPreviewCmd())
	rootCmd.AddCommand(newRunCmd())
	rootCmd.AddCommand(newFetchCmd())
	rootCmd.AddCommand(newUploadCmd())
	rootCmd.AddCommand(newProbeCmd())
	rootCmd.AddCommand(newQBCmd())
	rootCmd.AddCommand(newServeCmd())
}

// ---------------------------------------------------------------------------
// preview
// ---------------------------------------------------------------------------

func newPreviewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "preview",
		Short: "转种预览(只读):抓 RSS → 下载 → 解析 → 映射目标站字段",
		RunE:  runPreview,
	}
	cmd.Flags().String("target", "", "目标站类型(必填)")
	cmd.Flags().StringArray("keyword", nil, "命中关键词(默认 StarfallWeb)")
	cmd.Flags().String("torrent-id", "", "指定种子 id(如 169620);缺省用 RSS 命中")
	cmd.Flags().Int("rows", 100, "RSS 抓取条数")
	cmd.Flags().String("category", envOr("AUTOSEED_CATEGORY", "动漫"), "目标站分类(默认 动漫;环境变量 AUTOSEED_CATEGORY 优先)")
	cmd.Flags().String("small-descr", envOr("AUTOSEED_SMALL_DESCR", ""), "副标题(可选)")
	cmd.Flags().String("workdir", "samples_relay", "工作目录")
	cmd.Flags().Bool("confirm", false, "确认后执行(会真实上传 + 操作 qB;缺省只预览只读)")
	cmd.Flags().String("mode", "B", "执行模式 A=先下载做种再转 / B=先转再辅(仅 --confirm 时生效)")
	addCommonDownloadFlags(cmd)
	return cmd
}

func runPreview(cmd *cobra.Command, args []string) error {
	target, _ := cmd.Flags().GetString("target")
	if target == "" {
		return fmt.Errorf("--target 必填(可选: %s)", strings.Join(targets.TargetTypes(), ", "))
	}
	keywords, _ := cmd.Flags().GetStringArray("keyword")
	keywords = resolveKeywords(keywords)
	torrentID, _ := cmd.Flags().GetString("torrent-id")
	rows, _ := cmd.Flags().GetInt("rows")
	category, _ := cmd.Flags().GetString("category")
	smallDescr, _ := cmd.Flags().GetString("small-descr")
	workdir, _ := cmd.Flags().GetString("workdir")
	confirm, _ := cmd.Flags().GetBool("confirm")
	mode, _ := cmd.Flags().GetString("mode")
	downloadMode, _ := cmd.Flags().GetString("download-mode")
	proxy, _ := cmd.Flags().GetString("proxy")

	if confirm {
		return runConfirm(target, keywords, rows, mode, downloadMode, proxy, workdir, category, smallDescr)
	}

	rss := envOr("AUTOSEED_RSS", "")
	if rss == "" {
		return fmt.Errorf("未提供 RSS URL(用环境变量 AUTOSEED_RSS)")
	}
	client, err := source.NewSourceClient(rss, source.SourceClientOptions{
		Passkey:       envOr("AUTOSEED_PASSKEY", ""),
		DownloadMode:  downloadMode,
		Proxy:         proxy,
	})
	if err != nil {
		return err
	}
	defer client.Close()

	items, err := client.FetchRSS()
	if err != nil {
		return err
	}
	fmt.Printf("RSS=%d 命中关键词 %v:\n", len(items), keywords)
	hits := filterHits(items, keywords)
	for _, it := range hits {
		if torrentID != "" && it.ID != torrentID {
			continue
		}
		fmt.Printf("  id=%s title=%s\n", it.ID, truncate(it.Title, 70))
	}

	// 下载 + 解析 + 映射目标站字段
	if len(hits) == 0 {
		return nil
	}
	torrentDir := filepath.Join(workdir, "torrents")
	if err := os.MkdirAll(torrentDir, 0o755); err != nil {
		return err
	}
	cfg := targetCfgFromEnv()
	cfg["target"] = target
	cfg["category"] = category
	if smallDescr != "" {
		cfg["small_descr"] = smallDescr
	}

	for _, it := range hits {
		tp := filepath.Join(torrentDir, it.ID+".torrent")
		ok, err := client.DownloadTorrent(&it, tp)
		if err != nil {
			fmt.Printf("  下载失败 id=%s: %v\n", it.ID, err)
			continue
		}
		if !ok {
			fmt.Printf("  下载未成功 id=%s(被拦截),跳过\n", it.ID)
			continue
		}
		parsed, err := parser.ParseTorrent(tp)
		if err != nil {
			fmt.Printf("  解析失败 id=%s: %v\n", it.ID, err)
			continue
		}
		site, err := targets.NewTargetSite(target, cfg)
		if err != nil {
			return err
		}
		if loader, ok := site.(targets.CategoriesLoader); ok {
			site.SetCategories(loader.LoadCategories())
		}
		fields := targets.BuildUploadFields(parsed, site, cfg)
		fmt.Printf("  === 预览 id=%s ===\n", it.ID)
		for k, v := range fields {
			fmt.Printf("    %s: %v\n", k, v)
		}
	}
	return nil
}

func runConfirm(target string, keywords []string, rows int, mode, downloadMode, proxy, workdir, category, smallDescr string) error {
	rss := envOr("AUTOSEED_RSS", "")
	if rss == "" {
		return fmt.Errorf("未提供 RSS URL(用环境变量 AUTOSEED_RSS)")
	}
	client, err := source.NewSourceClient(rss, source.SourceClientOptions{
		Passkey:      envOr("AUTOSEED_PASSKEY", ""),
		DownloadMode: downloadMode,
		Proxy:        proxy,
	})
	if err != nil {
		return err
	}
	defer client.Close()

	qbClient := newQBFromEnv()
	if qbClient == nil {
		return fmt.Errorf("confirm 模式需要 qB 凭据(环境变量 QBHOST/QBUSER/QBPASS)")
	}

	items, err := client.FetchRSS()
	if err != nil {
		return err
	}
	hits := filterHits(items, keywords)
	if len(hits) == 0 {
		fmt.Println("无命中")
		return nil
	}
	it := hits[0]
	torrentDir := filepath.Join(workdir, "torrents")
	_ = os.MkdirAll(torrentDir, 0o755)
	tp := filepath.Join(torrentDir, it.ID+".torrent")
	ok, err := client.DownloadTorrent(&it, tp)
	if err != nil || !ok {
		return fmt.Errorf("下载失败 id=%s: %v", it.ID, err)
	}
	parsed, err := parser.ParseTorrent(tp)
	if err != nil {
		return err
	}

	cfg := targetCfgFromEnv()
	cfg["target"] = target
	cfg["category"] = category
	if smallDescr != "" {
		cfg["small_descr"] = smallDescr
	}
	// 目标站配置不齐时由 pipeline 降级为 cleaned_no_upload
	baseURL := getStr(cfg, "base_url")
	announce := getStr(cfg, "announce_base")
	if announce == "" {
		announce = strings.TrimRight(baseURL, "/") + "/announce.php"
	}
	siteName := getStr(cfg, "site_name")
	if siteName == "" {
		siteName = target
	}

	result := pipeline.RelayOne(&it, parsed, qbClient, pipeline.RelayOptions{
		Mode:           mode,
		TargetAnnounce: announce,
		TargetSiteName: siteName,
		TargetBaseURL:  baseURL,
		TargetCfg:      cfg,
		Savepath:       envOr("AUTOSEED_SAVEPATH", ""),
		Category:       "relay",
		Workdir:        workdir,
	})
	printResult(result)
	return nil
}

// ---------------------------------------------------------------------------
// run
// ---------------------------------------------------------------------------

type runFlags struct {
	target       string
	keywords     []string
	once         bool
	interval     int
	rows         int
	rss          string
	passkey      string
	db           string
	torrentsDir  string
	sourceSite   string
	timeout      float64
	downloadMode string
	proxy        string
}

func newRunCmd() *cobra.Command {
	f := &runFlags{}
	cmd := &cobra.Command{
		Use:   "run",
		Short: "转种调度主循环:RSS → 筛选 → 下载 → 解析 → qB(桩) → 上传 → 去重入库",
		RunE: func(cmd *cobra.Command, args []string) error {
			f.target, _ = cmd.Flags().GetString("target")
			f.keywords, _ = cmd.Flags().GetStringArray("keyword")
			f.once, _ = cmd.Flags().GetBool("once")
			f.interval, _ = cmd.Flags().GetInt("interval")
			f.rows, _ = cmd.Flags().GetInt("rows")
			f.rss, _ = cmd.Flags().GetString("rss")
			f.passkey, _ = cmd.Flags().GetString("passkey")
			f.db, _ = cmd.Flags().GetString("db")
			f.torrentsDir, _ = cmd.Flags().GetString("torrents-dir")
			f.sourceSite, _ = cmd.Flags().GetString("source-site")
			f.timeout, _ = cmd.Flags().GetFloat64("timeout")
			f.downloadMode, _ = cmd.Flags().GetString("download-mode")
			f.proxy, _ = cmd.Flags().GetString("proxy")
			return runRunLoop(f)
		},
	}
	cmd.Flags().String("target", "", "目标站类型(透传给 targets.upload)")
	cmd.Flags().StringArray("keyword", nil, "命中关键词/后缀,可多次(默认 StarfallWeb|LongWeb)")
	cmd.Flags().Bool("once", false, "跑一轮退出")
	cmd.Flags().Int("interval", envOrInt("AUTOSEED_INTERVAL", 300), "轮询间隔秒数(默认 300;环境变量 AUTOSEED_INTERVAL 优先)")
	cmd.Flags().Int("rows", 20, "抓取条数(1-200)")
	cmd.Flags().String("rss", envOr("AUTOSEED_RSS", ""), "源站 RSS URL(优先用环境变量 AUTOSEED_RSS)")
	cmd.Flags().String("passkey", envOr("AUTOSEED_PASSKEY", ""), "源站 passkey(优先用环境变量 AUTOSEED_PASSKEY)")
	cmd.Flags().String("db", envOr("AUTOSEED_DB", "data/relay.db"), "SQLite 去重库路径(默认 data/relay.db)")
	cmd.Flags().String("torrents-dir", "data/torrents", "源站种子保存目录(默认 data/torrents)")
	cmd.Flags().String("source-site", envOr("AUTOSEED_SOURCE_SITE", "starfall-nexus"), "源站标识(写入 store.source_site)")
	cmd.Flags().Float64("timeout", envOrFloat("AUTOSEED_TIMEOUT", 30), "HTTP 超时秒数(默认 30)")
	addCommonDownloadFlags(cmd)
	return cmd
}

var statusProcess = map[string]bool{"pending": true, "failed": true, "downloaded": true}

func runRunLoop(f *runFlags) error {
	if f.rss == "" {
		return fmt.Errorf("未提供 RSS URL(用 --rss 或环境变量 AUTOSEED_RSS)")
	}
	s, err := store.Open(f.db)
	if err != nil {
		return fmt.Errorf("初始化去重库失败: %v", err)
	}
	defer s.Close()

	round := 0
	for {
		round++
		fmt.Printf("[%d] === 第 %d 轮开始 ===\n", time.Now().Unix(), round)
		summary, err := runOnce(s, f)
		if err != nil {
			fmt.Printf("[%d] 本轮异常(记录,下轮重试): %v\n", time.Now().Unix(), err)
		} else {
			fmt.Printf("[%d] 本轮摘要: RSS=%d 命中=%d 新条目=%d 处理=%d 上传成功=%d 失败=%d store跳过=%d\n",
				time.Now().Unix(), summary["rss"], summary["matched"], summary["new"],
				summary["processed"], summary["uploaded"], summary["failed"], summary["store_skip"])
		}
		if f.once {
			break
		}
		time.Sleep(time.Duration(f.interval) * time.Second)
	}
	return nil
}

func runOnce(s *store.RelayStore, f *runFlags) (map[string]int, error) {
	summary := map[string]int{"rss": 0, "matched": 0, "new": 0, "processed": 0, "uploaded": 0, "failed": 0, "store_skip": 0}

	rss := f.rss
	if !strings.Contains(rss, "rows=") {
		sep := "&"
		if !strings.Contains(rss, "?") {
			sep = "?"
		}
		rss = rss + sep + "rows=" + strconv.Itoa(f.rows)
	}
	keywords := resolveKeywords(f.keywords)
	if err := os.MkdirAll(f.torrentsDir, 0o755); err != nil {
		return summary, err
	}

	cfg := map[string]any{
		"source_site":  f.sourceSite,
		"passkey":      f.passkey,
		"db_path":      f.db,
		"torrents_dir": f.torrentsDir,
		"target":       f.target,
	}

	client, err := source.NewSourceClient(rss, source.SourceClientOptions{
		TimeoutSeconds: f.timeout,
		Passkey:        f.passkey,
		DownloadMode:   f.downloadMode,
		Proxy:          f.proxy,
	})
	if err != nil {
		return summary, err
	}
	defer client.Close()

	items, err := client.FetchRSS()
	if err != nil {
		return summary, err
	}
	summary["rss"] = len(items)
	hits := filterHits(items, keywords)
	summary["matched"] = len(hits)
	fmt.Printf("[%d] RSS=%d 命中关键词 %v: %d\n", time.Now().Unix(), len(items), keywords, len(hits))

	for _, it := range hits {
		ih := source.GuidToInfohash(it.GUID)
		rec, _ := s.Get(ih)
		if rec == nil {
			_, _ = s.Add(map[string]any{
				"info_hash":     ih,
				"rss_id":        it.ID,
				"title":         it.Title,
				"source_site":   f.sourceSite,
				"source_size":   it.Size,
				"target_status": "pending",
			})
			summary["new"]++
			rec, _ = s.Get(ih)
		}
		if rec == nil {
			continue
		}
		status, _ := rec["target_status"].(string)
		if !statusProcess[status] {
			summary["store_skip"]++
			continue
		}
		summary["processed"]++
		fmt.Printf("[%d] 处理 id=%s status=%s title=%s\n", time.Now().Unix(), it.ID, status, truncate(it.Title, 60))

		// 下载 .torrent(已有文件则复用)
		tp := filepath.Join(f.torrentsDir, it.ID+".torrent")
		info, err := os.Stat(tp)
		if err != nil || info.Size() == 0 {
			ok, derr := client.DownloadTorrent(&it, tp)
			if derr != nil {
				_ = s.MarkStatus(ih, "failed", map[string]any{"error": "download: " + derr.Error()})
				summary["failed"]++
				continue
			}
			if !ok {
				_ = s.MarkStatus(ih, "failed", map[string]any{"error": "download: 所有后端均被 WAF 拦截"})
				summary["failed"]++
				continue
			}
			fmt.Printf("[%d]   下载 OK -> %s\n", time.Now().Unix(), filepath.Base(tp))
		}

		// 解析 bencode
		parsed, perr := parser.ParseTorrent(tp)
		if perr != nil {
			_ = s.MarkStatus(ih, "failed", map[string]any{"error": "parse: " + perr.Error()})
			summary["failed"]++
			continue
		}
		if !strings.EqualFold(parsed.InfoHash, ih) {
			fmt.Printf("[%d]   [warn] 种子 info_hash=%s != RSS guid=%s,去重以 RSS 为准\n",
				time.Now().Unix(), parsed.InfoHash, ih)
		}

		_ = s.MarkStatus(ih, "downloaded", nil)

		// 目标站上传
		res := uploadToTarget(s, ih, tp, parsed, &it, cfg)
		switch res {
		case "uploaded":
			summary["uploaded"]++
		case "failed":
			summary["failed"]++
		}
	}
	return summary, nil
}

func uploadToTarget(s *store.RelayStore, ih, torrentPath string, parsed *parser.ParsedTorrent, it *source.RssItem, cfg map[string]any) string {
	meta := map[string]any{
		"title":          it.Title,
		"descr":          it.Description,
		"category_id":    it.CategoryID,
		"category_name":  it.CategoryName,
		"small_descr":    it.SmallDescr,
		"imdb":           it.IMDB,
		"size":           it.Size,
		"info_hash":      parsed.InfoHash,
		"rss_id":         it.ID,
		"file_count":     parsed.FileCount,
		"file_list_text": fileListText(parsed),
	}
	ok, targetID, targetSite, err := targets.Upload(torrentPath, meta, cfg)
	if err != nil {
		_ = s.MarkStatus(ih, "failed", map[string]any{"error": "upload: " + err.Error()})
		fmt.Printf("[%d]   上传失败: %v\n", time.Now().Unix(), err)
		return "failed"
	}
	if ok {
		_ = s.MarkStatus(ih, "uploaded", map[string]any{"target_id": targetID, "target_site": targetSite})
		fmt.Printf("[%d]   上传成功 target_id=%d site=%s\n", time.Now().Unix(), targetID, targetSite)
		return "uploaded"
	}
	_ = s.MarkStatus(ih, "failed", map[string]any{"error": fmt.Sprintf("upload rejected (target_id=%d)", targetID)})
	return "failed"
}

// ---------------------------------------------------------------------------
// fetch
// ---------------------------------------------------------------------------

func newFetchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fetch",
		Short: "抓取源站 RSS、按关键词筛选、下载种子、解析字段并输出样本",
		RunE:  runFetch,
	}
	cmd.Flags().StringArray("keyword", nil, "命中关键词/后缀,可多次(如 --keyword StarfallWeb --keyword LongWeb)")
	cmd.Flags().Int("rows", 10, "抓取条数(1-200)")
	cmd.Flags().String("rss", envOr("AUTOSEED_RSS", ""), "源站 RSS URL(优先用环境变量 AUTOSEED_RSS)")
	cmd.Flags().String("passkey", envOr("AUTOSEED_PASSKEY", ""), "源站 passkey(优先用环境变量 AUTOSEED_PASSKEY)")
	cmd.Flags().String("out", "samples", "输出目录")
	cmd.Flags().Bool("json-only", false, "只解析 RSS 不下载种子")
	cmd.Flags().String("torrents-dir", "samples/torrents", "种子文件保存目录")
	addCommonDownloadFlags(cmd)
	return cmd
}

func runFetch(cmd *cobra.Command, args []string) error {
	keywords, _ := cmd.Flags().GetStringArray("keyword")
	keywords = resolveKeywords(keywords)
	rows, _ := cmd.Flags().GetInt("rows")
	rss, _ := cmd.Flags().GetString("rss")
	passkey, _ := cmd.Flags().GetString("passkey")
	out, _ := cmd.Flags().GetString("out")
	jsonOnly, _ := cmd.Flags().GetBool("json-only")
	torrentsDir, _ := cmd.Flags().GetString("torrents-dir")
	downloadMode, _ := cmd.Flags().GetString("download-mode")
	proxy, _ := cmd.Flags().GetString("proxy")

	if rss == "" {
		return fmt.Errorf("未提供 RSS URL(用 --rss 或环境变量 AUTOSEED_RSS)")
	}
	if !strings.Contains(rss, "rows=") {
		sep := "&"
		if !strings.Contains(rss, "?") {
			sep = "?"
		}
		rss = rss + sep + "rows=" + strconv.Itoa(rows)
	}
	client, err := source.NewSourceClient(rss, source.SourceClientOptions{
		Passkey:      passkey,
		DownloadMode: downloadMode,
		Proxy:        proxy,
	})
	if err != nil {
		return err
	}
	defer client.Close()

	items, err := client.FetchRSS()
	if err != nil {
		return err
	}
	hits := filterHits(items, keywords)
	fmt.Printf("RSS=%d 命中关键词 %v: %d\n", len(items), keywords, len(hits))

	if jsonOnly {
		b, _ := json.MarshalIndent(hits, "", "  ")
		fmt.Println(string(b))
		return nil
	}

	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(torrentsDir, 0o755); err != nil {
		return err
	}

	var samples []map[string]any
	for _, it := range hits {
		tp := filepath.Join(torrentsDir, it.ID+".torrent")
		ok, derr := client.DownloadTorrent(&it, tp)
		if derr != nil {
			fmt.Printf("  下载失败 id=%s: %v\n", it.ID, derr)
			continue
		}
		if !ok {
			fmt.Printf("  下载未成功 id=%s(被拦截),跳过\n", it.ID)
			continue
		}
		parsed, perr := parser.ParseTorrent(tp)
		if perr != nil {
			fmt.Printf("  解析失败 id=%s: %v\n", it.ID, perr)
			continue
		}
		fmt.Printf("  id=%s title=%s size=%d files=%d\n", it.ID, truncate(it.Title, 70), parsed.TotalSize, parsed.FileCount)
		samples = append(samples, map[string]any{
			"id": it.ID, "title": it.Title, "link": it.Link,
			"info_hash": parsed.InfoHash, "name": parsed.Name,
			"total_size": parsed.TotalSize, "file_count": parsed.FileCount,
			"files": parsed.Files,
		})
	}
	if len(samples) > 0 {
		outFile := filepath.Join(out, "samples.json")
		b, _ := json.MarshalIndent(samples, "", "  ")
		if err := os.WriteFile(outFile, b, 0o644); err != nil {
			return err
		}
		fmt.Printf("样本写入 %s\n", outFile)
	}
	return nil
}

// ---------------------------------------------------------------------------
// upload
// ---------------------------------------------------------------------------

func newUploadCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upload",
		Short: "清洗源站 .torrent 并上传到目标站(nexusphp/mteam/nexusphp_classic)",
		RunE:  runUpload,
	}
	cmd.Flags().String("torrent", "", "源站 .torrent 文件路径(将被清洗)(必填)")
	cmd.Flags().String("target", "", "目标站类型(必填)")
	cmd.Flags().String("config", "", "站点配置 JSON(含 token/passkey;优先于环境变量)")
	cmd.Flags().String("category", "", "分类名/ID(如 '电影' 或 401);缺省时尝试从种子/RSS 推断")
	cmd.Flags().Int("fallback-category", 0, "分类推断失败时的兜底分类 ID")
	cmd.Flags().String("title", "", "覆盖标题(name)")
	cmd.Flags().String("descr", "", "覆盖简介(descr)")
	cmd.Flags().StringArray("extra", nil, "额外表单字段 key=value,可多次(如 --extra small_descr=xx)")
	cmd.Flags().Bool("dry-run", false, "只打印将发送的字段/URL/头,不发任何请求")
	cmd.Flags().String("out", "samples", "dry-run 产物输出目录(默认 samples)")
	cmd.Flags().String("workdir", "", "临时目录(默认系统临时目录);留此目录便于人工核对")
	return cmd
}

func runUpload(cmd *cobra.Command, args []string) error {
	torrentPath, _ := cmd.Flags().GetString("torrent")
	target, _ := cmd.Flags().GetString("target")
	configJSON, _ := cmd.Flags().GetString("config")
	category, _ := cmd.Flags().GetString("category")
	fallback, _ := cmd.Flags().GetInt("fallback-category")
	title, _ := cmd.Flags().GetString("title")
	descr, _ := cmd.Flags().GetString("descr")
	extras, _ := cmd.Flags().GetStringArray("extra")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	out, _ := cmd.Flags().GetString("out")
	workdir, _ := cmd.Flags().GetString("workdir")

	if torrentPath == "" {
		return fmt.Errorf("--torrent 必填")
	}
	if target == "" {
		return fmt.Errorf("--target 必填(可选: %s)", strings.Join(targets.TargetTypes(), ", "))
	}

	cfg := targetCfgFromEnv()
	cfg["target"] = target
	if configJSON != "" {
		b, err := os.ReadFile(configJSON)
		if err != nil {
			return fmt.Errorf("读取配置 JSON 失败: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("解析配置 JSON 失败: %v", err)
		}
		for k, v := range m {
			cfg[k] = v
		}
	}
	if category != "" {
		cfg["category"] = category
	}
	if fallback != 0 {
		cfg["fallback_category"] = fallback
	}
	for _, e := range extras {
		k, v, ok := strings.Cut(e, "=")
		if !ok {
			return fmt.Errorf("--extra 需要 key=value,得到: %q", e)
		}
		cfg[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}

	parsed, err := parser.ParseTorrent(torrentPath)
	if err != nil {
		return fmt.Errorf("解析种子失败: %v", err)
	}
	meta := map[string]any{"parsed": parsed}
	if title != "" {
		meta["name"] = title
	}
	if descr != "" {
		meta["descr"] = descr
	}

	if dryRun {
		return dryRunUpload(torrentPath, parsed, cfg, meta, out, workdir)
	}

	ok, targetID, targetSite, uerr := targets.Upload(torrentPath, meta, cfg)
	if uerr != nil {
		return fmt.Errorf("上传失败: %v", uerr)
	}
	if ok {
		fmt.Printf("上传成功 target_id=%d site=%s\n", targetID, targetSite)
		return nil
	}
	return fmt.Errorf("上传被拒绝 target_id=%d site=%s", targetID, targetSite)
}

func dryRunUpload(torrentPath string, parsed *parser.ParsedTorrent, cfg, meta map[string]any, out, workdir string) error {
	site, err := targets.NewTargetSite(getStr(cfg, "target", "target_type"), cfg)
	if err != nil {
		return err
	}
	if loader, ok := site.(targets.CategoriesLoader); ok {
		site.SetCategories(loader.LoadCategories())
	}
	// 构造 extra:cfg 覆盖字段 + meta 覆盖(name/descr 等)
	extra := map[string]any{}
	for _, k := range []string{"category", "fallback_category", "small_descr", "smallDescr",
		"url", "imdb", "douban", "source", "medium", "codec", "standard", "processing",
		"team", "audiocodec", "tags", "uplver", "anonymous", "countries", "labels", "mediainfo"} {
		if v, ok := cfg[k]; ok {
			extra[k] = v
		}
	}
	for k, v := range meta {
		if k != "parsed" {
			extra[k] = v
		}
	}
	fields := targets.BuildUploadFields(parsed, site, extra)
	fmt.Printf("=== %s dry-run 字段 ===\n", getStr(cfg, "target"))
	for k, v := range fields {
		fmt.Printf("  %s: %v\n", k, v)
	}
	fmt.Printf("  upload_url = %s\n", site.(interface{ UploadURL() string }).UploadURL())
	if workdir != "" {
		if err := os.MkdirAll(workdir, 0o755); err != nil {
			return err
		}
		b, _ := json.MarshalIndent(fields, "", "  ")
		p := filepath.Join(workdir, fmt.Sprintf("fields_%s.json", truncate(parsed.InfoHash, 12)))
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return err
		}
		fmt.Printf("字段已写入 %s\n", p)
	}
	_ = out
	return nil
}

// ---------------------------------------------------------------------------
// probe
// ---------------------------------------------------------------------------

func newProbeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "probe",
		Short: "站点规格自动探测:架构/分类/上传表单/标题规范(只读)",
		RunE:  runProbe,
	}
	cmd.Flags().String("url", os.Getenv("SITE_URL"), "站点根 URL(优先用环境变量 SITE_URL)")
	cmd.Flags().String("cookie", os.Getenv("SITE_COOKIE"), "登录 cookie(如 access_token=...;SITE_COOKIE 优先)")
	cmd.Flags().String("token", os.Getenv("SITE_TOKEN"), "Bearer token(可选;SITE_TOKEN 优先)")
	cmd.Flags().String("out", "config/local/sites/probe.json", "输出 JSON 路径(默认 local,gitignored)")
	cmd.Flags().Bool("save-passkey", false, "把真实 passkey 写入输出(默认脱敏;仅本地调试用,勿提交)")
	return cmd
}

func runProbe(cmd *cobra.Command, args []string) error {
	url, _ := cmd.Flags().GetString("url")
	cookie, _ := cmd.Flags().GetString("cookie")
	token, _ := cmd.Flags().GetString("token")
	out, _ := cmd.Flags().GetString("out")

	if url == "" {
		return fmt.Errorf("--url 必填(优先用环境变量 SITE_URL)")
	}

	// 简化探测:尝试 NexusPHP API 的 /api/v1/sections(只读)
	site, err := targets.NewTargetSite("nexusphp", map[string]any{"base_url": url, "api_token": token})
	if err != nil {
		return err
	}
	np, ok := site.(*targets.NexusPHPAPI)
	if !ok {
		return fmt.Errorf("内部错误: 无法构造 NexusPHP 探测适配器")
	}
	_ = cookie
	sections := np.GetSections()
	cats := np.Categories()
	fmt.Printf("站点 %s 探测结果:\n", url)
	fmt.Printf("  sections 结构 keys: ")
	keys := make([]string, 0, len(sections))
	for k := range sections {
		keys = append(keys, k)
	}
	fmt.Println(keys)
	fmt.Printf("  分类映射(%d 个):\n", len(cats))
	for name, id := range cats {
		fmt.Printf("    %s → %d\n", name, id)
	}

	probeOut := map[string]any{
		"url":        url,
		"categories": cats,
		"sections":   sections,
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(probeOut, "", "  ")
	if err := os.WriteFile(out, b, 0o644); err != nil {
		return err
	}
	fmt.Printf("探测结果写入 %s\n", out)
	return nil
}

// ---------------------------------------------------------------------------
// qb
// ---------------------------------------------------------------------------

func newQBCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "qb",
		Short: "qBittorrent 联动:login/add-torrent/add-magnet/export/info/stop/start/delete/recheck",
	}
	cmd.PersistentFlags().String("host", os.Getenv("QBHOST"), "qB 服务器地址(环境变量 QBHOST)")
	cmd.PersistentFlags().String("user", os.Getenv("QBUSER"), "qB 用户名(环境变量 QBUSER)")
	cmd.PersistentFlags().String("pass", os.Getenv("QBPASS"), "qB 密码(环境变量 QBPASS)")

	cmd.AddCommand(&cobra.Command{
		Use:   "login",
		Short: "登录 qB",
		RunE: func(c *cobra.Command, args []string) error {
			qbClient, err := qbFromFlags(c)
			if err != nil {
				return err
			}
			return qbClient.Login()
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "info",
		Short: "查询种子列表",
		RunE: func(c *cobra.Command, args []string) error {
			qbClient, err := qbFromFlags(c)
			if err != nil {
				return err
			}
			infos, err := qbClient.Info()
			if err != nil {
				return err
			}
			for _, t := range infos {
				fmt.Printf("%s  %s  %v\n", t["hash"], t["state"], t["name"])
			}
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "export",
		Short: "导出 .torrent 到文件",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			qbClient, err := qbFromFlags(c)
			if err != nil {
				return err
			}
			data, err := qbClient.ExportTorrent(args[0])
			if err != nil {
				return err
			}
			return os.WriteFile(args[1], data, 0o644)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "add-torrent",
		Short: "添加本地 .torrent 到 qB",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			qbClient, err := qbFromFlags(c)
			if err != nil {
				return err
			}
			res, err := qbClient.AddTorrentFile(args[0], qb.AddOptions{})
			if err != nil {
				return err
			}
			fmt.Printf("add 响应: %v\n", res)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "add-magnet",
		Short: "添加 magnet/URL 到 qB",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			qbClient, err := qbFromFlags(c)
			if err != nil {
				return err
			}
			res, err := qbClient.AddTorrentURL(args[0], qb.AddOptions{})
			if err != nil {
				return err
			}
			fmt.Printf("add 响应: %v\n", res)
			return nil
		},
	})
	for _, op := range []struct {
		use  string
		desc string
		call func(*qb.QBittorrent, string) error
	}{
		{"stop", "停止做种", func(q *qb.QBittorrent, h string) error { return q.Stop(h) }},
		{"start", "继续做种", func(q *qb.QBittorrent, h string) error { return q.Start(h) }},
		{"recheck", "重新校验", func(q *qb.QBittorrent, h string) error { return q.Recheck(h) }},
	} {
		op := op
		cmd.AddCommand(&cobra.Command{
			Use:   op.use,
			Short: op.desc,
			Args:  cobra.ExactArgs(1),
			RunE: func(c *cobra.Command, args []string) error {
				qbClient, err := qbFromFlags(c)
				if err != nil {
					return err
				}
				return op.call(qbClient, args[0])
			},
		})
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "delete",
		Short: "删除种子(默认 deleteFiles=false)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			qbClient, err := qbFromFlags(c)
			if err != nil {
				return err
			}
			return qbClient.Delete(args[0], false)
		},
	})
	return cmd
}

func qbFromFlags(c *cobra.Command) (*qb.QBittorrent, error) {
	host, _ := c.Flags().GetString("host")
	user, _ := c.Flags().GetString("user")
	pass, _ := c.Flags().GetString("pass")
	if host == "" || user == "" || pass == "" {
		return nil, fmt.Errorf("qB 凭据缺失:设置 --host/--user/--pass 或环境变量 QBHOST/QBUSER/QBPASS")
	}
	return qb.NewQBittorrent(host, user, pass, 30)
}

func newQBFromEnv() *qb.QBittorrent {
	host := os.Getenv("QBHOST")
	user := os.Getenv("QBUSER")
	pass := os.Getenv("QBPASS")
	if host == "" || user == "" || pass == "" {
		return nil
	}
	client, err := qb.NewQBittorrent(host, user, pass, 30)
	if err != nil {
		return nil
	}
	return client
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

func filterHits(items []source.RssItem, keywords []string) []source.RssItem {
	var hits []source.RssItem
	for i := range items {
		if items[i].MatchesKeywords(keywords) {
			hits = append(hits, items[i])
		}
	}
	return hits
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func getStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
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

func printResult(result map[string]any) {
	b, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(b))
}

func envOrInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envOrFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
