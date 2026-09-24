package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/store"
)

// trackingBackend 用于监控探针请求期间是否有任何外部上游调用发生
type trackingBackend struct {
	fakeBackend
	upstreamCalls int64
}

func (b *trackingBackend) ListAliases(accountID string) ([]hme.Alias, error) {
	atomic.AddInt64(&b.upstreamCalls, 1)
	return b.fakeBackend.ListAliases(accountID)
}

func (b *trackingBackend) CreateAlias(accountID, label string) (*hme.CreateResult, error) {
	atomic.AddInt64(&b.upstreamCalls, 1)
	return b.fakeBackend.CreateAlias(accountID, label)
}

func (b *trackingBackend) ListInboxContext(ctx context.Context, q InboxQuery) (InboxResult, error) {
	atomic.AddInt64(&b.upstreamCalls, 1)
	return b.fakeBackend.ListInboxContext(ctx, q)
}

func (b *trackingBackend) GetAccount(id string) (account.Summary, error) {
	return b.fakeBackend.GetAccount(id)
}

// 1. 验证探针状态码、Content-Type、Cache-Control 与约定响应内容
func TestHealthCheck_LivezAndReadyzContract(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("failed to init store: %v", err)
	}
	defer st.Close()

	f := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Status: "active", HasCookies: true}},
	}
	s := newWithBackendAndStore(f, Config{AdminPassword: "admin-pass-2026-strong"}, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// 1.1 /livez 规范契约
	{
		resp, err := ts.Client().Get(ts.URL + "/livez")
		if err != nil {
			t.Fatalf("GET /livez failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected /livez status 200, got %d", resp.StatusCode)
		}
		ct := resp.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("expected Content-Type application/json, got %s", ct)
		}
		cc := resp.Header.Get("Cache-Control")
		if !strings.Contains(cc, "no-cache") || !strings.Contains(cc, "no-store") {
			t.Fatalf("expected Cache-Control to disable caching, got %s", cc)
		}

		var body map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("failed to decode JSON response: %v", err)
		}
		if body["status"] != "ok" {
			t.Fatalf("expected status 'ok', got %v", body["status"])
		}
	}

	// 1.2 /readyz 规范契约 (健康状态)
	{
		resp, err := ts.Client().Get(ts.URL + "/readyz")
		if err != nil {
			t.Fatalf("GET /readyz failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected /readyz status 200, got %d", resp.StatusCode)
		}
		ct := resp.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("expected Content-Type application/json, got %s", ct)
		}
		cc := resp.Header.Get("Cache-Control")
		if !strings.Contains(cc, "no-cache") || !strings.Contains(cc, "no-store") {
			t.Fatalf("expected Cache-Control to disable caching, got %s", cc)
		}

		var body map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("failed to decode JSON response: %v", err)
		}
		if body["status"] != "ok" {
			t.Fatalf("expected status 'ok', got %v", body["status"])
		}
	}
}

// 2. 验证旧版本回退 SPA HTML 时不能被当作探针成功
func TestHealthCheck_SPAHTMLFallbackCannotPassAsHealthy(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("failed to init store: %v", err)
	}
	defer st.Close()

	f := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Status: "active", HasCookies: true}},
	}
	s := newWithBackendAndStore(f, Config{AdminPassword: "admin-pass-2026-strong"}, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// 请求一个未注册的伪路径，模拟旧版本中未注册 /livez 时的 SPA fallback 行为
	resp, err := ts.Client().Get(ts.URL + "/unregistered_legacy_probe")
	if err != nil {
		t.Fatalf("GET request failed: %v", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	bodyStr := string(bodyBytes)

	// 旧版本 NoRoute 针对 GET 请求会返回 200 OK 并附带 HTML 内容 (SPA Fallback)
	// 验证：规范探针解析器必须拒绝该响应，绝不能将 200 HTML 误判为健康
	isHealthy := false
	var parsed map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &parsed); err == nil {
		if s, ok := parsed["status"].(string); ok && s == "ok" {
			isHealthy = true
		}
	}

	if isHealthy {
		t.Fatalf("SPA HTML fallback was erroneously treated as healthy! body: %s", bodyStr)
	}
}

// 3. 验证数据库不可用、探测超时与请求取消
func TestHealthCheck_DatabaseUnavailableTimeoutAndCancellation(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("failed to init store: %v", err)
	}

	f := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Status: "active", HasCookies: true}},
	}
	s := newWithBackendAndStore(f, Config{AdminPassword: "admin-pass-2026-strong"}, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// 3.1 数据库关闭后：/readyz 必须返回 503 UNAVAILABLE
	_ = st.Close()
	{
		resp, err := ts.Client().Get(ts.URL + "/readyz")
		if err != nil {
			t.Fatalf("GET /readyz failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 for closed store, got %d", resp.StatusCode)
		}

		var body map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("failed to parse 503 response: %v", err)
		}
		if body["status"] != "unavailable" {
			t.Fatalf("expected status 'unavailable', got %v", body["status"])
		}
	}

	// 3.2 Store 为 nil 时的边界保护：必须返回 503
	{
		sNil := newWithBackendAndStore(f, Config{AdminPassword: "admin-pass-2026-strong"}, nil)
		tsNil := httptest.NewServer(sNil.Handler())
		defer tsNil.Close()

		resp, err := tsNil.Client().Get(tsNil.URL + "/readyz")
		if err != nil {
			t.Fatalf("GET /readyz with nil store failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 for nil store, got %d", resp.StatusCode)
		}
	}

	// 3.3 客户端请求主动取消 (Request Cancellation)：服务端必须安全收敛不 panic
	{
		reqCtx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, ts.URL+"/readyz", nil)
		if err != nil {
			t.Fatalf("failed to create request: %v", err)
		}

		// 触发请求并发起取消
		go func() {
			time.Sleep(10 * time.Millisecond)
			cancel()
		}()

		_, _ = ts.Client().Do(req)
		// 关键在于确认服务未 panic 且后续请求仍能正常响应
		liveResp, liveErr := ts.Client().Get(ts.URL + "/livez")
		if liveErr != nil || liveResp.StatusCode != http.StatusOK {
			t.Fatalf("server failed to recover after request cancellation: %v", liveErr)
		}
		_ = liveResp.Body.Close()
	}
}

