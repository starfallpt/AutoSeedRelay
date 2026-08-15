package bencode

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func sha1Hex(b []byte) string {
	s := sha1.Sum(b)
	return hex.EncodeToString(s[:])
}

func TestDecodeBasics(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want any
	}{
		{"int zero", "i0e", int64(0)},
		{"int positive", "i42e", int64(42)},
		{"int negative", "i-3e", int64(-3)},
		{"int max", "i9223372036854775807e", int64(9223372036854775807)},
		{"int min", "i-9223372036854775808e", int64(-9223372036854775808)},
		{"string", "4:spam", "spam"},
		{"empty string", "0:", ""},
		{"empty list", "le", []any{}},
		{"list", "li1ei2ee", []any{int64(1), int64(2)}},
		{"nested list", "l4:spamli1eee", []any{"spam", []any{int64(1)}}},
		{"empty dict", "de", map[string]any{}},
		{"dict", "d3:fooi42ee", map[string]any{"foo": int64(42)}},
		{"dict string val", "d1:a1:be", map[string]any{"a": "b"}},
		{"nested dict", "d1:ad1:bi1eee", map[string]any{"a": map[string]any{"b": int64(1)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Decode([]byte(tc.in))
			if err != nil {
				t.Fatalf("Decode(%q) error: %v", tc.in, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Decode(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

func TestDecodeErrors(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantSub string // empty means only require a non-nil error
	}{
		{"empty input", "", "unexpected end"},
		{"unterminated int", "i12", "unterminated integer"},
		{"unterminated int no marker", "i12x", "unterminated integer"},
		{"bad integer", "iabce", "bad integer"},
		{"integer out of int64 range", "i999999999999999999999999999999e", "bad integer"},
		{"unterminated list", "li1e", "unterminated list"},
		{"unterminated dict", "d1:ai1e", "unterminated dict"},
		{"missing colon", "5x", "missing colon"},
		{"bad string length", "x5:abc", "unknown marker"},
		{"string overruns", "5:ab", "string overruns"},
		{"unknown marker", "z", "unknown marker"},
		{"dict key not bytes", "di1ei2ee", "dict key must be bytes"},
		{"dict key is list", "dllee", "dict key must be bytes"},
		{"trailing data", "i1eX", "trailing data"},
		{"trailing data after string", "1:ai1e", "trailing data"},
		// A '-' cannot begin a length prefix: bencode lengths are unsigned, so
		// "-1:abc" is rejected at the marker level. The parse-level `n < 0`
		// guard remains as defense-in-depth (unreachable via the digit-only
		// length branch).
		{"negative length prefix", "-1:abc", "unknown marker"},
		{"max int64 length prefix", "9223372036854775807:x", "string overruns"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode([]byte(tc.in))
			if err == nil {
				t.Fatalf("Decode(%q) expected error, got nil", tc.in)
			}
			if tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Decode(%q) error = %q, want substring %q", tc.in, err.Error(), tc.wantSub)
			}
		})
	}
}

func TestEncodeBasics(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"int", 42, "i42e"},
		{"int negative", -3, "i-3e"},
		{"int64", int64(9223372036854775807), "i9223372036854775807e"},
		{"int32", int32(7), "i7e"},
		{"string", "spam", "4:spam"},
		{"bytes", []byte("spam"), "4:spam"},
		{"empty list", []any{}, "le"},
		{"list", []any{1, "spam"}, "li1e4:spame"},
		{"empty dict", map[string]any{}, "de"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Encode(tc.in)
			if err != nil {
				t.Fatalf("Encode(%#v) error: %v", tc.in, err)
			}
			if string(got) != tc.want {
				t.Fatalf("Encode(%#v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestEncodeUnsupportedType(t *testing.T) {
	for _, v := range []any{3.14, float32(1.5), nil, true, uint(1)} {
		if _, err := Encode(v); err == nil {
			t.Fatalf("Encode(%#v) expected error, got nil", v)
		}
	}
}

func TestDictKeySorted(t *testing.T) {
	// Keys deliberately unsorted in the map; Encode must sort byte-wise.
	m := map[string]any{
		"z": int64(1),
		"a": int64(2),
		"m": int64(3),
	}
	got, err := Encode(m)
	if err != nil {
		t.Fatalf("Encode error: %v", err)
	}
	want := "d1:ai2e1:mi3e1:zi1ee"
	if string(got) != want {
		t.Fatalf("Encode dict = %q, want %q (keys not sorted byte-wise)", got, want)
	}
}

func TestRoundTrip(t *testing.T) {
	original := map[string]any{
		"announce": "http://tracker.example/announce",
		"info": map[string]any{
			"length":       int64(1024),
			"name":         "example.txt",
			"piece length": int64(16384),
			"pieces":       "0123456789abcdefghijklmnopqrst",
			"files":        []any{map[string]any{"length": int64(5), "path": []any{"a", "b.txt"}}},
		},
	}
	enc, err := Encode(original)
	if err != nil {
		t.Fatalf("Encode error: %v", err)
	}
	decoded, err := Decode(enc)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Fatalf("round trip mismatch:\n got  %#v\n want %#v", decoded, original)
	}

	// Encoding the decoded value must be byte-identical (canonical form).
	enc2, err := Encode(decoded)
	if err != nil {
		t.Fatalf("Encode(decoded) error: %v", err)
	}
	if !bytes.Equal(enc, enc2) {
		t.Fatalf("re-encode not stable:\n first %q\n second %q", enc, enc2)
	}
}

func TestInfoHashStableAcrossKeyOrder(t *testing.T) {
	// Same info dict, once decoded from out-of-order bencode and once built
	// from a sorted literal. Both must yield the identical info_hash, and
	// that hash must equal SHA-1 of the canonical re-encode.
	outOfOrder := "d6:pieces5:abcde6:lengthi5ee" // "pieces" precedes "length"
	decoded, err := Decode([]byte(outOfOrder))
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	infoFromDecode, ok := decoded.(map[string]any)
	if !ok {
		t.Fatalf("decoded value not a dict: %#v", decoded)
	}

	canonical := []byte("d6:lengthi5e6:pieces5:abcdee")
	reEncoded, err := Encode(infoFromDecode)
	if err != nil {
		t.Fatalf("Encode error: %v", err)
	}
	if string(reEncoded) != string(canonical) {
		t.Fatalf("re-encode = %q, want canonical %q", reEncoded, canonical)
	}

	a := map[string]any{"info": infoFromDecode}
	b := map[string]any{"info": map[string]any{"length": int64(5), "pieces": "abcde"}}

	ha, err := InfoHash(a)
	if err != nil {
		t.Fatalf("InfoHash(a) error: %v", err)
	}
	hb, err := InfoHash(b)
	if err != nil {
		t.Fatalf("InfoHash(b) error: %v", err)
	}
	want := sha1Hex(canonical)
	if ha != want || hb != want {
		t.Fatalf("InfoHash = %q / %q, want %q", ha, hb, want)
	}
}

func TestInfoHashErrors(t *testing.T) {
	if _, err := InfoHash(map[string]any{}); err == nil {
		t.Fatal("InfoHash without info dict expected error")
	}
	if _, err := InfoHash(map[string]any{"info": "not-a-dict"}); err == nil {
		t.Fatal("InfoHash with non-dict info expected error")
	}
}

func TestDecodeDeepNesting(t *testing.T) {
	// A deep list/dict must return an error instead of overflowing the stack.
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"deep list 3000", strings.Repeat("l", 3000) + strings.Repeat("e", 3000), true},
		{"deep dict 3000", strings.Repeat("d1:a", 3000) + "i1e" + strings.Repeat("e", 3000), true},
		{"moderate list 100", strings.Repeat("l", 100) + strings.Repeat("e", 100), false},
		{"moderate dict 100", strings.Repeat("d1:a", 100) + "i1e" + strings.Repeat("e", 100), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode([]byte(tc.in))
			if tc.wantErr && err == nil {
				t.Fatalf("Decode of deep input expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Decode of moderate input unexpected error: %v", err)
			}
			if tc.wantErr && err != nil && !strings.Contains(err.Error(), "nesting too deep") {
				t.Fatalf("expected nesting-too-deep error, got %q", err.Error())
			}
		})
	}
}

func TestLoadTorrent(t *testing.T) {
	torrent := map[string]any{
		"announce": "http://tracker.example/announce",
		"info": map[string]any{
			"name":   "example.txt",
			"length": int64(1024),
		},
	}
	path := filepath.Join(t.TempDir(), "test.torrent")
	if err := WriteTorrent(path, torrent); err != nil {
		t.Fatalf("WriteTorrent error: %v", err)
	}

	loaded, err := LoadTorrent(path)
	if err != nil {
		t.Fatalf("LoadTorrent error: %v", err)
	}
	if _, ok := loaded["info"]; !ok {
		t.Fatal("loaded torrent missing info dict")
	}
	ih, err := InfoHash(loaded)
	if err != nil {
		t.Fatalf("InfoHash(loaded) error: %v", err)
	}
	if len(ih) != 40 {
		t.Fatalf("InfoHash = %q, want 40 hex chars", ih)
	}
}

func TestLoadTorrentErrors(t *testing.T) {
	dir := t.TempDir()

	// Missing file.
	if _, err := LoadTorrent(filepath.Join(dir, "nope.torrent")); err == nil {
		t.Fatal("LoadTorrent of missing file expected error")
	}

	// Not a dict (top-level integer).
	p := filepath.Join(dir, "notdict.torrent")
	if err := os.WriteFile(p, []byte("i1e"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTorrent(p); err == nil {
		t.Fatal("LoadTorrent of non-dict expected error")
	}

	// Dict without info key.
	p = filepath.Join(dir, "noinfo.torrent")
	if err := os.WriteFile(p, []byte("d1:ai1ee"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTorrent(p); err == nil {
		t.Fatal("LoadTorrent of dict without info expected error")
	}

	// Invalid bencode.
	p = filepath.Join(dir, "bad.torrent")
	if err := os.WriteFile(p, []byte("i12"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTorrent(p); err == nil {
		t.Fatal("LoadTorrent of invalid bencode expected error")
	}
}

func TestLoadTorrentSizeLimit(t *testing.T) {
	// A sparse file larger than MaxTorrentSize must be rejected without
	// being read into memory.
	f, err := os.CreateTemp(t.TempDir(), "big*.torrent")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(MaxTorrentSize + 1); err != nil {
		t.Fatal(err)
	}

	_, err = LoadTorrent(f.Name())
	if err == nil {
		t.Fatal("LoadTorrent of oversized file expected error")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected size-limit error, got %q", err.Error())
	}
}

// TestRealTorrentFiles exercises LoadTorrent + InfoHash against the legacy
// repo's sample .torrent files when they are available (repo-root
// data/relay_test). Skipped when running from a standalone backend/ checkout.
func TestRealTorrentFiles(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("..", "..", "..", "data", "relay_test", "*.torrent"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Skip("no sample .torrent files found under data/relay_test")
	}
	for _, path := range matches {
		t.Run(filepath.Base(path), func(t *testing.T) {
			torrent, err := LoadTorrent(path)
			if err != nil {
				t.Fatalf("LoadTorrent error: %v", err)
			}
			ih, err := InfoHash(torrent)
			if err != nil {
				t.Fatalf("InfoHash error: %v", err)
			}
			if len(ih) != 40 {
				t.Fatalf("InfoHash = %q, want 40 hex chars", ih)
			}
			// Re-encode -> re-decode -> re-hash must be stable.
			enc, err := Encode(torrent)
			if err != nil {
				t.Fatalf("Encode error: %v", err)
			}
			re, err := Decode(enc)
			if err != nil {
				t.Fatalf("Decode(re-encode) error: %v", err)
			}
			ih2, err := InfoHash(re.(map[string]any))
			if err != nil {
				t.Fatalf("InfoHash(re-decode) error: %v", err)
			}
			if ih != ih2 {
				t.Fatalf("info_hash changed on round trip: %q -> %q", ih, ih2)
			}
		})
	}
}
