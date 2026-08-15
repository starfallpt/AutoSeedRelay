package secret

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fixedKey returns a deterministic 32-byte key for tests.
func fixedKey(seed byte) []byte {
	k := make([]byte, masterKeyBytes)
	for i := range k {
		k[i] = seed + byte(i)
	}
	return k
}

func TestEncryptDecryptRoundtrip(t *testing.T) {
	key := fixedKey(0x10)
	plaintexts := [][]byte{
		[]byte("cookie=abc;passkey=def"),
		[]byte("short"),
		{},                            // empty plaintext still seals
		bytes.Repeat([]byte{0xFF}, 1), // arbitrary bytes
	}

	for _, pt := range plaintexts {
		enc, err := Encrypt(key, pt)
		if err != nil {
			t.Fatalf("Encrypt(%q): %v", pt, err)
		}
		if enc == "" {
			t.Fatalf("Encrypt(%q): empty output", pt)
		}

		got, err := Decrypt(key, enc)
		if err != nil {
			t.Fatalf("Decrypt(%q): %v", pt, err)
		}
		if !bytes.Equal(got, pt) {
			t.Fatalf("roundtrip mismatch: want %q, got %q", pt, got)
		}
	}
}

func TestEncryptUsesFreshNonce(t *testing.T) {
	key := fixedKey(0x20)
	enc1, err := Encrypt(key, []byte("same plaintext"))
	if err != nil {
		t.Fatal(err)
	}
	enc2, err := Encrypt(key, []byte("same plaintext"))
	if err != nil {
		t.Fatal(err)
	}
	if enc1 == enc2 {
		t.Fatal("expected distinct ciphertexts for the same plaintext (random nonce)")
	}
}

func TestDecryptWrongKey(t *testing.T) {
	enc, err := Encrypt(fixedKey(0x30), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt(fixedKey(0x31), enc); err == nil {
		t.Fatal("expected error when decrypting with the wrong key")
	}
}

func TestDecryptInvalidCiphertext(t *testing.T) {
	key := fixedKey(0x40)

	// Not base64 at all.
	if _, err := Decrypt(key, "!!!not-base64!!!"); err == nil {
		t.Fatal("expected error for non-base64 input")
	}

	// Valid base64 but shorter than the GCM nonce (12 bytes).
	tooShort := base64.StdEncoding.EncodeToString([]byte("abc"))
	if _, err := Decrypt(key, tooShort); err == nil {
		t.Fatal("expected error for too-short ciphertext")
	}
}

func TestDecryptTampered(t *testing.T) {
	key := fixedKey(0x50)
	enc, err := Encrypt(key, []byte("tamper me"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0x01 // flip the last byte of the auth tag
	tampered := base64.StdEncoding.EncodeToString(raw)

	if _, err := Decrypt(key, tampered); err == nil {
		t.Fatal("expected authentication failure for tampered ciphertext")
	}
}

func TestEncryptWrongKeySize(t *testing.T) {
	if _, err := Encrypt([]byte("too-short"), []byte("x")); err == nil {
		t.Fatal("expected error for a non-32-byte key")
	}
}

func TestMask(t *testing.T) {
	if got := Mask(""); got != "" {
		t.Fatalf("Mask(empty) = %q, want empty", got)
	}
	if got := Mask("super-secret"); got != "***" {
		t.Fatalf("Mask(non-empty) = %q, want ***", got)
	}
	if got := Mask("   "); got != "***" {
		t.Fatalf("Mask(whitespace) = %q, want ***", got)
	}
}

func TestLoadMasterKeyGeneratesAndPersists(t *testing.T) {
	dataDir := t.TempDir()

	key, err := LoadMasterKey(dataDir)
	if err != nil {
		t.Fatalf("LoadMasterKey: %v", err)
	}
	if len(key) != masterKeyBytes {
		t.Fatalf("key length = %d, want %d", len(key), masterKeyBytes)
	}

	path := filepath.Join(dataDir, masterKeyFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("master key file not created: %v", err)
	}

	// Permissions: assert 0600 on POSIX; on Windows the bits are unreliable so
	// only verify the file exists (see task note).
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("master key permissions = %o, want 0600", perm)
		}
	}

	// The persisted key must reload to the identical bytes.
	key2, err := LoadMasterKey(dataDir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !bytes.Equal(key, key2) {
		t.Fatal("reloaded key differs from generated key")
	}
}

func TestLoadMasterKeyEnvPriority(t *testing.T) {
	dataDir := t.TempDir()

	// Plant a different on-disk key so we can prove env wins.
	diskHex := strings.Repeat("ab", masterKeyBytes)
	if err := os.WriteFile(filepath.Join(dataDir, masterKeyFileName), []byte(diskHex), 0o600); err != nil {
		t.Fatal(err)
	}

	envHex := strings.Repeat("cd", masterKeyBytes)
	t.Setenv(MasterKeyEnvVar, envHex)

	key, err := LoadMasterKey(dataDir)
	if err != nil {
		t.Fatalf("LoadMasterKey: %v", err)
	}
	want, _ := hex.DecodeString(envHex)
	if !bytes.Equal(key, want) {
		t.Fatalf("env key not honored: got %x, want %x", key, want)
	}
}

func TestLoadMasterKeyEnvInvalid(t *testing.T) {
	cases := map[string]string{
		"too short": "abcd",
		"too long":  strings.Repeat("aa", masterKeyBytes+1), // 66 chars
		"bad hex":   strings.Repeat("zz", masterKeyBytes),   // 64 chars, non-hex
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(MasterKeyEnvVar, v)
			if _, err := LoadMasterKey(t.TempDir()); err == nil {
				t.Fatalf("expected error for env value %q", v)
			}
		})
	}
}

