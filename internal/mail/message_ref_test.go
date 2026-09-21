/**
 * [INPUT]: 依赖 testing, errors
 * [OUTPUT]: 对外提供 PR-02 邮件引用与身份隔离单元测试
 * [POS]: internal/mail 的规范化邮件引用单元测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package mail

import (
	"errors"
	"strings"
	"testing"
)

func TestMessageRef_EncodeAndParse(t *testing.T) {
	orig := MessageRef{
		Provider:    "imap",
		AccountID:   "acc_123",
		Mailbox:     "INBOX",
		UIDValidity: 998877,
		UID:         42,
	}

	encoded := orig.Encode()
	if !strings.HasPrefix(encoded, "ref_v1_") {
		t.Fatalf("expected ref_v1_ prefix, got %s", encoded)
	}

	parsed, err := ParseMessageRef(encoded, "acc_123")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if parsed != orig {
		t.Fatalf("expected %+v, got %+v", orig, parsed)
	}
}

func TestMessageRef_NoCollisionAcrossFoldersAndAccounts(t *testing.T) {
	refInbox := MessageRef{
		Provider:    "imap",
		AccountID:   "acc_1",
		Mailbox:     "INBOX",
		UIDValidity: 100,
		UID:         42,
	}
	refJunk := MessageRef{
		Provider:    "imap",
		AccountID:   "acc_1",
		Mailbox:     "Junk",
		UIDValidity: 100,
		UID:         42,
	}
	refOtherAcc := MessageRef{
		Provider:    "imap",
		AccountID:   "acc_2",
		Mailbox:     "INBOX",
		UIDValidity: 100,
		UID:         42,
	}
	refNewValidity := MessageRef{
		Provider:    "imap",
		AccountID:   "acc_1",
		Mailbox:     "INBOX",
		UIDValidity: 200,
		UID:         42,
	}

	// 1. 同一账户下不同文件夹的相同 UID，CacheKey 必须互不相同
	if refInbox.CacheKey() == refJunk.CacheKey() {
		t.Fatalf("INBOX and Junk collided on CacheKey: %s", refInbox.CacheKey())
	}
	if refInbox.Encode() == refJunk.Encode() {
		t.Fatalf("INBOX and Junk collided on Encode: %s", refInbox.Encode())
	}

	// 2. 不同账户下的相同 UID，CacheKey 与 Encode 必须互不相同
	if refInbox.CacheKey() == refOtherAcc.CacheKey() {
		t.Fatalf("acc_1 and acc_2 collided on CacheKey: %s", refInbox.CacheKey())
	}
	if refInbox.Encode() == refOtherAcc.Encode() {
		t.Fatalf("acc_1 and acc_2 collided on Encode: %s", refInbox.Encode())
	}

	// 3. UIDVALIDITY 改变时，CacheKey 必须互不相同
	if refInbox.CacheKey() == refNewValidity.CacheKey() {
		t.Fatalf("different UIDValidity collided on CacheKey: %s", refInbox.CacheKey())
	}
}

func TestMessageRef_WebMailNonNumericThreadID(t *testing.T) {
	threadID := "thread_alphanumeric-9988_xyz.special"
	ref := MessageRef{
		Provider:  "webmail",
		AccountID: "acc_web",
		ThreadID:  threadID,
	}

	enc := ref.Encode()
	parsed, err := ParseMessageRef(enc, "acc_web")
	if err != nil {
		t.Fatalf("failed to parse webmail ref: %v", err)
	}
	if parsed.ThreadID != threadID || parsed.Provider != "webmail" {
		t.Fatalf("unexpected parsed ref: %+v", parsed)
	}

	// 兼容解析原始非纯数字 ThreadID
	legacyParsed, err := ParseMessageRef(threadID, "acc_web")
	if err != nil {
		t.Fatalf("failed to parse legacy threadID: %v", err)
	}
	if legacyParsed.ThreadID != threadID || legacyParsed.Provider != "webmail" {
		t.Fatalf("unexpected legacy parsed ref: %+v", legacyParsed)
	}

	// 验证 WebMail CacheKey
	expectedKey := "acc_web:webmail:" + threadID
	if ref.CacheKey() != expectedKey {
		t.Fatalf("expected CacheKey %s, got %s", expectedKey, ref.CacheKey())
	}
}

func TestMessageRef_AccountMismatchProtection(t *testing.T) {
	ref := MessageRef{
		Provider:    "imap",
		AccountID:   "acc_secret",
		Mailbox:     "INBOX",
		UIDValidity: 1,
		UID:         10,
	}
	enc := ref.Encode()

	// 使用其他账户解析此引用应直接报错
	_, err := ParseMessageRef(enc, "acc_attacker")
	if err == nil {
		t.Fatal("expected account mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "account mismatch") {
		t.Fatalf("expected account mismatch message, got: %v", err)
	}
}

func TestMessageRef_LegacyFormats(t *testing.T) {
	// folder:uid 格式
	r1, err := ParseMessageRef("Junk:108", "acc_1")
	if err != nil || r1.Mailbox != "Junk" || r1.UID != 108 || r1.Provider != "imap" {
		t.Fatalf("failed parsing Junk:108: %+v, err: %v", r1, err)
	}

	// 裸 UID 格式
	r2, err := ParseMessageRef("999", "acc_1")
	if err != nil || r2.Mailbox != "INBOX" || r2.UID != 999 || r2.Provider != "imap" {
		t.Fatalf("failed parsing 999: %+v, err: %v", r2, err)
	}

	// 空字符串格式
	_, err = ParseMessageRef("", "acc_1")
	if !errors.Is(err, ErrInvalidMessageRef) {
		t.Fatalf("expected ErrInvalidMessageRef, got: %v", err)
	}
}
