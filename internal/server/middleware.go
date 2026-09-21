/**
 * [INPUT]: 依赖 gin, internal/auth, net/http
 * [OUTPUT]: 对外提供 securityHeadersMiddleware, apiCacheControlMiddleware, bodyLimitMiddleware, csrfCheck
 * [POS]: internal/server 的安全与防御中间件层 (API Key 请求自动跳过 CSRF 检查，请求体限制 1MB 防 OOM)
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// maxBodyBytes 是 JSON 请求体上限。
const maxBodyBytes = 1 << 20 // 1 MiB

// bodyLimitMiddleware 限制请求体最大字节数，阻断恶意超大 payload 耗尽内存导致 OOM。
func bodyLimitMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
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
