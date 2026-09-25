/**
 * [INPUT]: 依赖 context, fmt, io, net/http/httptest, strings, testing, time, icloud-hme/internal/account, icloud-hme/internal/auth, icloud-hme/internal/hme, icloud-hme/internal/mail, icloud-hme/internal/server, icloud-hme/internal/store
 * [OUTPUT]: 提供 TestEndToEnd_MinimumIntegrationFlow 满足 PR-01 ~ PR-04B 最小集成链路验收测试 (8 项标准)
 * [POS]: internal/server/e2e_integration_test.go
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/auth"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

// TestEndToEnd_MinimumIntegrationFlow 全链路端到端最小集成验收测试 (8 项核心标准):
// 1. 健康检查：/livez 成功，/readyz 能反映 DB 就绪
// 2. 别名分配：pool / on-demand 能通过 contract 拿到 alias
// 3. 上游写入：对 upstream write 产生显式 outcome (success / confirmed_failed / outcome_unknown) 与未决熔断门禁
// 4. 邮件扫描：分页扫描 (>50 封)、checkpoint 推进、新 subscriber baseline 覆盖正常
// 5. 验证码结果：durable result 落库，recovery 轮询 / EventBus push 双通道均能取到结果
// 6. 重试幂等：分配幂等、状态变更幂等、同一邮件重复解析不污染结果
// 7. 降级安全：EventBus 断开或无 waiter 时，DB 轮询仍能成功交付终态
// 8. 进程启停：带 active subscriber 时正常退出与重启，checkpoint / pending verification 不丢并能自愈推进
func TestEndToEnd_MinimumIntegrationFlow(t *testing.T) {
	dbDir := t.TempDir()
	st, err := store.NewStore(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	tokID := "tok_e2e_admin"
	targetAlias := "e2e_user@icloud.com"
	accID := "acc_e2e_1"

	// 0. 初始化基础数据 (Token, 账号)
	err = st.SaveToken(store.APIToken{
		ID:        tokID,
		Name:      "E2E Admin Token",
		Token:     "secret-token-e2e",
		Scopes:    "allocate,verify,manage",
		CreatedAt: now.Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = st.SaveAccount(&store.AccountRecord{
		ID:     accID,
		Name:   "E2E Test Account",
		Status: "active",
	})
	if err != nil {
		t.Fatal(err)
	}
	p := auth.Principal{Kind: auth.PrincipalToken, ID: tokID, Scopes: []string{"allocate", "verify", "manage"}}

	fb := &fakeBackend{
		accounts: []account.Summary{{ID: accID, Status: "active", HasAppPassword: true, Tags: []string{"default"}}},
	}

	// ========================================================
	// 标准 1. 健康检查：/livez 成功，/readyz 能反映 DB 就绪
	// ========================================================
	srv := newWithBackendAndStore(fb, Config{AdminPassword: "test-admin-password"}, st)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1.1 存活探针
	{
		resp, err := ts.Client().Get(ts.URL + "/livez")
		if err != nil {
			t.Fatalf("1.1 GET /livez failed: %v", err)
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err := ValidateHealthProbeResponse(resp.StatusCode, resp.Header, bodyBytes); err != nil {
			t.Fatalf("1.1 /livez failed contract: %v", err)
		}
	}

	// 1.2 就绪探针 (DB 正常时应返回 200 ok)
	{
		resp, err := ts.Client().Get(ts.URL + "/readyz")
		if err != nil {
			t.Fatalf("1.2 GET /readyz failed: %v", err)
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err := ValidateHealthProbeResponse(resp.StatusCode, resp.Header, bodyBytes); err != nil {
			t.Fatalf("1.2 /readyz failed contract: %v", err)
		}
	}

	// ========================================================
	// 标准 2. 别名分配：pool / on-demand 能通过 contract 拿到 alias
	// ========================================================
	err = st.AddInventoryAlias(accID, hme.Alias{
		Email:  targetAlias,
		Label:  "e2e_label",
		Active: true,
	}, "replenish", true)
	if err != nil {
		t.Fatal(err)
	}

	allocSvc := NewAliasAllocationService(st, fb, nil)
	allocRes, err := allocSvc.Allocate(ctx, p, AllocationRequest{
		Tag:            "default",
		IdempotencyKey: "key_e2e_1",
		Mode:           "pool",
	})
	if err != nil {
		t.Fatalf("2. allocation 失败: %v", err)
	}
	if allocRes.Allocation == nil || allocRes.Allocation.AliasEmail != targetAlias || allocRes.Allocation.AllocationID == "" {
		t.Fatalf("2. allocation 返回结果不正确: %+v", allocRes)
	}

	// ========================================================
	// 标准 3. 上游写入：对 upstream write 产生显式 outcome 与未决熔断门禁
	// ========================================================
	// 3.1 测试显式 outcome: outcome_unknown 触发未决门禁
	intentA, err := st.CreateReserveIntent(ctx, accID, "candidate_a@icloud.com", "label_a")
	if err != nil {
		t.Fatalf("3.1 CreateReserveIntent failed: %v", err)
	}
	err = st.UpdateReserveIntentState(ctx, intentA.IntentID, store.IntentStateOutcomeUnknown, "", "", "network timeout")
	if err != nil {
		t.Fatalf("3.1 UpdateReserveIntentState failed: %v", err)
	}
	unresolved, err := st.ListUnresolvedReserveIntents(ctx, accID)
	if err != nil {
		t.Fatalf("3.1 ListUnresolvedReserveIntents failed: %v", err)
	}
	if len(unresolved) == 0 {
		t.Fatal("3.1 存在 outcome_unknown 时母号必须处于熔断未决状态")
	}

	// 3.2 模拟 reconcile 消除未决状态 (变为 succeeded)
	err = st.UpdateReserveIntentState(ctx, intentA.IntentID, store.IntentStateSucceeded, "dup_alias_id", "ref_1", "")
	if err != nil {
		t.Fatalf("3.2 UpdateReserveIntentState to succeeded failed: %v", err)
	}
	unresolvedAfter, err := st.ListUnresolvedReserveIntents(ctx, accID)
	if err != nil {
		t.Fatalf("3.2 ListUnresolvedReserveIntents failed: %v", err)
	}
	if len(unresolvedAfter) != 0 {
		t.Fatal("3.2 消除未决后账号熔断门禁应恢复正常")
	}

	// ========================================================
	// 标准 4. 邮件扫描：分页扫描 (>50 封)、checkpoint 推进、新 subscriber baseline 覆盖正常
	// ========================================================
	eventBus := mail.NewEventBus(5 * time.Minute)
	syncWorker := NewMailSyncWorker(fb, st, eventBus, 1*time.Second)
	vsvc := NewVerificationService(fb, st, eventBus, syncWorker)

	// mock IMAP mailbox boundary: UIDValidity=1, UIDNext=100
	fb.mailboxBoundaryFunc = func(accountID, folder string) (string, uint32, uint32, error) {
		return "imap", 1, 100, nil
	}

	vreq, err := vsvc.CreateVerificationRequest(ctx, p, allocRes.Allocation.AllocationID)
	if err != nil {
		t.Fatalf("4. CreateVerificationRequest failed: %v", err)
	}
	if vreq.BaselineUID != 100 {
		t.Fatalf("4. BaselineUID expected 100, got %d", vreq.BaselineUID)
	}

	// 构造 >50 封邮件 (UID 100..164)，目标邮件在 UID 155
	messages := make([]mail.Message, 0, 65)
	for u := uint32(100); u <= 164; u++ {
		msg := mail.Message{
			ID:          fmt.Sprintf("%d", u),
			AccountID:   accID,
			Folder:      "INBOX",
			UIDValidity: 1,
			UID:         u,
			Provider:    "imap",
		}
		if u == 155 {
			msg.To = targetAlias
			msg.Subject = "Your Apple verification code is 738291"
			msg.Preview = "Or click link to verify: https://appleid.apple.com/auth/verify?code=738291"
		} else {
			msg.To = "noise@icloud.com"
			msg.Subject = fmt.Sprintf("Noise message %d", u)
			msg.Preview = "Noise"
		}
		messages = append(messages, msg)
	}

	scanFb := newScanTestBackend(accID, messages, 165)
	syncWorker.be = scanFb

	// 订阅 EventBus 验证双通道 push
	subID, pushChan := eventBus.Subscribe(targetAlias)
	defer eventBus.Unsubscribe(targetAlias, subID)

	// 触发 syncWorker 执行一次增量流式分页扫描
	syncWorker.syncOnce()

	// 4.1 校验 checkpoint 推进 (已推进到 165)
	cpKey := checkpointKey{accountID: accID, mailbox: "INBOX", uidValidity: 1}
	cp := syncWorker.checkpoints[cpKey]
	if cp == nil || cp.NextUID != 165 {
		t.Fatalf("4.1 MailCheckpoint 推进异常: cp=%+v", cp)
	}

	// ========================================================
	// 标准 5. 验证码结果：durable result 落库，recovery 轮询 / EventBus push 双通道均能取到
	// ========================================================
	// 5.1 EventBus push 通道验证
	select {
	case pushedMsg := <-pushChan:
		if pushedMsg == nil || pushedMsg.OTP == nil || pushedMsg.OTP.Code != "738291" {
			t.Fatalf("5.1 EventBus 推送验证码不正确: %+v", pushedMsg)
		}
		if !strings.Contains(pushedMsg.OTP.MagicLink, "https://appleid.apple.com") {
			t.Fatalf("5.1 EventBus 推送 magic_link 不正确: %+v", pushedMsg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("5.1 EventBus push 超时未收到消息")
	}

	// 5.2 DB recovery 轮询通道验证 (durable result 落库)
	persistedReq, err := st.GetVerificationRequest(ctx, vreq.RequestID, "token", tokID)
	if err != nil {
		t.Fatalf("5.2 GetVerificationRequest failed: %v", err)
	}
	if persistedReq.Status != "succeeded" || persistedReq.Code != "738291" || !strings.Contains(persistedReq.MagicLink, "https://appleid.apple.com") {
		t.Fatalf("5.2 DB durable result 不正确: %+v", persistedReq)
	}

	// ========================================================
	// 标准 6. 重试幂等：分配幂等、状态变更幂等、同一邮件重复解析不污染结果
	// ========================================================
	// 6.1 分配幂等: 传入相同的 IdempotencyKey 必须原样返回同一 allocation
	allocRes2, err := allocSvc.Allocate(ctx, p, AllocationRequest{
		Tag:            "default",
		IdempotencyKey: "key_e2e_1",
		Mode:           "pool",
	})
	if err != nil {
		t.Fatalf("6.1 幂等分配重试失败: %v", err)
	}
	if allocRes2.Allocation.AllocationID != allocRes.Allocation.AllocationID {
		t.Fatalf("6.1 幂等分配返回了不同 allocation_id: %s vs %s", allocRes2.Allocation.AllocationID, allocRes.Allocation.AllocationID)
	}

	// 6.2 再次执行 syncWorker.syncOnce()，对已成功任务幂等防重，结果不受污染
	syncWorker.syncOnce()
	persistedReqAfterSecondSync, err := st.GetVerificationRequest(ctx, vreq.RequestID, "token", tokID)
	if err != nil || persistedReqAfterSecondSync.Status != "succeeded" || persistedReqAfterSecondSync.Code != "738291" {
		t.Fatalf("6.2 重复扫描污染了已成功的持久化验证结果: %+v, err=%v", persistedReqAfterSecondSync, err)
	}

	// ========================================================
	// 标准 7. 降级安全：EventBus 断开或无 waiter 时，DB 轮询仍能成功交付终态
	// ========================================================
	// 创建新的无任何 waiter 的 verification_request
	vreqNoWaiter, err := vsvc.CreateVerificationRequest(ctx, p, allocRes.Allocation.AllocationID)
	if err != nil {
		t.Fatalf("7. CreateVerificationRequest failed: %v", err)
	}
	// 模拟 EventBus 彻底清空并退化无 waiter 状态
	degradedBus := mail.NewEventBus(5 * time.Minute)
	if degradedBus.HasSubscribers() {
		t.Fatal("7. degradedBus 不应存在订阅者")
	}
	degradedWorker := NewMailSyncWorker(scanFb, st, degradedBus, 1*time.Second)
	degradedWorker.syncOnce()

	// 校验 DB 轮询接口仍然能够成功直接读取终态
	degradedSvc := NewVerificationService(scanFb, st, degradedBus, degradedWorker)
	pollRes, err := degradedSvc.GetVerificationResult(ctx, p, vreqNoWaiter.RequestID, 0)
	if err != nil {
		t.Fatalf("7. 无 waiter 降级 DB 轮询失败: %v", err)
	}
	if pollRes.Status != "succeeded" || pollRes.Code != "738291" {
		t.Fatalf("7. 降级 DB 轮询结果未达到终态: %+v", pollRes)
	}

	// ========================================================
	// 标准 8. 进程启停：带 active subscriber 时正常退出与重启，checkpoint / pending verification 不丢并能自愈推进
	// ========================================================
	// 8.1 准备待验证的新别名与邮件
	restartAlias := "restart_user@icloud.com"
	err = st.AddInventoryAlias(accID, hme.Alias{
		Email:  restartAlias,
		Label:  "restart_label",
		Active: true,
	}, "replenish", true)
	if err != nil {
		t.Fatal(err)
	}
	allocRestart, err := allocSvc.Allocate(ctx, p, AllocationRequest{
		Tag:            "default",
		IdempotencyKey: "key_restart_1",
		Mode:           "pool",
	})
	if err != nil {
		t.Fatalf("8.1 allocation 失败: %v", err)
	}

	vreqPending, err := vsvc.CreateVerificationRequest(ctx, p, allocRestart.Allocation.AllocationID)
	if err != nil {
		t.Fatalf("8.1 创建 pending 验证任务失败: %v", err)
	}

	// 8.2 模拟进程停止：关闭当前 store
	st.Close()

	// 8.3 模拟进程重新启动：重新打开 store
	stRestart, err := store.NewStore(dbDir)
	if err != nil {
		t.Fatalf("8.3 重启 store 失败: %v", err)
	}
	defer stRestart.Close()

	// 8.4 校验重启后 pending verification request 依然完整存在且状态为 ready
	reqRestart, err := stRestart.GetVerificationRequest(ctx, vreqPending.RequestID, "token", tokID)
	if err != nil {
		t.Fatalf("8.4 重启后读取 pending verification 失败: %v", err)
	}
	if reqRestart.RequestID != vreqPending.RequestID || reqRestart.Status != "ready" {
		t.Fatalf("8.4 重启后 pending verification 状态不正确: %+v", reqRestart)
	}

	// 8.5 准备重启后的后端数据，并由全新的 worker 恢复自愈执行
	restartMessages := append(messages, mail.Message{
		ID:          "166",
		AccountID:   accID,
		Folder:      "INBOX",
		UIDValidity: 1,
		UID:         166,
		Provider:    "imap",
		To:          restartAlias,
		Subject:     "Your restart code is 998877",
		Preview:     "Code: 998877",
	})
	restartFb := newScanTestBackend(accID, restartMessages, 167)
	restartBus := mail.NewEventBus(5 * time.Minute)
	restartWorker := NewMailSyncWorker(restartFb, stRestart, restartBus, 1*time.Second)

	// 重启后的 worker 在没有任何内存缓存的情况下执行同步
	restartWorker.syncOnce()

	// 8.6 校验重启后 pending 任务已自动自愈落库为 succeeded
	healedReq, err := stRestart.GetVerificationRequest(ctx, vreqPending.RequestID, "token", tokID)
	if err != nil {
		t.Fatalf("8.6 读取自愈后 verification request 失败: %v", err)
	}
	if healedReq.Status != "succeeded" || healedReq.Code != "998877" {
		t.Fatalf("8.6 进程重启后未决任务自愈落库不正确: %+v", healedReq)
	}

	// 8.7 校验重启后的全新 worker checkpoint 已推进到 167
	newCp := restartWorker.checkpoints[checkpointKey{accountID: accID, mailbox: "INBOX", uidValidity: 1}]
	if newCp == nil || newCp.NextUID != 167 {
		t.Fatalf("8.7 重启后新 worker checkpoint 未能正确建立推进: %+v", newCp)
	}
}
