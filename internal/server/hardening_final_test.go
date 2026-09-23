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
	"strings"
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

	_, _ = st.DB().Exec(`INSERT INTO accounts (id, name, real_email, status, tags, created_at, updated_at) 
		VALUES ('acc_1', 'Account 1', 'a1@test.com', 'active', '["default"]', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z')`)

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
	defer s.Close()
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

// ============================================================================
// P0-1: MailSyncWorker UIDNEXT Inclusive 与 Strict Verification INBOX 查询
// ============================================================================

func TestMailSyncWorker_UIDNextIsInclusive(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	targetEmail := "inclusive_uid@icloud.com"

	// 1. 创建基线 VerificationRequest: BaselineMailbox=INBOX, BaselineUIDValidity=10, BaselineUID=100
	now := time.Now().UTC()
	vreq := &store.VerificationRequest{
		RequestID:           "vreq_inclusive_1",
		PrincipalKind:       "token",
		PrincipalID:         "tok_inc",
		LeaseID:             "lease_inc",
		AliasEmail:          targetEmail,
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 10,
		BaselineUID:         100,
	}
	if err := st.CreateVerificationRequest(ctx, vreq); err != nil {
		t.Fatalf("CreateVerificationRequest 失败: %v", err)
	}

	// 2. 构造 backend，捕获 InboxQuery 并返回 UID=100 的邮件
	var capturedQuery InboxQuery
	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Status: "active", HasAppPassword: true}},
		onListInboxContext: func(c context.Context, q InboxQuery) (InboxResult, error) {
			capturedQuery = q
			return InboxResult{
				AccountID: q.AccountID,
				Alias:     q.Alias,
				Folder:    q.Folder,
				Count:     1,
				Messages: []mail.Message{
					{
						ID:          "100",
						AccountID:   q.AccountID,
						Folder:      "INBOX",
						UIDValidity: 10,
						UID:         100,
						To:          targetEmail,
						Subject:     "Your verification code is 123456",
						Preview:     "Code: 123456",
						Provider:    "imap",
					},
				},
				Method: "imap",
			}, nil
		},
	}

	eventBus := mail.NewEventBus(5 * time.Minute)
	worker := NewMailSyncWorker(fb, st, eventBus, 1*time.Second)

	// 3. 订阅该别名事件通道
	subID, ch := eventBus.SubscribeWithBoundary(targetEmail, "INBOX", 10, 100)
	defer eventBus.Unsubscribe(targetEmail, subID)

	// 4. 执行真实 fetchAndPublishBatch
	matched := worker.fetchAndPublishBatch(ctx, "acc_1", []string{targetEmail})
	if !matched {
		t.Fatal("fetchAndPublishBatch 应匹配成功")
	}

	// 5. 校验捕获的查询参数: Folder 必须为 INBOX，SinceUID 必须为 100 (不能 +1 变为 101)
	if capturedQuery.Folder != "INBOX" {
		t.Fatalf("Strict Verification 驱动查询 Folder 应为 'INBOX', 实际: %s", capturedQuery.Folder)
	}
	if capturedQuery.SinceUID != 100 {
		t.Fatalf("SinceUID 必须是包含基线的 100, 实际: %d (绝不能为 101)", capturedQuery.SinceUID)
	}

	// 6. 验证 EventBus 接收到 UID=100
	select {
	case ev := <-ch:
		if ev.UID != 100 || ev.OTP.Code != "123456" {
			t.Fatalf("EventBus 交付了非预期事件: %+v", ev)
		}
	default:
		t.Fatal("EventBus 未能交付 UID=100 的邮件事件")
	}
}

