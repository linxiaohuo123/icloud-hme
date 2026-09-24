/**
 * [INPUT]: 依赖 context, fmt, strings, testing, time, icloud-hme/internal/account, icloud-hme/internal/auth, icloud-hme/internal/mail, icloud-hme/internal/server, icloud-hme/internal/store
 * [OUTPUT]: 提供 TestPR04B 系列测试 (0 订阅者兜底扫描、独立持久化、确定性故障停止推进、DB-first、混合别名防降级、无订阅者代际失效、订阅竞争消除、重启恢复、EventBus缓存过期不丢数据、存储重启持久性、纯旧版兼容、并发单一胜出者)
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
// 验证关键可靠性铁律：使用 SetBeforeVerificationPersistHookForTest 模拟持久化失败，
// 当前 page 视为未完成，绝不能推进 checkpoint，绝不能 markPublished，下轮重试必须恢复。
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

	messages := make([]mail.Message, 0, 50)
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

	// 使用确定性故障注入 hook 模拟持久化前报错
	hookTriggered := false
	worker.SetBeforeVerificationPersistHookForTest(func(ctx context.Context, target string, uid uint32) error {
		if uid == 115 {
			hookTriggered = true
			return fmt.Errorf("injected disk failure")
		}
		return nil
	})

	// 执行该批次扫描
	worker.fetchAndPublishBatch(ctx, "acc_1", []string{targetEmail})

	if !hookTriggered {
		t.Fatal("预期故障注入 hook 必须被触发")
	}

	// 核心断言: 发生持久化失败后，checkpoint 绝对不能推进到 150，必须保持在 100
	cpKey := checkpointKey{accountID: "acc_1", mailbox: "INBOX", uidValidity: 1}
	worker.mu.RLock()
	cp := worker.checkpoints[cpKey]
	worker.mu.RUnlock()

	if cp != nil && cp.NextUID > 100 {
		t.Fatalf("数据库失败时 checkpoint 不得被推进! 实际: %+v", cp)
	}

	// 移除故障 hook，恢复正常执行本轮扫描
	worker.SetBeforeVerificationPersistHookForTest(nil)
	worker.fetchAndPublishBatch(ctx, "acc_1", []string{targetEmail})

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

// TestPR04B_PersistentWatchNotDowngradedByLegacySubscriber
// 验证同一账号下，persistent alias A (有 strict baseline 且 >50 封邮件) 与 legacy alias B (无 baseline) 混合时，
// A 依然走 scanAndPublishPages UID 升序增量分页扫描，不会被降级到 legacy ListInboxContext (Limit=10)。
func TestPR04B_PersistentWatchNotDowngradedByLegacySubscriber(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	strictEmail := "strict@icloud.com"
	legacyEmail := "legacy@icloud.com"

	// 1. strict alias 创建 baseline 为 100 的 verification request
	err = st.CreateVerificationRequest(ctx, &store.VerificationRequest{
		RequestID:           "vreq_strict_1",
		PrincipalKind:       "token",
		PrincipalID:         "tok_1",
		LeaseID:             "lease_strict",
		AliasEmail:          strictEmail,
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

	// 2. 构造 65 封邮件 (UID 100..164)
	// strict 的目标邮件在 UID 155 (超过一页 50 封且 > baseline + 50)
	// legacy 的邮件在 UID 102
	messages := make([]mail.Message, 0, 65)
	for u := uint32(100); u <= 164; u++ {
		msg := mail.Message{
			ID:          fmt.Sprintf("%d", u),
			AccountID:   "acc_mix",
			Folder:      "INBOX",
			UIDValidity: 1,
			UID:         u,
			Provider:    "imap",
		}
		if u == 102 {
			msg.To = legacyEmail
			msg.Subject = "Your verification code is 384912"
			msg.Preview = "Please use 384912 to login"
		} else if u == 155 {
			msg.To = strictEmail
			msg.Subject = "Your verification code is 492815"
			msg.Preview = "Please use 492815 to login"
		}
		messages = append(messages, msg)
	}

	fb := newScanTestBackend("acc_mix", messages, 165)
	// 为 ListInboxContext 模拟 legacy 行为 (只返回最前 Limit 封，若 strict 误入此路径绝查不到 UID 155)
	fb.onListInboxContext = func(ctx context.Context, q InboxQuery) (InboxResult, error) {
		var list []mail.Message
		for _, m := range messages {
			if strings.EqualFold(m.To, q.Alias) || q.Alias == "" {
				list = append(list, m)
			}
		}
		if len(list) > q.Limit && q.Limit > 0 {
			list = list[:q.Limit]
		}
		return InboxResult{Messages: list, Count: len(list)}, nil
	}

	eventBus := mail.NewEventBus(5 * time.Minute)
	worker := NewMailSyncWorker(fb, st, eventBus, 1*time.Second)

	// legacyEmail 只订阅 EventBus (无 baseline，即 uid == 0)
	subID, legacyCh := eventBus.Subscribe(legacyEmail)
	defer eventBus.Unsubscribe(legacyEmail, subID)

	// 同时将 strict 与 legacy 传给 fetchAndPublishBatch
	matched := worker.fetchAndPublishBatch(ctx, "acc_mix", []string{strictEmail, legacyEmail})
	if !matched {
		t.Fatal("fetchAndPublishBatch 应匹配成功")
	}

	// 核心断言 1: strictEmail 绝不能被降级到 Limit=10，必须通过 UID 分页扫描成功拿到 UID 155 的验证码
	vreq, err := st.GetVerificationRequest(ctx, "vreq_strict_1", "token", "tok_1")
	if err != nil {
		t.Fatal(err)
	}
	if vreq.Status != "succeeded" || vreq.Code != "492815" {
		t.Fatalf("strict alias 被降级或未成功匹配 UID 155: %+v", vreq)
	}

	// 核心断言 2: legacyEmail 也正常收到 legacy 消息
	select {
	case evt := <-legacyCh:
		if evt.OTP.Code != "384912" {
			t.Fatalf("legacy 验证码不符: %+v", evt)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("legacy 别名超时未收到事件")
	}
}

// TestPR04B_UIDValidityMismatchInvalidatesWithoutSubscriber
// 验证在没有任何 EventBus 订阅者时，worker 扫描发现 UIDVALIDITY 改变，
// 数据库中所有对应 alias 的 pending/ready 任务被原子置为 invalidated。
func TestPR04B_UIDValidityMismatchInvalidatesWithoutSubscriber(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	targetEmail := "mismatch_nosub@icloud.com"

	err = st.CreateVerificationRequest(ctx, &store.VerificationRequest{
		RequestID:           "vreq_mismatch_1",
		PrincipalKind:       "token",
		PrincipalID:         "tok_mismatch",
		LeaseID:             "lease_mismatch",
		AliasEmail:          targetEmail,
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1, // 旧代际 1
		BaselineUID:         100,
	})
	if err != nil {
		t.Fatal(err)
	}

	fb := newScanTestBackend("acc_mismatch", nil, 100)
	// 模拟 IMAP 服务端 UIDVALIDITY 重建为 2
	fb.mailboxBoundaryFunc = func(accID, folder string) (string, uint32, uint32, error) {
		return "imap", 2, 100, nil
	}

	eventBus := mail.NewEventBus(5 * time.Minute)
	worker := NewMailSyncWorker(fb, st, eventBus, 1*time.Second)
	worker.RegisterAliasAccount(targetEmail, "acc_mismatch")

	// 明确断言：内存中 0 订阅者
	if eventBus.HasSubscribers() {
		t.Fatal("当前绝不应有 EventBus 订阅者")
	}

	// 执行 syncOnce
	worker.syncOnce()

	// 核心断言: 数据库中该未决任务必须已被原子置为 invalidated
	vreq, err := st.GetVerificationRequest(ctx, "vreq_mismatch_1", "token", "tok_mismatch")
	if err != nil {
		t.Fatal(err)
	}
	if vreq.Status != "invalidated" {
		t.Fatalf("代际突变后，0 订阅者下的未决任务必须被置为 invalidated, 实际: %s", vreq.Status)
	}
}

// TestPR04B_SubscribeRaceReturnsFromDBWithoutWaitingForEvent
// 验证在 GetVerificationResult 调用时，若后台 worker 已经把任务持久化到 DB 为终态，
// GetVerificationResult 通过 post-subscribe DB recheck 立即直接返回权威终态，绝对不会阻塞等待 30 秒。
func TestPR04B_SubscribeRaceReturnsFromDBWithoutWaitingForEvent(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	tokID := "tok_race_test"

	_ = st.SaveToken(store.APIToken{
		ID:        tokID,
		Name:      "Token Race",
		Token:     "secret",
		Scopes:    "verify",
		CreatedAt: now.Format(time.RFC3339),
	})

	err = st.CreateVerificationRequest(ctx, &store.VerificationRequest{
		RequestID:           "vreq_race_1",
		PrincipalKind:       "token",
		PrincipalID:         tokID,
		LeaseID:             "lease_race",
		AliasEmail:          "race@icloud.com",
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

	fb := newScanTestBackend("acc_race", nil, 100)
	eventBus := mail.NewEventBus(5 * time.Minute)
	vsvc := NewVerificationService(fb, st, eventBus, nil)

	// 模拟在 GetVerificationResult 刚订阅瞬间或进入 select 前，后台已经落库终态
	_, _, err = st.CompleteVerificationRequestResult(ctx, "vreq_race_1", store.VerificationCompletion{
		Code:            "999888",
		MatchedEventRef: "ref_precompleted",
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	p := auth.Principal{Kind: auth.PrincipalToken, ID: tokID, Scopes: []string{"verify"}}

	start := time.Now()
	// 请求 30 秒长轮询，若存在 subscribe race，它会卡死等满 30 秒
	res, err := vsvc.GetVerificationResult(ctx, p, "vreq_race_1", 30)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("必须通过 post-subscribe DB recheck 立即返回，不应阻塞等待 30 秒! 耗时: %v", elapsed)
	}
	if res.Status != "succeeded" || res.Code != "999888" {
		t.Fatalf("返回结果不正确: %+v", res)
	}
}

// TestPR04B_RestartWithPendingRequest
// 验证进程重启后（创建全新的 worker、全新的 EventBus、内存 checkpoint 和路由均为空），
// 留存在数据库中的 pending/ready 任务能自动驱动 worker 恢复扫描并完成落库。
func TestPR04B_RestartWithPendingRequest(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	targetEmail := "restart@icloud.com"

	// 1. 模拟重启前：持久化了路由表与待处理验证码请求
	_ = st.UpsertAliasRoutes("acc_restart", []string{targetEmail})
	err = st.CreateVerificationRequest(ctx, &store.VerificationRequest{
		RequestID:           "vreq_restart_1",
		PrincipalKind:       "token",
		PrincipalID:         "tok_restart",
		LeaseID:             "lease_restart",
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

	// 2. 准备邮件
	messages := []mail.Message{
		{
			ID:          "105",
			AccountID:   "acc_restart",
			Folder:      "INBOX",
			UIDValidity: 1,
			UID:         105,
			Provider:    "imap",
			To:          targetEmail,
			Subject:     "Your Code is 888999",
			Preview:     "Please use 888999",
		},
	}
	fb := newScanTestBackend("acc_restart", messages, 110)

	// 3. 模拟进程重启：全新的 EventBus 和 MailSyncWorker (内存路由为空，checkpoints 为空)
	newBus := mail.NewEventBus(5 * time.Minute)
	newWorker := NewMailSyncWorker(fb, st, newBus, 1*time.Second)

	// 4. 重启后无任何客户端连接，执行 syncOnce
	newWorker.syncOnce()

	// 5. 校验：worker 依靠数据库中的 verification_requests 和 alias_routes 自愈并成功持久化完成
	vreq, err := st.GetVerificationRequest(ctx, "vreq_restart_1", "token", "tok_restart")
	if err != nil {
		t.Fatal(err)
	}
	if vreq.Status != "succeeded" || vreq.Code != "888999" {
		t.Fatalf("重启后未决任务未能恢复扫描落库: %+v", vreq)
	}
}

// TestPR04B_EventBusCacheExpiryCannotLoseDurableResult
// 验证即使 EventBus 的缓存过期或被清理，客户端通过 GetVerificationResult 查询仍然能从 DB 中获得 durable result。
func TestPR04B_EventBusCacheExpiryCannotLoseDurableResult(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	tokID := "tok_cache_expire"

	_ = st.SaveToken(store.APIToken{
		ID:        tokID,
		Name:      "Token Cache",
		Token:     "secret",
		Scopes:    "verify",
		CreatedAt: now.Format(time.RFC3339),
	})

	err = st.CreateVerificationRequest(ctx, &store.VerificationRequest{
		RequestID:           "vreq_cache_exp",
		PrincipalKind:       "token",
		PrincipalID:         tokID,
		LeaseID:             "lease_cache_exp",
		AliasEmail:          "cache_exp@icloud.com",
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

	fb := newScanTestBackend("acc_1", []mail.Message{
		{
			ID:          "105",
			AccountID:   "acc_1",
			Folder:      "INBOX",
			UIDValidity: 1,
			UID:         105,
			Provider:    "imap",
			To:          "cache_exp@icloud.com",
			Subject:     "Code 777111",
			Preview:     "Code 777111",
		},
	}, 110)

	// 使用仅 1 毫秒 cache TTL 的 EventBus
	fastBus := mail.NewEventBus(1 * time.Millisecond)
	worker := NewMailSyncWorker(fb, st, fastBus, 1*time.Second)
	worker.RegisterAliasAccount("cache_exp@icloud.com", "acc_1")

	// 执行同步落库
	worker.syncOnce()

	// 睡眠确保 EventBus 内存缓存完全过期淘汰
	time.Sleep(10 * time.Millisecond)

	// 客户端此时发起查询，EventBus 内存已无此事件
	vsvc := NewVerificationService(fb, st, fastBus, worker)
	p := auth.Principal{Kind: auth.PrincipalToken, ID: tokID, Scopes: []string{"verify"}}
	res, err := vsvc.GetVerificationResult(ctx, p, "vreq_cache_exp", 5)
	if err != nil {
		t.Fatal(err)
	}

	// 核心断言: 结果必须从数据库持久化存储中准确读出，绝不丢失
	if res.Status != "succeeded" || res.Code != "777111" {
		t.Fatalf("EventBus 缓存过期导致结果丢失或不正确: %+v", res)
	}
}

// TestPR04B_CodeAndMagicLinkSurviveStoreRestart
// 验证无论 store 如何重启（关闭并重新从目录打开 SQLite 数据库），已经落库的 Code 与 MagicLink 仍然完好无损地被读取。
func TestPR04B_CodeAndMagicLinkSurviveStoreRestart(t *testing.T) {
	dbDir := t.TempDir()
	ctx := context.Background()
	now := time.Now().UTC()
	tokID := "tok_survive"

	// 第一次启动 store
	st1, err := store.NewStore(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	_ = st1.SaveToken(store.APIToken{
		ID:        tokID,
		Name:      "Token Survive",
		Token:     "secret",
		Scopes:    "verify",
		CreatedAt: now.Format(time.RFC3339),
	})
	_ = st1.CreateVerificationRequest(ctx, &store.VerificationRequest{
		RequestID:           "vreq_survive_1",
		PrincipalKind:       "token",
		PrincipalID:         tokID,
		LeaseID:             "lease_survive",
		AliasEmail:          "survive@icloud.com",
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1,
		BaselineUID:         100,
	})
	_, won, err := st1.CompleteVerificationRequestResult(ctx, "vreq_survive_1", store.VerificationCompletion{
		Code:            "123456",
		MagicLink:       "https://appleid.apple.com/auth/verify?code=123456",
		MatchedEventRef: "msg_survive",
	}, now)
	if err != nil || !won {
		t.Fatalf("初次写入失败: %v, won=%v", err, won)
	}
	// 关闭 store 1
	st1.Close()

	// 重新从相同磁盘目录打开 store 2
	st2, err := store.NewStore(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()

	vsvc := NewVerificationService(nil, st2, mail.NewEventBus(5*time.Minute), nil)
	p := auth.Principal{Kind: auth.PrincipalToken, ID: tokID, Scopes: []string{"verify"}}
	res, err := vsvc.GetVerificationResult(ctx, p, "vreq_survive_1", 0)
	if err != nil {
		t.Fatal(err)
	}

	if res.Status != "succeeded" {
		t.Fatalf("状态应为 succeeded, 实际: %s", res.Status)
	}
	if res.Code != "123456" {
		t.Fatalf("Code 应为 123456, 实际: %s", res.Code)
	}
	if res.MagicLink != "https://appleid.apple.com/auth/verify?code=123456" {
		t.Fatalf("MagicLink 重启后未正确保留: %s", res.MagicLink)
	}
}

// TestPR04B_LegacySubscriberStillWorksWithoutPersistentRequest
// 验证在没有任何 persistent verification request 的情况下，原有的 legacy EventBus 订阅者调用
// fetchAndPublishBatch 依然能通过 fetchAndPublishLegacyBatch 正常工作，向 EventBus 广播。
func TestPR04B_LegacySubscriberStillWorksWithoutPersistentRequest(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	legacyEmail := "pure_legacy@icloud.com"

	// 没有任何 verification_requests 在 store 中
	messages := []mail.Message{
		{
			ID:          "201",
			AccountID:   "acc_legacy_only",
			Folder:      "inbox",
			UIDValidity: 1,
			UID:         201,
			Provider:    "imap",
			To:          legacyEmail,
			Subject:     "Your Code is 445566",
			Preview:     "Code: 445566",
		},
	}
	fb := newScanTestBackend("acc_legacy_only", messages, 205)
	fb.onListInboxContext = func(ctx context.Context, q InboxQuery) (InboxResult, error) {
		return InboxResult{Messages: messages, Count: len(messages)}, nil
	}

	eventBus := mail.NewEventBus(5 * time.Minute)
	worker := NewMailSyncWorker(fb, st, eventBus, 1*time.Second)

	subID, ch := eventBus.Subscribe(legacyEmail)
	defer eventBus.Unsubscribe(legacyEmail, subID)

	matched := worker.fetchAndPublishBatch(ctx, "acc_legacy_only", []string{legacyEmail})
	if !matched {
		t.Fatal("fetchAndPublishBatch 应匹配成功")
	}

	select {
	case evt := <-ch:
		if evt.OTP.Code != "445566" {
			t.Fatalf("验证码不符: %+v", evt)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("legacy 别名超时未收到事件")
	}
}

// TestPR04B_ConcurrentCompletionHasSingleWinner
// 验证多个并发流程同时调用 CompleteVerificationRequestResult 时，只有一个胜出（won=true），其他返回 won=false 并拿到权威终态。
func TestPR04B_ConcurrentCompletionHasSingleWinner(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	err = st.CreateVerificationRequest(ctx, &store.VerificationRequest{
		RequestID:           "vreq_concurrent_win",
		PrincipalKind:       "token",
		PrincipalID:         "tok_con",
		LeaseID:             "lease_con",
		AliasEmail:          "con@icloud.com",
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

	concurrency := 10
	type result struct {
		code string
		won  bool
		req  *store.VerificationRequest
		err  error
	}
	resCh := make(chan result, concurrency)

	for i := 0; i < concurrency; i++ {
		code := fmt.Sprintf("%06d", i+1)
		go func(c string) {
			req, won, err := st.CompleteVerificationRequestResult(ctx, "vreq_concurrent_win", store.VerificationCompletion{
				Code:            c,
				MatchedEventRef: "msg_" + c,
			}, time.Now().UTC())
			resCh <- result{code: c, won: won, req: req, err: err}
		}(code)
	}

	var wonCount int
	var winnerCode string
	for i := 0; i < concurrency; i++ {
		r := <-resCh
		if r.err != nil {
			t.Fatalf("并发完成报错: %v", r.err)
		}
		if r.won {
			wonCount++
			winnerCode = r.code
		}
	}

	if wonCount != 1 {
		t.Fatalf("并发完成必须严格只有 1 个胜出者, 实际: %d", wonCount)
	}

	finalReq, err := st.GetVerificationRequest(ctx, "vreq_concurrent_win", "token", "tok_con")
	if err != nil {
		t.Fatal(err)
	}
	if finalReq.Status != "succeeded" || finalReq.Code != winnerCode {
		t.Fatalf("数据库中的最终记录与胜出者不符: %+v, winnerCode=%s", finalReq, winnerCode)
	}
}
