package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// ValidateHealthProbeResponse 模拟或供容器冒烟验收脚本使用的同一健康判定入口。
// 必须严格核验：
// 1. HTTP 状态码 == 200;
// 2. Content-Type 为 application/json (严禁 text/html);
// 3. 响应体为合法 JSON 且包含 status == "ok"。
func ValidateHealthProbeResponse(statusCode int, header http.Header, bodyBytes []byte) error {
	if statusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d, expected 200", statusCode)
	}
	ct := header.Get("Content-Type")
	if strings.Contains(strings.ToLower(ct), "text/html") {
		return fmt.Errorf("invalid content-type: %s (SPA HTML fallback rejected)", ct)
	}
	if !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		return fmt.Errorf("unexpected content-type: %s, expected application/json", ct)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		return fmt.Errorf("body is not valid JSON: %w (preview: %.80s)", err, string(bodyBytes))
	}
	if status, ok := payload["status"].(string); !ok || status != "ok" {
		return fmt.Errorf("expected status 'ok', got %v", payload["status"])
	}
	return nil
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

	// 1.1 /livez 契约核验
	{
		resp, err := ts.Client().Get(ts.URL + "/livez")
		if err != nil {
			t.Fatalf("GET /livez failed: %v", err)
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if err := ValidateHealthProbeResponse(resp.StatusCode, resp.Header, bodyBytes); err != nil {
			t.Fatalf("/livez failed contract check: %v", err)
		}
		cc := resp.Header.Get("Cache-Control")
		if !strings.Contains(cc, "no-cache") || !strings.Contains(cc, "no-store") {
			t.Fatalf("expected Cache-Control to disable caching, got %s", cc)
		}
	}

	// 1.2 /readyz 契约核验
	{
		resp, err := ts.Client().Get(ts.URL + "/readyz")
		if err != nil {
			t.Fatalf("GET /readyz failed: %v", err)
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if err := ValidateHealthProbeResponse(resp.StatusCode, resp.Header, bodyBytes); err != nil {
			t.Fatalf("/readyz failed contract check: %v", err)
		}
		cc := resp.Header.Get("Cache-Control")
		if !strings.Contains(cc, "no-cache") || !strings.Contains(cc, "no-store") {
			t.Fatalf("expected Cache-Control to disable caching, got %s", cc)
		}
	}
}

// 2. 负向测试：先确认输入确实是 200 HTML，再验证同一验收判定入口必须拒绝 200 HTML
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

	// 请求未注册的路径，触发旧版 SPA fallback 行为
	resp, err := ts.Client().Get(ts.URL + "/unregistered_legacy_probe")
	if err != nil {
		t.Fatalf("GET request failed: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	// 负向测试前置确认：输入必须确凿为 200 且为 HTML，不能接受任意错误码冒充
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("precondition failed: expected SPA fallback to return 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Fatalf("precondition failed: expected SPA fallback Content-Type to be text/html, got %s", ct)
	}
	if !strings.Contains(string(bodyBytes), "<html") && !strings.Contains(string(bodyBytes), "<!DOCTYPE") {
		t.Fatalf("precondition failed: expected HTML body content, got: %s", string(bodyBytes))
	}

	// 执行与冒烟脚本相同的统一健康判定入口：必须报错失败，绝不能误判为通过
	err = ValidateHealthProbeResponse(resp.StatusCode, resp.Header, bodyBytes)
	if err == nil {
		t.Fatalf("FAIL: ValidateHealthProbeResponse erroneously accepted 200 HTML SPA fallback as healthy!")
	}
}

// 3. 拆分测试 1：数据库不可用 (关闭或未初始化)
func TestHealthCheck_Readyz_StoreClosedAndNil(t *testing.T) {
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

	// 3.1 关闭数据库
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

	// 3.2 Store 为 nil 边界
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
}

// 4. 拆分测试 2：业务锁争抢场景，证明 Ping 不受业务大锁阻塞
func TestHealthCheck_Readyz_LockContentionDoesNotBlockProbe(t *testing.T) {
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

	// 后台持有 Store 内部的业务大锁 400ms，模拟大事务
	st.LockForTest()
	lockReleased := make(chan struct{})
	go func() {
		time.Sleep(400 * time.Millisecond)
		st.UnlockForTest()
		close(lockReleased)
	}()

	start := time.Now()
	resp, err := ts.Client().Get(ts.URL + "/readyz")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("GET /readyz failed: %v", err)
	}
	defer resp.Body.Close()

	// 修复前: /readyz 在 s.mu.Lock() 上硬阻塞，耗时 >= 400ms
	// 修复后: /readyz 解耦 s.mu，耗时应在 150ms 以内快速返回 200 OK (预留合理调度余量)
	if elapsed >= 300*time.Millisecond {
		t.Fatalf("probe was blocked by business lock for %v (expected completion well under 300ms)", elapsed)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK despite business lock, got %d", resp.StatusCode)
	}
	<-lockReleased
}

// 5. 拆分测试 3：使用可控同步点验证请求取消传播与资源收敛 (不使用固定 Sleep 猜测)
func TestHealthCheck_Readyz_CancellationPropagationWithSyncPoint(t *testing.T) {
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

	// 可控同步点：
	// probeStarted: 当底层 Ping 确实被调用并开始等待时关闭
	// cancelReceived: 当底层确实接收到 ctx.Done() 取消信号后关闭
	probeStarted := make(chan struct{})
	cancelReceived := make(chan struct{})

	st.SetBeforePingHookForTest(func(ctx context.Context) {
		close(probeStarted)
		select {
		case <-ctx.Done():
			close(cancelReceived)
		case <-time.After(3 * time.Second):
		}
	})
	defer st.SetBeforePingHookForTest(nil)

	reqCtx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, ts.URL+"/readyz", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		resp, reqErr := ts.Client().Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		errCh <- reqErr
	}()

	// 1. 等待可控同步点：100% 确认服务端已经开始探测并进入等待，无需 sleep 猜测！
	select {
	case <-probeStarted:
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for probe to enter sync point")
	}

	// 2. 触发取消
	cancel()

	// 3. 断言底层确实收到了取消信号
	select {
	case <-cancelReceived:
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for underlying context cancellation to be received")
	}

	// 4. 断言客户端请求返回取消错误
	reqErr := <-errCh
	if reqErr == nil || (!errors.Is(reqErr, context.Canceled) && !strings.Contains(reqErr.Error(), "canceled")) {
		t.Fatalf("expected context.Canceled error from client.Do, got: %v", reqErr)
	}

	// 5. 断言 handler 结束后服务资源正常，后续请求仍立即可用
	liveResp, liveErr := ts.Client().Get(ts.URL + "/livez")
	if liveErr != nil || liveResp.StatusCode != http.StatusOK {
		t.Fatalf("server failed to recover after cancelled probe: %v", liveErr)
	}
	_ = liveResp.Body.Close()
}

// 6. 验证探针不访问 Apple、不泄露敏感信息
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

	for _, path := range []string{"/livez", "/readyz"} {
		resp, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s failed: %v", path, err)
		}

		bodyBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		bodyStr := string(bodyBytes)

		// 6.1 断言外部上游调用严格为 0
		if calls := atomic.LoadInt64(&tracker.upstreamCalls); calls > 0 {
			t.Fatalf("probe %s triggered %d upstream calls! must be 0", path, calls)
		}

		// 6.2 断言响应内容不泄露任何系统口令、Token、目录物理路径或敏感配置
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

// 7. 验证 /api/auth/session 未认证时严格为 401，且不被健康检查削弱
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
