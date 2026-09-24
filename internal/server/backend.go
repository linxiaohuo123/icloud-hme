/**
 * [INPUT]: 依赖 internal/account.Manager, internal/hme.Client, internal/mail.Client/WebClient
 * [OUTPUT]: 对外提供 Backend 接口、managerBackend 生产适配器与 BackendError 错误结构
 * [POS]: internal/server 的高层业务门面与依赖倒置边界，隔离 Handler 与具体协议实现
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

// Package server - 可替换业务接口与 Manager 适配器。
//
// Backend 边界固定为高层业务动作,不把具体 *hme.Client 或 *mail.Client 暴露给 handler。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	SetMailbox(string, account.MailboxConfig) (account.Summary, error)
	LoginAccount(string, string, string) (account.Summary, error)
	RemoveAccount(string) bool
	CreateAlias(string, string) (*hme.CreateResult, error)
	CreateAliasContext(context.Context, string, string) (*hme.CreateResult, error)
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
	if err := b.mgr.UpdateCookies(id, parsed); err != nil {
		return account.Summary{}, mapAccountErr(err)
	}
	b.invalidateSummaryCache()
	sum, ok := b.mgr.GetAccount(id)
	if !ok {
		return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
	}
	return sum.Summary(), nil
}

// SetAppPassword 设置 iCloud 邮箱与 App 专用密码并测试 IMAP 连接。
func (b *managerBackend) SetAppPassword(id, icloudEmail, appPassword string) (account.Summary, error) {
	if err := b.mgr.SetAppPassword(id, icloudEmail, appPassword); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "账号不存在") {
			return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
		}
		if strings.Contains(msg, "不能为空") {
			return account.Summary{}, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: msg}
		}
		// IMAP 连接失败属于上游错误,不拼接详细错误
		return account.Summary{}, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "IMAP 验证失败,请检查邮箱与 App 专用密码"}
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
	if err := b.mgr.SetMailbox(id, config); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "账号不存在") {
			return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
		}
		if strings.Contains(msg, "不能为空") || strings.Contains(msg, "无效") {
			return account.Summary{}, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: msg}
		}
		return account.Summary{}, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "收件邮箱验证失败,请检查邮箱、授权码和 IMAP 配置"}
	}
	b.invalidateSummaryCache()
	sum, ok := b.mgr.GetAccount(id)
	if !ok {
		return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
	}
	return sum.Summary(), nil
}

// LoginAccount 使用 iCloud 密码登录账号,成功只返回 Summary,绝不返回 Cookies。
func (b *managerBackend) LoginAccount(id, password, otpCode string) (account.Summary, error) {
	var otpProvider hme.OTPProvider
	if otpCode != "" {
		otp := otpCode
		otpProvider = func() (string, error) { return otp, nil }
	}

	client, err := b.mgr.HMEClientWithPassword(id, password, otpProvider)
	if err != nil {
		return account.Summary{}, classifyLoginErr(err)
	}
	_ = client
	b.invalidateSummaryCache()
	sum, ok := b.mgr.GetAccount(id)
	if !ok {
		return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
	}
	return sum.Summary(), nil
}

// classifyLoginErr 把 iCloud 登录错误映射为稳定错误。
func classifyLoginErr(err error) *BackendError {
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
	if isSessionError(msg) || strings.Contains(msg, "auth complete") || strings.Contains(msg, "401") || strings.Contains(msg, "403") {
		return &BackendError{Status: http.StatusUnauthorized, Code: "APPLE_AUTH_BLOCKED", Message: "Apple 拒绝了模拟密码登录（触发了苹果安全风控），请点击【更新 Cookie】直接粘贴浏览器 Cookie 激活"}
	}
	return &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "iCloud 登录失败: " + msg}
}

// RemoveAccount 删除账号并彻底驱逐关联的内存别名缓存。
func (b *managerBackend) RemoveAccount(id string) bool {
	b.invalidateAliasCache(id)
	ok := b.mgr.RemoveAccount(id)
	if ok {
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
	return strings.Contains(m, "401") || strings.Contains(m, "403") ||
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
	if b.mgr != nil {
		b.mgr.Close()
	}
}
