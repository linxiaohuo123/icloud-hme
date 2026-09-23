package server

import (
	"testing"

	"icloud-hme/internal/account"
)

// A04-1: 纯判定函数测试
func TestA04_PureProtectedAccountPolicy(t *testing.T) {
	cases := []struct {
		name      string
		accName   string
		tags      []string
		protected bool
	}{
		{"普通无标签账号", "Normal Account", []string{}, false},
		{"普通业务标签账号", "Bot Account", []string{"gpt", "claude"}, false},
		{"名称含大号", "私人大号", []string{}, true},
		{"名称含大号兼容前缀", "工作大号-01", []string{"business"}, true},
		{"标签含personal", "Acc 1", []string{"personal"}, true},
		{"标签含private", "Acc 2", []string{"private"}, true},
		{"标签含protected", "Acc 3", []string{"protected"}, true},
		{"标签大小写与空格", "Acc 4", []string{" Personal ", "GPT"}, true},
		{"混合标签保护优先", "Acc 5", []string{"gpt", "Protected"}, true},
		{"default标签但含保护", "Acc 6", []string{"default", "private"}, true},
		{"default标签但名称含大号", "我的大号", []string{"default"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := account.IsProtectedAccount(tc.accName, tc.tags)
			if got != tc.protected {
				t.Fatalf("expected protected=%v, got=%v for name=%q tags=%v", tc.protected, got, tc.accName, tc.tags)
			}
		})
	}
}

// A04-2: 候选账号选择器过滤受保护账号与隔离回退测试
func TestA04_CandidateSelectionProtectsAccountsAndDeniesFallback(t *testing.T) {
	accounts := []account.Summary{
		{ID: "acc_prot_name", Name: "私人大号", Status: "active", HasCookies: true, Tags: []string{}},
		{ID: "acc_prot_tag", Name: "Bot Tagged", Status: "active", HasCookies: true, Tags: []string{"gpt", "protected"}},
		{ID: "acc_prot_mixed", Name: "Mixed", Status: "active", HasCookies: true, Tags: []string{" Personal "}},
		{ID: "acc_normal_gpt", Name: "Normal GPT", Status: "active", HasCookies: true, Tags: []string{"gpt"}},
		{ID: "acc_public", Name: "Public Normal", Status: "active", HasCookies: true, Tags: []string{}},
	}

	// 1. 专属标签匹配 (tag="gpt")：只能选出 acc_normal_gpt，严禁选出 acc_prot_tag
	cands := selectAccountCandidates(accounts, "gpt", nil)
	if len(cands) != 1 || cands[0] != "acc_normal_gpt" {
		t.Fatalf("expected only ['acc_normal_gpt'], got: %v", cands)
	}

	// 2. 业务标签隔离红线：请求未打标的特定业务标签 (tag="exclusive_biz")，在库存池中绝不能回退至公共池造成跨业务盗领
	poolFallback := selectPoolAccounts(accounts, "exclusive_biz")
	if len(poolFallback) != 0 {
		t.Fatalf("A04 FAILED: selectPoolAccounts must NOT fall back to public accounts on unknown tag, got: %v", poolFallback)
	}

	// 且在仅有其他业务账号与保护账号时，selectAccountCandidates 绝不借用其他业务账号与保护账号
	otherBizAccounts := []account.Summary{
		{ID: "acc_prot_name", Name: "私人大号", Status: "active", HasCookies: true, Tags: []string{}},
		{ID: "acc_other_biz", Name: "Other Biz", Status: "active", HasCookies: true, Tags: []string{"other"}},
	}
	candsNoBorrow := selectAccountCandidates(otherBizAccounts, "exclusive_biz", nil)
	if len(candsNoBorrow) != 0 {
		t.Fatalf("A04 FAILED: selectAccountCandidates must NOT borrow other biz or protected accounts, got: %v", candsNoBorrow)
	}

	// 3. 公共池候选 (tag="")：只能选出 acc_public，严禁选出 acc_prot_name 或 acc_prot_mixed
	candsPublic := selectAccountCandidates(accounts, "", nil)
	if len(candsPublic) != 1 || candsPublic[0] != "acc_public" {
		t.Fatalf("expected only ['acc_public'], got: %v", candsPublic)
	}

	// 4. selectPoolAccounts 同样严格过滤受保护账号
	poolCands := selectPoolAccounts(accounts, "gpt")
	if len(poolCands) != 1 || poolCands[0] != "acc_normal_gpt" {
		t.Fatalf("selectPoolAccounts expected ['acc_normal_gpt'], got: %v", poolCands)
	}

	poolPublic := selectPoolAccounts(accounts, "")
	if len(poolPublic) != 1 || poolPublic[0] != "acc_public" {
		t.Fatalf("selectPoolAccounts expected ['acc_public'], got: %v", poolPublic)
	}
}
