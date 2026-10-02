package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

func TestMailSync_SharedInboxAggregation_ReducesNetworkCallsAndPreventsCrossTalk(t *testing.T) {
	dir := t.TempDir()
	st, err := store.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer st.Close()

	eb := mail.NewEventBus(1 * time.Minute)
	fb := &fakeBackend{
		store: st,
		accounts: []account.Summary{
			{ID: "acc_1", RealEmail: "u1@icloud.com"},
			{ID: "acc_2", RealEmail: "u2@icloud.com"},
			{ID: "acc_3", RealEmail: "u3@icloud.com"},
		},
	}

	sharedFingerprint := "ext|shared_gmail@gmail.com|imap.gmail.com|993||mockhash"

	var boundaryCalls int32
	fb.onGetMailboxBoundaryContext = func(ctx context.Context, accountID, folder string) (string, uint32, uint32, error) {
		atomic.AddInt32(&boundaryCalls, 1)
		return "imap", 100, 1000, nil
	}

	fb.onGetMailboxEndpointFingerprint = func(accountID string) (string, bool) {
		// acc_1, acc_2, acc_3 共用同一个 Gmail 端点指纹
		if accountID == "acc_1" || accountID == "acc_2" || accountID == "acc_3" {
			return sharedFingerprint, true
		}
		return accountID, true
	}

	var scanCalls int32
	fb.onScanMailboxUIDPage = func(ctx context.Context, q ScanPageQuery) (ScanPageResult, error) {
		atomic.AddInt32(&scanCalls, 1)
		return ScanPageResult{
			UIDValidity: 100,
			NextUID:     1000,
			Messages: []mail.Message{
				{
					UID:         101,
					UIDValidity: 100,
					Folder:      "INBOX",
					Provider:    "imap",
					MessageRef:  "msg_101",
					Subject:     "Your verification code is 123456",
					To:          "alias2@privaterelay.appleid.com",
				},
			},
		}, nil
	}

	fb.onGetMessagesContext = func(ctx context.Context, accountID string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
		var res []*mail.FullMessage
		for _, ref := range refs {
			res = append(res, &mail.FullMessage{
				Message: mail.Message{
					UID:         ref.UID,
					UIDValidity: ref.UIDValidity,
					Folder:      ref.Mailbox,
					Provider:    ref.Provider,
					MessageRef:  ref.Encode(),
					Subject:     "Your verification code is 123456",
					Body:        "Code: 123456",
					To:          "alias2@privaterelay.appleid.com",
				},
			})
		}
		return res, nil
	}

	worker := NewMailSyncWorker(fb, st, eb, 1*time.Second)

	// 注册 3 个母号与对应别名
	alias1 := "alias1@privaterelay.appleid.com"
	alias2 := "alias2@privaterelay.appleid.com"
	alias3 := "alias3@privaterelay.appleid.com"

	worker.RegisterAliasAccount(alias1, "acc_1")
	worker.RegisterAliasAccount(alias2, "acc_2")
	worker.RegisterAliasAccount(alias3, "acc_3")

	// 建立对 3 个别名的订阅
	_, ch1 := eb.SubscribeWithBoundary(alias1, "INBOX", 100, 100)
	_, ch2 := eb.SubscribeWithBoundary(alias2, "INBOX", 100, 100)
	_, ch3 := eb.SubscribeWithBoundary(alias3, "INBOX", 100, 100)

	// 创建持久化 VerificationRequest
	now := time.Now().UTC()
	vreq2 := &store.VerificationRequest{
		RequestID:           "vreq_2",
		PrincipalKind:       "token",
		PrincipalID:         "tok_user2",
		LeaseID:             "lease_2",
		AliasEmail:          alias2,
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 100,
		BaselineUID:         100,
	}
	if err := st.CreateVerificationRequestAtomic(context.Background(), vreq2, 100, 100); err != nil {
		t.Fatalf("CreateVerificationRequestAtomic: %v", err)
	}

	// 执行单轮增量邮件同步
	worker.syncOnce()

	// 验证 1：边界查询只调用了 1 次（聚合成 1 个物理请求，而不是 3 个！）
	if got := atomic.LoadInt32(&boundaryCalls); got != 1 {
		t.Fatalf("期望边界查询被聚合为 1 次，实际调用次数: %d", got)
	}

	// 验证 2：UID 分页扫描只调用了 1 次（聚合成 1 个物理扫描批次，而不是 3 个！）
	if got := atomic.LoadInt32(&scanCalls); got != 1 {
		t.Fatalf("期望分页扫描被聚合为 1 次，实际调用次数: %d", got)
	}

	// 验证 3：alias1 和 alias3 未收到验证码，无跨母号串码
	select {
	case item := <-ch1:
		t.Fatalf("alias1 不应收到邮件事件: %+v", item)
	default:
	}
	select {
	case item := <-ch3:
		t.Fatalf("alias3 不应收到邮件事件: %+v", item)
	default:
	}

	// 验证 4：alias2 收到验证码，且 AccountID 严格为真实母号 "acc_2"（绝非代表母号 acc_1）
	select {
	case item := <-ch2:
		if item.AccountID != "acc_2" {
			t.Fatalf("防串码验证失败：期望 AccountID 归属真实母号 acc_2，实际收到: %s", item.AccountID)
		}
		if item.OTP == nil || item.OTP.Code != "123456" {
			t.Fatalf("期望验证码 123456，实际: %+v", item.OTP)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("alias2 超时未收到验证码事件")
	}

	// 验证 5：数据库中 vreq_2 成功落库为 succeeded
	updatedReq, err := st.GetVerificationRequest(context.Background(), "vreq_2", "token", "tok_user2")
	if err != nil {
		t.Fatalf("GetVerificationRequest: %v", err)
	}
	if updatedReq.Status != "succeeded" || updatedReq.Code != "123456" {
		t.Fatalf("期望任务状态为 succeeded 且 code 为 123456，实际: status=%s code=%s", updatedReq.Status, updatedReq.Code)
	}
}
