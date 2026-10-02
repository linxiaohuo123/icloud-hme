/**
 * [INPUT]: 依赖 testing, net/http, net/http/httptest, encoding/json, internal/store, internal/account
 * [OUTPUT]: 对外提供 TestAPIAuthMatrix 全路由鉴权矩阵回归
 * [POS]: internal/server 的路由级安全门禁：遍历 Gin 已注册的每条路由 × 每种凭据，核对 401、作用域 403、CSRF 403 与放行；新增路由默认按管理面要求校验
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/store"
)

// 鉴权矩阵期望值
const (
	authSkip  = ""      // 公开端点，不校验
	authPass  = "pass"  // 通过鉴权 (业务层结果不关心)
	auth401   = "401"   // 未认证
	authScope = "scope" // 403 SCOPE_DENIED
	authCSRF  = "csrf"  // 403 CSRF_INVALID
)

type matrixCred struct {
	name  string
	apply func(r *http.Request)
}

// expectedAuth 描述每类路由在每种凭据下的期望鉴权结果。
// 未显式归类的路由一律按管理面 (admin 作用域 + 写操作 CSRF) 期望校验，
// 新增路由若忘记挂鉴权或 CSRF 中间件会直接失败。
func expectedAuth(method, path, cred string) string {
	write := method != http.MethodGet && method != http.MethodHead
	queryCred := strings.HasPrefix(cred, "q-")
	switch {
	case path == "/livez" || path == "/readyz" || strings.HasPrefix(path, "/mail/view-assets") ||
		path == "/api/auth/login" || path == "/api/auth/session":
		return authSkip

	// 浏览器直链：verify 作用域，允许 URL 携带令牌，Cookie 会话可用
	case strings.HasPrefix(path, "/mail/"):
		switch cred {
		case "none":
			return auth401
		case "alloc", "q-alloc":
			return authScope
		default:
			return authPass
		}

	// 外部 v2：仅请求头令牌，拒绝 Cookie 与 URL 凭据
	case strings.HasPrefix(path, "/api/external/v2/"):
		need := "alloc"
		if strings.Contains(path, "/verification-requests") {
			need = "verify"
		}
		switch {
		case cred == "admin" || cred == need:
			return authPass
		case cred == "alloc" || cred == "verify":
			return authScope
		default:
			return auth401
		}

	case path == "/api/auth/logout":
		switch {
		case cred == "none" || queryCred:
			return auth401
		case cred == "cookie":
			return authCSRF
		default:
			return authPass
		}

	// 管理面
	default:
		switch {
		case cred == "none" || queryCred:
			return auth401
		case cred == "alloc" || cred == "verify":
			return authScope
		case cred == "cookie" && write:
			return authCSRF
		default:
			return authPass
		}
	}
}

func TestAPIAuthMatrix(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fb := &fakeBackend{accounts: []account.Summary{{ID: "acc_1", Status: "active", HasCookies: true}}}
	s := newWithBackendAndStore(fb, Config{AdminPassword: "admin-pass-2026-strong"}, st)
	defer s.Close()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	newToken := func(scopes string) string {
		tok, err := st.CreateToken("matrix_"+scopes, scopes, "")
		if err != nil {
			t.Fatal(err)
		}
		return tok.Token
	}
	allocTok, verifyTok, adminTok := newToken(store.ScopeAllocate), newToken(store.ScopeVerify), newToken(store.ScopeAdmin)
	cookie, csrf := login(t, ts, "admin-pass-2026-strong")

	bearer := func(v string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+v) }
	}
	query := func(v string) func(*http.Request) {
		return func(r *http.Request) {
			q := r.URL.Query()
			q.Set("token", v)
			r.URL.RawQuery = q.Encode()
		}
	}
	creds := []matrixCred{
		{"none", func(*http.Request) {}},
		{"cookie", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie}) }},
		{"cookie+csrf", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
			r.Header.Set("X-CSRF-Token", csrf)
		}},
		{"alloc", bearer(allocTok)},
		{"verify", bearer(verifyTok)},
		{"admin", bearer(adminTok)},
		{"q-alloc", query(allocTok)},
		{"q-verify", query(verifyTok)},
		{"q-admin", query(adminTok)},
	}

	params := strings.NewReplacer(":email", "x@icloud.com", ":message_id", "INBOX:1", ":operation_id", "op_x",
		":request_id", "vreq_x", ":account_id", "acc_1", ":id", "x")
	client := &http.Client{Timeout: 10 * time.Second}

	probe := func(method, path string, c matrixCred) (int, string) {
		u, _ := url.Parse(ts.URL + params.Replace(path))
		q := u.Query()
		if strings.HasPrefix(path, "/mail/code") || strings.Contains(path, "/verification-requests/") {
			q.Set("timeout", "0")
		}
		if strings.HasPrefix(path, "/mail/") && !strings.Contains(path, ":email") {
			q.Set("email", "x@icloud.com")
		}
		u.RawQuery = q.Encode()
		var body io.Reader
		if method != http.MethodGet && method != http.MethodHead {
			body = strings.NewReader(`{}`)
		}
		req, _ := http.NewRequest(method, u.String(), body)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if path == "/api/external/v2/allocate" {
			req.Header.Set("Idempotency-Key", fmt.Sprintf("matrix-%s", c.name))
		}
		c.apply(req)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s [%s]: %v", method, path, c.name, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var out struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(raw, &out)
		switch {
		case resp.StatusCode == http.StatusUnauthorized:
			return resp.StatusCode, auth401
		case resp.StatusCode == http.StatusForbidden && out.Code == "SCOPE_DENIED":
			return resp.StatusCode, authScope
		case resp.StatusCode == http.StatusForbidden && out.Code == "CSRF_INVALID":
			return resp.StatusCode, authCSRF
		default:
			return resp.StatusCode, authPass
		}
	}

	checked := 0
	var logout []string
	for _, rt := range s.r.Routes() {
		// 登出会作废共享会话，放到最后校验
		if rt.Path == "/api/auth/logout" {
			logout = append(logout, rt.Method)
			continue
		}
		for _, c := range creds {
			want := expectedAuth(rt.Method, rt.Path, c.name)
			if want == authSkip {
				continue
			}
			status, got := probe(rt.Method, rt.Path, c)
			checked++
			if got != want {
				t.Errorf("%s %s [%s]: want %s, got %d (%s)", rt.Method, rt.Path, c.name, want, status, got)
			}
		}
	}
	for _, method := range logout {
		for _, c := range creds {
			want := expectedAuth(method, "/api/auth/logout", c.name)
			status, got := probe(method, "/api/auth/logout", c)
			checked++
			if got != want {
				t.Errorf("%s /api/auth/logout [%s]: want %s, got %d (%s)", method, c.name, want, status, got)
			}
		}
	}
	if len(logout) == 0 || checked < 500 {
		t.Fatalf("route matrix too small: routes=%d checks=%d logout=%v", len(s.r.Routes()), checked, logout)
	}
}
