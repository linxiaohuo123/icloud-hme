package store

import (
	"context"
	"errors"
	"testing"

	"icloud-hme/internal/hme"
)

// B01: 同一业务操作重试返回相同数据库租约 ID，审计不重复
func TestB01_SamePrincipalIdempotentRetryReturnsSavedLeaseWithoutDuplicateAudit(t *testing.T) {
	tempDir := t.TempDir()
	st, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer st.Close()

	alloc1 := &AliasAllocation{
		AllocationID: "alloc_first",
		AliasEmail:   "idemp_test@example.com",
		AccountID:    "acc_1",
		OwnerKind:    "token",
		OwnerID:      "tok_123",
		BusinessTag:  "tag_a",
		AllocatedAt:  "2026-09-20T00:00:00Z",
		Status:       "allocated",
	}

	_, _ = st.db.Exec(`INSERT INTO accounts (id, name, real_email, status, tags, created_at, updated_at) VALUES ('acc_1', 'Account 1', 'a1@test.com', 'active', '[]', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z')`)
	if err := st.AddInventoryAlias("acc_1", hme.Alias{Email: "idemp_test@example.com", Active: true}, "replenish", true); err != nil {
		t.Fatalf("AddInventoryAlias failed: %v", err)
	}

	saved1, err := st.RecordAllocation(alloc1, "token_123")
	if err != nil {
		t.Fatalf("first RecordAllocation failed: %v", err)
	}
	if saved1.AllocationID != "alloc_first" {
		t.Fatalf("expected alloc_first, got: %s", saved1.AllocationID)
	}

	// 模拟相同主体、相同业务标签、相同邮箱的重试请求，调用方生成了新的内存 ID alloc_retry
	allocRetry := &AliasAllocation{
		AllocationID: "alloc_retry",
		AliasEmail:   "idemp_test@example.com",
		AccountID:    "acc_1",
		OwnerKind:    "token",
		OwnerID:      "tok_123",
		BusinessTag:  "tag_a",
		AllocatedAt:  "2026-09-20T00:01:00Z",
		Status:       "allocated",
	}

	savedRetry, err := st.RecordAllocation(allocRetry, "token_123")
	if err != nil {
		t.Fatalf("idempotent retry RecordAllocation failed: %v", err)
	}

	// 必须返回原租约 ID，不能是新 ID
	if savedRetry.AllocationID != "alloc_first" {
		t.Fatalf("B01 FAILED: retry returned unpersisted/new allocation ID %s, expected original %s", savedRetry.AllocationID, "alloc_first")
	}

	// 审计表 lease_records 只能有 1 条记录，严禁重复插入
	var count int
	err = st.db.QueryRow(`SELECT COUNT(*) FROM lease_records WHERE email = 'idemp_test@example.com'`).Scan(&count)
	if err != nil {
		t.Fatalf("count audit failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("B01 FAILED: duplicate audit records created: %d, expected 1", count)
	}
}

// B02: 跨主体/来源不明的 email 冲突返回 conflict，旧 lease 和其 operation/vreq 仍可定位
func TestB02_CrossPrincipalConflictDoesNotOverwriteAndPreservesReferences(t *testing.T) {
	tempDir := t.TempDir()
	st, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer st.Close()

	_, _ = st.db.Exec(`INSERT INTO accounts (id, name, real_email, status, tags, created_at, updated_at) VALUES ('acc_1', 'Account 1', 'a1@test.com', 'active', '[]', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z')`)
	if err := st.AddInventoryAlias("acc_1", hme.Alias{Email: "conflict_test@example.com", Active: true}, "replenish", true); err != nil {
		t.Fatalf("AddInventoryAlias failed: %v", err)
	}

	// 1. 建立主体 A 的初始租约
	allocA := &AliasAllocation{
		AllocationID: "alloc_owner_a",
		AliasEmail:   "conflict_test@example.com",
		AccountID:    "acc_1",
		OwnerKind:    "token",
		OwnerID:      "tok_owner_a",
		BusinessTag:  "tag_a",
		AllocatedAt:  "2026-09-20T00:00:00Z",
		Status:       "allocated",
	}
	_, err = st.RecordAllocation(allocA, "bot_a")
	if err != nil {
		t.Fatalf("record allocA failed: %v", err)
	}

	// 关联 operation 和 verification_request
	_, err = st.db.Exec(`INSERT INTO operations (operation_id, principal_kind, principal_id, operation_kind, idempotency_key, state, result_ref, candidate_email, created_at, updated_at) VALUES ('op_a', 'token', 'tok_owner_a', 'allocate', 'k_a', 'succeeded', 'alloc_owner_a', 'conflict_test@example.com', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z')`)
	if err != nil {
		t.Fatalf("insert op_a failed: %v", err)
	}
	_, err = st.db.Exec(`INSERT INTO verification_requests (request_id, principal_kind, principal_id, lease_id, alias_email, status, created_at, expires_at, code, matched_event_ref) VALUES ('vr_a', 'token', 'tok_owner_a', 'alloc_owner_a', 'conflict_test@example.com', 'succeeded', '2026-09-20T00:00:00Z', '2099-01-01T00:00:00Z', 'code123', 'ev123')`)
	if err != nil {
		t.Fatalf("insert vr_a failed: %v", err)
	}

	// 2. 主体 B 尝试对同一邮箱发起 RecordAllocation
	allocB := &AliasAllocation{
		AllocationID: "alloc_owner_b",
		AliasEmail:   "conflict_test@example.com",
		AccountID:    "acc_1",
		OwnerKind:    "token",
		OwnerID:      "tok_owner_b",
		BusinessTag:  "tag_b",
		AllocatedAt:  "2026-09-20T00:05:00Z",
		Status:       "allocated",
	}
	_, err = st.RecordAllocation(allocB, "bot_b")
	if err == nil {
		t.Fatalf("B02 FAILED: cross-principal conflict was accepted without error")
	}

	// 3. 验证主体 A 的租约与归属完全未被篡改
	var currentOwnerID, currentAllocID string
	err = st.db.QueryRow(`SELECT owner_id, allocation_id FROM alias_allocations WHERE alias_email = 'conflict_test@example.com'`).Scan(&currentOwnerID, &currentAllocID)
	if err != nil {
		t.Fatalf("query allocation failed: %v", err)
	}
	if currentOwnerID != "tok_owner_a" || currentAllocID != "alloc_owner_a" {
		t.Fatalf("B02 FAILED: allocation was corrupted: owner=%s alloc_id=%s", currentOwnerID, currentAllocID)
	}

	// 4. 验证旧 operation 和 verification request 的引用仍然健全(无悬空引用)
	var danglingOps, danglingVreqs int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM operations o LEFT JOIN alias_allocations a ON a.allocation_id = o.result_ref WHERE o.operation_id = 'op_a' AND a.allocation_id IS NULL`).Scan(&danglingOps)
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM verification_requests v LEFT JOIN alias_allocations a ON a.allocation_id = v.lease_id WHERE v.request_id = 'vr_a' AND a.allocation_id IS NULL`).Scan(&danglingVreqs)
	if danglingOps != 0 || danglingVreqs != 0 {
		t.Fatalf("B02 FAILED: dangling references created: danglingOps=%d, danglingVreqs=%d", danglingOps, danglingVreqs)
	}
}

