package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/scheduler"
	"icloud-hme/internal/store"
)

// TestFault_01_ReserveCommittedButResponseMalformed
// Reserve 请求已在上游落盘，但上游返回畸形响应 (如 HTML 页面或非法 JSON)，核对列表恢复候选 A，严禁生成候选 B。
func TestFault_01_ReserveCommittedButResponseMalformed(t *testing.T) {
	var generateCalls int32
	var reserveCalls int32
	candidateA := "cand_a@icloud.com"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hme/generate":
			count := atomic.AddInt32(&generateCalls, 1)
			if count > 1 {
				t.Errorf("CRITICAL VIOLATION: Generate called %d times; candidate B must never be created!", count)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"success": true, "result": {"hme": "%s"}}`, candidateA)
		case "/v1/hme/reserve":
			atomic.AddInt32(&reserveCalls, 1)
			// 上游服务端落盘成功，但网关/代理返回畸形 HTML
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`<!DOCTYPE html><html><body>502 Bad Gateway from Proxy</body></html>`))
		case "/v2/hme/list":
			// 核对接口证实候选 A 已成功落盘
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{
				"success": true,
				"result": {
					"hmeEmails": [
						{"hme": "%s", "anonymousId": "anon_a", "label": "label_a", "isActive": true}
					]
				}
			}`, candidateA)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	client, err := hme.NewClient(nil, "icloud.com", "", false)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetServiceURL(server.URL)

	ctx := context.Background()
	res, createErr := client.CreateAliasWithContext(ctx, "test_malformed", 3)
	if createErr != nil {
		t.Fatalf("TestFault_01 期望通过核对成功恢复候选 A，实际得到错误: %v", createErr)
	}

	if res.Email != candidateA {
		t.Fatalf("TestFault_01 期望交付候选 %s, 实际交付: %s", candidateA, res.Email)
	}
	if atomic.LoadInt32(&generateCalls) != 1 {
		t.Fatalf("TestFault_01 违反单候选铁律: generateCalls=%d", atomic.LoadInt32(&generateCalls))
	}
}

// TestFault_02_ReserveCommittedThenConnectionDrops
// Reserve 已在上游落盘，但 TCP 连接被对端重置或静默断开，核对列表恢复候选 A，严禁生成候选 B。
func TestFault_02_ReserveCommittedThenConnectionDrops(t *testing.T) {
	var generateCalls int32
	var reserveCalls int32
	candidateA := "cand_a_conn_drop@icloud.com"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hme/generate":
			count := atomic.AddInt32(&generateCalls, 1)
			if count > 1 {
				t.Errorf("CRITICAL VIOLATION: Generate called %d times; candidate B must never be created!", count)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"success": true, "result": {"hme": "%s"}}`, candidateA)
		case "/v1/hme/reserve":
			atomic.AddInt32(&reserveCalls, 1)
			// 上游已落盘，但在写入响应阶段 TCP 连接中断
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
		case "/v2/hme/list":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{
				"success": true,
				"result": {
					"hmeEmails": [
						{"hme": "%s", "anonymousId": "anon_drop", "label": "label_drop", "isActive": true}
					]
				}
			}`, candidateA)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	client, err := hme.NewClient(nil, "icloud.com", "", false)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetServiceURL(server.URL)

	ctx := context.Background()
	res, createErr := client.CreateAliasWithContext(ctx, "test_conn_drop", 3)
	if createErr != nil {
		t.Fatalf("TestFault_02 期望通过核对成功恢复候选 A，实际得到错误: %v", createErr)
	}

	if res.Email != candidateA {
		t.Fatalf("TestFault_02 期望交付候选 %s, 实际交付: %s", candidateA, res.Email)
	}
	if atomic.LoadInt32(&generateCalls) != 1 {
		t.Fatalf("TestFault_02 违反单候选铁律: generateCalls=%d", atomic.LoadInt32(&generateCalls))
	}
}

