/**
 * [INPUT]: 依赖 testing, context, fmt, strings, sync/atomic, time, icloud-hme/internal/mail, icloud-hme/internal/store, icloud-hme/internal/account
 * [OUTPUT]: 提供 PR-04A 增量邮件 UID 分页流式扫描回归与压力单测 (Test 1 至 Test 8)
 * [POS]: internal/server 的验证码邮件高效扫描、断点续传、固定上界与 metadata-first 防退化验证套件 (PR-04A F07)
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

// helper: 构造一个模拟真实 IMAP 行为的 fakeBackend，包含全量邮箱消息列表
func newScanTestBackend(accountID string, messages []mail.Message, uidNext uint32) *fakeBackend {
	fb := &fakeBackend{
		accounts: []account.Summary{{ID: accountID, Status: "active", HasAppPassword: true}},
		mailboxBoundaryFunc: func(accID, folder string) (string, uint32, uint32, error) {
			return "imap", 1, uidNext, nil
		},
	}

	fb.onScanMailboxUIDPage = func(ctx context.Context, q ScanPageQuery) (ScanPageResult, error) {
		if err := ctx.Err(); err != nil {
			return ScanPageResult{}, err
		}
		var matched []mail.Message
		for _, m := range messages {
			if m.UID >= q.FromUIDInclusive && m.UID <= q.ToUIDInclusive {
				// Metadata-first: 仅包含头部与收件人，Preview 清空模拟第一阶段不拉正文
				metaMsg := m
				metaMsg.Preview = ""
				matched = append(matched, metaMsg)
			}
		}
		pageSize := q.PageSize
		if pageSize <= 0 {
			pageSize = 50
		}
		hasMore := false
		if len(matched) > pageSize {
			matched = matched[:pageSize]
			hasMore = true
		}
		var nextUID uint32 = q.ToUIDInclusive + 1
		if len(matched) > 0 {
			nextUID = matched[len(matched)-1].UID + 1
		}
		return ScanPageResult{
			UIDValidity: 1,
			Messages:    matched,
			NextUID:     nextUID,
			HasMore:     hasMore,
		}, nil
	}

	fb.onGetMessagesContext = func(ctx context.Context, accID string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var out []*mail.FullMessage
		for _, r := range refs {
			for _, m := range messages {
				if m.UID == r.UID {
					out = append(out, &mail.FullMessage{
						Message:      m,
						Body:         m.Preview,
						BodyComplete: true,
						Provider:     "imap",
						Method:       "imap",
					})
					break
				}
			}
		}
		return out, nil
	}

	return fb
}

// Test 1: TestVerificationScan_TargetInFirstPageWith200NewMessages
// baseline = 100，新增 200 封: UID 100..299，目标 alias 的验证码在 UID 101。
// 其后有 198+ 封无关邮件。新实现必须完整命中并 Publish 该验证码。
func TestVerificationScan_TargetInFirstPageWith200NewMessages(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	targetEmail := "target_user@icloud.com"
	unrelatedEmail := "other_user@icloud.com"

	now := time.Now().UTC()
	vreq := &store.VerificationRequest{
		RequestID:           "vreq_scan_1",
		PrincipalKind:       "token",
		PrincipalID:         "tok_scan_1",
		LeaseID:             "lease_scan_1",
		AliasEmail:          targetEmail,
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1,
		BaselineUID:         100,
	}
	if err := st.CreateVerificationRequest(ctx, vreq); err != nil {
		t.Fatalf("CreateVerificationRequest 失败: %v", err)
	}

	messages := make([]mail.Message, 0, 200)
	for u := uint32(100); u <= 299; u++ {
		msg := mail.Message{
			ID:          fmt.Sprintf("%d", u),
			AccountID:   "acc_1",
			Folder:      "INBOX",
			UIDValidity: 1,
			UID:         u,
			Provider:    "imap",
		}
		if u == 101 {
			msg.To = targetEmail
			msg.Subject = "Your Apple ID verification code is 889900"
			msg.Preview = "Code: 889900"
		} else {
			msg.To = unrelatedEmail
			msg.Subject = fmt.Sprintf("Unrelated newsletter #%d", u)
			msg.Preview = fmt.Sprintf("Noise content for message %d", u)
		}
		messages = append(messages, msg)
	}

	fb := newScanTestBackend("acc_1", messages, 300)
	eventBus := mail.NewEventBus(5 * time.Minute)
	worker := NewMailSyncWorker(fb, st, eventBus, 1*time.Second)

	subID, ch := eventBus.SubscribeWithBoundary(targetEmail, "INBOX", 1, 100)
	defer eventBus.Unsubscribe(targetEmail, subID)

	matched := worker.fetchAndPublishBatch(ctx, "acc_1", []string{targetEmail})
	if !matched {
		t.Fatal("fetchAndPublishBatch 应返回 true (成功匹配并发布验证码)")
	}

	select {
	case evt := <-ch:
		if evt == nil || evt.OTP == nil || evt.OTP.Code != "889900" {
			t.Fatalf("收到错误的 OTP 事件: %+v", evt)
		}
		if evt.UID != 101 {
			t.Fatalf("期望命中 UID 101, 实际: %d", evt.UID)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("超时未收到验证码事件! 新分页增量扫描未能命中 UID 101")
	}
}

// Test 2: TestVerificationScan_TargetAcrossPageBoundaries
// pageSize = 50，分别测试目标在:
// baseline (100), baseline+49 (149), baseline+50 (150), baseline+99 (199), baseline+150 (250), 最后一个 UID (299)。
// 全部必须能够精准找到并 Publish。
func TestVerificationScan_TargetAcrossPageBoundaries(t *testing.T) {
	testTargets := []uint32{100, 149, 150, 199, 250, 299}

	for _, targetUID := range testTargets {
		targetUID := targetUID
		t.Run(fmt.Sprintf("TargetUID_%d", targetUID), func(t *testing.T) {
			st, err := store.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()

			ctx := context.Background()
			targetEmail := fmt.Sprintf("target_%d@icloud.com", targetUID)
			unrelatedEmail := "noise@icloud.com"

			now := time.Now().UTC()
			_ = st.CreateVerificationRequest(ctx, &store.VerificationRequest{
				RequestID:           fmt.Sprintf("vreq_%d", targetUID),
				PrincipalKind:       "token",
				PrincipalID:         "tok",
				LeaseID:             fmt.Sprintf("lease_%d", targetUID),
				AliasEmail:          targetEmail,
				Status:              "ready",
				CreatedAt:           now.Format(time.RFC3339),
				ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
				BaselineProvider:    "imap",
				BaselineMailbox:     "INBOX",
				BaselineUIDValidity: 1,
				BaselineUID:         100,
			})

			messages := make([]mail.Message, 0, 200)
			expectedCode := fmt.Sprintf("%06d", (targetUID*1000+123456)%1000000)
			for u := uint32(100); u <= 299; u++ {
				msg := mail.Message{
					ID:          fmt.Sprintf("%d", u),
					AccountID:   "acc_1",
					Folder:      "INBOX",
					UIDValidity: 1,
					UID:         u,
					Provider:    "imap",
				}
				if u == targetUID {
					msg.To = targetEmail
					msg.Subject = fmt.Sprintf("Your verification code is %s", expectedCode)
					msg.Preview = fmt.Sprintf("Verification code: %s", expectedCode)
				} else {
					msg.To = unrelatedEmail
					msg.Subject = fmt.Sprintf("Spam #%d", u)
					msg.Preview = "Spam body"
				}
				messages = append(messages, msg)
			}

			fb := newScanTestBackend("acc_1", messages, 300)
			eventBus := mail.NewEventBus(5 * time.Minute)
			worker := NewMailSyncWorker(fb, st, eventBus, 1*time.Second)

			subID, ch := eventBus.SubscribeWithBoundary(targetEmail, "INBOX", 1, 100)
			defer eventBus.Unsubscribe(targetEmail, subID)

			matched := worker.fetchAndPublishBatch(ctx, "acc_1", []string{targetEmail})
			if !matched {
				t.Fatalf("targetUID=%d 时 fetchAndPublishBatch 应匹配成功", targetUID)
			}

			select {
			case evt := <-ch:
				if evt.UID != targetUID {
					t.Fatalf("期望命中 UID %d, 实际: %d", targetUID, evt.UID)
				}
				if evt.OTP == nil || evt.OTP.Code != expectedCode {
					t.Fatalf("targetUID=%d 提取到的验证码不正确: %+v, 预期: %s", targetUID, evt.OTP, expectedCode)
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatalf("跨页边界 UID=%d 超时未收到验证码事件", targetUID)
			}
		})
	}
}

// Test 3: TestVerificationScan_MultipleAliasesAcrossDifferentPages
// 同一个 account: alias A 验证码在 page 1 (UID 105), alias B 在 page 3 (UID 210), alias C 在 page 4 (UID 280)。
// 一次 account scan，三者均收到自己的事件，且绝不串号。
func TestVerificationScan_MultipleAliasesAcrossDifferentPages(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	aliasA := "alias_a@icloud.com" // UID 105 (Page 1: 100..149)
	aliasB := "alias_b@icloud.com" // UID 210 (Page 3: 200..249)
	aliasC := "alias_c@icloud.com" // UID 280 (Page 4: 250..299)
	unrelated := "noise@icloud.com"

	now := time.Now().UTC()
	for i, alias := range []string{aliasA, aliasB, aliasC} {
		err := st.CreateVerificationRequest(ctx, &store.VerificationRequest{
			RequestID:           fmt.Sprintf("vreq_multi_%d", i),
			PrincipalKind:       "token",
			PrincipalID:         fmt.Sprintf("tok_%d", i),
			LeaseID:             fmt.Sprintf("lease_multi_%d", i),
			AliasEmail:          alias,
			Status:              "ready",
			CreatedAt:           now.Format(time.RFC3339),
			ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
			BaselineProvider:    "imap",
			BaselineMailbox:     "INBOX",
			BaselineUIDValidity: 1,
			BaselineUID:         100,
		})
		if err != nil {
			t.Fatalf("CreateVerificationRequest [%d] 失败: %v", i, err)
		}
	}

	messages := make([]mail.Message, 0, 200)
	for u := uint32(100); u <= 299; u++ {
		msg := mail.Message{
			ID:          fmt.Sprintf("%d", u),
			AccountID:   "acc_1",
			Folder:      "INBOX",
			UIDValidity: 1,
			UID:         u,
			Provider:    "imap",
		}
		switch u {
		case 105:
			msg.To = aliasA
			msg.Subject = "Code for A: 123456"
			msg.Preview = "Your code is 123456"
		case 210:
			msg.To = aliasB
			msg.Subject = "Code for B: 234567"
			msg.Preview = "Your code is 234567"
		case 280:
			msg.To = aliasC
			msg.Subject = "Code for C: 345678"
			msg.Preview = "Your code is 345678"
		default:
			msg.To = unrelated
			msg.Subject = "Noise"
			msg.Preview = "Noise"
		}
		messages = append(messages, msg)
	}

	fb := newScanTestBackend("acc_1", messages, 300)
	eventBus := mail.NewEventBus(5 * time.Minute)
	worker := NewMailSyncWorker(fb, st, eventBus, 1*time.Second)

	subA, chA := eventBus.SubscribeWithBoundary(aliasA, "INBOX", 1, 100)
	defer eventBus.Unsubscribe(aliasA, subA)
	subB, chB := eventBus.SubscribeWithBoundary(aliasB, "INBOX", 1, 100)
	defer eventBus.Unsubscribe(aliasB, subB)
	subC, chC := eventBus.SubscribeWithBoundary(aliasC, "INBOX", 1, 100)
	defer eventBus.Unsubscribe(aliasC, subC)

	matched := worker.fetchAndPublishBatch(ctx, "acc_1", []string{aliasA, aliasB, aliasC})
	if !matched {
		t.Fatal("多别名跨页批量扫描应成功")
	}

	// 验证 A 收到正确的事件
	select {
	case evt := <-chA:
		if evt.Email != aliasA || evt.UID != 105 || evt.OTP == nil || evt.OTP.Code != "123456" {
			t.Fatalf("Alias A 收到错误事件: %+v", evt)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Alias A 超时未收到事件")
	}

	// 验证 B 收到正确的事件
	select {
	case evt := <-chB:
		if evt.Email != aliasB || evt.UID != 210 || evt.OTP == nil || evt.OTP.Code != "234567" {
			t.Fatalf("Alias B 收到错误事件: %+v", evt)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Alias B 超时未收到事件")
	}

	// 验证 C 收到正确的事件
	select {
	case evt := <-chC:
		if evt.Email != aliasC || evt.UID != 280 || evt.OTP == nil || evt.OTP.Code != "345678" {
			t.Fatalf("Alias C 收到错误事件: %+v", evt)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Alias C 超时未收到事件")
	}
}

// Test 4: TestVerificationScan_NoDuplicatePublishAcrossPagesAndRetries
// 同一 page 因失败或 cursor rewind 被再次扫描。
// markPublished + MessageRef 确保: 同 alias + 同 UID + 同 UIDVALIDITY 最多 Publish 一次。
func TestVerificationScan_NoDuplicatePublishAcrossPagesAndRetries(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	targetEmail := "dedup_target@icloud.com"

	now := time.Now().UTC()
	_ = st.CreateVerificationRequest(ctx, &store.VerificationRequest{
		RequestID:           "vreq_dedup",
		PrincipalKind:       "token",
		PrincipalID:         "tok",
		LeaseID:             "lease_dedup",
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
			To:          targetEmail,
			Subject:     "Your verification code is 445566",
			Preview:     "Code: 445566",
			Provider:    "imap",
		},
	}

	fb := newScanTestBackend("acc_1", messages, 200)
	eventBus := mail.NewEventBus(5 * time.Minute)
	worker := NewMailSyncWorker(fb, st, eventBus, 1*time.Second)

	subID, ch := eventBus.SubscribeWithBoundary(targetEmail, "INBOX", 1, 100)
	defer eventBus.Unsubscribe(targetEmail, subID)

	// 第一次扫描发布事件
	_ = worker.fetchAndPublishBatch(ctx, "acc_1", []string{targetEmail})

	select {
	case evt := <-ch:
		if evt.UID != 105 || evt.OTP.Code != "445566" {
			t.Fatalf("第一次应收到正常验证码事件: %+v", evt)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("第一次超时未收到事件")
	}

	// 模拟 cursor rewind (例如新 subscriber 加入且带更早 baseline)
	worker.mu.Lock()
	cpKey := checkpointKey{accountID: "acc_1", mailbox: "INBOX", uidValidity: 1}
	worker.checkpoints[cpKey] = 100 // rewind cursor back to 100
	worker.mu.Unlock()

	// 第二次扫描同一页面
	_ = worker.fetchAndPublishBatch(ctx, "acc_1", []string{targetEmail})

	// 必须绝对不再收到重复事件
	select {
	case evt := <-ch:
		t.Fatalf("不应收到重复的事件: %+v", evt)
	case <-time.After(100 * time.Millisecond):
		// 正确: 幂等去重生效
	}
}

// Test 5: TestVerificationScan_CancelAfterPageThenResume
// page 1 (100..149) 成功，page 2 (150..199) 成功，page 3 (200..249) 拉取时 context canceled。
// checkpoint 只推进到 page 2 之后 (200)。
// 下一轮使用新 context 恢复，直接从 page 3 (200) 开始，不得重扫 page 1/2，也不得跳过 page 3。
func TestVerificationScan_CancelAfterPageThenResume(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	targetEmail := "resume_target@icloud.com"
	now := time.Now().UTC()
	_ = st.CreateVerificationRequest(context.Background(), &store.VerificationRequest{
		RequestID:           "vreq_resume",
		PrincipalKind:       "token",
		PrincipalID:         "tok",
		LeaseID:             "lease_resume",
		AliasEmail:          targetEmail,
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1,
		BaselineUID:         100,
	})

	messages := make([]mail.Message, 0, 150)
	for u := uint32(100); u <= 249; u++ {
		msg := mail.Message{
			ID:          fmt.Sprintf("%d", u),
			AccountID:   "acc_1",
			Folder:      "INBOX",
			UIDValidity: 1,
			UID:         u,
			Provider:    "imap",
		}
		if u == 215 { // 在第 3 页 (200..249)
			msg.To = targetEmail
			msg.Subject = "Code in Page 3: 778899"
			msg.Preview = "Your code is 778899"
		} else {
			msg.To = "noise@icloud.com"
			msg.Subject = "Noise"
			msg.Preview = "Noise"
		}
		messages = append(messages, msg)
	}

	fb := newScanTestBackend("acc_1", messages, 250)

	// 自定义 onScanMailboxUIDPage: 当扫描到 Page 3 (FromUID=200) 时取消 ctx 并返回 context.Canceled
	ctx1, cancel1 := context.WithCancel(context.Background())
	var pageScans []uint32

	origScan := fb.onScanMailboxUIDPage
	fb.onScanMailboxUIDPage = func(ctx context.Context, q ScanPageQuery) (ScanPageResult, error) {
		pageScans = append(pageScans, q.FromUIDInclusive)
		if q.FromUIDInclusive == 200 {
			cancel1() // 触发取消
			return ScanPageResult{}, context.Canceled
		}
		return origScan(ctx, q)
	}

	eventBus := mail.NewEventBus(5 * time.Minute)
	worker := NewMailSyncWorker(fb, st, eventBus, 1*time.Second)

	// 第一轮执行: 在 Page 3 处取消
	worker.fetchAndPublishBatch(ctx1, "acc_1", []string{targetEmail})

	// 校验 checkpoint 状态: 必须刚好停在 200 (Page 2 完成后的下一游标)，绝不能推进到 250
	cpKey := checkpointKey{accountID: "acc_1", mailbox: "INBOX", uidValidity: 1}
	worker.mu.RLock()
	cpVal := worker.checkpoints[cpKey]
	worker.mu.RUnlock()

	if cpVal != 200 {
		t.Fatalf("中断后 checkpoint 应当停在 200, 实际: %d", cpVal)
	}

	// 第二轮恢复执行: 传入新的 context，恢复原始未拦截的扫描
	fb.onScanMailboxUIDPage = origScan
	pageScans = nil

	subID, ch := eventBus.SubscribeWithBoundary(targetEmail, "INBOX", 1, 100)
	defer eventBus.Unsubscribe(targetEmail, subID)

	ctx2 := context.Background()
	matched2 := worker.fetchAndPublishBatch(ctx2, "acc_1", []string{targetEmail})
	if !matched2 {
		t.Fatal("第二轮恢复扫描应成功命中 Page 3 中的验证码")
	}

	select {
	case evt := <-ch:
		if evt.UID != 215 || evt.OTP.Code != "778899" {
			t.Fatalf("第二轮应命中 UID 215, 实际: %+v", evt)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("第二轮超时未收到验证码事件")
	}

	// 校验 checkpoint 已推进完成
	worker.mu.RLock()
	finalCp := worker.checkpoints[cpKey]
	worker.mu.RUnlock()

	if finalCp < 250 {
		t.Fatalf("扫描完成后 checkpoint 应推进到 >=250, 实际: %d", finalCp)
	}
}

// Test 6: TestVerificationScan_SnapshotUpperBound
// 扫描开始时 UIDNEXT = 201 (upper = 200)。
// 扫描过程中又新增 201..250。本轮只处理 <= 200。下一轮从 201 继续。
func TestVerificationScan_SnapshotUpperBound(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	targetEmail := "upper_target@icloud.com"
	now := time.Now().UTC()
	_ = st.CreateVerificationRequest(context.Background(), &store.VerificationRequest{
		RequestID:           "vreq_upper",
		PrincipalKind:       "token",
		PrincipalID:         "tok",
		LeaseID:             "lease_upper",
		AliasEmail:          targetEmail,
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1,
		BaselineUID:         100,
	})

	// 初始仅 100..200
	messages := make([]mail.Message, 0, 150)
	for u := uint32(100); u <= 200; u++ {
		messages = append(messages, mail.Message{
			ID:          fmt.Sprintf("%d", u),
			AccountID:   "acc_1",
			Folder:      "INBOX",
			UIDValidity: 1,
			UID:         u,
			To:          "noise@icloud.com",
			Subject:     "Noise",
			Preview:     "Noise",
			Provider:    "imap",
		})
	}

	var currentUIDNext uint32 = 201 // initial upper = 200
	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Status: "active", HasAppPassword: true}},
		mailboxBoundaryFunc: func(accID, folder string) (string, uint32, uint32, error) {
			return "imap", 1, atomic.LoadUint32(&currentUIDNext), nil
		},
	}

	var maxScannedUIDInRound1 uint32
	fb.onScanMailboxUIDPage = func(ctx context.Context, q ScanPageQuery) (ScanPageResult, error) {
		// 模拟扫描过程中，邮箱新增了 201..250 (含目标验证码在 210)
		if atomic.CompareAndSwapUint32(&currentUIDNext, 201, 251) {
			for u := uint32(201); u <= 250; u++ {
				msg := mail.Message{
					ID:          fmt.Sprintf("%d", u),
					AccountID:   "acc_1",
					Folder:      "INBOX",
					UIDValidity: 1,
					UID:         u,
					Provider:    "imap",
				}
				if u == 210 {
					msg.To = targetEmail
					msg.Subject = "Code in new mail: 998877"
					msg.Preview = "Your code is 998877"
				} else {
					msg.To = "noise@icloud.com"
					msg.Subject = "Noise"
					msg.Preview = "Noise"
				}
				messages = append(messages, msg)
			}
		}

		var matched []mail.Message
		for _, m := range messages {
			if m.UID >= q.FromUIDInclusive && m.UID <= q.ToUIDInclusive {
				matched = append(matched, m)
				if m.UID > maxScannedUIDInRound1 {
					maxScannedUIDInRound1 = m.UID
				}
			}
		}
		pageSize := q.PageSize
		hasMore := false
		if len(matched) > pageSize {
			matched = matched[:pageSize]
			hasMore = true
		}
		var nextUID uint32 = q.ToUIDInclusive + 1
		if len(matched) > 0 {
			nextUID = matched[len(matched)-1].UID + 1
		}
		return ScanPageResult{
			UIDValidity: 1,
			Messages:    matched,
			NextUID:     nextUID,
			HasMore:     hasMore,
		}, nil
	}

	fb.onGetMessagesContext = func(ctx context.Context, accID string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
		var out []*mail.FullMessage
		for _, r := range refs {
			for _, m := range messages {
				if m.UID == r.UID {
					out = append(out, &mail.FullMessage{Message: m, Body: m.Preview, BodyComplete: true, Provider: "imap", Method: "imap"})
					break
				}
			}
		}
		return out, nil
	}

	eventBus := mail.NewEventBus(5 * time.Minute)
	worker := NewMailSyncWorker(fb, st, eventBus, 1*time.Second)

	subID, ch := eventBus.SubscribeWithBoundary(targetEmail, "INBOX", 1, 100)
	defer eventBus.Unsubscribe(targetEmail, subID)

	// 第 1 轮扫描: upper bound 在本轮开始时固定为 200
	_ = worker.fetchAndPublishBatch(context.Background(), "acc_1", []string{targetEmail})

	// 必须满足: 第一轮绝不超过 upper bound (200)，UID 210 留给下一轮
	if maxScannedUIDInRound1 > 200 {
		t.Fatalf("第一轮扫描越界! 超过了本轮开始时固定的 upper bound (200), 最大扫描 UID: %d", maxScannedUIDInRound1)
	}

	select {
	case evt := <-ch:
		t.Fatalf("第一轮不应提前收到 >200 的事件: %+v", evt)
	case <-time.After(50 * time.Millisecond):
		// 正确: 第一轮不追逐新邮件
	}

	// 第 2 轮扫描: 从 201 继续扫到 250
	matched2 := worker.fetchAndPublishBatch(context.Background(), "acc_1", []string{targetEmail})
	if !matched2 {
		t.Fatal("第二轮应成功命中 UID 210")
	}

	select {
	case evt := <-ch:
		if evt.UID != 210 || evt.OTP.Code != "998877" {
			t.Fatalf("第二轮收到非预期事件: %+v", evt)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("第二轮超时未收到 UID 210 事件")
	}
}

// Test 7: TestVerificationScan_MetadataFirstAvoidsUnrelatedBodyFetch
// 200 封普通邮件，1 封目标 alias 邮件 (总计 201 封)。
// 验证: metadata fetch 覆盖全部 201 封，而 full body fetch 仅请求了 1 封 candidate 邮件，绝不全量拉 body。
func TestVerificationScan_MetadataFirstAvoidsUnrelatedBodyFetch(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	targetEmail := "candidate_target@icloud.com"
	unrelatedEmail := "noise@icloud.com"

	now := time.Now().UTC()
	_ = st.CreateVerificationRequest(context.Background(), &store.VerificationRequest{
		RequestID:           "vreq_meta_first",
		PrincipalKind:       "token",
		PrincipalID:         "tok",
		LeaseID:             "lease_meta_first",
		AliasEmail:          targetEmail,
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1,
		BaselineUID:         100,
	})

	messages := make([]mail.Message, 0, 201)
	for u := uint32(100); u <= 300; u++ {
		msg := mail.Message{
			ID:          fmt.Sprintf("%d", u),
			AccountID:   "acc_1",
			Folder:      "INBOX",
			UIDValidity: 1,
			UID:         u,
			Provider:    "imap",
		}
		if u == 155 {
			msg.To = targetEmail
			msg.Subject = "Security OTP: 334455"
			msg.Preview = "Your code is 334455"
		} else {
			msg.To = unrelatedEmail
			msg.Subject = "Noise"
			msg.Preview = "Heavy body payload..."
		}
		messages = append(messages, msg)
	}

	fb := newScanTestBackend("acc_1", messages, 301)

	var bodyFetchCount int32
	var requestedUIDs []uint32

	origGetMsgs := fb.onGetMessagesContext
	fb.onGetMessagesContext = func(ctx context.Context, accID string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
		atomic.AddInt32(&bodyFetchCount, int32(len(refs)))
		for _, r := range refs {
			requestedUIDs = append(requestedUIDs, r.UID)
		}
		return origGetMsgs(ctx, accID, refs)
	}

	eventBus := mail.NewEventBus(5 * time.Minute)
	worker := NewMailSyncWorker(fb, st, eventBus, 1*time.Second)

	subID, ch := eventBus.SubscribeWithBoundary(targetEmail, "INBOX", 1, 100)
	defer eventBus.Unsubscribe(targetEmail, subID)

	matched := worker.fetchAndPublishBatch(context.Background(), "acc_1", []string{targetEmail})
	if !matched {
		t.Fatal("fetchAndPublishBatch 应该匹配成功")
	}

	select {
	case evt := <-ch:
		if evt.UID != 155 || evt.OTP.Code != "334455" {
			t.Fatalf("收到非预期事件: %+v", evt)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("超时未收到事件")
	}

	// 关键断言: full body fetch 绝不能为 201 封，必须严格仅针对 candidate UID (1 封)
	totalBodyFetched := atomic.LoadInt32(&bodyFetchCount)
	if totalBodyFetched != 1 {
		t.Fatalf("全量正文拉取封数必须等于 1 (仅 candidate UID 155), 实际拉取了 %d 封", totalBodyFetched)
	}
	if len(requestedUIDs) != 1 || requestedUIDs[0] != 155 {
		t.Fatalf("拉取正文的 UID 必须是 [155], 实际: %v", requestedUIDs)
	}
}

// Test 8: TestNormalInboxLimitSemanticsUnchanged
// 普通收件箱 API (ListInbox limit=50) 仍然保持只返回 50 条最新邮件。
// PR-04A 内部增量扫描未污染或改变普通收件箱查询的语义与性能契约。
func TestNormalInboxLimitSemanticsUnchanged(t *testing.T) {
	messages := make([]mail.Message, 0, 100)
	for u := uint32(1); u <= 100; u++ {
		messages = append(messages, mail.Message{
			ID:          fmt.Sprintf("%d", u),
			Folder:      "INBOX",
			UIDValidity: 1,
			UID:         u,
			Subject:     fmt.Sprintf("Mail %d", u),
			Provider:    "imap",
		})
	}

	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Status: "active", HasAppPassword: true}},
		onListInboxContext: func(ctx context.Context, q InboxQuery) (InboxResult, error) {
			// 现有收件箱正常行为: 若邮件超过 limit，仅返回最新的 limit 条
			var out []mail.Message
			for _, m := range messages {
				out = append(out, m)
			}
			if q.Limit > 0 && len(out) > q.Limit {
				out = out[len(out)-q.Limit:] // newest limit
			}
			return InboxResult{
				AccountID: q.AccountID,
				Folder:    q.Folder,
				Count:     len(out),
				Messages:  out,
				Method:    "imap",
			}, nil
		},
	}

	// 调用普通 ListInbox
	res, err := fb.ListInbox(InboxQuery{
		AccountID: "acc_1",
		Folder:    "inbox",
		Limit:     50,
	})
	if err != nil {
		t.Fatalf("ListInbox 失败: %v", err)
	}

	if len(res.Messages) != 50 {
		t.Fatalf("普通收件箱 limit=50 应当严格返回 50 封邮件，实际返回: %d", len(res.Messages))
	}
	if res.Messages[0].UID != 51 || res.Messages[49].UID != 100 {
		t.Fatalf("普通收件箱应当保留最新的 50 封 (51..100), 实际首尾: %d..%d", res.Messages[0].UID, res.Messages[49].UID)
	}
}
