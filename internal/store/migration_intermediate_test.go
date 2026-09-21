/**
 * [INPUT]: 依赖 testing, database/sql, path/filepath, time, context, internal/store
 * [OUTPUT]: 提供 TestMIG_MID_01 至 TestMIG_MID_05 中间态迁移与架构演进回归单测
 * [POS]: internal/store 的 PR-04 ~ PR-08 中间态架构兼容性与补列数据完整性门禁 (Issue 17)
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// MIG-MID-01: PR-04 遗留 verification_requests 补列 (baseline_mailbox/provider/uid 等) 与数据完整性
func TestMIG_MID_01_PR04_To_PR08_VerificationRequestsSchemaEvolution(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "icloud_hme.db")
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// 模拟 PR-04 阶段较早的 verification_requests 结构 (缺少 baseline_mailbox 等)
	_, err = rawDB.Exec(`
		CREATE TABLE verification_requests (
			request_id TEXT PRIMARY KEY,
			principal_kind TEXT NOT NULL,
			principal_id TEXT NOT NULL,
			lease_id TEXT NOT NULL,
			alias_email TEXT NOT NULL,
			status TEXT NOT NULL,
			created_at TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			code TEXT DEFAULT ''
		);
		INSERT INTO verification_requests (request_id, principal_kind, principal_id, lease_id, alias_email, status, created_at, expires_at)
		VALUES ('vreq_pr04_old', 'token', 'tok_legacy', 'alloc_legacy', 'legacy@icloud.com', 'pending', '2026-09-20T00:00:00Z', '2026-09-25T00:00:00Z');
	`)
	if err != nil {
		t.Fatal(err)
	}
	rawDB.Close()

	// 加载升级至当前 Store
	st, err := NewStore(dir)
	if err != nil {
		t.Fatalf("升级 PR-04 库失败: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	req, err := st.GetVerificationRequest(ctx, "vreq_pr04_old", "token", "tok_legacy")
	if err != nil {
		t.Fatalf("查询迁移后 verification_request 失败: %v", err)
	}
	if req.BaselineMailbox != "INBOX" {
		t.Fatalf("MIG-MID-01 失败: baseline_mailbox 默认值期望 'INBOX', 实际: %s", req.BaselineMailbox)
	}
	if req.Status != "pending" {
		t.Fatalf("MIG-MID-01 失败: 既有状态被篡改: %s", req.Status)
	}

	// 验证终态 CAS 对历史补列数据生效
	_, ok, err := st.InvalidateVerificationRequest(ctx, "vreq_pr04_old")
	if err != nil || !ok {
		t.Fatalf("MIG-MID-01 失败: 对补列历史记录执行 Invalidate 失败: ok=%v, err=%v", ok, err)
	}
}

// MIG-MID-02: PR-05 中间态未知库存与历史归属隔离 (未知库存绝不变成 available，历史归属绝不模糊继承)
func TestMIG_MID_02_PR05_To_PR08_InventoryStateQuarantine(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "icloud_hme.db")
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// 模拟 PR-05 中间库：存在 alias_routes 和 lease_records，但无 alias_inventory 或无不可变 token_id
	_, err = rawDB.Exec(`
		CREATE TABLE alias_routes (email TEXT PRIMARY KEY, account_id TEXT, updated_at TEXT);
		CREATE TABLE lease_records (id TEXT PRIMARY KEY, email TEXT, account_id TEXT, tag TEXT, status TEXT, allocated_at TEXT, token_name TEXT);
		CREATE TABLE api_tokens (id TEXT PRIMARY KEY, name TEXT, token TEXT, created_at TEXT, scopes TEXT);

		INSERT INTO alias_routes (email, account_id, updated_at) VALUES ('orphan_route@icloud.com', 'acc_1', '2026-09-20T00:00:00Z');
		INSERT INTO lease_records (id, email, account_id, tag, status, allocated_at, token_name) VALUES ('lease_mid', 'leased_mid@icloud.com', 'acc_1', 'tag_mid', 'completed', '2026-09-20T00:00:00Z', 'recreated_bot');
		INSERT INTO api_tokens (id, name, token, created_at, scopes) VALUES ('tok_mid_new', 'recreated_bot', 'sec_mid', '2026-09-21T00:00:00Z', 'allocate');
	`)
	if err != nil {
		t.Fatal(err)
	}
	rawDB.Close()

	st, err := NewStore(dir)
	if err != nil {
		t.Fatalf("加载 PR-05 中间库失败: %v", err)
	}
	defer st.Close()

	// 1. orphan_route 绝不可作为可用库存被领走
	ctx := context.Background()
	_, _, err = st.ClaimInventoryAlias(ctx, "token", "tok_mid_new", "allocate", "key_mid_claim", "hash", "tag", nil)
	if !errors.Is(err, ErrNoAvailableInventory) {
		t.Fatalf("MIG-MID-02 失败: 未知库存 orphan_route 绝不能被认领, 得到 err=%v", err)
	}

	// 2. leased_mid 归属必须为 legacy_unknown，新创建的同名 token 绝不可越权认领其 allocation (去除假阳性，使用真实 email 参数)
	alloc, err := st.GetPrincipalAllocation(ctx, "leased_mid@icloud.com", "token", "tok_mid_new")
	if err == nil || alloc != nil {
		t.Fatalf("MIG-MID-02 失败: 重建同名 token 越权获得了历史出号资产: %+v", alloc)
	}
	if !errors.Is(err, ErrAllocationNotFound) {
		t.Fatalf("MIG-MID-02 失败: 期望 ErrAllocationNotFound, 实际: %v", err)
	}

	// 3. 校验历史分配记录必须安全归属于 legacy_unknown，且可由 legacy 身份查询
	legacyAlloc, err := st.GetPrincipalAllocation(ctx, "leased_mid@icloud.com", "legacy_unknown", "legacy_unknown")
	if err != nil || legacyAlloc == nil {
		t.Fatalf("MIG-MID-02 失败: 历史分配记录必须归属于 legacy_unknown: %v", err)
	}
	if legacyAlloc.OwnerKind != "legacy_unknown" || legacyAlloc.OwnerID != "legacy_unknown" {
		t.Fatalf("MIG-MID-02 失败: 历史归属篡改: %+v", legacyAlloc)
	}
}

// MIG-MID-03: PR-06 幂等 operations 表约束与并发请求 hash 冲突校验
func TestMIG_MID_03_PR06_OperationsIdempotencyTableAndHashEnforcement(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "icloud_hme.db")
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// 模拟 PR-06 operations 存在但有一条进行中未结记录
	_, err = rawDB.Exec(`
		CREATE TABLE operations (
			operation_id TEXT PRIMARY KEY,
			principal_kind TEXT NOT NULL,
			principal_id TEXT NOT NULL,
			operation_kind TEXT NOT NULL,
			idempotency_key TEXT NOT NULL,
			request_hash TEXT NOT NULL,
			state TEXT NOT NULL,
			candidate_email TEXT DEFAULT '',
			result_ref TEXT DEFAULT '',
			error_code TEXT DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			CONSTRAINT uq_op_idempotency UNIQUE (principal_kind, principal_id, operation_kind, idempotency_key)
		);
		INSERT INTO operations (operation_id, principal_kind, principal_id, operation_kind, idempotency_key, request_hash, state, created_at, updated_at)
		VALUES ('op_mid_exist', 'token', 'tok_mid', 'allocate', 'key_idemp_mid', 'orig_hash', 'pending', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z');
	`)
	if err != nil {
		t.Fatal(err)
	}
	rawDB.Close()

	st, err := NewStore(dir)
	if err != nil {
		t.Fatalf("加载 PR-06 operations 库失败: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	// 1. 相同 key 但不同 hash 必须报 409 冲突
	_, _, err = st.ClaimInventoryAlias(ctx, "token", "tok_mid", "allocate", "key_idemp_mid", "conflict_hash", "tag", nil)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("MIG-MID-03 失败: 预期 ErrIdempotencyConflict, 实际: %v", err)
	}

	// 2. 相同 key 相同 hash 处于 pending 状态必须报 ErrOperationPending
	_, _, err = st.ClaimInventoryAlias(ctx, "token", "tok_mid", "allocate", "key_idemp_mid", "orig_hash", "tag", nil)
	if !errors.Is(err, ErrOperationPending) {
		t.Fatalf("MIG-MID-03 失败: 预期 ErrOperationPending, 实际: %v", err)
	}
}

// MIG-MID-04: PR-07/PR-08 增量游标基线查询与终态原子 CAS 状态机演化
func TestMIG_MID_04_PR07_PR08_VerificationAtomicCASAndBaselineMinUID(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(dir)
	if err != nil {
		t.Fatalf("创建 Store 失败: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	email := "cursor_test@icloud.com"

	// 插入两条活跃请求和一条已完结请求
	err = st.CreateVerificationRequest(ctx, &VerificationRequest{
		RequestID:        "vreq_c1",
		PrincipalKind:    "token",
		PrincipalID:      "tok_1",
		LeaseID:          "lease_1",
		AliasEmail:       email,
		Status:           "pending",
		BaselineProvider: "imap",
		BaselineMailbox:  "INBOX",
		BaselineUID:      100,
		CreatedAt:        now.Format(time.RFC3339),
		ExpiresAt:        now.Add(10 * time.Minute).Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("CreateVerificationRequest vreq_c1 失败: %v", err)
	}

	err = st.CreateVerificationRequest(ctx, &VerificationRequest{
		RequestID:        "vreq_c2",
		PrincipalKind:    "token",
		PrincipalID:      "tok_2",
		LeaseID:          "lease_2",
		AliasEmail:       email,
		Status:           "ready",
		BaselineProvider: "imap",
		BaselineMailbox:  "INBOX",
		BaselineUID:      150,
		CreatedAt:        now.Format(time.RFC3339),
		ExpiresAt:        now.Add(10 * time.Minute).Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("CreateVerificationRequest vreq_c2 失败: %v", err)
	}

	// 1. 两条活跃任务中最小基线 UID 必须是 100
	minUID, err := st.GetMinBaselineUIDByEmail(ctx, email)
	if err != nil || minUID != 100 {
		t.Fatalf("MIG-MID-04 失败: 预期最小 baseline UID 为 100, 实际: %d, err: %v", minUID, err)
	}

	// 2. 原子完成 vreq_c1
	_, ok, err := st.CompleteVerificationRequest(ctx, "vreq_c1", "654321", "ref_msg_1")
	if err != nil || !ok {
		t.Fatalf("MIG-MID-04 失败: CompleteVerificationRequest 失败: ok=%v, err=%v", ok, err)
	}

	// 3. 重复完成或尝试改写终态必须被 CAS 阻断
	_, okRepeat, err := st.CompleteVerificationRequest(ctx, "vreq_c1", "999999", "ref_msg_2")
	if err != nil || okRepeat {
		t.Fatalf("MIG-MID-04 失败: 已完成任务严禁被重复完成覆写: ok=%v", okRepeat)
	}
	_, okExpire, err := st.ExpireVerificationRequest(ctx, "vreq_c1")
	if err != nil || okExpire {
		t.Fatalf("MIG-MID-04 失败: 已完成任务严禁被过期状态覆写: ok=%v", okExpire)
	}

	// 4. vreq_c1 完结后，剩余唯一活跃任务 vreq_c2 的基线是 150，GetMinBaselineUIDByEmail 必须上浮为 150
	minUIDAfter, err := st.GetMinBaselineUIDByEmail(ctx, email)
	if err != nil || minUIDAfter != 150 {
		t.Fatalf("MIG-MID-04 失败: 预期最小 baseline UID 上浮为 150, 实际: %d, err: %v", minUIDAfter, err)
	}

	// 5. 将 vreq_c2 过期后，无活跃任务，GetMinBaselineUIDByEmail 必须返回 0
	_, okExp2, err := st.ExpireVerificationRequest(ctx, "vreq_c2")
	if err != nil || !okExp2 {
		t.Fatalf("MIG-MID-04 失败: ExpireVerificationRequest vreq_c2 失败: ok=%v, err=%v", okExp2, err)
	}
	minUIDZero, err := st.GetMinBaselineUIDByEmail(ctx, email)
	if err != nil || minUIDZero != 0 {
		t.Fatalf("MIG-MID-04 失败: 无活跃任务预期 baseline UID 为 0, 实际: %d, err: %v", minUIDZero, err)
	}
}

// MIG-MID-05: 中间态 alias_allocations 表缺失 account_id 时的平滑补列与 backfill 验证 (MIG-01)
func TestMIG_MID_05_IntermediateAliasAllocationMissingAccountID(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "icloud_hme.db")
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// 模拟中间版本：alias_allocations 无 account_id 列
	_, err = rawDB.Exec(`
		CREATE TABLE alias_inventory (
			email TEXT PRIMARY KEY,
			account_id TEXT NOT NULL,
			remote_state TEXT NOT NULL DEFAULT 'active',
			allocation_state TEXT NOT NULL DEFAULT 'allocated',
			source_type TEXT NOT NULL DEFAULT 'replenish',
			snapshot_version INTEGER NOT NULL DEFAULT 1
		);
		CREATE TABLE alias_allocations (
			allocation_id TEXT PRIMARY KEY,
			alias_email TEXT NOT NULL UNIQUE,
			owner_kind TEXT NOT NULL,
			owner_id TEXT NOT NULL,
			business_tag TEXT DEFAULT '',
			allocated_at TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'allocated'
		);

		INSERT INTO alias_inventory (email, account_id, remote_state, allocation_state, source_type)
		VALUES ('backfill_test@icloud.com', 'acc_backfilled_target', 'active', 'allocated', 'replenish');

		INSERT INTO alias_allocations (allocation_id, alias_email, owner_kind, owner_id, business_tag, allocated_at, status)
		VALUES ('alloc_no_acc', 'backfill_test@icloud.com', 'token', 'tok_owner_1', 'tag_test', '2026-09-20T00:00:00Z', 'allocated');
	`)
	if err != nil {
		t.Fatal(err)
	}
	rawDB.Close()

	// 启动 Store，触发自动补列与 backfill
	st, err := NewStore(dir)
	if err != nil {
		t.Fatalf("中间态缺失 account_id 补列升级失败: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	alloc, err := st.GetPrincipalAllocation(ctx, "backfill_test@icloud.com", "token", "tok_owner_1")
	if err != nil {
		t.Fatalf("查询补列后 allocation 失败: %v", err)
	}
	if alloc.AccountID != "acc_backfilled_target" {
		t.Fatalf("MIG-01 失败: account_id backfill 期望 'acc_backfilled_target', 实际: '%s'", alloc.AccountID)
	}
}

