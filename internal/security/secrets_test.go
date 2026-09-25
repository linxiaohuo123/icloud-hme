package security

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretCipher_EncryptDecryptRoundTrip(t *testing.T) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}

	cipher, err := NewSecretCipher(key)
	if err != nil {
		t.Fatalf("NewSecretCipher failed: %v", err)
	}

	plaintext := []byte("apple-cookie-data-secret-123456")
	aad := AccountAAD("acc_01", "cookies")

	envelope, err := cipher.Encrypt(plaintext, aad)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	if !strings.HasPrefix(envelope, EnvelopePrefixV1) {
		t.Fatalf("Expected envelope prefix %s, got %s", EnvelopePrefixV1, envelope)
	}

	decrypted, err := cipher.Decrypt(envelope, aad)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}

	if !bytes.Equal(plaintext, decrypted) {
		t.Fatalf("Expected plaintext %s, got %s", string(plaintext), string(decrypted))
	}
}

func TestSecretCipher_AADMismatchFailsClosed(t *testing.T) {
	key := make([]byte, KeySize)
	_, _ = rand.Read(key)
	cipher, _ := NewSecretCipher(key)

	plaintext := []byte("secret-payload")
	aadA := AccountAAD("acc_01", "cookies")
	aadB := AccountAAD("acc_02", "cookies") // 跨账号搬运
	aadC := AccountAAD("acc_01", "mailbox") // 同账号跨字段搬运

	envelope, err := cipher.Encrypt(plaintext, aadA)
	if err != nil {
		t.Fatal(err)
	}

	// 搬运到 acc_02: 必须解密失败
	if _, err := cipher.Decrypt(envelope, aadB); err == nil {
		t.Fatalf("Expected AAD mismatch error for cross-account swap, got nil")
	}

	// 搬运到 mailbox 字段: 必须解密失败
	if _, err := cipher.Decrypt(envelope, aadC); err == nil {
		t.Fatalf("Expected AAD mismatch error for cross-field swap, got nil")
	}
}

func TestSecretCipher_CiphertextTamperFailsClosed(t *testing.T) {
	key := make([]byte, KeySize)
	_, _ = rand.Read(key)
	cipher, _ := NewSecretCipher(key)

	plaintext := []byte("secret-payload")
	aad := NotifySettingsAAD()

	envelope, err := cipher.Encrypt(plaintext, aad)
	if err != nil {
		t.Fatal(err)
	}

	// 篡改密文字符
	tampered := envelope[:len(envelope)-5] + "AAAAA"
	if _, err := cipher.Decrypt(tampered, aad); err == nil {
		t.Fatalf("Expected decryption error for tampered ciphertext, got nil")
	}

	// 损坏的版本前缀
	unknownVer := "enc:v99:" + envelope[len(EnvelopePrefixV1):]
	if _, err := cipher.Decrypt(unknownVer, aad); err == nil {
		t.Fatalf("Expected decryption error for unknown envelope version, got nil")
	}
}

func TestSecretCipher_KeyLengthValidation(t *testing.T) {
	// 短 key
	if _, err := NewSecretCipher([]byte("too-short")); err == nil {
		t.Fatalf("Expected error for short key, got nil")
	}

	// 长 key (33 字节)
	if _, err := NewSecretCipher(make([]byte, 33)); err == nil {
		t.Fatalf("Expected error for 33-byte key, got nil")
	}
}

func TestParseMasterKey(t *testing.T) {
	raw32 := make([]byte, 32)
	for i := range raw32 {
		raw32[i] = byte(i)
	}
	b64 := base64.StdEncoding.EncodeToString(raw32)

	parsed, err := ParseMasterKey(b64)
	if err != nil {
		t.Fatalf("ParseMasterKey failed: %v", err)
	}
	if !bytes.Equal(raw32, parsed) {
		t.Fatalf("Parsed key does not match original")
	}

	// 非 32 字节 Base64
	badB64 := base64.StdEncoding.EncodeToString([]byte("short"))
	if _, err := ParseMasterKey(badB64); err == nil {
		t.Fatalf("Expected error for non-32 byte key, got nil")
	}

	// 非法 Base64
	if _, err := ParseMasterKey("!!not-base64!!"); err == nil {
		t.Fatalf("Expected error for invalid base64, got nil")
	}
}

func TestLoadMasterKey(t *testing.T) {
	raw32 := make([]byte, 32)
	b64 := base64.StdEncoding.EncodeToString(raw32)

	// 测试环境变量加载
	os.Setenv("ICLOUD_HME_MASTER_KEY", b64)
	defer os.Unsetenv("ICLOUD_HME_MASTER_KEY")

	k, err := LoadMasterKey()
	if err != nil {
		t.Fatalf("LoadMasterKey failed: %v", err)
	}
	if len(k) != 32 {
		t.Fatalf("Expected 32-byte key, got %d", len(k))
	}

	// 测试文件优先
	tempDir := t.TempDir()
	keyFile := filepath.Join(tempDir, "master.key")
	if err := os.WriteFile(keyFile, []byte(b64+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	os.Setenv("ICLOUD_HME_MASTER_KEY_FILE", keyFile)
	defer os.Unsetenv("ICLOUD_HME_MASTER_KEY_FILE")

	kFile, err := LoadMasterKey()
	if err != nil {
		t.Fatalf("LoadMasterKey from file failed: %v", err)
	}
	if !bytes.Equal(k, kFile) {
		t.Fatalf("Key from file does not match")
	}
}