// B03: 通过 SQLite trigger/故障注入让 inventory UPDATE、operation UPDATE 分别失败，整个事务不能提交成功
func TestB03_FaultInjectionRollsBackTransaction(t *testing.T) {
	tempDir := t.TempDir()
	st, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer st.Close()

	_, _ = st.db.Exec(`INSERT INTO accounts (id, name, real_email, status, tags, created_at, updated_at) VALUES ('acc_1', 'Account 1', 'a1@test.com', 'active', '[]', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z')`)
	// 准备可用库存
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "fault@example.com", Active: true}, "replenish", true)

	// 注入 1: inventory UPDATE 失败触发器
	_, err = st.db.Exec(`CREATE TRIGGER trigger_fail_inv BEFORE UPDATE ON alias_inventory BEGIN SELECT RAISE(FAIL, 'injected inventory failure'); END;`)
	if err != nil {
		t.Fatalf("create trigger failed: %v", err)
	}

	_, _, err = st.ClaimInventoryAlias(context.Background(), "token", "tok_fault", "allocate", "idemp_fault1", "hash1", "tag", nil)
	if err == nil {
		t.Fatalf("B03 FAILED: ClaimInventoryAlias succeeded despite injected inventory failure")
	}

	// 验证未产生孤立 allocation
	var allocCount int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM alias_allocations WHERE alias_email = 'fault@example.com'`).Scan(&allocCount)
	if allocCount != 0 {
		t.Fatalf("B03 FAILED: isolated allocation was committed: count=%d", allocCount)
	}

	// 删除触发器 1，恢复 inventory 更新
	_, _ = st.db.Exec(`DROP TRIGGER trigger_fail_inv`)

	// 注入 2: operations UPDATE 失败触发器
	_, err = st.db.Exec(`CREATE TRIGGER trigger_fail_op BEFORE UPDATE ON operations BEGIN SELECT RAISE(FAIL, 'injected operation failure'); END;`)
	if err != nil {
		t.Fatalf("create trigger failed: %v", err)
	}

	_, _, err = st.ClaimInventoryAlias(context.Background(), "token", "tok_fault", "allocate", "idemp_fault2", "hash2", "tag", nil)
	if err == nil {
		t.Fatalf("B03 FAILED: ClaimInventoryAlias succeeded despite injected operation failure")
	}

	// 验证 inventory 未被错误置为 allocated，且 operations 未留在 pending
	var invState string
	_ = st.db.QueryRow(`SELECT allocation_state FROM alias_inventory WHERE email = 'fault@example.com'`).Scan(&invState)
	if invState == "allocated" {
		t.Fatalf("B03 FAILED: inventory state was updated to allocated despite rollback: %s", invState)
	}
}

// B05: 幂等成功/失败重放保持原业务结果；记录不一致不发新号
func TestB05_IdempotentReplayIntegrity(t *testing.T) {
	tempDir := t.TempDir()
	st, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer st.Close()

	_, _ = st.db.Exec(`INSERT INTO accounts (id, name, real_email, status, tags, created_at, updated_at) VALUES ('acc_1', 'Account 1', 'a1@test.com', 'active', '[]', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z')`)
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "b05@example.com", Active: true}, "replenish", true)

	// 1. 成功执行一次认领
	alloc1, op1, err := st.ClaimInventoryAlias(context.Background(), "token", "tok_b05", "allocate", "idemp_b05", "hash_valid", "tag", nil)
	if err != nil || alloc1 == nil {
		t.Fatalf("initial claim failed: %v", err)
	}

	// 2. 正常幂等重放: 返回同一分配结果
	alloc2, op2, err := st.ClaimInventoryAlias(context.Background(), "token", "tok_b05", "allocate", "idemp_b05", "hash_valid", "tag", nil)
	if err != nil {
		t.Fatalf("idempotent replay failed: %v", err)
	}
	if alloc2.AllocationID != alloc1.AllocationID || alloc2.AliasEmail != alloc1.AliasEmail {
		t.Fatalf("replay returned different allocation: %v vs %v", alloc2, alloc1)
	}
	if op2.OperationID != op1.OperationID {
		t.Fatalf("replay returned different op: %v vs %v", op2, op1)
	}

	// 3. 破坏数据一致性: 将 operations.result_ref 改为一个不存在的 allocation_id
	_, _ = st.db.Exec(`UPDATE operations SET result_ref = 'alloc_nonexistent' WHERE operation_id = ?`, op1.OperationID)

	// 再次重放: 必须明确报告状态不一致异常，绝不静默发新号
	allocTampered, _, err := st.ClaimInventoryAlias(context.Background(), "token", "tok_b05", "allocate", "idemp_b05", "hash_valid", "tag", nil)
	if err == nil {
		t.Fatalf("B05 FAILED: tampered operation returned success: %v", allocTampered)
	}
}

