/**
 * [INPUT]: 依赖 testing, os, path/filepath, icloud-hme/internal/account, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 TestSelectAccountByTag, TestSelectAccountCandidatesQuotaPriority, TestSelectAccountCandidatesDual500Limit, TestQuickCreateStrictTagIsolation
 * [POS]: internal/server 的一键快速出号、配额感知号池智能优选、双维 500 熔断避障与严格业务标签隔离单元测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"os"
	"path/filepath"
	"testing"

	"icloud-hme/internal/account"
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
