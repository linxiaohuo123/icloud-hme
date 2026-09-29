/**
 * [INPUT]: 依赖 errors, fmt, strings, time, unicode/utf8, icloud-hme/internal/hme
 * [OUTPUT]: 对外提供 Cookie 校验错误、Cookie 与登录区域原子更新、会话校验与凭据代际保护
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
var ErrAccountIdentityMismatch = errors.New("Apple 账号身份与当前母账号不一致，请新建账号")
var ErrCookiesSavedInvalid = errors.New("Cookie 已保存，但校验未通过")
var ErrCookiesRejectedInvalid = errors.New("Cookie 校验未通过，原有凭据未改变")

func verifyAppleIdentity(acc *Account, info *hme.AccountInfo) error {
	if acc.AppleDSID != "" {
		if info == nil || info.DSID == "" || info.DSID != acc.AppleDSID {
			return ErrAccountIdentityMismatch
		}
		return nil
	}
	if acc.LastValidated != "" || acc.AliasTotal > 0 {
		current := strings.TrimSpace(acc.RealEmail)
		incoming := ""
		if info != nil {
			incoming = firstNonEmpty(info.AppleID, info.PrimaryEmail)
		}
		if current == "" || incoming == "" || !strings.EqualFold(current, incoming) {
			return ErrAccountIdentityMismatch
		}
	}
	return nil
}

// UpdateCookies 更新指定账号的 Cookie,并自动校验会话有效性。
func (m *Manager) UpdateCookies(id string, cookies map[string]string) error {
	return m.updateCookies(id, cookies, true, "", nil)
}

// UpdateCookiesIfValid 仅在新 Cookie 通过校验时替换原有凭据，供自动登录使用。
func (m *Manager) UpdateCookiesIfValid(id string, cookies map[string]string) error {
	return m.updateCookies(id, cookies, false, "", nil)
}

// UpdateCookiesIfValidForHost 将代理实际登录的区域和通过校验的 Cookie 一起保存。
func (m *Manager) UpdateCookiesIfValidForHost(id string, cookies map[string]string, host string) error {
	return m.updateCookies(id, cookies, false, host, nil)
}

func (m *Manager) UpdateBrowserSession(id string, session *hme.BrowserSession) error {
	if session == nil {
		return fmt.Errorf("missing browser session")
	}
	if err := session.Validate(session.Host); err != nil {
		return err
	}
	return m.updateCookies(id, session.CookieMap(), false, session.Host, session)
}

func (m *Manager) updateCookies(id string, cookies map[string]string, saveInvalid bool, host string, session *hme.BrowserSession) error {
	if len(cookies) == 0 {
		return fmt.Errorf("cookies 不能为空")
	}
	if host != "" {
		var err error
		host, err = validateHost(host)
		if err != nil {
			return err
		}
	}
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
		return fmt.Errorf("账号不存在: %s", id)
	}

	// 自动校验 Cookie 是否有效(锁外对快照操作)
	snap.Cookies = cookies
	snap.Session = session.Clone()
	originalHost := snap.Host
	if host != "" {
		snap.Host = host
	} else if snap.Host == "" {
		snap.Host = "icloud.com"
	}
	aliasesFetched := false
	var validateErr error
	client, err := hme.NewClientWithSession(cookies, snap.Session, snap.Host, snap.Proxy, false)
	if err != nil {
		snap.Status = "error"
		snap.LastError = "创建客户端失败: " + err.Error()
	} else {
		defer client.Close()
		if err := client.ValidateSession(); err != nil {
			validateErr = err
			// validate 即使失败也可能通过 Set-Cookie 刷新部分会话状态。
			snap.Cookies = client.CookieSnapshot()
			snap.Session = client.SessionSnapshot()
			snap.Status = "error"
			snap.LastError = "Cookie 校验失败: " + err.Error()
		} else {
			if err := verifyAppleIdentity(snap, client.AccountInfo()); err != nil {
				return err
			}
			snap.Status = "active"
			snap.LastValidated = time.Now().Format(time.RFC3339)
			snap.LastError = ""
			if info := client.AccountInfo(); info != nil {
				snap.AppleDSID = info.DSID
				snap.RealEmail = firstNonEmpty(info.AppleID, info.PrimaryEmail)
				if snap.ICloudEmail == "" {
					snap.ICloudEmail = deriveICloudEmail(info)
				}
			}
			if aliases, errList := listAliasesForSession(context.Background(), client); errList == nil {
				aliasesFetched = true
				snap.AliasTotal = len(aliases)
				snap.AliasActive = 0
				for _, al := range aliases {
					if al.Active {
						snap.AliasActive++
					}
				}
			} else if session != nil || errors.Is(errList, hme.ErrAuthFailed) || errors.Is(errList, hme.ErrOTPRequired) || errors.Is(errList, hme.ErrSessionIdentity) {
				validateErr = errList
				snap.Status = "error"
				snap.LastError = "HME 会话验证失败"
			}
			// ListAliases 之后再快照，避免丢掉列表阶段的 Set-Cookie
			snap.Cookies = client.CookieSnapshot()
			snap.Session = client.SessionSnapshot()
		}
	}
	if validationErr := errors.Join(err, validateErr); validationErr != nil && !saveInvalid {
		return fmt.Errorf("%w: %v", ErrCookiesRejectedInvalid, validationErr)
	}

	m.mu.Lock()
	cur, ok := m.accounts[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("账号不存在: %s", id)
	}
	if cur.credentialEpoch != snap.credentialEpoch || cur.Host != originalHost || cur.Proxy != snap.Proxy {
		m.mu.Unlock()
		return ErrSessionChanged
	}
	if validateErr == nil && err == nil {
		if identityErr := verifyAppleIdentity(cur, client.AccountInfo()); identityErr != nil {
			m.mu.Unlock()
			return identityErr
		}
	}
	old := *cur
	cur.Cookies = snap.Cookies
	cur.Session = snap.Session
	if host != "" {
		cur.Host = snap.Host
	}
	cur.ServiceURL = ""
	cur.Status = snap.Status
	cur.LastValidated = snap.LastValidated
	cur.LastError = snap.LastError
	cur.RealEmail = snap.RealEmail
	cur.AppleDSID = snap.AppleDSID
	if cur.ICloudEmail == "" {
		cur.ICloudEmail = snap.ICloudEmail
	}
	if aliasesFetched {
		cur.AliasTotal = snap.AliasTotal
		cur.AliasActive = snap.AliasActive
	}
	saveErr := m.saveAccount(cur)
	if saveErr != nil {
		*cur = old
	} else {
		cur.credentialEpoch++
	}
	m.mu.Unlock()
	if saveErr != nil {
		return errors.Join(err, validateErr, saveErr)
	}
	if validationErr := errors.Join(err, validateErr); validationErr != nil {
		return fmt.Errorf("%w: %v", ErrCookiesSavedInvalid, validationErr)
	}
	return nil
}

// ValidateAccount 对指定账号执行一次会话校验并刷新状态(供后台健康监控周期调用)。
func (m *Manager) ValidateAccount(id string) error {
	return m.ValidateAccountWithContext(context.Background(), id)
}

// ValidateAccountWithContext 支持 Context 贯穿的账号会话校验 (PR-05 F10)。
//
// 成功: 状态回 active、刷新 LastValidated 与别名计数，并保存 validate 响应刷新的
// Cookie；新版会话在明确认证失败时执行一次受控恢复，不保证延期。
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
	if len(acc.Cookies) == 0 && acc.Session == nil {
		m.mu.RUnlock()
		return fmt.Errorf("账号 %s 未配置 Cookie", id)
	}
	epoch := acc.credentialEpoch
	m.mu.RUnlock()

	// 走账号级客户端池: 本函数由 Cookie 监控器对每个账号每轮调用一次，
	// 若每次新建客户端就要为全部账号反复构造 Chrome TLS 指纹并重新握手。
	var (
		serviceURL  string
		accountInfo *hme.AccountInfo
		aliases     []hme.Alias
	)
	err := m.WithHMEClientContext(ctx, id, func(client *hme.Client) error {
		if err := client.ValidateSessionWithRecovery(ctx); err != nil {
			return err
		}
		serviceURL = client.ServiceURL()
		accountInfo = client.AccountInfo()
		// 顺带刷新别名计数，让配额水位始终有近 30 分钟内的真实值
		if list, listErr := listAliasesForSession(ctx, client); listErr == nil {
			aliases = list
		} else if client.SessionSnapshot() != nil || errors.Is(listErr, hme.ErrAuthFailed) || errors.Is(listErr, hme.ErrOTPRequired) || errors.Is(listErr, hme.ErrSessionIdentity) {
			return listErr
		}
		// 会话由账号池持锁原子保存，不在释放锁后重复回写旧快照。
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrSessionChanged) {
			return err
		}
		if errors.Is(err, ErrAccountIdentityMismatch) || errors.Is(err, hme.ErrSessionIdentity) {
			m.markAccountError(id, epoch, err.Error())
			return errors.Join(ErrAccountIdentityMismatch, err)
		}
		m.mu.RLock()
		current, exists := m.accounts[id]
		changed := !exists || current.credentialEpoch != epoch
		m.mu.RUnlock()
		if changed {
			return ErrSessionChanged
		}
		if errors.Is(err, ErrHMEClientUnavailable) {
			m.markAccountError(id, epoch, "创建客户端失败")
			return fmt.Errorf("%w: %v", ErrCookieExpired, err)
		}
		if errors.Is(err, hme.ErrAuthFailed) || errors.Is(err, hme.ErrOTPRequired) {
			m.markAccountError(id, epoch, "Cookie 已失效")
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
	if cur.credentialEpoch != epoch {
		m.mu.Unlock()
		return ErrSessionChanged
	}
	if identityErr := verifyAppleIdentity(cur, accountInfo); identityErr != nil {
		m.mu.Unlock()
		m.markAccountError(id, epoch, identityErr.Error())
		return identityErr
	}
	old := *cur
	cur.Status = "active"
	cur.LastValidated = time.Now().Format(time.RFC3339)
	cur.LastError = ""
	if serviceURL != "" {
		cur.ServiceURL = serviceURL
	}
	if accountInfo != nil {
		if accountInfo.DSID != "" {
			cur.AppleDSID = accountInfo.DSID
		}
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
	if saveErr != nil {
		*cur = old
	}
	m.mu.Unlock()
	return saveErr
}

// markAccountError 将账号标记为凭据失效并持久化(监控器专用，不改 LastValidated)。
func (m *Manager) markAccountError(id string, epoch uint64, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.accounts[id]
	if !ok || cur.credentialEpoch != epoch {
		return
	}
	oldStatus, oldError := cur.Status, cur.LastError
	cur.Status = "error"
	cur.LastError = reason
	if m.store != nil {
		if err := m.store.UpdateAccountFields(id, map[string]interface{}{
			"status":     "error",
			"last_error": reason,
		}); err != nil {
			cur.Status, cur.LastError = oldStatus, oldError
		}
		return
	}
	if err := m.saveJSON(); err != nil {
		cur.Status, cur.LastError = oldStatus, oldError
	}
}

// isAuthFailure 判断上游错误是否为凭据级失效(401/403/421，hme 客户端不重试直接返回)。
func isAuthFailure(msg string) bool {
	return strings.Contains(msg, "HTTP 401") || strings.Contains(msg, "HTTP 403") || strings.Contains(msg, "HTTP 421")
}

// deriveICloudEmail 从账号身份推导 iCloud 邮箱地址(用于 IMAP 登录)。
//
// 规则:
//  1. primaryEmail 是 @icloud.com/@me.com/@mac.com → 直接用
//  2. appleId 是上述域名 → 直接用
//  3. 若均非原生 iCloud 域名，则返回空（严禁盲猜拼接，避免误导 IMAP 认证失败）
func deriveICloudEmail(info *hme.AccountInfo) string {
	primary := strings.TrimSpace(info.PrimaryEmail)
	appleID := strings.TrimSpace(info.AppleID)

	if isICloudDomain(primary) {
		return primary
	}
	if isICloudDomain(appleID) {
		return appleID
	}
	return ""
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
