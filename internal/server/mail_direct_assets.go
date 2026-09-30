/**
 * [INPUT]: 内嵌邮件查信页的 CSP 兼容 CSS/JavaScript 资源
 * [OUTPUT]: 对外提供 mailView 外部静态资源响应
 * [POS]: internal/server 的邮件查信页资源管道
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed mail_direct.css mail_direct.js
var mailDirectAssets embed.FS

func serveMailDirectAsset(name, contentType string) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := mailDirectAssets.ReadFile(name)
		if err != nil {
			c.Status(http.StatusNotFound)
			return
		}
		c.Data(http.StatusOK, contentType, body)
	}
}
