/**
 * [INPUT]: 依赖 testing, path/filepath, sync, sync/atomic, time, context, fmt, icloud-hme/internal/store, icloud-hme/internal/hme
 * [OUTPUT]: 对外提供 PR-03 别名库存状态机、SQLite 事务原子认领与幂等防重单元测试 (D01-D08) 及 PR-08 原子验证码创建并发压测
 * [POS]: internal/store 的领域状态与事务正确性回归防线
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"icloud-hme/internal/hme"
)

func insertTestAccount(t *testing.T, st *Store, id string) {
	t.Helper()
	_, err := st.db.Exec(`INSERT INTO accounts (id, name, real_email, status, tags, created_at, updated_at) 
		VALUES (?, ?, ? || '@test.com', 'active', '[]', '2026-09-20T00:00:00Z', '2026-09-20T00:00:00Z')`,
		id, "Name_"+id, id)
	if err != nil {
		t.Fatalf("insertTestAccount failed: %v", err)
	}
}

// D01: 100 个并发相同幂等请求仅产生一个 allocation，响应结果完全一致
func TestPR03_D01_ConcurrentIdenticalIdempotentClaim(t *testing.T) {
	tempDir := t.TempDir()
	st, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	insertTestAccount(t, st, "acc_1")

	// 准备可用库存
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "pool1@icloud.com", Active: true}, "replenish", true)

	ctx := context.Background()
	const concurrency = 100
	idempKey := "idemp_same_key_100"
	reqHash := "hash_abc_123"

	var wg sync.WaitGroup
	wg.Add(concurrency)

	results := make([]*AliasAllocation, concurrency)
	errorsList := make([]error, concurrency)

	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()
			alloc, _, err := st.ClaimInventoryAlias(ctx, "token", "tok_test", "allocate", idempKey, reqHash, "test_tag", nil)
			results[idx] = alloc
			errorsList[idx] = err
		}(i)
	}

	wg.Wait()

	var firstAlloc *AliasAllocation
	for i := 0; i < concurrency; i++ {
		// 只要有结果，要么成功获得 allocation，要么报告 operation pending
		if errorsList[i] == nil {
			if firstAlloc == nil {
				firstAlloc = results[i]
			} else if results[i].AllocationID != firstAlloc.AllocationID {
				t.Fatalf("concurrent claims returned different allocations: %s vs %s", results[i].AllocationID, firstAlloc.AllocationID)
			}
		} else if !errors.Is(errorsList[i], ErrOperationPending) {
			t.Fatalf("unexpected error during concurrent claim: %v", errorsList[i])
		}
	}

	if firstAlloc == nil {
		t.Fatal("at least one claim must succeed")
	}

	// 再次以相同 key 和 hash 查询，必须返回相同的 allocation
	repeatAlloc, _, err := st.ClaimInventoryAlias(ctx, "token", "tok_test", "allocate", idempKey, reqHash, "test_tag", nil)
	if err != nil || repeatAlloc.AllocationID != firstAlloc.AllocationID {
		t.Fatalf("subsequent claim with same idempKey must return exact same allocation, got %+v, err=%v", repeatAlloc, err)
	}
}

// D02: 100 个并发不同幂等请求不会重复认领同一别名
func TestPR03_D02_ConcurrentDifferentClaimsNoDuplicate(t *testing.T) {
	tempDir := t.TempDir()
	st, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	insertTestAccount(t, st, "acc_1")

	const poolSize = 30
	for i := 0; i < poolSize; i++ {
		email := fmt.Sprintf("pool_%d@icloud.com", i)
		_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: email, Active: true}, "replenish", true)
	}

	ctx := context.Background()
	const concurrency = 100
	var wg sync.WaitGroup
	wg.Add(concurrency)

	claimedEmails := make([]string, concurrency)

	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()
			idempKey := fmt.Sprintf("idemp_diff_%d", idx)
			alloc, _, err := st.ClaimInventoryAlias(ctx, "token", fmt.Sprintf("tok_%d", idx), "allocate", idempKey, "hash", "tag", nil)
			if err == nil && alloc != nil {
				claimedEmails[idx] = alloc.AliasEmail
			}
		}(i)
	}

	wg.Wait()

	seen := make(map[string]bool)
	successCount := 0
	for _, email := range claimedEmails {
		if email != "" {
			if seen[email] {
				t.Fatalf("CRITICAL: duplicate allocation detected for email: %s", email)
			}
			seen[email] = true
			successCount++
		}
	}

	if successCount != poolSize {
		t.Fatalf("expected exactly %d successful claims, got %d", poolSize, successCount)
	}
}

// D03: 两个独立 Store 连接访问同一数据库文件，依然具备防重能力
func TestPR03_D03_TwoIndependentStoreConnections(t *testing.T) {
	tempDir := t.TempDir()
	st1, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("store 1 init failed: %v", err)
	}
	defer st1.Close()

	insertTestAccount(t, st1, "acc_1")

	// 存入仅有的一封可用别名
	_ = st1.AddInventoryAlias("acc_1", hme.Alias{Email: "single_stock@icloud.com", Active: true}, "replenish", true)

	// 创建第二个独立的 Store 实例连接同一个 SQLite 文件
	st2, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("store 2 init failed: %v", err)
	}
	defer st2.Close()

	ctx := context.Background()
	var wg sync.WaitGroup
	wg.Add(2)

	var alloc1, alloc2 *AliasAllocation
	var err1, err2 error

	go func() {
		defer wg.Done()
		alloc1, _, err1 = st1.ClaimInventoryAlias(ctx, "token", "tok_conn1", "allocate", "key_conn1", "h1", "tag", nil)
	}()

	go func() {
		defer wg.Done()
		alloc2, _, err2 = st2.ClaimInventoryAlias(ctx, "token", "tok_conn2", "allocate", "key_conn2", "h2", "tag", nil)
	}()

	wg.Wait()

	// 必须且仅有一个成功，另一个必须失败为 ErrNoAvailableInventory
	successes := 0
	if err1 == nil && alloc1 != nil {
		successes++
	}
	if err2 == nil && alloc2 != nil {
		successes++
	}

	if successes != 1 {
		t.Fatalf("expected exactly 1 success between independent store connections, got %d (err1=%v, err2=%v)", successes, err1, err2)
	}
}

// D04: 令牌改名或删除不影响库存的已分配状态
func TestPR03_D04_TokenRenameDeleteDoesNotResetInventory(t *testing.T) {
	tempDir := t.TempDir()
	st, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	insertTestAccount(t, st, "acc_1")

	tok := APIToken{ID: NewAPITokenID(), Name: "OriginalName", Token: "tok_secret_123", Scopes: DefaultExternalScopes}
	_ = st.SaveToken(tok)
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "d04@icloud.com", Active: true}, "replenish", true)

	alloc, _, err := st.ClaimInventoryAlias(context.Background(), "token", tok.ID, "allocate", "k_d04", "h", "tag", nil)
	if err != nil || alloc.AliasEmail != "d04@icloud.com" {
		t.Fatalf("initial claim failed: %v", err)
	}

	// 1. 令牌改名
	tok.Name = "RenamedToken"
	_ = st.SaveToken(tok)
	// 2. 验证库存仍然为 allocated，绝不可被再次认领
	_, _, err = st.ClaimInventoryAlias(context.Background(), "token", "another_tok", "allocate", "k_diff", "h", "tag", nil)
	if !errors.Is(err, ErrNoAvailableInventory) {
		t.Fatalf("inventory must remain allocated after token rename, got: %v", err)
	}

	// 3. 删除令牌
	_, _ = st.DeleteToken(tok.ID)
	// 4. 验证库存仍然为 allocated
	_, _, err = st.ClaimInventoryAlias(context.Background(), "token", "another_tok", "allocate", "k_diff2", "h", "tag", nil)
	if !errors.Is(err, ErrNoAvailableInventory) {
		t.Fatalf("inventory must remain allocated after token deletion, got: %v", err)
	}
}

// D05: 清理 lease_records 不使已分配别名再次可用
func TestPR03_D05_PruningLeaseRecordsDoesNotResetInventory(t *testing.T) {
	tempDir := t.TempDir()
	st, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	insertTestAccount(t, st, "acc_1")

	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "d05@icloud.com", Active: true}, "replenish", true)
	_, _, err = st.ClaimInventoryAlias(context.Background(), "token", "tok_d05", "allocate", "k_d05", "h", "tag", nil)
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}

	// 模拟旧代码物理清空 lease_records 审计历史
	_, _ = st.db.Exec("DELETE FROM lease_records")

	// 确认即使 lease_records 为空，alias_inventory 的 allocated 状态仍永久存在！
	_, _, err = st.ClaimInventoryAlias(context.Background(), "token", "tok_new", "allocate", "k_new", "h", "tag", nil)
	if !errors.Is(err, ErrNoAvailableInventory) {
		t.Fatalf("pruning lease_records must NOT reset inventory to available, got err: %v", err)
	}
}

// D06: 相同幂等键不同参数返回 409 冲突
func TestPR03_D06_IdempotencyConflictOnDifferentHash(t *testing.T) {
	tempDir := t.TempDir()
	st, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	insertTestAccount(t, st, "acc_1")

	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "d06@icloud.com", Active: true}, "replenish", true)

	// 第一次调用: hash1
	_, _, err = st.ClaimInventoryAlias(context.Background(), "token", "tok_d06", "allocate", "key_d06", "hash1", "tag", nil)
	if err != nil {
		t.Fatalf("first claim failed: %v", err)
	}

	// 第二次调用: 相同 key 但 hash2
	_, _, err = st.ClaimInventoryAlias(context.Background(), "token", "tok_d06", "allocate", "key_d06", "hash2", "tag", nil)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("expected ErrIdempotencyConflict, got: %v", err)
	}
}

// D07: 迁移历史未知别名保持 unknown，不可被认领
func TestPR03_D07_MigrationUnknownRemainsQuarantined(t *testing.T) {
	tempDir := t.TempDir()
	st, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("init store failed: %v", err)
	}

	// 模拟旧系统仅有 alias_routes
	_, _ = st.db.Exec("INSERT INTO alias_routes (email, account_id, updated_at) VALUES ('legacy_route@icloud.com', 'acc_1', '2026-09-20T00:00:00Z')")
	_ = st.migrateInventory()

	// 尝试认领此历史别名，必须返回无库存，因为其 allocation_state 必须为 unknown
	_, _, err = st.ClaimInventoryAlias(context.Background(), "token", "tok_1", "allocate", "key_legacy", "h", "tag", nil)
	if !errors.Is(err, ErrNoAvailableInventory) {
		t.Fatalf("legacy route without allocation history must NOT be available, got: %v", err)
	}
	st.Close()

	// 重新打开 Store，验证重启后重入迁移幂等安全
	stReopen, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("reopen store failed: %v", err)
	}
	defer stReopen.Close()

	_, _, err = stReopen.ClaimInventoryAlias(context.Background(), "token", "tok_1", "allocate", "key_legacy2", "h", "tag", nil)
	if !errors.Is(err, ErrNoAvailableInventory) {
		t.Fatalf("reopened store must still preserve quarantined state, got: %v", err)
	}
}

// D08: Apple 远端同步 active 不会把 allocated 覆盖回 available
func TestPR03_D08_RemoteSyncDoesNotOverwriteAllocated(t *testing.T) {
	tempDir := t.TempDir()
	st, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	insertTestAccount(t, st, "acc_1")

	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "d08@icloud.com", Active: true}, "replenish", true)
	_, _, err = st.ClaimInventoryAlias(context.Background(), "token", "tok_d08", "allocate", "k_d08", "h", "tag", nil)
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}

	// 模拟从 Apple 同步到该别名当前仍然为 Active
	err = st.SyncAliasInventory("acc_1", []hme.Alias{
		{Email: "d08@icloud.com", AnonymousID: "anon_d08", Active: true},
	})
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	// 校验 allocation_state 仍然是 allocated，绝不变成 available
	var allocState, remoteState string
	_ = st.db.QueryRow("SELECT allocation_state, remote_state FROM alias_inventory WHERE email = 'd08@icloud.com'").Scan(&allocState, &remoteState)
	if allocState != "allocated" {
		t.Fatalf("expected allocation_state to remain allocated, got %s", allocState)
	}
	if remoteState != "active" {
		t.Fatalf("expected remote_state to be active, got %s", remoteState)
	}

	// 再次认领必须依然无库存
	_, _, err = st.ClaimInventoryAlias(context.Background(), "token", "tok_other", "allocate", "k_other", "h", "tag", nil)
	if !errors.Is(err, ErrNoAvailableInventory) {
		t.Fatalf("expected ErrNoAvailableInventory after sync, got: %v", err)
	}
}

// ============================================================================
// PR-08 Final Hardening §4: VerificationRequest 创建原子操作并发压力测试
// ============================================================================

// 1. 100 并发抢同一个 lease，恰好 1 个成功，99 个返回 conflict
func TestPR08_AtomicVerificationRequest_100ConcurrentSameLease(t *testing.T) {
	tempDir := t.TempDir()
	st, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	leaseID := "lease_same_100"
	const concurrency = 100

	var wg sync.WaitGroup
	wg.Add(concurrency)

	successCount := int32(0)
	conflictCount := int32(0)

	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()
			req := &VerificationRequest{
				RequestID:     fmt.Sprintf("vreq_lease_%d", idx),
				LeaseID:       leaseID,
				AliasEmail:    "test@icloud.com",
				PrincipalKind: "token",
				PrincipalID:   "tok_test",
				Status:        "pending",
				ExpiresAt:     time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339),
			}
			err := st.CreateVerificationRequestAtomic(ctx, req, 1000, 50)
			if err == nil {
				atomic.AddInt32(&successCount, 1)
			} else if errors.Is(err, ErrConflictActiveRequest) {
				atomic.AddInt32(&conflictCount, 1)
			}
		}(i)
	}
	wg.Wait()

	if successCount != 1 {
		t.Fatalf("expected exactly 1 success, got %d", successCount)
	}
	if conflictCount != 99 {
		t.Fatalf("expected 99 conflict errors, got %d", conflictCount)
	}
}

// 2. 49 active 状态下并发 100 个同 token 请求，成功数加上已有 active 严格 <= 50
func TestPR08_AtomicVerificationRequest_PerTokenLimit(t *testing.T) {
	tempDir := t.TempDir()
	st, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	principalID := "tok_limited_user"

	// 先插入 49 个活跃请求 (每个 lease 独立)
	for i := 0; i < 49; i++ {
		req := &VerificationRequest{
			RequestID:     fmt.Sprintf("vreq_pre_%d", i),
			LeaseID:       fmt.Sprintf("lease_pre_%d", i),
			AliasEmail:    fmt.Sprintf("test%d@icloud.com", i),
			PrincipalKind: "token",
			PrincipalID:   principalID,
			Status:        "pending",
			ExpiresAt:     time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339),
		}
		if err := st.CreateVerificationRequestAtomic(ctx, req, 1000, 50); err != nil {
			t.Fatalf("pre-insert failed at %d: %v", i, err)
		}
	}

	// 49 active 状态下并发 100 个同 token 请求 (每个 lease 独立)
	const concurrency = 100
	var wg sync.WaitGroup
	wg.Add(concurrency)

	successCount := int32(0)
	tooManyCount := int32(0)

	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()
			req := &VerificationRequest{
				RequestID:     fmt.Sprintf("vreq_conc_%d", idx),
				LeaseID:       fmt.Sprintf("lease_conc_%d", idx),
				AliasEmail:    fmt.Sprintf("test_conc_%d@icloud.com", idx),
				PrincipalKind: "token",
				PrincipalID:   principalID,
				Status:        "pending",
				ExpiresAt:     time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339),
			}
			err := st.CreateVerificationRequestAtomic(ctx, req, 1000, 50)
			if err == nil {
				atomic.AddInt32(&successCount, 1)
			} else if errors.Is(err, ErrTooManyRequests) {
				atomic.AddInt32(&tooManyCount, 1)
			}
		}(i)
	}
	wg.Wait()

	totalActive := 49 + int(successCount)
	if totalActive > 50 {
		t.Fatalf("total active exceeded limit: expected <= 50, got %d (successCount=%d)", totalActive, successCount)
	}
	if successCount != 1 {
		t.Fatalf("expected exactly 1 success out of 100 (49+1=50), got %d", successCount)
	}
	if tooManyCount != 99 {
		t.Fatalf("expected 99 ErrTooManyRequests, got %d", tooManyCount)
	}
}

// 3. 999 global active 状态下并发请求，最终 global active 严格 <= 1000
func TestPR08_AtomicVerificationRequest_GlobalLimit(t *testing.T) {
	tempDir := t.TempDir()
	st, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	maxGlobal := 1000

	// 批量准备 999 个 global active
	nowStr := time.Now().UTC().Format(time.RFC3339)
	expStr := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
	tx, err := st.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO verification_requests (
		request_id, lease_id, alias_email, principal_kind, principal_id,
		status, expires_at, created_at
	) VALUES (?, ?, ?, 'token', ?, 'pending', ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 999; i++ {
		pID := fmt.Sprintf("tok_g_%d", i)
		_, err := stmt.Exec(fmt.Sprintf("vreq_g_%d", i), fmt.Sprintf("lease_g_%d", i), fmt.Sprintf("g%d@icloud.com", i), pID, expStr, nowStr)
		if err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// 999 global active 状态下并发 20 个不同 principal 的请求
	const concurrency = 20
	var wg sync.WaitGroup
	wg.Add(concurrency)

	successCount := int32(0)
	busyCount := int32(0)

	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()
			req := &VerificationRequest{
				RequestID:     fmt.Sprintf("vreq_g_conc_%d", idx),
				LeaseID:       fmt.Sprintf("lease_g_conc_%d", idx),
				AliasEmail:    fmt.Sprintf("g_conc_%d@icloud.com", idx),
				PrincipalKind: "token",
				PrincipalID:   fmt.Sprintf("tok_diff_%d", idx),
				Status:        "pending",
				ExpiresAt:     time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339),
			}
			err := st.CreateVerificationRequestAtomic(ctx, req, maxGlobal, 50)
			if err == nil {
				atomic.AddInt32(&successCount, 1)
			} else if errors.Is(err, ErrServerBusy) {
				atomic.AddInt32(&busyCount, 1)
			}
		}(i)
	}
	wg.Wait()

	totalGlobal := 999 + int(successCount)
	if totalGlobal > maxGlobal {
		t.Fatalf("total global exceeded limit: expected <= %d, got %d", maxGlobal, totalGlobal)
	}
	if successCount != 1 {
		t.Fatalf("expected exactly 1 success (999+1=1000), got %d", successCount)
	}
	if busyCount != 19 {
		t.Fatalf("expected 19 ErrServerBusy, got %d", busyCount)
	}
}

// ============================================================================
// P0-3: Verification 成功 CAS 必须原子包含 expires_at 判定
// ============================================================================

func TestVerificationCAS_CannotSucceedAfterExpiry(t *testing.T) {
	// VERIFY-CAS-01: expires_at = now - 1ms, status=ready -> CompleteVerificationRequest -> won=false -> 最终不是 succeeded
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	reqID := "vreq_cas_expired"

	vreq := &VerificationRequest{
		RequestID:           reqID,
		PrincipalKind:       "token",
		PrincipalID:         "tok_cas",
		LeaseID:             "lease_cas",
		AliasEmail:          "cas_exp@icloud.com",
		Status:              "ready",
		CreatedAt:           now.Add(-10 * time.Minute).Format(time.RFC3339),
		ExpiresAt:           now.Add(-1 * time.Millisecond).Format(time.RFC3339), // 已过期 1ms
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1,
		BaselineUID:         100,
	}
	if err := st.CreateVerificationRequest(ctx, vreq); err != nil {
		t.Fatal(err)
	}

	// 尝试 Complete (由于当前尚未修复，expires_at 不在 WHERE 条件中，会导致 won=true 且变成 succeeded)
	curReq, won, err := st.CompleteVerificationRequest(ctx, reqID, "123456", "ref_100")
	if err != nil {
		t.Fatalf("CompleteVerificationRequest unexpected err: %v", err)
	}
	if won {
		t.Fatalf("VERIFY-CAS-01 失败: 已过 expires_at 的请求绝不能 won=true")
	}
	if curReq == nil || curReq.Status == "succeeded" {
		t.Fatalf("VERIFY-CAS-01 失败: 已过期的请求状态绝不能变迁为 succeeded, 实际: %+v", curReq)
	}
}

func TestVerificationCAS_ConcurrentExpiryVsSuccess(t *testing.T) {
	// VERIFY-CAS-02: expiry 与 OTP completion 并发竞争，最终只能是 succeeded 或 expired，绝不能有非法中间态或逾期 succeeded
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()

	for round := 0; round < 30; round++ {
		now := time.Now().UTC()
		reqID := fmt.Sprintf("vreq_race_%d", round)
		// 设置极短的过期时间 (2ms)
		vreq := &VerificationRequest{
			RequestID:           reqID,
			PrincipalKind:       "token",
			PrincipalID:         "tok_race",
			LeaseID:             fmt.Sprintf("lease_race_%d", round),
			AliasEmail:          fmt.Sprintf("race_%d@icloud.com", round),
			Status:              "ready",
			CreatedAt:           now.Format(time.RFC3339),
			ExpiresAt:           now.Add(2 * time.Millisecond).Format(time.RFC3339),
			BaselineProvider:    "imap",
			BaselineMailbox:     "INBOX",
			BaselineUIDValidity: 1,
			BaselineUID:         100,
		}
		if err := st.CreateVerificationRequest(ctx, vreq); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			time.Sleep(1 * time.Millisecond)
			_, _, _ = st.ExpireVerificationRequest(ctx, reqID)
		}()

		go func() {
			defer wg.Done()
			time.Sleep(1 * time.Millisecond)
			_, _, _ = st.CompleteVerificationRequest(ctx, reqID, "666888", "ref_race")
		}()

		wg.Wait()

		finalReq, err := st.GetVerificationRequest(ctx, reqID, "token", "tok_race")
		if err != nil {
			t.Fatalf("round %d: get req failed: %v", round, err)
		}
		if finalReq.Status != "succeeded" && finalReq.Status != "expired" {
			t.Fatalf("round %d: 终态必须是 succeeded 或 expired, 实际: %s", round, finalReq.Status)
		}
		// 若为 succeeded，则再次尝试 Expire 必须无法修改终态
		if finalReq.Status == "succeeded" {
			_, won, _ := st.ExpireVerificationRequest(ctx, reqID)
			if won {
				t.Fatalf("round %d: succeeded 终态绝不可被修改为 expired", round)
			}
		}
		// 若为 expired，则再次尝试 Complete 必须无法修改终态
		if finalReq.Status == "expired" {
			_, won, _ := st.CompleteVerificationRequest(ctx, reqID, "999999", "ref_late")
			if won {
				t.Fatalf("round %d: expired 终态绝不可被修改为 succeeded", round)
			}
		}
	}
}

func TestQuarantineInventoryForAccountOnDeletion(t *testing.T) {
	tempDir := t.TempDir()
	st, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	insertTestAccount(t, st, "acc_del")
	insertTestAccount(t, st, "acc_keep")

	// 添加两个账号的可用库存
	_ = st.AddInventoryAlias("acc_del", hme.Alias{Email: "del_1@icloud.com", Active: true}, "replenish", true)
	_ = st.AddInventoryAlias("acc_del", hme.Alias{Email: "del_2@icloud.com", Active: true}, "replenish", true)
	_ = st.AddInventoryAlias("acc_keep", hme.Alias{Email: "keep_1@icloud.com", Active: true}, "replenish", true)

	if cnt := st.CountAuthoritativeAvailableAliases(); cnt != 3 {
		t.Fatalf("初始可用库存期望为 3, 实际为 %d", cnt)
	}

	// 模拟删除 acc_del
	if err := st.DeleteAccount("acc_del"); err != nil {
		t.Fatalf("DeleteAccount 失败: %v", err)
	}

	// 确认可用库存已被级联隔离，仅剩 acc_keep 的 1 个
	if cnt := st.CountAuthoritativeAvailableAliases(); cnt != 1 {
		t.Fatalf("删除账号后可用库存期望为 1, 实际为 %d", cnt)
	}

	// 确认认领出号只会领到 keep_1，绝不会领到 del_1 或 del_2
	alloc, _, err := st.ClaimInventoryAlias(context.Background(), "token", "tok_1", "claim", "", "", "", nil)
	if err != nil {
		t.Fatalf("ClaimInventoryAlias 失败: %v", err)
	}
	if alloc.AliasEmail != "keep_1@icloud.com" {
		t.Fatalf("期望认领到 keep_1@icloud.com, 实际: %s", alloc.AliasEmail)
	}

	// 再次认领应当返回库存为空
	_, _, err = st.ClaimInventoryAlias(context.Background(), "token", "tok_2", "claim", "", "", "", nil)
	if !errors.Is(err, ErrNoAvailableInventory) {
		t.Fatalf("期望 ErrNoAvailableInventory, 实际得到: %v", err)
	}
}

// 针对 UpdateAliasRemoteState 的身份强校验与事务回滚确定性测试
func TestUpdateAliasRemoteState_IdentityValidationAndRollback(t *testing.T) {
	tempDir := t.TempDir()
	st, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	insertTestAccount(t, st, "acc_1")
	insertTestAccount(t, st, "acc_2")

	// 1. 测试用例 1: id-a 与 email-b 指向两条库存 -> 返回冲突，而且两条原记录都不变
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "email_a@icloud.com", AnonymousID: "id_a", Active: true}, "replenish", true)
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "email_b@icloud.com", AnonymousID: "id_b", Active: true}, "replenish", true)

	err1 := st.UpdateAliasRemoteState("acc_1", "id_a", "email_b@icloud.com", RemoteInactive)
	if err1 == nil {
		t.Fatal("期望 id-a 与 email-b 冲突报错，但返回了 nil")
	}
	if !strings.Contains(err1.Error(), "conflict") {
		t.Fatalf("期望冲突错误包含 conflict, 实际: %v", err1)
	}

	invA, _ := st.GetInventoryAlias("email_a@icloud.com")
	invB, _ := st.GetInventoryAlias("email_b@icloud.com")
	if invA.RemoteState != RemoteActive || invA.ProviderAliasID != "id_a" {
		t.Fatalf("email_a 原记录被意外篡改: %+v", invA)
	}
	if invB.RemoteState != RemoteActive || invB.ProviderAliasID != "id_b" {
		t.Fatalf("email_b 原记录被意外篡改: %+v", invB)
	}

	// 2. 测试用例 2: email-a 存在，但输入 provider ID 与数据库不一致 -> 返回冲突，原记录不变
	err2 := st.UpdateAliasRemoteState("acc_1", "id_diff", "email_a@icloud.com", RemoteInactive)
	if err2 == nil {
		t.Fatal("期望 provider ID 不一致报错，但返回了 nil")
	}
	if !strings.Contains(err2.Error(), "conflict") {
		t.Fatalf("期望错误包含 conflict, 实际: %v", err2)
	}
	invA2, _ := st.GetInventoryAlias("email_a@icloud.com")
	if invA2.RemoteState != RemoteActive || invA2.ProviderAliasID != "id_a" {
		t.Fatalf("email_a 原记录在冲突后被篡改: %+v", invA2)
	}

	// 3. 测试用例 3: 同邮箱、不同 account -> 拒绝修改
	err3 := st.UpdateAliasRemoteState("acc_2", "id_a", "email_a@icloud.com", RemoteInactive)
	if err3 == nil {
		t.Fatal("期望账号不匹配报错，但返回了 nil")
	}
	if !strings.Contains(err3.Error(), "account mismatch") {
		t.Fatalf("期望错误包含 account mismatch, 实际: %v", err3)
	}
	invA3, _ := st.GetInventoryAlias("email_a@icloud.com")
	if invA3.RemoteState != RemoteActive || invA3.AccountID != "acc_1" {
		t.Fatalf("email_a 账号归属被意外篡改: %+v", invA3)
	}

	// 4. 测试用例 5: 历史 provider ID 为空、可靠映射成功 -> 只更新目标邮箱，其他资产不变
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "email_empty_id@icloud.com", AnonymousID: "", Active: true}, "replenish", true)
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "email_untouched@icloud.com", AnonymousID: "id_untouched", Active: true}, "replenish", true)

	err5 := st.UpdateAliasRemoteState("acc_1", "id_newly_resolved", "email_empty_id@icloud.com", RemoteInactive)
	if err5 != nil {
		t.Fatalf("历史 provider ID 为空时回填更新失败: %v", err5)
	}

	invTarget, _ := st.GetInventoryAlias("email_empty_id@icloud.com")
	if invTarget.RemoteState != RemoteInactive || invTarget.ProviderAliasID != "id_newly_resolved" {
		t.Fatalf("email_empty_id 未正确回填或更新: %+v", invTarget)
	}

	invUntouched, _ := st.GetInventoryAlias("email_untouched@icloud.com")
	if invUntouched.RemoteState != RemoteActive || invUntouched.ProviderAliasID != "id_untouched" {
		t.Fatalf("对照资产 email_untouched 被意外修改: %+v", invUntouched)
	}
}



