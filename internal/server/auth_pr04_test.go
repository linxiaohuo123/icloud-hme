/**
 * [INPUT]: 依赖 testing, net/http, net/http/httptest, encoding/json, time, icloud-hme/internal/account, icloud-hme/internal/hme, icloud-hme/internal/mail, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 PR-04 资源级主体认证、跨令牌隔离与 v2 外部 API 契约单元测试套件 (A01-A07)
 * [POS]: internal/server 的权限与多租户安全回归测试集
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

// A01: Token A 不能读取 Token B 的验证码，冷缓存与热缓存均返回 404
func TestPR04_A01_CrossTokenVerificationIsolation(t *testing.T) {
	tempDir := t.TempDir()
	st, err := store.NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	// 注册两个独立外部令牌 (仅有 allocate,verify 权限)
	tokA := store.APIToken{ID: store.NewAPITokenID(), Name: "TokenA", Token: "key_token_a_111", Scopes: store.DefaultExternalScopes}
	tokB := store.APIToken{ID: store.NewAPITokenID(), Name: "TokenB", Token: "key_token_b_222", Scopes: store.DefaultExternalScopes}
	_ = st.SaveToken(tokA)
	_ = st.SaveToken(tokB)

	// 录入可用别名并分配给 Token B
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "victim@icloud.com", Active: true}, "replenish", true)
	allocB, _, err := st.ClaimInventoryAlias(context.Background(), "token", tokB.ID, "allocate", "k_b", "h", "tag", nil)
	if err != nil || allocB.AliasEmail != "victim@icloud.com" {
		t.Fatalf("failed to allocate to Token B: %v", err)
	}

	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Name: "主号"}},
	}
	s, ts := newTestServerWithStore(fb, st)
	defer ts.Close()

	// 1. 冷缓存测试：Token A 试图直接查询属于 Token B 的邮箱验证码
	reqCold := authedReq(t, ts, "GET", "/api/verify-code?email=victim@icloud.com&timeout=1", "")
	reqCold.Header.Set("X-API-Key", tokA.Token)

	statusCold, bodyCold, _ := do(t, reqCold)
	if statusCold != http.StatusNotFound {
		t.Fatalf("cold cache: expected 404 NotFound for Token A, got %d: %s", statusCold, bodyCold)
	}

	// 2. 热缓存测试：模拟一封真实邮件到达 EventBus 内存缓存
	s.eventBus.Publish("victim@icloud.com", "acc_1", "您的验证码是 889900", "sender@example.com", "2026-09-21 12:00:00", &mail.OTPResult{Code: "889900"})

	// Token A 再次试图拉取热缓存中的验证码
	reqHot := authedReq(t, ts, "GET", "/api/verify-code?email=victim@icloud.com&timeout=1", "")
	reqHot.Header.Set("X-API-Key", tokA.Token)

	statusHot, bodyHot, _ := do(t, reqHot)
	if statusHot != http.StatusNotFound {
		t.Fatalf("hot cache: expected 404 NotFound for Token A, got %d: %s", statusHot, bodyHot)
	}

	// 3. 验证拥有者 Token B 可以正常读取
	reqOwner := authedReq(t, ts, "GET", "/api/verify-code?email=victim@icloud.com&timeout=1", "")
	reqOwner.Header.Set("X-API-Key", tokB.Token)

	statusOwner, bodyOwner, _ := do(t, reqOwner)
	if statusOwner != http.StatusOK {
		t.Fatalf("expected 200 OK for owner Token B, got %d: %s", statusOwner, bodyOwner)
	}
	var res struct {
		Success bool `json:"success"`
		Data    struct {
			Code string `json:"code"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(bodyOwner), &res)
	if res.Data.Code != "889900" {
		t.Fatalf("expected code 889900 for owner, got %s", res.Data.Code)
	}
}

// A02: 外部普通令牌禁止指定母号 account_id
func TestPR04_A02_OrdinaryTokenCannotSpecifyAccountID(t *testing.T) {
	tempDir := t.TempDir()
	st, err := store.NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	tok := store.APIToken{ID: store.NewAPITokenID(), Name: "OrdinaryToken", Token: "key_ordinary_123", Scopes: store.DefaultExternalScopes}
	_ = st.SaveToken(tok)

	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_victim", Name: "受害母号"}},
	}
	_, ts := newTestServerWithStore(fb, st)
	defer ts.Close()

	payload := `{"account_id":"acc_victim","tag":"default"}`
	req := authedReq(t, ts, "POST", "/api/external/v2/allocate", payload)
	req.Header.Set("X-API-Key", tok.Token)

	status, body, _ := do(t, req)
	if status != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for external token specifying account_id, got %d: %s", status, body)
	}
}

// A04: 令牌改名或重新创建同名令牌不继承旧数据
func TestPR04_A04_TokenRenameAndRecreateNoDataLeak(t *testing.T) {
	tempDir := t.TempDir()
	st, err := store.NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	tokOld := store.APIToken{ID: "tok_immutable_old_id", Name: "CustomerService", Token: "key_old_token", Scopes: store.DefaultExternalScopes}
	_ = st.SaveToken(tokOld)

	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "cs_mail@icloud.com", Active: true}, "replenish", true)
	_, _, _ = st.ClaimInventoryAlias(context.Background(), "token", tokOld.ID, "allocate", "k_cs", "h", "tag", nil)

	// 删除旧令牌
	_, _ = st.DeleteToken(tokOld.ID)

	// 新建同名新令牌 (将获得全新 ID: tok_immutable_new_id)
	tokNew := store.APIToken{ID: "tok_immutable_new_id", Name: "CustomerService", Token: "key_new_token", Scopes: store.DefaultExternalScopes}
	_ = st.SaveToken(tokNew)

	fb := &fakeBackend{}
	_, ts := newTestServerWithStore(fb, st)
	defer ts.Close()

	// 新令牌试图读取同名旧令牌的历史别名，必须返回 404，绝不越权继承！
	req := authedReq(t, ts, "GET", "/api/verify-code?email=cs_mail@icloud.com&timeout=1", "")
	req.Header.Set("X-API-Key", tokNew.Token)

	status, body, _ := do(t, req)
	if status != http.StatusNotFound {
		t.Fatalf("new token with same name must NOT inherit old resources, expected 404, got %d: %s", status, body)
	}
}

// A05: 长轮询等待期间令牌被删除/撤销，在完成交付前返回 401 REVOKED_TOKEN
func TestPR04_A05_TokenRevocationDuringLongPoll(t *testing.T) {
	tempDir := t.TempDir()
	st, err := store.NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	tok := store.APIToken{ID: store.NewAPITokenID(), Name: "EphemeralToken", Token: "key_ephemeral", Scopes: store.DefaultExternalScopes}
	_ = st.SaveToken(tok)

	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "ephemeral@icloud.com", Active: true}, "replenish", true)
	_, _, _ = st.ClaimInventoryAlias(context.Background(), "token", tok.ID, "allocate", "k_eph", "h", "tag", nil)

	fb := &fakeBackend{}
	s, ts := newTestServerWithStore(fb, st)
	defer ts.Close()

	doneCh := make(chan struct{})
	var status int
	var body string

	go func() {
		req := authedReq(t, ts, "GET", "/api/verify-code?email=ephemeral@icloud.com&timeout=5", "")
		req.Header.Set("X-API-Key", tok.Token)
		status, body, _ = do(t, req)
		close(doneCh)
	}()

	// 等待 100ms 确保长轮询已挂起订阅
	time.Sleep(100 * time.Millisecond)

	// 此时管理员在后台注销/删除了该令牌
	_, _ = st.DeleteToken(tok.ID)

	// 随后邮件到达
	s.eventBus.Publish("ephemeral@icloud.com", "acc_1", "您的动态码: 123456", "sender@example.com", "2026-09-21 12:00:00", &mail.OTPResult{Code: "123456"})

	select {
	case <-doneCh:
		// 校验返回 401 撤销状态，绝不泄露敏感验证码
		if status != http.StatusUnauthorized {
			t.Fatalf("expected 401 Unauthorized for revoked token, got %d: %s", status, body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("long poll timeout")
	}
}

// A07: v2 端点全生命周期契约测试 (allocate -> verification-request -> get operation)
func TestPR04_A07_V2LifecycleContract(t *testing.T) {
	tempDir := t.TempDir()
	st, err := store.NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	tok := store.APIToken{ID: store.NewAPITokenID(), Name: "V2Client", Token: "key_v2_client", Scopes: store.DefaultExternalScopes}
	_ = st.SaveToken(tok)

	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "v2_flow@icloud.com", Active: true}, "replenish", true)

	fb := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_1", Status: "active", HasCookies: true, Tags: []string{"sales"}},
		},
	}
	_, ts := newTestServerWithStore(fb, st)
	defer ts.Close()

	// 1. POST /api/external/v2/allocate
	reqAlloc := authedReq(t, ts, "POST", "/api/external/v2/allocate", `{"tag":"sales"}`)
	reqAlloc.Header.Set("X-API-Key", tok.Token)
	reqAlloc.Header.Set("Idempotency-Key", "idemp_v2_flow_1")

	status1, body1, _ := do(t, reqAlloc)
	if status1 != http.StatusOK {
		t.Fatalf("v2 allocate failed: status=%d, body=%s", status1, body1)
	}

	var resAlloc struct {
		Success bool `json:"success"`
		Data    struct {
			LeaseID string `json:"lease_id"`
			Email   string `json:"email"`
			Source  string `json:"source"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(body1), &resAlloc)
	if resAlloc.Data.Email != "v2_flow@icloud.com" || resAlloc.Data.LeaseID == "" {
		t.Fatalf("unexpected allocate response: %+v", resAlloc.Data)
	}

	// 2. POST /api/external/v2/verification-requests
	vReqBody := fmt.Sprintf(`{"lease_id":"%s"}`, resAlloc.Data.LeaseID)
	reqVReq := authedReq(t, ts, "POST", "/api/external/v2/verification-requests", vReqBody)
	reqVReq.Header.Set("X-API-Key", tok.Token)

	status2, body2, _ := do(t, reqVReq)
	if status2 != http.StatusOK {
		t.Fatalf("v2 verification request creation failed: status=%d, body=%s", status2, body2)
	}
	var resVReq struct {
		Success bool `json:"success"`
		Data    struct {
			RequestID string `json:"request_id"`
			Status    string `json:"status"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(body2), &resVReq)
	if resVReq.Data.RequestID == "" || resVReq.Data.Status != "ready" {
		t.Fatalf("unexpected verification request response: %+v", resVReq.Data)
	}
}
