package source

import (
	"net/url"
	"regexp"
	"strings"
)

// RedactURL 脱敏一个 URL:保留 scheme/host/path,query 替换为 "?[redacted]"。
// 用于在错误信息、日志、DB 记录与通知文本中剥离 passkey 等 query 凭据。
// userinfo 与 fragment 一并丢弃;解析失败时返回固定占位符,绝不回传原始串。
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[redacted-url]"
	}
	var b strings.Builder
	if u.Scheme != "" {
		b.WriteString(u.Scheme)
		b.WriteString("://")
	}
	b.WriteString(u.Host)
	b.WriteString(u.Path)
	if u.RawQuery != "" {
		b.WriteString("?[redacted]")
	}
	return b.String()
}

// redactURLRe 匹配字符串中出现的 http(s) URL(不含空白/引号/尖括号)。
var redactURLRe = regexp.MustCompile(`https?://[^\s"'<>]+`)

// RedactError 剥离字符串中出现的 URL 的 userinfo 与 query。供持久化
// (seeds.error、relay_records.last_error、activity_log.detail)、slog 与
// notifier 通知文本写入前脱敏。
func RedactError(s string) string {
	return redactURLRe.ReplaceAllStringFunc(s, RedactURL)
}
