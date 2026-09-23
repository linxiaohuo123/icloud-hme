package store

import (
	"context"
	"errors"
	"testing"

	"icloud-hme/internal/hme"
)

// A01: unknown + 无 allocation + 普通账号，重启/对账后仍不能自动变 available
func TestA01_UnknownInventoryRemainsUnavailableAfterReconcile(t *testing.T) {
	tempDir := t.TempDir()
	st, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer st.Close()

	// 插入一个普通账号
	_, err = st.db.Exec(`INSERT INTO accounts (id, name, real_email, status, tags, created_at, updated_at) VALUES ('acc_normal', 'Normal Account', 'norm@test.com', 'active', '[]', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z')`)
	if err != nil {
		t.Fatalf("insert account failed: %v", err)
	}

	// 插入一个 unknown 状态别名，且无 allocation
	_, err = st.db.Exec(`INSERT INTO alias_inventory (email, account_id, remote_state, allocation_state, source_type) VALUES ('unknown@example.com', 'acc_normal', 'unknown', 'unknown', 'legacy_unknown')`)
	if err != nil {
		t.Fatalf("insert inventory failed: %v", err)
	}

	// 执行库存对齐
	_, err = st.ReconcileAvailableInventory()
	if err != nil {
		t.Fatalf("ReconcileAvailableInventory failed: %v", err)
	}

	// 核验：状态绝不能变成 available
	var allocState, remoteState string
	err = st.db.QueryRow(`SELECT allocation_state, remote_state FROM alias_inventory WHERE email = 'unknown@example.com'`).Scan(&allocState, &remoteState)
	if err != nil {
		t.Fatalf("query inventory failed: %v", err)
	}
	if allocState == "available" {
		t.Fatalf("A01 FAILED: unknown inventory was erroneously promoted to available")
	}
	if remoteState == "active" {
		t.Fatalf("A01 FAILED: remote_state was erroneously set to active without upstream evidence")
	}

	// 认领尝试必须返回 ErrNoAvailableInventory
	_, _, err = st.ClaimInventoryAlias(context.Background(), "token", "tok_test", "allocate", "idemp_1", "hash_1", "", nil)
	if !errors.Is(err, ErrNoAvailableInventory) {
		t.Fatalf("expected ErrNoAvailableInventory, got: %v", err)
	}
}

// A02: unknown/inactive/deleted 远端状态不能被本地对账写 active；账号不存在也不可分配
func TestA02_ReconcileDoesNotPromoteInactiveOrOrphanAccounts(t *testing.T) {
	tempDir := t.TempDir()
	st, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer st.Close()

	// 1. 普通账号但远端为 inactive 的别名
	_, _ = st.db.Exec(`INSERT INTO accounts (id, name, real_email, status, tags, created_at, updated_at) VALUES ('acc_norm', 'Normal', 'norm@test.com', 'active', '[]', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z')`)
	_, _ = st.db.Exec(`INSERT INTO alias_inventory (email, account_id, remote_state, allocation_state, source_type) VALUES ('inactive@example.com', 'acc_norm', 'inactive', 'unknown', 'legacy_unknown')`)

	// 2. 账号根本不存在的孤儿别名
	_, _ = st.db.Exec(`INSERT INTO alias_inventory (email, account_id, remote_state, allocation_state, source_type) VALUES ('orphan@example.com', 'missing_account', 'unknown', 'unknown', 'legacy_unknown')`)

	_, err = st.ReconcileAvailableInventory()
	if err != nil {
		t.Fatalf("ReconcileAvailableInventory failed: %v", err)
	}

	var inactAlloc, inactRemote string
	_ = st.db.QueryRow(`SELECT allocation_state, remote_state FROM alias_inventory WHERE email = 'inactive@example.com'`).Scan(&inactAlloc, &inactRemote)
	if inactRemote == "active" || inactAlloc == "available" {
		t.Fatalf("A02 FAILED: inactive asset promoted to active/available (remote=%s, alloc=%s)", inactRemote, inactAlloc)
	}

	var orphanAlloc string
	_ = st.db.QueryRow(`SELECT allocation_state FROM alias_inventory WHERE email = 'orphan@example.com'`).Scan(&orphanAlloc)
	if orphanAlloc == "available" {
		t.Fatalf("A02 FAILED: orphan asset without account promoted to available")
	}
}

// A03: 明确补货库存可分配；历史已分配资产在重启/刷新/清日志后不再分配
func TestA03_ReplenishedAvailableAndAllocatedRemainsAllocated(t *testing.T) {
	tempDir := t.TempDir()
	st, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer st.Close()

	_, _ = st.db.Exec(`INSERT INTO accounts (id, name, real_email, status, tags, created_at, updated_at) VALUES ('acc_1', 'Account 1', 'a1@test.com', 'active', '[]', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z')`)

	// 1. 系统明确补货入库条目
	err = st.AddInventoryAlias("acc_1", hme.Alias{Email: "replenished@example.com", Active: true}, "replenish", true)
	if err != nil {
		t.Fatalf("AddInventoryAlias failed: %v", err)
	}

	// 2. 认领该别名
	alloc, _, err := st.ClaimInventoryAlias(context.Background(), "token", "tok_user1", "allocate", "idemp_rep", "hash_rep", "test", nil)
	if err != nil || alloc == nil {
		t.Fatalf("claim replenished alias failed: %v", err)
	}
	if alloc.AliasEmail != "replenished@example.com" {
		t.Fatalf("unexpected claimed email: %s", alloc.AliasEmail)
	}

	// 3. 执行对账和重入
	_, _ = st.ReconcileAvailableInventory()

	// 再次认领必须池空，历史已分配资产绝不再被认领
	_, _, err = st.ClaimInventoryAlias(context.Background(), "token", "tok_user2", "allocate", "idemp_rep2", "hash_rep2", "test", nil)
	if !errors.Is(err, ErrNoAvailableInventory) {
		t.Fatalf("expected ErrNoAvailableInventory after asset allocated, got: %v", err)
	}
}

// A04: 后加保护账号场景，Claim 事务必须阻断发放
func TestA04_SubsequentProtectionBlocksClaim(t *testing.T) {
	tempDir := t.TempDir()
	st, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer st.Close()

	// 1. 账号最初是正常的普通账号
	_, err = st.db.Exec(`INSERT INTO accounts (id, name, real_email, status, tags, created_at, updated_at) VALUES ('acc_dyn', 'Normal Init', 'dyn@test.com', 'active', '[]', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z')`)
	if err != nil {
		t.Fatalf("insert account failed: %v", err)
	}

	// 明确补货入库为 available
	err = st.AddInventoryAlias("acc_dyn", hme.Alias{Email: "dyn@example.com", Active: true}, "replenish", true)
	if err != nil {
		t.Fatalf("AddInventoryAlias failed: %v", err)
	}

	// 2. 账号后来被标记为保护账号 (例如打标 protected，或改名含大号)
	_, err = st.db.Exec(`UPDATE accounts SET tags = '["protected"]' WHERE id = 'acc_dyn'`)
	if err != nil {
		t.Fatalf("update account to protected failed: %v", err)
	}

	// 3. 此时外部直接尝试从库存中认领，由于当前所属账号已是保护账号，必须 fail closed 阻断
	_, _, err = st.ClaimInventoryAlias(context.Background(), "token", "tok_dyn", "allocate", "idemp_dyn", "hash_dyn", "", nil)
	if !errors.Is(err, ErrNoAvailableInventory) {
		t.Fatalf("A04 FAILED: dynamically protected account's inventory was claimed, err=%v", err)
	}
}

