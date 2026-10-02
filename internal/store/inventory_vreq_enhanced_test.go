package store

import (
	"context"
	"testing"
	"time"
)

func TestStore_VerificationRequest_IdempotencyAndBatch(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	exp := now.Add(10 * time.Minute).Format(time.RFC3339)

	// 1. 创建带幂等键的任务
	req := &VerificationRequest{
		RequestID:           "vreq-idemp-1",
		PrincipalKind:       "token",
		PrincipalID:         "tok-abc",
		LeaseID:             "lease-1",
		AliasEmail:          "alias1@icloud.com",
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           exp,
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 12345,
		BaselineUID:         100,
		IdempotencyKey:      "idemp-key-1",
		IdempotencyHash:     "hash-12345",
	}

	if err := st.CreateVerificationRequestAtomic(ctx, req, 100, 50); err != nil {
		t.Fatalf("CreateVerificationRequestAtomic failed: %v", err)
	}

	// 2. 根据幂等键查回
	recovered, err := st.GetVerificationRequestByIdempotencyKey(ctx, "token", "tok-abc", "idemp-key-1")
	if err != nil {
		t.Fatalf("GetVerificationRequestByIdempotencyKey failed: %v", err)
	}
	if recovered == nil {
		t.Fatalf("expected recovered request, got nil")
	}
	if recovered.RequestID != "vreq-idemp-1" || recovered.IdempotencyHash != "hash-12345" {
		t.Fatalf("recovered mismatch: %+v", recovered)
	}

	// 3. 跨主体隔离
	diffPrincipal, err := st.GetVerificationRequestByIdempotencyKey(ctx, "token", "tok-diff", "idemp-key-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diffPrincipal != nil {
		t.Fatalf("expected nil for different principal")
	}

	// 4. 只读预检测试: 同 lease 活跃冲突
	err = st.CheckVerificationRequestAdmission(ctx, "token", "tok-abc", "lease-1", 100, 50)
	if err != ErrConflictActiveRequest {
		t.Fatalf("expected ErrConflictActiveRequest, got %v", err)
	}

	// 5. 批量查询基线 GetMinBaselineUIDsByEmails
	req2 := &VerificationRequest{
		RequestID:           "vreq-idemp-2",
		PrincipalKind:       "token",
		PrincipalID:         "tok-abc",
		LeaseID:             "lease-2",
		AliasEmail:          "alias2@icloud.com",
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           exp,
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 12345,
		BaselineUID:         250,
	}
	if err := st.CreateVerificationRequestAtomic(ctx, req2, 100, 50); err != nil {
		t.Fatalf("CreateVerificationRequestAtomic req2 failed: %v", err)
	}

	baselines, err := st.GetMinBaselineUIDsByEmails(ctx, []string{"alias1@icloud.com", "alias2@icloud.com", "nonexistent@icloud.com"})
	if err != nil {
		t.Fatalf("GetMinBaselineUIDsByEmails failed: %v", err)
	}
	if len(baselines) != 2 {
		t.Fatalf("expected 2 baselines, got %d: %+v", len(baselines), baselines)
	}
	if baselines["alias1@icloud.com"] != 100 || baselines["alias2@icloud.com"] != 250 {
		t.Fatalf("baselines values mismatch: %+v", baselines)
	}

	// 6. 批量代际失效测试 (传入不匹配的 uidValidity 99999)
	affected, err := st.InvalidateVerificationRequestsForGenerationMismatchBatch(ctx, []string{"alias1@icloud.com", "alias2@icloud.com"}, "INBOX", 99999)
	if err != nil {
		t.Fatalf("batch invalidate failed: %v", err)
	}
	if affected != 2 {
		t.Fatalf("expected 2 invalidated, got %d", affected)
	}

	// 再次查询基线应为空
	baselinesAfter, err := st.GetMinBaselineUIDsByEmails(ctx, []string{"alias1@icloud.com", "alias2@icloud.com"})
	if err != nil {
		t.Fatalf("GetMinBaselineUIDsByEmails after invalidate failed: %v", err)
	}
	if len(baselinesAfter) != 0 {
		t.Fatalf("expected 0 baselines after invalidate, got %+v", baselinesAfter)
	}
}

func TestStore_CompleteMatchingVerificationRequests_StrictErrorPropagation(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	exp := now.Add(10 * time.Minute).Format(time.RFC3339)

	req := &VerificationRequest{
		RequestID:           "vreq-match-1",
		PrincipalKind:       "token",
		PrincipalID:         "tok-abc",
		LeaseID:             "lease-match-1",
		AliasEmail:          "match@icloud.com",
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           exp,
		BaselineProvider:    "imap",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1000,
		BaselineUID:         50,
	}
	if err := st.CreateVerificationRequestAtomic(ctx, req, 100, 50); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// 匹配成功
	completed, err := st.CompleteMatchingVerificationRequests(ctx, VerificationEventInput{
		AliasEmail:  "match@icloud.com",
		Provider:    "imap",
		Mailbox:     "INBOX",
		UIDValidity: 1000,
		UID:         55,
		MessageRef:  "ref-msg-1",
		Code:        "123456",
		MagicLink:   "https://verify.apple.com",
		Now:         now,
	})
	if err != nil {
		t.Fatalf("CompleteMatchingVerificationRequests failed: %v", err)
	}
	if len(completed) != 1 || completed[0].Code != "123456" {
		t.Fatalf("expected 1 completed request with code 123456, got %+v", completed)
	}

	// 再次调用，已无活跃匹配任务，合法返回 nil, nil
	completedAgain, err := st.CompleteMatchingVerificationRequests(ctx, VerificationEventInput{
		AliasEmail:  "match@icloud.com",
		Provider:    "imap",
		Mailbox:     "INBOX",
		UIDValidity: 1000,
		UID:         56,
		Code:        "654321",
		Now:         now,
	})
	if err != nil {
		t.Fatalf("expected nil error on zero matches, got %v", err)
	}
	if len(completedAgain) != 0 {
		t.Fatalf("expected 0 completed requests, got %d", len(completedAgain))
	}
}

func TestStore_LockContext_Cancellation(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 占住锁
	st.mu.Lock()

	// 派生已取消的 Context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = st.mu.LockContext(ctx)
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	// 释放原本的锁
	st.mu.Unlock()

	// 随后正常加锁应能成功
	ctxNormal := context.Background()
	if err := st.mu.LockContext(ctxNormal); err != nil {
		t.Fatalf("expected success on normal LockContext, got %v", err)
	}
	st.mu.Unlock()
}
