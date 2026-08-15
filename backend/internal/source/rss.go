// Package source 源站侧客户端:RSS 抓取 + .torrent 下载 + 详情/文件列表抓取。
//
// 单包合并了旧工程拆分的 internal/source 与 internal/detail 两个包,并做了安全
// 加固:
//   - 所有 HTTP 响应体读取都经过 http.MaxBytesReader 限制上限(RSS 10MB、
//     详情/API 20MB、torrent 64MB)
//   - 所有出站请求前都经过 safeURL 做 SSRF 防护(拒绝环回/私网/链路本地)
//   - 对 403/503 做可注入的指数退避(默认基数 60s、上限 900s)
//   - GuidToInfohash 修复了空 guid 全部散列成 sha1("") 的碰撞问题
package source

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// RssItem RSS 单条种子的解析结果。
type RssItem struct {
	ID           string
	Title        string
	Link         string
	Description  string
	CategoryName string
	CategoryID   string
	Size         *int64
	EnclosureURL string
	GUID         string
	Author       string
	PubDate      string

	IMDB       string
	SmallDescr string
}

// MatchesKeywords 标题后缀/关键词匹配(不区分大小写)。命中任一即返回 true。
func (it *RssItem) MatchesKeywords(keywords []string) bool {
	low := strings.ToLower(it.Title)
	for _, k := range keywords {
		if k != "" && strings.Contains(low, strings.ToLower(k)) {
			return true
		}
	}
	return false
}

// Infohash 返回该条目的规范化 info_hash:guid 为空时用 link/title 兜底参与哈希。
func (it *RssItem) Infohash() string {
	return GuidToInfohash(it.GUID, it.Link, it.Title)
}

type rssEnvelope struct {
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Items []rssItemXML `xml:"item"`
}

type rssItemXML struct {
	Title       string      `xml:"title"`
	Link        string      `xml:"link"`
	Description string      `xml:"description"`
	Category    rssCategory `xml:"category"`
	Enclosure   rssEncXML   `xml:"enclosure"`
	GUID        string      `xml:"guid"`
	Author      string      `xml:"author"`
	PubDate     string      `xml:"pubDate"`
}

type rssCategory struct {
	Text   string `xml:",chardata"`
	Domain string `xml:"domain,attr"`
}

type rssEncXML struct {
	URL    string `xml:"url,attr"`
	Length string `xml:"length,attr"`
}

// UnmarshalXML 自定义解析:仅匹配无命名空间的子元素,跳过 atom:link 等带命名空间
// 的同名元素。Go 默认的 xml:"link" 会忽略命名空间按本地名匹配,导致 <atom:link>
// 覆盖真正的 <link>。
func (it *rssItemXML) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	for {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			// 带命名空间的元素(atom:link、content:encoded 等)一律跳过。
			if t.Name.Space != "" {
				if err := d.Skip(); err != nil {
					return err
				}
				continue
			}
			var dst any
			switch t.Name.Local {
			case "title":
				dst = &it.Title
			case "link":
				dst = &it.Link
			case "description":
				dst = &it.Description
			case "category":
				dst = &it.Category
			case "enclosure":
				dst = &it.Enclosure
			case "guid":
				dst = &it.GUID
			case "author":
				dst = &it.Author
			case "pubDate":
				dst = &it.PubDate
			}
			if dst == nil {
				if err := d.Skip(); err != nil {
					return err
				}
				continue
			}
			if err := d.DecodeElement(dst, &t); err != nil {
				return err
			}
		case xml.EndElement:
			if t.Name == start.Name {
				return nil
			}
		}
	}
}

var (
	imdbRssRe       = regexp.MustCompile(`(tt\d{6,})`)
	smallDescrRssRe = regexp.MustCompile(`副标题[:：]\s*([^\n<]+)`)
	idFromLinkRe    = regexp.MustCompile(`id=(\d+)`)
	catFromDomainRe = regexp.MustCompile(`cat=(\d+)`)
)

// ParseRSS 解析 NexusPHP 系 RSS 输出(RSS 2.0)。
func ParseRSS(xmlBytes []byte) ([]RssItem, error) {
	var env rssEnvelope
	if err := xml.Unmarshal(xmlBytes, &env); err != nil {
		return nil, fmt.Errorf("解析 RSS XML 失败: %w", err)
	}
	var items []RssItem
	for _, el := range env.Channel.Items {
		link := strings.TrimSpace(el.Link)
		tid := ""
		if m := idFromLinkRe.FindStringSubmatch(link); m != nil {
			tid = m[1]
		}

		catID := ""
		if cm := catFromDomainRe.FindStringSubmatch(el.Category.Domain); cm != nil {
			catID = cm[1]
		}

		var size *int64
		if el.Enclosure.Length != "" {
			if n, err := strconv.ParseInt(el.Enclosure.Length, 10, 64); err == nil {
				size = &n
			}
		}

		description := el.Description
		imdb := ""
		if im := imdbRssRe.FindStringSubmatch(description); im != nil {
			imdb = im[1]
		}
		smallDescr := ""
		if sm := smallDescrRssRe.FindStringSubmatch(description); sm != nil {
			smallDescr = strings.TrimSpace(sm[1])
		}

		items = append(items, RssItem{
			ID:           tid,
			Title:        strings.TrimSpace(el.Title),
			Link:         link,
			Description:  description,
			CategoryName: strings.TrimSpace(el.Category.Text),
			CategoryID:   catID,
			Size:         size,
			EnclosureURL: el.Enclosure.URL,
			GUID:         strings.TrimSpace(el.GUID),
			Author:       strings.TrimSpace(el.Author),
			PubDate:      strings.TrimSpace(el.PubDate),
			IMDB:         imdb,
			SmallDescr:   smallDescr,
		})
	}
	return items, nil
}

// FetchRSS 抓取并解析指定 URL 的 RSS。请求前做 SSRF 校验,响应体限制 10MB。
func FetchRSS(ctx context.Context, urlStr string, client *http.Client) ([]RssItem, error) {
	if err := safeURL(urlStr); err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("构造 RSS 请求失败: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("RSS 抓取失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("RSS 抓取失败: HTTP %d", resp.StatusCode)
	}
	body, err := readBody(resp, maxRSSBody)
	if err != nil {
		return nil, fmt.Errorf("RSS 读取失败: %w", err)
	}
	return ParseRSS(body)
}

// GuidToInfohash 将 RSS 的 <guid> 规范化为 info_hash(40 位小写 hex):
//   - 40 位 hex(大小写均可)→ 小写直通(guid 本身就是 info_hash)
//   - 其它非空 guid → sha1(guid) 的 hex
//   - guid 为空 → 用 fallback(通常传 link、title)参与 sha1,避免所有空 guid
//     都坍缩成 sha1(""),造成不同种子 info_hash 互相碰撞
func GuidToInfohash(guid string, fallback ...string) string {
	guid = strings.TrimSpace(guid)
	if len(guid) == 40 && isHexString(guid) {
		return strings.ToLower(guid)
	}
	var b strings.Builder
	if guid != "" {
		b.WriteString(guid)
	} else {
		for _, f := range fallback {
			b.WriteString(f)
		}
	}
	sum := sha1.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func isHexString(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
