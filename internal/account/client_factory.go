/**
 * [INPUT]: 依赖 internal/hme, internal/mail, time, strings, fmt
 * [OUTPUT]: 对外提供 HMEClient, HMEClientWithPassword, MailClient, WithMailClient, WebMailClient
 * [POS]: internal/account 的外设客户端装配与连接池驱动工厂
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package account

import (
	"context"
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
	if len(snap.Cookies) == 0 {
		return nil, fmt.Errorf("账号未配置 Cookie，无法使用 HME 功能")
	}
	c, err := hme.NewClient(snap.Cookies, snap.Host, snap.Proxy, verbose)
	if err != nil {
		return nil, err
	}
	if snap.ServiceURL != "" {
		c.SetServiceURL(snap.ServiceURL)
	}
	return c, nil
}

// HMEClientWithPassword 为指定账号创建一个新的 HME 客户端,使用账号密码登录。
// 登录成功后会自动获取 Cookie 并保存到账号配置。
func (m *Manager) HMEClientWithPassword(id, password string, otpProvider hme.OTPProvider) (*hme.Client, error) {
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

	email := snap.ICloudEmail
	if email == "" {
		email = snap.RealEmail
	}
	if email == "" {
		return nil, fmt.Errorf("账号未设置邮箱地址")
	}

	// verbose 必须为 false:该模式会把 Cookie 请求头明文打进 stdout(进程日志),
	// 密码登录路径必然携带 X-APPLE-WEBAUTH-* 会话凭据。
	client, err := hme.NewClient(nil, snap.Host, snap.Proxy, false)
	if err != nil {
		return nil, err
	}

	if err := client.Login(email, password, otpProvider); err != nil {
		return nil, err
	}

	// 先保存 accountLogin 返回的 Cookie，随后通过 validate 刷新会话并再次持久化。
	// 国区与美区都走同一条刷新链路，避免只保存登录阶段的临时 token。
	if err := m.SaveCookies(id, client.CookieSnapshot()); err != nil {
		return nil, err
	}
	if err := client.ValidateSession(); err != nil {
		// validate 的失败响应也可能携带 Set-Cookie，尽量保留服务端最新状态。
		_ = m.SaveCookies(id, client.CookieSnapshot())
		return nil, err
	}

	// 保存 validate 刷新后的 Cookie 和账号状态。
	m.mu.Lock()
	cur, ok := m.accounts[id]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("账号不存在: %s", id)
	}
	cur.Cookies = client.CookieSnapshot()
	cur.Status = "active"
	cur.LastValidated = time.Now().Format(time.RFC3339)
	cur.LastError = ""
	if info := client.AccountInfo(); info != nil {
		cur.RealEmail = firstNonEmpty(info.AppleID, info.PrimaryEmail)
		if cur.ICloudEmail == "" {
			cur.ICloudEmail = deriveICloudEmail(info)
		}
	}
	saveErr := m.saveAccount(cur)
	m.mu.Unlock()
	if saveErr != nil {
		return nil, saveErr
	}

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
	if snap.Mailbox != nil && snap.Mailbox.Email != "" && snap.Mailbox.Password != "" {
		mc := mail.NewClientWithServer(snap.Mailbox.Email, snap.Mailbox.Password, snap.Mailbox.IMAPHost, snap.Mailbox.IMAPPort)
		if snap.Proxy != "" {
			mc.SetProxy(snap.Proxy)
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
	return mail.NewClientWithProxy(imapEmail, snap.AppPassword, snap.Proxy), nil
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
		mc := mail.NewClientWithServer(mailbox.Email, mailbox.Password, mailbox.IMAPHost, mailbox.IMAPPort)
		if proxyURL != "" && os.Getenv("ICLOUD_HME_IMAP_DIRECT") != "true" && os.Getenv("ICLOUD_HME_IMAP_DIRECT") != "1" {
			mc.SetProxy(proxyURL)
		}
		if err := mc.Connect(); err != nil {
			return err
		}
		defer mc.Disconnect()

		stopWatch := make(chan struct{})
		defer close(stopWatch)
		go func() {
			select {
			case <-ctx.Done():
				mc.SetDeadline(time.Now())
				mc.ForceClose()
			case <-stopWatch:
			}
		}()

		mc.SetDeadline(time.Now().Add(mail.IMAPCommandTimeout))
		defer mc.SetDeadline(time.Time{})
		err := fn(mc)
		if ctxErr := ctx.Err(); ctxErr != nil {
			mc.ForceClose()
			return ctxErr
		}
		return err
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
