/**
 * [INPUT]: 依赖 testing, net/http, net/http/httptest, strings, encoding/json, internal/store, internal/hme, internal/account
 * [OUTPUT]: 提供 LEGACY01~03, ALLOC01~04, IDEMP01~06 出号与幂等性全链路 Correctness Gate 单测
 * [POS]: internal/server 的出号架构单真相源与外部 v2 契约正确性验证套件
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/store"
)

func setupAllocTestServer(t *testing.T) (*store.Store, *fakeBackend, *httptest.Server) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	fb := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_1", Status: "active", HasCookies: true, Tags: []string{"default"}},
		},
		created: &hme.CreateResult{
			Email: "created_alias@icloud.com",
			Label: "created",
		},
	}
	cfg := Config{
		Debug:         false,
		AdminPassword: "admin-pass-2026-strong",
	}
	s := newWithBackendAndStore(fb, cfg, st)
	ts := httptest.NewServer(s.Handler())
	return st, fb, ts
}

// LEGACY01: POST /api/quick-create, /api/alias/lease, /api/allocate, /api/external/v1/allocate 均调用同一出号服务
func TestLEGACY01_AllLegacyEndpointsUnified(t *testing.T) {
	st, _, ts := setupAllocTestServer(t)
	defer st.Close()
	defer ts.Close()

	// 预置 4 个别名到库存
	for i := 1; i <= 4; i++ {
		email := fmt.Sprintf("unified_%d@icloud.com", i)
		_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: email, Active: true}, "replenish", true)
	}

	cookie, csrf := login(t, ts, "admin-pass-2026-strong")
	endpoints := []string{
		"/api/quick-create",
		"/api/alias/lease",
		"/api/allocate",
		"/api/external/v1/allocate",
	}

	for i, ep := range endpoints {
		req, _ := http.NewRequest("POST", ts.URL+ep, strings.NewReader(`{"tag":"default"}`))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
		req.Header.Set("X-CSRF-Token", csrf)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("LEGACY01 失败: 路径 %s 期望 200, 实际得到: %d", ep, resp.StatusCode)
		}
		var out struct {
			Success bool `json:"success"`
			Data    struct {
				Email  string `json:"email"`
				Source string `json:"source"`
			} `json:"data"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()

		expectedEmail := fmt.Sprintf("unified_%d@icloud.com", i+1)
		if out.Data.Email != expectedEmail || out.Data.Source != "pool" {
			t.Fatalf("LEGACY01 失败: 路径 %s 期望分配 %s (source=pool), 实际得到: %s (source=%s)", ep, expectedEmail, out.Data.Email, out.Data.Source)
		}
	}
}

// LEGACY02: 所有旧入口均操作 alias_inventory，不再写入旧独立状态
func TestLEGACY02_OperatesOnAliasInventory(t *testing.T) {
	st, _, ts := setupAllocTestServer(t)
	defer st.Close()
	defer ts.Close()

	email := "inv_target@icloud.com"
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: email, Active: true}, "replenish", true)

	cookie, csrf := login(t, ts, "admin-pass-2026-strong")
	req, _ := http.NewRequest("POST", ts.URL+"/api/quick-create", strings.NewReader(`{"tag":"default"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	req.Header.Set("X-CSRF-Token", csrf)

	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("出号请求失败: %v", err)
	}
	resp.Body.Close()

	// 验证底层 alias_inventory 表的状态变为 allocated
	inv, err := st.GetInventoryAlias(email)
	if err != nil {
		t.Fatalf("LEGACY02 失败: 查询库存记录失败: %v", err)
	}
	if inv.AllocationState != store.AllocationAllocated {
		t.Fatalf("LEGACY02 失败: alias_inventory 状态未更新为 allocated, 实际: %s", inv.AllocationState)
	}
}

// LEGACY03: 外部 token 在号池为空时访问旧入口均返回 503 POOL_EMPTY
func TestLEGACY03_ExternalTokenPoolEmpty503(t *testing.T) {
	st, _, ts := setupAllocTestServer(t)
	defer st.Close()
	defer ts.Close()

	_ = st.SaveToken(store.APIToken{
		ID:     "tok_ext",
		Name:   "ext_bot",
		Token:  "sec_ext",
		Scopes: "allocate,verify",
	})

	endpoints := []string{
		"/api/quick-create",
		"/api/alias/lease",
		"/api/allocate",
		"/api/external/v1/allocate",
	}

	for _, ep := range endpoints {
		req, _ := http.NewRequest("POST", ts.URL+ep, strings.NewReader(`{"tag":"default"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer sec_ext")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("LEGACY03 失败: 外部 token 号池空时访问 %s 期望 503, 实际: %d", ep, resp.StatusCode)
		}
	}
}

// ALLOC01: 外部 token 在号池充足时正常认领，更新 alias_allocations
func TestALLOC01_ExternalTokenSuccessClaim(t *testing.T) {
	st, _, ts := setupAllocTestServer(t)
	defer st.Close()
	defer ts.Close()

	_ = st.SaveToken(store.APIToken{
		ID:     "tok_client1",
		Name:   "client1",
		Token:  "sec_c1",
		Scopes: "allocate",
	})
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "avail@icloud.com", Active: true}, "replenish", true)

	req, _ := http.NewRequest("POST", ts.URL+"/api/allocate", strings.NewReader(`{"tag":"default"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sec_c1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("ALLOC01 失败: 期望 200, 实际: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 确认 alias_allocations 中记录了正确的 owner_id
	alloc, err := st.GetPrincipalAllocation(t.Context(), "avail@icloud.com", "token", "tok_client1")
	if err != nil || alloc == nil {
		t.Fatalf("ALLOC01 失败: alias_allocations 未正确记录分配关系: %v", err)
	}
}

// ALLOC02: 外部 token 试图在请求体中传 account_id 被 403 阻断
func TestALLOC02_ExternalTokenAccountIdForbidden(t *testing.T) {
	st, _, ts := setupAllocTestServer(t)
	defer st.Close()
	defer ts.Close()

	_ = st.SaveToken(store.APIToken{
		ID:     "tok_c2",
		Name:   "client2",
		Token:  "sec_c2",
		Scopes: "allocate",
	})
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "some@icloud.com", Active: true}, "replenish", true)

	req, _ := http.NewRequest("POST", ts.URL+"/api/allocate", strings.NewReader(`{"account_id":"acc_1","tag":"default"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sec_c2")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("ALLOC02 失败: 普通外部 token 指定 account_id 期望 403, 实际: %d", resp.StatusCode)
	}
}

// ALLOC03: 外部 token 在号池为空时禁止远程建号，返回 503 + Retry-After: 60
func TestALLOC03_ExternalTokenEmptyPoolNoRemoteCreate(t *testing.T) {
	st, _, ts := setupAllocTestServer(t)
	defer st.Close()
	defer ts.Close()

	_ = st.SaveToken(store.APIToken{
		ID:     "tok_c3",
		Name:   "client3",
		Token:  "sec_c3",
		Scopes: "allocate",
	})

	req, _ := http.NewRequest("POST", ts.URL+"/api/allocate", strings.NewReader(`{"mode":"create","tag":"default"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sec_c3")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// mode=create 对普通 token 会被禁止，号池空时返回 503 或 503/400
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("ALLOC03 失败: 外部 token 试图远程建号期望 503, 实际: %d", resp.StatusCode)
	}
}

// ALLOC04: 管理员在号池为空且 mode=create 时允许远程建号并入库
func TestALLOC04_AdminAllowRemoteCreateOnEmpty(t *testing.T) {
	st, _, ts := setupAllocTestServer(t)
	defer st.Close()
	defer ts.Close()

	cookie, csrf := login(t, ts, "admin-pass-2026-strong")
	req, _ := http.NewRequest("POST", ts.URL+"/api/allocate", strings.NewReader(`{"mode":"create","tag":"default"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	req.Header.Set("X-CSRF-Token", csrf)

	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("ALLOC04 失败: 管理员创建模式期望 200, 实际: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 验证新建别名已被持久化到 inventory
	inv, err := st.GetInventoryAlias("created_alias@icloud.com")
	if err != nil || inv == nil || inv.AllocationState != store.AllocationAllocated {
		t.Fatalf("ALLOC04 失败: 新建别名未正确持久化到 alias_inventory: %v", err)
	}
}

// IDEMP01: 外部 token POST /api/external/v2/allocate 缺少 Idempotency-Key 返回 400
func TestIDEMP01_MissingIdempotencyKey400(t *testing.T) {
	st, _, ts := setupAllocTestServer(t)
	defer st.Close()
	defer ts.Close()

	_ = st.SaveToken(store.APIToken{
		ID:     "tok_v2",
		Name:   "v2bot",
		Token:  "sec_v2",
		Scopes: "allocate",
	})

	req, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", strings.NewReader(`{"tag":"default"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sec_v2")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("IDEMP01 失败: 缺少 Idempotency-Key 期望 400, 实际: %d", resp.StatusCode)
	}
}

// IDEMP02: 相同 Idempotency-Key + 相同请求参数返回相同结果（幂等）
func TestIDEMP02_SameKeySameParamsReturnsSameResult(t *testing.T) {
	st, _, ts := setupAllocTestServer(t)
	defer st.Close()
	defer ts.Close()

	_ = st.SaveToken(store.APIToken{
		ID:     "tok_v2",
		Name:   "v2bot",
		Token:  "sec_v2",
		Scopes: "allocate",
	})
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "idemp@icloud.com", Active: true}, "replenish", true)

	body := `{"tag":"default"}`
	idempKey := "key_test_12345"

	// 第一次调用
	req1, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", strings.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Authorization", "Bearer sec_v2")
	req1.Header.Set("Idempotency-Key", idempKey)

	resp1, err := http.DefaultClient.Do(req1)
	if err != nil || resp1.StatusCode != http.StatusOK {
		t.Fatalf("第一次出号失败: %v, code=%d", err, resp1.StatusCode)
	}
	var out1 struct {
		Data struct {
			Email   string `json:"email"`
			LeaseID string `json:"lease_id"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp1.Body).Decode(&out1)
	resp1.Body.Close()

	// 第二次重复调用
	req2, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer sec_v2")
	req2.Header.Set("Idempotency-Key", idempKey)

	resp2, err := http.DefaultClient.Do(req2)
	if err != nil || resp2.StatusCode != http.StatusOK {
		t.Fatalf("第二次重入失败: %v, code=%d", err, resp2.StatusCode)
	}
	var out2 struct {
		Data struct {
			Email   string `json:"email"`
			LeaseID string `json:"lease_id"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&out2)
	resp2.Body.Close()

	if out1.Data.Email != out2.Data.Email || out1.Data.LeaseID != out2.Data.LeaseID {
		t.Fatalf("IDEMP02 失败: 幂等返回结果不一致: out1=%v, out2=%v", out1, out2)
	}
}

// IDEMP03: 相同 Idempotency-Key + 不同请求参数返回 409 IDEMPOTENCY_CONFLICT
func TestIDEMP03_SameKeyDifferentParamsConflict(t *testing.T) {
	st, _, ts := setupAllocTestServer(t)
	defer st.Close()
	defer ts.Close()

	_ = st.SaveToken(store.APIToken{
		ID:     "tok_v2",
		Name:   "v2bot",
		Token:  "sec_v2",
		Scopes: "allocate",
	})
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "idemp_diff@icloud.com", Active: true}, "replenish", true)

	idempKey := "key_conflict_123"

	// 第一次调用
	req1, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", strings.NewReader(`{"tag":"default"}`))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Authorization", "Bearer sec_v2")
	req1.Header.Set("Idempotency-Key", idempKey)

	resp1, err := http.DefaultClient.Do(req1)
	if err != nil || resp1.StatusCode != http.StatusOK {
		t.Fatalf("第一次出号失败: %v", err)
	}
	resp1.Body.Close()

	// 相同 key，不同参数
	req2, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", strings.NewReader(`{"tag":"different_tag"}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer sec_v2")
	req2.Header.Set("Idempotency-Key", idempKey)

	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("IDEMP03 失败: 参数冲突期望 409, 实际: %d", resp2.StatusCode)
	}
}

// IDEMP04: POST /api/external/v2/verification-requests 拒绝 query 传 email 绕过
func TestIDEMP04_RejectQueryEmailBypass(t *testing.T) {
	st, _, ts := setupAllocTestServer(t)
	defer st.Close()
	defer ts.Close()

	_ = st.SaveToken(store.APIToken{
		ID:     "tok_v2",
		Name:   "v2bot",
		Token:  "sec_v2",
		Scopes: "verify",
	})

	// 试图通过 query 传 email，但缺少 lease_id
	req, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/verification-requests?email=target@icloud.com", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sec_v2")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("IDEMP04 失败: query email 绕过期望 400, 实际: %d", resp.StatusCode)
	}
}

// IDEMP05: GET /api/external/v2/verification-requests/:id 仅返回归属于该 token 的请求
func TestIDEMP05_VerificationRequestOwnership(t *testing.T) {
	st, _, ts := setupAllocTestServer(t)
	defer st.Close()
	defer ts.Close()

	_ = st.SaveToken(store.APIToken{ID: "tok_owner", Name: "owner", Token: "sec_owner", Scopes: "verify"})
	_ = st.SaveToken(store.APIToken{ID: "tok_other", Name: "other", Token: "sec_other", Scopes: "verify"})

	// 创建属于 tok_owner 的 verification request
	vreq := &store.VerificationRequest{
		RequestID:     "vreq_123",
		PrincipalKind: "token",
		PrincipalID:   "tok_owner",
		LeaseID:       "lease_1",
		AliasEmail:    "target@icloud.com",
		Status:        "pending",
		CreatedAt:     "2026-01-01T00:00:00Z",
		ExpiresAt:     "2026-01-01T00:10:00Z",
	}
	_ = st.CreateVerificationRequest(t.Context(), vreq)

	// tok_other 试图查询
	req, _ := http.NewRequest("GET", ts.URL+"/api/external/v2/verification-requests/vreq_123", nil)
	req.Header.Set("Authorization", "Bearer sec_other")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("IDEMP05 失败: 未授权 token 查询他人取码请求期望 404/403, 实际: %d", resp.StatusCode)
	}
}

// IDEMP06: GET /api/external/v2/operations/:id 归属鉴权生效
func TestIDEMP06_OperationOwnership(t *testing.T) {
	st, _, ts := setupAllocTestServer(t)
	defer st.Close()
	defer ts.Close()

	_ = st.SaveToken(store.APIToken{ID: "tok_op_owner", Name: "op_owner", Token: "sec_op_owner"})
	_ = st.SaveToken(store.APIToken{ID: "tok_op_other", Name: "op_other", Token: "sec_op_other"})

	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "op_target@icloud.com", Active: true}, "replenish", true)

	// tok_op_owner 触发一次带幂等键的出号操作
	reqAlloc, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", strings.NewReader(`{"tag":"default"}`))
	reqAlloc.Header.Set("Content-Type", "application/json")
	reqAlloc.Header.Set("Authorization", "Bearer sec_op_owner")
	reqAlloc.Header.Set("Idempotency-Key", "op_idemp_key_1")

	respAlloc, err := http.DefaultClient.Do(reqAlloc)
	if err != nil || respAlloc.StatusCode != http.StatusOK {
		t.Fatalf("出号操作失败: %v", err)
	}
	var allocOut struct {
		Data struct {
			OperationID string `json:"operation_id"`
		} `json:"data"`
	}
	_ = json.NewDecoder(respAlloc.Body).Decode(&allocOut)
	respAlloc.Body.Close()

	opID := allocOut.Data.OperationID
	if opID == "" {
		t.Fatalf("未能取得 operation_id")
	}

	// tok_op_other 试图窥探该 operation
	reqOp, _ := http.NewRequest("GET", ts.URL+"/api/external/v2/operations/"+opID, nil)
	reqOp.Header.Set("Authorization", "Bearer sec_op_other")

	respOp, err := http.DefaultClient.Do(reqOp)
	if err != nil {
		t.Fatal(err)
	}
	respOp.Body.Close()
	if respOp.StatusCode != http.StatusNotFound && respOp.StatusCode != http.StatusForbidden {
		t.Fatalf("IDEMP06 失败: 跨 token 窥探 operation 期望 404/403, 实际: %d", respOp.StatusCode)
	}
}
