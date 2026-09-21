/**
 * [INPUT]: 依赖 internal/auth, gin
 * [OUTPUT]: 对外提供 requireSession 中间件 (支持 API Key 旁路), handleLogin, handleSession, handleLogout
 * [POS]: internal/server 的鉴权与会话管理层
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/auth"
	"icloud-hme/internal/store"
)

// authManager 是 auth.Manager 的别名,便于 handler 签名。
type authManager = auth.Manager

const sessionCookieName = "hme_session"

// sessionIDFromCookie 从请求 Cookie 中提取 session_id。
func sessionIDFromCookie(c *gin.Context) string {
	cookie, err := c.Request.Cookie(sessionCookieName)
	if err == nil && cookie.Value != "" {
		return cookie.Value
	}
	return ""
}

// requireSession 校验会话,失败返回 401/AUTH_REQUIRED。支持全局 API Key 及动态 Tokens 旁路。
//
// 通过后写入两个上下文键:
//
//	is_api_key_auth: 是否为令牌/Key 认证(决定是否跳过 CSRF)
//	auth_scopes:     授权作用域;浏览器会话恒为 "admin",令牌取库中 scopes
func requireSession(mgr *authManager, apiKey string, st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		reqKey := c.GetHeader("X-API-Key")
		if reqKey == "" {
			authHeader := c.GetHeader("Authorization")
			if strings.HasPrefix(authHeader, "Bearer ") {
				reqKey = strings.TrimPrefix(authHeader, "Bearer ")
			}
		}
		if reqKey != "" {
			// 恒时比较, 避免环境变量 Key 的时序侧信道
			if apiKey != "" && subtle.ConstantTimeCompare([]byte(reqKey), []byte(apiKey)) == 1 {
				p := auth.Principal{
					Kind:      auth.PrincipalAdmin,
					ID:        "admin",
					TokenName: "global_api_key",
					Scopes:    []string{store.ScopeAdmin},
				}
				c.Set("is_api_key_auth", true)
				c.Set("token_name", "global_api_key")
				c.Set("auth_scopes", store.ScopeAdmin)
				c.Set("principal", p)
				c.Next()
				return
			}
			if st != nil {
				if id, tokName, scopes, ok := st.ValidateTokenPrincipal(reqKey); ok {
					var scopeList []string
					if strings.TrimSpace(scopes) == "" || strings.TrimSpace(scopes) == store.ScopeAdmin {
						scopeList = []string{store.ScopeAdmin}
					} else {
						for _, sc := range strings.Split(scopes, ",") {
							sc = strings.TrimSpace(sc)
							if sc != "" {
								scopeList = append(scopeList, sc)
							}
						}
					}
					p := auth.Principal{
						Kind:      auth.PrincipalToken,
						ID:        id,
						TokenName: tokName,
						Scopes:    scopeList,
					}
					c.Set("is_api_key_auth", true)
					c.Set("token_name", tokName)
					c.Set("auth_scopes", scopes)
					c.Set("principal", p)
					c.Next()
					return
				}
			}
			failCode(c, http.StatusUnauthorized, "INVALID_API_KEY", "API Key 无效")
			return
		}

		sessionID := sessionIDFromCookie(c)
		if sessionID == "" {
			failCode(c, http.StatusUnauthorized, "AUTH_REQUIRED", "请先登录或提供有效的 API Key")
			return
		}
		if _, ok := mgr.Validate(sessionID); !ok {
			failCode(c, http.StatusUnauthorized, "AUTH_REQUIRED", "会话已失效,请重新登录")
			return
		}
		p := auth.Principal{
			Kind:      auth.PrincipalAdmin,
			ID:        "admin",
			TokenName: "admin_session",
			Scopes:    []string{store.ScopeAdmin},
		}
		c.Set("session_id", sessionID)
		// 浏览器管理员会话拥有全部作用域
		c.Set("auth_scopes", store.ScopeAdmin)
		c.Set("principal", p)
		c.Next()
	}
}

// getPrincipal 从 Gin 上下文中获取当前已认证的统一主体 (PR-04)
func getPrincipal(c *gin.Context) (auth.Principal, bool) {
	v, exists := c.Get("principal")
	if !exists {
		return auth.Principal{}, false
	}
	p, ok := v.(auth.Principal)
	return p, ok
}

// requireScope 在 requireSession 之后做最小权限校验。
// 对外发放的令牌可收窄为 "allocate,verify"，从而无法触达账号/令牌/设置等管理面接口。
func requireScope(scope string) gin.HandlerFunc {
	return func(c *gin.Context) {
		scopes, _ := c.Get("auth_scopes")
		scopesStr, _ := scopes.(string)
		if !store.HasScope(scopesStr, scope) {
			failCode(c, http.StatusForbidden, "SCOPE_DENIED", "当前令牌不具备该接口的授权作用域")
			c.Abort()
			return
		}
		c.Next()
	}
}

// setSessionCookie 设置会话 Cookie(固定属性:Path=/、HttpOnly、SameSite=Strict)。
func setSessionCookie(c *gin.Context, sessionID string, expiresAt time.Time, secure bool) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		Expires:  expiresAt,
	})
}

// clearSessionCookie 清除会话 Cookie(属性须与设置时一致,含 Secure)。
func clearSessionCookie(c *gin.Context, secure bool) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// handleLogin 处理 POST /api/auth/login。
func (s *Server) handleLogin(c *gin.Context) {
	var req struct {
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Password == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误")
		return
	}

	ip := c.ClientIP()
	allowed, retryAfter := s.limiter.Allow(ip)
	if !allowed {
		c.Header("Retry-After", formatRetryAfter(retryAfter))
		failCode(c, http.StatusTooManyRequests, "RATE_LIMITED", "登录尝试过于频繁,请稍后再试")
		return
	}

	sessionID, sess, valid := s.auth.Login(req.Password)
	if !valid {
		failCode(c, http.StatusUnauthorized, "INVALID_CREDENTIALS", "管理员密码错误")
		return
	}
	s.limiter.Success(ip)
	setSessionCookie(c, sessionID, sess.ExpiresAt, s.cfg.SecureCookie)
	ok(c, gin.H{
		"csrf_token": sess.CSRFToken,
		"expires_at": sess.ExpiresAt.Format(time.RFC3339),
	})
}

// formatRetryAfter 把等待时间格式化为秒数(Retry-After 头规范)。
func formatRetryAfter(d time.Duration) string {
	secs := int(d.Seconds())
	if secs < 1 {
		secs = 1
	}
	return strconv.Itoa(secs)
}

// handleSession 处理 GET /api/auth/session。
func (s *Server) handleSession(c *gin.Context) {
	sessionID := sessionIDFromCookie(c)
	if sessionID == "" {
		failCode(c, http.StatusUnauthorized, "AUTH_REQUIRED", "请先登录")
		return
	}
	sess, valid := s.auth.Validate(sessionID)
	if !valid {
		failCode(c, http.StatusUnauthorized, "AUTH_REQUIRED", "会话已失效,请重新登录")
		return
	}
	ok(c, gin.H{
		"csrf_token": sess.CSRFToken,
		"expires_at": sess.ExpiresAt.Format(time.RFC3339),
	})
}

// handleLogout 处理 POST /api/auth/logout(需会话 + CSRF)。
func (s *Server) handleLogout(c *gin.Context) {
	sessionID := sessionIDFromCookie(c)
	if sessionID != "" {
		s.auth.Logout(sessionID)
	}
	clearSessionCookie(c, s.cfg.SecureCookie)
	ok(c, gin.H{"logged_out": true})
}
