// Package mteam M-Team 标题规范化 — 把源站标题转成 M-Team 规范标题。
//
// 参照已审核种子(实测 M-Team 动漫区)归纳的命名规范:
//
//	英文主标题 S01E05 1080p 片源 WEB-DL 音频编码 视频编码-制作组
//
// 规范要点:
//   - 空格分隔(源站常为 `.` 或 `_` 分隔)
//   - 编码:H.264/H.265(不用 HEVC/x264);音频 AAC2.0/DD5.1(紧凑写法)
//   - 中文前缀(如 `斗罗大陆Ⅱ绝世唐门.年番1.`)剥离到 smallDescr
//   - 制作组后缀(如 `-StarfallWeb` / `-Pure@StarfallWeb`)保留
//
// 本模块只做确定性转换(规则表 + 分词),不做 AI 猜测。
package mteam

import (
	"regexp"
	"strconv"
	"strings"
)

// CODEC_MAP 编码名 → M-Team 规范写法(大小写归一)。
var CODEC_MAP = map[string]string{
	"HEVC": "H.265", "H265": "H.265", "X265": "H.265", "X.265": "H.265",
	"hevc": "H.265", "x265": "H.265", "x.265": "H.265",
	"H264": "H.264", "X264": "H.264", "AVC": "H.264", "H.264": "H.264",
	"h264": "H.264", "x264": "H.264", "avc": "H.264",
	"H.265": "H.265",
}

// AUDIO_MAP 音频名 → M-Team 规范写法。
var AUDIO_MAP = map[string]string{
	"AAC": "AAC", "AAC2.0": "AAC2.0", "AAC 2.0": "AAC2.0",
	"DD5.1": "DD5.1", "DDP5.1": "DD5.1", "DDP 5.1": "DD5.1",
	"DD2.0": "DD2.0", "DDP2.0": "DD2.0", "DDP 2.0": "DD2.0",
	"AC3": "AC3", "TrueHD": "TrueHD", "FLAC": "FLAC", "LPCM": "LPCM",
}

// SOURCE_TOKENS 片源(源站 → M-Team 保留原名,M-Team 用 friDay/LINETV/Baha/ITUNES 等)。
var SOURCE_TOKENS = map[string]bool{
	"WEB-DL": true, "WEBRip": true, "BluRay": true, "REMUX": true,
	"Baha": true, "friDay": true, "LINETV": true, "ITUNES": true,
	"DSNP": true, "Netflix": true, "HULU": true, "AMZN": true,
}

// RES_TOKENS 分辨率。
var RES_TOKENS = map[string]bool{
	"1080p": true, "2160p": true, "720p": true, "4K": true,
	"1440p": true, "SD": true, "480p": true,
}

// groupRe 制作组分隔符:标题尾部 `-XXX` 或 `@XXX`。
var groupRe = regexp.MustCompile(`[-@]([A-Za-z0-9_.]+)$`)

// seasonEpRe 季集。
var seasonEpRe = regexp.MustCompile(`^(?:S\d+(?:E\d+)*|Season\s*\d+|第\d+季|S\d+E\d+-S\d+E\d+)$`)

// MTeamTitle M-Team 标题解析结果。
type MTeamTitle struct {
	Name          string // M-Team 规范主标题
	SmallDescrCN  string // 剥离出的中文信息(可作副标题)
	Group         string // 制作组后缀(如 StarfallWeb)
	Raw           string
}

var protectedRe = regexp.MustCompile(
	`(?:WEB-?DL|WEBRip|H\.26[45]|DDP?\.?2\.0|AAC\.?2\.0|S\d+E\d+[-–]?S?\d*E?\d*|BluRay|REMUX)`,
)

var tokenSplitRe = regexp.MustCompile(`[\s._\-@]+`)
var placeholderRe = regexp.MustCompile(`\x01(\d+)\x01`)

// splitTokens 按空白/点拆分标题,保留连字符/点复合 token(WEB-DL / H.265 等)。
func splitTokens(name string) []string {
	var protected []string
	held := protectedRe.ReplaceAllStringFunc(name, func(m string) string {
		protected = append(protected, m)
		return "\x01" + strconv.Itoa(len(protected)-1) + "\x01"
	})
	var out []string
	for _, t := range tokenSplitRe.Split(held, -1) {
		if t == "" {
			continue
		}
		if m := placeholderRe.FindStringSubmatch(t); m != nil && len(m) == 2 {
			idx, err := strconv.Atoi(m[1])
			if err == nil && idx >= 0 && idx < len(protected) {
				out = append(out, protected[idx])
			}
		} else {
			out = append(out, t)
		}
	}
	return out
}

// CleanMTteamTitle 把源站标题规范化为 M-Team 标题。
//
// 输入: '斗罗大陆Ⅱ绝世唐门.年番1.Soul.Land.2.The.Peerless.Tang.Clan.S01.2023.2160p.WEB-DL.HEVC.DDP.2.0.4Audio-StarfallWeb'
// 输出: name='Soul Land 2 The Peerless Tang Clan S01 2023 2160p WEB-DL H.265 DDP2.0 4Audio StarfallWeb'
func CleanMTteamTitle(raw string) MTeamTitle {
	raw = strings.TrimSpace(raw)
	// 1) 提取中文前缀(主英文名之前的非 ASCII 段)
	enStart := -1
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			enStart = i
			break
		}
	}
	cnPart := ""
	tail := raw
	if enStart >= 0 {
		cnPart = strings.Trim(raw[:enStart], "._ ")
		tail = raw[enStart:]
	}

	// 2) 提取制作组后缀
	group := ""
	if m := groupRe.FindStringIndex(tail); m != nil {
		group = tail[m[0]+1 : m[1]]
		tail = strings.TrimRight(tail[:m[0]], ".-_@ ")
	}

	// 3) token 化 + 规范化
	tokens := splitTokens(tail)
	var out []string
	for _, t := range tokens {
		up := strings.ToUpper(t)
		if hit, ok := CODEC_MAP[up]; ok {
			out = append(out, hit)
			continue
		}
		if hit, ok := CODEC_MAP[t]; ok {
			out = append(out, hit)
			continue
		}
		if hit, ok := AUDIO_MAP[up]; ok {
			out = append(out, hit)
			continue
		}
		if hit, ok := AUDIO_MAP[t]; ok {
			out = append(out, hit)
			continue
		}
		if isFullDigits(t, 4) {
			out = append(out, t)
		} else if RES_TOKENS[up] {
			out = append(out, t)
		} else if SOURCE_TOKENS[up] {
			out = append(out, t)
		} else if seasonEpRe.MatchString(t) {
			out = append(out, t)
		} else if up == "HDR" || up == "DV" || up == "HDR10" || up == "HDR10+" ||
			up == "10BIT" || up == "60FPS" || up == "SDR" {
			out = append(out, t)
		} else if t == "Complete" || t == "COMPLETE" || t == "S1" || t == "S2" {
			out = append(out, t)
		} else {
			out = append(out, t)
		}
	}

	name := strings.Join(out, " ")
	name = strings.ReplaceAll(name, "  ", " ")
	name = strings.TrimSpace(name)
	if group != "" {
		name = name + "-" + group
	}

	return MTeamTitle{
		Name:         name,
		SmallDescrCN: cnPart,
		Group:        group,
		Raw:          raw,
	}
}

func isFullDigits(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// SplitSmallDescr 便捷函数:返回 (M-Team 标题, 中文副标题)。
func SplitSmallDescr(rawName string) (string, string) {
	t := CleanMTteamTitle(rawName)
	return t.Name, t.SmallDescrCN
}
