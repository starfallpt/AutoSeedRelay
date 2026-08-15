// Package titler 种子标题结构化解析 — 纯函数,不联网。
//
// 把 PT 站点标题(如 `Zhang Ga the Soldier Boy 1963 1440p WEB-DL H.265
// DDP 2.0 2Audio-LongWeb`)拆成结构化组件:主标题 / 季集 / 年份 / 分辨率 /
// 片源 / 介质 / 视频编码 / 音频编码 / HDR / 声道 / 位深 / 制作组等。
//
// 解析基于"可扩展正则组件表 + 逐项剥离 + 兜底逻辑":
//  1. 结尾剥离制作组(`-LongWeb` / `_UBWEB` 等,已识别组件结尾不算制作组);
//  2. 季集(S01E02 / S01E07-S01E08 / Season 1 / 第1集);
//  3. 组件正则表按序逐个剥离并记录:年份 → 分辨率 → HDR → 视频编码 →
//     音频编码 → 声道 → 位深 → 片源/介质 → Complete → 版本噪声;
//  4. 剩余文本清理为主标题(全被剥离时回退到原始标题)。
//
// 注意:Go 的 regexp(RE2)不支持 lookbehind / lookahead,原 Python 中的
// 负向断言已改写为"匹配后手工校验边界"的等价形式(见 findBareEP / findComplete)。
package titler

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// TitleComponents 种子标题的结构化组件。
type TitleComponents struct {
	Title       string  // 主标题(剥离版本信息后剩余文本)
	Season      *string // 季,如 "1"、"1-2";无则 nil
	Episode     *string // 集,如 "13"、"7-8";无则 nil
	Year        *string // 4 位年份,如 "1963"
	Resolution  *string // 分辨率,如 "1080p"/"2160p"/"720p"/"4K"
	Source      *string // 片源,如 "WEB-DL"/"BluRay"/"HDTV"
	Medium      *string // 介质,如 "WEB"/"BLURAY"/"REMUX"/"BDMV"
	VideoCodec  *string // 视频编码,如 "HEVC"/"H264"
	AudioCodec  *string // 音频编码,如 "DDP"/"AC3"/"TRUEHD"/"AAC"
	HDR         *string // HDR,如 "HDR10"/"DV"/"HDRVIVID"
	Channels    *string // 声道,如 "2.0"/"5.1"/"2.0 2Audio"
	Bits        *string // 位深,如 "10bit"/"8bit"
	Group       *string // 制作组后缀,如 "LongWeb"/"UBWEB"
	Complete    bool    // 是否为整季/全集(标题含 Complete)
	Raw         string  // 原始标题
}

// strPtr 返回 *string 帮助函数。
func strPtr(s string) *string { return &s }

// 组件 → 标准化键 映射。
var RESOLUTION_KEYS = map[string]string{
	"2160p": "2160", "4K": "2160", "UHD": "2160",
	"1440p": "1440",
	"1080p": "1080", "720p": "720", "576p": "576", "480p": "480",
}

var SOURCE_KEYS = map[string]string{
	"WEB-DL": "WEB-DL", "WEBRip": "WEBRIP", "BluRay": "BLURAY",
	"BDRip": "BDRIP", "HDTV": "HDTV", "DVDRip": "DVDRIP",
	"HDRip": "HDRIP", "PDTV": "PDTV", "BDMV": "BDMV",
}

var MEDIUM_KEYS = map[string]string{
	"WEB": "WEB", "BLURAY": "BLURAY", "REMUX": "REMUX",
	"BDMV": "BDMV", "HDTV": "HDTV", "DVD": "DVD",
}

var VIDEO_CODEC_KEYS = map[string]string{
	"HEVC": "HEVC", "H264": "H264", "MPEG4": "MPEG4",
	"VC-1": "VC-1", "MPEG2": "MPEG2", "XVID": "XVID", "DIVX": "DIVX",
}

var AUDIO_CODEC_KEYS = map[string]string{
	"DDP": "DDP", "AC3": "AC3", "TRUEHD": "TRUEHD",
	"DTS-HD MA": "DTS-HD MA", "DTS-HD": "DTS-HD", "DTS": "DTS",
	"ATMOS": "ATMOS", "FLAC": "FLAC", "AAC": "AAC",
	"LPCM": "LPCM", "PCM": "PCM", "MP3": "MP3", "OPUS": "OPUS",
}

