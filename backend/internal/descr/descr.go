// Package descr 上传简介(descr)HTML 构造模块。
//
// 职责:把源站 RSS 的完整简介 HTML 复用为目标站上传的 `descr` 字段。
// 源站 RSS 的 description 已是渲染后的完整 HTML(含 IMDb/豆瓣/TMDB 链接、
// 海报图、剧情等),可直接复用;本模块负责:
//
//   - NormalizeDescription:结构规范化(去 source 站 logo/脚本/多余空行,保守保留正文/图片/链接)
//   - StripSourceReferences:移除"来自/发布自/转自 XX 站"等引用文字(已知模式,保守)
//   - ExtractSections:从 description 提取 片名/别名/年代/产地/类别/语言/
//     片长/导演/主演/剧情/IMDb/豆瓣 等区块(若结构清晰)
//   - BuildDescription:低层组装:小描述头部 + 正文 + 文件列表 + 自定义区块
//   - DescrBuilder:顶层入口,可配置模板/是否附文件列表/是否附小描述
//
// 约束:纯 stdlib,不联网。保守策略:不删可能重要的内容;拿不准就保留。
package descr

import (
	"html"
	"regexp"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// 常量
// ---------------------------------------------------------------------------

// 已知"来源引用"短语(去 source 站引用时匹配)。
var siteDomainRe = `[A-Za-z0-9][A-Za-z0-9.\-]*\.[A-Za-z]{2,}`
var siteNameCNRe = `[\x{4e00}-\x{9fa5}A-Za-z0-9]{1,20}?(?:站|论坛|发布组|发布|小组|站点)`

// refStrongRe 强引用词:发布自/转自/转载自/出自/来源:
var refStrongRe = regexp.MustCompile(`(?i)(?:发布自|转自|转载自|出自|来源[:：])\s*[:：]?\s*[^\n<]{1,80}`)

// refFromRe "来自" 只有后面紧跟"像站点名"时才算引用。
var refFromRe = regexp.MustCompile(`(?i)来自\s*[:：]?\s*(?:` + siteDomainRe + `|` + siteNameCNRe + `)[^\n<]{0,80}`)

// 源站 logo/banner 图(仅删这类明显是站点标识的图,保留内容图)。
var logoImgRe = regexp.MustCompile(`(?i)<img\b[^>]*\bsrc=["'][^"']*(?:logo|sitelogo|site_logo|/banner|banner\.)[^"']*["'][^>]*>`)

var scriptTagRe = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>`)
var styleTagRe = regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style>`)

// imdbIDRe IMDb 标题 ID 白名单(先校验再嵌入,杜绝注入)。
var imdbIDRe = regexp.MustCompile(`^tt\d{6,}$`)

// 对外部 HTML 的基础安全清洗:去掉事件属性(on*=)与 javascript: 伪协议 URL。
var eventAttrRe = regexp.MustCompile(`(?i)\son[a-zA-Z][a-zA-Z0-9_-]*\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>"']*)`)
var jsURLAttrRe = regexp.MustCompile(`(?i)\s(?:href|src|xlink:href|action|formaction|poster|background)\s*=\s*["']?\s*javascript:[^"'\s>]*["']?`)

// lineLabels 剧情/IMDb/豆瓣 等区块在文本里的标签样式。
var lineLabels = []struct{ key, label string }{
	{"chinese_title", "中文片名"},
	{"title", "片名"}, // 原 Python 为 (?<!中文)片名,此处特殊处理
	{"alias", "别名|又名"},
	{"year", "年代"},
	{"release_date", "上映日期"},
	{"country", "国家|地区|产地"},
	{"genre", "类别|类型"},
	{"language", "对白语言|语言"},
	{"runtime", "片长|时长"},
	{"rating", "评分"},
	{"votes", "票数"},
	{"director", "导演"},
	{"writer", "编剧"},
	{"cast", "主演|演出"},
	{"imdb_link", "IMDb链接|IMDb"},
	{"douban_link", "豆瓣链接|豆瓣"},
}

// plotRe 剧情:从标签起跨行,直到空行或文本结束。
var plotRe = regexp.MustCompile(`(?s)剧情概要[:：]?\s*(.*?)(?:\n\s*\n|\z)`)

// ---------------------------------------------------------------------------
// HTML → 文本(纯 stdlib)
// ---------------------------------------------------------------------------