// B06: 认领成功与空池失败测试
func TestB06_ClaimInventoryAlias_SuccessAndEmptyPool(t *testing.T) {
	tempDir := t.TempDir()
	st, err := NewStore(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	_, _ = st.db.Exec(`INSERT INTO accounts (id, name, real_email, status, tags, created_at, updated_at) VALUES ('acc_1', 'Account 1', 'a1@test.com', 'active', '[]', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z')`)
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "b06@example.com", Active: true}, "replenish", true)

	// 1. 成功认领
	alloc, op, err := st.ClaimInventoryAlias(context.Background(), "token", "tok_b06", "allocate", "idemp_b06", "h1", "default", nil)
	if err != nil || alloc == nil || op == nil {
		t.Fatalf("claim failed: %v", err)
	}
	if alloc.AliasEmail != "b06@example.com" || op.State != "succeeded" {
		t.Fatalf("unexpected allocation or op: alloc=%+v op=%+v", alloc, op)
	}

	// 2. 池空后认领失败，operation 标记为 failed
	alloc2, op2, err := st.ClaimInventoryAlias(context.Background(), "token", "tok_b06", "allocate", "idemp_b06_2", "h2", "default", nil)
	if err == nil || alloc2 != nil {
		t.Fatalf("expected error on empty pool, got: %v", err)
	}
	if !errors.Is(err, ErrNoAvailableInventory) {
		t.Fatalf("expected ErrNoAvailableInventory, got: %v", err)
	}
	if op2 == nil || op2.State != "failed" || op2.ErrorCode != "NO_AVAILABLE_INVENTORY" {
		t.Fatalf("expected failed operation with code NO_AVAILABLE_INVENTORY, got: %+v", op2)
	}
}

