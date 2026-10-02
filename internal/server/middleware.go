/**
 * [INPUT]: 依赖 context, gin, internal/auth, net/http
 * [OUTPUT]: 对外提供 securityHeadersMiddleware, apiCacheControlMiddleware, bodyLimitMiddleware, csrfCheck
 * [POS]: internal/server 的安全与防御中间件层；请求体限制 1MB、仅对有正文请求施加 15 秒读取预算和物理取消，防止慢正文及 net/http 排空阻塞停机，且不误杀无正文长请求
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// maxBodyBytes 是 JSON 请求体上限。
const maxBodyBytes = 1 << 20 // 1 MiB

// bodyReadTimeout 是请求体读取预算；变量形式供测试缩短。
var bodyReadTimeout = 15 * time.Second

type readTimeoutReader struct {
	rc       io.ReadCloser
	ctx      context.Context
	deadline time.Time
}

func (r *readTimeoutReader) Read(p []byte) (n int, err error) {
	if r.ctx != nil && r.ctx.Err() != nil {
		return 0, r.ctx.Err()
	}
	if time.Now().After(r.deadline) {
		return 0, errors.New("request body read timeout")
	}
	return r.rc.Read(p)
}

func (r *readTimeoutReader) Close() error {
	return r.rc.Close()
}

// bodyLimitMiddleware 限制请求体最大字节数并施加 15 秒物理读取超时，阻断慢速攻击与超大 payload 耗尽内存导致 OOM。
//
// 无正文请求不设连接读取期限：net/http 在 handler 运行前已开始后台读连接以探测断开，
// 期限到达会被当作客户端断开并取消请求 Context，导致超过 15 秒的 GET/DELETE/空 POST 被误杀。
// 有正文请求读到 EOF 后 net/http 才启动后台读，并在启动时清除期限，因此只约束正文读取阶段。
func bodyLimitMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body == nil || c.Request.Body == http.NoBody {
			c.Next()
			return
		}
		timeout := bodyReadTimeout
		rc := http.NewResponseController(c.Writer)
		_ = rc.SetReadDeadline(time.Now().Add(timeout))
		cancelDone := make(chan struct{})
		stopCancel := context.AfterFunc(c.Request.Context(), func() {
			// 取消必须打断已经进入 net/http 的阻塞 Read。
			_ = rc.SetReadDeadline(time.Now())
			close(cancelDone)
		})
		defer func() {
			if !stopCancel() {
				<-cancelDone
			}
			// 已取消连接保留到期期限，net/http 收尾时还可能排空未读正文。
			// 清零会让 handler 已退出、HTTP Shutdown 却再次阻塞在正文排空。
			if c.Request.Context().Err() == nil {
				_ = rc.SetReadDeadline(time.Time{})
			}
		}()

		c.Request.Body = &readTimeoutReader{
			rc:       http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes),
			ctx:      c.Request.Context(),
			deadline: time.Now().Add(timeout),
		}
		c.Next()
	}
}

// securityHeaders 是全局安全响应头。
var securityHeaders = map[string]string{
	"Content-Security-Policy": "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'",
	"X-Content-Type-Options":  "nosniff",
	"Referrer-Policy":         "no-referrer",
	"Permissions-Policy":      "camera=(), microphone=(), geolocation=()",
}

// securityHeadersMiddleware 设置全局安全响应头。
func securityHeadersMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		for k, v := range securityHeaders {
			c.Header(k, v)
		}
		c.Next()
	}
}

// apiCacheControlMiddleware 给 API 响应设置 no-store。
func apiCacheControlMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Next()
	}
}

// csrfCheck 校验状态变更请求的 CSRF token。API Key 认证时自动跳过。
func csrfCheck(mgr *authManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetBool("is_api_key_auth") {
			c.Next()
			return
		}
		sessionID := sessionIDFromCookie(c)
		if sessionID == "" {
			failCode(c, http.StatusForbidden, "CSRF_INVALID", "缺少会话")
			return
		}
		token := c.GetHeader("X-CSRF-Token")
		if token == "" || !mgr.ValidateCSRF(sessionID, token) {
			failCode(c, http.StatusForbidden, "CSRF_INVALID", "CSRF 校验失败")
			return
		}
		c.Next()
	}
}
