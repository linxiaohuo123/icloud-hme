/**
 * [INPUT]: 依赖 os, filepath, sync, github.com/google/uuid, icloud-hme/internal/mail, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 Account, MailboxConfig, Manager, NewManager
 * [POS]: internal/account 的核心账号管理器与状态机；业务逻辑拆解至 manager_validate.go
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

// Package account 实现多账号管理器。
//
// 负责账号 CRUD、状态机管理、与持久化存储配合。对应原 Python 项目 account_manager.py。
package account

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

// MaxAliasesPerAccount 是 Apple 官方单个 iCloud 账号的 Hide My Email 别名物理上限 (实测与官方实践为 750 个)。
const MaxAliasesPerAccount = 750

// Account 描述一个 iCloud 账号。
type Account struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	RealEmail     string            `json:"real_email"`
	ICloudEmail   string            `json:"icloud_email"`
	Cookies       map[string]string `json:"cookies"`
	Host          string            `json:"host"`
	ServiceURL    string            `json:"service_url,omitempty"` // 已解析的 HME 服务端点
	Proxy         string            `json:"proxy,omitempty"`       // HTTP/SOCKS5 代理
	AppPassword   string            `json:"app_password,omitempty"`
	Mailbox       *MailboxConfig    `json:"mailbox,omitempty"`
	Status        string            `json:"status"` // active / error
	AliasTotal    int               `json:"alias_total"`
	AliasActive   int               `json:"alias_active"`
	LastValidated string            `json:"last_validated"`
	LastError     string            `json:"last_error,omitempty"`
	CreatedAt     string            `json:"created_at"`
	Tags          []string          `json:"tags,omitempty"`
}

// MailboxConfig describes an external mailbox used to receive forwarded mail.
type MailboxConfig struct {
	Provider string `json:"provider"`
	Email    string `json:"email"`
	IMAPHost string `json:"imap_host"`
	IMAPPort int    `json:"imap_port"`
	Password string `json:"password,omitempty"`
}

// Manager 管理多个 iCloud 账号,线程安全。
type Manager struct {
	mu       sync.RWMutex
	accounts map[string]*Account
	dataDir  string
	dataFile string
	store    *store.Store // SQLite 持久化后端
	imapPool *mail.Pool   // IMAP 长连接池
	hmePool  *hmeClientPool
}

// NewManager 创建管理器。st 为 SQLite 持久化后端，dataDir 用于存放 IMAP 等资源。
func NewManager(dataDir string, st *store.Store) (*Manager, error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, err
	}
	m := &Manager{
		accounts: make(map[string]*Account),
		dataDir:  dataDir,
		dataFile: filepath.Join(dataDir, "accounts.json"),
		store:    st,
		imapPool: mail.NewPool(),
		hmePool:  newHMEClientPool(),
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

// Close 释放 IMAP 连接池与 HME 客户端池等资源。
func (m *Manager) Close() {
	if m.imapPool != nil {
		m.imapPool.Close()
	}
	if m.hmePool != nil {
		m.hmePool.Close()
	}
}

// Store 返回底层绑定的 SQLite Store 实例（可能为 nil）。
func (m *Manager) Store() *store.Store {
	return m.store
}

// Reload 重新加载账号数据，并平滑重置 IMAP 连接池。
func (m *Manager) Reload() error {
	// 先在锁内摘除旧池（持锁时间极短），再在锁外关闭。
	// Pool.Close() 对每个连接执行阻塞 LOGOUT（单连接上限 IMAPCommandTimeout=90s，
	// 最多 50 连接），若持 m.mu 关闭会把全站 API（几乎所有 handler 都走 RLock）冻结数分钟。
	m.mu.Lock()
	oldPool := m.imapPool
	m.imapPool = nil
	m.mu.Unlock()
	if oldPool != nil {
		oldPool.Close()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	return m.load()
}

// AddAccount 添加一个账号。cookieInput 可为空,后续可通过 /login 获取。
//
// cookieInput 支持 Header String 或 JSON。校验失败仍会保存账号(status=error),
// 方便用户后续修正 Cookie 后重新校验。
//
// 兼容入口:新调用方请使用 AddAccountWithInput。
func (m *Manager) AddAccount(name, cookieInput, host, proxy string) (*Account, error) {
	if host == "" {
		host = "icloud.com"
	}
	acc, err := m.newAccount(name, "", cookieInput, host, proxy)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.accounts[acc.ID] = acc
	saveErr := m.saveAccount(acc)
	if saveErr != nil && m.store != nil {
		delete(m.accounts, acc.ID)
	}
	m.mu.Unlock()
	if saveErr != nil {
		return nil, saveErr
	}
	return acc, nil
}

// AddAccountWithInput 添加账号(带完整校验)。
//
// 无 Cookie 的添加路径不访问网络;有 Cookie 时在锁外对快照执行会话校验。
func (m *Manager) AddAccountWithInput(input AddAccountInput) (Summary, error) {
	name, err := validateName(input.Name)
	if err != nil {
		return Summary{}, err
	}
	if err := validateEmail(input.ICloudEmail); err != nil {
		return Summary{}, err
	}
	host, err := validateHost(input.Host)
	if err != nil {
		return Summary{}, err
	}
	proxy, err := validateProxy(input.Proxy)
	if err != nil {
		return Summary{}, err
	}
	acc, err := m.newAccount(name, input.ICloudEmail, input.CookieInput, host, proxy)
	if err != nil {
		return Summary{}, err
	}
	acc.Tags = input.Tags
	m.mu.Lock()
	m.accounts[acc.ID] = acc
	saveErr := m.saveAccount(acc)
	if saveErr != nil && m.store != nil {
		// SQLite 落库失败必须回滚内存：否则接口报错但账号已经可见，
		// 用户重试会产生重复账号，重启后内存态又凭空消失。
		delete(m.accounts, acc.ID)
	}
	m.mu.Unlock()
	if saveErr != nil {
		return Summary{}, saveErr
	}
	return acc.Summary(), nil
}

// newAccount 构造账号;cookieInput 非空时在锁外对快照执行会话校验。
func (m *Manager) newAccount(name, icloudEmail, cookieInput, host, proxy string) (*Account, error) {
	var cookies map[string]string
	if cookieInput != "" {
		var err error
		cookies, err = ParseCookieInput(cookieInput)
		if err != nil {
			return nil, err
		}
	} else {
		cookies = make(map[string]string)
	}

	acc := &Account{
		ID:          "acc_" + uuid.New().String()[:8],
		Name:        name,
		RealEmail:   icloudEmail,
		ICloudEmail: icloudEmail,
		Cookies:     cookies,
		Host:        host,
		Proxy:       proxy,
		Status:      "pending", // 无 Cookie 时为 pending
		CreatedAt:   time.Now().Format(time.RFC3339),
	}

	// 有 Cookie 才校验会话
	if len(cookies) > 0 {
		acc.validateCookies()
	}
	return acc, nil
}

// validateCookies 用 Cookie 校验会话并填充账号身份(在锁外对快照操作)。
func (a *Account) validateCookies() {
	host := a.Host
	if host == "" {
		host = "icloud.com"
	}
	client, err := hme.NewClient(a.Cookies, host, a.Proxy, false)
	if err != nil {
		a.Status = "error"
		a.LastError = truncate(err.Error(), 300)
		return
	}
	defer client.Close()
	if err := client.ValidateSession(); err != nil {
		// validate 即使失败也可能通过 Set-Cookie 刷新部分会话状态。
		a.Cookies = client.CookieSnapshot()
		a.Status = "error"
		a.LastError = truncate(err.Error(), 300)
		return
	}
	// 显式接收 validate 刷新的 Cookie，不依赖传入 map 的引用关系。
	a.Cookies = client.CookieSnapshot()
	a.Status = "active"
	if serviceURL := client.ServiceURL(); serviceURL != "" {
		a.ServiceURL = serviceURL
	}
	if info := client.AccountInfo(); info != nil {
		a.RealEmail = firstNonEmpty(info.AppleID, info.PrimaryEmail)
		if a.ICloudEmail == "" {
			a.ICloudEmail = deriveICloudEmail(info)
		}
	}
	if aliases, err := client.ListAliases(); err == nil {
		a.AliasTotal = len(aliases)
		a.AliasActive = 0
		for _, al := range aliases {
			if al.Active {
				a.AliasActive++
			}
		}
	}
	a.LastValidated = time.Now().Format(time.RFC3339)
}

// UpdateMetadata 编辑账号基本信息(名称、iCloud 邮箱、主机、业务标签),至少提供一个字段。
func (m *Manager) UpdateMetadata(id string, input UpdateAccountInput) (Summary, error) {
	if input.Name == nil && input.ICloudEmail == nil && input.Host == nil && input.Tags == nil {
		return Summary{}, fmt.Errorf("至少需要提供一个可编辑字段")
	}
	var name, email, host *string
	if input.Name != nil {
		v, err := validateName(*input.Name)
		if err != nil {
			return Summary{}, err
		}
		name = &v
	}
	if input.ICloudEmail != nil {
		if err := validateEmail(*input.ICloudEmail); err != nil {
			return Summary{}, err
		}
		v := strings.TrimSpace(*input.ICloudEmail)
		email = &v
	}
	if input.Host != nil {
		v, err := validateHost(*input.Host)
		if err != nil {
			return Summary{}, err
		}
		host = &v
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return Summary{}, fmt.Errorf("账号不存在: %s", id)
	}
	if name != nil {
		acc.Name = *name
	}
	if email != nil {
		acc.ICloudEmail = *email
	}
	if host != nil {
		acc.Host = *host
	}
	if input.Tags != nil {
		acc.Tags = *input.Tags
	}
	if err := m.saveAccount(acc); err != nil {
		return Summary{}, err
	}
	return acc.Summary(), nil
}

// UpdateProxy 更新或清除账号代理。空字符串表示清除。
func (m *Manager) UpdateProxy(id, proxy string) (Summary, error) {
	proxy, err := validateProxy(proxy)
	if err != nil {
		return Summary{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return Summary{}, fmt.Errorf("账号不存在: %s", id)
	}
	acc.Proxy = proxy
	if err := m.saveAccount(acc); err != nil {
		return Summary{}, err
	}
	return acc.Summary(), nil
}

// RemoveAccount 删除账号。
//
// 落库失败时回滚内存删除：否则接口报成功、账号仍留在 DB，重启后会「复活」成
// 幽灵账号并被调度器继续选中发号。
func (m *Manager) RemoveAccount(id string) bool {
	m.mu.Lock()
	acc, ok := m.accounts[id]
	if !ok {
		m.mu.Unlock()
		return false
	}
	delete(m.accounts, id)
	err := m.deleteAccountFromStore(id)
	if err != nil {
		m.accounts[id] = acc
		m.mu.Unlock()
		return false
	}
	m.mu.Unlock()

	// 账号已删除: 丢弃其缓存的 HME 客户端(锁外执行，避免与池内锁形成反转)
	if m.hmePool != nil {
		m.hmePool.drop(id)
	}
	return true
}

// GetAccount 返回账号深拷贝(含 Cookies),调用方可安全使用。
func (m *Manager) GetAccount(id string) (*Account, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	acc, ok := m.accounts[id]
	if !ok {
		return nil, false
	}
	return copyAccount(acc), true
}

// ListAccounts 返回所有账号的深拷贝(脱敏,不含 Cookies),按活跃状态排序。
// 兼容入口:新调用方请使用 ListSummaries。
func (m *Manager) ListAccounts() []*Account {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Account, 0, len(m.accounts))
	for _, acc := range m.accounts {
		if acc == nil {
			continue
		}
		cp := copyAccount(acc)
		cp.Cookies = nil
		cp.AppPassword = ""
		if acc.Mailbox != nil {
			mailbox := *acc.Mailbox
			mailbox.Password = ""
			cp.Mailbox = &mailbox
		}
		out = append(out, cp)
	}
	return out
}

// ListSummaries 返回所有账号的安全摘要,排序为 active → pending → error,
// 同状态按 name、id 升序。
func (m *Manager) ListSummaries() []Summary {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Summary, 0, len(m.accounts))
	for _, acc := range m.accounts {
		if acc == nil {
			continue
		}
		out = append(out, acc.Summary())
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := statusRank(out[i].Status), statusRank(out[j].Status)
		if ri != rj {
			return ri < rj
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// statusRank 返回状态的排序权重。
func statusRank(status string) int {
	switch status {
	case "active":
		return 0
	case "pending":
		return 1
	default:
		return 2
	}
}

// SetMailbox validates and stores an external IMAP mailbox after testing it.
func (m *Manager) SetMailbox(id string, config MailboxConfig) error {
	config.Provider = strings.TrimSpace(config.Provider)
	config.Email = strings.TrimSpace(config.Email)
	config.IMAPHost = strings.TrimSpace(config.IMAPHost)
	if config.Email == "" || config.IMAPHost == "" || config.Password == "" {
		return fmt.Errorf("收件邮箱、IMAP 服务器和授权码不能为空")
	}
	if strings.Contains(config.IMAPHost, "://") || config.IMAPPort < 1 || config.IMAPPort > 65535 {
		return fmt.Errorf("IMAP 服务器或端口无效")
	}
	m.mu.RLock()
	acc, ok := m.accounts[id]
	var proxyURL string
	if ok {
		proxyURL = acc.Proxy
	}
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	mc := mail.NewClientWithServer(config.Email, config.Password, config.IMAPHost, config.IMAPPort)
	if proxyURL != "" {
		mc.SetProxy(proxyURL)
	}
	if err := mc.Connect(); err != nil {
		return err
	}
	_, err := mc.InboxCount()
	mc.Disconnect()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok = m.accounts[id]
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	acc.Mailbox = &config
	return m.saveAccount(acc)
}

// SetAppPassword 设置 iCloud 邮箱和 App 专用密码,并测试 IMAP 连接。
func (m *Manager) SetAppPassword(id, icloudEmail, appPassword string) error {
	if icloudEmail == "" {
		return fmt.Errorf("iCloud 邮箱不能为空")
	}
	if appPassword == "" {
		return fmt.Errorf("App 专用密码不能为空")
	}

	m.mu.RLock()
	acc, ok := m.accounts[id]
	var proxyURL string
	if ok {
		proxyURL = acc.Proxy
	}
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}

	// 测试连接(锁外，透传账号配置的代理)
	var mc *mail.Client
	if proxyURL != "" {
		mc = mail.NewClientWithProxy(icloudEmail, appPassword, proxyURL)
	} else {
		mc = mail.NewClient(icloudEmail, appPassword)
	}
	if err := mc.Connect(); err != nil {
		return err
	}
	count, err := mc.InboxCount()
	mc.Disconnect()
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok = m.accounts[id]
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	acc.ICloudEmail = icloudEmail
	acc.AppPassword = appPassword
	if err := m.saveAccount(acc); err != nil {
		return err
	}
	_ = count
	return nil
}

// SaveCookies 保存指定账号的最新 Cookie（HMEClient 操作后刷新的 token）。
// 用于客户端 validate/操作过程中从 Set-Cookie 获取了新 token 后持久化。
func (m *Manager) SaveCookies(id string, cookies map[string]string) error {
	return m.SaveSession(id, cookies, "")
}

// SaveSession 保存指定账号的最新 Cookie 与已解析的服务端点。
func (m *Manager) SaveSession(id string, cookies map[string]string, serviceURL string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	if cookies != nil {
		acc.Cookies = cloneCookies(cookies)
	}
	if serviceURL != "" {
		acc.ServiceURL = serviceURL
	}
	return m.saveAccount(acc)
}

// UpdateAliasCounts 更新指定账号的别名统计数据并持久化。
func (m *Manager) UpdateAliasCounts(id string, total, active int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	acc.AliasTotal = total
	acc.AliasActive = active
	if m.store != nil {
		return m.store.UpdateAccountFields(id, map[string]interface{}{
			"alias_total":  total,
			"alias_active": active,
		})
	}
	return m.saveJSON()
}

// AdjustAliasCounts 增量调整指定账号的别名统计数据并持久化。
func (m *Manager) AdjustAliasCounts(id string, deltaTotal, deltaActive int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	acc.AliasTotal += deltaTotal
	if acc.AliasTotal < 0 {
		acc.AliasTotal = 0
	}
	acc.AliasActive += deltaActive
	if acc.AliasActive < 0 {
		acc.AliasActive = 0
	}
	if m.store != nil {
		return m.store.UpdateAccountFields(id, map[string]interface{}{
			"alias_total":  acc.AliasTotal,
			"alias_active": acc.AliasActive,
		})
	}
	return m.saveJSON()
}
