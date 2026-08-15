// Package secret implements the credential encryption layer (M1).
//
// It owns the master key lifecycle (env / file / first-run generation) plus
// AES-256-GCM encryption for credential fields at rest, and a masking helper
// for API responses. See docs/ARCHITECTURE-v4.md §5.
package secret

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// MasterKeyEnvVar is the environment variable that overrides the on-disk
	// master key. It must hold exactly 64 hex characters (32 bytes).
	MasterKeyEnvVar = "AUTOSEED_MASTER_KEY"

	// masterKeyFileName is the on-disk key file name inside the data dir.
	masterKeyFileName = "master.key"

	// masterKeyBytes is the AES-256 key length in bytes.
	masterKeyBytes = 32
)

// LoadMasterKey resolves the AES-256 master key in priority order:
//
//  1. MasterKeyEnvVar (64 hex chars → 32 bytes), validated for length/hex;
//  2. <dataDir>/master.key, accepting either 64 hex chars or 32 raw bytes;
//  3. otherwise a fresh 32-byte key is generated with crypto/rand and written
//     to <dataDir>/master.key as hex (0600, creating dataDir as needed).
func LoadMasterKey(dataDir string) ([]byte, error) {
	if v := os.Getenv(MasterKeyEnvVar); v != "" {
		return decodeEnvKey(v)
	}

	path := filepath.Join(dataDir, masterKeyFileName)
	if b, err := os.ReadFile(path); err == nil {
		key, err := decodeFileKey(b)
		if err != nil {
			return nil, fmt.Errorf("load master key %q: %w", path, err)
		}
		return key, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read master key %q: %w", path, err)
	}

	key := make([]byte, masterKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir %q: %w", dataDir, err)
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		return nil, fmt.Errorf("write master key %q: %w", path, err)
	}
	return key, nil
}

// decodeEnvKey validates the master key environment variable: exactly 64 hex
// characters, decoding to 32 bytes.
func decodeEnvKey(v string) ([]byte, error) {
	v = strings.TrimSpace(v)
	if len(v) != masterKeyBytes*2 {
		return nil, fmt.Errorf("%s must be exactly %d hex characters (%d bytes), got %d",
			MasterKeyEnvVar, masterKeyBytes*2, masterKeyBytes, len(v))
	}
	key := make([]byte, masterKeyBytes)
	if _, err := hex.Decode(key, []byte(v)); err != nil {
		return nil, fmt.Errorf("%s: invalid hex: %w", MasterKeyEnvVar, err)
	}
	return key, nil
}

// decodeFileKey accepts the on-disk key in either canonical form: 64 hex
// characters (32 bytes) or 32 raw bytes. A single trailing newline (common in
// hand-edited files) is tolerated.
func decodeFileKey(b []byte) ([]byte, error) {
	b = bytes.TrimRight(b, "\r\n")
	switch len(b) {
	case masterKeyBytes * 2:
		key := make([]byte, masterKeyBytes)
		if _, err := hex.Decode(key, b); err != nil {
			return nil, fmt.Errorf("invalid hex encoding: %w", err)
		}
		return key, nil
	case masterKeyBytes:
		return b, nil
	default:
		return nil, fmt.Errorf("invalid length %d (want %d hex chars or %d raw bytes)",
			len(b), masterKeyBytes*2, masterKeyBytes)
	}
}

// Encrypt seals plaintext with AES-256-GCM using a fresh random nonce and
// returns base64(nonce || ciphertext).
func Encrypt(key, plaintext []byte) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", fmt.Errorf("encrypt: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("encrypt: generate nonce: %w", err)
	}

	sealed := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt: it decodes the base64 blob and authenticates +
// decrypts it. A wrong key, corrupted ciphertext, or malformed input yields a
// non-nil error (GCM auth failure surfaces the wrong-key case).
func Decrypt(key []byte, enc string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil, fmt.Errorf("decrypt: invalid base64: %w", err)
	}

	gcm, err := newGCM(key)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}

	if len(raw) < gcm.NonceSize() {
		return nil, fmt.Errorf("decrypt: ciphertext too short")
	}
	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: authentication failed: %w", err)
	}
	return plaintext, nil
}

// Mask returns "***" for any non-empty credential and "" for empty input. It is
// used to redact secrets in API responses; the real value is only revealed via
// an authenticated "show/copy" path that decrypts on demand.
func Mask(s string) string {
	if s == "" {
		return ""
	}
	return "***"
}

// newGCM builds an AES-GCM AEAD from a raw key, validating its length.
func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("invalid key: %w", err)
	}
	return cipher.NewGCM(block)
}