// TestFault_03_ReserveUnknownAndReconciliationFails
// Reserve 发生网络异常，且后续核对列表也失败/未找到，必须抛出 ErrOutcomeUnknown，严禁重试生成候选 B。
func TestFault_03_ReserveUnknownAndReconciliationFails(t *testing.T) {
	var generateCalls int32
	candidateA := "cand_a_unknown@icloud.com"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hme/generate":
			count := atomic.AddInt32(&generateCalls, 1)
			if count > 1 {
				t.Errorf("CRITICAL VIOLATION: Generate called %d times; candidate B must never be created!", count)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"success": true, "result": {"hme": "%s"}}`, candidateA)
		case "/v1/hme/reserve":
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
		case "/v2/hme/list":
			// 核对服务同样不可用
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error": "service unavailable"}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	client, err := hme.NewClient(nil, "icloud.com", "", false)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetServiceURL(server.URL)

	ctx := context.Background()
	_, createErr := client.CreateAliasWithContext(ctx, "test_unknown", 3)
	if createErr == nil {
		t.Fatalf("TestFault_03 期望返回 ErrOutcomeUnknown，实际得到 nil 成功")
	}

	if !errors.Is(createErr, hme.ErrOutcomeUnknown) {
		t.Fatalf("TestFault_03 期望包装 ErrOutcomeUnknown, 实际错误: %v", createErr)
	}

	if atomic.LoadInt32(&generateCalls) != 1 {
		t.Fatalf("TestFault_03 发生未知错误时严禁盲目重试生成第二候选: generateCalls=%d", atomic.LoadInt32(&generateCalls))
	}
}

// TestFault_04_ContextCancelledAfterWriteMayHaveStarted
// 写入请求发出后调用方 Context 被取消，不可当成确定未写入，必须经由一致性闭环处理，严禁生成第二候选。
func TestFault_04_ContextCancelledAfterWriteMayHaveStarted(t *testing.T) {
	var generateCalls int32
	candidateA := "cand_a_cancel@icloud.com"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hme/generate":
			count := atomic.AddInt32(&generateCalls, 1)
			if count > 1 {
				t.Errorf("CRITICAL VIOLATION: Generate called %d times; candidate B must never be created!", count)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"success": true, "result": {"hme": "%s"}}`, candidateA)
		case "/v1/hme/reserve":
			// 慢写入：确保调用方 context 超时取消先触发
			time.Sleep(150 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"success": true, "result": {"hme": {"hme": "%s", "anonymousId": "anon_cancel"}}}`, candidateA)
		case "/v2/hme/list":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{
				"success": true,
				"result": {
					"hmeEmails": [
						{"hme": "%s", "anonymousId": "anon_cancel", "label": "cancel_recovered", "isActive": true}
					]
				}
			}`, candidateA)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	client, err := hme.NewClient(nil, "icloud.com", "", false)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetServiceURL(server.URL)

	// 设置 40ms 超时 (会在 reserve 发出但在响应前触发 context cancellation)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	res, createErr := client.CreateAliasWithContext(ctx, "test_ctx_cancel", 3)
	// 因为 reserve 已经发出，后台独立上下文核对成功恢复该候选，或者以 ErrOutcomeUnknown 拒绝
	if createErr == nil {
		if res.Email != candidateA {
			t.Fatalf("TestFault_04 恢复结果非候选 A: %s", res.Email)
		}
	} else {
		if !errors.Is(createErr, hme.ErrOutcomeUnknown) && !errors.Is(createErr, context.DeadlineExceeded) && !errors.Is(createErr, context.Canceled) {
			t.Fatalf("TestFault_04 异常错误类型: %v", createErr)
		}
	}

	if atomic.LoadInt32(&generateCalls) != 1 {
		t.Fatalf("TestFault_04 上游写入可能已开始时取消，严禁盲目生成候选 B: generateCalls=%d", atomic.LoadInt32(&generateCalls))
	}
}