// 4. 验证探针不访问 Apple、不泄露敏感信息
func TestHealthCheck_NoUpstreamCallsAndNoSecretLeak(t *testing.T) {
	tempDir := t.TempDir()
	st, err := store.NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to init store: %v", err)
	}
	defer st.Close()

	secretPassword := "SuperSecretAdminPassword123!"
	secretAPIKey := "secret-global-api-key-9999"
	secretToken := "am_partner_secret_token_8888"

	_ = st.SaveToken(store.APIToken{
		Name:   "partner",
		Token:  secretToken,
		Scopes: store.DefaultExternalScopes,
	})

	tracker := &trackingBackend{
		fakeBackend: fakeBackend{
			accounts: []account.Summary{{ID: "acc_secret", Status: "active", HasCookies: true}},
		},
	}

	s := newWithBackendAndStore(tracker, Config{
		AdminPassword: secretPassword,
		APIKey:        secretAPIKey,
	}, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// 测试 /livez 与 /readyz
	for _, path := range []string{"/livez", "/readyz"} {
		resp, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s failed: %v", path, err)
		}

		bodyBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		bodyStr := string(bodyBytes)

		// 4.1 断言外部上游调用严格为 0
		if calls := atomic.LoadInt64(&tracker.upstreamCalls); calls > 0 {
			t.Fatalf("probe %s triggered %d upstream calls! must be 0", path, calls)
		}

		// 4.2 断言响应内容不泄露任何系统口令、Token、目录物理路径或敏感配置
		if strings.Contains(bodyStr, secretPassword) {
			t.Fatalf("probe %s leaked AdminPassword!", path)
		}
		if strings.Contains(bodyStr, secretAPIKey) {
			t.Fatalf("probe %s leaked APIKey!", path)
		}
		if strings.Contains(bodyStr, secretToken) {
			t.Fatalf("probe %s leaked API token!", path)
		}
		if strings.Contains(bodyStr, tempDir) {
			t.Fatalf("probe %s leaked storage directory path!", path)
		}
		if strings.Contains(bodyStr, "acc_secret") {
			t.Fatalf("probe %s leaked account identifier!", path)
		}
	}
}

// 5. 验证 /api/auth/session 未认证时严格为 401，且不被健康检查削弱
func TestHealthCheck_SessionAuthRemainsStrict401(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("failed to init store: %v", err)
	}
	defer st.Close()

	f := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Status: "active", HasCookies: true}},
	}
	s := newWithBackendAndStore(f, Config{AdminPassword: "admin-pass-2026-strong"}, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/api/auth/session")
	if err != nil {
		t.Fatalf("GET /api/auth/session failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected /api/auth/session without cookies to return 401 Unauthorized, got %d", resp.StatusCode)
	}

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode 401 response: %v", err)
	}
	if body["success"] != false {
		t.Fatalf("expected success: false in 401 body, got %v", body["success"])
	}
	if body["code"] != "AUTH_REQUIRED" {
		t.Fatalf("expected code: 'AUTH_REQUIRED', got %v", body["code"])
	}
}
