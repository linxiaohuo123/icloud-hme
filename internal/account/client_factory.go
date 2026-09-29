/**
 * [INPUT]: 依赖 internal/hme, internal/mail, time, strings, fmt
 * [OUTPUT]: 对外提供 HMEClient, HMEClientWithPassword, MailClient, WithMailClient, WebMailClient
 * [POS]: internal/account 的外设客户端装配与连接池驱动工厂，密码登录回写按凭据代际校验
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package account

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
)

// HMEClient 为指定账号创建一个新的 HME 客户端。
// 必须有有效的 Cookie 才能使用 HME 功能。
func (m *Manager) HMEClient(id string, verbose bool) (*hme.Client, error) {
	m.mu.RLock()
	acc, ok := m.accounts[id]
	var snap *Account
	if ok {
		snap = copyAccount(acc)
	}
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("账号不存在: %s", id)
	}
	if len(snap.Cookies) == 0 && snap.Session == nil {
		return nil, fmt.Errorf("账号未配置 Cookie，无法使用 HME 功能")
	}
	c, err := hme.NewClientWithSession(snap.Cookies, snap.Session, snap.Host, snap.Proxy, verbose)
	if err != nil {
		return nil, err
	}
	if snap.ServiceURL != "" {
		c.SetServiceURL(snap.ServiceURL)
	}
	return c, nil
}

// HMEClientWithPassword 为指定账号创建一个新的 HME 客户端,使用账号密码登录。
// 支持两阶段 2FA 认证挂起与恢复；登录成功后会自动获取 Cookie 并保存到账号配置。
func (m *Manager) HMEClientWithPassword(id, password string, otpProvider hme.OTPProvider) (*hme.Client, error) {
	entry := m.hmePool.acquire(id)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	m.mu.RLock()
	acc, ok := m.accounts[id]
	var snap *Account
	if ok {
		snap = copyAccount(acc)
	}
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("账号不存在: %s", id)
	}

	var client *hme.Client
	keepClient := false

	m.mu.Lock()
	// 清理已超过 5 分钟的过期挂起登录
	for k, pl := range m.pendingLogins {
		if time.Since(pl.createdAt) > 5*time.Minute {
			pl.client.Close()
			delete(m.pendingLogins, k)
		}
	}

	pl, hasPending := m.pendingLogins[id]
	if hasPending {
		if otpProvider != nil && time.Since(pl.createdAt) < 5*time.Minute {
			client = pl.client
		} else {
			delete(m.pendingLogins, id)
			pl.client.Close()
		}
	}
	m.mu.Unlock()

	defer func() {
		if !keepClient && client != nil {
			client.Close()
		}
	}()

	if client != nil {
		// 复用挂起的会话提交 2FA 验证码
		otp, err := otpProvider()
		if err != nil {
			return nil, fmt.Errorf("获取 2FA 验证码失败: %w", err)
		}
		if err := client.SubmitOTP(otp); err != nil {
			// 输错验证码时保留挂起会话，不关闭 client，允许用户修正验证码后在同一会话中重试
			keepClient = true
			return nil, err
		}
		// 验证成功，从挂起池中移除
		m.mu.Lock()
		delete(m.pendingLogins, id)
		m.mu.Unlock()
	} else {
		// 全新登录握手
		email := snap.ICloudEmail
		if email == "" {
			email = snap.RealEmail
		}
		if email == "" {
			return nil, fmt.Errorf("账号未设置邮箱地址")
		}

		c, err := hme.NewClient(nil, snap.Host, snap.Proxy, false)
		if err != nil {
			return nil, err
		}
		client = c

		if err := client.Login(email, password, otpProvider); err != nil {
			if errors.Is(err, hme.ErrOTPRequired) || strings.Contains(err.Error(), "需要提供 OTP") {
				m.mu.Lock()
				if old, exists := m.pendingLogins[id]; exists {
					old.client.Close()
				}
				m.pendingLogins[id] = &pendingLogin{client: client, createdAt: time.Now()}
				m.mu.Unlock()
				keepClient = true // 挂起等待验证码，不在此次 defer 中关闭
			}
			return nil, err
		}
	}

	if err := client.ValidateSession(); err != nil {
		return nil, err
	}
	if err := verifyAppleIdentity(snap, client.AccountInfo()); err != nil {
		return nil, err
	}

	// 校验身份后一次保存完整会话，避免中途将另一 Apple 账号的 Cookie 绑定到旧库存。
	m.mu.Lock()
	cur, ok := m.accounts[id]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("账号不存在: %s", id)
	}
	if cur.credentialEpoch != snap.credentialEpoch || cur.Host != snap.Host || cur.Proxy != snap.Proxy {
		m.mu.Unlock()
		return nil, ErrSessionChanged
	}
	if err := verifyAppleIdentity(cur, client.AccountInfo()); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	old := *cur
	cur.Cookies = client.CookieSnapshot()
	cur.Session = client.SessionSnapshot()
	cur.ServiceURL = client.ServiceURL()
	cur.Status = "active"
	cur.LastValidated = time.Now().Format(time.RFC3339)
	cur.LastError = ""
	if info := client.AccountInfo(); info != nil {
		cur.AppleDSID = info.DSID
		cur.RealEmail = firstNonEmpty(info.AppleID, info.PrimaryEmail)
		if cur.ICloudEmail == "" {
			cur.ICloudEmail = deriveICloudEmail(info)
		}
	}
	saveErr := m.saveAccount(cur)
	if saveErr != nil {
		*cur = old
	} else {
		cur.credentialEpoch++
	}
	m.mu.Unlock()
	if saveErr != nil {
		return nil, saveErr
	}

	keepClient = true
	return client, nil
}

// MailClient 为指定账号创建 IMAP 邮件客户端(每次新建, 不走连接池)。
// 需要事先设置 iCloud 邮箱和 App 专用密码。
// 高频读信请用 WithMailClient 复用长连接。
func (m *Manager) MailClient(id string) (*mail.Client, error) {
	m.mu.RLock()
	acc, ok := m.accounts[id]
	var snap *Account
	if ok {
		snap = copyAccount(acc)
	}
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("账号不存在: %s", id)
	}
	proxy := snap.Proxy
	if os.Getenv("ICLOUD_HME_IMAP_DIRECT") == "true" || os.Getenv("ICLOUD_HME_IMAP_DIRECT") == "1" {
		proxy = ""
	}
	if snap.Mailbox != nil && snap.Mailbox.Email != "" && snap.Mailbox.Password != "" {
		mc := mail.NewClientWithServer(snap.Mailbox.Email, snap.Mailbox.Password, snap.Mailbox.IMAPHost, snap.Mailbox.IMAPPort)
		if proxy != "" {
			mc.SetProxy(proxy)
		}
		return mc, nil
	}
	imapEmail := snap.ICloudEmail
	if imapEmail == "" {
		imapEmail = snap.RealEmail
	}
	if !isICloudDomain(imapEmail) {
		return nil, fmt.Errorf("账号未设置 iCloud 邮箱 (当前: %s)", imapEmail)
	}
	if snap.AppPassword == "" {
		return nil, fmt.Errorf("账号未设置 App 专用密码")
	}
	return mail.NewClientWithProxy(imapEmail, snap.AppPassword, proxy), nil
}

// WithMailClientContext 使用连接池中的长连接执行 fn，支持真实 Context 超时与取消 (Issue 13)。
func (m *Manager) WithMailClientContext(ctx context.Context, id string, fn func(*mail.Client) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.RLock()
	acc, ok := m.accounts[id]
	var mailbox *MailboxConfig
	var proxyURL string
	if ok {
		if acc.Mailbox != nil {
			copy := *acc.Mailbox
			mailbox = &copy
		}
		proxyURL = acc.Proxy
	}
	m.mu.RUnlock()
	if mailbox != nil && mailbox.Email != "" && mailbox.Password != "" {
		if os.Getenv("ICLOUD_HME_IMAP_DIRECT") == "true" || os.Getenv("ICLOUD_HME_IMAP_DIRECT") == "1" {
			proxyURL = ""
		}
		return m.getIMAPPool().DoContextWithServer(ctx, mailbox.Email, mailbox.Password, mailbox.IMAPHost, mailbox.IMAPPort, proxyURL, fn)
	}
	imapEmail, appPassword, proxyURL, err := m.imapCreds(id)
	if err != nil {
		return err
	}
	return m.getIMAPPool().DoContext(ctx, imapEmail, appPassword, proxyURL, fn)
}

// WithMailClient 使用连接池中的长连接执行 fn(串行/账号级)。
// fn 返回后连接保留在池中, 不会 Logout。
func (m *Manager) WithMailClient(id string, fn func(*mail.Client) error) error {
	return m.WithMailClientContext(context.Background(), id, fn)
}

func (m *Manager) getIMAPPool() *mail.Pool {
	m.mu.RLock()
	p := m.imapPool
	m.mu.RUnlock()
	if p != nil {
		return p
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.imapPool == nil {
		m.imapPool = mail.NewPool()
	}
	return m.imapPool
}

func (m *Manager) imapCreds(id string) (imapEmail, appPassword, proxyURL string, err error) {
	m.mu.RLock()
	acc, ok := m.accounts[id]
	var snap *Account
	if ok {
		snap = copyAccount(acc)
	}
	m.mu.RUnlock()
	if !ok {
		return "", "", "", fmt.Errorf("账号不存在: %s", id)
	}
	imapEmail = snap.ICloudEmail
	if imapEmail == "" {
		imapEmail = snap.RealEmail
	}
	if !isICloudDomain(imapEmail) {
		return "", "", "", fmt.Errorf("账号未设置 iCloud 邮箱 (当前: %s)", imapEmail)
	}
	if snap.AppPassword == "" {
		return "", "", "", fmt.Errorf("账号未设置 App 专用密码")
	}
	proxy := snap.Proxy
	if os.Getenv("ICLOUD_HME_IMAP_DIRECT") == "true" || os.Getenv("ICLOUD_HME_IMAP_DIRECT") == "1" {
		proxy = ""
	}
	return imapEmail, snap.AppPassword, proxy, nil
}

// WebMailClient 为指定账号创建 Web 邮件客户端。
// 使用 Cookie 认证，无需 App Password。
func (m *Manager) WebMailClient(id string) (*mail.WebClient, error) {
	m.mu.RLock()
	acc, ok := m.accounts[id]
	var snap *Account
	if ok {
		snap = copyAccount(acc)
	}
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("账号不存在: %s", id)
	}
	if len(snap.Cookies) == 0 {
		return nil, fmt.Errorf("账号未配置 Cookie，无法读取邮件")
	}
	// 从 cookies 中获取 dsid
	dsid := ""
	if v, ok := snap.Cookies["X-APPLE-WEBAUTH-USER"]; ok {
		// 解析 "v=1:s=1:d=22789132008" 或含后续字段格式
		parts := strings.Split(v, ":d=")
		if len(parts) == 2 {
			dsid = strings.Trim(strings.Split(parts[1], ":")[0], `"`)
		}
	}
	return mail.NewWebClient(snap.Cookies, dsid, snap.Host, snap.Proxy)
}
