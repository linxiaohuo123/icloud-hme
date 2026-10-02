/**
 * [INPUT]: 依赖 crypto/sha256, errors, fmt, strings, icloud-hme/internal/mail
 * [OUTPUT]: 对外提供 Manager.MailAuthFailing 收信认证健康度查询与 Manager.MarkMailAuth 摘要标注
 * [POS]: internal/account 的收信链路健康度：按物理邮箱凭据指纹记录连续认证失败，供出号排除与告警使用
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package account

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"icloud-hme/internal/mail"
)

// mailAuthFailThreshold 物理邮箱连续被服务端拒绝认证达到此次数，视为收信链路故障。
// ponytail: 仅内存计数，重启后归零，由下一轮监控探测重新判定。
const mailAuthFailThreshold = 3

// mailAuthKey 返回账号当前收信凭据的指纹；改授权码或换绑邮箱后指纹随之变化，旧失败记录自然失效。
func mailAuthKey(acc *Account) string {
	email, host, password := "", mail.IMAPServer, ""
	port := mail.IMAPPort
	if mb := acc.Mailbox; mb != nil && mb.Email != "" && mb.Password != "" {
		email, host, port, password = mb.Email, mb.IMAPHost, mb.IMAPPort, mb.Password
	} else {
		var err error
		if email, password, _, err = nativeIMAPCreds(acc); err != nil {
			return ""
		}
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%s", strings.ToLower(email), host, port, password))))
}

// noteMailAuth 成功即清零；仅服务端明确拒绝认证才累计，网络/超时/业务错误不改变健康度。
func (m *Manager) noteMailAuth(key string, err error) {
	if key == "" || (err != nil && !errors.Is(err, mail.ErrAuthFailed)) {
		return
	}
	m.mailAuthMu.Lock()
	defer m.mailAuthMu.Unlock()
	if err == nil {
		delete(m.mailAuthFails, key)
		return
	}
	if m.mailAuthFails == nil {
		m.mailAuthFails = make(map[string]int)
	}
	m.mailAuthFails[key]++
}

// MailAuthFailing 报告账号当前收信凭据是否已连续被拒绝认证。
func (m *Manager) MailAuthFailing(acc *Account) bool {
	key := mailAuthKey(acc)
	if key == "" {
		return false
	}
	m.mailAuthMu.Lock()
	defer m.mailAuthMu.Unlock()
	return m.mailAuthFails[key] >= mailAuthFailThreshold
}

// MarkMailAuth 把收信认证故障写入摘要，管理台账号列表直接显示原因。
func (m *Manager) MarkMailAuth(sum *Summary, acc *Account) {
	if !m.MailAuthFailing(acc) {
		return
	}
	sum.MailAuthFailed = true
	if sum.StatusMessage == "" {
		sum.StatusMessage = "收信邮箱认证连续失败，已暂停出号，请检查授权码"
	}
}
