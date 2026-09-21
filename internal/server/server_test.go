/**
 * [INPUT]: 依赖 testing, net/http, net/http/httptest, testing/fstest
 * [OUTPUT]: 对外提供 TestSPAAPI404JSON, TestSPAIndexServed
 * [POS]: internal/server 的 SPA 单页回退与 API 404 JSON 保护单元测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"testing/fstest"
)

// TestSPAAPI404JSON 验证 /api/not-found 返回 JSON 404 而非 HTML。
func TestSPAAPI404JSON(t *testing.T) {
	f := &fakeBackend{}
	s := newWithBackend(f, Config{
		Debug:         false,
		AdminPassword: "admin-pass-2026-strong",
	})
	// 替换 webui 为内存 FS,避免依赖构建产物
	webuiFS = fstest.MapFS{
		"index.html": {Data: []byte("<html>管理界面</html>")},
	}
	defer func() { webuiFS = nil }()

	req := httptest.NewRequest("GET", "/api/not-found", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("期望 404,得到 %d", rec.Code)
	}
	if !strings.HasPrefix(rec.Body.String(), `{"success":false`) {
		t.Fatalf("API 404 应返回 JSON: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"NOT_FOUND"`) {
		t.Fatalf("API 404 错误码应为 NOT_FOUND: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("API 404 不应返回 HTML: %s", rec.Body.String())
	}
}

// TestSPAIndexServed 验证根路径由 webui 服务(返回 index 而非 404)。
func TestSPAIndexServed(t *testing.T) {
	f := &fakeBackend{}
	s := newWithBackend(f, Config{
		Debug:         false,
		AdminPassword: "admin-pass-2026-strong",
	})
	webuiFS = fstest.MapFS{
		"index.html": {Data: []byte("<html>管理界面</html>")},
	}
	defer func() { webuiFS = nil }()

	req := httptest.NewRequest("GET", "/accounts?account_id=acc_test", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("SPA 路径期望 200,得到 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "管理界面") {
		t.Fatalf("SPA 路径应返回 index 内容: %s", rec.Body.String())
	}
}