func TestMailSyncWorker_StrictVerificationUsesInbox(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	alias := "inbox_req@icloud.com"

	now := time.Now().UTC()
	vreq := &store.VerificationRequest{
		RequestID:           "vreq_inbox_only",
		PrincipalKind:       "token",
		PrincipalID:         "tok_inc",
		LeaseID:             "lease_inc",
		AliasEmail:          alias,
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 20,
		BaselineUID:         200,
	}
	_ = st.CreateVerificationRequest(ctx, vreq)

	var capturedQuery InboxQuery
	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Status: "active", HasAppPassword: true}},
		onListInboxContext: func(c context.Context, q InboxQuery) (InboxResult, error) {
			capturedQuery = q
			return InboxResult{}, nil
		},
	}

	worker := NewMailSyncWorker(fb, st, mail.NewEventBus(5*time.Minute), 1*time.Second)
	_ = worker.fetchAndPublishBatch(ctx, "acc_1", []string{alias})

	if capturedQuery.Folder != "INBOX" {
		t.Fatalf("Strict Verification 驱动查询必须显式指定 Folder: 'INBOX', 实际: '%s'", capturedQuery.Folder)
	}
	if capturedQuery.SinceUID != 200 {
		t.Fatalf("SinceUID 必须为 200, 实际: %d", capturedQuery.SinceUID)
	}
}

func TestMailSyncWorker_MultiAliasUsesInclusiveMinUID(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	aliasA := "alias_a@icloud.com"
	aliasB := "alias_b@icloud.com"

	now := time.Now().UTC()
	vreqA := &store.VerificationRequest{
		RequestID:           "vreq_multi_a",
		PrincipalKind:       "token",
		PrincipalID:         "tok_multi",
		LeaseID:             "lease_multi_a",
		AliasEmail:          aliasA,
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1,
		BaselineUID:         100, // baseline 100
	}
	vreqB := &store.VerificationRequest{
		RequestID:           "vreq_multi_b",
		PrincipalKind:       "token",
		PrincipalID:         "tok_multi",
		LeaseID:             "lease_multi_b",
		AliasEmail:          aliasB,
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1,
		BaselineUID:         150, // baseline 150
	}
	_ = st.CreateVerificationRequest(ctx, vreqA)
	_ = st.CreateVerificationRequest(ctx, vreqB)

	var capturedQuery InboxQuery
	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Status: "active", HasAppPassword: true}},
		onListInboxContext: func(c context.Context, q InboxQuery) (InboxResult, error) {
			capturedQuery = q
			return InboxResult{}, nil
		},
	}

	worker := NewMailSyncWorker(fb, st, mail.NewEventBus(5*time.Minute), 1*time.Second)
	_ = worker.fetchAndPublishBatch(ctx, "acc_1", []string{aliasA, aliasB})

	if capturedQuery.Folder != "INBOX" {
		t.Fatalf("多别名查询严格基线 Folder 必须为 'INBOX', 实际: '%s'", capturedQuery.Folder)
	}
	if capturedQuery.SinceUID != 100 {
		t.Fatalf("多别名查询下界必须是 min(100, 150) = 100, 实际: %d (严禁 101)", capturedQuery.SinceUID)
	}
}

func TestPublishedFingerprintIncludesUIDValidity(t *testing.T) {
	// DEDUPE-01: 同 account + folder + UID，当 UIDValidity 突变时，必须作为新事件发布
	worker := NewMailSyncWorker(nil, nil, mail.NewEventBus(5*time.Minute), 1*time.Second)

	// 1. validity=1, UID=100 -> 首次发布成功
	p1 := worker.markPublished("acc_1", "INBOX", 1, 100, "", "imap", "target@icloud.com")
	if !p1 {
		t.Fatal("validity=1, UID=100 首次发布应返回 true")
	}

	// 2. 相同 validity=1, UID=100 -> 重复忽略
	p2 := worker.markPublished("acc_1", "INBOX", 1, 100, "", "imap", "target@icloud.com")
	if p2 {
		t.Fatal("相同 validity=1, UID=100 重复发布应返回 false")
	}

	// 3. 代际变更 validity=2, UID=100 -> 必须视为新事件
	p3 := worker.markPublished("acc_1", "INBOX", 2, 100, "", "imap", "target@icloud.com")
	if !p3 {
		t.Fatal("代际变更 validity=2, UID=100 必须视为新事件发布")
	}
}

