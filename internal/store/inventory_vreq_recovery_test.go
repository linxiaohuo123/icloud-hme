/**
 * [INPUT]: 依赖 context, testing, time, icloud-hme/internal/store
 * [OUTPUT]: 提供 TestStore_ActiveVerificationWatches_OnlyUnexpired, TestStore_CompleteMatchingVerificationRequests_AtomicCASAndFilters, TestStore_MagicLinkIndependentPersistence
 * [POS]: internal/store 的 PR-04B 持久化恢复与原子匹配测试集
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"context"
	"testing"
	"time"
)

// 验证 ListActiveVerificationWatches 与 HasActiveVerificationRequests 仅返回 status IN ('ready', 'pending') 且未过期的任务
func TestStore_ActiveVerificationWatches_OnlyUnexpired(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now().UTC()

	// 1. 创建一个正常未过期的 ready 任务
	err = st.CreateVerificationRequest(ctx, &VerificationRequest{
		RequestID:           "vreq_active_1",
		PrincipalKind:       "token",
		PrincipalID:         "tok_1",
		LeaseID:             "lease_1",
		AliasEmail:          "active@icloud.com",
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(5 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 10,
		BaselineUID:         100,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 2. 创建一个已过期的任务
	err = st.CreateVerificationRequest(ctx, &VerificationRequest{
		RequestID:           "vreq_expired_1",
		PrincipalKind:       "token",
		PrincipalID:         "tok_1",
		LeaseID:             "lease_2",
		AliasEmail:          "expired@icloud.com",
		Status:              "ready",
		CreatedAt:           now.Add(-10 * time.Minute).Format(time.RFC3339),
		ExpiresAt:           now.Add(-1 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 10,
		BaselineUID:         100,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 3. 创建一个已完成 succeeded 的任务
	err = st.CreateVerificationRequest(ctx, &VerificationRequest{
		RequestID:           "vreq_succeeded_1",
		PrincipalKind:       "token",
		PrincipalID:         "tok_1",
		LeaseID:             "lease_3",
		AliasEmail:          "succeeded@icloud.com",
		Status:              "succeeded",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(5 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 10,
		BaselineUID:         100,
	})
	if err != nil {
		t.Fatal(err)
	}

	hasActive, err := st.HasActiveVerificationRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !hasActive {
		t.Fatal("应当存在活跃验证请求")
	}

	watches, err := st.ListActiveVerificationWatches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(watches) != 1 {
		t.Fatalf("预期仅有 1 个活跃 watch, 实际获取: %d (%+v)", len(watches), watches)
	}
	if watches[0].RequestID != "vreq_active_1" || watches[0].AliasEmail != "active@icloud.com" {
		t.Fatalf("活跃 watch 内容不匹配: %+v", watches[0])
	}
	if watches[0].BaselineUID != 100 || watches[0].BaselineUIDValidity != 10 {
		t.Fatalf("基线参数不匹配: %+v", watches[0])
	}
}

// 验证 CompleteMatchingVerificationRequests 的原子匹配、多重基线过滤与 CAS 终态保护
func TestStore_CompleteMatchingVerificationRequests_AtomicCASAndFilters(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now().UTC()

	// 任务 A: 目标别名，UIDValidity=1, baseline=200
	_ = st.CreateVerificationRequest(ctx, &VerificationRequest{
		RequestID:           "vreq_match_a",
		PrincipalKind:       "token",
		PrincipalID:         "tok_1",
		LeaseID:             "lease_a",
		AliasEmail:          "target@icloud.com",
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1,
		BaselineUID:         200,
	})

	// 任务 B: 目标别名，但 baseline=300 (更晚)
	_ = st.CreateVerificationRequest(ctx, &VerificationRequest{
		RequestID:           "vreq_match_b",
		PrincipalKind:       "token",
		PrincipalID:         "tok_1",
		LeaseID:             "lease_b",
		AliasEmail:          "target@icloud.com",
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1,
		BaselineUID:         300,
	})

	// 任务 C: 不同 UIDValidity (例如 2)
	_ = st.CreateVerificationRequest(ctx, &VerificationRequest{
		RequestID:           "vreq_diff_val",
		PrincipalKind:       "token",
		PrincipalID:         "tok_1",
		LeaseID:             "lease_c",
		AliasEmail:          "target@icloud.com",
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 2,
		BaselineUID:         200,
	})

	// 事件: UID=250, UIDValidity=1
	completed, err := st.CompleteMatchingVerificationRequests(ctx, VerificationEventInput{
		AliasEmail:  "TARGET@ICLOUD.COM", // 大小写不敏感
		Provider:    "imap",
		Mailbox:     "INBOX",
		UIDValidity: 1,
		UID:         250,
		MessageRef:  "ref_250",
		Code:        "123456",
		MagicLink:   "https://apple.com/auth?token=abc",
		Now:         now,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 预期: 仅任务 A 被匹配成功完成 (UID 250 >= 200, 但 250 < 300, UIDValidity 1 != 2)
	if len(completed) != 1 {
		t.Fatalf("预期仅完成 1 个任务, 实际完成: %d", len(completed))
	}
	if completed[0].RequestID != "vreq_match_a" {
		t.Fatalf("完成的任务不是 A: %+v", completed[0])
	}
	if completed[0].Code != "123456" || completed[0].MagicLink != "https://apple.com/auth?token=abc" {
		t.Fatalf("完成的结果内容不正确: %+v", completed[0])
	}

	// 再次传入相同或新的事件，任务 A 不得被二次覆盖 (CAS 保护)
	completedSecond, err := st.CompleteMatchingVerificationRequests(ctx, VerificationEventInput{
		AliasEmail:  "target@icloud.com",
		Provider:    "imap",
		Mailbox:     "INBOX",
		UIDValidity: 1,
		UID:         260,
		MessageRef:  "ref_260",
		Code:        "999999",
		MagicLink:   "https://apple.com/new",
		Now:         now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(completedSecond) != 0 {
		t.Fatalf("已 succeeded 的任务不得再次被完成: %+v", completedSecond)
	}

	// 验证库内任务 A 仍为第一次的 123456
	reqA, err := st.GetVerificationRequest(ctx, "vreq_match_a", "token", "tok_1")
	if err != nil {
		t.Fatal(err)
	}
	if reqA.Code != "123456" || reqA.Status != "succeeded" {
		t.Fatalf("任务 A 终态遭到篡改: %+v", reqA)
	}
}

// 验证 MagicLink 独立持久化，不再被粗暴塞进 Code
func TestStore_MagicLinkIndependentPersistence(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now().UTC()

	req := &VerificationRequest{
		RequestID:           "vreq_magic_test",
		PrincipalKind:       "token",
		PrincipalID:         "tok_magic",
		LeaseID:             "lease_magic",
		AliasEmail:          "magic@icloud.com",
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1,
		BaselineUID:         100,
	}
	if err := st.CreateVerificationRequest(ctx, req); err != nil {
		t.Fatal(err)
	}

	// 使用 CompleteVerificationRequestResult 同时持久化独立 Code 和 MagicLink
	compReq, won, err := st.CompleteVerificationRequestResult(ctx, "vreq_magic_test", VerificationCompletion{
		Code:            "654321",
		MagicLink:       "https://verify.apple.com/magic?code=654321",
		MatchedEventRef: "ref_m1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !won || compReq == nil {
		t.Fatal("CAS 应当成功")
	}

	// 查出验证
	got, err := st.GetVerificationRequest(ctx, "vreq_magic_test", "token", "tok_magic")
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != "654321" {
		t.Fatalf("Code 字段预期 654321, 实际: %s", got.Code)
	}
	if got.MagicLink != "https://verify.apple.com/magic?code=654321" {
		t.Fatalf("MagicLink 字段预期独立 URL, 实际: %s", got.MagicLink)
	}
}
