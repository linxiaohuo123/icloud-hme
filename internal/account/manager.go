/**
 * [INPUT]: 依赖 os, filepath, sync, github.com/google/uuid, icloud-hme/internal/mail
 * [OUTPUT]: 对外提供 Account, MailboxConfig, Manager, NewManager
 * [POS]: internal/account 的核心账号管理器与状态机；ValidateAccount 在 ListAliases 之后快照 Cookie，UpdateCookies 零别名计数也落盘
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

// Package account 实现多账号管理器。
//
// 负责账号 CRUD、状态机管理、与持久化存储配合。对应原 Python 项目 account_manager.py。
package account

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

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

// UpdateCookies 更新指定账号的 Cookie,并自动校验会话有效性。
func (m *Manager) UpdateCookies(id string, cookies map[string]string) error {
	if len(cookies) == 0 {
		return fmt.Errorf("cookies 不能为空")
	}
	m.mu.RLock()
	acc, ok := m.accounts[id]
	var snap *Account
	if ok {
		snap = copyAccount(acc)
	}
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}

	// 自动校验 Cookie 是否有效(锁外对快照操作)
	snap.Cookies = cookies
	if snap.Host == "" {
		snap.Host = "icloud.com"
	}
	aliasesFetched := false
	client, err := hme.NewClient(cookies, snap.Host, snap.Proxy, false)
	if err != nil {
		snap.Status = "error"
		snap.LastError = "创建客户端失败: " + err.Error()
	} else {
		defer client.Close()
		if err := client.ValidateSession(); err != nil {
			// validate 即使失败也可能通过 Set-Cookie 刷新部分会话状态。
			snap.Cookies = client.CookieSnapshot()
			snap.Status = "error"
			snap.LastError = "Cookie 校验失败: " + err.Error()
		} else {
			snap.Status = "active"
			snap.LastValidated = time.Now().Format(time.RFC3339)
			snap.LastError = ""
			if info := client.AccountInfo(); info != nil {
				snap.RealEmail = firstNonEmpty(info.AppleID, info.PrimaryEmail)
				if snap.ICloudEmail == "" {
					snap.ICloudEmail = deriveICloudEmail(info)
				}
			}
			if aliases, errList := client.ListAliases(); errList == nil {
				aliasesFetched = true
				snap.AliasTotal = len(aliases)
				snap.AliasActive = 0
				for _, al := range aliases {
					if al.Active {
						snap.AliasActive++
					}
				}
			}
			// ListAliases 之后再快照，避免丢掉列表阶段的 Set-Cookie
			snap.Cookies = client.CookieSnapshot()
		}
	}

	m.mu.Lock()
	cur, ok := m.accounts[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("账号不存在: %s", id)
	}
	cur.Cookies = snap.Cookies
	cur.Status = snap.Status
	cur.LastValidated = snap.LastValidated
	cur.LastError = snap.LastError
	cur.RealEmail = snap.RealEmail
	if cur.ICloudEmail == "" {
		cur.ICloudEmail = snap.ICloudEmail
	}
	if aliasesFetched {
		cur.AliasTotal = snap.AliasTotal
		cur.AliasActive = snap.AliasActive
	}
	saveErr := m.saveAccount(cur)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	return saveErr
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

// ErrCookieExpired 表示账号 Cookie 已被 Apple 判定失效(401/403)，需要人工更新。
// 后台健康监控依据本哨兵错误区分"凭据级失效"与"瞬时网络错误"。
var ErrCookieExpired = errors.New("cookie expired")

// ValidateAccount 对指定账号执行一次会话校验并刷新状态(供后台健康监控周期调用)。
//
// 成功: 状态回 active、刷新 LastValidated 与别名计数，并保存 validate 响应刷新的
// Cookie(等效一次会话保活)。
// 凭据级失败(401/403): 账号标记 error 并返回包装 ErrCookieExpired 的错误，
// 调度器与预热池会随之跳过该账号。
// 瞬时失败(网络抖动、超时): 不改变账号状态，返回原始错误由调用方记录。
func (m *Manager) ValidateAccount(id string) error {
	m.mu.RLock()
	acc, ok := m.accounts[id]
	if !ok {
		m.mu.RUnlock()
		return fmt.Errorf("账号不存在: %s", id)
	}
	if len(acc.Cookies) == 0 {
		m.mu.RUnlock()
		return fmt.Errorf("账号 %s 未配置 Cookie", id)
	}
	m.mu.RUnlock()

	// 走账号级客户端池: 本函数由 Cookie 监控器对每个账号每轮调用一次，
	// 若每次新建客户端就要为全部账号反复构造 Chrome TLS 指纹并重新握手。
	var (
		refreshedCookies map[string]string
		serviceURL       string
		accountInfo      *hme.AccountInfo
		aliases          []hme.Alias
	)
	err := m.WithHMEClient(id, func(client *hme.Client) error {
		if err := client.ValidateSession(); err != nil {
			return err
		}
		serviceURL = client.ServiceURL()
		accountInfo = client.AccountInfo()
		// 顺带刷新别名计数，让配额水位始终有近 30 分钟内的真实值
		if list, listErr := client.ListAliases(); listErr == nil {
			aliases = list
		}
		// 必须在 ListAliases 之后克隆: 列表接口也可能 Set-Cookie。
		// 若在 validate 后立刻快照，WithHMEClient 回写的是新 Cookie，
		// 本函数再用旧快照覆盖，会把池条目指纹打成脏值、下一轮拆掉长连接。
		// client 会原地写自己的 Cookies map，若直接共享引用，
		// 遍历(save 序列化/copyAccount)与写并发会触发 fatal error 杀死进程。
		refreshedCookies = client.CookieSnapshot()
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrHMEClientUnavailable) {
			m.markAccountError(id, "创建客户端失败")
			return fmt.Errorf("%w: %v", ErrCookieExpired, err)
		}
		if isAuthFailure(err.Error()) {
			m.markAccountError(id, "Cookie 已失效")
			return fmt.Errorf("%w: %v", ErrCookieExpired, err)
		}
		return fmt.Errorf("校验暂时失败: %w", err)
	}

	// 校验通过: 同步 validate 刷新的 Cookie 与账号身份（单次落库，杜绝重复写入）
	m.mu.Lock()
	cur, ok := m.accounts[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("账号不存在: %s", id)
	}
	if refreshedCookies != nil {
		cur.Cookies = refreshedCookies
	}
	cur.Status = "active"
	cur.LastValidated = time.Now().Format(time.RFC3339)
	cur.LastError = ""
	if serviceURL != "" {
		cur.ServiceURL = serviceURL
	}
	if accountInfo != nil {
		cur.RealEmail = firstNonEmpty(accountInfo.AppleID, accountInfo.PrimaryEmail)
		if cur.ICloudEmail == "" {
			cur.ICloudEmail = deriveICloudEmail(accountInfo)
		}
	}
	if aliases != nil {
		cur.AliasTotal = len(aliases)
		cur.AliasActive = 0
		for _, al := range aliases {
			if al.Active {
				cur.AliasActive++
			}
		}
	}
	saveErr := m.saveAccount(cur)
	m.mu.Unlock()
	return saveErr
}

// markAccountError 将账号标记为凭据失效并持久化(监控器专用，不改 LastValidated)。
func (m *Manager) markAccountError(id, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.accounts[id]
	if !ok {
		return
	}
	cur.Status = "error"
	cur.LastError = reason
	if m.store != nil {
		_ = m.store.UpdateAccountFields(id, map[string]interface{}{
			"status":     "error",
			"last_error": reason,
		})
		return
	}
	_ = m.saveJSON()
}

// isAuthFailure 判断上游错误是否为凭据级失效(401/403，hme 客户端不重试直接返回)。
func isAuthFailure(msg string) bool {
	return strings.Contains(msg, "HTTP 401") || strings.Contains(msg, "HTTP 403")
}

// ---- 辅助函数 ----

// deriveICloudEmail 从账号身份推导 iCloud 邮箱地址(用于 IMAP 登录)。
//
// 规则:
//  1. primaryEmail 是 @icloud.com/@me.com/@mac.com → 直接用
//  2. appleId 是上述域名 → 直接用
//  3. appleId 是第三方邮箱(如 @qq.com) → 取 local part 拼 @icloud.com
//  4. appleId 为纯数字/手机号，而 primaryEmail 是第三方邮箱 → 取 primaryEmail local part 拼 @icloud.com
func deriveICloudEmail(info *hme.AccountInfo) string {
	primary := strings.TrimSpace(info.PrimaryEmail)
	appleID := strings.TrimSpace(info.AppleID)

	if isICloudDomain(primary) {
		return primary
	}
	if isICloudDomain(appleID) {
		return appleID
	}
	target := appleID
	if !strings.Contains(target, "@") {
		target = primary
	}
	if strings.Contains(target, "@") {
		local := strings.SplitN(target, "@", 2)[0]
		return local + "@icloud.com"
	}
	return firstNonEmpty(primary, appleID)
}

func isICloudDomain(email string) bool {
	lower := strings.ToLower(strings.TrimSpace(email))
	return lower != "" && (strings.HasSuffix(lower, "@icloud.com") ||
		strings.HasSuffix(lower, "@me.com") ||
		strings.HasSuffix(lower, "@mac.com"))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// 回退到 UTF-8 字符边界, 避免把多字节中文截成乱码
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
