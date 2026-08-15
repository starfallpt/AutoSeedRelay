package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autoseedrelay/relay/internal/bencode"
)

// realSampleDir resolves the legacy repo's sample .torrent directory from
// the parser package directory (backend/internal/parser).
func realSampleDir() string {
	return filepath.Join("..", "..", "..", "data", "relay_test")
}

// TestParseRealSamples parses every sample .torrent file and verifies the
// info_hash matches bencode.LoadTorrent + InfoHash (the reference
// path-based computation). Skipped when samples are unavailable.
func TestParseRealSamples(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join(realSampleDir(), "*.torrent"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Skip("no sample .torrent files found under data/relay_test")
	}

	for _, path := range matches {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile error: %v", err)
			}
			p, err := ParseTorrent(data)
			if err != nil {
				t.Fatalf("ParseTorrent error: %v", err)
			}

			ref, err := bencode.LoadTorrent(path)
			if err != nil {
				t.Fatalf("LoadTorrent error: %v", err)
			}
			want, err := bencode.InfoHash(ref)
			if err != nil {
				t.Fatalf("InfoHash error: %v", err)
			}
			if p.InfoHash != want {
				t.Fatalf("InfoHash = %q, want %q", p.InfoHash, want)
			}
			if len(p.InfoHash) != 40 {
				t.Fatalf("InfoHash = %q, want 40 hex chars", p.InfoHash)
			}
			if p.Name == "" {
				t.Fatal("Name is empty")
			}
			if len(p.Files) == 0 {
				t.Fatal("Files is empty")
			}
			if p.TotalSize <= 0 {
				t.Fatalf("TotalSize = %d, want > 0", p.TotalSize)
			}
			if p.FileCount != len(p.Files) {
				t.Fatalf("FileCount = %d, want %d", p.FileCount, len(p.Files))
			}
		})
	}
}

// TestParseSingleFile exercises the length-based (single-file) branch
// using the synthetic sample.
func TestParseSingleFile(t *testing.T) {
	path := filepath.Join(realSampleDir(), "synthetic.torrent")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	p, err := ParseTorrent(data)
	if err != nil {
		t.Fatalf("ParseTorrent error: %v", err)
	}
	if len(p.Files) != 1 {
		t.Fatalf("Files = %d entries, want 1", len(p.Files))
	}
	if p.Files[0].Path != p.Name {
		t.Fatalf("single-file path = %q, want name %q", p.Files[0].Path, p.Name)
	}
}

// TestCleanChangesInfoHashAndPrivate cleans a real sample with a fresh
// announce + source and asserts the info_hash changes, private is forced
// to 1, and the original torrent is left untouched.
func TestCleanChangesInfoHashAndPrivate(t *testing.T) {
	path := filepath.Join(realSampleDir(), "synthetic.torrent")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	p, err := ParseTorrent(data)
	if err != nil {
		t.Fatalf("ParseTorrent error: %v", err)
	}

	origAnnounce := p.Announce
	origHash := p.InfoHash
	origCreation := p.CreationDate

	cleaned, err := CleanTorrentForTarget(p, "https://target.example/announce", "[target.example] testsite")
	if err != nil {
		t.Fatalf("CleanTorrentForTarget error: %v", err)
	}

	if cleaned.InfoHash == origHash {
		t.Fatal("info_hash did not change after clean")
	}
	if cleaned.Private != true {
		t.Fatalf("Private = %v, want true", cleaned.Private)
	}
	if cleaned.Announce != "https://target.example/announce" {
		t.Fatalf("Announce = %q, want target announce", cleaned.Announce)
	}
	if cleaned.Source != "[target.example] testsite" {
		t.Fatalf("Source = %q, want target source", cleaned.Source)
	}

	info := cleaned.RawDict["info"].(map[string]any)
	if info["private"] == nil {
		t.Fatal("info.private missing after clean")
	}
	if v, ok := asInt64(info["private"]); !ok || v == 0 {
		t.Fatalf("info.private = %#v, want non-zero", info["private"])
	}
	if info["source"] != "[target.example] testsite" {
		t.Fatalf("info.source = %#v, want target source", info["source"])
	}

	// Creation date must have shifted forward by 600..1200 seconds.
	delta := cleaned.CreationDate - origCreation
	if delta < 600 || delta > 1200 {
		t.Fatalf("creation date delta = %d, want in [600,1200]", delta)
	}

	// The original torrent must not be mutated.
	if p.Announce != origAnnounce {
		t.Fatalf("original Announce mutated: %q -> %q", origAnnounce, p.Announce)
	}
	if p.InfoHash != origHash {
		t.Fatalf("original InfoHash mutated: %q -> %q", origHash, p.InfoHash)
	}
	if p.RawDict["announce"] != origAnnounce {
		t.Fatal("original RawDict announce mutated")
	}
}

