/**
 * [INPUT]: 依赖 testing, net/http, net/http/httptest, bytes, encoding/json, icloud-hme/internal/store, icloud-hme/internal/account, icloud-hme/internal/hme
 * [OUTPUT]: 对外提供 TestPromoteAliasesToPoolEndpoint 与 TestSystemStatsThreeTierWatermark 测试套件
 * [POS]: internal/server 的存量别名受控激活与号池三级水位端到端单元测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"icloud-hme/internal/account"
	"icloud-hme/internal/store"
)

func TestPromoteAliasesToPoolEndpoint(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 1. 初始化账号与存量别名数据
	_, _ = st.DB().Exec(`INSERT INTO accounts (id, name, real_email, status, tags, created_at, updated_at) 
		VALUES ('acc_norm', '业务小号', 'norm@test.com', 'active', '["default"]', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z')`)
	_, _ = st.DB().Exec(`INSERT INTO accounts (id, name, real_email, status, tags, created_at, updated_at) 
		VALUES ('acc_prot', '私人大号', 'big@test.com', 'active', '["personal"]', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z')`)

	// 存量 unknown 别名
	_, _ = st.DB().Exec(`INSERT INTO alias_inventory (email, account_id, provider_alias_id, remote_state, allocation_state, source_type, last_verified_at) 
		VALUES ('stock1@test.com', 'acc_norm', '', 'unknown', 'unknown', 'legacy_unknown', '')`)
	_, _ = st.DB().Exec(`INSERT INTO alias_inventory (email, account_id, provider_alias_id, remote_state, allocation_state, source_type, last_verified_at) 
		VALUES ('stock2@test.com', 'acc_norm', '', 'unknown', 'unknown', 'legacy_unknown', '')`)
	_, _ = st.DB().Exec(`INSERT INTO alias_inventory (email, account_id, provider_alias_id, remote_state, allocation_state, source_type, last_verified_at) 
		VALUES ('prot1@test.com', 'acc_prot', '', 'unknown', 'unknown', 'legacy_unknown', '')`)

	fb := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_norm", Name: "业务小号", Status: "active", HasCookies: true, Tags: []string{"default"}, AliasActive: 100},
			{ID: "acc_prot", Name: "私人大号", Status: "active", HasCookies: true, Tags: []string{"personal"}, AliasActive: 50},
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

	// 2. 检查未激活前的看板水位
	sess, csrf := login(t, ts, "admin-pass-2026-strong")
	reqStats := authedReq(t, ts, "GET", "/api/system/stats", "")
	reqStats.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	status, body, _ := do(t, reqStats)
	if status != http.StatusOK {
		t.Fatalf("stats expected 200, got %d: %s", status, body)
	}

	var statsRes struct {
		Data struct {
			AliasPool struct {
				TotalActiveAliases  int `json:"total_active_aliases"`
				AvailableAliases    int `json:"available_aliases"`
				DormantAliases      int `json:"dormant_aliases"`
				AppleQuotaRemaining int `json:"apple_quota_remaining"`
			} `json:"alias_pool"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(body), &statsRes)
	if statsRes.Data.AliasPool.AvailableAliases != 0 {
		t.Fatalf("expected available_aliases=0 before promotion, got %d", statsRes.Data.AliasPool.AvailableAliases)
	}
	if statsRes.Data.AliasPool.DormantAliases != 2 {
		t.Fatalf("expected dormant_aliases=2 before promotion, got %d", statsRes.Data.AliasPool.DormantAliases)
	}
	// 750 - 100 = 650 (大号被排除，仅计算小号剩余配额)
	if statsRes.Data.AliasPool.AppleQuotaRemaining != 650 {
		t.Fatalf("expected apple_quota_remaining=650, got %d", statsRes.Data.AliasPool.AppleQuotaRemaining)
	}

	// 3. 尝试激活受保护大号，必须失败
	protBody := `{"account_id": "acc_prot"}`
	reqProt := authedReq(t, ts, "POST", "/api/aliases/promote-to-pool", protBody)
	reqProt.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	reqProt.Header.Set("X-CSRF-Token", csrf)
	statusProt, _, _ := do(t, reqProt)
	if statusProt != http.StatusBadRequest {
		t.Fatalf("expected 400 when promoting protected account, got %d", statusProt)
	}

	// 3.1 尝试发送损坏的 JSON，必须返回 400 INVALID_JSON
	reqBadJSON := authedReq(t, ts, "POST", "/api/aliases/promote-to-pool", "{invalid-json")
	reqBadJSON.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	reqBadJSON.Header.Set("X-CSRF-Token", csrf)
	statusBadJSON, _, _ := do(t, reqBadJSON)
	if statusBadJSON != http.StatusBadRequest {
		t.Fatalf("expected 400 when sending bad JSON, got %d", statusBadJSON)
	}

	// 4. 正常激活存量别名
	reqPromote := authedReq(t, ts, "POST", "/api/aliases/promote-to-pool", "{}")
	reqPromote.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	reqPromote.Header.Set("X-CSRF-Token", csrf)
	statusPromote, bodyPromote, _ := do(t, reqPromote)
	if statusPromote != http.StatusOK {
		t.Fatalf("promote expected 200, got %d: %s", statusPromote, bodyPromote)
	}

	var promRes struct {
		Success bool `json:"success"`
		Data    struct {
			PromotedCount    int `json:"promoted_count"`
			AvailableAliases int `json:"available_aliases"`
			DormantAliases   int `json:"dormant_aliases"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(bodyPromote), &promRes)
	if promRes.Data.PromotedCount != 2 {
		t.Fatalf("expected promoted_count=2, got %d", promRes.Data.PromotedCount)
	}
	if promRes.Data.AvailableAliases != 2 {
		t.Fatalf("expected available_aliases=2, got %d", promRes.Data.AvailableAliases)
	}
	if promRes.Data.DormantAliases != 0 {
		t.Fatalf("expected dormant_aliases=0, got %d", promRes.Data.DormantAliases)
	}

	// 5. 注册外部 Token 并立即请求出号，验证出号顺利成功且零延迟走号池
	createdTok, err := st.CreateToken("registration_bot", "allocate,read_mail", "")
	if err != nil {
		t.Fatal(err)
	}

	allocReq, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", bytes.NewBufferString(`{"mode":"pool"}`))
	allocReq.Header.Set("Authorization", "Bearer "+createdTok.Token)
	allocReq.Header.Set("Idempotency-Key", "idemp_test_post_promote_1")
	allocReq.Header.Set("Content-Type", "application/json")
	allocResp, err := http.DefaultClient.Do(allocReq)
	if err != nil {
		t.Fatal(err)
	}
	defer allocResp.Body.Close()

	if allocResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after promote, got %d", allocResp.StatusCode)
	}

	var allocData struct {
		Success bool `json:"success"`
		Data    struct {
			Email string `json:"email"`
		} `json:"data"`
	}
	_ = json.NewDecoder(allocResp.Body).Decode(&allocData)
	if allocData.Data.Email != "stock1@test.com" && allocData.Data.Email != "stock2@test.com" {
		t.Fatalf("unexpected allocated email: %s", allocData.Data.Email)
	}
}
