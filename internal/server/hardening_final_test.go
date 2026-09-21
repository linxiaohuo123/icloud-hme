/**
 * [INPUT]: 依赖 testing, net/http, net/http/httptest, sync, sync/atomic, time, context, strings, encoding/json, icloud-hme/internal/store, icloud-hme/internal/account, icloud-hme/internal/hme, icloud-hme/internal/mail
 * [OUTPUT]: 对外提供 PR-08 Final Hardening 终极验收单测套件 (同名 Token 隔离、v2 统一收拢、MailSync 真实超时与快速归零)
 * [POS]: internal/server 的安全收尾与领域正确性回归防线
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

// ============================================================================
// PR-08 Final Hardening §1: token_name 永久退出资源归属逻辑
// ============================================================================
func TestPR08_FinalHardening_TokenIdentityIsolation(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fb := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_1", Status: "active", HasCookies: true, Tags: []string{"default"}},
		},
	}
	cfg := Config{
		Debug:         false,
		AdminPassword: "admin-pass-2026-strong",
	}
	s := newWithBackendAndStore(fb, cfg, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// 1. 创建两个完全同名的 Token: Token A 与 Token B
	tokenA := store.APIToken{
		ID:     "tok_identity_a_111",
		Name:   "faka_shared_name_bot",
		Token:  "secret-token-aaa-111",
		Scopes: "allocate,verify",
	}
	tokenB := store.APIToken{
		ID:     "tok_identity_b_222",
		Name:   "faka_shared_name_bot",
		Token:  "secret-token-bbb-222",
		Scopes: "allocate,verify",
	}
	if err := st.SaveToken(tokenA); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveToken(tokenB); err != nil {
		t.Fatal(err)
	}

	// 准备可用库存
	targetEmail := "identity_iso@icloud.com"
	if err := st.AddInventoryAlias("acc_1", hme.Alias{Email: targetEmail, Active: true}, "replenish", true); err != nil {
		t.Fatal(err)
	}

	// 2. Token A 领取 alias
	allocReqBody, _ := json.Marshal(map[string]interface{}{
		"tag": "default",
	})
	httpReq, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", bytes.NewReader(allocReqBody))
	httpReq.Header.Set("Authorization", "Bearer secret-token-aaa-111")
	httpReq.Header.Set("Idempotency-Key", "idemp-token-a-key-1")
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Token A allocate expected 200, got %d", resp.StatusCode)
	}

	var allocResp struct {
		Success bool `json:"success"`
		Data    struct {
			Email   string `json:"email"`
			LeaseID string `json:"lease_id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&allocResp); err != nil {
		t.Fatal(err)
	}
	if allocResp.Data.Email != targetEmail {
		t.Fatalf("expected email %s, got %s", targetEmail, allocResp.Data.Email)
	}
	leaseID := allocResp.Data.LeaseID
	if leaseID == "" {
		t.Fatal("expected non-empty lease_id")
	}

	// 3. 核验底层 alias_allocations.owner_id 必须精确等于 Token A ID (tok_identity_a_111)
	ctx := context.Background()
	allocA, err := st.GetPrincipalAllocation(ctx, targetEmail, "token", "tok_identity_a_111")
	if err != nil {
		t.Fatalf("Token A allocation record not found under Token A ID: %v", err)
	}
	if allocA.OwnerID != "tok_identity_a_111" {
		t.Fatalf("expected owner_id == 'tok_identity_a_111', got: %s", allocA.OwnerID)
	}

	// Token B 不得拥有此别名归属
	_, err = st.GetPrincipalAllocation(ctx, targetEmail, "token", "tok_identity_b_222")
	if !errors.Is(err, store.ErrAllocationNotFound) {
		t.Fatalf("Token B must NOT own the allocation, expected ErrAllocationNotFound, got: %v", err)
	}

	// 按 Token Name 查询归属必须为未找到 (禁止通过 name 归属)
	_, err = st.GetPrincipalAllocation(ctx, targetEmail, "token", "faka_shared_name_bot")
	if !errors.Is(err, store.ErrAllocationNotFound) {
		t.Fatalf("Token name lookup must NOT own the allocation, got: %v", err)
	}

	// 核验 tokenDisplayName 写入了 lease_records.token_name 作为审计
	leases, total := st.ListLeases("", "default", "", 10, 0)
	if total == 0 || len(leases) == 0 {
		t.Fatal("expected lease record in store")
	}
	if leases[0].TokenName != "faka_shared_name_bot" {
		t.Fatalf("expected lease_records.token_name == 'faka_shared_name_bot', got: %s", leases[0].TokenName)
	}

	// 4. Token A 创建 verification request
	vreqBody, _ := json.Marshal(map[string]interface{}{
		"lease_id": leaseID,
	})
	vreqHttp, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/verification-requests", bytes.NewReader(vreqBody))
	vreqHttp.Header.Set("Authorization", "Bearer secret-token-aaa-111")
	vreqHttp.Header.Set("Content-Type", "application/json")

	vreqResp, err := http.DefaultClient.Do(vreqHttp)
	if err != nil {
		t.Fatal(err)
	}
	defer vreqResp.Body.Close()

	if vreqResp.StatusCode != http.StatusOK {
		t.Fatalf("Token A create verification-request expected 200, got %d", vreqResp.StatusCode)
	}

	var vreqOut struct {
		Success bool `json:"success"`
		Data    struct {
			RequestID string `json:"request_id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(vreqResp.Body).Decode(&vreqOut); err != nil {
		t.Fatal(err)
	}
	requestID := vreqOut.Data.RequestID
	if requestID == "" {
		t.Fatal("expected non-empty request_id")
	}

	// 5. Token B 试图读取 Token A 的 verification resource -> 必须返回 404 (禁止越权与探针泄露)
	getReqB, _ := http.NewRequest("GET", ts.URL+"/api/external/v2/verification-requests/"+requestID+"?timeout=0", nil)
	getReqB.Header.Set("Authorization", "Bearer secret-token-bbb-222")

	getRespB, err := http.DefaultClient.Do(getReqB)
	if err != nil {
		t.Fatal(err)
	}
	defer getRespB.Body.Close()

	if getRespB.StatusCode != http.StatusNotFound {
		t.Fatalf("Token B reading Token A's verification resource expected 404, got %d", getRespB.StatusCode)
	}

	// 6. Token B 试图通过 v1 接口获取 Token A 别名的验证码 -> 必须返回 404
	v1ReqB, _ := http.NewRequest("GET", ts.URL+"/api/external/v1/verify-code?email="+targetEmail+"&timeout=1", nil)
	v1ReqB.Header.Set("Authorization", "Bearer secret-token-bbb-222")

	v1RespB, err := http.DefaultClient.Do(v1ReqB)
	if err != nil {
		t.Fatal(err)
	}
	defer v1RespB.Body.Close()

	if v1RespB.StatusCode != http.StatusNotFound {
		t.Fatalf("Token B querying Token A's email verify-code expected 404, got %d", v1RespB.StatusCode)
	}
}

// ============================================================================
// PR-08 Final Hardening §2: external/v2/allocate 也统一进入 AliasAllocationService
// ============================================================================
func TestPR08_FinalHardening_AllFiveAllocateEndpointsUnified(t *testing.T) {
	st, _, ts := setupAllocTestServer(t)
	defer st.Close()
	defer ts.Close()

	// 准备外部受限令牌: 仅具备 allocate 作用域，且 allowed_tags 仅包含 default
	extToken := store.APIToken{
		ID:     "tok_ext_scope_test",
		Name:   "ext_bot",
		Token:  "secret-ext-token-123",
		Scopes: "allocate",
	}
	if err := st.SaveToken(extToken); err != nil {
		t.Fatal(err)
	}

	endpoints := []struct {
		url               string
		requireIdempotent bool
	}{
		{url: "/api/quick-create", requireIdempotent: false},
		{url: "/api/alias/lease", requireIdempotent: false},
		{url: "/api/allocate", requireIdempotent: false},
		{url: "/api/external/v1/allocate", requireIdempotent: false},
		{url: "/api/external/v2/allocate", requireIdempotent: true},
	}

	// 1. 外部令牌试图指定 account_id，所有 5 个端点统一返回 403 FORBIDDEN
	for _, ep := range endpoints {
		t.Run("ForbidAccountID_"+ep.url, func(t *testing.T) {
			body, _ := json.Marshal(map[string]interface{}{
				"account_id": "acc_1",
				"tag":        "default",
			})
			req, _ := http.NewRequest("POST", ts.URL+ep.url, bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer secret-ext-token-123")
			req.Header.Set("Content-Type", "application/json")
			if ep.requireIdempotent {
				req.Header.Set("Idempotency-Key", "idemp-account-forbidden")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("%s with account_id expected 403, got %d", ep.url, resp.StatusCode)
			}
		})
	}

	// 2. 外部令牌试图指定 mode=create，所有 5 个端点统一阻断并返回 503 (ALLOCATION_STATE_NOT_READY)
	for _, ep := range endpoints {
		t.Run("ForbidCreateMode_"+ep.url, func(t *testing.T) {
			body, _ := json.Marshal(map[string]interface{}{
				"mode": "create",
				"tag":  "default",
			})
			req, _ := http.NewRequest("POST", ts.URL+ep.url, bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer secret-ext-token-123")
			req.Header.Set("Content-Type", "application/json")
			if ep.requireIdempotent {
				req.Header.Set("Idempotency-Key", "idemp-create-forbidden")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("%s with mode=create expected 503, got %d", ep.url, resp.StatusCode)
			}
		})
	}

	// 3. 外部令牌未传 Idempotency-Key 访问 external/v2/allocate，必须返回 400
	t.Run("V2RequireIdempotencyKey", func(t *testing.T) {
		body, _ := json.Marshal(map[string]interface{}{
			"tag": "default",
		})
		req, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret-ext-token-123")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("v2 allocate without Idempotency-Key expected 400, got %d", resp.StatusCode)
		}
	})
}

// ============================================================================
// PR-08 Final Hardening §3: 修复 MailSyncWorker 的假 timeout 与快速停止
// ============================================================================
func TestPR08_FinalHardening_MailSyncWorkerRealTimeoutAndStop(t *testing.T) {
	const numAccounts = 100
	accounts := make([]account.Summary, numAccounts)
	for i := 0; i < numAccounts; i++ {
		accounts[i] = account.Summary{
			ID:             fmt.Sprintf("acc_block_%d", i),
			Status:         "active",
			HasCookies:     true,
			HasAppPassword: true,
		}
	}

	var activeFetches atomic.Int32
	var maxActiveFetches atomic.Int32

	fb := &fakeBackend{
		accounts: accounts,
		onListInboxContext: func(ctx context.Context, q InboxQuery) (InboxResult, error) {
			cur := activeFetches.Add(1)
			for {
				old := maxActiveFetches.Load()
				if cur <= old || maxActiveFetches.CompareAndSwap(old, cur) {
					break
				}
			}
			defer activeFetches.Add(-1)

			// 模拟慢调用：严格监听 ctx.Done() 取消；若无取消则长达 30 秒
			select {
			case <-ctx.Done():
				return InboxResult{}, ctx.Err()
			case <-time.After(30 * time.Second):
				return InboxResult{}, nil
			}
		},
	}

	eventBus := mail.NewEventBus(1 * time.Minute)
	// 为全部 100 个账号注册别名订阅
	for i := 0; i < numAccounts; i++ {
		alias := fmt.Sprintf("target_%d@icloud.com", i)
		subID, _ := eventBus.Subscribe(alias)
		defer eventBus.Unsubscribe(alias, subID)
	}

	worker := NewMailSyncWorker(fb, nil, eventBus, 1*time.Second)
	for i := 0; i < numAccounts; i++ {
		alias := fmt.Sprintf("target_%d@icloud.com", i)
		accID := fmt.Sprintf("acc_block_%d", i)
		worker.RegisterAliasAccount(alias, accID)
	}

	// 启动并触发 100 个阻塞账号的并发 syncOnce
	worker.Start()
	worker.Trigger()

	// 等待 150ms 使得有界并发 worker 派发拉取
	time.Sleep(150 * time.Millisecond)

	peakActive := maxActiveFetches.Load()
	currentActive := activeFetches.Load()

	// 1. 活跃网络调用并发数严格不超过 worker 并发上限 (<= 5)
	if peakActive > 5 {
		t.Fatalf("peak active fetches exceeded worker concurrency limit 5: got %d", peakActive)
	}
	if peakActive == 0 {
		t.Fatal("expected at least 1 active fetch dispatched")
	}
	if currentActive > 5 {
		t.Fatalf("current active fetches exceeded 5: got %d", currentActive)
	}

	// 2. 调用 Stop() 时，mock upstream 的活跃 goroutine 迅速归零，不发生泄露
	stopStart := time.Now()
	worker.Stop()
	stopDuration := time.Since(stopStart)

	finalActive := activeFetches.Load()
	if finalActive != 0 {
		t.Fatalf("active fetches must be exactly 0 after Stop(), got %d", finalActive)
	}
	if stopDuration > 2*time.Second {
		t.Fatalf("Stop() took too long (%v), expected prompt cancellation (<2s)", stopDuration)
	}
}
