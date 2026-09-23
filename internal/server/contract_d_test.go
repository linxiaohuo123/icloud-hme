package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

// D01: 文档推荐 v2 示例通过真实 router/handler (allocate -> verification-request -> get result)
func TestD01_RecommendedV2FlowThroughRealRouter(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fb := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_d01", Status: "active", HasCookies: true, Tags: []string{"default"}},
		},
		mailboxBoundaryFunc: func(accountID, folder string) (string, uint32, uint32, error) {
			return "icloud", 100, 50, nil
		},
	}
	cfg := Config{AdminPassword: "admin-pass-strong-2026"}
	s := newWithBackendAndStore(fb, cfg, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// 插入真实账号
	_, _ = st.DB().Exec(`INSERT INTO accounts (id, name, real_email, status, tags, created_at, updated_at) 
		VALUES ('acc_d01', 'Account D01', 'd01@test.com', 'active', '["default"]', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z')`)

	// 存入可用库存
	_ = st.AddInventoryAlias("acc_d01", hme.Alias{Email: "v2_test@icloud.com", AnonymousID: "ano_v2", Active: true}, "replenish", true)

	// 创建一个普通外部 token (仅 allocate,verify 作用域)
	tokSecret := "sec_v2_token_client"
	_ = st.SaveToken(store.APIToken{ID: "tok_v2", Name: "client_v2", Token: tokSecret, Scopes: "allocate,verify"})

	// 步骤 1: POST /api/external/v2/allocate (带 Idempotency-Key)
	allocBody := `{"tag":"default","label":"AutoTask"}`
	reqAlloc, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", strings.NewReader(allocBody))
	reqAlloc.Header.Set("Authorization", "Bearer "+tokSecret)
	reqAlloc.Header.Set("Idempotency-Key", "task_v2_idemp_key_001")
	reqAlloc.Header.Set("Content-Type", "application/json")

	respAlloc, err := http.DefaultClient.Do(reqAlloc)
	if err != nil {
		t.Fatal(err)
	}
	defer respAlloc.Body.Close()

	if respAlloc.StatusCode != http.StatusOK {
		t.Fatalf("Step 1 allocate failed with status %d", respAlloc.StatusCode)
	}

	var allocData map[string]interface{}
	_ = json.NewDecoder(respAlloc.Body).Decode(&allocData)
	dAlloc, _ := allocData["data"].(map[string]interface{})
	if dAlloc == nil {
		t.Fatalf("unexpected allocate response structure: %v", allocData)
	}
	leaseID, _ := dAlloc["lease_id"].(string)
	allocationID, _ := dAlloc["allocation_id"].(string)
	aliasEmail, _ := dAlloc["email"].(string)
	allocatedAt, _ := dAlloc["allocated_at"].(string)
	if allocationID == "" || leaseID != allocationID || aliasEmail != "v2_test@icloud.com" || allocatedAt == "" {
		t.Fatalf("invalid allocation result: lease_id=%s, alloc_id=%s, email=%s, allocated_at=%s", leaseID, allocationID, aliasEmail, allocatedAt)
	}
	// 验证契约规范：不返回遗留的 tag 与 created_at
	if dAlloc["tag"] != nil {
		t.Fatalf("v2 contract violation: unexpected 'tag' field present in v2 allocate response")
	}
	if dAlloc["created_at"] != nil {
		t.Fatalf("v2 contract violation: unexpected 'created_at' field present in v2 allocate response")
	}

	// 步骤 2: POST /api/external/v2/verification-requests (使用 lease_id 建立意图与基线)
	vreqBody := fmt.Sprintf(`{"lease_id":"%s"}`, allocationID)
	reqVReq, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/verification-requests", strings.NewReader(vreqBody))
	reqVReq.Header.Set("Authorization", "Bearer "+tokSecret)
	reqVReq.Header.Set("Content-Type", "application/json")

	respVReq, err := http.DefaultClient.Do(reqVReq)
	if err != nil {
		t.Fatal(err)
	}
	defer respVReq.Body.Close()

	if respVReq.StatusCode != http.StatusOK {
		t.Fatalf("Step 2 verification-request failed with status %d", respVReq.StatusCode)
	}

	var vreqData map[string]interface{}
	_ = json.NewDecoder(respVReq.Body).Decode(&vreqData)
	dVReq, _ := vreqData["data"].(map[string]interface{})
	requestID, _ := dVReq["request_id"].(string)
	status, _ := dVReq["status"].(string)
	if requestID == "" || status != "ready" {
		t.Fatalf("invalid vreq result: request_id=%s, status=%s", requestID, status)
	}

	// 步骤 3: 模拟目标网站发信与邮件到达 (高于基线 UID=50)
	go func() {
		time.Sleep(50 * time.Millisecond)
		s.eventBus.PublishEvent(&mail.CachedOTP{
			AccountID:   "acc_d01",
			Email:       aliasEmail,
			Folder:      "INBOX",
			UIDValidity: 100,
			UID:         51,
			EventID:     "msg_v2_1001",
			OTP:         &mail.OTPResult{Code: "654321"},
			Date:        time.Now().Format(time.RFC3339),
		})
	}()

	// 步骤 4: GET /api/external/v2/verification-requests/:id (长轮询等待)
	reqPoll, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/external/v2/verification-requests/%s?timeout=5", ts.URL, requestID), nil)
	reqPoll.Header.Set("Authorization", "Bearer "+tokSecret)

	respPoll, err := http.DefaultClient.Do(reqPoll)
	if err != nil {
		t.Fatal(err)
	}
	defer respPoll.Body.Close()

	if respPoll.StatusCode != http.StatusOK {
		t.Fatalf("Step 4 polling failed with status %d", respPoll.StatusCode)
	}

	var pollData map[string]interface{}
	_ = json.NewDecoder(respPoll.Body).Decode(&pollData)
	dPoll, _ := pollData["data"].(map[string]interface{})
	if dPoll["status"] != "succeeded" || dPoll["code"] != "654321" {
		t.Fatalf("D01 FAILED: expected succeeded with code 654321, got: %v", dPoll)
	}
}

