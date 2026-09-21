/**
 * [INPUT]: 依赖 testing, os, path/filepath, net/http, net/http/httptest, strings, encoding/json, icloud-hme/internal/account, icloud-hme/internal/hme, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 TestSelectAccountByTag, TestSelectAccountCandidatesQuotaPriority, TestSelectAccountCandidatesDual500Limit, TestQuickCreateStrictTagIsolation, TestQuickCreatePoolFirstAndFallback
 * [POS]: internal/server 的一键快速出号、别名池毫秒优先领用 (Pool-First)、配额感知号池智能优选、双维 500 熔断避障与严格业务标签隔离单元测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/store"
)

func TestSelectAccountByTag(t *testing.T) {
	accounts := []account.Summary{
		{
			ID:         "acc_tag_full",
			Status:     "active",
			HasCookies: true,
			AliasTotal: 500,
			Tags:       []string{"biz_a"},
		},
		{
			ID:         "acc_tag_busy",
			Status:     "active",
			HasCookies: true,
			AliasTotal: 120,
			Tags:       []string{"biz_a"},
		},
		{
			ID:         "acc_tag_idle",
			Status:     "active",
			HasCookies: true,
			AliasTotal: 10,
			Tags:       []string{"biz_a"},
		},
		{
			ID:         "acc_common_full",
			Status:     "active",
			HasCookies: true,
			AliasTotal: 500,
			Tags:       nil,
		},
		{
			ID:         "acc_common_idle",
			Status:     "active",
			HasCookies: true,
			AliasTotal: 5,
			Tags:       nil,
		},
		{
			ID:         "acc_common_busy",
			Status:     "active",
			HasCookies: true,
			AliasTotal: 80,
			Tags:       nil,
		},
		{
			ID:         "acc_inactive",
			Status:     "error",
			HasCookies: false,
			AliasTotal: 0,
			Tags:       []string{"biz_a"},
		},
	}

	// 1. 业务标签匹配：应跳过 500 上限的 acc_tag_full，并在 acc_tag_busy (120) 与 acc_tag_idle (10) 中按最低负载选中 acc_tag_idle
	selectedA := selectAccountByTag(accounts, "biz_a")
	if selectedA != "acc_tag_idle" {
		t.Fatalf("期望选中负载最低的专属账号 acc_tag_idle, 实际得到: %s", selectedA)
	}

	// 2. 回退到通用池：标签未匹配时，跳过 500 上限的 acc_common_full，在 acc_common_busy(80) 与 acc_common_idle(5) 中选中 acc_common_idle
	selectedFallback := selectAccountByTag(accounts, "biz_unmatched")
	if selectedFallback != "acc_common_idle" {
		t.Fatalf("期望回退选中负载最低的通用账号 acc_common_idle, 实际得到: %s", selectedFallback)
	}

	// 3. 所有账号皆满 500 时应熔断返回空
	allFull := []account.Summary{
		{
			ID:         "full_1",
			Status:     "active",
			HasCookies: true,
			AliasTotal: 500,
			Tags:       []string{"test"},
		},
		{
			ID:         "full_2",
			Status:     "active",
			HasCookies: true,
			AliasTotal: 501,
			Tags:       nil,
		},
	}
	if sel := selectAccountByTag(allFull, "test"); sel != "" {
		t.Fatalf("全满时应熔断返回空串, 实际得到: %s", sel)
	}
}

func TestSelectAccountCandidatesQuotaPriority(t *testing.T) {
	tempDir := filepath.Join(os.TempDir(), "test_select_candidates_quota")
	_ = os.RemoveAll(tempDir)
	defer os.RemoveAll(tempDir)

	st, err := store.NewStore(tempDir)
	if err != nil {
		t.Fatalf("创建 store 失败: %v", err)
	}
	defer st.Close()

	_ = st.SaveScheduleConfig(store.ScheduleConfig{
		AccountID:   "acc_tag_idle",
		HourlyQuota: 10,
	})
	_ = st.SaveScheduleConfig(store.ScheduleConfig{
		AccountID:   "acc_tag_busy",
		HourlyQuota: 10,
	})

	// acc_idle 水位低 (10) 但配额用完 (TryReserve 10)
	// acc_busy 水位高 (120) 但有配额 (剩余 10)
	allowed, _ := st.TryReserveQuota("acc_tag_idle", 10)
	if !allowed {
		t.Fatalf("预扣配额失败")
	}

	accounts := []account.Summary{
		{
			ID:         "acc_tag_idle",
			Status:     "active",
			HasCookies: true,
			AliasTotal: 10,
			Tags:       []string{"biz_a"},
		},
		{
			ID:         "acc_tag_busy",
			Status:     "active",
			HasCookies: true,
			AliasTotal: 120,
			Tags:       []string{"biz_a"},
		},
	}

	cands := selectAccountCandidates(accounts, "biz_a", st)
	if len(cands) != 2 {
		t.Fatalf("期望返回 2 个候选, 实际得到: %d", len(cands))
	}
	// 关键验证：有配额的 acc_tag_busy 必须排在前面，消除配额枯竭导致的撞墙死锁
	if cands[0] != "acc_tag_busy" {
		t.Fatalf("期望有配额的 acc_tag_busy 排在第一候选位, 实际是: %s", cands[0])
	}
	if cands[1] != "acc_tag_idle" {
		t.Fatalf("期望配额耗尽的 acc_tag_idle 追加在后作为备选, 实际是: %s", cands[1])
	}
}

func TestSelectAccountCandidatesDual500Limit(t *testing.T) {
	accounts := []account.Summary{
		{
			ID:          "acc_total_500",
			Status:      "active",
			HasCookies:  true,
			AliasTotal:  500,
			AliasActive: 10,
		},
		{
			ID:          "acc_active_500",
			Status:      "active",
			HasCookies:  true,
			AliasTotal:  450,
			AliasActive: 500,
		},
		{
			ID:          "acc_ok",
			Status:      "active",
			HasCookies:  true,
			AliasTotal:  490,
			AliasActive: 490,
		},
	}

	cands := selectAccountCandidates(accounts, "default", nil)
	if len(cands) != 1 || cands[0] != "acc_ok" {
		t.Fatalf("期望仅筛选出双维均未达 500 的 acc_ok, 实际得到: %v", cands)
	}
}

func TestQuickCreateStrictTagIsolation(t *testing.T) {
	// 账号仅属于 biz_finance，业务申请 biz_marketing
	accounts := []account.Summary{
		{
			ID:          "acc_finance",
			Status:      "active",
			HasCookies:  true,
			AliasTotal:  10,
			AliasActive: 10,
			Tags:        []string{"biz_finance"},
		},
	}

	cands := selectAccountCandidates(accounts, "biz_marketing", nil)
	if len(cands) != 0 {
		t.Fatalf("期望严格标签隔离下返回 0 候选，禁止借用其他业务账号，实际得到: %v", cands)
	}
}

func TestQuickCreatePoolFirstAndFallback(t *testing.T) {
	f := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_pool", Status: "active", HasCookies: true, AliasTotal: 10, AliasActive: 10},
		},
		aliases: []hme.Alias{
			{Email: "pool_alias_1@icloud.com", Active: true},
			{Email: "pool_alias_2@icloud.com", Active: true},
		},
	}
	f.created = &hme.CreateResult{
		Email:     "fresh_created@icloud.com",
		CreatedAt: "2026-09-21T00:00:00Z",
		Label:     "fallback",
	}

	cfg := Config{
		Debug:         false,
		AdminPassword: "admin-pass-2026-strong",
		APIKey:        "test-key",
	}
	s := newWithBackend(f, cfg)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	client := ts.Client()

	postAllocate := func(body string) (int, map[string]any) {
		req, _ := http.NewRequest("POST", ts.URL+"/api/allocate", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", "test-key")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var res struct {
			Success bool           `json:"success"`
			Code    string         `json:"code"`
			Data    map[string]any `json:"data"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&res)
		return resp.StatusCode, res.Data
	}

	// 1. 第一次出号：应命中别名池第一个预存别名 pool_alias_1
	code1, data1 := postAllocate(`{"tag":"default"}`)
	if code1 != http.StatusOK || data1["email"] != "pool_alias_1@icloud.com" || data1["source"] != "pool" {
		t.Fatalf("第一次出号期望命中池 pool_alias_1, 实际: code=%d data=%v", code1, data1)
	}

	// 2. 第二次出号：应命中别名池第二个预存别名 pool_alias_2
	code2, data2 := postAllocate(`{"tag":"default"}`)
	if code2 != http.StatusOK || data2["email"] != "pool_alias_2@icloud.com" || data2["source"] != "pool" {
		t.Fatalf("第二次出号期望命中池 pool_alias_2, 实际: code=%d data=%v", code2, data2)
	}

	// 3. 第三次出号 (mode="pool_only")：池已空，且禁止新建，应返回 503 POOL_EMPTY
	code3, _ := postAllocate(`{"tag":"default","mode":"pool_only"}`)
	if code3 != http.StatusServiceUnavailable {
		t.Fatalf("第三次出号 pool_only 期望 503, 实际: %d", code3)
	}

	// 4. 第四次出号 (默认 mode="pool")：池已空，自动降级调用 CreateAlias 现场新建
	code4, data4 := postAllocate(`{"tag":"default"}`)
	if code4 != http.StatusOK || data4["email"] != "fresh_created@icloud.com" || data4["source"] != "created" {
		t.Fatalf("第四次出号期望降级新建 fresh_created, 实际: code=%d data=%v", code4, data4)
	}

	// 5. 非法 mode 参数防护：应返回 400 VALIDATION_ERROR
	code5, _ := postAllocate(`{"mode":"invalid_mode"}`)
	if code5 != http.StatusBadRequest {
		t.Fatalf("非法 mode 期望返回 400, 实际: %d", code5)
	}

	// 6. 指定不存在 account_id：应立即返回 404 ACCOUNT_NOT_FOUND
	code6, _ := postAllocate(`{"account_id":"acc_not_exists"}`)
	if code6 != http.StatusNotFound {
		t.Fatalf("指定不存在 account_id 期望返回 404, 实际: %d", code6)
	}

	// 7. 畸形 JSON 请求体防护：应返回 400 VALIDATION_ERROR
	code7, _ := postAllocate(`{invalid_json`)
	if code7 != http.StatusBadRequest {
		t.Fatalf("畸形 JSON 期望返回 400, 实际: %d", code7)
	}
}

type multiAccountFakeBackend struct {
	fakeBackend
	accountAliases map[string][]hme.Alias
}

func (m *multiAccountFakeBackend) ListAliases(accID string) ([]hme.Alias, error) {
	if list, ok := m.accountAliases[accID]; ok {
		return list, nil
	}
	return m.fakeBackend.ListAliases(accID)
}

func TestQuickCreateRoundRobinInterleaving(t *testing.T) {
	f := &multiAccountFakeBackend{
		fakeBackend: fakeBackend{
			accounts: []account.Summary{
				{ID: "acc_alpha", Status: "active", HasCookies: true},
				{ID: "acc_beta", Status: "active", HasCookies: true},
			},
		},
		accountAliases: map[string][]hme.Alias{
			"acc_alpha": {
				{Email: "alpha_1@icloud.com", Active: true},
				{Email: "alpha_2@icloud.com", Active: true},
			},
			"acc_beta": {
				{Email: "beta_1@icloud.com", Active: true},
				{Email: "beta_2@icloud.com", Active: true},
			},
		},
	}

	cfg := Config{
		AdminPassword: "admin-pass-2026-strong",
		APIKey:        "test-key",
	}
	s := newWithBackend(f, cfg)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	client := ts.Client()
	postAllocate := func() (string, string) {
		req, _ := http.NewRequest("POST", ts.URL+"/api/allocate", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", "test-key")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var res struct {
			Data struct {
				Email     string `json:"email"`
				AccountID string `json:"account_id"`
			} `json:"data"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&res)
		return res.Data.AccountID, res.Data.Email
	}

	// 验证交织轮转：第1次出 alpha_1，第2次出 beta_1，第3次出 alpha_2，第4次出 beta_2
	acc1, em1 := postAllocate()
	if acc1 != "acc_alpha" || em1 != "alpha_1@icloud.com" {
		t.Fatalf("第1次出号期望 alpha_1@icloud.com, 实际: %s %s", acc1, em1)
	}

	acc2, em2 := postAllocate()
	if acc2 != "acc_beta" || em2 != "beta_1@icloud.com" {
		t.Fatalf("第2次出号期望轮转至 beta_1@icloud.com, 实际: %s %s", acc2, em2)
	}

	acc3, em3 := postAllocate()
	if acc3 != "acc_alpha" || em3 != "alpha_2@icloud.com" {
		t.Fatalf("第3次出号期望轮转回 alpha_2@icloud.com, 实际: %s %s", acc3, em3)
	}

	acc4, em4 := postAllocate()
	if acc4 != "acc_beta" || em4 != "beta_2@icloud.com" {
		t.Fatalf("第4次出号期望轮转至 beta_2@icloud.com, 实际: %s %s", acc4, em4)
	}
}


