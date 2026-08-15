// Package parser extracts the fields required for cross-site relay from
// .torrent files, and cleans a torrent for a target site (rewriting the
// announce, forcing private, adding a source tag, jittering the creation
// date). It mirrors the cross-site cleaning rules used by the relay
// pipeline: announce -> target announce, drop announce-list/nodes/
// url-list/httpseeds/comment, info.private = 1, info.source = "<source>",
// and a randomized forward shift of the creation date.
package parser

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/autoseedrelay/relay/internal/bencode"
)

// FileInfo describes one file inside a torrent.
type FileInfo struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// ParsedTorrent carries all fields extracted from a .torrent file that
// the relay pipeline needs. RawDict holds the decoded top-level dict and
// is what CleanTorrentForTarget rewrites; it is excluded from JSON.
type ParsedTorrent struct {
	Name         string         `json:"name"`
	Announce     string         `json:"announce"`
	Private      bool           `json:"private"`
	Source       string         `json:"source"`
	InfoHash     string         `json:"info_hash"`
	Files        []FileInfo     `json:"files"`
	TotalSize    int64          `json:"total_size"`
	FileCount    int            `json:"file_count"`
	CreationDate int64          `json:"creation_date"`
	RawDict      map[string]any `json:"-"`
}

// ParseTorrent decodes a .torrent file from raw bencoded bytes and
// extracts the fields the relay pipeline needs. BitTorrent v2 / hybrid
// torrents are rejected with an explicit error.
func ParseTorrent(data []byte) (*ParsedTorrent, error) {
	decoded, err := bencode.Decode(data)
	if err != nil {
		return nil, err
	}
	d, ok := decoded.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("parser: torrent is not a dict")
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
	if mv, ok := info["meta version"]; ok {
		if vi, ok := asInt64(mv); ok && vi == 2 {
			return nil, fmt.Errorf("parser: BitTorrent v2 torrents are not supported (info.meta version = 2)")
		}
	}
	if _, ok := d["piece layers"]; ok {
		return nil, fmt.Errorf("parser: BitTorrent v2/hybrid torrents are not supported (piece layers present)")
	}
	if _, ok := info["files tree"]; ok {
		return nil, fmt.Errorf("parser: BitTorrent v2/hybrid torrents are not supported (files tree present)")
	}

	name := bstr(info["name"])
	announce := bstr(d["announce"])

	private := false
	if pv, ok := info["private"]; ok {
		if pi, ok := asInt64(pv); ok && pi != 0 {
			private = true
		}
	}
	source := bstr(info["source"])

	var files []FileInfo
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
				files = append(files, FileInfo{Path: strings.Join(parts, "/"), Size: length})
				total += length
			}
		}
	} else {
		length := int64Default(info["length"], 0)
		files = append(files, FileInfo{Path: name, Size: length})
		total += length
	}

	infoHash, err := bencode.InfoHash(d)
	if err != nil {
		return nil, err
	}

	return &ParsedTorrent{
		Name:         name,
		Announce:     announce,
		Private:      private,
		Source:       source,
		InfoHash:     infoHash,
		Files:        files,
		TotalSize:    total,
		FileCount:    len(files),
		CreationDate: int64Default(d["creation date"], 0),
		RawDict:      d,
	}, nil
}

// CleanTorrentForTarget deep-copies the torrent and rewrites it for the
// target site: announce is replaced, announce-list/nodes/url-list/
// httpseeds/comment are dropped, info.private is forced to 1,
// info.source is set to source, and the creation date is shifted forward
// by a random amount in 600..1200 seconds. The info_hash is recomputed
// for the cleaned info dict. The original ParsedTorrent is not modified.
func CleanTorrentForTarget(p *ParsedTorrent, announce, source string) (*ParsedTorrent, error) {
	if p == nil {
		return nil, fmt.Errorf("parser: nil torrent")
	}
	newT, ok := deepCopy(p.RawDict).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("parser: torrent must be a dict")
	}
	info, ok := newT["info"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("parser: torrent missing info dict")
	}

	newT["announce"] = announce
	delete(newT, "announce-list")
	delete(newT, "nodes")
	delete(newT, "url-list")
	delete(newT, "httpseeds")
	delete(newT, "comment")

	info["private"] = 1
	info["source"] = source

	// Shift the creation date forward by 600..1200 seconds. When the
	// torrent has no (parseable) creation date, fall back to "now" so the
	// cleaned torrent never carries an epoch-stale timestamp.
	now := time.Now().Unix()
	old := now
	if cv, ok := newT["creation date"]; ok {
		switch v := cv.(type) {
		case int64:
			old = v
		case int:
			old = int64(v)
		case int32:
			old = int64(v)
		case string:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				old = n
			}
		}
	}
	creation := old + int64(600+rand.Intn(601))
	newT["creation date"] = creation

	infoHash, err := bencode.InfoHash(newT)
	if err != nil {
		return nil, err
	}

	files := make([]FileInfo, len(p.Files))
	copy(files, p.Files)

	return &ParsedTorrent{
		Name:         bstr(info["name"]),
		Announce:     announce,
		Private:      true,
		Source:       source,
		InfoHash:     infoHash,
		Files:        files,
		TotalSize:    p.TotalSize,
		FileCount:    len(files),
		CreationDate: creation,
		RawDict:      newT,
	}, nil
}

// Summarize renders the file list as "path size" lines, truncated to at
// most n entries.
func Summarize(files []FileInfo, n int) string {
	if n < 0 {
		n = 0
	}
	if len(files) > n {
		files = files[:n]
	}
	parts := make([]string, 0, len(files))
	for _, f := range files {
		parts = append(parts, fmt.Sprintf("%s %d", f.Path, f.Size))
	}
	return strings.Join(parts, "\n")
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
