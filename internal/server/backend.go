/**
 * [INPUT]: 依赖 internal/account.Manager, internal/hme.Client, internal/mail.Client/WebClient
 * [OUTPUT]: 对外提供 Backend 接口、managerBackend 生产适配器与 BackendError 错误结构
 * [POS]: internal/server 的高层业务门面，隔离协议实现并传递 Camoufox 登录请求取消与任务回收
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

// Package server - 可替换业务接口与 Manager 适配器。
//
// Backend 边界固定为高层业务动作,不把具体 *hme.Client 或 *mail.Client 暴露给 handler。
package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

// BackendError 是后端返回的稳定错误,携带 HTTP 状态码与稳定错误码。
type BackendError struct {
	Status  int
	Code    string
	Message string
	Data    any
}

func (e *BackendError) Error() string { return e.Message }

// Backend 是可替换的业务接口;handler 只依赖本接口,测试使用内存 fake。
//
// 【返回切片的调用约定】实现可能返回**内部缓存切片**(如 ListAliases 的 TTL 缓存
// 快照、ListAccounts 的 1 秒快照)。调用方**只读**，禁止就地修改或排序 ——
// 否则既是数据竞争，又会污染缓存。需要改写字段时请自行复制后再改。
type Backend interface {
	ListAccounts() []account.Summary
	GetAccount(string) (account.Summary, error)
	AddAccount(account.AddAccountInput) (account.Summary, error)
	UpdateAccount(string, account.UpdateAccountInput) (account.Summary, error)
	UpdateProxy(string, string) (account.Summary, error)
	UpdateCookies(string, string) (account.Summary, error)
	SetAppPassword(string, string, string) (account.Summary, error)
	SetAppPasswordContext(context.Context, string, string, string) (account.Summary, error)
	SetMailbox(string, account.MailboxConfig) (account.Summary, error)
	SetMailboxContext(context.Context, string, account.MailboxConfig) (account.Summary, error)
	RemoveMailbox(string) (account.Summary, error)
	LoginAccount(string, string, string) (account.Summary, error)
	LoginAccountContext(context.Context, string, string, string) (account.Summary, error)
	CancelCamoufoxLogin(string, string) (bool, error)
	RemoveAccount(string) bool
	CreateAlias(string, string) (*hme.CreateResult, error)
	CreateAliasContext(context.Context, string, string) (*hme.CreateResult, error)
	CreateAliasForAllocationContext(context.Context, string, string, string) (*hme.CreateResult, error)
	CreateAliasForReplenishmentContext(context.Context, string, string) (*hme.CreateResult, error)
	BatchCreateAlias(string, int, string) (*BatchCreateResult, error)
	BatchCreateAliasContext(context.Context, string, int, string) (*BatchCreateResult, error)
	ListAliases(string) ([]hme.Alias, error)
	ListAliasesContext(context.Context, string) ([]hme.Alias, error)
	RefreshAliases(string) ([]hme.Alias, error)
	RefreshAliasesContext(context.Context, string) ([]hme.Alias, error)
	SetAliasActive(string, string, bool) (bool, error)
	SetAliasActiveContext(context.Context, string, string, bool) (bool, error)
	UpdateAlias(string, string, string, string) error
	BatchUpdateAliases(string, []string, string, string) (BatchUpdateResult, error)
	DeleteAlias(string, string) error
	ListInbox(InboxQuery) (InboxResult, error)
	ListInboxContext(context.Context, InboxQuery) (InboxResult, error)
	ListMailboxes(string) ([]mail.Folder, error)
	ListMailboxesContext(context.Context, string) ([]mail.Folder, error)
	GetMessage(string, string) (*mail.FullMessage, error)
	GetMessageContext(context.Context, string, string) (*mail.FullMessage, error)
	GetMessages(string, []mail.MessageRef) ([]*mail.FullMessage, error)
	GetMessagesContext(context.Context, string, []mail.MessageRef) ([]*mail.FullMessage, error)
	GetMailboxBoundary(string, string) (string, uint32, uint32, error)
	GetMailboxBoundaryContext(context.Context, string, string) (string, uint32, uint32, error)
	ScanMailboxUIDPage(context.Context, ScanPageQuery) (ScanPageResult, error)
	DeleteMessage(string, uint32) error
	ValidateAccount(string) error
	ValidateAccountContext(context.Context, string) error
	CheckProxy(string) (bool, int64, string, error)
	Reload() error
}

// managerBackend 是生产 Backend,包装 *account.Manager。
type managerBackend struct {
	mgr        *account.Manager
	store      *store.Store
	aliasMu    sync.RWMutex
	aliasCache map[string]*aliasCacheItem
	// onAliasesFetched 在成功拉取到某账号的别名列表后回调，用于自愈「别名 → 母号」路由。
	//
	// 有了它，凡是拉过一次别名的账号(启动预热 autoSyncAccounts、GUI 浏览、手动刷新)
	// 其全部别名都会立刻变得可路由，不必再依赖 mail_sync 里有上限的盲扫兜底。
	onAliasesFetched func(accountID string, aliases []hme.Alias)

	// summaryMu 保护 ListAccounts 的快照缓存。
	//
	// 该函数在高频路径上被反复调用(mail_sync 每 2 秒、cookie_monitor 每账号一次、
	// quick-create 每次请求、作业接口…)，而每次都要构造并排序 N 条 Summary ——
	// 2000 账号实测 1.8ms / 400KB 分配。加一层短 TTL 缓存把突发请求折叠成一次计算。
	summaryMu    sync.Mutex
	summaryCache []account.Summary
	summaryAt    time.Time

	// accountMutations 存储每个账号的互斥锁 (*sync.Mutex)，用于串行化单账号的 HME 写操作生命周期与未决门禁
	accountMutations sync.Map

	camoufoxMu    sync.Mutex
	camoufoxTasks map[string]*camoufoxPendingTask
}

type camoufoxPendingTask struct {
	taskID    string
	baseURL   string
	createdAt time.Time
	recovered bool
}

// summaryCacheTTL 是 ListAccounts 快照的有效期。
// 取值很短:它只用于负载均衡选号与列表展示，权威的 750 上限与配额校验另有专门路径。
const summaryCacheTTL = time.Second

// ListAccounts 返回账号安全摘要列表。
//
// 别名计数直接读缓存里已算好的值(O(N))。绝不可在此现场遍历别名求和:
// 该函数被 mail_sync(每 2 秒)、cookie_monitor(每账号一次)、job/quick-create 等
// 高频路径调用，2000 账号 × 200 别名会让每次调用退化成 40 万次迭代。
//
// 【调用约定】返回的切片由内部缓存持有，调用方**只读**，禁止就地修改/排序，
// 否则会污染缓存。需要重排时请先自行复制。
func (b *managerBackend) ListAccounts() []account.Summary {
	b.summaryMu.Lock()
	if b.summaryCache != nil && time.Since(b.summaryAt) < summaryCacheTTL {
		// 【BUG-04 修复】防御性浅拷贝,杜绝调用方排序/修改污染全局缓存(Data Race)
		res := make([]account.Summary, len(b.summaryCache))
		copy(res, b.summaryCache)
		b.summaryMu.Unlock()
		return res
	}
	b.summaryMu.Unlock()

	summaries := b.mgr.ListSummaries()
	b.aliasMu.RLock()
	if len(b.aliasCache) > 0 {
		for i := range summaries {
			if item, ok := b.aliasCache[summaries[i].ID]; ok && item != nil && time.Since(item.fetchedAt) < aliasCacheTTL {
				summaries[i].AliasTotal = item.total
				summaries[i].AliasActive = item.active
			}
		}
	}
	b.aliasMu.RUnlock()

	b.summaryMu.Lock()
	b.summaryCache = summaries
	b.summaryAt = time.Now()
	b.summaryMu.Unlock()
	return summaries
}

// invalidateSummaryCache 立即作废账号列表快照(账号增删改后调用，避免读到已删除的账号)。
func (b *managerBackend) invalidateSummaryCache() {
	b.summaryMu.Lock()
	b.summaryCache = nil
	b.summaryMu.Unlock()
}

// GetAccount 获取单个账号的脱敏信息。
func (b *managerBackend) GetAccount(id string) (account.Summary, error) {
	acc, ok := b.mgr.GetAccount(id)
	if !ok {
		return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
	}
	sum := acc.Summary()
	b.aliasMu.RLock()
	item, ok := b.aliasCache[id]
	b.aliasMu.RUnlock()
	if ok && item != nil && time.Since(item.fetchedAt) < aliasCacheTTL {
		sum.AliasTotal = item.total
		sum.AliasActive = item.active
	}
	return sum, nil
}

// AddAccount 添加账号。
func (b *managerBackend) AddAccount(in account.AddAccountInput) (account.Summary, error) {
	sum, err := b.mgr.AddAccountWithInput(in)
	if err != nil {
		return account.Summary{}, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: err.Error()}
	}
	b.invalidateSummaryCache()
	return sum, nil
}

// UpdateAccount 编辑账号基本信息。
// 会改变 Tags/Status 等选号判据，必须立即作废列表快照，否则业务标签隔离会出现 1 秒窗口。
func (b *managerBackend) UpdateAccount(id string, in account.UpdateAccountInput) (account.Summary, error) {
	sum, err := b.mgr.UpdateMetadata(id, in)
	if err != nil {
		return account.Summary{}, mapAccountErr(err)
	}
	b.invalidateSummaryCache()
	return sum, nil
}

// UpdateProxy 更新或清除账号代理。
func (b *managerBackend) UpdateProxy(id, proxy string) (account.Summary, error) {
	sum, err := b.mgr.UpdateProxy(id, proxy)
	if err != nil {
		return account.Summary{}, mapAccountErr(err)
	}
	b.invalidateSummaryCache()
	return sum, nil
}

// UpdateCookies 更新账号 Cookie。cookies 为原始文本(Header String 或 JSON)。
func (b *managerBackend) UpdateCookies(id, cookies string) (account.Summary, error) {
	parsed, err := account.ParseCookieInput(cookies)
	if err != nil {
		return account.Summary{}, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: err.Error()}
	}
	updateErr := b.mgr.UpdateCookies(id, parsed)
	// 校验失败也可能已保存新凭据和错误状态，旧别名快照不能继续复用。
	b.invalidateAliasCache(id)
	b.invalidateSummaryCache()
	if updateErr != nil {
		return account.Summary{}, mapAccountErr(updateErr)
	}
	sum, ok := b.mgr.GetAccount(id)
	if !ok {
		return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
	}
	return sum.Summary(), nil
}

// SetAppPassword 设置 iCloud 邮箱与 App 专用密码并测试 IMAP 连接。
func (b *managerBackend) SetAppPassword(id, icloudEmail, appPassword string) (account.Summary, error) {
	return b.SetAppPasswordContext(context.Background(), id, icloudEmail, appPassword)
}

func (b *managerBackend) SetAppPasswordContext(ctx context.Context, id, icloudEmail, appPassword string) (account.Summary, error) {
	if err := b.mgr.SetAppPasswordContext(ctx, id, icloudEmail, appPassword); err != nil {
		if errors.Is(err, account.ErrMailConfigPersistence) {
			return account.Summary{}, &BackendError{Status: http.StatusInternalServerError, Code: "PERSISTENCE_FAILURE", Message: "iCloud 邮箱配置保存失败"}
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return account.Summary{}, classifyUpstreamErr("IMAP 验证失败", err)
		}
		msg := err.Error()
		if strings.Contains(msg, "账号不存在") {
			return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
		}
		if strings.Contains(msg, "不能为空") || strings.Contains(msg, "无效") {
			return account.Summary{}, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: msg}
		}
		return account.Summary{}, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "IMAP 验证失败: " + msg}
	}
	b.invalidateSummaryCache()
	sum, ok := b.mgr.GetAccount(id)
	if !ok {
		return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
	}
	return sum.Summary(), nil
}

// SetMailbox configures and verifies an external IMAP mailbox.
func (b *managerBackend) SetMailbox(id string, config account.MailboxConfig) (account.Summary, error) {
	return b.SetMailboxContext(context.Background(), id, config)
}

func (b *managerBackend) SetMailboxContext(ctx context.Context, id string, config account.MailboxConfig) (account.Summary, error) {
	if err := b.mgr.SetMailboxContext(ctx, id, config); err != nil {
		if errors.Is(err, account.ErrMailConfigPersistence) {
			return account.Summary{}, &BackendError{Status: http.StatusInternalServerError, Code: "PERSISTENCE_FAILURE", Message: "收件邮箱配置保存失败"}
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return account.Summary{}, classifyUpstreamErr("收件邮箱验证失败", err)
		}
		msg := err.Error()
		if strings.Contains(msg, "账号不存在") {
			return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
		}
		if strings.Contains(msg, "不能为空") || strings.Contains(msg, "无效") {
			return account.Summary{}, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: msg}
		}
		return account.Summary{}, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "收件邮箱验证失败: " + msg}
	}
	b.invalidateSummaryCache()
	sum, ok := b.mgr.GetAccount(id)
	if !ok {
		return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
	}
	return sum.Summary(), nil
}

// RemoveMailbox 解除外部收件邮箱绑定。
func (b *managerBackend) RemoveMailbox(id string) (account.Summary, error) {
	if err := b.mgr.RemoveMailbox(id); err != nil {
		if errors.Is(err, account.ErrMailConfigPersistence) {
			return account.Summary{}, &BackendError{Status: http.StatusInternalServerError, Code: "PERSISTENCE_FAILURE", Message: "收件邮箱配置保存失败"}
		}
		return account.Summary{}, mapAccountErr(err)
	}
	b.invalidateSummaryCache()
	sum, ok := b.mgr.GetAccount(id)
	if !ok {
		return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
	}
	return sum.Summary(), nil
}

func getCamoufoxURL() string {
	if u := os.Getenv("ICLOUD_HME_CAMOUFOX_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://127.0.0.1:8089"
}

func camoufoxConfigured() bool {
	return strings.TrimSpace(os.Getenv("ICLOUD_HME_CAMOUFOX_URL")) != "" || strings.TrimSpace(os.Getenv("ICLOUD_HME_CAMOUFOX_TOKEN")) != ""
}

// validateCamoufoxURL 约束代理服务的传输边界。HTTP 只允许回环地址和 Compose
// 内部服务名；跨主机必须使用 HTTPS。若部署在已加密的专用 VPN 上，可显式设置
// ICLOUD_HME_CAMOUFOX_ALLOW_INSECURE=true，作为运维侧的网络安全承诺。
func validateCamoufoxURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("Camoufox URL 无效")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("Camoufox URL 必须使用 http 或 https")
	}
	if u.Scheme == "http" && !isLocalCamoufoxHost(u.Hostname()) && os.Getenv("ICLOUD_HME_CAMOUFOX_ALLOW_INSECURE") != "true" {
		return fmt.Errorf("跨主机 Camoufox 通信必须使用 HTTPS；仅受控 VPN 可显式设置 ICLOUD_HME_CAMOUFOX_ALLOW_INSECURE=true")
	}
	return nil
}

func isLocalCamoufoxHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "localhost" || host == "camoufox-agent" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func isValidOTPCode(code string) bool {
	if len(code) != 6 {
		return false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < '0' || code[i] > '9' {
			return false
		}
	}
	return true
}

// newCamoufoxHTTPClient 构造代理通信客户端，并支持 HTTPS 自定义 CA 与 mTLS。
func newCamoufoxHTTPClient(timeout time.Duration) (*http.Client, error) {
	transport := &http.Transport{Proxy: func(req *http.Request) (*url.URL, error) {
		if isLocalCamoufoxHost(req.URL.Hostname()) {
			return nil, nil
		}
		return http.ProxyFromEnvironment(req)
	}}
	caFile := strings.TrimSpace(os.Getenv("ICLOUD_HME_CAMOUFOX_CA_FILE"))
	certFile := strings.TrimSpace(os.Getenv("ICLOUD_HME_CAMOUFOX_CLIENT_CERT_FILE"))
	keyFile := strings.TrimSpace(os.Getenv("ICLOUD_HME_CAMOUFOX_CLIENT_KEY_FILE"))
	if (certFile == "") != (keyFile == "") {
		return nil, errors.New("Camoufox mTLS 必须同时配置客户端证书和私钥")
	}
	if caFile != "" || certFile != "" {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
		if caFile != "" {
			pem, err := os.ReadFile(caFile)
			if err != nil {
				return nil, fmt.Errorf("读取 Camoufox CA 文件失败: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, errors.New("Camoufox CA 文件不包含有效证书")
			}
			tlsConfig.RootCAs = pool
		}
		if certFile != "" {
			cert, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				return nil, fmt.Errorf("读取 Camoufox mTLS 客户端证书失败: %w", err)
			}
			tlsConfig.Certificates = []tls.Certificate{cert}
		}
		transport.TLSClientConfig = tlsConfig
	}
	return &http.Client{
		Timeout: timeout, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

func newCamoufoxRequest(method, url string, body io.Reader) (*http.Request, error) {
	if err := validateCamoufoxURL(url); err != nil {
		return nil, err
	}
	token := os.Getenv("ICLOUD_HME_CAMOUFOX_TOKEN")
	if token == "" {
		return nil, errors.New("未配置 ICLOUD_HME_CAMOUFOX_TOKEN")
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Camoufox-Token", token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (b *managerBackend) getCamoufoxTask(accountID string) (camoufoxPendingTask, bool) {
	b.camoufoxMu.Lock()
	defer b.camoufoxMu.Unlock()
	t, ok := b.camoufoxTasks[accountID]
	if !ok {
		return camoufoxPendingTask{}, false
	}
	return *t, true
}

func (b *managerBackend) reserveCamoufoxTask(accountID, baseURL string) bool {
	b.camoufoxMu.Lock()
	defer b.camoufoxMu.Unlock()
	if b.camoufoxTasks == nil {
		b.camoufoxTasks = make(map[string]*camoufoxPendingTask)
	}
	if _, exists := b.camoufoxTasks[accountID]; exists {
		return false
	}
	b.camoufoxTasks[accountID] = &camoufoxPendingTask{baseURL: baseURL, createdAt: time.Now()}
	return true
}

func (b *managerBackend) persistCamoufoxTask(task camoufoxPendingTask, accountID string) error {
	if b.store == nil {
		return nil
	}
	return b.store.SaveCamoufoxTask(store.CamoufoxTask{
		AccountID: accountID,
		TaskID:    task.taskID,
		BaseURL:   task.baseURL,
		CreatedAt: task.createdAt,
	})
}

func (b *managerBackend) deletePersistedCamoufoxTask(accountID, taskID string) {
	if b.store == nil {
		return
	}
	if err := b.store.DeleteCamoufoxTask(accountID, taskID); err != nil {
		log.Printf("[Camoufox] 清理任务持久化记录失败 account=%s task=%s: %v", accountID, taskID, err)
	}
}

func (b *managerBackend) setCamoufoxTask(accountID, taskID string) error {
	b.camoufoxMu.Lock()
	defer b.camoufoxMu.Unlock()
	if task := b.camoufoxTasks[accountID]; task != nil {
		task.taskID = taskID
		return b.persistCamoufoxTask(*task, accountID)
	}
	return errors.New("Camoufox 登录任务不存在")
}

func (b *managerBackend) markCamoufoxOTP(accountID, taskID string) error {
	b.camoufoxMu.Lock()
	if task := b.camoufoxTasks[accountID]; task != nil && task.taskID == taskID {
		task.createdAt = time.Now()
		pending := *task
		baseURL := task.baseURL
		if err := b.persistCamoufoxTask(pending, accountID); err != nil {
			b.camoufoxMu.Unlock()
			return err
		}
		b.camoufoxMu.Unlock()
		time.AfterFunc(3*time.Minute, func() {
			b.camoufoxMu.Lock()
			pending := b.camoufoxTasks[accountID]
			expired := pending != nil && pending.taskID == taskID && time.Since(pending.createdAt) >= 3*time.Minute
			b.camoufoxMu.Unlock()
			if expired {
				if err := b.cancelAndClearCamoufoxTask(accountID, taskID, baseURL); err != nil && !errors.Is(err, errCamoufoxTaskMissing) {
					log.Printf("[Camoufox] OTP 超时取消任务失败 account=%s task=%s: %v", accountID, taskID, err)
				}
			}
		})
		return nil
	}
	b.camoufoxMu.Unlock()
	return errors.New("Camoufox 登录任务不存在")
}

func (b *managerBackend) clearCamoufoxTask(accountID, taskID string) {
	b.camoufoxMu.Lock()
	defer b.camoufoxMu.Unlock()
	if task := b.camoufoxTasks[accountID]; task != nil && task.taskID == taskID {
		delete(b.camoufoxTasks, accountID)
	}
	if taskID != "" {
		b.deletePersistedCamoufoxTask(accountID, taskID)
	}
}

func camoufoxHTTPError(action string, status int) *BackendError {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return &BackendError{Status: http.StatusBadGateway, Code: "CAMOUFOX_AUTH_FAILED", Message: "Camoufox 通信令牌无效或无权访问代理"}
	case http.StatusNotFound:
		return &BackendError{Status: http.StatusBadGateway, Code: "CAMOUFOX_ENDPOINT_NOT_FOUND", Message: "Camoufox 代理端点不存在"}
	case http.StatusTooManyRequests:
		return &BackendError{Status: http.StatusTooManyRequests, Code: "AUTH_BUSY", Message: "Camoufox 登录任务已达并发上限，请稍后重试"}
	case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout:
		return &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: fmt.Sprintf("Camoufox %s失败，代理返回 HTTP %d", action, status)}
	default:
		return &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: fmt.Sprintf("Camoufox %s失败，代理返回 HTTP %d", action, status)}
	}
}

var errCamoufoxTaskMissing = errors.New("Camoufox 任务不存在")

const camoufoxInitialPollTimeout = 210 * time.Second

func cancelCamoufoxTask(baseURL, taskID string) error {
	req, err := newCamoufoxRequest(http.MethodDelete, baseURL+"/tasks/"+taskID, nil)
	if err != nil {
		return err
	}
	client, err := newCamoufoxHTTPClient(3 * time.Second)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errCamoufoxTaskMissing
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Camoufox 取消请求返回 HTTP %d", resp.StatusCode)
	}
	var result struct {
		Success bool `json:"success"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("Camoufox 取消响应无效: %w", err)
	}
	if !result.Success {
		return errCamoufoxTaskMissing
	}
	return nil
}

// cancelAndClearCamoufoxTask 仅在代理确认终止或任务已不存在时清理本地元数据。
func (b *managerBackend) cancelAndClearCamoufoxTask(accountID, taskID, baseURL string) error {
	err := cancelCamoufoxTask(baseURL, taskID)
	if err == nil || errors.Is(err, errCamoufoxTaskMissing) {
		b.clearCamoufoxTask(accountID, taskID)
	} else {
		b.camoufoxMu.Lock()
		if pending := b.camoufoxTasks[accountID]; pending != nil && pending.taskID == taskID {
			pending.recovered = true
		}
		b.camoufoxMu.Unlock()
	}
	return err
}

func (b *managerBackend) CancelCamoufoxLogin(accountID, taskID string) (bool, error) {
	pending, ok := b.getCamoufoxTask(accountID)
	if !ok || taskID == "" || pending.taskID != taskID {
		return false, nil
	}
	if err := b.cancelAndClearCamoufoxTask(accountID, taskID, pending.baseURL); err != nil {
		if errors.Is(err, errCamoufoxTaskMissing) {
			return false, nil
		}
		return false, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "取消 Camoufox 登录任务失败: " + err.Error()}
	}
	return true, nil
}

// recoverCamoufoxTasks 在主服务重启后回收代理端仍可能存在的旧浏览器任务。
// 取消失败时保留任务元数据，后续登录继续尝试回收，不把未知状态当成成功。
func (b *managerBackend) recoverCamoufoxTasks() {
	if b.store == nil {
		return
	}
	tasks, err := b.store.ListCamoufoxTasks()
	if err != nil {
		log.Printf("[Camoufox] 读取待回收任务失败: %v", err)
		return
	}
	for _, task := range tasks {
		err := cancelCamoufoxTask(task.BaseURL, task.TaskID)
		if err != nil && !errors.Is(err, errCamoufoxTaskMissing) {
			log.Printf("[Camoufox] 启动恢复时取消旧任务失败 account=%s task=%s: %v", task.AccountID, task.TaskID, err)
		} else {
			b.deletePersistedCamoufoxTask(task.AccountID, task.TaskID)
		}
		b.camoufoxMu.Lock()
		if b.camoufoxTasks == nil {
			b.camoufoxTasks = make(map[string]*camoufoxPendingTask)
		}
		b.camoufoxTasks[task.AccountID] = &camoufoxPendingTask{
			taskID: task.TaskID, baseURL: task.BaseURL, createdAt: task.CreatedAt, recovered: true,
		}
		b.camoufoxMu.Unlock()
	}
}

func (b *managerBackend) cancelAllCamoufoxTasks() {
	b.camoufoxMu.Lock()
	tasks := make([]camoufoxPendingTask, 0, len(b.camoufoxTasks))
	accounts := make([]string, 0, len(b.camoufoxTasks))
	for accountID, task := range b.camoufoxTasks {
		if task != nil {
			tasks = append(tasks, *task)
			accounts = append(accounts, accountID)
		}
	}
	b.camoufoxTasks = nil
	b.camoufoxMu.Unlock()
	for i, task := range tasks {
		if task.taskID != "" {
			if err := b.cancelAndClearCamoufoxTask(accounts[i], task.taskID, task.baseURL); err != nil && !errors.Is(err, errCamoufoxTaskMissing) {
				log.Printf("[Camoufox] 停机取消任务失败 account=%s task=%s: %v", accounts[i], task.taskID, err)
			}
		}
	}
	if b.store != nil {
		// 处理 map 尚未加载但已落库的记录，避免停机时遗漏恢复任务。
		if persisted, err := b.store.ListCamoufoxTasks(); err == nil {
			for _, task := range persisted {
				if err := b.cancelAndClearCamoufoxTask(task.AccountID, task.TaskID, task.BaseURL); err != nil && !errors.Is(err, errCamoufoxTaskMissing) {
					log.Printf("[Camoufox] 停机取消持久化任务失败 account=%s task=%s: %v", task.AccountID, task.TaskID, err)
				}
			}
		}
	}
}

func (b *managerBackend) loginWithCamoufox(ctx context.Context, id, password, otpCode, camoufoxBase string) (account.Summary, error) {
	if err := ctx.Err(); err != nil {
		return account.Summary{}, err
	}
	var cleanupTaskID string
	defer func() {
		if ctx.Err() != nil && cleanupTaskID != "" {
			if err := b.cancelAndClearCamoufoxTask(id, cleanupTaskID, camoufoxBase); err != nil && !errors.Is(err, errCamoufoxTaskMissing) {
				log.Printf("[Camoufox] 请求取消后回收任务失败 account=%s task=%s: %v", id, cleanupTaskID, err)
			}
		}
	}()
	acc, ok := b.mgr.GetAccount(id)
	if !ok {
		return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
	}

	email := acc.ICloudEmail
	if email == "" {
		email = acc.RealEmail
	}
	if email == "" {
		return account.Summary{}, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: "账号未配置邮箱地址"}
	}

	httpClient, err := newCamoufoxHTTPClient(70 * time.Second)
	if err != nil {
		return account.Summary{}, &BackendError{Status: http.StatusServiceUnavailable, Code: "CAMOUFOX_CONFIG_ERROR", Message: err.Error()}
	}

	if otpCode != "" {
		// 阶段 2: 提交 2FA 验证码
		pending, hasPending := b.getCamoufoxTask(id)
		if hasPending {
			camoufoxBase = pending.baseURL
		}
		if !hasPending || pending.taskID == "" || time.Since(pending.createdAt) > 3*time.Minute {
			if hasPending && pending.taskID != "" {
				_ = b.cancelAndClearCamoufoxTask(id, pending.taskID, camoufoxBase)
			}
			return account.Summary{}, &BackendError{Status: http.StatusBadRequest, Code: "OTP_EXPIRED", Message: "2FA 验证会话已超时，请重新登录"}
		}
		cleanupTaskID = pending.taskID

		submitPayload := map[string]string{
			"task_id":  pending.taskID,
			"otp_code": otpCode,
		}
		data, err := json.Marshal(submitPayload)
		if err != nil {
			return account.Summary{}, &BackendError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: err.Error()}
		}

		req, err := newCamoufoxRequest(http.MethodPost, camoufoxBase+"/submit-otp", bytes.NewReader(data))
		if err != nil {
			return account.Summary{}, &BackendError{Status: http.StatusServiceUnavailable, Code: "CAMOUFOX_CONFIG_ERROR", Message: err.Error()}
		}
		resp, err := httpClient.Do(req.WithContext(ctx))
		if err != nil {
			return account.Summary{}, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "提交验证码失败: " + err.Error()}
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			b.clearCamoufoxTask(id, pending.taskID)
			return account.Summary{}, &BackendError{Status: http.StatusBadRequest, Code: "OTP_EXPIRED", Message: "2FA 验证会话已失效，请重新登录"}
		}
		if resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusBadRequest {
			resp.Body.Close()
			b.clearCamoufoxTask(id, pending.taskID)
			return account.Summary{}, &BackendError{Status: http.StatusBadRequest, Code: "OTP_EXPIRED", Message: "2FA 验证会话已结束，请重新登录"}
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return account.Summary{}, camoufoxHTTPError("提交验证码", resp.StatusCode)
		}
		var submitRes struct {
			Success bool `json:"success"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&submitRes)
		resp.Body.Close()
		if decodeErr != nil || !submitRes.Success {
			return account.Summary{}, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "Camoufox 未接受验证码提交"}
		}

		return b.pollCamoufoxTask(ctx, id, pending.taskID, camoufoxBase, 55*time.Second, true)
	}

	// 阶段 1: 发起全新无头登录
	if !b.reserveCamoufoxTask(id, camoufoxBase) {
		return account.Summary{}, &BackendError{Status: http.StatusConflict, Code: "AUTH_IN_PROGRESS", Message: "该账号已有进行中的登录任务"}
	}
	started := false
	defer func() {
		if !started {
			b.clearCamoufoxTask(id, "")
		}
	}()
	loginPayload := map[string]string{
		"account_id": id,
		"username":   email,
		"password":   password,
		"proxy":      acc.Proxy,
		"host":       acc.Host,
	}
	data, err := json.Marshal(loginPayload)
	if err != nil {
		return account.Summary{}, &BackendError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: err.Error()}
	}

	req, err := newCamoufoxRequest(http.MethodPost, camoufoxBase+"/login", bytes.NewReader(data))
	if err != nil {
		return account.Summary{}, &BackendError{Status: http.StatusServiceUnavailable, Code: "CAMOUFOX_CONFIG_ERROR", Message: err.Error()}
	}
	// Keep the bounded task-creation handshake alive so cancellation cannot
	// discard a newly created task ID. Once received, the deferred cleanup
	// cancels it if the caller has disconnected.
	resp, err := httpClient.Do(req.WithContext(context.WithoutCancel(ctx)))
	if err != nil {
		return account.Summary{}, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "启动 Camoufox 任务失败: " + err.Error()}
	}
	var loginRes struct {
		Success bool   `json:"success"`
		TaskID  string `json:"task_id"`
		Status  string `json:"status"`
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			return account.Summary{}, &BackendError{Status: http.StatusTooManyRequests, Code: "AUTH_BUSY", Message: "Camoufox 登录任务已达并发上限，请稍后重试"}
		}
		return account.Summary{}, camoufoxHTTPError("启动登录任务", resp.StatusCode)
	}
	decodeErr := json.NewDecoder(resp.Body).Decode(&loginRes)
	resp.Body.Close()
	if decodeErr != nil || !loginRes.Success || loginRes.TaskID == "" {
		return account.Summary{}, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "Camoufox 任务创建异常"}
	}
	cleanupTaskID = loginRes.TaskID

	if err := b.setCamoufoxTask(id, loginRes.TaskID); err != nil {
		if cancelErr := b.cancelAndClearCamoufoxTask(id, loginRes.TaskID, camoufoxBase); cancelErr != nil && !errors.Is(cancelErr, errCamoufoxTaskMissing) {
			log.Printf("[Camoufox] 持久化失败后取消任务失败 account=%s task=%s: %v", id, loginRes.TaskID, cancelErr)
		}
		return account.Summary{}, &BackendError{Status: http.StatusInternalServerError, Code: "PERSISTENCE_FAILURE", Message: "Camoufox 登录任务持久化失败"}
	}
	started = true
	return b.pollCamoufoxTask(ctx, id, loginRes.TaskID, camoufoxBase, camoufoxInitialPollTimeout, false)
}

func (b *managerBackend) pollCamoufoxTask(ctx context.Context, accountID, taskID, camoufoxBase string, timeout time.Duration, isOTPPhase bool) (account.Summary, error) {
	deadline := time.Now().Add(timeout)
	httpClient, err := newCamoufoxHTTPClient(5 * time.Second)
	if err != nil {
		return account.Summary{}, &BackendError{Status: http.StatusServiceUnavailable, Code: "CAMOUFOX_CONFIG_ERROR", Message: err.Error()}
	}
	firstPoll := true

	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return account.Summary{}, err
		}
		if !firstPoll {
			select {
			case <-ctx.Done():
				return account.Summary{}, ctx.Err()
			case <-time.After(time.Second):
			}
		}
		firstPoll = false

		req, err := newCamoufoxRequest(http.MethodGet, fmt.Sprintf("%s/tasks/%s", camoufoxBase, taskID), nil)
		if err != nil {
			return account.Summary{}, &BackendError{Status: http.StatusServiceUnavailable, Code: "CAMOUFOX_CONFIG_ERROR", Message: err.Error()}
		}
		resp, err := httpClient.Do(req.WithContext(ctx))
		if err != nil {
			if ctx.Err() != nil {
				return account.Summary{}, ctx.Err()
			}
			_ = b.cancelAndClearCamoufoxTask(accountID, taskID, camoufoxBase)
			return account.Summary{}, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "查询 Camoufox 登录任务失败: " + err.Error()}
		}
		if resp.StatusCode != http.StatusOK {
			status := resp.StatusCode
			resp.Body.Close()
			if status == http.StatusNotFound {
				b.clearCamoufoxTask(accountID, taskID)
				if isOTPPhase {
					return account.Summary{}, &BackendError{Status: http.StatusBadRequest, Code: "OTP_EXPIRED", Message: "2FA 验证会话已失效，请重新登录"}
				}
				return account.Summary{}, &BackendError{Status: http.StatusBadGateway, Code: "AUTH_TASK_EXPIRED", Message: "Camoufox 登录任务已失效，请重新登录"}
			}
			_ = b.cancelAndClearCamoufoxTask(accountID, taskID, camoufoxBase)
			return account.Summary{}, camoufoxHTTPError("查询登录任务", status)
		}
		var task struct {
			TaskID       string              `json:"task_id"`
			Status       string              `json:"status"`
			Host         string              `json:"host"`
			ErrorMessage string              `json:"error_message"`
			Cookies      map[string]string   `json:"cookies"`
			Session      *hme.BrowserSession `json:"session"`
		}
		err = json.NewDecoder(resp.Body).Decode(&task)
		resp.Body.Close()
		if ctx.Err() != nil {
			return account.Summary{}, ctx.Err()
		}
		if err != nil {
			_ = b.cancelAndClearCamoufoxTask(accountID, taskID, camoufoxBase)
			return account.Summary{}, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "Camoufox 任务响应格式无效"}
		}
		if task.TaskID != "" && task.TaskID != taskID {
			_ = b.cancelAndClearCamoufoxTask(accountID, taskID, camoufoxBase)
			return account.Summary{}, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "Camoufox 返回了不匹配的任务"}
		}

		switch task.Status {
		case "otp_required":
			if !isOTPPhase {
				if err := b.markCamoufoxOTP(accountID, taskID); err != nil {
					_ = b.cancelAndClearCamoufoxTask(accountID, taskID, camoufoxBase)
					return account.Summary{}, &BackendError{Status: http.StatusInternalServerError, Code: "PERSISTENCE_FAILURE", Message: "Camoufox 验证任务持久化失败"}
				}
				return account.Summary{}, &BackendError{
					Status:  http.StatusConflict,
					Code:    "OTP_REQUIRED",
					Message: "该 Apple ID 已启用双重认证，请输入 6 位验证码",
					Data:    map[string]string{"task_id": taskID},
				}
			}
			// 若当前为验证码提交阶段，继续等待后端状态流转至 verifying / success / failed
		case "failed":
			_ = b.cancelAndClearCamoufoxTask(accountID, taskID, camoufoxBase)
			errMsg := task.ErrorMessage
			if strings.Contains(errMsg, "完整的保持登录会话") {
				return account.Summary{}, &BackendError{Status: http.StatusUnauthorized, Code: "APPLE_AUTH_REJECTED", Message: "未捕获完整的保持登录会话，请重新登录并确认保持登录和信任浏览器"}
			}
			if strings.Contains(errMsg, "验证码输入失败") {
				return account.Summary{}, &BackendError{Status: http.StatusBadGateway, Code: "OTP_INPUT_FAILED", Message: "Camoufox 未能填入双重认证验证码，请重新登录"}
			}
			if strings.Contains(errMsg, "代理") {
				return account.Summary{}, &BackendError{Status: http.StatusBadGateway, Code: "CAMOUFOX_PROXY_ERROR", Message: errMsg}
			}
			if strings.Contains(errMsg, "密码错误") || strings.Contains(errMsg, "incorrect") {
				return account.Summary{}, &BackendError{Status: http.StatusUnauthorized, Code: "INVALID_CREDENTIALS", Message: "Apple ID 账号或密码错误"}
			}
			if strings.Contains(errMsg, "验证码") || strings.Contains(strings.ToLower(errMsg), "verification") {
				return account.Summary{}, &BackendError{Status: http.StatusUnauthorized, Code: "OTP_REJECTED", Message: "Apple ID 双重认证验证码无效"}
			}
			return account.Summary{}, &BackendError{Status: http.StatusUnauthorized, Code: "APPLE_AUTH_REJECTED", Message: "Apple ID 登录失败，请稍后重试"}
		case "success":
			defer func() { _ = b.cancelAndClearCamoufoxTask(accountID, taskID, camoufoxBase) }()
			if len(task.Cookies) == 0 {
				return account.Summary{}, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "Camoufox 登录成功但未返回 Cookie"}
			}
			if task.Session == nil || task.Session.Host != task.Host {
				return account.Summary{}, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "Camoufox 未返回完整会话，请更新登录代理后重新登录"}
			}
			if err := b.mgr.UpdateBrowserSession(accountID, task.Session); err != nil {
				return account.Summary{}, mapAccountErr(err)
			}
			b.invalidateAliasCache(accountID)
			b.invalidateSummaryCache()
			acc, ok := b.mgr.GetAccount(accountID)
			if !ok {
				return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
			}
			return acc.Summary(), nil
		case "initializing", "entering_credentials", "verifying":
			// 任务仍在进行，下一轮继续查询。
		default:
			_ = b.cancelAndClearCamoufoxTask(accountID, taskID, camoufoxBase)
			return account.Summary{}, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "Camoufox 返回了未知任务状态"}
		}
	}

	_ = b.cancelAndClearCamoufoxTask(accountID, taskID, camoufoxBase)
	return account.Summary{}, &BackendError{
		Status:  http.StatusGatewayTimeout,
		Code:    "AUTH_TIMEOUT",
		Message: "Apple ID 认证超时，请稍后重试",
	}
}

// LoginAccount 使用 iCloud 密码登录账号,成功只返回 Summary,绝不返回 Cookies。
// 未配置 Camoufox 时使用原生 SRP；已配置的代理失败会明确返回错误。
func (b *managerBackend) LoginAccount(id, password, otpCode string) (account.Summary, error) {
	return b.LoginAccountContext(context.Background(), id, password, otpCode)
}

func (b *managerBackend) LoginAccountContext(ctx context.Context, id, password, otpCode string) (account.Summary, error) {
	if err := ctx.Err(); err != nil {
		return account.Summary{}, err
	}
	if (password == "") == (otpCode == "") {
		return account.Summary{}, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: "password 与 otp_code 必须二选一"}
	}
	if otpCode != "" && !isValidOTPCode(otpCode) {
		return account.Summary{}, &BackendError{Status: http.StatusBadRequest, Code: "OTP_INVALID", Message: "otp_code 必须是 6 位数字"}
	}
	camoufoxBase := getCamoufoxURL()
	pending, hasCamoufoxPending := b.getCamoufoxTask(id)
	if hasCamoufoxPending && pending.recovered {
		err := b.cancelAndClearCamoufoxTask(id, pending.taskID, pending.baseURL)
		if err != nil && !errors.Is(err, errCamoufoxTaskMissing) {
			return account.Summary{}, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "回收旧 Camoufox 登录任务失败: " + err.Error()}
		}
		if otpCode != "" {
			return account.Summary{}, &BackendError{Status: http.StatusBadRequest, Code: "OTP_EXPIRED", Message: "2FA 验证会话已失效，请重新登录"}
		}
		hasCamoufoxPending = false
	}
	if hasCamoufoxPending || camoufoxConfigured() {
		if otpCode != "" && !hasCamoufoxPending {
			return account.Summary{}, &BackendError{Status: http.StatusBadRequest, Code: "OTP_EXPIRED", Message: "2FA 验证会话已失效，请重新登录"}
		}
		if err := validateCamoufoxURL(camoufoxBase); err != nil {
			return account.Summary{}, &BackendError{Status: http.StatusServiceUnavailable, Code: "CAMOUFOX_TRANSPORT_INVALID", Message: err.Error()}
		}
		if !hasCamoufoxPending {
			if _, _, err := camoufoxHealthContext(ctx, camoufoxBase, 10*time.Second); err != nil {
				return account.Summary{}, &BackendError{Status: http.StatusServiceUnavailable, Code: "CAMOUFOX_UNAVAILABLE", Message: "Camoufox 代理不可用: " + err.Error()}
			}
		}
		return b.loginWithCamoufox(ctx, id, password, otpCode, camoufoxBase)
	}

	var otpProvider hme.OTPProvider
	if otpCode != "" {
		otp := otpCode
		otpProvider = func() (string, error) { return otp, nil }
	}

	client, err := b.mgr.HMEClientWithPassword(id, password, otpProvider)
	// 登录 Cookie 可能已保存，后续校验失败也必须丢弃旧别名快照。
	b.invalidateAliasCache(id)
	b.invalidateSummaryCache()
	if err != nil {
		return account.Summary{}, classifyLoginErr(err)
	}
	defer client.Close()
	sum, ok := b.mgr.GetAccount(id)
	if !ok {
		return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
	}
	return sum.Summary(), nil
}

// classifyLoginErr 把 iCloud 登录错误映射为稳定错误。
func classifyLoginErr(err error) *BackendError {
	if errors.Is(err, account.ErrAccountIdentityMismatch) {
		return &BackendError{Status: http.StatusConflict, Code: "ACCOUNT_IDENTITY_MISMATCH", Message: err.Error()}
	}
	msg := err.Error()
	if strings.Contains(msg, "需要提供 OTP") {
		return &BackendError{Status: http.StatusConflict, Code: "OTP_REQUIRED", Message: "需要提供 OTP 验证码"}
	}
	if strings.Contains(msg, "2FA 验证失败") {
		return &BackendError{Status: http.StatusUnauthorized, Code: "OTP_INVALID", Message: "OTP 验证码错误"}
	}
	if strings.Contains(msg, "账号不存在") {
		return &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
	}
	if strings.Contains(msg, "用户名或密码错误") {
		return &BackendError{Status: http.StatusUnauthorized, Code: "INVALID_CREDENTIALS", Message: "Apple ID 账号或密码错误"}
	}
	if strings.Contains(msg, "隐私条款") {
		return &BackendError{Status: http.StatusForbidden, Code: "TERMS_REQUIRED", Message: "需要先在 appleid.apple.com 同意苹果隐私条款"}
	}
	if strings.Contains(msg, "Apple 认证失败:") {
		cleanMsg := msg
		if idx := strings.Index(msg, "Apple 认证失败:"); idx >= 0 {
			cleanMsg = msg[idx:]
		}
		return &BackendError{Status: http.StatusUnauthorized, Code: "APPLE_AUTH_REJECTED", Message: cleanMsg}
	}
	if isSessionError(msg) || strings.Contains(msg, "auth complete") || strings.Contains(msg, "401") || strings.Contains(msg, "403") {
		return &BackendError{Status: http.StatusUnauthorized, Code: "APPLE_AUTH_BLOCKED", Message: "Apple 拒绝了模拟密码登录（触发了苹果安全风控），请点击【更新 Cookie】直接粘贴浏览器 Cookie 激活"}
	}
	return &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "iCloud 登录失败: " + msg}
}

// RemoveAccount 删除账号并彻底驱逐关联的内存别名缓存。
func (b *managerBackend) RemoveAccount(id string) bool {
	hadPending := false
	if pending, ok := b.getCamoufoxTask(id); ok && pending.taskID != "" {
		hadPending = true
		if err := b.cancelAndClearCamoufoxTask(id, pending.taskID, pending.baseURL); err != nil && !errors.Is(err, errCamoufoxTaskMissing) {
			log.Printf("[Camoufox] 删除账号时取消登录任务失败 account=%s task=%s: %v", id, pending.taskID, err)
		}
	}
	if !hadPending && b.store != nil {
		if tasks, err := b.store.ListCamoufoxTasks(); err != nil {
			log.Printf("[Camoufox] 删除账号时读取待回收任务失败 account=%s: %v", id, err)
		} else {
			for _, task := range tasks {
				if task.AccountID == id {
					if err := b.cancelAndClearCamoufoxTask(id, task.TaskID, task.BaseURL); err != nil && !errors.Is(err, errCamoufoxTaskMissing) {
						log.Printf("[Camoufox] 删除账号时取消持久化任务失败 account=%s task=%s: %v", id, task.TaskID, err)
					}
				}
			}
		}
	}
	ok := b.mgr.RemoveAccount(id)
	if ok {
		b.invalidateAliasCache(id)
		// 账号已被删除，立即作废列表快照，避免它还出现在列表/选号里
		b.invalidateSummaryCache()
	}
	return ok
}

// Reload 重新加载 accounts.json 配置文件，并清空别名缓存。
func (b *managerBackend) Reload() error {
	b.aliasMu.Lock()
	b.aliasCache = make(map[string]*aliasCacheItem)
	b.aliasMu.Unlock()
	b.invalidateSummaryCache()
	if err := b.mgr.Reload(); err != nil {
		return &BackendError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "重新加载配置失败"}
	}
	return nil
}

// ValidateAccount 校验账号 Cookie 会话并刷新状态(CookieMonitor 周期调用)。
// 错误保留 account.ErrCookieExpired 哨兵包装，供监控器区分凭据失效与瞬时故障。
func (b *managerBackend) ValidateAccount(id string) error {
	return b.ValidateAccountContext(context.Background(), id)
}

// ValidateAccountContext 支持 context 贯穿的会话校验 (PR-05 F10)。
func (b *managerBackend) ValidateAccountContext(ctx context.Context, id string) error {
	return b.mgr.ValidateAccountWithContext(ctx, id)
}

// mapAccountErr 把账号管理器错误映射为稳定错误。
func mapAccountErr(err error) *BackendError {
	if errors.Is(err, hme.ErrRecoveryDeferred) {
		return &BackendError{Status: http.StatusServiceUnavailable, Code: "SESSION_RECOVERY_DEFERRED", Message: "会话恢复暂缓，请稍后重试"}
	}
	if errors.Is(err, hme.ErrOTPRequired) {
		return &BackendError{Status: http.StatusUnauthorized, Code: "SESSION_REAUTH_REQUIRED", Message: "Apple 要求重新验证，请重新登录"}
	}
	if errors.Is(err, account.ErrAccountIdentityMismatch) {
		return &BackendError{Status: http.StatusConflict, Code: "ACCOUNT_IDENTITY_MISMATCH", Message: err.Error()}
	}
	if errors.Is(err, account.ErrCookiesRejectedInvalid) {
		return &BackendError{Status: http.StatusUnprocessableEntity, Code: "COOKIE_VALIDATION_FAILED", Message: "自动登录 Cookie 未通过 Apple 校验，原有凭据未改变"}
	}
	if errors.Is(err, account.ErrCookiesSavedInvalid) {
		return &BackendError{Status: http.StatusUnprocessableEntity, Code: "COOKIE_SAVED_INVALID", Message: "Cookie 已保存，但 Apple 校验未通过，请检查凭据或网络"}
	}
	msg := err.Error()
	if strings.Contains(msg, "账号不存在") {
		return &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
	}
	if strings.Contains(msg, "Cookie") && strings.Contains(msg, "未配置") {
		return &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: "账号未配置 Cookie"}
	}
	return &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: msg}
}

// classifyUpstreamErr 把上游 (iCloud) 错误映射为稳定错误,不拼接上游响应体。
func classifyUpstreamErr(fixedMsg string, err error) *BackendError {
	if err == nil {
		return nil
	}
	if errors.Is(err, hme.ErrRecoveryDeferred) || errors.Is(err, hme.ErrOTPRequired) {
		return mapAccountErr(err)
	}
	if errors.Is(err, hme.ErrAccessDenied) {
		return &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_ACCESS_DENIED", Message: "Apple 拒绝访问，尚不能判定会话过期，请检查访问环境"}
	}
	if errors.Is(err, hme.ErrSessionIdentity) {
		return &BackendError{Status: http.StatusConflict, Code: "ACCOUNT_IDENTITY_MISMATCH", Message: "Apple 会话身份不一致，请重新登录"}
	}
	if errors.Is(err, account.ErrAccountIdentityMismatch) {
		return mapAccountErr(err)
	}
	if errors.Is(err, hme.ErrOutcomeUnknown) {
		return &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_OUTCOME_UNKNOWN", Message: "上游写操作结果未知，需核对后处理"}
	}
	if errors.Is(err, context.Canceled) {
		return &BackendError{Status: 499, Code: "REQUEST_CANCELED", Message: "请求已取消"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &BackendError{Status: http.StatusGatewayTimeout, Code: "REQUEST_TIMEOUT", Message: "请求超时"}
	}
	if isSessionError(err.Error()) {
		return &BackendError{Status: http.StatusUnauthorized, Code: "UPSTREAM_UNAUTHORIZED", Message: "iCloud 会话失效,请更新 Cookie"}
	}
	return &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: fixedMsg}
}

// isSessionError 判断错误是否由会话失效引起。
func isSessionError(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "401") || strings.Contains(m, "403") || strings.Contains(m, "421") ||
		strings.Contains(m, "session") || strings.Contains(m, "cookie") ||
		strings.Contains(m, "unauthorized") || strings.Contains(m, "认证") ||
		strings.Contains(m, "会话校验失败")
}

// asBackendError 提取 BackendError,非 BackendError 统一为 INTERNAL_ERROR。
func asBackendError(err error) *BackendError {
	var be *BackendError
	if errors.As(err, &be) {
		return be
	}
	return &BackendError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "内部错误"}
}

// cookieInputToJSON 把 handler 解析出的 map 转回 JSON 文本,交给 ParseCookieInput。
func cookieInputToJSON(cookies map[string]string) string {
	raw, err := json.Marshal(cookies)
	if err != nil {
		return ""
	}
	return string(raw)
}

// CheckProxy 探测指定代理节点连通性与到 Apple 网关的往返延迟
func (b *managerBackend) CheckProxy(proxyURL string) (bool, int64, string, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return false, 0, "", errors.New("代理地址不能为空")
	}

	options := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(8),
		tls_client.WithClientProfile(profiles.Chrome_146),
		tls_client.WithProxyUrl(proxyURL),
		tls_client.WithNotFollowRedirects(),
	}
	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
	if err != nil {
		return false, 0, "", fmt.Errorf("创建代理客户端失败: %w", err)
	}

	start := time.Now()
	req, err := fhttp.NewRequest(http.MethodGet, "https://setup.icloud.com/setup/ws/1/validate", nil)
	if err != nil {
		return false, 0, "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36")

	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return false, latency, "", fmt.Errorf("代理握手失败: %w", err)
	}
	defer resp.Body.Close()

	// 【BUG-15 修复】额外探测出口 IP,供运维确认代理出口与预期一致
	var exitIP string
	ipReq, ipErr := fhttp.NewRequest(http.MethodGet, "https://api.ipify.org", nil)
	if ipErr == nil {
		ipReq.Header.Set("User-Agent", "curl/8.0")
		if ipResp, ipFetchErr := client.Do(ipReq); ipFetchErr == nil {
			defer ipResp.Body.Close()
			buf := make([]byte, 64)
			if n, readErr := ipResp.Body.Read(buf); readErr == nil || n > 0 {
				exitIP = strings.TrimSpace(string(buf[:n]))
			}
		}
	}

	msg := fmt.Sprintf("代理连通成功 (Apple 网关响应 %d)", resp.StatusCode)
	if exitIP != "" {
		msg = fmt.Sprintf("出口 IP: %s (Apple 网关响应 %d)", exitIP, resp.StatusCode)
	}
	return true, latency, msg, nil
}

// Close 释放底层 Manager 的长连接池与客户端池 (PR-07 §10.4)。
func (b *managerBackend) Close() {
	b.cancelAllCamoufoxTasks()
	if b.mgr != nil {
		b.mgr.Close()
	}
}
