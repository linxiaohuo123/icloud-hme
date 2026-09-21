/**
 * [INPUT]: 依赖 embed, io/fs, net/http
 * [OUTPUT]: 对外提供 Embedded, Handler 等静态资源内嵌与 SPA fallback 能力
 * [POS]: internal/webui 的静态资源服务层
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

// Package webui 提供内嵌前端静态资源服务与 SPA 路由 fallback。
//
// 生产产物写入 internal/webui/dist/ 并由 embed.FS 内嵌,随单个 Go 二进制分发。
package webui

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed dist/*
var embedded embed.FS

// dist 是内嵌的 dist 子文件系统。
var dist fs.FS

func init() {
	dist, _ = fs.Sub(embedded, "dist")
}

// Embedded 返回内嵌的 dist 文件系统。
func Embedded() (fs.FS, error) {
	if dist == nil {
		return nil, errors.New("前端资源未构建")
	}
	return dist, nil
}

// mimeByExt 返回常见扩展名对应的 Content-Type。
func mimeByExt(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "text/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".json"):
		return "application/json"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".jpg"), strings.HasSuffix(name, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(name, ".gif"):
		return "image/gif"
	case strings.HasSuffix(name, ".ico"):
		return "image/x-icon"
	case strings.HasSuffix(name, ".woff2"):
		return "font/woff2"
	case strings.HasSuffix(name, ".woff"):
		return "font/woff"
	case strings.HasSuffix(name, ".txt"):
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// hashedAsset 判断路径是否含哈希文件名(如 app-abc123.js)。
func hashedAsset(name string) bool {
	base := path.Base(name)
	dot := strings.Index(base, ".")
	if dot < 0 {
		return false
	}
	return len(base[:dot]) > 1 && strings.Contains(base[:dot], "-")
}

// Handler 返回服务内嵌静态资源并支持 SPA fallback 的 http.Handler。
//
// 只允许 GET/HEAD;路径先 path.Clean 并拒绝 "..";存在文件就按扩展名服务,
// 不存在且路径不含文件扩展名时返回 index.html;生产 dist 未生成时返回
// 清晰的 503 中文纯文本,而不是 panic。
func Handler(fsys fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
			return
		}

		clean := path.Clean("/" + r.URL.Path)
		if strings.Contains(clean, "..") {
			http.Error(w, "路径无效", http.StatusBadRequest)
			return
		}
		name := strings.TrimPrefix(clean, "/")
		if name == "" {
			name = "index.html"
		}

		// 检查文件是否存在
		file, err := fsys.Open(name)
		if err == nil {
			defer file.Close()
			if info, statErr := file.Stat(); statErr == nil && !info.IsDir() {
				if strings.HasSuffix(name, ".html") || name == "index.html" {
					w.Header().Set("Cache-Control", "no-cache")
					w.Header().Set("Pragma", "no-cache")
				} else if hashedAsset(name) {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				w.Header().Set("Content-Type", mimeByExt(name))
				// 显式写状态,避免继承外层(如 gin NoRoute)预设的状态码
				w.WriteHeader(http.StatusOK)
				if r.Method == http.MethodHead {
					return
				}
				_, _ = io.Copy(w, file)
				return
			}
			// 目录:落到 SPA fallback
		}

		// 路径含文件扩展名 → 404(不 fallback)
		if path.Ext(clean) != "" && clean != "/" && !strings.HasSuffix(clean, "/") {
			http.NotFound(w, r)
			return
		}

		// SPA fallback:返回 index.html(不存在时 503 中文提示)
		index, err := fsys.Open("index.html")
		if err != nil {
			msg := "管理界面资源未构建,请先运行 npm run build 后重新编译服务"
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintln(w, msg)
			return
		}
		defer index.Close()
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// 显式写状态,避免继承外层(如 gin NoRoute)预设的状态码
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = io.Copy(w, index)
	})
}
