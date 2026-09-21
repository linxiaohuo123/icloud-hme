/**
 * [INPUT]: 依赖 gin, time, strings, strconv, icloud-hme/internal/mail
 * [OUTPUT]: 对外提供 verifyCodeHandler
 * [POS]: server 的验证码极速提取管道，基于 MailEventBus 实现纯内存事件分发与零锁争用
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
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
	autoDelete := c.Query("auto_delete") == "true"
	fresh := c.Query("fresh") == "true" || c.Query("nocache") == "true"

	// 内存订阅与原子缓存捕获 (缓存命中直接返回, 邮件到达触发事件唤醒, 避免 TOCTOU 竞态)
	subID, ch := s.eventBus.SubscribeWithFresh(email, fresh)
	defer s.eventBus.Unsubscribe(email, subID)

	timer := time.NewTimer(time.Duration(timeoutSec) * time.Second)
	defer timer.Stop()

	select {
	case item := <-ch:
		// 成功返回后原子消费清除该别名缓存，杜绝后续重发验证码或二次登录误采陈旧历史 OTP
		s.eventBus.ConsumeCache(email)
		if autoDelete {
			goSafe("auto-deactivate-alias", func() { s.autoDeactivateAlias(item.AccountID, email) })
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
