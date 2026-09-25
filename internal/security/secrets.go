/**
 * [INPUT]: 依赖 crypto/aes, crypto/cipher, crypto/rand, encoding/base64, errors, fmt, os, strings
 * [OUTPUT]: 对外提供 SecretCipher 结构、NewSecretCipher、LoadMasterKey、ParseMasterKey、AccountAAD、NotifySettingsAAD
 * [POS]: internal/security 的核心加密机，为可恢复凭据 (Apple cookies/app_password/mailbox/proxy/notify_settings) 提供 AES-256-GCM 封装与 AAD 防篡改绑定
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	// EnvelopePrefixV1 密文信封版本前缀
	EnvelopePrefixV1 = "enc:v1:"
	// NonceSize AES-GCM 标准 96-bit 随机随机数大小
	NonceSize = 12
	// KeySize 严格 256-bit (32 字节) 密钥长度
	KeySize = 32
)

// SecretCipher 封装基于 AES-256-GCM 的认证加密与解密器
type SecretCipher struct {
	key []byte
}

// NewSecretCipher 创建 SecretCipher。key 长度必须严格为 32 字节 (256-bit)
func NewSecretCipher(key []byte) (*SecretCipher, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("master key 长度必须严格为 %d 字节, 当前为 %d 字节", KeySize, len(key))
	}
	keyCopy := make([]byte, KeySize)
	copy(keyCopy, key)
	return &SecretCipher{key: keyCopy}, nil
}

// LoadMasterKey 从环境变量读取 Master Key:
// 优先读取 ICLOUD_HME_MASTER_KEY_FILE (支持 Docker Secret / systemd credentials)，
// 其次读取 ICLOUD_HME_MASTER_KEY。
// 密钥必须为 Base64 编码的严格 32 字节。
// 严禁打印密钥或密钥哈希。
func LoadMasterKey() ([]byte, error) {
	if filePath := strings.TrimSpace(os.Getenv("ICLOUD_HME_MASTER_KEY_FILE")); filePath != "" {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("读取 Master Key 文件失败 (%s): %w", filePath, err)
		}
		return ParseMasterKey(string(data))
	}

	keyEnv := strings.TrimSpace(os.Getenv("ICLOUD_HME_MASTER_KEY"))
	if keyEnv == "" {
		return nil, errors.New("缺少 Master Key 配置: 请设置环境变量 ICLOUD_HME_MASTER_KEY 或 ICLOUD_HME_MASTER_KEY_FILE")
	}
	return ParseMasterKey(keyEnv)
}

// ParseMasterKey 将 Base64 字符串解析为严格 32 字节的 Master Key
func ParseMasterKey(raw string) ([]byte, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("master key 不能为空")
	}

	// 兼容标准 base64 与 url safe base64
	decoded, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		var urlErr error
		decoded, urlErr = base64.RawURLEncoding.DecodeString(trimmed)
		if urlErr != nil {
			decoded, urlErr = base64.URLEncoding.DecodeString(trimmed)
			if urlErr != nil {
				return nil, fmt.Errorf("master key base64 解码失败: %w", err)
			}
		}
	}

	if len(decoded) != KeySize {
		return nil, fmt.Errorf("master key 解码后长度必须为严格 %d 字节, 当前为 %d 字节", KeySize, len(decoded))
	}
	return decoded, nil
}

// IsEncrypted 判断字符串是否为当前已支持的密文信封格式 (例如 enc:v1:...)
func IsEncrypted(s string) bool {
	return strings.HasPrefix(s, EnvelopePrefixV1)
}

// Encrypt 使用 AES-256-GCM 与附加验证数据 (AAD) 加密明文，返回版本化信封 enc:v1:<base64(nonce+ciphertext)>
func (c *SecretCipher) Encrypt(plaintext []byte, aad []byte) (string, error) {
	if c == nil || len(c.key) != KeySize {
		return "", errors.New("cipher 未初始化或 key 非法")
	}

	block, err := aes.NewCipher(c.key)
	if err != nil {
		return "", fmt.Errorf("创建 AES cipher 失败: %w", err)
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("创建 GCM 模式失败: %w", err)
	}

	nonce := make([]byte, NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("生成随机 nonce 失败: %w", err)
	}

	ciphertext := aesGCM.Seal(nil, nonce, plaintext, aad)

	payload := make([]byte, len(nonce)+len(ciphertext))
	copy(payload[:NonceSize], nonce)
	copy(payload[NonceSize:], ciphertext)

	return EnvelopePrefixV1 + base64.StdEncoding.EncodeToString(payload), nil
}

// Decrypt 解析版本化信封并使用 AAD 解密。
// 若版本未知、解密失败、AAD 不匹配或密文损坏，必须 Fail Closed 并返回明确 error。
func (c *SecretCipher) Decrypt(envelope string, aad []byte) ([]byte, error) {
	if c == nil || len(c.key) != KeySize {
		return nil, errors.New("cipher 未初始化或 key 非法")
	}

	if !strings.HasPrefix(envelope, EnvelopePrefixV1) {
		return nil, fmt.Errorf("未知或不受支持的密文信封格式: %s", envelopePrefixSummary(envelope))
	}

	b64Payload := envelope[len(EnvelopePrefixV1):]
	payload, err := base64.StdEncoding.DecodeString(b64Payload)
	if err != nil {
		return nil, fmt.Errorf("密文 payload base64 解码失败: %w", err)
	}

	if len(payload) < NonceSize+16 { // 12-byte nonce + 16-byte GCM overhead
		return nil, errors.New("密文 payload 长度过短，已损坏")
	}

	nonce := payload[:NonceSize]
	ciphertext := payload[NonceSize:]

	block, err := aes.NewCipher(c.key)
	if err != nil {
		return nil, fmt.Errorf("创建 AES cipher 失败: %w", err)
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("创建 GCM 模式失败: %w", err)
	}

	plaintext, err := aesGCM.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("AES-GCM 解密/身份验证失败 (AAD 不匹配或密文受损): %w", err)
	}

	return plaintext, nil
}

// AccountAAD 生成 accounts 表对应字段的 AAD: accounts:<account_id>:<field>
func AccountAAD(accountID, field string) []byte {
	return []byte(fmt.Sprintf("accounts:%s:%s", accountID, field))
}

// NotifySettingsAAD 生成 settings 表 notify_settings 记录的 AAD: settings:notify_settings
func NotifySettingsAAD() []byte {
	return []byte("settings:notify_settings")
}

func envelopePrefixSummary(env string) string {
	if len(env) > 16 {
		return env[:16] + "..."
	}
	return env
}
