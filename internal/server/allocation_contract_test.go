/**
 * [INPUT]: 依赖 testing, net/http, net/http/httptest, strings, encoding/json, internal/store, internal/hme, internal/auth
 * [OUTPUT]: 提供 F04(指纹碰撞与mode区分)、F05(池空同键重试契约)、F06(统一operation持久化与查询)回归测试套件
 * [POS]: internal/server 的分配幂等与 API 契约验收测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/store"
)

// TestContract_F04_FingerprintCollisionRejected 验证字符串拼接碰撞被消除
// 请求A: tag="default&account_id=&label=x", label="y"
// 请求B: tag="default", label="x&account_id=&label=y"
// 旧实现两者拼接均为 "tag=default&account_id=&label=x&account_id=&label=y"，导致不同参数未被识别为冲突。
func TestContract_F04_FingerprintCollisionRejected(t *testing.T) {
	st, fb, ts := setupAllocTestServer(t)
	defer st.Close()
	defer ts.Close()

	// 配置 acc_1 同时包含 default 和 拼接注入 tag，确保请求A与请求B均可正常路由
	collisionTag := "default&account_id=&label=x"
	fb.accounts[0].Tags = []string{"default", collisionTag}
	_, _ = st.DB().Exec(`UPDATE accounts SET tags = '["default", "default&account_id=&label=x"]' WHERE id = 'acc_1'`)

	_ = st.SaveToken(store.APIToken{
		ID:     "tok_f04",
		Name:   "f04_bot",
		Token:  "sec_f04",
		Scopes: "allocate",
	})
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "f04_1@icloud.com", Active: true}, "replenish", true)
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "f04_2@icloud.com", Active: true}, "replenish", true)

	idempKey := "key_f04_collision"

	// 第一次调用: 请求A
	reqA, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", strings.NewReader(`{"tag":"default&account_id=&label=x","label":"y"}`))
	reqA.Header.Set("Content-Type", "application/json")
	reqA.Header.Set("Authorization", "Bearer sec_f04")
	reqA.Header.Set("Idempotency-Key", idempKey)

	respA, err := http.DefaultClient.Do(reqA)
	if err != nil || respA.StatusCode != http.StatusOK {
		t.Fatalf("请求A失败: err=%v, code=%d", err, respA.StatusCode)
	}
	respA.Body.Close()

	// 第二次调用: 相同 key, 请求B (不同参数)
	reqB, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", strings.NewReader(`{"tag":"default","label":"x&account_id=&label=y"}`))
	reqB.Header.Set("Content-Type", "application/json")
	reqB.Header.Set("Authorization", "Bearer sec_f04")
	reqB.Header.Set("Idempotency-Key", idempKey)

	respB, err := http.DefaultClient.Do(reqB)
	if err != nil {
		t.Fatalf("请求B失败: %v", err)
	}
	b, _ := io.ReadAll(respB.Body)
	respB.Body.Close()

	if respB.StatusCode != http.StatusConflict {
		t.Fatalf("F04 失败: 请求A与请求B参数不同但拼接碰撞，期望 409 Conflict, 实际得到: %d, body: %s", respB.StatusCode, string(b))
	}
}

// TestContract_F04_ModeMismatchCausesConflict 验证不同 mode 必须触发 409 冲突
func TestContract_F04_ModeMismatchCausesConflict(t *testing.T) {
	st, _, ts := setupAllocTestServer(t)
	defer st.Close()
	defer ts.Close()

	_ = st.SaveToken(store.APIToken{
		ID:     "tok_f04_mode",
		Name:   "f04_mode_bot",
		Token:  "sec_f04_mode",
		Scopes: "allocate",
	})
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "f04_mode_1@icloud.com", Active: true}, "replenish", true)

	idempKey := "key_f04_mode"

	// 第一次调用: mode="pool"
	req1, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", strings.NewReader(`{"tag":"default","mode":"pool"}`))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Authorization", "Bearer sec_f04_mode")
	req1.Header.Set("Idempotency-Key", idempKey)

	resp1, err := http.DefaultClient.Do(req1)
	if err != nil || resp1.StatusCode != http.StatusOK {
		t.Fatalf("第一次请求失败: err=%v, code=%d", err, resp1.StatusCode)
	}
	resp1.Body.Close()

	// 第二次调用: 相同 key, mode="pool_only"
	req2, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", strings.NewReader(`{"tag":"default","mode":"pool_only"}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer sec_f04_mode")
	req2.Header.Set("Idempotency-Key", idempKey)

	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("第二次请求失败: %v", err)
	}
	resp2.Body.Close()

	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("F04 失败: 相同 key 使用不同 mode 期望 409 Conflict, 实际得到: %d", resp2.StatusCode)
	}
}

// TestContract_F05_EmptyPoolSameKeyRetrySucceedsAfterReplenish 验证池空同键重试契约
// 初始池空返回 503 + Retry-After: 60；补货后相同幂等键再次请求应当成功发放并完成分配
func TestContract_F05_EmptyPoolSameKeyRetrySucceedsAfterReplenish(t *testing.T) {
	st, _, ts := setupAllocTestServer(t)
	defer st.Close()
	defer ts.Close()

	_ = st.SaveToken(store.APIToken{
		ID:     "tok_f05",
		Name:   "f05_bot",
		Token:  "sec_f05",
		Scopes: "allocate",
	})

	idempKey := "key_f05_retry"

	// 1. 池空时第一次请求
	req1, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", strings.NewReader(`{"tag":"default"}`))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Authorization", "Bearer sec_f05")
	req1.Header.Set("Idempotency-Key", idempKey)

	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatalf("第一次请求网络失败: %v", err)
	}
	retryAfter := resp1.Header.Get("Retry-After")
	if resp1.StatusCode != http.StatusServiceUnavailable || retryAfter != "60" {
		t.Fatalf("池空期望 503 且 Retry-After=60, 实际 code=%d, retryAfter=%s", resp1.StatusCode, retryAfter)
	}
	resp1.Body.Close()

	// 2. 补货: 新增 1 个可用库存
	replenishEmail := "f05_replenished@icloud.com"
	if err := st.AddInventoryAlias("acc_1", hme.Alias{Email: replenishEmail, Active: true}, "replenish", true); err != nil {
		t.Fatalf("补货失败: %v", err)
	}

	// 3. 相同主体持相同幂等键再次重试
	req2, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", strings.NewReader(`{"tag":"default"}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer sec_f05")
	req2.Header.Set("Idempotency-Key", idempKey)

	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("重试请求网络失败: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("F05 失败: 池空补货后同键重试期望 200 OK, 实际仍返回: %d", resp2.StatusCode)
	}

	var out struct {
		Success bool `json:"success"`
		Data    struct {
			Email       string `json:"email"`
			OperationID string `json:"operation_id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&out); err != nil {
		t.Fatalf("解析重试响应失败: %v", err)
	}
	if out.Data.Email != replenishEmail {
		t.Fatalf("F05 失败: 重试分配别名不符合预期: expected=%s, got=%s", replenishEmail, out.Data.Email)
	}
}

// TestContract_F06_AdminNoKeyOperationIsQueryable 验证管理员全局APIKey无键分配生成的 operation_id 真实可查，杜绝幽灵 ID
func TestContract_F06_AdminNoKeyOperationIsQueryable(t *testing.T) {
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
	apiKey := "admin-global-api-key-2026"
	cfg := Config{
		Debug:         false,
		AdminPassword: "admin-pass-2026-strong",
		APIKey:        apiKey,
	}
	s := newWithBackendAndStore(fb, cfg, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "admin_nokey@icloud.com", Active: true}, "replenish", true)

	// 管理员使用 APIKey 无键调用 v2 allocate
	req, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", strings.NewReader(`{"tag":"default"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("管理员调用分配接口失败: err=%v, code=%d", err, resp.StatusCode)
	}
	var out struct {
		Success bool `json:"success"`
		Data    struct {
			OperationID string `json:"operation_id"`
			Email       string `json:"email"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()

	opID := out.Data.OperationID
	if opID == "" {
		t.Fatalf("未能取得 operation_id")
	}

	// 随后查询该 operation_id
	qReq, _ := http.NewRequest("GET", ts.URL+"/api/external/v2/operations/"+opID, nil)
	qReq.Header.Set("Authorization", "Bearer "+apiKey)

	qResp, err := http.DefaultClient.Do(qReq)
	if err != nil {
		t.Fatalf("查询 operation 失败: %v", err)
	}
	defer qResp.Body.Close()

	if qResp.StatusCode != http.StatusOK {
		t.Fatalf("F06 失败: 管理员无键分配产生的 operation_id %s 查询返回 %d (幽灵ID未持久化)", opID, qResp.StatusCode)
	}

	var opOut struct {
		Success bool `json:"success"`
		Data    struct {
			OperationID string `json:"operation_id"`
			State       string `json:"state"`
		} `json:"data"`
	}
	_ = json.NewDecoder(qResp.Body).Decode(&opOut)
	if opOut.Data.OperationID != opID || opOut.Data.State != "succeeded" {
		t.Fatalf("F06 失败: 查询操作数据不符合预期: %+v", opOut)
	}
}

// TestContract_F06_PendingResponseUnifiedFormat 验证 pending 响应统一为 {success: true, data: ...}
func TestContract_F06_PendingResponseUnifiedFormat(t *testing.T) {
	st, _, ts := setupAllocTestServer(t)
	defer st.Close()
	defer ts.Close()

	_ = st.SaveToken(store.APIToken{
		ID:     "tok_f06_pending",
		Name:   "f06_pending_bot",
		Token:  "sec_f06_pending",
		Scopes: "allocate",
	})

	idempKey := "key_pending_sample"
	// 预先向 operations 插入一条 pending 操作
	_, _ = st.DB().Exec(`
		INSERT INTO operations (operation_id, principal_kind, principal_id, operation_kind, idempotency_key, request_hash, state, created_at, updated_at)
		VALUES ('op_pending_1', 'token', 'tok_f06_pending', 'v2_allocate', ?, 'tag=default&account_id=&label=', 'pending', '2026-09-24T00:00:00Z', '2026-09-24T00:00:00Z')
	`, idempKey)

	req, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", strings.NewReader(`{"tag":"default"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sec_f06_pending")
	req.Header.Set("Idempotency-Key", idempKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求网络错误: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("期望 202 Accepted, 实际得到: %d", resp.StatusCode)
	}

	var rawMap map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&rawMap)

	if rawMap["success"] != true {
		t.Fatalf("F06 失败: pending 响应未遵循统一契约包含 success: true: %+v", rawMap)
	}
	data, ok := rawMap["data"].(map[string]any)
	if !ok || data["status"] != "pending" || data["operation_id"] != "op_pending_1" {
		t.Fatalf("F06 失败: pending 响应 data 格式不合规: %+v", rawMap)
	}
}

// TestContract_50ConcurrentSameKeyProducesSingleAllocation 验证 50 个并发同键请求只产生一份分配
func TestContract_50ConcurrentSameKeyProducesSingleAllocation(t *testing.T) {
	st, _, ts := setupAllocTestServer(t)
	defer st.Close()
	defer ts.Close()

	_ = st.SaveToken(store.APIToken{
		ID:     "tok_concurrent",
		Name:   "concurrent_bot",
		Token:  "sec_concurrent",
		Scopes: "allocate",
	})

	// 放入 10 个可用别名，确保并发时即使有多余库存也绝不多发
	for i := 1; i <= 10; i++ {
		_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: strings.ToLower(strings.TrimSpace("pool_concurrent_" + string(rune('a'+i)) + "@icloud.com")), Active: true}, "replenish", true)
	}

	idempKey := "key_concurrent_50_allocations"
	const concurrency = 50

	type result struct {
		statusCode int
		email      string
		leaseID    string
		err        error
	}
	results := make([]result, concurrency)

	var wg sync.WaitGroup
	startCh := make(chan struct{})

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-startCh

			req, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", strings.NewReader(`{"tag":"default"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer sec_concurrent")
			req.Header.Set("Idempotency-Key", idempKey)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results[idx] = result{err: err}
				return
			}
			defer resp.Body.Close()

			var out struct {
				Success bool `json:"success"`
				Data    struct {
					Email   string `json:"email"`
					LeaseID string `json:"lease_id"`
				} `json:"data"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&out)
			results[idx] = result{
				statusCode: resp.StatusCode,
				email:      out.Data.Email,
				leaseID:    out.Data.LeaseID,
			}
		}(i)
	}

	close(startCh)
	wg.Wait()

	firstEmail := ""
	firstLeaseID := ""
	successCount := 0

	for i, res := range results {
		if res.err != nil {
			t.Fatalf("goroutine %d 请求异常: %v", i, res.err)
		}
		// 允许 200 OK，或者若有由于极高并发处于 pending 的允许 202
		if res.statusCode == http.StatusOK {
			successCount++
			if firstEmail == "" {
				firstEmail = res.email
				firstLeaseID = res.leaseID
			} else {
				if res.email != firstEmail || res.leaseID != firstLeaseID {
					t.Fatalf("并发同键幂等性破坏: 得到不同分配 email1=%s, email2=%s", firstEmail, res.email)
				}
			}
		}
	}

	if successCount == 0 {
		t.Fatalf("50 个并发请求未产生成功的分配")
	}

	// 验证底层数据库只有 1 条分配记录
	var allocCount int
	_ = st.DB().QueryRow(`SELECT COUNT(1) FROM alias_allocations WHERE owner_id = 'tok_concurrent'`).Scan(&allocCount)
	if allocCount != 1 {
		t.Fatalf("契约验收失败: 50 并发同键请求产生了 %d 条分配凭据，期望严格为 1", allocCount)
	}
}

// TestContract_CrossPrincipalCannotAccessOperationOrAllocation 验证两个不同主体不能互读 operation/allocation
func TestContract_CrossPrincipalCannotAccessOperationOrAllocation(t *testing.T) {
	st, _, ts := setupAllocTestServer(t)
	defer st.Close()
	defer ts.Close()

	_ = st.SaveToken(store.APIToken{ID: "tok_user_a", Token: "sec_a", Scopes: "allocate"})
	_ = st.SaveToken(store.APIToken{ID: "tok_user_b", Token: "sec_b", Scopes: "allocate"})

	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "iso_target@icloud.com", Active: true}, "replenish", true)

	// 主体 A 创建一次分配
	reqA, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", strings.NewReader(`{"tag":"default"}`))
	reqA.Header.Set("Content-Type", "application/json")
	reqA.Header.Set("Authorization", "Bearer sec_a")
	reqA.Header.Set("Idempotency-Key", "key_principal_iso")

	respA, err := http.DefaultClient.Do(reqA)
	if err != nil || respA.StatusCode != http.StatusOK {
		t.Fatalf("主体 A 出号失败: %v", err)
	}
	var outA struct {
		Data struct {
			OperationID string `json:"operation_id"`
		} `json:"data"`
	}
	_ = json.NewDecoder(respA.Body).Decode(&outA)
	respA.Body.Close()

	opID := outA.Data.OperationID

	// 主体 B 试图读取主体 A 的 operation
	reqB, _ := http.NewRequest("GET", ts.URL+"/api/external/v2/operations/"+opID, nil)
	reqB.Header.Set("Authorization", "Bearer sec_b")

	respB, err := http.DefaultClient.Do(reqB)
	if err != nil {
		t.Fatalf("主体 B 查询网络失败: %v", err)
	}
	defer respB.Body.Close()

	if respB.StatusCode != http.StatusNotFound && respB.StatusCode != http.StatusForbidden {
		t.Fatalf("主体隔离破坏: 主体 B 窥探主体 A 的 operation 期望 404/403, 实际: %d", respB.StatusCode)
	}
}

// TestContract_LegacyFingerprintCompatibilityReplay 验证旧版本指纹记录无缝向下兼容回放
func TestContract_LegacyFingerprintCompatibilityReplay(t *testing.T) {
	st, _, ts := setupAllocTestServer(t)
	defer st.Close()
	defer ts.Close()

	_ = st.SaveToken(store.APIToken{ID: "tok_legacy_compat", Token: "sec_legacy_compat", Scopes: "allocate"})

	idempKey := "key_legacy_replay"
	legacyHash := "tag=default&account_id=&label="
	email := "legacy_replayed@icloud.com"
	allocID := "alloc_legacy_001"
	opID := "op_legacy_001"
	now := "2026-09-20T00:00:00Z"

	// 插入历史分配及旧指纹 operation
	_, _ = st.DB().Exec(`
		INSERT INTO alias_inventory (email, account_id, remote_state, allocation_state, origin, active, created_at, updated_at)
		VALUES (?, 'acc_1', 'active', 'allocated', 'replenish', 1, ?, ?)
	`, email, now, now)

	_, _ = st.DB().Exec(`
		INSERT INTO alias_allocations (allocation_id, alias_email, account_id, owner_kind, owner_id, business_tag, allocated_at, status)
		VALUES (?, ?, 'acc_1', 'token', 'tok_legacy_compat', 'default', ?, 'allocated')
	`, allocID, email, now)

	_, _ = st.DB().Exec(`
		INSERT INTO operations (operation_id, principal_kind, principal_id, operation_kind, idempotency_key, request_hash, state, candidate_email, result_ref, created_at, updated_at)
		VALUES (?, 'token', 'tok_legacy_compat', 'v2_allocate', ?, ?, 'succeeded', ?, ?, ?, ?)
	`, opID, idempKey, legacyHash, email, allocID, now, now)

	// 新客户端升级后发起请求 (新代码会自动产生 v2 指纹但携带 legacy 回退)
	req, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", strings.NewReader(`{"tag":"default"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sec_legacy_compat")
	req.Header.Set("Idempotency-Key", idempKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求网络异常: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("旧指纹向后兼容失败: 期望 200 回放, 实际得到: %d", resp.StatusCode)
	}

	var out struct {
		Success bool `json:"success"`
		Data    struct {
			Email   string `json:"email"`
			LeaseID string `json:"lease_id"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Data.Email != email || out.Data.LeaseID != allocID {
		t.Fatalf("旧指纹回放结果不匹配: expected=(%s, %s), got=(%s, %s)", email, allocID, out.Data.Email, out.Data.LeaseID)
	}
}