// D02: auto_delete=true 仍按声明拒绝 (400)，普通 token 调管理员停用仍拒绝 (403/401)
func TestD02_AutoDeleteRejectedAndAdminDeactivationPermission(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fb := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_d02", Status: "active", HasCookies: true, Tags: []string{"default"}},
		},
	}
	cfg := Config{AdminPassword: "admin-pass-strong-2026"}
	s := newWithBackendAndStore(fb, cfg, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	tokSecret := "sec_token_d02"
	_ = st.SaveToken(store.APIToken{ID: "tok_d02", Name: "client_d02", Token: tokSecret, Scopes: "allocate,verify"})

	// 1. GET /api/verify-code 传 auto_delete=true 必须返回 400 UNSUPPORTED_PARAMETER
	req1, _ := http.NewRequest("GET", ts.URL+"/api/verify-code?email=target@icloud.com&auto_delete=true", nil)
	req1.Header.Set("Authorization", "Bearer "+tokSecret)
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusBadRequest {
		t.Fatalf("D02 FAILED: verify-code auto_delete=true should return 400, got: %d", resp1.StatusCode)
	}

	// 2. GET /api/external/v1/verify-code 传 auto_delete=1 必须返回 400 UNSUPPORTED_PARAMETER
	req2, _ := http.NewRequest("GET", ts.URL+"/api/external/v1/verify-code?email=target@icloud.com&auto_delete=1", nil)
	req2.Header.Set("Authorization", "Bearer "+tokSecret)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("D02 FAILED: external v1 verify-code auto_delete=1 should return 400, got: %d", resp2.StatusCode)
	}

	// 3. 普通 allocate,verify 令牌尝试调用管理员停用接口 POST /api/aliases/:id/deactivate
	reqDeact, _ := http.NewRequest("POST", ts.URL+"/api/aliases/ano_target/deactivate", bytes.NewReader([]byte(`{"account_id":"acc_d02"}`)))
	reqDeact.Header.Set("Authorization", "Bearer "+tokSecret)
	reqDeact.Header.Set("Content-Type", "application/json")
	respDeact, err := http.DefaultClient.Do(reqDeact)
	if err != nil {
		t.Fatal(err)
	}
	respDeact.Body.Close()

	// 必须被拦截 (401 或 403 Forbidden)，普通外部令牌绝不可拥有管理员别名管理权限！
	if respDeact.StatusCode == http.StatusOK {
		t.Fatalf("D02 FAILED: regular token without admin scope was allowed to deactivate alias")
	}
}