var HDR_KEYS = map[string]string{
	"HDR10+": "HDR10+", "HDR10": "HDR10", "DV": "DV",
	"HDRVIVID": "HDRVIVID", "HDR": "HDR",
}

var CHANNEL_KEYS = map[string]string{
	"2.0": "2.0", "5.1": "5.1", "7.1": "7.1", "6.1": "6.1",
	"4.0": "4.0", "1.0": "1.0", "5.1.4": "5.1.4", "7.1.4": "7.1.4",
	"2Audio": "2AUDIO",
}

// StandardKeys 把解析出的组件映射为目标站常用的标准化键。
func StandardKeys(c TitleComponents) map[string]*string {
	category := "movie"
	if c.Season != nil || c.Episode != nil {
		category = "tv"
	}
	var channels *string
	if c.Channels != nil {
		parts := strings.Fields(*c.Channels)
		for i, t := range parts {
			if v, ok := CHANNEL_KEYS[t]; ok {
				parts[i] = v
			}
		}
		channels = strPtr(strings.Join(parts, " "))
	}
	out := map[string]*string{
		"category": strPtr(category),
		"season":   c.Season,
		"episode":  c.Episode,
		"channels": channels,
	}
	setKey := func(name string, val *string, table map[string]string) {
		if val != nil {
			if v, ok := table[*val]; ok {
				out[name] = strPtr(v)
			}
		}
	}
	setKey("resolution", c.Resolution, RESOLUTION_KEYS)
	setKey("source", c.Source, SOURCE_KEYS)
	setKey("medium", c.Medium, MEDIUM_KEYS)
	setKey("video_codec", c.VideoCodec, VIDEO_CODEC_KEYS)
	setKey("audio_codec", c.AudioCodec, AUDIO_CODEC_KEYS)
	setKey("hdr", c.HDR, HDR_KEYS)
	return out
}

// ---------------------------------------------------------------------------
// 正则组件表(可扩展:新组件 = 新增正则 + 新增 apply 函数,挂到 _COMPONENT_RULES)
// ---------------------------------------------------------------------------

var yearRe = regexp.MustCompile(`\b(19\d{2}|20\d{2})\b`)

var resolutionRe = regexp.MustCompile(`(?i)\b(2160p|1440p|1080p|720p|576p|480p)\b|\b(4K|UHD)\b`)

var hdrRe = regexp.MustCompile(
	`(?i)\bDolby\s*Vision\b|\bHDR10Plus\b|\bHDR10\+|\bHDR10\b|\bHDRVivid\b|\bHDR\s*Vivid\b|\bDoVi\b|\bDV\b|\bHDR\b`,
)

var videoRe = regexp.MustCompile(
	`(?i)\bH\.?265\b|\bHEVC\b|\bx265\b|\bH\.?264\b|\bx264\b|\bAVC\b|\bMPEG-?4\b|\bVC-?1\b|\bMPEG-?2\b|\bXviD\b|\bDivX\b`,
)

var audioRe = regexp.MustCompile(
	`(?i)\bDolby\s*TrueHD\b|\bTrueHD\b|\bDolby\s*Digital\s*Plus\b|\bDDP\b|\bE-?AC-?3\b|\bAC-?3\b|\bDTS-?HD\s*MA\b|\bDTS-?HD\b|\bDTS\b|\bDolby\s*Atmos\b|\bAtmos\b|\bFLAC\b|\bAAC\b|\bLPCM\b|\bPCM\b|\bMP3\b|\bOpus\b|\bVorbis\b`,
)

var channelsRe = regexp.MustCompile(
	`(?i)\b7\.1\.4\b|\b5\.1\.4\b|\b7\.1\b|\b5\.1\b|\b6\.1\b|\b4\.0\b|\b2\.0\b|\b1\.0\b|\b2Audio\b`,
)

var bitsRe = regexp.MustCompile(`\b(?:10|8|12)-?[bB]it\b`)

var sourceRe = regexp.MustCompile(
	`(?i)\bWEB-?DL\b|\bWEB-?Rip\b|\bBlu-?Ray\b|\bHD-?TV\b|\bDVD-?Rip\b|\bBD-?Rip\b|\bHDRip\b|\bPDTV\b|\bBDMV\b`,
)

// 注意:WEB 大小写敏感,避免误吞标题里的普通单词 "Web"。
var mediumRe = regexp.MustCompile(`\bREMUX\b|\bWEB\b`)

