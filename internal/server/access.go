/**
 * [INPUT]: 依赖 net, net/http, net/url, strings, github.com/gin-gonic/gin
 * [OUTPUT]: 对外提供 isLoopbackHost, validateListenAddress, dnsRebindingMiddleware
 * [POS]: internal/server 的网络边界防护与安全准入守卫，阻断公网裸奔与 DNS Rebinding 攻击
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

// ====================================================================
// 网络安全与回环地址判定
// ====================================================================

// isLoopbackHost 判断主机名或 IP 是否属于本地回环 (localhost / 127.0.0.1 / ::1)。
func isLoopbackHost(host string) bool {
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validateListenAddress 校验服务监听地址。当监听非回环地址(如 0.0.0.0 或公网 IP)时，
// 强制要求配置管理密码或 API 密钥，杜绝中台资产未授权裸奔暴露。
func validateListenAddress(addr string, hasAuth bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// 若仅传入端口如 ":8080"，则 host 为空，在网络栈中代表绑定所有网卡 (0.0.0.0)
		if strings.HasPrefix(addr, ":") {
			host = ""
		} else {
			return fmt.Errorf("非法监听地址: %w", err)
		}
	}
	// 空主机名或 0.0.0.0 均代表非回环全局监听
	if (host == "" || !isLoopbackHost(host)) && !hasAuth {
		return fmt.Errorf("监听非回环地址 (%s) 存在安全风险，必须配置管理密码 (ADMIN_PASSWORD) 或 API Key", addr)
	}
	return nil
}

// dnsRebindingMiddleware 针对未配置全局凭据的本地开发模式，
// 严格校验 Host 与 Origin 请求头，阻断恶意网页通过 DNS Rebinding 或跨域探针盗刷本地母号。
func dnsRebindingMiddleware(hasAuth bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 已配置密码或 API Key 时由鉴权中间件接管，无需重复限制 Host
		if hasAuth {
			c.Next()
			return
		}

		peer, _, err := net.SplitHostPort(c.Request.RemoteAddr)
		if err != nil || !isLoopbackHost(peer) || !isLoopbackHost(c.Request.Host) {
			failCode(c, http.StatusForbidden, "FORBIDDEN", "未配置访问凭据时仅允许本机回环访问")
			c.Abort()
			return
		}

		// 拦截浏览器跨域伪造探测 (Origin 必须为回环)
		if origin := c.GetHeader("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !isLoopbackHost(u.Host) {
				failCode(c, http.StatusForbidden, "FORBIDDEN", "请求来源 (Origin) 不被允许: 触发 DNS Rebinding 防御")
				c.Abort()
				return
			}
		}

		c.Next()
	}
}
