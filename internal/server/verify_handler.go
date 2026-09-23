/**
 * [INPUT]: 依赖 gin, time, strings, strconv, icloud-hme/internal/mail
 * [OUTPUT]: 对外提供 verifyCodeHandler
 * [POS]: server 的验证码极速提取管道，基于 MailEventBus 实现纯内存事件分发，Trigger 即时触发收信，安全拒绝 auto_delete 副作用
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/auth"
)

// verifyCodeHandler 处理 GET /api/verify-code。
// 参数:
//
//	email (必须): 待接收验证码的别名邮箱
//	timeout (可选): 最大等待秒数, 默认 30, 上限 120
//	auto_delete (可选): 成功后是否自动在后台停用该别名以释放配额, 默认 false
func (s *Server) verifyCodeHandler(c *gin.Context) {
	email := strings.ToLower(strings.TrimSpace(c.Query("email")))
	if email == "" {
		email = strings.ToLower(strings.TrimSpace(c.Query("alias")))
	}
	if email == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "email 参数必填")
		return
	}

	timeoutSec := 30
	if raw := c.Query("timeout"); raw != "" {
		if t, err := strconv.Atoi(raw); err == nil && t > 0 {
			if t > 120 {
				t = 120
			}
			timeoutSec = t
		}
	}
	// 【PR-01 安全止损】GET verify-code 不再支持 auto_delete 参数。
	// HTTP GET 必须具备安全/无副作用语义 (RFC 9110)，严禁因查询操作导致别名被隐式停用；明确报错拒绝。
	if raw := c.Query("auto_delete"); raw == "true" || raw == "1" {
		failCode(c, http.StatusBadRequest, "UNSUPPORTED_PARAMETER", "GET verify-code 不再支持 auto_delete 参数，不可通过 GET 请求产生停用别名副作用")
		return
	}

	// 【PR-04 资源归属核验】在命中缓存与开启订阅前强制核验主体权限，杜绝越权读取
	p, exists := getPrincipal(c)
	if !exists || !p.CanVerify() {
		failCode(c, http.StatusForbidden, "SCOPE_DENIED", "当前主体无权读取验证码")
		return
	}
	if p.Kind == auth.PrincipalToken && !p.IsAdmin() && s.store != nil {
		if !s.store.IsEmailOwnedByToken(c.Request.Context(), email, p.ID) {
			failCode(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "未找到该别名或无权访问")
			return
		}
	}

	fresh := c.Query("fresh") == "true" || c.Query("nocache") == "true"

	// 内存订阅与原子缓存捕获 (缓存命中直接返回, 邮件到达触发事件唤醒, 避免 TOCTOU 竞态)
	subID, ch := s.eventBus.SubscribeWithFresh(email, fresh)
	defer s.eventBus.Unsubscribe(email, subID)

	// 立即唤醒后台拉信同步器，消除最多 2 秒的轮询盲等
	if s.syncWorker != nil {
		s.syncWorker.Trigger()
	}

	timer := time.NewTimer(time.Duration(timeoutSec) * time.Second)
	defer timer.Stop()

	select {
	case item := <-ch:
		// 【PR-04】长轮询唤醒后、完成交付前复查令牌状态 (防止等待期间令牌被撤销)
		if p.Kind == auth.PrincipalToken && s.store != nil {
			reqKey := c.GetHeader("X-API-Key")
			if reqKey == "" {
				authHeader := c.GetHeader("Authorization")
				if strings.HasPrefix(authHeader, "Bearer ") {
					reqKey = strings.TrimPrefix(authHeader, "Bearer ")
				}
			}
			if _, _, _, ok := s.store.ValidateTokenPrincipal(reqKey); !ok {
				failCode(c, http.StatusUnauthorized, "REVOKED_TOKEN", "令牌已被撤销")
				return
			}
		}
		// 成功返回后原子消费清除该别名缓存，杜绝后续重发验证码或二次登录误采陈旧历史 OTP
		s.eventBus.ConsumeCache(email)
		ok(c, gin.H{
			"email":      email,
			"code":       item.OTP.Code,
			"magic_link": item.OTP.MagicLink,
			"subject":    item.Subject,
			"from":       item.From,
			"date":       item.Date,
			"account_id": item.AccountID,
		})
	case <-timer.C:
		failCode(c, http.StatusRequestTimeout, "VERIFY_TIMEOUT", "等待验证码超时")
	case <-c.Request.Context().Done():
		return
	}
}

// autoDeactivateAlias 后台查找别名并停用以释放配额。
func (s *Server) autoDeactivateAlias(accountID, email string) {
	if accountID == "" && s.syncWorker != nil {
		accountID = s.syncWorker.GetAliasAccount(email)
	}
	if accountID == "" && s.store != nil {
		if accID, ok := s.store.FindAliasRoute(email); ok {
			accountID = accID
		} else if accID, ok := s.store.FindLeaseAccount(email); ok {
			accountID = accID
		}
	}
	if accountID == "" {
		return
	}
	aliases, err := s.be.ListAliases(accountID)
	if err != nil {
		return
	}
	for _, a := range aliases {
		if strings.EqualFold(a.Email, email) && a.Active {
			_, _ = s.be.SetAliasActive(accountID, a.AnonymousID, false)
			return
		}
	}
}
