package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	_, tamperErr := st.ReconcileUnknownOperation(ctx, opID, store.ReconciliationFound, candB, "acc_1", "default", "token", "tok_restart", "test")
	if tamperErr == nil {
		t.Fatalf("TestFault_05 致命错误: 恢复程序成功将候选 A 篡改为候选 B，违反单候选铁律！")
	}
	if !strings.Contains(tamperErr.Error(), "candidate mismatch") {
		t.Fatalf("TestFault_05 期望 candidate mismatch 拦截错误，实际得到: %v", tamperErr)
	}

	// 4. 正确针对原候选 A 进行一致性恢复：原子成功转为 succeeded
	alloc, recoverErr := st.ReconcileUnknownOperation(ctx, opID, store.ReconciliationFound, candA, "acc_1", "default", "token", "tok_restart", "test")
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
	mockCreator := func(ctx context.Context, id, label string) (*hme.CreateResult, error) {
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

// setupFaultTestEnvironment 为真实故障模拟建立隔离的临时测试环境
func setupFaultTestEnvironment(t *testing.T, accountID string, handler http.Handler) (*httptest.Server, *managerBackend, *store.Store, string) {
	t.Helper()
	server := httptest.NewServer(handler)
	dir := t.TempDir()
	accData := fmt.Sprintf(`{
		"accounts": {
			%q: {
				"id": %q,
				"name": "Fault Test Account",
				"real_email": "fault@example.com",
				"icloud_email": "fault@icloud.com",
				"cookies": {"X-APPLE-WEBAUTH-USER": "cookie-val", "dsid": "dsid-val"},
				"host": "icloud.com",
				"status": "active",
				"service_url": %q
			}
		}
	}`, accountID, accountID, server.URL)
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(accData), 0600); err != nil {
		t.Fatal(err)
	}
	mgr, err := account.NewManager(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = mgr.WithHMEClient(accountID, func(c *hme.Client) error {
		c.SetFixedServiceURL(server.URL)
		return nil
	})
	st, err := store.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	mb := &managerBackend{mgr: mgr, store: st}
	return server, mb, st, dir
}

// TestFault_PersistIntentBeforeReserve
// 铁律验证：在调用上游 /v1/hme/reserve 发出写请求之前，SQLite 数据库中必须已经持久化了该候选 A 的 intent，
// 且状态必须为 prepared 或 reserve_sent。
func TestFault_PersistIntentBeforeReserve(t *testing.T) {
	accID := "acc_test_persist"
	candA := "cand_a_persist@icloud.com"
	var reserveReceived int32
	var intentVerifiedBeforeReserve int32

	var server *httptest.Server
	var st *store.Store

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hme/generate":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"success": true, "result": {"hme": "%s"}}`, candA)
		case "/v1/hme/reserve":
			atomic.AddInt32(&reserveReceived, 1)
			// 核心断言：在写入网络响应之前，校验 SQLite 中 intent 已经落盘且为 reserve_sent 或 prepared
			if st != nil {
				intents, err := st.ListUnresolvedReserveIntents(context.Background(), accID)
				if err != nil {
					t.Errorf("查询 unresolved intents 失败: %v", err)
				} else {
					var foundIntent *store.HmeReserveIntent
					for i := range intents {
						if intents[i].CandidateEmail == candA {
							foundIntent = &intents[i]
							break
						}
					}
					if foundIntent == nil {
						t.Errorf("CRITICAL VIOLATION: upstream reserve 收到请求时，SQLite 未持久化候选 A 的 intent!")
					} else {
						if foundIntent.State != store.IntentStatePrepared && foundIntent.State != store.IntentStateReserveSent {
							t.Errorf("upstream reserve 收到请求时，intent 状态不符合预期 (prepared/reserve_sent): %s", foundIntent.State)
						} else {
							atomic.StoreInt32(&intentVerifiedBeforeReserve, 1)
						}
					}
				}
			}
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"success": true, "result": {"hme": {"hme": "%s", "anonymousId": "anon_persist"}}}`, candA)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	srv, mb, storeInst, _ := setupFaultTestEnvironment(t, accID, handler)
	server = srv
	st = storeInst
	defer server.Close()
	defer st.Close()

	res, err := mb.CreateAlias(accID, "test_label")
	if err != nil {
		t.Fatalf("CreateAlias 失败: %v", err)
	}
	if res.Email != candA {
		t.Fatalf("返回别名不匹配: 期望 %s, 实际 %s", candA, res.Email)
	}

	if atomic.LoadInt32(&reserveReceived) != 1 {
		t.Fatalf("reserve 调用次数不符合预期: %d", atomic.LoadInt32(&reserveReceived))
	}
	if atomic.LoadInt32(&intentVerifiedBeforeReserve) != 1 {
		t.Fatalf("CRITICAL: 未通过写前持久化意图断言！")
	}

	// 最终状态必须为 succeeded
	intent, err := st.FindLatestIntentForCandidate(context.Background(), candA)
	if err != nil || intent == nil {
		t.Fatalf("未能查询到完成后的 intent: %v", err)
	}
	if intent.State != store.IntentStateSucceeded {
		t.Fatalf("最终 intent 状态期望 succeeded, 实际: %s", intent.State)
	}
}

// TestFault_CrashAfterReserveSendBeforeResponse
// 场景验证：候选 A intent 已持久化并且写请求已发往上游，但进程在收到 response 前“崩溃”退出；
// 进程重启后新建 Store/Backend 实例，扫描未决意图，核对接口列表中存在候选 A，
// 恢复为 succeeded 并录入 inventory；绝不调用 Generate，绝不产生候选 B！
func TestFault_CrashAfterReserveSendBeforeResponse(t *testing.T) {
	accID := "acc_test_crash"
	candA := "cand_a_crash@icloud.com"
	var generateCalls int32

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hme/generate":
			count := atomic.AddInt32(&generateCalls, 1)
			t.Errorf("CRITICAL VIOLATION: 重启恢复核对期间严禁调用 Generate (调用次数=%d)！", count)
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"success": true, "result": {"hme": "cand_b_forbidden@icloud.com"}}`)
		case "/v2/hme/list":
			// 核对接口证实：在崩溃前发送的候选 A 已经成功落盘在上游
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{
				"success": true,
				"result": {
					"hmeEmails": [
						{"hme": "%s", "anonymousId": "anon_crash_a", "label": "test_crash", "isActive": true}
					]
				}
			}`, candA)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	server, _, st1, dir := setupFaultTestEnvironment(t, accID, handler)
	defer server.Close()

	// 1. 模拟崩溃前留在磁盘的现场：候选 A 的 intent 已持久化且状态为 reserve_sent
	ctx := context.Background()
	intent, err := st1.CreateReserveIntent(ctx, accID, candA, "test_crash")
	if err != nil {
		t.Fatalf("CreateReserveIntent 失败: %v", err)
	}
	if err := st1.UpdateReserveIntentState(ctx, intent.IntentID, store.IntentStateReserveSent, "", "", ""); err != nil {
		t.Fatalf("UpdateReserveIntentState 失败: %v", err)
	}

	// 2. 模拟进程崩溃：关闭当前 store
	_ = st1.Close()

	// 3. 模拟进程重启：使用相同数据目录启动新 Store 与新 Backend
	st2, err := store.NewStore(dir)
	if err != nil {
		t.Fatalf("重启 NewStore 失败: %v", err)
	}
	defer st2.Close()

	mgr2, err := account.NewManager(dir, nil)
	if err != nil {
		t.Fatalf("重启 NewManager 失败: %v", err)
	}
	_ = mgr2.WithHMEClient(accID, func(c *hme.Client) error {
		c.SetFixedServiceURL(server.URL)
		return nil
	})
	mb2 := &managerBackend{mgr: mgr2, store: st2}

	// 4. 执行重启恢复扫描
	recovered, err := mb2.ReconcileUnresolvedIntents(ctx)
	if err != nil {
		t.Fatalf("ReconcileUnresolvedIntents 失败: %v", err)
	}
	if len(recovered) != 1 {
		t.Fatalf("期望恢复 1 个未决意图, 实际: %d", len(recovered))
	}
	if recovered[0].CandidateEmail != candA {
		t.Fatalf("恢复的候选不匹配: 期望 %s, 实际 %s", candA, recovered[0].CandidateEmail)
	}
	if recovered[0].State != store.IntentStateSucceeded {
		t.Fatalf("恢复状态期望 succeeded, 实际: %s", recovered[0].State)
	}

	// 5. 验证数据库中最终记录及 inventory 落盘
	savedIntent, err := st2.GetReserveIntent(ctx, intent.IntentID)
	if err != nil {
		t.Fatalf("GetReserveIntent 失败: %v", err)
	}
	if savedIntent.State != store.IntentStateSucceeded {
		t.Fatalf("数据库持久化状态期望 succeeded, 实际: %s", savedIntent.State)
	}
	if savedIntent.AnonymousID != "anon_crash_a" {
		t.Fatalf("数据库持久化 anonymousID 期望 anon_crash_a, 实际: %s", savedIntent.AnonymousID)
	}

	// 检查 inventory 自动入库
	var invCount int
	_ = st2.DB().QueryRowContext(ctx, "SELECT COUNT(1) FROM alias_inventory WHERE email = ? AND account_id = ?", candA, accID).Scan(&invCount)
	if invCount != 1 {
		t.Fatalf("期望候选 A 录入 alias_inventory, 实际未查到")
	}

	// 6. 核心铁律断言：绝对没有调用 Generate 生成候选 B！
	if calls := atomic.LoadInt32(&generateCalls); calls != 0 {
		t.Fatalf("CRITICAL: 重启恢复期间产生候选 B: generateCalls=%d", calls)
	}
}

// TestFault_RestartUnknownCandidateStillMissing
// 核心安全铁律验证：
// 候选 A 处于 outcome_unknown，进程重启后核对列表，如果 Apple upstream 尚未列出该候选 A (可能同步延迟)，
// 坚决保持 outcome_unknown，严禁误标记为 failed / confirmed_failed，严禁生成候选 B！
func TestFault_RestartUnknownCandidateStillMissing(t *testing.T) {
	accID := "acc_test_missing"
	candA := "cand_a_missing@icloud.com"
	var generateCalls int32

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hme/generate":
			count := atomic.AddInt32(&generateCalls, 1)
			t.Errorf("CRITICAL VIOLATION: 核对 inconclusive 期间严禁调用 Generate (调用次数=%d)！", count)
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"success": true, "result": {"hme": "cand_b_forbidden@icloud.com"}}`)
		case "/v2/hme/list":
			// 上游列表为空或仅有其他不相关别名，候选 A 尚未出现
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{
				"success": true,
				"result": {
					"hmeEmails": []
				}
			}`)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	server, mb, st, _ := setupFaultTestEnvironment(t, accID, handler)
	defer server.Close()
	defer st.Close()

	ctx := context.Background()
	// 预置处于 outcome_unknown 的候选 A
	intent, err := st.CreateReserveIntent(ctx, accID, candA, "test_missing")
	if err != nil {
		t.Fatalf("CreateReserveIntent 失败: %v", err)
	}
	if err := st.UpdateReserveIntentState(ctx, intent.IntentID, store.IntentStateOutcomeUnknown, "", "", "pre-existing unknown"); err != nil {
		t.Fatalf("UpdateReserveIntentState 失败: %v", err)
	}

	// 执行核对
	recovered, err := mb.ReconcileUnresolvedIntents(ctx)
	if err != nil {
		t.Fatalf("ReconcileUnresolvedIntents 返回错误: %v", err)
	}
	if len(recovered) != 1 {
		t.Fatalf("期望包含 1 条 intent, 实际: %d", len(recovered))
	}

	// 核心断言 1：状态必须保持为 outcome_unknown，绝不能变为 failed 或 confirmed_failed
	if recovered[0].State != store.IntentStateOutcomeUnknown {
		t.Fatalf("CRITICAL: 未找到候选时禁止判定为失败！当前状态: %s", recovered[0].State)
	}

	savedIntent, err := st.GetReserveIntent(ctx, intent.IntentID)
	if err != nil {
		t.Fatalf("GetReserveIntent 失败: %v", err)
	}
	if savedIntent.State != store.IntentStateOutcomeUnknown {
		t.Fatalf("CRITICAL: 数据库状态必须保持 outcome_unknown，实际: %s", savedIntent.State)
	}

	// 核心断言 2：绝对没有调用 Generate 生成候选 B！
	if calls := atomic.LoadInt32(&generateCalls); calls != 0 {
		t.Fatalf("CRITICAL: inconclusive 期间严禁产生候选 B: generateCalls=%d", calls)
	}
}

// TestFault_RestartLaterFindsCandidate
// 最终一致性验证：
// 首次核对 missing 保持 outcome_unknown；后续第二次核对时上游出现候选 A，
// 成功转为 succeeded 并录入 inventory；全程候选始终为 A，Generate 绝不被调用。
func TestFault_RestartLaterFindsCandidate(t *testing.T) {
	accID := "acc_test_later"
	candA := "cand_a_later@icloud.com"
	var generateCalls int32
	var listCalls int32

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hme/generate":
			count := atomic.AddInt32(&generateCalls, 1)
			t.Errorf("CRITICAL VIOLATION: Generate called %d times!", count)
			w.WriteHeader(http.StatusOK)
		case "/v2/hme/list":
			count := atomic.AddInt32(&listCalls, 1)
			w.WriteHeader(http.StatusOK)
			if count == 1 {
				// 第一次核对：列表尚未出现候选 A
				_, _ = fmt.Fprintf(w, `{"success": true, "result": {"hmeEmails": []}}`)
			} else {
				// 第二次核对：上游最终一致性完成，候选 A 出现！
				_, _ = fmt.Fprintf(w, `{
					"success": true,
					"result": {
						"hmeEmails": [
							{"hme": "%s", "anonymousId": "anon_later", "label": "test_later", "isActive": true}
						]
					}
				}`, candA)
			}
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	server, mb, st, _ := setupFaultTestEnvironment(t, accID, handler)
	defer server.Close()
	defer st.Close()

	ctx := context.Background()
	intent, err := st.CreateReserveIntent(ctx, accID, candA, "test_later")
	if err != nil {
		t.Fatalf("CreateReserveIntent 失败: %v", err)
	}
	_ = st.UpdateReserveIntentState(ctx, intent.IntentID, store.IntentStateOutcomeUnknown, "", "", "network drop")

	// 第一次核对：missing -> 维持 outcome_unknown
	res1, _ := mb.ReconcileUnresolvedIntents(ctx)
	if len(res1) != 1 || res1[0].State != store.IntentStateOutcomeUnknown {
		t.Fatalf("第一次核对未保持 outcome_unknown: %v", res1)
	}

	// 第二次核对：found -> 成功转为 succeeded
	res2, _ := mb.ReconcileUnresolvedIntents(ctx)
	if len(res2) != 1 || res2[0].State != store.IntentStateSucceeded {
		t.Fatalf("第二次核对未能恢复 succeeded: %v", res2)
	}

	saved, err := st.GetReserveIntent(ctx, intent.IntentID)
	if err != nil || saved.State != store.IntentStateSucceeded {
		t.Fatalf("数据库状态未更新为 succeeded: %v, state=%s", err, saved.State)
	}

	if calls := atomic.LoadInt32(&generateCalls); calls != 0 {
		t.Fatalf("CRITICAL: 全生命周期绝不允许生成第二候选: generateCalls=%d", calls)
	}
}

// TestFault_ExplicitConfirmedFailureAllowsNewCandidate
// 明确拒绝与安全重试验证：
// 仅当上游明确返回业务拒绝 (如 errorCode -9999 / 别名被占用等，确知写入未落盘) 时，
// 才将候选 A 记录为 confirmed_failed，并在重试机制下生成下一个候选 B 并最终成功。
func TestFault_ExplicitConfirmedFailureAllowsNewCandidate(t *testing.T) {
	accID := "acc_test_explicit_retry"
	candA := "cand_a_explicit@icloud.com"
	candB := "cand_b_explicit@icloud.com"
	var generateCalls int32

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hme/generate":
			count := atomic.AddInt32(&generateCalls, 1)
			w.WriteHeader(http.StatusOK)
			if count == 1 {
				_, _ = fmt.Fprintf(w, `{"success": true, "result": {"hme": "%s"}}`, candA)
			} else {
				_, _ = fmt.Fprintf(w, `{"success": true, "result": {"hme": "%s"}}`, candB)
			}
		case "/v1/hme/reserve":
			w.WriteHeader(http.StatusOK)
			if atomic.LoadInt32(&generateCalls) == 1 {
				// 候选 A: 上游返回明确业务错误 (结构完整、确知未落盘)
				_, _ = w.Write([]byte(`{
					"success": false,
					"error": {
						"errorCode": "-9999",
						"errorMessage": "Alias already taken"
					}
				}`))
			} else {
				// 候选 B: 成功创建
				_, _ = fmt.Fprintf(w, `{
					"success": true,
					"result": {
						"hme": {
							"hme": "%s",
							"anonymousId": "anon_b_success"
						}
					}
				}`, candB)
			}
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	server, mb, st, _ := setupFaultTestEnvironment(t, accID, handler)
	defer server.Close()
	defer st.Close()

	res, err := mb.CreateAlias(accID, "test_retry")
	if err != nil {
		t.Fatalf("CreateAlias 期望成功交付候选 B, 实际失败: %v", err)
	}
	if res.Email != candB {
		t.Fatalf("交付别名期望候选 B %s, 实际: %s", candB, res.Email)
	}

	if calls := atomic.LoadInt32(&generateCalls); calls != 2 {
		t.Fatalf("期望共调用 2 次 Generate, 实际: %d", calls)
	}

	// 检查数据库中候选 A 的记录状态为 confirmed_failed
	intentA, err := st.FindLatestIntentForCandidate(context.Background(), candA)
	if err != nil || intentA == nil {
		t.Fatalf("未能查到候选 A 的 intent: %v", err)
	}
	if intentA.State != store.IntentStateConfirmedFailed {
		t.Fatalf("候选 A 状态期望 confirmed_failed, 实际: %s", intentA.State)
	}

	// 检查数据库中候选 B 的记录状态为 succeeded
	intentB, err := st.FindLatestIntentForCandidate(context.Background(), candB)
	if err != nil || intentB == nil {
		t.Fatalf("未能查到候选 B 的 intent: %v", err)
	}
	if intentB.State != store.IntentStateSucceeded {
		t.Fatalf("候选 B 状态期望 succeeded, 实际: %s", intentB.State)
	}
}

// TestFault_UnresolvedIntentBlocksSubsequentCreate
// 验证 account-level unresolved gate:
// 当某账号存在 prepared / reserve_sent / outcome_unknown 的未决意图，
// 且核对列表暂时未找到原候选 A 时，坚决保持 outcome_unknown；
// 随后调用真正的 mb.CreateAlias(accountID, "new_request")：
// 必须立即返回 ErrOutcomeUnknown / UPSTREAM_OUTCOME_UNKNOWN，
// 严禁调用 Generate (调用次数=0)，严禁调用 Reserve (调用次数=0)，
// 数据库中原候选 A 保持 outcome_unknown，严禁产生 candidate B！
func TestFault_UnresolvedIntentBlocksSubsequentCreate(t *testing.T) {
	states := []store.IntentState{
		store.IntentStateOutcomeUnknown,
		store.IntentStatePrepared,
		store.IntentStateReserveSent,
	}

	for _, initialState := range states {
		t.Run(string(initialState), func(t *testing.T) {
			accID := "acc_block_" + string(initialState)
			candA := "cand_a_" + string(initialState) + "@icloud.com"
			var generateCalls int32
			var reserveCalls int32

			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/hme/generate":
					count := atomic.AddInt32(&generateCalls, 1)
					t.Errorf("CRITICAL VIOLATION: 未决账号新创建请求严禁调用 Generate (调用次数=%d)！", count)
					w.WriteHeader(http.StatusOK)
					_, _ = fmt.Fprintf(w, `{"success": true, "result": {"hme": "cand_b_forbidden@icloud.com"}}`)
				case "/v1/hme/reserve":
					count := atomic.AddInt32(&reserveCalls, 1)
					t.Errorf("CRITICAL VIOLATION: 未决账号新创建请求严禁调用 Reserve (调用次数=%d)！", count)
					w.WriteHeader(http.StatusOK)
					_, _ = fmt.Fprintf(w, `{"success": true, "result": {"hme": {"hme": "cand_b_forbidden@icloud.com", "anonymousId": "anon_b"}}}`)
				case "/v2/hme/list":
					// 上游列表暂时未出现该候选 A
					w.WriteHeader(http.StatusOK)
					_, _ = fmt.Fprintf(w, `{"success": true, "result": {"hmeEmails": []}}`)
				default:
					w.WriteHeader(http.StatusOK)
				}
			})

			server, mb, st, _ := setupFaultTestEnvironment(t, accID, handler)
			defer server.Close()
			defer st.Close()

			ctx := context.Background()

			// 1. 预置处于 initialState 的候选 A
			intent, err := st.CreateReserveIntent(ctx, accID, candA, "initial_intent")
			if err != nil {
				t.Fatalf("CreateReserveIntent 失败: %v", err)
			}
			if err := st.UpdateReserveIntentState(ctx, intent.IntentID, initialState, "", "", "pre-existing"); err != nil {
				t.Fatalf("UpdateReserveIntentState 失败: %v", err)
			}

			// 2. 模拟启动/后台核对，上游列表暂时无 A，核对结果 inconclusive
			_, _ = mb.ReconcileUnresolvedIntents(ctx)

			// 3. 随后调用真正的业务创建门面 mb.CreateAlias
			res, createErr := mb.CreateAlias(accID, "new_request")

			// 断言 1: 必须返回 ErrOutcomeUnknown / UPSTREAM_OUTCOME_UNKNOWN
			if createErr == nil {
				t.Fatalf("CRITICAL: 未决账号新创建必须报错阻断，实际成功交付: %v", res)
			}
			var be *BackendError
			if errors.As(createErr, &be) {
				if be.Code != "UPSTREAM_OUTCOME_UNKNOWN" {
					t.Fatalf("期望错误码 UPSTREAM_OUTCOME_UNKNOWN, 实际: %s", be.Code)
				}
			} else if !errors.Is(createErr, hme.ErrOutcomeUnknown) {
				t.Fatalf("期望包装 ErrOutcomeUnknown, 实际错误: %v", createErr)
			}

			// 断言 2: 严禁产生网络写操作
			if calls := atomic.LoadInt32(&generateCalls); calls != 0 {
				t.Fatalf("CRITICAL: Generate 调用次数必须为 0, 实际: %d", calls)
			}
			if calls := atomic.LoadInt32(&reserveCalls); calls != 0 {
				t.Fatalf("CRITICAL: Reserve 调用次数必须为 0, 实际: %d", calls)
			}

			// 断言 3: A intent 仍是 outcome_unknown
			saved, err := st.GetReserveIntent(ctx, intent.IntentID)
			if err != nil {
				t.Fatalf("GetReserveIntent 失败: %v", err)
			}
			if saved.State != store.IntentStateOutcomeUnknown {
				t.Fatalf("原候选 A 状态期望 outcome_unknown, 实际: %s", saved.State)
			}

			// 断言 4: 数据库中不存在任何 candidate B intent
			allIntents, err := st.ListUnresolvedReserveIntents(ctx, accID)
			if err != nil {
				t.Fatalf("ListUnresolvedReserveIntents 失败: %v", err)
			}
			if len(allIntents) != 1 || allIntents[0].CandidateEmail != candA {
				t.Fatalf("发现异常的多余候选意图: %v", allIntents)
			}
		})
	}
}

// TestFault_ResolvedIntentAllowsLaterIndependentCreate
// 证明 gate 只冻结“仍未解决”的账号，不会永久锁死创建能力：
// 1. A initially outcome_unknown；
// 2. reconciliation 后找到 A -> succeeded；
// 3. unresolved 列表变空；
// 4. 此时再发一个明确独立的新 CreateAlias 请求；
// 预期：新请求可以正常 Generate B，返回 B。
func TestFault_ResolvedIntentAllowsLaterIndependentCreate(t *testing.T) {
	accID := "acc_test_resolved_allows_create"
	candA := "cand_a_resolved@icloud.com"
	candB := "cand_b_new@icloud.com"
	var generateCalls int32
	var reserveCalls int32

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hme/generate":
			atomic.AddInt32(&generateCalls, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"success": true, "result": {"hme": "%s"}}`, candB)
		case "/v1/hme/reserve":
			atomic.AddInt32(&reserveCalls, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"success": true, "result": {"hme": {"hme": "%s", "anonymousId": "anon_b_new"}}}`, candB)
		case "/v2/hme/list":
			// 核对列表包含候选 A
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{
				"success": true,
				"result": {
					"hmeEmails": [
						{"hme": "%s", "anonymousId": "anon_a_resolved", "label": "old_a", "isActive": true}
					]
				}
			}`, candA)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	server, mb, st, _ := setupFaultTestEnvironment(t, accID, handler)
	defer server.Close()
	defer st.Close()

	ctx := context.Background()

	// 1. A initially outcome_unknown
	intentA, err := st.CreateReserveIntent(ctx, accID, candA, "old_intent")
	if err != nil {
		t.Fatalf("CreateReserveIntent 失败: %v", err)
	}
	_ = st.UpdateReserveIntentState(ctx, intentA.IntentID, store.IntentStateOutcomeUnknown, "", "", "network drop")

	// 2. reconciliation 后找到 A -> succeeded
	recovered, err := mb.ReconcileUnresolvedIntents(ctx)
	if err != nil {
		t.Fatalf("ReconcileUnresolvedIntents 失败: %v", err)
	}
	if len(recovered) != 1 || recovered[0].State != store.IntentStateSucceeded {
		t.Fatalf("核对未能将 A 转为 succeeded: %v", recovered)
	}

	// 3. unresolved 列表变空
	unresolved, err := st.ListUnresolvedReserveIntents(ctx, accID)
	if err != nil {
		t.Fatalf("ListUnresolvedReserveIntents 失败: %v", err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("期望 unresolved 列表变空, 实际仍有: %d 条", len(unresolved))
	}

	// 4. 此时再发一个明确独立的新 CreateAlias 请求
	res, createErr := mb.CreateAlias(accID, "independent_new_request")
	if createErr != nil {
		t.Fatalf("独立新请求期望成功创建，实际失败: %v", createErr)
	}
	if res.Email != candB {
		t.Fatalf("独立新请求期望交付新别名 %s, 实际: %s", candB, res.Email)
	}

	// 验证 Generate 和 Reserve 均只针对候选 B 调用了 1 次
	if calls := atomic.LoadInt32(&generateCalls); calls != 1 {
		t.Fatalf("期望 Generate 被调用 1 次 (针对候选 B), 实际: %d", calls)
	}
	if calls := atomic.LoadInt32(&reserveCalls); calls != 1 {
		t.Fatalf("期望 Reserve 被调用 1 次 (针对候选 B), 实际: %d", calls)
	}

	// 验证库中 A 仍为 succeeded，B 也为 succeeded
	savedA, _ := st.GetReserveIntent(ctx, intentA.IntentID)
	if savedA.State != store.IntentStateSucceeded {
		t.Fatalf("候选 A 状态期望 succeeded, 实际: %s", savedA.State)
	}
	intentB, err := st.FindLatestIntentForCandidate(ctx, candB)
	if err != nil || intentB == nil || intentB.State != store.IntentStateSucceeded {
		t.Fatalf("候选 B 未能持久化为 succeeded: %v", err)
	}
}


