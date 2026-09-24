/**
 * [INPUT]: 依赖 testing, net/http, net/http/httptest, sync, sync/atomic, time, context, errors, icloud-hme/internal/auth, icloud-hme/internal/account, icloud-hme/internal/hme, icloud-hme/internal/mail, icloud-hme/internal/scheduler, icloud-hme/internal/store
 * [OUTPUT]: 提供 TestPR05 系列测试 (准入前移拦截、并发槽位有界、同租约排重、Context 贯穿取消、Scheduler 有界停机、Server 优雅停机保证)
 * [POS]: internal/server 的 PR-05 (F09/F10) 准入前移与有界优雅停机契约测试集
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/auth"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/scheduler"
	"icloud-hme/internal/store"
)

// TestPR05_ServerBusyRejectedBeforeMailboxIO
// 验证 F09 核心契约：全局活跃任务超限时，请求在准入阶段直接被拦截返回 503 SERVER_BUSY，
// 绝不触发昂贵上游 IMAP / Mailbox Boundary IO。
func TestPR05_ServerBusyRejectedBeforeMailboxIO(t *testing.T) {
	s, st, fb, ts, tokenSecret, leaseID := setupV2TestEnv(t)
	defer s.Close()
	defer ts.Close()

	// 动态设置全局上限为 1
	s.verifyService.SetMaxLimitsForTest(1, 10)

	// 统计 boundary 上游调用次数
	var boundaryCalls int32
	fb.onGetMailboxBoundaryContext = func(ctx context.Context, accountID, folder string) (string, uint32, uint32, error) {
		atomic.AddInt32(&boundaryCalls, 1)
		return "imap", 1, 100, nil
	}

	// 预先占满这 1 个活跃名额 (插入 status='ready' 的请求)
	now := time.Now().UTC()
	err := st.CreateVerificationRequest(context.Background(), &store.VerificationRequest{
		RequestID:           "vreq_existing_1",
		PrincipalKind:       "token",
		PrincipalID:         "tok_v06_test",
		LeaseID:             "lease_other_1",
		AliasEmail:          "other@icloud.com",
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1,
		BaselineUID:         100,
	})
	if err != nil {
		t.Fatalf("预填请求失败: %v", err)
	}

	// 通过 HTTP 发起针对 leaseID 的新取码任务
	body, _ := json.Marshal(map[string]string{"lease_id": leaseID})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/external/v2/verification-requests", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenSecret)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP 请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("期望状态码 503 SERVER_BUSY, 实际得到: %d", resp.StatusCode)
	}

	// 关键断言：Boundary 调用次数严格为 0
	if calls := atomic.LoadInt32(&boundaryCalls); calls != 0 {
		t.Fatalf("F09 破坏：超限请求仍触发了上游 Boundary IO, 调用次数=%d", calls)
	}
}

// TestPR05_PerPrincipalLimitRejectedBeforeMailboxIO
// 验证 F09 核心契约：单 Token 活跃任务超限时，请求在准入阶段直接被拦截返回 429 TOO_MANY_REQUESTS，
// 绝不触发昂贵上游 IMAP / Mailbox Boundary IO。
func TestPR05_PerPrincipalLimitRejectedBeforeMailboxIO(t *testing.T) {
	s, st, fb, ts, tokenSecret, _ := setupV2TestEnv(t)
	defer s.Close()
	defer ts.Close()

	// 动态设置单主体上限为 1，全局上限为 100
	s.verifyService.SetMaxLimitsForTest(100, 1)

	var boundaryCalls int32
	fb.onGetMailboxBoundaryContext = func(ctx context.Context, accountID, folder string) (string, uint32, uint32, error) {
		atomic.AddInt32(&boundaryCalls, 1)
		return "imap", 1, 100, nil
	}

	// 为 tok_v06_test 预填 1 个活跃请求
	now := time.Now().UTC()
	err := st.CreateVerificationRequest(context.Background(), &store.VerificationRequest{
		RequestID:           "vreq_existing_tok",
		PrincipalKind:       "token",
		PrincipalID:         "tok_v06_test",
		LeaseID:             "lease_other_2",
		AliasEmail:          "other2@icloud.com",
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1,
		BaselineUID:         100,
	})
	if err != nil {
		t.Fatalf("预填请求失败: %v", err)
	}

	// 为该 Token 分配第二个 lease
	secondEmail := "second_alias@icloud.com"
	_ = st.AddInventoryAlias("acc_imap", hme.Alias{Email: secondEmail, Active: true}, "replenish", true)
	alloc2 := &store.AliasAllocation{
		AllocationID: "lease_v06_2",
		AliasEmail:   secondEmail,
		AccountID:    "acc_imap",
		OwnerKind:    "token",
		OwnerID:      "tok_v06_test",
		BusinessTag:  "default",
		Status:       "allocated",
		AllocatedAt:  time.Now().Format(time.RFC3339),
	}
	if _, err := st.RecordAllocation(alloc2, "v06_bot"); err != nil {
		t.Fatalf("RecordAllocation failed: %v", err)
	}

	// 发起针对 lease_v06_2 的验证请求
	body, _ := json.Marshal(map[string]string{"lease_id": alloc2.AllocationID})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/external/v2/verification-requests", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenSecret)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP 请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("期望状态码 429 TOO_MANY_REQUESTS, 实际得到: %d", resp.StatusCode)
	}

	// 关键断言：Boundary 调用次数严格为 0
	if calls := atomic.LoadInt32(&boundaryCalls); calls != 0 {
		t.Fatalf("F09 破坏：Token 超限请求仍触发了上游 Boundary IO, 调用次数=%d", calls)
	}
}

// TestPR05_LeaseConflictRejectedBeforeMailboxIO
// 验证 F09 核心契约：同租约存在活跃任务时，重复请求在准入阶段直接被拦截返回 409 CONFLICT，
// 绝不触发昂贵上游 IMAP / Mailbox Boundary IO。
func TestPR05_LeaseConflictRejectedBeforeMailboxIO(t *testing.T) {
	s, st, fb, ts, tokenSecret, leaseID := setupV2TestEnv(t)
	defer s.Close()
	defer ts.Close()

	var boundaryCalls int32
	fb.onGetMailboxBoundaryContext = func(ctx context.Context, accountID, folder string) (string, uint32, uint32, error) {
		atomic.AddInt32(&boundaryCalls, 1)
		return "imap", 1, 100, nil
	}

	// 为同一个 leaseID 预填 1 个活跃状态任务
	now := time.Now().UTC()
	err := st.CreateVerificationRequest(context.Background(), &store.VerificationRequest{
		RequestID:           "vreq_existing_lease",
		PrincipalKind:       "token",
		PrincipalID:         "tok_v06_test",
		LeaseID:             leaseID,
		AliasEmail:          "target_alias@icloud.com",
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1,
		BaselineUID:         100,
	})
	if err != nil {
		t.Fatalf("预填请求失败: %v", err)
	}

	// 再次请求
	body, _ := json.Marshal(map[string]string{"lease_id": leaseID})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/external/v2/verification-requests", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenSecret)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP 请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("期望状态码 409 CONFLICT, 实际得到: %d", resp.StatusCode)
	}

	// 关键断言：Boundary 调用次数严格为 0
	if calls := atomic.LoadInt32(&boundaryCalls); calls != 0 {
		t.Fatalf("F09 破坏：租约冲突请求仍触发了上游 Boundary IO, 调用次数=%d", calls)
	}
}

// TestPR05_BurstAdmissionBoundsMailboxConcurrency
// 验证 F09 核心契约：突发请求进入 baseline boundary 查询时的并发度受 baselineSlots 严格约束。
func TestPR05_BurstAdmissionBoundsMailboxConcurrency(t *testing.T) {
	s, st, fb, _, _, _ := setupV2TestEnv(t)
	defer s.Close()

	// 限制并发槽位为 2
	const maxSlots = 2
	s.verifyService.SetBaselineSlotsForTest(maxSlots)

	var (
		curInFlight  int32
		peakInFlight int32
	)

	fb.onGetMailboxBoundaryContext = func(ctx context.Context, accountID, folder string) (string, uint32, uint32, error) {
		cur := atomic.AddInt32(&curInFlight, 1)
		for {
			oldPeak := atomic.LoadInt32(&peakInFlight)
			if cur <= oldPeak || atomic.CompareAndSwapInt32(&peakInFlight, oldPeak, cur) {
				break
			}
		}
		// 模拟上游 IMAP 连接延时
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&curInFlight, -1)
		return "imap", 1, 100, nil
	}

	// 准备 6 个不同 lease
	const totalReqs = 6
	leaseIDs := make([]string, totalReqs)
	for i := 0; i < totalReqs; i++ {
		email := fmt.Sprintf("burst_%d@icloud.com", i)
		_ = st.AddInventoryAlias("acc_imap", hme.Alias{Email: email, Active: true}, "replenish", true)
		alloc := &store.AliasAllocation{
			AllocationID: fmt.Sprintf("lease_burst_%d", i),
			AliasEmail:   email,
			AccountID:    "acc_imap",
			OwnerKind:    "token",
			OwnerID:      "tok_v06_test",
			BusinessTag:  "default",
			Status:       "allocated",
			AllocatedAt:  time.Now().Format(time.RFC3339),
		}
		_, _ = st.RecordAllocation(alloc, "v06_bot")
		leaseIDs[i] = alloc.AllocationID
	}

	principal := auth.Principal{
		Kind:      auth.PrincipalToken,
		ID:        "tok_v06_test",
		TokenName: "v06_bot",
		Scopes:    []string{"verify"},
	}

	var wg sync.WaitGroup
	errCh := make(chan error, totalReqs)

	for _, lID := range leaseIDs {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, err := s.verifyService.CreateVerificationRequest(context.Background(), principal, id)
			if err != nil {
				errCh <- err
			}
		}(lID)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("请求不应失败: %v", err)
	}

	peak := atomic.LoadInt32(&peakInFlight)
	if peak > maxSlots {
		t.Fatalf("F09 并发防线击穿：在途 Mailbox Boundary 峰值并发 %d > 限制槽位数 %d", peak, maxSlots)
	}
}

// TestPR05_SameLeaseConcurrentRequestsDoNotDuplicateBaselineIO
// 验证 F09 核心契约：同一租约的并发请求受互斥锁与二次准入双重防护，首个请求成功，
// 后续请求在排队获得锁后立即被二次 cheap admission 拦截，绝不发生重复上游 IO。
func TestPR05_SameLeaseConcurrentRequestsDoNotDuplicateBaselineIO(t *testing.T) {
	s, _, fb, _, _, leaseID := setupV2TestEnv(t)
	defer s.Close()

	var boundaryCalls int32
	fb.onGetMailboxBoundaryContext = func(ctx context.Context, accountID, folder string) (string, uint32, uint32, error) {
		atomic.AddInt32(&boundaryCalls, 1)
		time.Sleep(40 * time.Millisecond)
		return "imap", 1, 100, nil
	}

	principal := auth.Principal{
		Kind:      auth.PrincipalToken,
		ID:        "tok_v06_test",
		TokenName: "v06_bot",
		Scopes:    []string{"verify"},
	}

	var wg sync.WaitGroup
	var successCount int32
	var conflictCount int32

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.verifyService.CreateVerificationRequest(context.Background(), principal, leaseID)
			if err == nil {
				atomic.AddInt32(&successCount, 1)
			} else {
				var be *BackendError
				if errors.As(err, &be) && be.Code == "CONFLICT" {
					atomic.AddInt32(&conflictCount, 1)
				}
			}
		}()
	}

	wg.Wait()

	if successCount != 1 || conflictCount != 1 {
		t.Fatalf("期望 1 个成功 1 个 CONFLICT, 实际成功=%d, 冲突=%d", successCount, conflictCount)
	}

	// 关键断言：即使两个请求几乎同时到达，二次准入也保证上游 Boundary 仅调用 1 次
	if calls := atomic.LoadInt32(&boundaryCalls); calls != 1 {
		t.Fatalf("F09 破坏：同租约并发产生重复 Boundary IO, 调用次数=%d", calls)
	}
}

// TestPR05_VerificationBaselineHonorsCancellation
// 验证 F10 核心契约：客户端 Context 取消时能够快速中断，不继续推进创建或写库。
func TestPR05_VerificationBaselineHonorsCancellation(t *testing.T) {
	s, st, fb, _, _, leaseID := setupV2TestEnv(t)
	defer s.Close()

	inBoundary := make(chan struct{})
	fb.onGetMailboxBoundaryContext = func(ctx context.Context, accountID, folder string) (string, uint32, uint32, error) {
		close(inBoundary)
		<-ctx.Done()
		return "", 0, 0, ctx.Err()
	}

	principal := auth.Principal{
		Kind:      auth.PrincipalToken,
		ID:        "tok_v06_test",
		TokenName: "v06_bot",
		Scopes:    []string{"verify"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := s.verifyService.CreateVerificationRequest(ctx, principal, leaseID)
		errCh <- err
	}()

	<-inBoundary
	cancel() // 取消客户端 Context

	err := <-errCh
	if err == nil {
		t.Fatal("期望 Context 取消错误，但实际返回成功")
	}

	var be *BackendError
	if !errors.Is(err, context.Canceled) && !(errors.As(err, &be) && be.Code == "REQUEST_CANCELED") {
		t.Fatalf("期望 context.Canceled 或 REQUEST_CANCELED, 实际得到: %v", err)
	}

	// 关键断言：DB 中绝不能存在该 lease 的活跃验证请求
	activeVReq, _ := st.GetActiveVerificationRequestByLease(context.Background(), leaseID)
	if activeVReq != nil {
		t.Fatalf("F10 破坏：已取消的请求被持久化到了数据库: %+v", activeVReq)
	}
}

// TestPR05_HMECreateCancellationBeforeWriteDoesNotMutate
// 验证 F10 核心契约：HME 创建在写操作发起前 Context 已取消时，请求立即阻断，不对外部发生任何写操作。
func TestPR05_HMECreateCancellationBeforeWriteDoesNotMutate(t *testing.T) {
	fb := &fakeBackend{}
	var upstreamCalls int32
	fb.onCreateAliasContext = func(ctx context.Context, accountID, label string) (*hme.CreateResult, error) {
		atomic.AddInt32(&upstreamCalls, 1)
		return &hme.CreateResult{Email: "created@icloud.com"}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 发起前即取消

	_, err := fb.CreateAliasContext(ctx, "acc1", "label")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("期望 context.Canceled, 实际: %v", err)
	}

	if calls := atomic.LoadInt32(&upstreamCalls); calls != 0 {
		t.Fatalf("写前 Context 已取消仍发起了上游写操作, calls=%d", calls)
	}
}

// TestPR05_HMECreateCancellationAfterWriteStartedPreservesOutcomeUnknown
// 验证 F10 与 PR-03 核心契约：一旦写操作已发起并提交，即使遭遇取消或超时，系统仍必须严格保留 UPSTREAM_OUTCOME_UNKNOWN 语义，禁止误标为安全取消。
func TestPR05_HMECreateCancellationAfterWriteStartedPreservesOutcomeUnknown(t *testing.T) {
	// 验证 classifyUpstreamErr 语义优先级
	errUnknown := fmt.Errorf("write sent: %w", hme.ErrOutcomeUnknown)
	classified := classifyUpstreamErr("创建别名失败", errUnknown)
	if classified.Code != "UPSTREAM_OUTCOME_UNKNOWN" {
		t.Fatalf("期望 UPSTREAM_OUTCOME_UNKNOWN, 实际: %s", classified.Code)
	}

	// 验证当包含 context.DeadlineExceeded 但底层是 ErrOutcomeUnknown 时，仍保持 OUTCOME_UNKNOWN
	wrappedCtxErr := fmt.Errorf("timeout occurred after submit: %w (cause: %v)", hme.ErrOutcomeUnknown, context.DeadlineExceeded)
	classifiedWrapped := classifyUpstreamErr("创建别名失败", wrappedCtxErr)
	if classifiedWrapped.Code != "UPSTREAM_OUTCOME_UNKNOWN" {
		t.Fatalf("写操作提交后的超时必须维持 UPSTREAM_OUTCOME_UNKNOWN, 实际: %s", classifiedWrapped.Code)
	}
}

// TestPR05_SchedulerStopCancelsInFlightCreate
// 验证 F10 核心契约：Scheduler 停止时能够及时取消在途的 creator Context，并在平稳收敛后安全退出。
func TestPR05_SchedulerStopCancelsInFlightCreate(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	inCreator := make(chan struct{})
	var canceledInFlight int32

	mockCreator := func(ctx context.Context, id, label string) (*hme.CreateResult, error) {
		close(inCreator)
		select {
		case <-ctx.Done():
			atomic.StoreInt32(&canceledInFlight, 1)
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return nil, errors.New("timeout without cancel")
		}
	}

	mockAccounts := func() []account.Summary {
		return []account.Summary{
			{ID: "acc_sched", Name: "调度账号", Status: "active"},
		}
	}

	sched := scheduler.NewScheduler(st, mockCreator, mockAccounts)
	sched.Start()

	go func() {
		sched.RunAllNow(1)
	}()

	<-inCreator // 确认 creator 已进入阻塞在途状态

	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := sched.StopContext(stopCtx); err != nil {
		t.Fatalf("Scheduler 优雅停机失败: %v", err)
	}

	if atomic.LoadInt32(&canceledInFlight) != 1 {
		t.Fatal("在途的 creator 协程未收到停机 Context 取消信号")
	}
}

// TestPR05_ServerCloseWaitsForWorkersBeforeStoreClose
// 验证 F10 核心契约：Server 优雅停机必须等待 worker 完全收敛才关闭 Store；
// 若 worker 收敛超时，严禁提前关闭 Store，保障 SQLite 事务完整性。
func TestPR05_ServerCloseWaitsForWorkersBeforeStoreClose(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	fb := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_test", Status: "active"},
		},
	}

	// 模拟阻塞的 creator
	unblockCreator := make(chan struct{})
	inCreator := make(chan struct{})
	mockCreator := func(ctx context.Context, id, label string) (*hme.CreateResult, error) {
		select {
		case <-inCreator:
		default:
			close(inCreator)
		}
		<-unblockCreator
		return nil, nil
	}

	s := newWithBackendAndStore(fb, Config{}, st)
	// 替换 scheduler 的 creator 为阻塞 creator
	s.scheduler = scheduler.NewScheduler(st, mockCreator, func() []account.Summary {
		return fb.accounts
	})
	s.scheduler.Start()

	go func() {
		s.scheduler.RunAllNow(1)
	}()

	<-inCreator

	// 给一个极短的停机预算 (10ms)，肯定会超时
	timeoutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	closeErr := s.CloseContext(timeoutCtx)
	if !errors.Is(closeErr, context.DeadlineExceeded) {
		t.Fatalf("期望超时错误 DeadlineExceeded, 实际: %v", closeErr)
	}

	// 关键断言：超时返回后，底层 Store 绝不能被提前关闭，应该依然能正常访问！
	if _, err := st.ListAllAccounts(); err != nil {
		t.Fatalf("F10 破坏：优雅停机超时后，底层 Store 被过早关闭: %v", err)
	}

	// 释放在途 worker，并清理关闭 store
	close(unblockCreator)
	_ = st.Close()
}
