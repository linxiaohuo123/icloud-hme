/**
 * [INPUT]: 依赖 net/http, net/http/httptest, testing, context, time, errors, sync/atomic
 * [OUTPUT]: 对外提供 TestPR05_* 单元测试
 * [POS]: internal/hme 的 PR-05 可靠上游调用与核对恢复测试套件
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package hme

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// U01: 实际超时遵循 context 并在到期时迅速返回，而非只挂起等静态参数。
func TestPR05_ContextCancellation_U01(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1 * time.Second)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer server.Close()

	client, err := NewClient(nil, "icloud.com", "", false)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetServiceURL(server.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, reqErr := client.RequestWithContext(ctx, "GET", server.URL+"/v2/hme/list", nil, 5*time.Second, 1)
	elapsed := time.Since(start)

	if reqErr == nil {
		t.Fatalf("expected context timeout error, got nil")
	}
	if !errors.Is(reqErr, context.DeadlineExceeded) && !errors.Is(reqErr, context.Canceled) {
		t.Fatalf("expected context.DeadlineExceeded, got: %v", reqErr)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("request took too long to abort (%v), did not obey context promptly", elapsed)
	}
}

// U02: 401/403 认证错误不被创建循环反复重试，直接返回 ErrAuthFailed。
func TestPR05_NoRetryOnAuthFailure_U02(t *testing.T) {
	var hitCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hitCount, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"success":false,"error":{"errorMessage":"Authentication required"}}`))
	}))
	defer server.Close()

	client, err := NewClient(nil, "icloud.com", "", false)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetServiceURL(server.URL)

	ctx := context.Background()
	_, createErr := client.CreateAliasWithContext(ctx, "test-label", 5)

	if createErr == nil {
		t.Fatalf("expected auth failure, got success")
	}
	if !errors.Is(createErr, ErrAuthFailed) {
		t.Fatalf("expected ErrAuthFailed, got: %v", createErr)
	}
	if hits := atomic.LoadInt32(&hitCount); hits != 1 {
		t.Fatalf("expected exactly 1 attempt on 401, but got %d attempts (retry amplification occurred)", hits)
	}
}

// U03: 429 错误被正确分类为 ErrRateLimited。
func TestPR05_RateLimitTypedError_U03(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`Too Many Requests`))
	}))
	defer server.Close()

	client, err := NewClient(nil, "icloud.com", "", false)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetServiceURL(server.URL)

	ctx := context.Background()
	_, reqErr := client.RequestWithContext(ctx, "GET", server.URL+"/v2/hme/list", nil, 1*time.Second, 1)

	if reqErr == nil {
		t.Fatalf("expected rate limit error, got nil")
	}
	if !errors.Is(reqErr, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got: %v", reqErr)
	}
}

// U04: Reserve 已上游成功但响应发生网络断开，通过列表核对成功恢复已保留别名，不重复建号。
func TestPR05_OutcomeUnknownReconciliation_U04(t *testing.T) {
	candidate := "reconciled_alias@icloud.com"
	var reservedInMock atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hme/reserve":
			// 远端记录成功，但突发连接重置/网络中断
			reservedInMock.Store(true)
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
		case "/v2/hme/list":
			w.WriteHeader(http.StatusOK)
			if reservedInMock.Load() {
				_, _ = fmt.Fprintf(w, `{
					"success": true,
					"result": {
						"hmeEmails": [
							{"hme": "%s", "anonymousId": "anon_rec_1", "label": "test", "isActive": true}
						]
					}
				}`, candidate)
			} else {
				_, _ = w.Write([]byte(`{"success": true, "result": {"hmeEmails": []}}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(nil, "icloud.com", "", false)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetServiceURL(server.URL)

	ctx := context.Background()
	// 调用 ReserveWithContext，虽然 /reserve 遭遇断连，但它应自动通过 List 找到已创建记录并成功交付
	res, reserveErr := client.ReserveWithContext(ctx, candidate, "test-label")
	if reserveErr != nil {
		t.Fatalf("expected successful reconciliation recovery, got error: %v", reserveErr)
	}
	if res != candidate {
		t.Fatalf("expected returned alias %s, got %s", candidate, res)
	}
}

// U06: 200+success:false、HTML 登录页、非法 JSON、结构漂移绝不能被当成「合法空库存」。
func TestPR05_SchemaValidation_U06(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		shouldErr bool
		wantCount int
	}{
		{
			name:      "HTML 登录拦截页面",
			body:      "<!DOCTYPE html><html><head><title>Sign In</title></head><body>Login required</body></html>",
			shouldErr: true,
		},
		{
			name:      "小写 html 标签",
			body:      "<html><body>error</body></html>",
			shouldErr: true,
		},
		{
			name:      "损坏的 JSON 语法",
			body:      `{"success": true, "result": {"hmeEmails": [`,
			shouldErr: true,
		},
		{
			name:      "HTTP 200 但 success=false",
			body:      `{"success": false, "error": {"errorMessage": "Session Expired"}}`,
			shouldErr: true,
		},
		{
			name:      "缺少别名数组",
			body:      `{"success": true, "result": {"somethingElse": 123}}`,
			shouldErr: true,
		},
		{
			name:      "空响应体",
			body:      "",
			shouldErr: true,
		},
		{
			name:      "合法空别名列表 (真正的空库存)",
			body:      `{"success": true, "result": {"hmeEmails": []}}`,
			shouldErr: false,
			wantCount: 0,
		},
		{
			name:      "合法别名列表 (正常解析)",
			body:      `{"success": true, "result": {"hmeEmails": [{"hme": "a@icloud.com", "isActive": true}]}}`,
			shouldErr: false,
			wantCount: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			aliases, err := parseAliasList(tc.body)
			if tc.shouldErr {
				if err == nil {
					t.Fatalf("case %q: expected error, got nil (returned %d aliases, falsely treated as empty/valid)", tc.name, len(aliases))
				}
				if !errors.Is(err, ErrInvalidResponseSchema) {
					t.Fatalf("case %q: expected ErrInvalidResponseSchema, got: %v", tc.name, err)
				}
			} else {
				if err != nil {
					t.Fatalf("case %q: unexpected error: %v", tc.name, err)
				}
				if len(aliases) != tc.wantCount {
					t.Fatalf("case %q: expected %d aliases, got %d", tc.name, tc.wantCount, len(aliases))
				}
			}
		})
	}
}

// U09: 凭据与敏感请求头脱敏，避免泄露上游密钥与 Cookie。
func TestPR05_SensitiveHeaderRedaction_U09(t *testing.T) {
	sensitive := []string{"cookie", "Cookie", "Set-Cookie", "authorization", "x-apple-webauth-token", "proxy-authorization"}
	for _, h := range sensitive {
		if !isSensitiveHeader(h) {
			t.Fatalf("expected header %q to be recognized as sensitive", h)
		}
	}

	nonSensitive := []string{"Content-Type", "Accept", "User-Agent", "Referer"}
	for _, h := range nonSensitive {
		if isSensitiveHeader(h) {
			t.Fatalf("header %q should not be marked as sensitive", h)
		}
	}

	secret := "secret-jwt-token-12345"
	redacted := redactSecret(secret)
	if redacted != "<redacted 22 bytes>" {
		t.Fatalf("unexpected redacted value: %q", redacted)
	}
}