var blockTags = map[string]bool{
	"br": true, "p": true, "div": true, "tr": true, "td": true, "li": true,
	"fieldset": true, "legend": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "table": true, "ul": true, "ol": true, "dl": true,
	"dt": true, "dd": true, "pre": true, "hr": true, "blockquote": true,
}

// htmlToText 把 HTML 转成可读文本(保留空行分隔,便于正则按行提取)。
// 丢弃 script/style 内容;块级标签转换为换行。
func htmlToText(htmlSrc string) string {
	var parts []byte
	skipDepth := 0
	i := 0
	n := len(htmlSrc)
	for i < n {
		if htmlSrc[i] == '<' {
			// 注释
			if i+3 < n && htmlSrc[i+1] == '!' && htmlSrc[i+2] == '-' && htmlSrc[i+3] == '-' {
				end := strings.Index(htmlSrc[i+4:], "-->")
				if end < 0 {
					break
				}
				i += 4 + end + 3
				continue
			}
			end := strings.IndexByte(htmlSrc[i:], '>')
			if end < 0 {
				break
			}
			tag := htmlSrc[i+1 : i+end]
			trimmed := strings.TrimSpace(tag)
			isEnd := strings.HasPrefix(trimmed, "/")
			tagName := trimmed
			if isEnd {
				tagName = strings.TrimPrefix(trimmed, "/")
			}
			// 去掉属性
			if sp := strings.IndexAny(tagName, " \t\r\n"); sp >= 0 {
				tagName = tagName[:sp]
			}
			low := strings.ToLower(tagName)
			if isEnd {
				if skipDepth > 0 {
					if low == "script" || low == "style" {
						skipDepth--
					}
				} else if blockTags[low] {
					parts = append(parts, '\n')
				}
			} else {
				if low == "script" || low == "style" {
					skipDepth++
				} else if skipDepth == 0 && blockTags[low] {
					parts = append(parts, '\n')
				}
			}
			i += end + 1
			continue
		}
		next := strings.IndexByte(htmlSrc[i:], '<')
		if next < 0 {
			next = n - i
		}
		if skipDepth == 0 {
			parts = append(parts, htmlSrc[i:i+next]...)
		}
		i += next
	}
	raw := html.UnescapeString(string(parts))
	lines := strings.Split(raw, "\n")
	var cleaned []string
	prevBlank := false
	for _, ln := range lines {
		ln = strings.TrimSpace(collapseWS(ln))
		if ln != "" {
			cleaned = append(cleaned, ln)
			prevBlank = false
		} else if len(cleaned) > 0 && !prevBlank {
			cleaned = append(cleaned, "")
			prevBlank = true
		}
	}
	return strings.TrimSpace(strings.Join(cleaned, "\n"))
}

func collapseWS(s string) string {
	return wsRe.ReplaceAllString(s, " ")
}

var wsRe = regexp.MustCompile(`\s+`)

// ---------------------------------------------------------------------------
// 公共函数
// ---------------------------------------------------------------------------

// sanitizeEventAttrs 对外部 HTML 做基础安全清洗:去掉事件属性(on*=)与
// javascript: 伪协议 URL,防止注入。
func sanitizeEventAttrs(htmlSrc string) string {
	s := eventAttrRe.ReplaceAllString(htmlSrc, "")
	s = jsURLAttrRe.ReplaceAllString(s, "")
	return s
}

