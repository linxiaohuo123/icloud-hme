/**
 * [INPUT]: 依赖 encoding/json, encoding/base64, errors, fmt, strconv, strings
 * [OUTPUT]: 对外提供 MessageRef 结构体、Encode、ParseMessageRef、CacheKey 与标准错误
 * [POS]: internal/mail 的规范化邮件引用模型，统一 IMAP 与 WebMail 双轨身份
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package mail

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var (
	// ErrInvalidMessageRef 邮件引用格式无效
	ErrInvalidMessageRef = errors.New("invalid message ref")
	// ErrUIDValidityMismatch 邮箱 UIDVALIDITY 已变更，旧引用失效
	ErrUIDValidityMismatch = errors.New("uid validity mismatch")
	// ErrAccountMismatch 跨账号邮件引用不匹配
	ErrAccountMismatch = errors.New("account mismatch")
)

// MessageRef 统一邮件身份模型
type MessageRef struct {
	Provider    string `json:"provider"`               // "imap" 或 "webmail"
	AccountID   string `json:"account_id"`             // 归属母号 ID
	Mailbox     string `json:"mailbox,omitempty"`      // 文件夹名 (如 "INBOX", "Junk")
	UIDValidity uint32 `json:"uid_validity,omitempty"` // IMAP UIDVALIDITY (非零)
	UID         uint32 `json:"uid,omitempty"`          // IMAP UID
	ThreadID    string `json:"thread_id,omitempty"`    // WebMail ThreadID
}

// Encode 将 MessageRef 序列化为版本化、不透明的安全 Base64URL 字符串
func (r MessageRef) Encode() string {
	b, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	return "ref_v1_" + base64.RawURLEncoding.EncodeToString(b)
}

// CacheKey 生成服务端详情缓存的防碰撞唯一键
func (r MessageRef) CacheKey() string {
	if r.Provider == "webmail" {
		return fmt.Sprintf("%s:webmail:%s", r.AccountID, r.ThreadID)
	}
	mailbox := strings.TrimSpace(r.Mailbox)
	if mailbox == "" || strings.EqualFold(mailbox, "inbox") {
		mailbox = "INBOX"
	}
	return fmt.Sprintf("%s:imap:%s:%d:%d", r.AccountID, mailbox, r.UIDValidity, r.UID)
}

// ParseMessageRef 解析引用字符串或旧 ID
func ParseMessageRef(raw string, defaultAccountID string) (MessageRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return MessageRef{}, ErrInvalidMessageRef
	}

	// 1. 版本化规范引用 (ref_v1_...)
	if strings.HasPrefix(raw, "ref_v1_") {
		payload := strings.TrimPrefix(raw, "ref_v1_")
		data, err := base64.RawURLEncoding.DecodeString(payload)
		if err != nil {
			return MessageRef{}, fmt.Errorf("%w: base64 decode failed", ErrInvalidMessageRef)
		}
		var ref MessageRef
		if err := json.Unmarshal(data, &ref); err != nil {
			return MessageRef{}, fmt.Errorf("%w: json unmarshal failed", ErrInvalidMessageRef)
		}
		if defaultAccountID != "" && ref.AccountID != "" && ref.AccountID != defaultAccountID {
			return MessageRef{}, fmt.Errorf("%w: ref belongs to %s, request for %s", ErrAccountMismatch, ref.AccountID, defaultAccountID)
		}
		if ref.AccountID == "" {
			ref.AccountID = defaultAccountID
		}
		return ref, nil
	}

	// 2. 兼容解析: folder:uid 格式 (如 INBOX:42)
	if i := strings.IndexByte(raw, ':'); i > 0 && i < len(raw)-1 {
		folder, right := raw[:i], raw[i+1:]
		if u, err := strconv.ParseUint(right, 10, 32); err == nil {
			return MessageRef{
				Provider:  "imap",
				AccountID: defaultAccountID,
				Mailbox:   folder,
				UID:       uint32(u),
			}, nil
		}
	}

	// 3. 兼容解析: 裸数字 UID (如 42)
	if u, err := strconv.ParseUint(raw, 10, 32); err == nil {
		return MessageRef{
			Provider:  "imap",
			AccountID: defaultAccountID,
			Mailbox:   "INBOX",
			UID:       uint32(u),
		}, nil
	}

	// 4. 兼容解析: WebMail ThreadID 字符串
	return MessageRef{
		Provider:  "webmail",
		AccountID: defaultAccountID,
		ThreadID:  raw,
	}, nil
}
