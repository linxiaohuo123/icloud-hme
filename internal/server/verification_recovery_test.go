/**
 * [INPUT]: 依赖 context, testing, time, icloud-hme/internal/account, icloud-hme/internal/auth, icloud-hme/internal/mail, icloud-hme/internal/server, icloud-hme/internal/store
 * [OUTPUT]: 提供 TestPR04B_SyncOnce_ScansEvenWithZeroEventBusSubscribers, TestPR04B_WorkerDurableCompletion_DirectPersistenceAndMagicLink, TestPR04B_PageCheckpointHaltedOnDatabaseFailure, TestPR04B_GetVerificationResult_DBFirstAndLegacyCompatibility
 * [POS]: internal/server 的 PR-04B 验证恢复、权威持久化与 DB-first 契约测试集
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"icloud-hme/internal/auth"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

// TestPR04B_SyncOnce_ScansEvenWithZeroEventBusSubscribers
// 验证 PR-04B 核心契约：
// 即使 EventBus 订阅者数量为 0 (无客户端在做 HTTP 长轮询)，
// 只要数据库存在 status IN ('ready', 'pending') 且未过期的 verification_requests，
// MailSyncWorker 仍必须持续扫描并直接将验证码持久化到数据库中。
// 客户端在邮件入库后调用 GetVerificationResult 能立刻 DB-first 获取到终态结果。
func TestPR04B_SyncOnce_ScansEvenWithZeroEventBusSubscribers(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	targetEmail := "nobus_target@icloud.com"
	tokID := "tok_pr04b_1"

	// 1. 初始化 Token
	err = st.SaveToken(store.APIToken{
		ID:        tokID,
		Name:      "Token 1",
		Token:     "test-secret-token",
		Scopes:    "verify",
		CreatedAt: now.Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}

	// 2. 插入持久化取码请求 (status='ready', baseline=100)
	err = st.CreateVerificationRequest(ctx, &store.VerificationRequest{
		RequestID:           "vreq_nobus_1",
		PrincipalKind:       "token",
		PrincipalID:         tokID,
		LeaseID:             "lease_nobus_1",
		AliasEmail:          targetEmail,
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1,
		BaselineUID:         100,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 3. 准备邮件: UID 105 是发给 targetEmail 的验证码邮件
	messages := []mail.Message{
		{
			ID:          "105",
			AccountID:   "acc_nobus",
			Folder:      "INBOX",
			UIDValidity: 1,
			UID:         105,
			Provider:    "imap",
			To:          targetEmail,
			Subject:     "Your Code is 654321",
			Preview:     "Please use 654321 to login",
		},
	}

	fb := newScanTestBackend("acc_nobus", messages, 110)
	eventBus := mail.NewEventBus(5 * time.Minute)
	worker := NewMailSyncWorker(fb, st, eventBus, 1*time.Second)

	// 登记别名到账号路由
	worker.RegisterAliasAccount(targetEmail, "acc_nobus")

	// 明确断言: 此时没有任何 EventBus 订阅者！
	if eventBus.HasSubscribers() {
		t.Fatal("当前 EventBus 绝不应有订阅者")
	}

	// 4. 执行 syncOnce
	worker.syncOnce()

	// 5. 校验数据库：取码任务必须已被 worker 直接持久化置为 succeeded！
	vreq, err := st.GetVerificationRequest(ctx, "vreq_nobus_1", "token", tokID)
	if err != nil {
		t.Fatal(err)
	}
	if vreq.Status != "succeeded" {
		t.Fatalf("即使无 EventBus 订阅者，任务也必须被持久化完成为 succeeded, 实际: %s", vreq.Status)
	}
	if vreq.Code != "654321" {
		t.Fatalf("持久化 Code 预期 654321, 实际: %s", vreq.Code)
	}

	// 6. 验证 VerificationService.GetVerificationResult (DB-first 快速读取，timeoutSec=0)
	vsvc := NewVerificationService(fb, st, eventBus, worker)
	res, err := vsvc.GetVerificationResult(ctx, auth.Principal{
		Kind:   auth.PrincipalToken,
		ID:     tokID,
		Scopes: []string{"verify"},
	}, "vreq_nobus_1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "succeeded" || res.Code != "654321" {
		t.Fatalf("DB-first 读取结果不正确: %+v", res)
	}
}

// TestPR04B_WorkerDurableCompletion_DirectPersistenceAndMagicLink
// 验证 worker 在扫描到同时包含 Code 与 MagicLink 的邮件时，独立持久化两项，不发生粗暴覆盖。
func TestPR04B_WorkerDurableCompletion_DirectPersistenceAndMagicLink(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	targetEmail := "magic_target@icloud.com"
	tokID := "tok_magic_1"

	_ = st.SaveToken(store.APIToken{
		ID:        tokID,
		Name:      "Token Magic",
		Token:     "secret",
		Scopes:    "verify",
		CreatedAt: now.Format(time.RFC3339),
	})

	_ = st.CreateVerificationRequest(ctx, &store.VerificationRequest{
		RequestID:           "vreq_magic_indep",
		PrincipalKind:       "token",
		PrincipalID:         tokID,
		LeaseID:             "lease_magic_indep",
		AliasEmail:          targetEmail,
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1,
		BaselineUID:         100,
	})

	messages := []mail.Message{
		{
			ID:          "105",
			AccountID:   "acc_1",
			Folder:      "INBOX",
			UIDValidity: 1,
			UID:         105,
			Provider:    "imap",
			To:          targetEmail,
			Subject:     "Sign in with code 789012",
			Preview:     "Or click here to verify: https://appleid.apple.com/auth/verify?code=789012",
		},
	}

	fb := newScanTestBackend("acc_1", messages, 110)
	eventBus := mail.NewEventBus(5 * time.Minute)
	worker := NewMailSyncWorker(fb, st, eventBus, 1*time.Second)

	// 带 EventBus 订阅运行
	subID, ch := eventBus.SubscribeWithBoundary(targetEmail, "INBOX", 1, 100)
	defer eventBus.Unsubscribe(targetEmail, subID)

	matched := worker.fetchAndPublishBatch(ctx, "acc_1", []string{targetEmail})
	if !matched {
		t.Fatal("fetchAndPublishBatch 应该匹配成功")
	}

	// 1. 验证 EventBus 正常唤醒
	select {
	case evt := <-ch:
		if evt.UID != 105 || evt.OTP.Code != "789012" || !strings.Contains(evt.OTP.MagicLink, "https://appleid.apple.com") {
			t.Fatalf("EventBus 事件内容不正确: %+v", evt)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("EventBus 超时未收到事件")
	}

	// 2. 核心验证: 数据库中 Code 与 MagicLink 均独立持久化且值完全准确
	vreq, err := st.GetVerificationRequest(ctx, "vreq_magic_indep", "token", tokID)
	if err != nil {
		t.Fatal(err)
	}
	if vreq.Status != "succeeded" {
		t.Fatalf("数据库状态应为 succeeded, 实际: %s", vreq.Status)
	}
	if vreq.Code != "789012" {
		t.Fatalf("数据库 Code 应为 789012, 实际: %s", vreq.Code)
	}
	if !strings.Contains(vreq.MagicLink, "https://appleid.apple.com") {
		t.Fatalf("数据库 MagicLink 未独立保存: %s", vreq.MagicLink)
	}
}

// TestPR04B_PageCheckpointHaltedOnDatabaseFailure
// 验证关键可靠性铁律：如果数据库持久化出错，当前 page 视为未完成，
// 绝不能推进 checkpoint，绝不能 markPublished，下轮重试必须恢复。
func TestPR04B_PageCheckpointHaltedOnDatabaseFailure(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	targetEmail := "fail_page@icloud.com"

	_ = st.CreateVerificationRequest(ctx, &store.VerificationRequest{
		RequestID:           "vreq_fail_1",
		PrincipalKind:       "token",
		PrincipalID:         "tok",
		LeaseID:             "lease",
		AliasEmail:          targetEmail,
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1,
		BaselineUID:         100,
	})

	messages := make([]mail.Message, 0, 100)
	for u := uint32(100); u <= 140; u++ {
		msg := mail.Message{
			ID:          fmt.Sprintf("%d", u),
			AccountID:   "acc_1",
			Folder:      "INBOX",
			UIDValidity: 1,
			UID:         u,
			Provider:    "imap",
		}
		if u == 115 {
			msg.To = targetEmail
			msg.Subject = "Code 345678"
			msg.Preview = "Your code is 345678"
		}
		messages = append(messages, msg)
	}

	fb := newScanTestBackend("acc_1", messages, 150)
	eventBus := mail.NewEventBus(5 * time.Minute)
	worker := NewMailSyncWorker(fb, st, eventBus, 1*time.Second)

	// 模拟数据库发生锁争用或异常：先占有底层排他业务锁，使 CompleteMatchingVerificationRequests 无法在极短时间内完成并报错/超时
	// 我们直接利用一个已超时的 context 模拟执行该批次
	timeoutCtx, cancel := context.WithTimeout(ctx, 1*time.Nanosecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond) // 确保超时

	// 此时带超时的 ctx 访问 store 会失败
	worker.fetchAndPublishBatch(timeoutCtx, "acc_1", []string{targetEmail})

	// 核心断言: 发生数据库写入失败/取消后，checkpoint 绝对不能推进到 150，必须保持在 100
	cpKey := checkpointKey{accountID: "acc_1", mailbox: "INBOX", uidValidity: 1}
	worker.mu.RLock()
	cp := worker.checkpoints[cpKey]
	worker.mu.RUnlock()

	if cp != nil && cp.NextUID > 100 {
		t.Fatalf("数据库失败时 checkpoint 不得被推进! 实际: %+v", cp)
	}

	// 恢复正常 context 重新执行本轮扫描
	worker.fetchAndPublishBatch(context.Background(), "acc_1", []string{targetEmail})

	// 恢复后数据库应当成功落地，且 checkpoint 正常推进
	vreq, _ := st.GetVerificationRequest(ctx, "vreq_fail_1", "token", "tok")
	if vreq.Status != "succeeded" || vreq.Code != "345678" {
		t.Fatalf("重试后取码应当成功落地: %+v", vreq)
	}

	worker.mu.RLock()
	cpAfter := worker.checkpoints[cpKey]
	worker.mu.RUnlock()

	if cpAfter == nil || cpAfter.NextUID < 140 {
		t.Fatalf("重试后 checkpoint 应当成功推进: %+v", cpAfter)
	}
}

// TestPR04B_GetVerificationResult_DBFirstAndLegacyCompatibility
// 验证 GetVerificationResult 的 DB-first 特性与历史数据兼容
func TestPR04B_GetVerificationResult_DBFirstAndLegacyCompatibility(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	tokID := "tok_legacy_test"

	_ = st.SaveToken(store.APIToken{
		ID:        tokID,
		Name:      "Token Legacy",
		Token:     "secret",
		Scopes:    "verify",
		CreatedAt: now.Format(time.RFC3339),
	})

	// 场景 1: 数据库中已有 succeeded 任务 (例如由 worker 离线写入)
	_ = st.CreateVerificationRequest(ctx, &store.VerificationRequest{
		RequestID:           "vreq_db_first_done",
		PrincipalKind:       "token",
		PrincipalID:         tokID,
		LeaseID:             "lease_1",
		AliasEmail:          "done@icloud.com",
		Status:              "succeeded",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		Code:                "112233",
		MagicLink:           "https://apple.com/done",
		MatchedEventRef:     "ref_done",
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1,
		BaselineUID:         100,
	})

	// 场景 2: 历史脏数据兼容 (Code 存放了 URL，MagicLink 为空)
	_ = st.CreateVerificationRequest(ctx, &store.VerificationRequest{
		RequestID:           "vreq_legacy_url_code",
		PrincipalKind:       "token",
		PrincipalID:         tokID,
		LeaseID:             "lease_2",
		AliasEmail:          "legacy@icloud.com",
		Status:              "succeeded",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		Code:                "https://apple.com/legacy-login",
		MagicLink:           "",
		MatchedEventRef:     "ref_legacy",
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1,
		BaselineUID:         100,
	})

	fb := newScanTestBackend("acc_1", nil, 100)
	vsvc := NewVerificationService(fb, st, mail.NewEventBus(5*time.Minute), nil)
	p := auth.Principal{Kind: auth.PrincipalToken, ID: tokID, Scopes: []string{"verify"}}

	// 验证场景 1: DB-first 秒回，无任何等待
	res1, err := vsvc.GetVerificationResult(ctx, p, "vreq_db_first_done", 30)
	if err != nil {
		t.Fatal(err)
	}
	if res1.Status != "succeeded" || res1.Code != "112233" || res1.MagicLink != "https://apple.com/done" {
		t.Fatalf("场景 1 DB-first 读取失败: %+v", res1)
	}

	// 验证场景 2: 历史 URL Code 兼容推断 MagicLink
	res2, err := vsvc.GetVerificationResult(ctx, p, "vreq_legacy_url_code", 30)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Status != "succeeded" || res2.MagicLink != "https://apple.com/legacy-login" {
		t.Fatalf("场景 2 历史数据兼容推断失败: %+v", res2)
	}
}