// TestCleanRemovesLegacyFields constructs a torrent carrying every field
// that must be dropped, cleans it, and asserts url-list / httpseeds /
// comment / announce-list / nodes are all removed.
func TestCleanRemovesLegacyFields(t *testing.T) {
	src := map[string]any{
		"announce":      "https://old.example/announce",
		"announce-list": []any{[]any{"https://old.example/announce", "https://old.example/backup"}},
		"nodes":         []any{[]any{"1.2.3.4", int64(6881)}},
		"url-list":      "https://webseed.example/file.bin",
		"httpseeds":     []any{"https://seed.example/file.bin"},
		"comment":       "please remove me",
		"creation date": int64(1700000000),
		"info": map[string]any{
			"name":         "sample.bin",
			"length":       int64(1024),
			"piece length": int64(16384),
			"pieces":       "0123456789abcdefghijklmnopqrst",
			"private":      int64(0),
		},
	}
	data, err := bencode.Encode(src)
	if err != nil {
		t.Fatalf("Encode error: %v", err)
	}
	p, err := ParseTorrent(data)
	if err != nil {
		t.Fatalf("ParseTorrent error: %v", err)
	}
	if p.Private {
		t.Fatal("Private = true before clean, want false")
	}

	cleaned, err := CleanTorrentForTarget(p, "https://target.example/announce", "[target.example] testsite")
	if err != nil {
		t.Fatalf("CleanTorrentForTarget error: %v", err)
	}

	for _, key := range []string{"announce-list", "nodes", "url-list", "httpseeds", "comment"} {
		if _, ok := cleaned.RawDict[key]; ok {
			t.Fatalf("key %q not removed by clean", key)
		}
	}
	if cleaned.Announce != "https://target.example/announce" {
		t.Fatalf("Announce = %q, want target announce", cleaned.Announce)
	}
	if cleaned.Private != true {
		t.Fatalf("Private = %v, want true", cleaned.Private)
	}
}

// TestRejectV2Torrents constructs v2 / hybrid torrents and asserts they
// are rejected with a clear error.
func TestRejectV2Torrents(t *testing.T) {
	cases := []struct {
		name    string
		torrent map[string]any
		wantSub string
	}{
		{
			name: "meta version 2",
			torrent: map[string]any{
				"announce": "https://tracker.example/announce",
				"info": map[string]any{
					"meta version": int64(2),
					"name":         "v2.torrent",
				},
			},
			wantSub: "meta version",
		},
		{
			name: "piece layers",
			torrent: map[string]any{
				"announce":     "https://tracker.example/announce",
				"piece layers": map[string]any{},
				"info": map[string]any{
					"name":   "hybrid.torrent",
					"length": int64(1),
				},
			},
			wantSub: "piece layers",
		},
		{
			name: "files tree",
			torrent: map[string]any{
				"announce": "https://tracker.example/announce",
				"info": map[string]any{
					"name":       "hybrid.torrent",
					"files tree": map[string]any{},
				},
			},
			wantSub: "files tree",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := bencode.Encode(tc.torrent)
			if err != nil {
				t.Fatalf("Encode error: %v", err)
			}
			_, err = ParseTorrent(data)
			if err == nil {
				t.Fatal("ParseTorrent expected error for v2/hybrid torrent, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestParseErrors verifies clear errors for malformed input.
func TestParseErrors(t *testing.T) {
	if _, err := ParseTorrent([]byte("i1e")); err == nil {
		t.Fatal("expected error for non-dict top-level value")
	}
	if _, err := ParseTorrent([]byte("d1:ai1ee")); err == nil {
		t.Fatal("expected error for dict missing info")
	}
	if _, err := ParseTorrent([]byte("d4:infoi1ee")); err == nil {
		t.Fatal("expected error for non-dict info")
	}
	if _, err := ParseTorrent([]byte("not bencode")); err == nil {
		t.Fatal("expected error for invalid bencode")
	}
}

// TestSummarize verifies truncation and line formatting.
func TestSummarize(t *testing.T) {
	files := []FileInfo{
		{Path: "a/1.txt", Size: 10},
		{Path: "a/2.txt", Size: 20},
		{Path: "b/3.txt", Size: 30},
	}

	if got := Summarize(files, 2); got != "a/1.txt 10\na/2.txt 20" {
		t.Fatalf("Summarize(files, 2) = %q", got)
	}
	if got := Summarize(files, 10); got != "a/1.txt 10\na/2.txt 20\nb/3.txt 30" {
		t.Fatalf("Summarize(files, 10) = %q", got)
	}
	if got := Summarize(files, 0); got != "" {
		t.Fatalf("Summarize(files, 0) = %q, want empty", got)
	}
	if got := Summarize(nil, 5); got != "" {
		t.Fatalf("Summarize(nil, 5) = %q, want empty", got)
	}
}

// TestCleanNilTorrent verifies a nil receiver yields an error.
func TestCleanNilTorrent(t *testing.T) {
	if _, err := CleanTorrentForTarget(nil, "x", "y"); err == nil {
		t.Fatal("expected error for nil torrent")
	}
}