// P1-A: 系统水位中 available_aliases 取自 alias_inventory 权威库存
func TestP1A_StatsAvailableAliasesUsesAuthoritativeInventory(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.DB().Exec(`INSERT INTO accounts (id, name, real_email, status, tags, created_at, updated_at) 
		VALUES ('acc_1', 'Account 1', 'a1@test.com', 'active', '[]', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}

	// 添加 2 个 active & available 别名
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "auth1@icloud.com", Active: true}, "replenish", true)
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "auth2@icloud.com", Active: true}, "replenish", true)

	// 模拟 backend 报告 10 个 active 别名 (若用旧减法会得到 10 - 0 = 10)
	fb := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_1", Status: "active", HasCookies: true, AliasActive: 10},
		},
	}

	s, ts := newTestServerWithStore(fb, st)
	defer s.Close()
	defer ts.Close()

	cookie, _ := login(t, ts, "admin-pass-2026-strong")
	req := authedReq(t, ts, "GET", "/api/system/stats", "")
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})

	status, body, _ := do(t, req)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}

	var res struct {
		Success bool `json:"success"`
		Data    struct {
			AliasPool struct {
				TotalActiveAliases int `json:"total_active_aliases"`
				AvailableAliases   int `json:"available_aliases"`
			} `json:"alias_pool"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(body), &res)
	if res.Data.AliasPool.AvailableAliases != 2 {
		t.Fatalf("P1-A 失败: available_aliases 期望权威库存 2, 实际: %d", res.Data.AliasPool.AvailableAliases)
	}
}

// P1-B: listInboxHandler 必须正确传递 request context 到底层
func TestP1B_ListInboxContextCancellation(t *testing.T) {
	ctxCancelled := make(chan bool, 1)
	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_ctx", Status: "active", HasAppPassword: true}},
		onListInboxContext: func(ctx context.Context, q InboxQuery) (InboxResult, error) {
			select {
			case <-ctx.Done():
				ctxCancelled <- true
				return InboxResult{}, ctx.Err()
			case <-time.After(500 * time.Millisecond):
				ctxCancelled <- false
				return InboxResult{}, nil
			}
		},
	}

	_, ts := newTestServer(fb)
	defer ts.Close()

	cookie, _ := login(t, ts, "admin-pass-2026-strong")

	reqCtx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(reqCtx, "GET", ts.URL+"/api/inbox?account_id=acc_ctx", nil)
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel() // 主动中断客户端请求
	}()

	_, _ = http.DefaultClient.Do(req)

	select {
	case cancelled := <-ctxCancelled:
		if !cancelled {
			t.Fatal("P1-B 失败: request context 取消未正确传导至后端 ListInboxContext")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("P1-B 失败: 等待超时")
	}
}

// P1-C: 调度器补货持久化失败时向上返回错误
func TestP1C_ReplenishPersistenceFailurePropagatesError(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.NewStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}

	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_rep", Status: "active", HasCookies: true}},
		onCreateAlias: func(accountID, label string) (*hme.CreateResult, error) {
			return &hme.CreateResult{Email: "rep_err@icloud.com", Label: label, CreatedAt: "now"}, nil
		},
	}

	cfg := Config{
		DataDir:       dataDir,
		AdminPassword: "admin-pass-2026-strong",
	}
	srv := newWithBackendAndStore(fb, cfg, st)
	defer srv.Close()

	// 提前关闭 store，制造 AddInventoryAlias 数据库持久化失败
	_ = st.Close()

	// 触发调度器执行补货：RunAllNow 遍历所有账号并执行补货
	created, failed := srv.scheduler.RunAllNow(1)
	if created != 0 || failed != 1 {
		t.Fatalf("P1-C 失败: 持久化失败时补货期望 created=0, failed=1, 实际: created=%d, failed=%d", created, failed)
	}

	// 检查调度器日志中包含持久化失败记录
	logs := srv.scheduler.Logs()
	foundFail := false
	for _, l := range logs {
		if strings.Contains(l.Message, "补货入库持久化失败") {
			foundFail = true
			break
		}
	}
	if !foundFail {
		t.Fatalf("P1-C 失败: 调度器日志中未记录持久化失败错误, logs=%+v", logs)
	}
}