// B07: 上下文取消测试 (Cancellation)
func TestB07_ClaimInventoryAlias_ContextCancellation(t *testing.T) {
	tempDir := t.TempDir()
	st, err := NewStore(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	_, _ = st.db.Exec(`INSERT INTO accounts (id, name, real_email, status, tags, created_at, updated_at) VALUES ('acc_1', 'Account 1', 'a1@test.com', 'active', '[]', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z')`)
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "b07@example.com", Active: true}, "replenish", true)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预先取消

	alloc, _, err := st.ClaimInventoryAlias(ctx, "token", "tok_b07", "allocate", "idemp_b07", "h1", "default", nil)
	if err == nil || alloc != nil {
		t.Fatalf("expected cancellation error, got: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

// B08: 零行更新、缺失库存或非合法状态直接拒绝，事务回滚
func TestB08_RecordAllocation_ZeroRowsOrStateRejection(t *testing.T) {
	tempDir := t.TempDir()
	st, err := NewStore(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	_, _ = st.db.Exec(`INSERT INTO accounts (id, name, real_email, status, tags, created_at, updated_at) VALUES ('acc_1', 'Account 1', 'a1@test.com', 'active', '[]', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z')`)

	// 1. 无库存记录直接拒绝
	allocMissing := &AliasAllocation{
		AllocationID: "alloc_missing",
		AliasEmail:   "missing@example.com",
		AccountID:    "acc_1",
		OwnerKind:    "token",
		OwnerID:      "tok_1",
		BusinessTag:  "tag",
		AllocatedAt:  "2026-09-20T00:00:00Z",
		Status:       "allocated",
	}
	_, err = st.RecordAllocation(allocMissing, "tok_1")
	if err == nil || !errors.Is(err, ErrNoAvailableInventory) {
		t.Fatalf("expected ErrNoAvailableInventory for missing inventory, got: %v", err)
	}

	// 2. 隔离或受保护状态 (reserved/quarantined/legacy_unknown) 拒绝直接 UPDATE 为 allocated
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "res@example.com", Active: true}, "replenish", false) // unknown
	_, _ = st.db.Exec(`UPDATE alias_inventory SET allocation_state = 'reserved' WHERE email = 'res@example.com'`)

	allocRes := &AliasAllocation{
		AllocationID: "alloc_res",
		AliasEmail:   "res@example.com",
		AccountID:    "acc_1",
		OwnerKind:    "token",
		OwnerID:      "tok_1",
		BusinessTag:  "tag",
		AllocatedAt:  "2026-09-20T00:00:00Z",
		Status:       "allocated",
	}
	_, err = st.RecordAllocation(allocRes, "tok_1")
	if err == nil || !errors.Is(err, ErrAllocationConflict) {
		t.Fatalf("expected ErrAllocationConflict for reserved inventory, got: %v", err)
	}
}

// B09: 提交失败保证事务回滚 (零行孤立写入)
func TestB09_ClaimInventoryAlias_TxFailureRollback(t *testing.T) {
	tempDir := t.TempDir()
	st, err := NewStore(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	_, _ = st.db.Exec(`INSERT INTO accounts (id, name, real_email, status, tags, created_at, updated_at) VALUES ('acc_1', 'Account 1', 'a1@test.com', 'active', '[]', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z')`)
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "b09@example.com", Active: true}, "replenish", true)

	// 注入触发器阻断 lease_records 插入
	_, _ = st.db.Exec(`CREATE TRIGGER fail_lease_insert BEFORE INSERT ON lease_records BEGIN SELECT RAISE(FAIL, 'injected lease insert fail'); END;`)

	alloc, _, err := st.ClaimInventoryAlias(context.Background(), "token", "tok_b09", "allocate", "idemp_b09", "h1", "default", nil)
	if err == nil || alloc != nil {
		t.Fatalf("expected error on injected commit failure, got: %v", err)
	}

	// 确认事务回滚：inventory 保持 available，无 allocation
	var state string
	_ = st.db.QueryRow(`SELECT allocation_state FROM alias_inventory WHERE email = 'b09@example.com'`).Scan(&state)
	if state != "available" {
		t.Fatalf("expected state available, got %s", state)
	}
	var allocCnt int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM alias_allocations WHERE alias_email = 'b09@example.com'`).Scan(&allocCnt)
	if allocCnt != 0 {
		t.Fatalf("expected 0 allocations, got %d", allocCnt)
	}
}
