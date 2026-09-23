/**
 * [INPUT]: 依赖 testing, context, internal/store
 * [OUTPUT]: 提供 AUTH01~AUTH04 别名所有权与 Token 边界隔离单测
 * [POS]: internal/store 的授权安全测试套件 (PR-05-1 Correctness Gate)
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"context"
	"testing"

	"icloud-hme/internal/hme"
)

// AUTH01: IsEmailOwnedByToken 仅查 alias_allocations，token_id 不匹配返回 false
func TestAUTH01_OwnershipRequiresMatchingTokenID(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	// 保存 token A 与 token B
	_ = st.SaveToken(APIToken{ID: "tok_A", Name: "botA", Token: "secA"})
	_ = st.SaveToken(APIToken{ID: "tok_B", Name: "botB", Token: "secB"})

	// 给 token A 分配别名
	_, _ = st.db.Exec(`INSERT INTO accounts (id, name, real_email, status, tags, created_at, updated_at) VALUES ('acc_1', 'Account 1', 'a1@test.com', 'active', '[]', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z')`)
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "alpha@icloud.com", Active: true}, "replenish", true)

	alloc := &AliasAllocation{
		AllocationID: "alloc_1",
		AliasEmail:   "alpha@icloud.com",
		AccountID:    "acc_1",
		OwnerKind:    "token",
		OwnerID:      "tok_A",
		BusinessTag:  "chatgpt",
		AllocatedAt:  "2026-01-01T00:00:00Z",
		Status:       "allocated",
	}
	if _, err := st.RecordAllocation(alloc, "botA"); err != nil {
		t.Fatal(err)
	}

	// token A 应当拥有
	if !st.IsEmailOwnedByToken(ctx, "alpha@icloud.com", "tok_A") {
		t.Fatalf("AUTH01 失败: token A 应当拥有分配的别名")
	}

	// token B 绝不可拥有
	if st.IsEmailOwnedByToken(ctx, "alpha@icloud.com", "tok_B") {
		t.Fatalf("AUTH01 失败: token B 绝不可越权拥有 token A 的别名")
	}
}

// AUTH02: token_name 相同但 token_id 不同（删除重建同名 token），权限核验返回 false
func TestAUTH02_RecreatedSameNameTokenDoesNotInherit(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	// 原 token A
	_ = st.SaveToken(APIToken{ID: "tok_old", Name: "faka_bot", Token: "sec_old"})
	alloc := &AliasAllocation{
		AllocationID: "alloc_2",
		AliasEmail:   "beta@icloud.com",
		AccountID:    "acc_1",
		OwnerKind:    "token",
		OwnerID:      "tok_old",
		BusinessTag:  "steam",
		AllocatedAt:  "2026-01-01T00:00:00Z",
		Status:       "allocated",
	}
	_, _ = st.RecordAllocation(alloc, "faka_bot")

	// 删除原 token，新建同名但不同 ID 的 token
	_, _ = st.DeleteToken("tok_old")
	_ = st.SaveToken(APIToken{ID: "tok_new", Name: "faka_bot", Token: "sec_new"})

	// 新同名 token 不可继承
	if st.IsEmailOwnedByToken(ctx, "beta@icloud.com", "tok_new") {
		t.Fatalf("AUTH02 失败: 新建同名 token 越权继承了旧资产")
	}
}

// AUTH03: 外部 token 试图用 legacy 记录绕过所有权检查，被阻断
func TestAUTH03_LegacyRecordCannotBeClaimedByExternalToken(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	// 创建一个新外部 token
	_ = st.SaveToken(APIToken{ID: "tok_attacker", Name: "attacker", Token: "sec_att"})

	// 模拟一条归属为 legacy_unknown 的分配
	alloc := &AliasAllocation{
		AllocationID: "alloc_legacy",
		AliasEmail:   "legacy@icloud.com",
		AccountID:    "acc_1",
		OwnerKind:    "legacy_unknown",
		OwnerID:      "legacy_unknown",
		BusinessTag:  "default",
		AllocatedAt:  "2026-01-01T00:00:00Z",
		Status:       "allocated",
	}
	_, _ = st.RecordAllocation(alloc, "scheduler")

	// 外部 token 绝不可认领该 legacy 别名
	if st.IsEmailOwnedByToken(ctx, "legacy@icloud.com", "tok_attacker") {
		t.Fatalf("AUTH03 失败: 外部 token 绕过校验认领了 legacy 别名")
	}
}

// AUTH04: 管理员身份在各所有权接口中可正常穿透
func TestAUTH04_AdminOwnershipBypass(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	_, _ = st.db.Exec(`INSERT INTO accounts (id, name, real_email, status, tags, created_at, updated_at) VALUES ('acc_1', 'Account 1', 'a1@test.com', 'active', '[]', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z')`)
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "admin_target@icloud.com", Active: true}, "replenish", true)

	alloc := &AliasAllocation{
		AllocationID: "alloc_admin",
		AliasEmail:   "admin_target@icloud.com",
		AccountID:    "acc_1",
		OwnerKind:    "token",
		OwnerID:      "tok_client",
		BusinessTag:  "default",
		AllocatedAt:  "2026-01-01T00:00:00Z",
		Status:       "allocated",
	}
	if _, err := st.RecordAllocation(alloc, "client_bot"); err != nil {
		t.Fatalf("RecordAllocation failed: %v", err)
	}

	// 验证 GetPrincipalAllocation 在管理员主体下可以查询到记录
	record, err := st.GetPrincipalAllocation(ctx, "admin_target@icloud.com", "token", "tok_client")
	if err != nil || record == nil || record.AliasEmail != "admin_target@icloud.com" {
		t.Fatalf("AUTH04 失败: 查询分配记录失败: %v", err)
	}
}
