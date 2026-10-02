package account

import (
	"errors"
	"fmt"
	"testing"

	"icloud-hme/internal/mail"
)

// TestMailAuthHealth 连续认证失败达阈值才判故障；网络错误不计数；成功或换授权码即恢复。
func TestMailAuthHealth(t *testing.T) {
	m := &Manager{}
	acc := &Account{ID: "acc_1", ICloudEmail: "owner@icloud.com", AppPassword: "old-pass", Status: "active"}
	authErr := fmt.Errorf("IMAP 登录失败: %w", mail.ErrAuthFailed)
	key := mailAuthKey(acc)

	for i := 0; i < mailAuthFailThreshold-1; i++ {
		m.noteMailAuth(key, authErr)
	}
	m.noteMailAuth(key, errors.New("dial tcp: i/o timeout")) // 网络错误既不计数也不清零
	if m.MailAuthFailing(acc) {
		t.Fatal("未达阈值不应判定故障")
	}
	m.noteMailAuth(key, authErr)
	if !m.MailAuthFailing(acc) {
		t.Fatal("连续认证失败达阈值应判定故障")
	}
	sum := acc.Summary()
	m.MarkMailAuth(&sum, acc)
	if !sum.MailAuthFailed || sum.StatusMessage == "" {
		t.Fatalf("摘要应标注收信故障: %+v", sum)
	}

	rotated := *acc
	rotated.AppPassword = "new-pass"
	if m.MailAuthFailing(&rotated) {
		t.Fatal("更换授权码后应视为恢复")
	}
	m.noteMailAuth(key, nil)
	if m.MailAuthFailing(acc) {
		t.Fatal("一次成功读取应清零")
	}
	if m.MailAuthFailing(&Account{ID: "cookie_only", Status: "active"}) {
		t.Fatal("未配置 IMAP 的账号不应判定收信故障")
	}
}