func TestLoadMasterKeyFileHex(t *testing.T) {
	dataDir := t.TempDir()
	hexKey := strings.Repeat("0f", masterKeyBytes)
	if err := os.WriteFile(filepath.Join(dataDir, masterKeyFileName), []byte(hexKey), 0o600); err != nil {
		t.Fatal(err)
	}

	key, err := LoadMasterKey(dataDir)
	if err != nil {
		t.Fatalf("LoadMasterKey: %v", err)
	}
	want, _ := hex.DecodeString(hexKey)
	if !bytes.Equal(key, want) {
		t.Fatalf("got %x, want %x", key, want)
	}
}

func TestLoadMasterKeyFileRaw(t *testing.T) {
	dataDir := t.TempDir()
	raw := fixedKey(0x60)
	if err := os.WriteFile(filepath.Join(dataDir, masterKeyFileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	key, err := LoadMasterKey(dataDir)
	if err != nil {
		t.Fatalf("LoadMasterKey: %v", err)
	}
	if !bytes.Equal(key, raw) {
		t.Fatalf("got %x, want %x", key, raw)
	}
}

func TestLoadMasterKeyFileInvalidLength(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, masterKeyFileName), []byte("not-a-valid-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMasterKey(dataDir); err == nil {
		t.Fatal("expected error for a master.key of invalid length")
	}
}

func TestLoadMasterKeyWarnsOnOpenPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows skips the permission check")
	}
	dataDir := t.TempDir()
	hexKey := strings.Repeat("0a", masterKeyBytes)
	if err := os.WriteFile(filepath.Join(dataDir, masterKeyFileName), []byte(hexKey), 0o644); err != nil {
		t.Fatal(err)
	}

	// Capture slog output to assert the warning is actually emitted.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	key, err := LoadMasterKey(dataDir)
	if err != nil {
		t.Fatalf("LoadMasterKey: %v", err)
	}
	want, _ := hex.DecodeString(hexKey)
	if !bytes.Equal(key, want) {
		t.Fatalf("got %x, want %x", key, want)
	}
	if !strings.Contains(buf.String(), "master key file permissions too open") {
		t.Fatalf("expected permission warning, got log %q", buf.String())
	}
}
