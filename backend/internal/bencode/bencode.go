// Package bencode implements a complete bencode encoder/decoder.
//
// It is used to read and write .torrent files, re-encode cleaned
// torrents back to bytes, and compute info_hash values (SHA-1 of the
// bencoded info dictionary). Dictionary keys are byte strings; in Go
// they are represented as string values holding raw bytes. Keys are
// sorted byte-wise (sort.Strings) as required by the bencode spec so
// that info_hash stays stable across re-encodes.
//
// Ported from the legacy internal/bencode package and hardened against
// the original's decoding vulnerabilities:
//
//   - String length overflow: the length prefix is validated against the
//     remaining data before any slice arithmetic, so a prefix near
//     math.MaxInt64 can no longer overflow `start + int(n)` into a
//     negative index and panic (a negative length is also rejected).
//   - Recursion depth is capped at maxDepth so deeply nested lists/dicts
//     return an error instead of exhausting the stack.
//   - LoadTorrent rejects files larger than MaxTorrentSize.
//   - Integers are parsed with bitSize 64, bounding every integer to the
//     int64 range.
package bencode

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strconv"
)

// maxDepth is the maximum allowed nesting depth of lists and dicts.
// Deeper structures are rejected to prevent stack exhaustion on
// maliciously crafted input.
const maxDepth = 256

// MaxTorrentSize caps the on-disk size of a .torrent file accepted by
// LoadTorrent. It guards against a maliciously large file exhausting
// memory while being read and decoded.
const MaxTorrentSize = 64 << 20 // 64 MiB

// BencodeError reports a bencode decoding or encoding problem.
type BencodeError struct {
	Msg string
}

// Error implements the error interface.
func (e *BencodeError) Error() string { return e.Msg }

func errBencodef(format string, args ...any) error {
	return &BencodeError{Msg: fmt.Sprintf(format, args...)}
}

// parse decodes one bencode value starting at data[idx] and returns the
// value plus the index just past it. depth tracks the current list/dict
// nesting level so that over-deep input can be rejected before it
// overflows the call stack.
func parse(data []byte, idx int, depth int) (any, int, error) {
	if depth > maxDepth {
		return nil, 0, errBencodef("bencode: nesting too deep")
	}
	if idx >= len(data) {
		return nil, 0, errBencodef("bencode: unexpected end of data")
	}
	c := data[idx]

	switch {
	case c == 'i':
		rel := bytes.IndexByte(data[idx+1:], 'e')
		if rel == -1 {
			return nil, 0, errBencodef("bencode: unterminated integer")
		}
		end := idx + 1 + rel
		n, err := strconv.ParseInt(string(data[idx+1:end]), 10, 64)
		if err != nil {
			return nil, 0, errBencodef("bencode: bad integer")
		}
		return n, end + 1, nil

	case c == 'l':
		items := []any{}
		pos := idx + 1
		for pos < len(data) && data[pos] != 'e' {
			val, np, err := parse(data, pos, depth+1)
			if err != nil {
				return nil, 0, err
			}
			items = append(items, val)
			pos = np
		}
		if pos >= len(data) {
			return nil, 0, errBencodef("bencode: unterminated list")
		}
		return items, pos + 1, nil

	case c == 'd':
		d := map[string]any{}
		pos := idx + 1
		for pos < len(data) && data[pos] != 'e' {
			keyVal, np, err := parse(data, pos, depth+1)
			if err != nil {
				return nil, 0, err
			}
			key, ok := keyVal.(string)
			if !ok {
				return nil, 0, errBencodef("bencode: dict key must be bytes")
			}
			val, np2, err := parse(data, np, depth+1)
			if err != nil {
				return nil, 0, err
			}
			d[key] = val
			pos = np2
		}
		if pos >= len(data) {
			return nil, 0, errBencodef("bencode: unterminated dict")
		}
		return d, pos + 1, nil

	case c >= '0' && c <= '9':
		rel := bytes.IndexByte(data[idx:], ':')
		if rel == -1 {
			return nil, 0, errBencodef("bencode: missing colon in string")
		}
		n, err := strconv.ParseInt(string(data[idx:idx+rel]), 10, 64)
		if err != nil {
			return nil, 0, errBencodef("bencode: bad string length")
		}
		// Hardening: validate the length before any slice arithmetic.
		// The legacy code computed `end := start + int(n)`; for a length
		// prefix of math.MaxInt64 that sum overflowed int into a negative
		// value, slipping past the `end > len(data)` guard and panicking on
		// the slice. A negative prefix was accepted for the same reason.
		if n < 0 {
			return nil, 0, errBencodef("bencode: negative string length")
		}
		start := idx + rel + 1
		if n > int64(len(data)-start) {
			return nil, 0, errBencodef("bencode: string overruns data")
		}
		end := start + int(n)
		return string(data[start:end]), end, nil

	default:
		return nil, 0, errBencodef("bencode: unknown marker %q", c)
	}
}

