/**
 * [INPUT]: 依赖 errors, fmt, strings, time, unicode/utf8, icloud-hme/internal/hme
 * [OUTPUT]: 对外提供 ErrCookieExpired, (*Manager).UpdateCookies, (*Manager).ValidateAccount, (*Manager).markAccountError
 * [POS]: internal/account 的账号会话校验、健康巡检状态机与凭据失效判定
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package account

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"icloud-hme/internal/hme"
)

// ErrCookieExpired 表示账号 Cookie 已被 Apple 判定失效(401/403)，需要人工更新。
// 后台健康监控依据本哨兵错误区分"凭据级失效"与"瞬时网络错误"。
var ErrCookieExpired = errors.New("cookie expired")

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

// ValidateAccount 对指定账号执行一次会话校验并刷新状态(供后台健康监控周期调用)。
func (m *Manager) ValidateAccount(id string) error {
	return m.ValidateAccountWithContext(context.Background(), id)
}

// ValidateAccountWithContext 支持 Context 贯穿的账号会话校验 (PR-05 F10)。
//
// 成功: 状态回 active、刷新 LastValidated 与别名计数，并保存 validate 响应刷新的
// Cookie(等效一次会话保活)。
// 凭据级失败(401/403): 账号标记 error 并返回包装 ErrCookieExpired 的错误，
// 调度器与预热池会随之跳过该账号。
// 瞬时失败(网络抖动、超时): 不改变账号状态，返回原始错误由调用方记录。
func (m *Manager) ValidateAccountWithContext(ctx context.Context, id string) error {
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
		if err := client.ValidateSessionWithContext(ctx); err != nil {
			return err
		}
		serviceURL = client.ServiceURL()
		accountInfo = client.AccountInfo()
		// 顺带刷新别名计数，让配额水位始终有近 30 分钟内的真实值
		if list, listErr := client.ListAliasesWithContext(ctx); listErr == nil {
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
