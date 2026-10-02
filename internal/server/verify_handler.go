/**
 * [INPUT]: 依赖 gin, time, strconv, icloud-hme/internal/auth, icloud-hme/internal/mail
 * [OUTPUT]: 对外提供 verifyCodeHandler
 * [POS]: server 的验证码提取管道，交付前复查原令牌凭据或直链签名，Trigger 即时触发收信，精确消费来源事件，停机显式返回 503，安全拒绝 auto_delete 副作用
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/auth"
	"icloud-hme/internal/mail"
)

// verifyCodeHandler 处理浏览器直链取码 GET /mail/code 与 /mail/code/:email。
// 参数:
//
//	email (必须): 待接收验证码的别名邮箱
//	timeout (可选): 最大等待秒数, 默认 30, 上限 120; 0 表示只查缓存立即返回
//	auto_delete: 已废弃并明确拒绝 (传入返回 400 UNSUPPORTED_PARAMETER)
func (s *Server) verifyCodeHandler(c *gin.Context) {
	email := directMailEmail(c)
	if email == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "email 参数必填")
		return
	}

	timeoutSec := 30
	if raw := c.Query("timeout"); raw != "" {
		if t, err := strconv.Atoi(raw); err == nil && t >= 0 {
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
	if p.Kind == auth.PrincipalLink && p.ID != email {
		failCode(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "未找到该别名或无权访问")
		return
	}

	fresh := c.Query("fresh") == "true" || c.Query("nocache") == "true"

	// 内存订阅与原子缓存捕获 (缓存命中直接返回, 邮件到达触发事件唤醒, 避免 TOCTOU 竞态)
	subID, ch := s.eventBus.SubscribeWithFresh(email, fresh)
	defer s.eventBus.Unsubscribe(email, subID)

	// 立即唤醒后台拉信同步器，消除最多 2 秒的轮询盲等
	if s.syncWorker != nil {
		s.syncWorker.Trigger()
	}

	// deliver 交付事件：轮换保持主体 ID 不变，必须重新验证原请求凭据才能阻止旧请求继续取码
	deliver := func(item *mail.CachedOTP) {
		if p.Kind == auth.PrincipalToken && s.store != nil {
			id, _, _, valid := s.store.ValidateTokenPrincipal(requestAPIKey(c))
			if !valid || id != p.ID {
				failCode(c, http.StatusUnauthorized, "REVOKED_TOKEN", "令牌已失效或被轮换")
				return
			}
		}
		if p.Kind == auth.PrincipalLink && !s.validMailLink(email, c.Query("exp"), c.Query("sig")) {
			failCode(c, http.StatusUnauthorized, "INVALID_LINK", "直链已过期或已被作废")
			return
		}
		// 【C3】精准消费采用的事件 ID，绝不整桶清除更晚到达的其它新事件
		if item != nil && item.EventID != "" {
			s.eventBus.ConsumeEvent(email, item.EventID, item.Source)
		}
		if c.Query("raw") == "1" || c.Query("format") == "text" {
			c.String(http.StatusOK, item.OTP.Code)
			return
		}
		ok(c, gin.H{
			"email":      email,
			"code":       item.OTP.Code,
			"magic_link": item.OTP.MagicLink,
			"subject":    item.Subject,
			"from":       item.From,
			"date":       item.Date,
			"account_id": item.AccountID,
		})
	}

	// timeout=0：只查已缓存的事件，不占等待者名额，立即返回
	if timeoutSec == 0 {
		select {
		case item := <-ch:
			deliver(item)
		default:
			failCode(c, http.StatusRequestTimeout, "VERIFY_TIMEOUT", "暂无可用验证码")
		}
		return
	}

	// 申请长轮询等待者名额，并释放短阶段在途名额 (PR-CONCURRENCY T1)
	if s.requestLimiter != nil {
		relWaiter, err := s.requestLimiter.AcquireWaiter(p, email)
		if err != nil {
			var be *BackendError
			if errors.As(err, &be) {
				failCode(c, be.Status, be.Code, be.Message)
				return
			}
			failCode(c, http.StatusTooManyRequests, "VERIFY_WAITER_LIMIT", err.Error())
			return
		}
		defer relWaiter()
	}
	releaseInflight(c)

	timer := time.NewTimer(time.Duration(timeoutSec) * time.Second)
	defer timer.Stop()

	var srvDone <-chan struct{}
	if s != nil && s.ctx != nil {
		srvDone = s.ctx.Done()
	}

	select {
	case item := <-ch:
		deliver(item)
	case <-timer.C:
		failCode(c, http.StatusRequestTimeout, "VERIFY_TIMEOUT", "等待验证码超时")
	case <-c.Request.Context().Done():
		if s.ctx != nil && s.ctx.Err() != nil {
			backendFail(c, &BackendError{Status: http.StatusServiceUnavailable, Code: "SERVER_SHUTTING_DOWN", Message: "服务正在优雅停机"})
		}
		return
	case <-srvDone:
		backendFail(c, &BackendError{Status: http.StatusServiceUnavailable, Code: "SERVER_SHUTTING_DOWN", Message: "服务正在优雅停机"})
		return
	}
}