// TestFault_05_RestartRecoveryDoesNotCreateSecondCandidate
// 进程重启后遇到 outcome_unknown 记录，恢复流程严格针对原候选 A 核对，严禁覆盖分配候选 B。
func TestFault_05_RestartRecoveryDoesNotCreateSecondCandidate(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("初始化 Store 失败: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	opID := "op_restart_01"
	key := "idemp_restart_01"
	candA := "cand_a_restart@icloud.com"
	candB := "cand_b_tampered@icloud.com"

	// 1. 模拟崩溃前持久化的 outcome_unknown 记录 (绑定原候选 A)
	now := time.Now().Format(time.RFC3339)
	_, err = st.DB().ExecContext(ctx, `
		INSERT INTO operations (
			operation_id, principal_kind, principal_id, operation_kind, idempotency_key, request_hash, state, candidate_email, created_at, updated_at
		) VALUES (?, 'token', 'tok_restart', 'v2_allocate', ?, 'v2:test_hash', 'outcome_unknown', ?, ?, ?)
	`, opID, key, candA, now, now)
	if err != nil {
		t.Fatalf("插入 outcome_unknown 记录失败: %v", err)
	}

	// 2. 验证同键重放时直接返回 ErrOperationOutcomeUnknown，阻止新一轮盲目分配
	_, _, claimErr := st.ClaimInventoryAlias(ctx, "token", "tok_restart", "v2_allocate", key, "v2:test_hash", "default", nil)
	if !errors.Is(claimErr, store.ErrOperationOutcomeUnknown) {
		t.Fatalf("TestFault_05 期望 ErrOperationOutcomeUnknown, 实际得到: %v", claimErr)
	}

	// 3. 模拟恶意或错误恢复程序试图将操作替换为候选 B：必须硬性拒绝！
	_, tamperErr := st.ReconcileUnknownOperation(ctx, opID, true, candB, "acc_1", "default", "token", "tok_restart", "test")
	if tamperErr == nil {
		t.Fatalf("TestFault_05 致命错误: 恢复程序成功将候选 A 篡改为候选 B，违反单候选铁律！")
	}
	if !strings.Contains(tamperErr.Error(), "candidate mismatch") {
		t.Fatalf("TestFault_05 期望 candidate mismatch 拦截错误，实际得到: %v", tamperErr)
	}

	// 4. 正确针对原候选 A 进行一致性恢复：原子成功转为 succeeded
	alloc, recoverErr := st.ReconcileUnknownOperation(ctx, opID, true, candA, "acc_1", "default", "token", "tok_restart", "test")
	if recoverErr != nil {
		t.Fatalf("针对原候选 A 的恢复失败: %v", recoverErr)
	}
	if alloc.AliasEmail != candA {
		t.Fatalf("恢复交付别名不匹配: 期望 %s, 实际 %s", candA, alloc.AliasEmail)
	}

	// 核对状态变更为 succeeded
	op, _ := st.GetOperation(ctx, opID, "token", "tok_restart")
	if op.State != "succeeded" {
		t.Fatalf("恢复后操作状态期望 succeeded, 实际: %s", op.State)
	}
}

// TestFault_06_SchedulerStopsOnOutcomeUnknown
// 调度器在调用创建时遭遇 UPSTREAM_OUTCOME_UNKNOWN，必须立即阻断本轮补货并记录告警，严禁换号或继续创建。
func TestFault_06_SchedulerStopsOnOutcomeUnknown(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer st.Close()

	accID := "acc_test_sched"
	_ = st.SaveScheduleConfig(store.ScheduleConfig{
		AccountID:   accID,
		Enabled:     true,
		HourlyQuota: 10,
	})

	mockAccounts := func() []account.Summary {
		return []account.Summary{
			{ID: accID, Name: "测试调度号", Status: "active"},
		}
	}

	var creatorCalls int32
	mockCreator := func(id, label string) (*hme.CreateResult, error) {
		atomic.AddInt32(&creatorCalls, 1)
		return nil, &BackendError{
			Status:  http.StatusBadGateway,
			Code:    "UPSTREAM_OUTCOME_UNKNOWN",
			Message: "上游写操作结果未知，需核对后处理",
		}
	}

	sched := scheduler.NewScheduler(st, mockCreator, mockAccounts)

	// 请求单批补货 5 个
	created, errs := sched.RunOnce(false, 5)

	if created != 0 {
		t.Fatalf("TestFault_06 期望 created=0, 实际: %d", created)
	}
	if errs != 1 {
		t.Fatalf("TestFault_06 期望 errs=1, 实际: %d", errs)
	}
	// 关键断言：虽然请求创建 5 个，但第 1 个遇到 UPSTREAM_OUTCOME_UNKNOWN 后必须立刻 break 阻断，creator 调用次数必须为 1！
	if calls := atomic.LoadInt32(&creatorCalls); calls != 1 {
		t.Fatalf("TestFault_06 调度器遇到 outcome unknown 未立即阻断，调用次数: %d", calls)
	}

	logs := sched.Logs()
	foundAlert := false
	for _, l := range logs {
		if strings.Contains(l.Message, "遭遇上游写操作结果未知") && strings.Contains(l.Message, "立即停止本轮补货") {
			foundAlert = true
			break
		}
	}
	if !foundAlert {
		t.Fatalf("TestFault_06 未在调度器日志中找到阻断告警记录: %v", logs)
	}
}

// TestFault_07_ExplicitFailureAllowsRetry
// 上游返回结构完整、含义明确的业务拒绝错误（非网络中断、非格式畸形），确知写操作未落盘，正常允许重试生成候选 B 并成功。
func TestFault_07_ExplicitFailureAllowsRetry(t *testing.T) {
	var generateCalls int32
	candidateA := "cand_a_explicit_fail@icloud.com"
	candidateB := "cand_b_success@icloud.com"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hme/generate":
			count := atomic.AddInt32(&generateCalls, 1)
			w.WriteHeader(http.StatusOK)
			if count == 1 {
				_, _ = fmt.Fprintf(w, `{"success": true, "result": {"hme": "%s"}}`, candidateA)
			} else {
				_, _ = fmt.Fprintf(w, `{"success": true, "result": {"hme": "%s"}}`, candidateB)
			}
		case "/v1/hme/reserve":
			// 如果是候选 A，明确返回业务拒绝错误
			w.WriteHeader(http.StatusOK)
			if atomic.LoadInt32(&generateCalls) == 1 {
				_, _ = w.Write([]byte(`{
					"success": false,
					"error": {
						"errorCode": "-9999",
						"errorMessage": "Requested alias already in use"
					}
				}`))
			} else {
				// 候选 B 成功确认
				_, _ = fmt.Fprintf(w, `{
					"success": true,
					"result": {
						"hme": {
							"hme": "%s",
							"anonymousId": "anon_b_success"
						}
					}
				}`, candidateB)
			}
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	client, err := hme.NewClient(nil, "icloud.com", "", false)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.SetFixedServiceURL(server.URL)

	ctx := context.Background()
	res, createErr := client.CreateAliasWithContext(ctx, "test_explicit_retry", 3)
	if createErr != nil {
		t.Fatalf("TestFault_07 明确业务拒绝重试后期望成功，实际失败: %v", createErr)
	}

	if res.Email != candidateB {
		t.Fatalf("TestFault_07 期望交付新候选 %s, 实际: %s", candidateB, res.Email)
	}

	// 确认调用了 2 次 generate (候选 A 失败后重试候选 B)
	if calls := atomic.LoadInt32(&generateCalls); calls != 2 {
		t.Fatalf("TestFault_07 期望重试后共计 2 次 generate, 实际: %d", calls)
	}
}
