/**
 * [INPUT]: 依赖 testing, context, time, errors, net/http, net/http/httptest, sync/atomic
 * [OUTPUT]: 对外提供 TestUP01_*, TestUP02_*, TestUP03_*, TestUP04_*, TestUP05_*, TestUP06_* 单元测试
 * [POS]: internal/hme 的 PR-05-1 上游可靠性契约测试套件 (UP01~UP06)
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

// UP01: RequestWithContext 超时截断测试 (验证 context.WithTimeout 快速中断)
func TestUP01_RequestWithContext_TimeoutTruncation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer server.Close()

	client, err := NewClient(nil, "icloud.com", "", false)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetServiceURL(server.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, reqErr := client.RequestWithContext(ctx, "GET", server.URL+"/test-timeout", nil, 5*time.Second, 1)
	elapsed := time.Since(start)
	t.Logf("UP01 request elapsed: %v, reqErr: %v", elapsed, reqErr)

	if reqErr == nil {
		t.Fatalf("UP01 failed: expected context deadline exceeded error, got nil")
	}
	if !errors.Is(reqErr, context.DeadlineExceeded) && !errors.Is(reqErr, context.Canceled) {
		t.Fatalf("UP01 failed: expected DeadlineExceeded or Canceled, got: %v", reqErr)
	}
	if elapsed > 400*time.Millisecond {
		t.Fatalf("UP01 failed: request took too long (%v), context timeout not truncated promptly", elapsed)
	}
}

// UP02: 429 Retry-After 退避预算测试 (验证按照头信息等待，并在 context 超时前正确退出)
func TestUP02_RateLimit_RetryAfter_Budget(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Retry-After", "5") // 建议等待 5 秒
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"Too Many Requests"}`))
	}))
	defer server.Close()

	client, err := NewClient(nil, "icloud.com", "", false)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetServiceURL(server.URL)

	// Context 预算仅 80ms，远小于 5s 退避
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, reqErr := client.RequestWithContext(ctx, "GET", server.URL+"/test-429", nil, 1*time.Second, 3)
	elapsed := time.Since(start)

	if reqErr == nil {
		t.Fatalf("UP02 failed: expected error, got nil")
	}
	if !errors.Is(reqErr, context.DeadlineExceeded) && !errors.Is(reqErr, context.Canceled) {
		t.Fatalf("UP02 failed: expected context cancellation, got: %v", reqErr)
	}
	if elapsed > 400*time.Millisecond {
		t.Fatalf("UP02 failed: retry did not abort within context budget (%v)", elapsed)
	}
}

// UP03: 写操作 maxAttempts=1 禁止盲目重试测试
func TestUP03_WriteOperations_NoBlindRetry(t *testing.T) {
	var reserveHits, deleteHits, deactHits, reactHits, updateHits int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hme/reserve":
			atomic.AddInt32(&reserveHits, 1)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"Internal Error"}`))
		case "/v2/hme/list":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"success":true,"result":{"hmeEmails":[]}}`))
		case "/v1/hme/delete":
			atomic.AddInt32(&deleteHits, 1)
			w.WriteHeader(http.StatusInternalServerError)
		case "/v1/hme/deactivate":
			atomic.AddInt32(&deactHits, 1)
			w.WriteHeader(http.StatusInternalServerError)
		case "/v1/hme/reactivate":
			atomic.AddInt32(&reactHits, 1)
			w.WriteHeader(http.StatusInternalServerError)
		case "/v1/hme/updateMetaData":
			atomic.AddInt32(&updateHits, 1)
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"success":true}`))
		}
	}))
	defer server.Close()

	client, err := NewClient(nil, "icloud.com", "", false)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetServiceURL(server.URL)

	ctx := context.Background()

	// 1. ReserveWithContext (写操作单次尝试)
	_, _ = client.ReserveWithContext(ctx, "write_test@icloud.com", "label")
	if h := atomic.LoadInt32(&reserveHits); h != 1 {
		t.Fatalf("UP03 Reserve: expected 1 attempt, got %d", h)
	}

	// 2. DeactivateHMEWithContext
	_, _ = client.DeactivateHMEWithContext(ctx, "anon_deact")
	if h := atomic.LoadInt32(&deactHits); h != 1 {
		t.Fatalf("UP03 Deactivate: expected 1 attempt, got %d", h)
	}

	// 3. ReactivateHMEWithContext
	_, _ = client.ReactivateHMEWithContext(ctx, "anon_react")
	if h := atomic.LoadInt32(&reactHits); h != 1 {
		t.Fatalf("UP03 Reactivate: expected 1 attempt, got %d", h)
	}

	// 4. UpdateMetaDataWithContext
	_ = client.UpdateMetaDataWithContext(ctx, "anon_update", "new_label", "new_note")
	if h := atomic.LoadInt32(&updateHits); h != 1 {
		t.Fatalf("UP03 UpdateMetaData: expected 1 attempt, got %d", h)
	}
}

// UP04: Reserve 失败后 ListAliasesWithContext 成功核对恢复测试
func TestUP04_Reserve_Failure_Reconciliation_Recovery(t *testing.T) {
	candidate := "reconciled_up04@icloud.com"
	var reserved atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hme/reserve":
			reserved.Store(true)
			// 模拟服务端收到并落库，但回写响应时网络故障
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
		case "/v2/hme/list":
			w.WriteHeader(http.StatusOK)
			if reserved.Load() {
				_, _ = fmt.Fprintf(w, `{
					"success": true,
					"result": {
						"hmeEmails": [
							{"hme": "%s", "anonymousId": "anon_up04", "label": "reconciled", "isActive": true}
						]
					}
				}`, candidate)
			} else {
				_, _ = w.Write([]byte(`{"success": true, "result": {"hmeEmails": []}}`))
			}
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	client, err := NewClient(nil, "icloud.com", "", false)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetServiceURL(server.URL)

	ctx := context.Background()
	resultAlias, reserveErr := client.ReserveWithContext(ctx, candidate, "test-up04")
	if reserveErr != nil {
		t.Fatalf("UP04 failed: expected reconciliation recovery, got error: %v", reserveErr)
	}
	if resultAlias != candidate {
		t.Fatalf("UP04 failed: expected alias %s, got: %s", candidate, resultAlias)
	}
}

// UP05: parseAliasList 畸形/模糊响应拒绝测试 (严格 result.hmeEmails)
func TestUP05_ParseAliasList_StrictValidation(t *testing.T) {
	cases := []struct {
		name      string
		payload   string
		shouldErr bool
	}{
		{"空内容", "", true},
		{"空白字符", "   \t\n  ", true},
		{"HTML 拦截页面", "<!DOCTYPE html><html><body>Error</body></html>", true},
		{"HTML 小写标签", "<html><head></head><body>502 Bad Gateway</body></html>", true},
		{"非法 JSON 语法", `{"success": true, "result": {`, true},
		{"Upstream success=false", `{"success": false, "error": {"errorMessage": "auth error"}}`, true},
		{"缺少 result 对象", `{"success": true}`, true},
		{"缺少 hmeEmails 数组", `{"success": true, "result": {"other": []}}`, true},
		{"hmeEmails 字段不是数组", `{"success": true, "result": {"hmeEmails": "not_an_array"}}`, true},
		{"合法空列表", `{"success": true, "result": {"hmeEmails": []}}`, false},
		{"合法单别名", `{"success": true, "result": {"hmeEmails": [{"hme": "test@icloud.com", "isActive": true}]}}`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			aliases, err := parseAliasList(tc.payload)
			if tc.shouldErr {
				if err == nil {
					t.Fatalf("UP05 %s: expected error, got nil with %d aliases", tc.name, len(aliases))
				}
				if !errors.Is(err, ErrInvalidResponseSchema) {
					t.Fatalf("UP05 %s: expected ErrInvalidResponseSchema, got: %v", tc.name, err)
				}
			} else {
				if err != nil {
					t.Fatalf("UP05 %s: unexpected error: %v", tc.name, err)
				}
			}
		})
	}
}

// UP06: 错误分类 code 稳定性测试 (ErrInvalidResponseSchema, ErrOutcomeUnknown, ErrAuthFailed, ErrRateLimited)
func TestUP06_ErrorClassification_Stability(t *testing.T) {
	// 1. ErrInvalidResponseSchema
	_, err := parseAliasList(`{"broken": true}`)
	if !errors.Is(err, ErrInvalidResponseSchema) {
		t.Fatalf("UP06: expected ErrInvalidResponseSchema, got: %v", err)
	}

	// 2. ErrAuthFailed (401 & 403)
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`Auth required`))
		}))
		c, _ := NewClient(nil, "icloud.com", "", false)
		c.SetServiceURL(server.URL)
		_, reqErr := c.RequestWithContext(context.Background(), "GET", server.URL+"/auth-test", nil, 1*time.Second, 1)
		server.Close()
		if !errors.Is(reqErr, ErrAuthFailed) {
			t.Fatalf("UP06 status %d: expected ErrAuthFailed, got: %v", status, reqErr)
		}
	}

	// 3. ErrRateLimited (429)
	{
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`Rate limited`))
		}))
		c, _ := NewClient(nil, "icloud.com", "", false)
		c.SetServiceURL(server.URL)
		_, reqErr := c.RequestWithContext(context.Background(), "GET", server.URL+"/rate-test", nil, 1*time.Second, 1)
		server.Close()
		if !errors.Is(reqErr, ErrRateLimited) {
			t.Fatalf("UP06: expected ErrRateLimited, got: %v", reqErr)
		}
	}

	// 4. ErrOutcomeUnknown (Reserve 发生网络错误且后续核对中候选不存在)
	{
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/hme/reserve":
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`Server crash`))
			case "/v2/hme/list":
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"success":true,"result":{"hmeEmails":[]}}`))
			default:
				w.WriteHeader(http.StatusOK)
			}
		}))
		defer server.Close()
		c, _ := NewClient(nil, "icloud.com", "", false)
		c.SetServiceURL(server.URL)
		_, reserveErr := c.ReserveWithContext(context.Background(), "unknown@icloud.com", "label")
		if !errors.Is(reserveErr, ErrOutcomeUnknown) {
			t.Fatalf("UP06: expected ErrOutcomeUnknown, got: %v", reserveErr)
		}
	}
}
