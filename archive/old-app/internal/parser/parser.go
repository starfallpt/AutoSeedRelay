// Package parser extracts the fields required for cross-site relay from
// .torrent files, and cleans a torrent for a target site (rewriting the
// announce, forcing private, adding a source tag, jittering the creation
// date). It mirrors the cross-site cleaning rules used by the relay
// pipeline: announce -> target announce, drop announce-list/nodes,
// info.private = 1, info.source = "[<base_url>] <site name>", and a
// randomized forward shift of the creation date.
package parser

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/autoseedrelay/go-relay/internal/bencode"
)

// FileEntry describes one file inside a torrent.
type FileEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// ParsedTorrent carries all fields extracted from a .torrent file that
// the relay pipeline needs.
type ParsedTorrent struct {
	Path      string         `json:"path"`
	InfoHash  string         `json:"info_hash"`
	Name      string         `json:"name"`
	Announce  string         `json:"announce"`
	TotalSize int64          `json:"total_size"`
	FileCount int            `json:"file_count"`
	Files     []FileEntry    `json:"files"`
	IsPrivate bool           `json:"is_private"`
	Source    string         `json:"source"`
	IsV2      bool           `json:"is_v2"`
	RawDict   map[string]any `json:"raw_dict"`
}

// FileListText renders the file list as "path size" lines (useful for
// descriptions).
func (p *ParsedTorrent) FileListText() string {
	parts := make([]string, 0, len(p.Files))
	for _, f := range p.Files {
		parts = append(parts, fmt.Sprintf("%s %d", f.Path, f.Size))
	}
	return strings.Join(parts, "\n")
}

// ParseTorrent loads and parses a .torrent file into a ParsedTorrent.
// BitTorrent v2 / hybrid torrents are rejected.
func ParseTorrent(path string) (*ParsedTorrent, error) {
	d, err := bencode.LoadTorrent(path)
	if err != nil {
		return nil, err
	}
	infoVal, ok := d["info"]
	if !ok {
		return nil, fmt.Errorf("parser: torrent missing info dict")
	}
	info, ok := infoVal.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("parser: info is not a dict")
	}

	// Reject BitTorrent v2 / hybrid: meta version == 2, or a piece
	// layers dict, or a files tree in info.
	isV2 := false
	if mv, ok := info["meta version"]; ok {
		if vi, ok := asInt64(mv); ok && vi == 2 {
			isV2 = true
		}
	}
	if _, ok := d["piece layers"]; ok {
		isV2 = true
	}
	if _, ok := info["files tree"]; ok {
		isV2 = true
	}

	name := bstr(info["name"])
	announce := bstr(d["announce"])
	isPrivate := false
	if pv, ok := info["private"]; ok {
		if pi, ok := asInt64(pv); ok && pi != 0 {
			isPrivate = true
		}
	}
	source := bstr(info["source"])

	var files []FileEntry
	var total int64
	if rawFiles, ok := info["files"]; ok {
		if fl, ok := rawFiles.([]any); ok {
			for _, fv := range fl {
				fm, ok := fv.(map[string]any)
				if !ok {
					continue
				}
				length := int64Default(fm["length"], 0)
				var parts []string
				if rawPath, ok := fm["path"].([]any); ok {
					for _, pv := range rawPath {
						parts = append(parts, bstr(pv))
					}
				}
				files = append(files, FileEntry{Path: strings.Join(parts, "/"), Size: length})
				total += length
			}
		}
	} else {
		length := int64Default(info["length"], 0)
		files = append(files, FileEntry{Path: name, Size: length})
		total += length
	}

	infoHash, err := bencode.InfoHash(d)
	if err != nil {
		return nil, err
	}

	return &ParsedTorrent{
		Path:      path,
		InfoHash:  infoHash,
		Name:      name,
		Announce:  announce,
		TotalSize: total,
		FileCount: len(files),
		Files:     files,
		IsPrivate: isPrivate,
		Source:    source,
		IsV2:      isV2,
		RawDict:   d,
	}, nil
}

// CleanTorrentForTarget deep-copies torrent and rewrites it for the
// target site: announce is replaced with targetAnnounce, announce-list
// and nodes are dropped, info.private is forced to 1, info.source is set
// to "[<targetBaseURL>] <targetSiteName>", and the creation date is
// shifted forward by a random amount within jitterRange (default
// 600-1200 seconds). The original torrent is not modified.
func CleanTorrentForTarget(torrent map[string]any, targetAnnounce, targetSiteName, targetBaseURL string, jitterRange ...int) (map[string]any, error) {
	low, high := 600, 1200
	if len(jitterRange) >= 2 {
		low, high = jitterRange[0], jitterRange[1]
	}
	newT, ok := deepCopy(torrent).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("parser: torrent must be a dict")
	}
	info, ok := newT["info"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("parser: torrent missing info dict")
	}

	newT["announce"] = targetAnnounce
	delete(newT, "announce-list")
	delete(newT, "nodes")

	info["private"] = 1
	info["source"] = "[" + targetBaseURL + "] " + targetSiteName

	old := 0
	now := int(time.Now().Unix())
	if cv, ok := newT["creation date"]; ok {
		switch v := cv.(type) {
		case int64:
			old = int(v)
		case int:
			old = v
		case string:
			if n, err := strconv.Atoi(v); err == nil {
				old = n
			} else {
				old = now
			}
		default:
			old = now
		}
	}
	jitter := low
	if high > low {
		jitter = low + rand.Intn(high-low+1)
	}
	newT["creation date"] = int64(old + jitter)

	return newT, nil
}

// Summarize converts a ParsedTorrent to a JSON-serializable summary,
// truncating the file list to 50 entries.
func Summarize(p *ParsedTorrent) map[string]any {
	files := p.Files
	if len(files) > 50 {
		files = files[:50]
	}
	return map[string]any{
		"path":       p.Path,
		"info_hash":  p.InfoHash,
		"name":       p.Name,
		"announce":   p.Announce,
		"total_size": p.TotalSize,
		"file_count": p.FileCount,
		"files":      files,
		"is_private": p.IsPrivate,
		"source":     p.Source,
		"is_v2":      p.IsV2,
	}
}

// bstr converts a decoded value to a string, mirroring the Python _bstr
// helper: byte strings pass through, other scalars are formatted.
func bstr(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case float64:
		return int64(n), true
	case string:
		i, err := strconv.ParseInt(n, 10, 64)
		return i, err == nil
	default:
		return 0, false
	}
}

func int64Default(v any, def int64) int64 {
	if n, ok := asInt64(v); ok {
		return n
	}
	return def
}

// deepCopy recursively copies maps, slices and scalar values so that
// cleaning never mutates the caller's torrent dict.
func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = deepCopy(val)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, val := range t {
			s[i] = deepCopy(val)
		}
		return s
	default:
		return v
	}
}