var completeWhitespaceRe = regexp.MustCompile(`(?i)(\s+Complete)`)
var completeCnRe = regexp.MustCompile(`全集`)

// 版本噪声:剥离但不记录。
var noiseRe = regexp.MustCompile(
	`(?i)\b\d{2,3}\s?[Ff]ps\b|\bDSNP\b|\bMAXPLUS\b|\bVMAX\b|\bCR\b|\bI[nN]TERNAL\b|\bREPACK\b|\bPROPER\b|\bEXTENDED\s*CUT\b|\bEXTENDED\b|\bUNCUT\b|\bUNRATED\b|\bRemastered\b|\bRestored\b|\b(?:The\s+)?Criterion\s+Collection\b|\bDirector'?s\s+Cut\b|\bTheatrical\s+Cut\b|\bIMAX\b`,
)

// 季集:多种约定(顺序敏感,长的/具体的在前)。
// Go 用命名组 (?P<name>...) 与 Python (?P<name>...) 对齐。
var sePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)S(?P<s>\d{1,2})E(?P<e>\d{1,3})(?:-S(?P<s2>\d{1,2})E(?P<e2>\d{1,3}))?`),
	regexp.MustCompile(`(?i)S(?P<s>\d{1,2})E(?P<e>\d{1,3})(?:-E(?P<e2>\d{1,3}))?`),
	regexp.MustCompile(`(?i)\bS(?P<s>\d{1,2})\s*-\s*S(?P<s2>\d{1,2})\b`),
	regexp.MustCompile(`(?i)Season\s+(?P<s>\d{1,2})\s*-\s*(?P<s2>\d{1,2})`),
	regexp.MustCompile(`(?i)\bS(?P<s>\d{1,2})\b`),
	regexp.MustCompile(`(?i)Season\s+(?P<s>\d{1,2})`),
	regexp.MustCompile(`(?i)Episode\s+(?P<e>\d{1,3})`),
	regexp.MustCompile(`(?i)EP?(?P<e>\d{1,3})`),
	regexp.MustCompile(`第(?P<e>\d{1,4})集`),
	regexp.MustCompile(`第(?P<s>\d{1,4})季`),
}

// bareEPRe 是第 8 条季集模式(Python 原文为
// (?<![A-Za-z0-9])EP?(?P<e>\d{1,3})(?![A-Za-z0-9])),这里先裸匹配再由
// findBareEP 校验边界。
var bareEPRe = sePatterns[7]

// 结尾制作组:[-_.] 分隔 + 字母开头。
// 注意分隔符不含空格 —— 空格分隔的是标题单词,不是制作组。
var groupRe = regexp.MustCompile(`[-_.]+([A-Za-z][A-Za-z0-9._\-]{1,24})$`)

// 已知组件 token(结尾出现这些时不算制作组)。
var knownComponentTokens = map[string]bool{
	"H264": true, "H265": true, "HEVC": true, "AVC": true, "X264": true, "X265": true,
	"MPEG4": true, "MPEG2": true, "VC1": true, "XVID": true, "DIVX": true,
	"AAC": true, "DDP": true, "AC3": true, "EAC3": true, "TRUEHD": true,
	"DTS": true, "DTSHD": true, "DTSHDMA": true, "LPCM": true, "PCM": true,
	"FLAC": true, "MP3": true, "OPUS": true, "ATMOS": true,
	"WEB": true, "WEBDL": true, "WEBRIP": true, "BLURAY": true, "REMUX": true,
	"BDMV": true, "HDTV": true, "DVDRIP": true, "BDRIP": true, "HDRIP": true,
	"PDTV": true, "HDR": true, "HDR10": true, "HDR10PLUS": true, "DV": true,
	"DOVI": true, "HDRVIVID": true, "COMPLETE": true, "EXTENDED": true,
	"UNRATED": true, "UNCUT": true, "IMAX": true, "CR": true, "DSNP": true,
	"MAXPLUS": true, "REPACK": true, "PROPER": true, "INTERNAL": true,
}

// 结尾常见非制作组单词(大小写不敏感)。
var nonGroupSuffixes = map[string]bool{
	"complete": true, "extended": true, "unrated": true, "uncut": true,
	"remastered": true, "restored": true, "proper": true, "repack": true,
	"internal": true, "v2": true, "final": true, "theatrical": true,
	"director": true, "directors": true, "edition": true, "version": true,
	"cut": true, "collection": true,
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// findAndBlank 返回把所有匹配区间替换为等长空格后的文本,以及匹配区间列表。
// 注意:匹配区间索引指向**原文本**,应用函数需用原始文本提取内容。
func findAndBlank(work string, re *regexp.Regexp) (string, [][]int) {
	locs := re.FindAllStringIndex(work, -1)
	if len(locs) > 0 {
		b := []byte(work)
		for _, loc := range locs {
			for i := loc[0]; i < loc[1]; i++ {
				b[i] = ' '
			}
		}
		work = string(b)
	}
	return work, locs
}

// findBareEP 模拟 Python 的 (?<![A-Za-z0-9])EP?\d{1,3}(?![A-Za-z0-9])。
func findBareEP(work string) (string, [][]int) {
	var locs [][]int
	b := []byte(work)
	for _, loc := range bareEPRe.FindAllStringIndex(work, -1) {
		s, e := loc[0], loc[1]
		if s > 0 && isAlnumByte(work[s-1]) {
			continue
		}
		if e < len(work) && isAlnumByte(work[e]) {
			continue
		}
		for i := s; i < e; i++ {
			b[i] = ' '
		}
		locs = append(locs, []int{s, e})
	}
	return string(b), locs
}

// findComplete 模拟 Python 的 (?<=\S)\s+\bComplete\b 与 \b全集\b。
func findComplete(work string) (string, [][]int) {
	var locs [][]int
	b := []byte(work)
	for _, loc := range completeWhitespaceRe.FindAllStringIndex(work, -1) {
		s, e := loc[0], loc[1]
		// 匹配起点之前的字符必须是非空白(即 \S)
		if s > 0 && !isSpaceByte(work[s-1]) {
			for i := s; i < e; i++ {
				b[i] = ' '
			}
			locs = append(locs, []int{s, e})
		}
	}
	for _, loc := range completeCnRe.FindAllStringIndex(work, -1) {
		s, e := loc[0], loc[1]
		for i := s; i < e; i++ {
			b[i] = ' '
		}
		locs = append(locs, []int{s, e})
	}
	return string(b), locs
}

func isAlnumByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

var collapseRe = regexp.MustCompile(`\s+`)

func collapse(s string) string {
	s = collapseRe.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	s = strings.Trim(s, " .-_")
	return strings.TrimSpace(collapseRe.ReplaceAllString(s, " "))
}

// ---------------------------------------------------------------------------
// 归一化函数(原始 token → 规范值)
// ---------------------------------------------------------------------------

var (
	reVideoHEVC  = regexp.MustCompile(`(?i)^(?:H\.?265|HEVC|x265)`)
	reVideoH264  = regexp.MustCompile(`(?i)^(?:H\.?264|x264|AVC)`)
	reVideoMP4   = regexp.MustCompile(`(?i)^MPEG-?4`)
	reVideoVC1   = regexp.MustCompile(`(?i)^VC-?1`)
	reVideoMP2   = regexp.MustCompile(`(?i)^MPEG-?2`)
	reVideoXVID  = regexp.MustCompile(`(?i)^XviD`)
	reVideoDIVX  = regexp.MustCompile(`(?i)^DivX`)
	reAudioDDP   = regexp.MustCompile(`(?i)^Dolby\s*Digital\s*Plus|^DDP|^E-?AC-?3`)
	reAudioAC3   = regexp.MustCompile(`(?i)^AC-?3`)
	reAudioTHD   = regexp.MustCompile(`(?i)^Dolby\s*TrueHD|^TrueHD`)
	reAudioDTSMA = regexp.MustCompile(`(?i)^DTS-?HD\s*MA`)
	reAudioDTSHD = regexp.MustCompile(`(?i)^DTS-?HD`)
	reAudioDTS   = regexp.MustCompile(`(?i)^DTS`)
	reAudioATMOS = regexp.MustCompile(`(?i)^Dolby\s*Atmos|^Atmos`)
	reAudioFLAC  = regexp.MustCompile(`(?i)^FLAC`)
	reAudioAAC   = regexp.MustCompile(`(?i)^AAC`)
	reAudioLPCM  = regexp.MustCompile(`(?i)^LPCM`)
	reAudioPCM   = regexp.MustCompile(`(?i)^PCM`)
	reAudioMP3   = regexp.MustCompile(`(?i)^MP3`)
	reAudioOpus  = regexp.MustCompile(`(?i)^Opus`)
	reBits       = regexp.MustCompile(`(?i)^(10|8|12)-?bit`)
	reSSeason    = regexp.MustCompile(`(?i)^S\d{1,3}`)
)

func videoNormalize(t string) string {
	switch {
	case reVideoHEVC.MatchString(t):
		return "HEVC"
	case reVideoH264.MatchString(t):
		return "H264"
	case reVideoMP4.MatchString(t):
		return "MPEG4"
	case reVideoVC1.MatchString(t):
		return "VC-1"
	case reVideoMP2.MatchString(t):
		return "MPEG2"
	case reVideoXVID.MatchString(t):
		return "XVID"
	case reVideoDIVX.MatchString(t):
		return "DIVX"
	}
	return t
}

func audioNormalize(t string) string {
	switch {
	case reAudioDDP.MatchString(t):
		return "DDP"
	case reAudioAC3.MatchString(t):
		return "AC3"
	case reAudioTHD.MatchString(t):
		return "TRUEHD"
	case reAudioDTSMA.MatchString(t):
		return "DTS-HD MA"
	case reAudioDTSHD.MatchString(t):
		return "DTS-HD"
	case reAudioDTS.MatchString(t):
		return "DTS"
	case reAudioATMOS.MatchString(t):
		return "ATMOS"
	case reAudioFLAC.MatchString(t):
		return "FLAC"
	case reAudioAAC.MatchString(t):
		return "AAC"
	case reAudioLPCM.MatchString(t):
		return "LPCM"
	case reAudioPCM.MatchString(t):
		return "PCM"
	case reAudioMP3.MatchString(t):
		return "MP3"
	case reAudioOpus.MatchString(t):
		return "OPUS"
	}
	return t
}

var (
	reHDRDV   = regexp.MustCompile(`(?i)Dolby\s*Vision|DoVi|\bDV\b`)
	reHDRPlus = regexp.MustCompile(`(?i)HDR10Plus|HDR10\+`)
	reHDR10   = regexp.MustCompile(`(?i)HDR10`)
	reHDRViv  = regexp.MustCompile(`(?i)HDRVivid|HDR\s*Vivid`)
	reHDR     = regexp.MustCompile(`(?i)HDR`)
)

func hdrNormalize(t string) string {
	switch {
	case reHDRDV.MatchString(t):
		return "DV"
	case reHDRPlus.MatchString(t):
		return "HDR10+"
	case reHDR10.MatchString(t):
		return "HDR10"
	case reHDRViv.MatchString(t):
		return "HDRVIVID"
	case reHDR.MatchString(t):
		return "HDR"
	}
	return t
}

func bitsNormalize(t string) string {
	m := reBits.FindStringSubmatch(t)
	if m != nil {
		return m[1] + "bit"
	}
	return t
}

// sourceNormalize 片源 token → (source, medium)。
var (
	reSrcWEB    = regexp.MustCompile(`(?i)^WEB-?DL`)
	reSrcWEBRip = regexp.MustCompile(`(?i)^WEB-?Rip`)
	reSrcBluRay = regexp.MustCompile(`(?i)^Blu-?Ray`)
	reSrcHDTV   = regexp.MustCompile(`(?i)^HD-?TV`)
	reSrcDVDRip = regexp.MustCompile(`(?i)^DVD-?Rip`)
	reSrcBDRip  = regexp.MustCompile(`(?i)^BD-?Rip`)
	reSrcHDRip  = regexp.MustCompile(`(?i)^HDRip`)
	reSrcPDTV   = regexp.MustCompile(`(?i)^PDTV`)
	reSrcBDMV   = regexp.MustCompile(`(?i)^BDMV`)
)

func sourceNormalize(t string) (string, *string) {
	switch {
	case reSrcWEB.MatchString(t):
		return "WEB-DL", strPtr("WEB")
	case reSrcWEBRip.MatchString(t):
		return "WEBRip", strPtr("WEB")
	case reSrcBluRay.MatchString(t):
		return "BluRay", strPtr("BLURAY")
	case reSrcHDTV.MatchString(t):
		return "HDTV", strPtr("HDTV")
	case reSrcDVDRip.MatchString(t):
		return "DVDRip", strPtr("DVD")
	case reSrcBDRip.MatchString(t):
		return "BDRip", strPtr("BLURAY")
	case reSrcHDRip.MatchString(t):
		return "HDRip", strPtr("WEB")
	case reSrcPDTV.MatchString(t):
		return "PDTV", strPtr("HDTV")
	case reSrcBDMV.MatchString(t):
		return "BDMV", strPtr("BDMV")
	}
	return t, nil
}

// ---------------------------------------------------------------------------
// apply 函数(按组件把匹配结果写进 TitleComponents)
// ---------------------------------------------------------------------------

func applyYear(c *TitleComponents, ms []string) {
	if c.Year == nil && len(ms) > 0 {
		c.Year = strPtr(ms[0])
	}
}

func applyResolution(c *TitleComponents, ms []string) {
	if c.Resolution == nil && len(ms) > 0 {
		c.Resolution = strPtr(ms[0])
	}
}

func applyHDR(c *TitleComponents, ms []string) {
	if c.HDR == nil && len(ms) > 0 {
		c.HDR = strPtr(hdrNormalize(ms[0]))
	}
}

func applyVideo(c *TitleComponents, ms []string) {
	if c.VideoCodec == nil && len(ms) > 0 {
		c.VideoCodec = strPtr(videoNormalize(ms[0]))
	}
}

func applyAudio(c *TitleComponents, ms []string) {
	if c.AudioCodec == nil && len(ms) > 0 {
		c.AudioCodec = strPtr(audioNormalize(ms[0]))
	}
}

func applyChannels(c *TitleComponents, ms []string) {
	if c.Channels == nil {
		// 多声道 token(如 "2.0 2Audio")按出现顺序去重拼接
		var seen []string
		seenSet := map[string]bool{}
		for _, m := range ms {
			if !seenSet[m] {
				seenSet[m] = true
				seen = append(seen, m)
			}
		}
		c.Channels = strPtr(strings.Join(seen, " "))
	}
}

func applyBits(c *TitleComponents, ms []string) {
	if c.Bits == nil && len(ms) > 0 {
		c.Bits = strPtr(bitsNormalize(ms[0]))
	}
}

func applySource(c *TitleComponents, ms []string) {
	if c.Source == nil && len(ms) > 0 {
		src, med := sourceNormalize(ms[0])
		c.Source = strPtr(src)
		if c.Medium == nil && med != nil {
			c.Medium = med
		}
	}
}

func applyMedium(c *TitleComponents, ms []string) {
	// REMUX 是更具体的介质,可覆盖 source 推导出的 BLURAY
	if len(ms) > 0 {
		c.Medium = strPtr(strings.ToUpper(ms[0]))
	}
}

func applyComplete(c *TitleComponents, ms []string) {
	c.Complete = true
}

func applyNoise(c *TitleComponents, ms []string) {
	// 只剥离,不记录
}

// componentRule 组件正则表:顺序即优先级(先剥离的 token 不会干扰后续匹配)。
type componentRule struct {
	name  string
	re    *regexp.Regexp
	find  func(string) (string, [][]int)
	apply func(c *TitleComponents, ms []string)
}

var componentRules = []componentRule{
	{"year", yearRe, nil, applyYear},
	{"resolution", resolutionRe, nil, applyResolution},
	{"hdr", hdrRe, nil, applyHDR},
	{"video_codec", videoRe, nil, applyVideo},
	{"audio_codec", audioRe, nil, applyAudio},
	{"channels", channelsRe, nil, applyChannels},
	{"bits", bitsRe, nil, applyBits},
	{"source", sourceRe, nil, applySource},
	{"medium", mediumRe, nil, applyMedium},
	{"complete", nil, findComplete, applyComplete},
	{"noise", noiseRe, nil, applyNoise},
}

// ---------------------------------------------------------------------------
// 季集剥离
// ---------------------------------------------------------------------------

// groupStr 从 SubmatchIndex 里取指定捕获组文本。idx 由 re.SubexpIndex 给出。
func groupStr(orig string, loc []int, idx int) string {
	start := loc[2*idx]
	end := loc[2*idx+1]
	if start < 0 || end < 0 {
		return ""
	}
	return orig[start:end]
}

func toInt(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func applySeMatch(orig string, loc []int, re *regexp.Regexp, c *TitleComponents) {
	g := func(name string) string {
		idx := re.SubexpIndex(name)
		if idx < 0 {
			return ""
		}
		return groupStr(orig, loc, idx)
	}
	s := g("s")
	s2 := g("s2")
	e := g("e")
	e2 := g("e2")

	if s != "" {
		sInt := toInt(s)
		if s2 != "" && toInt(s2) != sInt {
			c.Season = strPtr(fmt.Sprintf("%d-%d", sInt, toInt(s2)))
		} else if c.Season == nil {
			c.Season = strPtr(fmt.Sprintf("%d", sInt))
		}
	}
	if e != "" {
		eInt := toInt(e)
		if e2 != "" {
			c.Episode = strPtr(fmt.Sprintf("%d-%d", eInt, toInt(e2)))
		} else if c.Episode == nil {
			c.Episode = strPtr(fmt.Sprintf("%d", eInt))
		}
	}
}

func extractSeasonEpisode(work string, c *TitleComponents) string {
	for _, re := range sePatterns {
		orig := work
		var ms [][]int
		if re == bareEPRe {
			work, ms = findBareEP(work)
			for _, loc := range ms {
				e := groupStr(orig, loc, re.SubexpIndex("e"))
				if e != "" && c.Episode == nil {
					c.Episode = strPtr(fmt.Sprintf("%d", toInt(e)))
				}
			}
			continue
		}
		locs := re.FindAllStringSubmatchIndex(work, -1)
		if len(locs) > 0 {
			b := []byte(work)
			for _, loc := range locs {
				for i := loc[0]; i < loc[1]; i++ {
					b[i] = ' '
				}
			}
			work = string(b)
			for _, loc := range locs {
				applySeMatch(orig, loc, re, c)
			}
		}
	}
	return work
}

// ---------------------------------------------------------------------------
// 制作组剥离
// ---------------------------------------------------------------------------

func normTokenKey(s string) string {
	return strings.ToUpper(strings.NewReplacer(".", "", "_", "", "-", "").Replace(s))
}

var groupSplitRe = regexp.MustCompile(`[-_.]`)
var groupTokensRe = regexp.MustCompile(`[-_.]+`)

func extractGroup(work string) (string, string) {
	m := groupRe.FindStringSubmatchIndex(work)
	if m == nil {
		return "", work
	}
	cand := work[m[2]:m[3]]
	key := normTokenKey(cand)

	// 结尾多 token 粘连时,尽量从最后已知组件之后切分。
	head := groupSplitRe.Split(cand, 2)[0]
	headKey := normTokenKey(head)
	if knownComponentTokens[headKey] {
		tokens := groupTokensRe.Split(cand, -1)
		if len(tokens) > 1 {
			lastKnownIdx := -1
			for i, tok := range tokens {
				if knownComponentTokens[normTokenKey(tok)] {
					lastKnownIdx = i
				}
			}
			if lastKnownIdx >= 0 {
				groupPart := strings.Join(tokens[lastKnownIdx+1:], "")
				if groupPart != "" {
					return groupPart, strings.TrimRight(work[:m[0]], " \t\n\r")
				}
			}
		}
	}

	if knownComponentTokens[key] {
		return "", work
	}
	if reSSeason.MatchString(cand) {
		return "", work
	}
	if nonGroupSuffixes[strings.ToLower(cand)] {
		return "", work
	}
	return cand, strings.TrimRight(work[:m[0]], " \t\n\r")
}

// ---------------------------------------------------------------------------
// 主入口
// ---------------------------------------------------------------------------

// ParseTitle 解析种子标题为结构化组件(纯函数,不联网)。
func ParseTitle(title string) TitleComponents {
	raw := title
	work := strings.TrimSpace(title)
	comps := TitleComponents{Raw: raw}
	if work == "" {
		return comps
	}

	// 1. 结尾制作组
	group, work := extractGroup(work)
	if group != "" {
		comps.Group = strPtr(group)
	}

	// 2. 季集
	work = extractSeasonEpisode(work, &comps)

	// 3. 组件正则表
	for _, rule := range componentRules {
		orig := work
		var ms [][]int
		if rule.find != nil {
			work, ms = rule.find(work)
		} else {
			work, ms = findAndBlank(work, rule.re)
		}
		if len(ms) > 0 {
			rule.apply(&comps, matchStrings(orig, ms))
		}
	}

	// 4. 主标题兜底(全被剥离则回退原始标题)
	titleOut := collapse(work)
	if titleOut == "" {
		titleOut = strings.TrimSpace(raw)
	}
	comps.Title = titleOut
	return comps
}

func matchStrings(orig string, ms [][]int) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, orig[m[0]:m[1]])
	}
	return out
}