// Decode parses data as a single bencode value. The top-level value may
// be an int64, a string (raw bytes), a []any list, or a map[string]any
// dict. Trailing bytes after the value are rejected.
func Decode(data []byte) (any, error) {
	val, end, err := parse(data, 0, 0)
	if err != nil {
		return nil, err
	}
	if end != len(data) {
		return nil, errBencodef("bencode: trailing data after value")
	}
	return val, nil
}

// Encode serializes value into bencode bytes.
//
// Supported Go types: int/int64/int32 (bencode integer), string and
// []byte (bencode byte string), []any (list), map[string]any (dict).
// Dict keys are sorted byte-wise (sort.Strings). Any other type is an
// error.
func Encode(value any) ([]byte, error) {
	switch v := value.(type) {
	case int:
		return []byte("i" + strconv.Itoa(v) + "e"), nil
	case int64:
		return []byte("i" + strconv.FormatInt(v, 10) + "e"), nil
	case int32:
		return []byte("i" + strconv.FormatInt(int64(v), 10) + "e"), nil
	case string:
		return []byte(strconv.Itoa(len(v)) + ":" + v), nil
	case []byte:
		return []byte(strconv.Itoa(len(v)) + ":" + string(v)), nil
	case []any:
		buf := make([]byte, 1, 64)
		buf[0] = 'l'
		for _, item := range v {
			b, err := Encode(item)
			if err != nil {
				return nil, err
			}
			buf = append(buf, b...)
		}
		return append(buf, 'e'), nil
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf := make([]byte, 1, 64)
		buf[0] = 'd'
		for _, k := range keys {
			buf = append(buf, []byte(strconv.Itoa(len(k))+":"+k)...)
			valb, err := Encode(v[k])
			if err != nil {
				return nil, err
			}
			buf = append(buf, valb...)
		}
		return append(buf, 'e'), nil
	default:
		return nil, errBencodef("bencode: cannot encode %T", value)
	}
}

// InfoHash computes the info_hash of a torrent: the lowercase hex
// SHA-1 digest of the bencoded info dictionary. Because Encode sorts
// dict keys byte-wise, the digest is stable regardless of the original
// key order or Go map iteration order.
func InfoHash(torrent map[string]any) (string, error) {
	infoVal, ok := torrent["info"]
	if !ok {
		return "", errBencodef("bencode: missing info dict")
	}
	info, ok := infoVal.(map[string]any)
	if !ok {
		return "", errBencodef("bencode: info must be a dict")
	}
	enc, err := Encode(info)
	if err != nil {
		return "", err
	}
	sum := sha1.Sum(enc)
	return hex.EncodeToString(sum[:]), nil
}

// LoadTorrent reads and decodes a .torrent file, verifying that it has
// an info dictionary. Files larger than MaxTorrentSize are rejected
// before being read.
func LoadTorrent(path string) (map[string]any, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Size() > MaxTorrentSize {
		return nil, errBencodef("bencode: torrent file %q exceeds %d bytes", path, MaxTorrentSize)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Defense in depth: guard against a size that changed between Stat and
	// ReadFile (or a filesystem that misreports Stat size).
	if int64(len(raw)) > MaxTorrentSize {
		return nil, errBencodef("bencode: torrent file %q exceeds %d bytes", path, MaxTorrentSize)
	}
	data, err := Decode(raw)
	if err != nil {
		return nil, err
	}
	d, ok := data.(map[string]any)
	if !ok {
		return nil, errBencodef("not a valid torrent file (missing info dict)")
	}
	if _, ok := d["info"]; !ok {
		return nil, errBencodef("not a valid torrent file (missing info dict)")
	}
	return d, nil
}

// WriteTorrent encodes the torrent dict and writes it to path (0600, since
// .torrent files may embed passkeys).
func WriteTorrent(path string, torrent map[string]any) error {
	enc, err := Encode(torrent)
	if err != nil {
		return err
	}
	return os.WriteFile(path, enc, 0o600)
}