// NormalizeDescription 描述 HTML 结构规范化(保守)。
func NormalizeDescription(htmlSrc string) string {
	s := htmlSrc
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = scriptTagRe.ReplaceAllString(s, "")
	s = styleTagRe.ReplaceAllString(s, "")
	s = logoImgRe.ReplaceAllString(s, "")
	s = sanitizeEventAttrs(s)

	lines := strings.Split(s, "\n")
	var out []string
	blank := 0
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t")
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
			blank = 0
		} else {
			blank++
			if blank <= 2 { // 保留 ≤2 个空行作为段落分隔
				out = append(out, "")
			}
		}
	}
	for len(out) > 0 && strings.TrimSpace(out[0]) == "" {
		out = out[1:]
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

var brRe = regexp.MustCompile(`(?i)<br\s*/?>`)

// StripSourceReferences 移除"来自/发布自/转自 XX 站"类引用文字(已知模式)。
func StripSourceReferences(htmlSrc string) string {
	lines := strings.Split(htmlSrc, "\n")
	out := []string{}
	for _, ln := range lines {
		cut := -1
		if m := refStrongRe.FindStringIndex(ln); m != nil {
			cut = m[0]
		}
		if m := refFromRe.FindStringIndex(ln); m != nil && (cut < 0 || m[0] < cut) {
			cut = m[0]
		}
		if cut >= 0 {
			ln = ln[:cut]
			ln = brRe.ReplaceAllString(ln, "")
			ln = strings.TrimSpace(ln)
			if ln == "" {
				continue // 整行都是引用,直接丢弃
			}
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

// searchInlineLabel 在文本里找 "label[:：] 值";若 lookbehind 非空,则
// 拒绝紧跟该前缀的匹配(用于 title 标签避开 "中文片名")。
func searchInlineLabel(text, label, lookbehind string) (string, bool) {
	re := regexp.MustCompile(`(?:` + label + `)[:：]?\s*([^\n]+)`)
	searchFrom := 0
	for {
		rel := re.FindStringSubmatchIndex(text[searchFrom:])
		if rel == nil {
			return "", false
		}
		absStart := searchFrom + rel[0]
		if lookbehind != "" && absStart >= len(lookbehind) &&
			text[absStart-len(lookbehind):absStart] == lookbehind {
			searchFrom = absStart + 1
			continue
		}
		val := strings.TrimSpace(text[searchFrom+rel[2] : searchFrom+rel[3]])
		if val == "" {
			searchFrom = absStart + 1
			continue
		}
		return val, true
	}
}

// ExtractSections 从 description 提取结构化区块(结构清晰时)。
func ExtractSections(htmlSrc string) map[string]any {
	text := htmlToText(htmlSrc)
	result := map[string]any{}

	// 行内字段
	for _, ll := range lineLabels {
		lb := ""
		if ll.key == "title" {
			lb = "中文"
		}
		val, ok := searchInlineLabel(text, ll.label, lb)
		if ok {
			result[ll.key] = val
			if ll.key == "year" {
				if m := regexp.MustCompile(`(\d{4})`).FindStringSubmatch(val); m != nil {
					result["year_num"] = m[1]
				}
			}
		}
	}

	// 剧情(跨行)
	if m := plotRe.FindStringSubmatch(text); m != nil {
		plot := wsRe.ReplaceAllString(m[1], " ")
		plot = strings.TrimSpace(plot)
		if plot != "" {
			result["plot"] = plot
		}
	}

	// 附件信息:图片 / IMDb ttID / 豆瓣 id / 链接
	imgRe := regexp.MustCompile(`(?i)<img\b[^>]*\bsrc=["']([^"']+)["']`)
	if imgs := imgRe.FindAllStringSubmatch(htmlSrc, -1); len(imgs) > 0 {
		var images []string
		seen := map[string]bool{}
		for _, m := range imgs {
			if len(m) > 1 && !seen[m[1]] {
				seen[m[1]] = true
				images = append(images, m[1])
			}
		}
		result["images"] = images
	}

	ttRe := regexp.MustCompile(`(tt\d{6,})`)
	var imdbIDs []string
	seenTT := map[string]bool{}
	for _, m := range ttRe.FindAllStringSubmatch(htmlSrc, -1) {
		if !seenTT[m[1]] {
			seenTT[m[1]] = true
			imdbIDs = append(imdbIDs, m[1])
		}
	}
	if len(imdbIDs) > 0 {
		result["imdb"] = imdbIDs[0]
		result["imdb_ids"] = imdbIDs
	}

	if m := regexp.MustCompile(`(?i)douban\.com/(?:subject/)?(\d+)`).FindStringSubmatch(htmlSrc); m != nil {
		result["douban_id"] = m[1]
	}

	aRe := regexp.MustCompile(`(?i)<a\b[^>]*\bhref=["']([^"']+)["']`)
	var links []string
	seenLink := map[string]bool{}
	for _, m := range aRe.FindAllStringSubmatch(htmlSrc, -1) {
		if len(m) > 1 && !seenLink[m[1]] {
			seenLink[m[1]] = true
			links = append(links, m[1])
		}
	}
	if len(links) > 0 {
		result["links"] = links
	}

	result["raw_text"] = text
	return result
}

// ---------------------------------------------------------------------------
// 组装
// ---------------------------------------------------------------------------

// humanSize 字节数 → 可读大小(B/KiB/MiB/GiB...)。
func humanSize(num any) string {
	var n float64
	switch v := num.(type) {
	case float64:
		n = v
	case float32:
		n = float64(v)
	case int:
		n = float64(v)
	case int64:
		n = float64(v)
	case uint64:
		n = float64(v)
	case uint:
		n = float64(v)
	case string:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return v
		}
		n = f
	default:
		if v != nil {
			return strconv.FormatFloat(n, 'f', -1, 64)
		}
		return ""
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	for i, unit := range units {
		if n < 0 {
			n = -n
		}
		if absf(n) < 1024.0 || unit == "PiB" {
			if unit == "B" {
				return strconv.Itoa(int(n)) + " B"
			}
			return strconv.FormatFloat(n, 'f', 2, 64) + " " + unit
		}
		n /= 1024.0
		_ = i
	}
	return strconv.FormatFloat(n, 'f', 2, 64)
}

func absf(n float64) float64 {
	if n < 0 {
		return -n
	}
	return n
}

// getStr 从 map 取字符串字段。
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

// RenderFileList 文件列表 → HTML。
// style="table":<fieldset><legend>文件列表</legend><table>...</table></fieldset>
// style="code" :<fieldset><legend>文件列表</legend><pre>...</pre></fieldset>
func RenderFileList(fileList []map[string]any, style string) string {
	if len(fileList) == 0 {
		return ""
	}
	var rows []string
	if style == "code" {
		var lines []string
		for _, f := range fileList {
			path := strAny(f["path"])
			size := humanSize(f["size"])
			lines = append(lines, path+"  "+size)
		}
		rows = append(rows, "<fieldset><legend>文件列表</legend>")
		rows = append(rows, "<pre>"+html.EscapeString(strings.Join(lines, "\n"))+"</pre>")
		rows = append(rows, "</fieldset>")
	} else { // table(默认,兼容 NexusPHP 系站点常见样式)
		rows = append(rows,
			`<fieldset><legend>文件列表</legend>`+
				`<table class="filelist" cellspacing="0" cellpadding="3" border="0">`)
		rows = append(rows, `<tr><td><b>文件名</b></td><td align="right"><b>大小</b></td></tr>`)
		for _, f := range fileList {
			rows = append(rows,
				"<tr><td>"+html.EscapeString(strAny(f["path"]))+
					"</td><td align=\"right\">"+
					html.EscapeString(humanSize(f["size"]))+
					"</td></tr>")
		}
		rows = append(rows, "</table></fieldset>")
	}
	return strings.Join(rows, "\n")
}

func strAny(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return strconv.FormatFloat(toFloat(v), 'f', -1, 64)
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case uint64:
		return float64(n)
	case uint:
		return float64(n)
	}
	return 0
}

// BuildDescription 构造上传简介 HTML(低层组装)。
func BuildDescription(bodyHTML string, fileList []map[string]any, smallDescr string, extraSections [][2]string) string {
	var parts []string
	if smallDescr != "" {
		parts = append(parts,
			`<fieldset><legend>简介</legend>`+html.EscapeString(smallDescr)+`</fieldset>`)
	}
	if bodyHTML != "" {
		parts = append(parts, bodyHTML)
	}
	if len(fileList) > 0 {
		parts = append(parts, RenderFileList(fileList, "table"))
	}
	for _, sec := range extraSections {
		label, value := sec[0], sec[1]
		if value == "" {
			continue
		}
		parts = append(parts,
			`<fieldset><legend>`+html.EscapeString(label)+`</legend>`+value+`</fieldset>`)
	}
	return strings.Join(parts, "\n")
}

// ---------------------------------------------------------------------------
// 顶层构造器
// ---------------------------------------------------------------------------

// DescrBuilder 可配置的简介构造器(顶层入口)。
type DescrBuilder struct {
	// Template 为含 `{descr}` 占位符的字符串模板;为空则不应用。
	Template string
	// TemplateFunc 可选后处理钩子;优先于 Template。
	TemplateFunc func(string) string

	IncludeFileList  bool
	AttachSmallDescr bool
	FileListStyle    string
	StripReferences  bool
	Normalize        bool
	// ExtraSections 固定附加区块;为 nil 时由 Build 自动推断。
	ExtraSections [][2]string
}

// NewDescrBuilder 构造默认配置的 DescrBuilder。
func NewDescrBuilder() *DescrBuilder {
	return &DescrBuilder{
		IncludeFileList:  true,
		AttachSmallDescr: true,
		FileListStyle:    "table",
		StripReferences:  true,
		Normalize:        true,
	}
}

// Build 构造上传简介。缺 titler / 缺 parsed 时自动降级,绝不抛错。
func (b *DescrBuilder) Build(itemFields map[string]any, parsed map[string]any) string {
	if itemFields == nil {
		itemFields = map[string]any{}
	}
	if parsed == nil {
		parsed = map[string]any{}
	}

	rawDesc := getStr(itemFields, "description", "descr")
	if rawDesc == "" {
		rawDesc = getStr(parsed, "description")
	}
	small := getStr(itemFields, "small_descr")
	if small == "" {
		small = getStr(parsed, "small_descr")
	}
	title := getStr(itemFields, "title")
	if title == "" {
		title = getStr(parsed, "name")
	}
	// titler.build_title 在 Python 中也不存在,始终用原始标题兜底。
	title = resolveTitle(title)

	body := rawDesc
	if b.StripReferences {
		body = StripSourceReferences(body)
	}
	if b.Normalize {
		body = NormalizeDescription(body)
	}

	// 文件列表
	var fileList []map[string]any
	var fileListText string
	if b.IncludeFileList {
		fileList = asFileList(parsed["files"])
		if len(fileList) == 0 {
			if s, ok := parsed["file_list_text"].(string); ok && s != "" {
				fileListText = s
			}
		}
	}

	extra := b.resolveExtraSections(itemFields, parsed, body)

	// parsed 只给了 file_list_text(未给 files)时,用 pre 代码块呈现
	if fileList == nil && fileListText != "" {
		extra = append([][2]string{{"文件列表", "<pre>" + html.EscapeString(fileListText) + "</pre>"}}, extra...)
	}

	smallDescr := ""
	if b.AttachSmallDescr {
		smallDescr = small
	}
	out := BuildDescription(body, fileList, smallDescr, extra)

	if b.TemplateFunc != nil {
		out = b.TemplateFunc(out)
	} else if b.Template != "" {
		out = strings.ReplaceAll(b.Template, "{descr}", out)
	}
	_ = title
	return out
}

func resolveTitle(fallback string) string {
	return fallback
}

func anyMaps(files []any) []map[string]any {
	out := make([]map[string]any, 0, len(files))
	for _, f := range files {
		if m, ok := f.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// asFileList 把 files 字段转成 []map[string]any,兼容 []any 与 []map[string]any。
func asFileList(v any) []map[string]any {
	switch files := v.(type) {
	case []any:
		return anyMaps(files)
	case []map[string]any:
		return files
	}
	return nil
}

func (b *DescrBuilder) resolveExtraSections(itemFields, parsed map[string]any, body string) [][2]string {
	if b.ExtraSections != nil {
		return b.ExtraSections
	}
	var extra [][2]string

	// IMDb:先 `tt\d{6,}` 白名单校验并 html 转义,杜绝注入。
	imdb := getStr(itemFields, "imdb")
	if imdb != "" {
		imdb = strings.ToLower(strings.TrimSpace(imdb))
		if imdbIDRe.MatchString(imdb) {
			imdbEsc := html.EscapeString(imdb)
			if !regexp.MustCompile(`(?i)` + regexp.QuoteMeta(imdb)).MatchString(body) {
				extra = append(extra, [2]string{
					"IMDb",
					`<a href="https://www.imdb.com/title/` + imdbEsc + `/" target="_blank">` + imdbEsc + `</a>`,
				})
			}
		}
	}

	// 大小 / 文件数
	total := parsed["total_size"]
	count := parsed["file_count"]
	hasFiles := len(asFileList(parsed["files"])) > 0
	var infoParts []string
	if count != nil {
		if c, ok := count.(int); ok && c > 0 {
			infoParts = append(infoParts, strconv.Itoa(c)+" 个文件")
		}
	}
	if total != nil {
		if s := humanSize(total); s != "" {
			infoParts = append(infoParts, s)
		}
	}
	if len(infoParts) > 0 && !hasFiles {
		// 已有文件列表时不再重复大小信息
		extra = append(extra, [2]string{"文件信息", strings.Join(infoParts, " / ")})
	}
	return extra
}