// D03: v2 allocate 遇到冲突时返回 HTTP 409
func TestD03_V2Allocate_Conflict409Responses(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fb := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_d03", Status: "active", HasCookies: true, Tags: []string{"default"}},
		},
	}
	cfg := Config{AdminPassword: "admin-pass-strong-2026"}
	s := newWithBackendAndStore(fb, cfg, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	_, _ = st.DB().Exec(`INSERT INTO accounts (id, name, real_email, status, tags, created_at, updated_at) 
		VALUES ('acc_d03', 'Account D03', 'd03@test.com', 'active', '["default"]', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z')`)
	_ = st.AddInventoryAlias("acc_d03", hme.Alias{Email: "conflict_test@icloud.com", Active: true}, "replenish", true)

	tokSecret := "sec_v2_conflict"
	_ = st.SaveToken(store.APIToken{ID: "tok_d03", Name: "client_d03", Token: tokSecret, Scopes: "allocate,verify"})

	// 1. 相同 Idempotency-Key 不同参数 -> 409 IDEMPOTENCY_CONFLICT
	req1, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", strings.NewReader(`{"tag":"default"}`))
	req1.Header.Set("Authorization", "Bearer "+tokSecret)
	req1.Header.Set("Idempotency-Key", "key_conflict_01")
	req1.Header.Set("Content-Type", "application/json")
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first allocate failed with %d", resp1.StatusCode)
	}

	req2, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", strings.NewReader(`{"tag":"different_tag"}`))
	req2.Header.Set("Authorization", "Bearer "+tokSecret)
	req2.Header.Set("Idempotency-Key", "key_conflict_01")
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for different params with same key, got: %d", resp2.StatusCode)
	}
	var errData2 map[string]interface{}
	_ = json.NewDecoder(resp2.Body).Decode(&errData2)
	if errData2["code"] != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("expected code IDEMPOTENCY_CONFLICT, got: %v", errData2["code"])
	}

	// 2. 底层发生分配冲突 (ErrAllocationConflict) -> 409 ALLOCATION_CONFLICT
	_ = st.AddInventoryAlias("acc_d03", hme.Alias{Email: "conflict_test2@icloud.com", Active: true}, "replenish", true)
	// 制造冲突：直接向 alias_allocations 插入一条占用记录，但未标记 alias_inventory 为 allocated
	_, _ = st.DB().Exec(`INSERT INTO alias_allocations (allocation_id, alias_email, account_id, owner_kind, owner_id, business_tag, allocated_at, status) 
		VALUES ('existing_alloc', 'conflict_test2@icloud.com', 'acc_d03', 'token', 'other_tok', 'default', '2026-09-20T00:00:00Z', 'allocated')`)

	req3, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", strings.NewReader(`{"tag":"default"}`))
	req3.Header.Set("Authorization", "Bearer "+tokSecret)
	req3.Header.Set("Idempotency-Key", "key_conflict_02")
	req3.Header.Set("Content-Type", "application/json")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for allocation conflict, got: %d", resp3.StatusCode)
	}
	var errData3 map[string]interface{}
	_ = json.NewDecoder(resp3.Body).Decode(&errData3)
	if errData3["code"] != "ALLOCATION_CONFLICT" {
		t.Fatalf("expected code ALLOCATION_CONFLICT, got: %v", errData3["code"])
	}
}
