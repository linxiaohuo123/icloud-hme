package server

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"icloud-hme/internal/auth"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

// 等待期间先到达旧代际事件、再到达有效验证码：旧事件经当前邮箱边界确认后忽略，任务仍能成功取码。
func TestStaleGenerationLiveEventDoesNotBlockValidCode(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	alias := "target@icloud.com"
	fb := newScanTestBackend("acc", nil, 100)
	fb.onCaptureMailboxContext = func(ctx context.Context, _ string) (context.Context, string, string, error) {
		return ctx, "inbox", "inbox", nil
	}
	fb.mailboxBoundaryFunc = func(string, string) (string, uint32, uint32, error) { return "imap", 2, 100, nil }
	bus := mail.NewEventBus(time.Minute)
	w := NewMailSyncWorker(fb, st, bus, time.Second)
	w.RegisterAliasAccount(alias, "acc")
	svc := NewVerificationService(fb, st, bus, w)
	principal := auth.Principal{Kind: auth.PrincipalAdmin, ID: "admin", Scopes: []string{"admin"}}
	req, err := svc.CreateVerificationRequest(ctx, principal, alias)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		for bus.SubscriberCount(alias) == 0 {
			time.Sleep(time.Millisecond)
		}
		bus.PublishEvent(&mail.CachedOTP{Source: "inbox", EventID: "old", Email: alias, Folder: "INBOX", UIDValidity: 1, UID: 150, OTP: &mail.OTPResult{Code: "111111"}})
		bus.PublishEvent(&mail.CachedOTP{Source: "inbox", EventID: "new", Email: alias, Folder: "INBOX", UIDValidity: 2, UID: 101, OTP: &mail.OTPResult{Code: "222222"}})
	}()

	res, err := svc.GetVerificationResult(ctx, principal, req.RequestID, 5, "")
	if err != nil || res == nil || res.Status != "succeeded" || res.Code != "222222" {
		t.Fatalf("stale live event blocked valid code: res=%+v err=%v", res, err)
	}
}

// 当前邮箱确实已换代时，旧基线任务仍被事件驱动失效。
func TestConfirmedGenerationChangeStillInvalidates(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	alias := "target@icloud.com"
	fb := newScanTestBackend("acc", nil, 100)
	fb.onCaptureMailboxContext = func(ctx context.Context, _ string) (context.Context, string, string, error) {
		return ctx, "inbox", "inbox", nil
	}
	fb.mailboxBoundaryFunc = func(string, string) (string, uint32, uint32, error) { return "imap", 1, 100, nil }
	bus := mail.NewEventBus(time.Minute)
	svc := NewVerificationService(fb, st, bus, nil)
	principal := auth.Principal{Kind: auth.PrincipalAdmin, ID: "admin", Scopes: []string{"admin"}}
	w := NewMailSyncWorker(fb, st, bus, time.Second)
	w.RegisterAliasAccount(alias, "acc")
	svc.syncWorker = w
	req, err := svc.CreateVerificationRequest(ctx, principal, alias)
	if err != nil {
		t.Fatal(err)
	}
	// 邮箱重建：当前边界变为第 2 代，并发布第 2 代事件。
	fb.mailboxBoundaryFunc = func(string, string) (string, uint32, uint32, error) { return "imap", 2, 10, nil }
	bus.PublishEvent(&mail.CachedOTP{Source: "inbox", EventID: "gen2", Email: alias, Folder: "INBOX", UIDValidity: 2, UID: 11, OTP: &mail.OTPResult{Code: "333333"}})

	_, err = svc.GetVerificationResult(ctx, principal, req.RequestID, 0, "")
	if !errors.Is(err, ErrUIDValidityChanged) {
		t.Fatalf("confirmed generation change must invalidate, got err=%v", err)
	}
	got, err := st.GetVerificationRequest(ctx, req.RequestID, "admin", "admin")
	if err != nil || got.Status != "invalidated" {
		t.Fatalf("status=%v err=%v", got, err)
	}
}

// 基线母号被删除后来源不可恢复，GET 持久化失效而不是每次轮询返回 500。
func TestDeletedBaselineAccountInvalidatesRequest(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	alias := "target@icloud.com"
	fb := newScanTestBackend("acc", nil, 100)
	fb.onCaptureMailboxContext = func(ctx context.Context, _ string) (context.Context, string, string, error) {
		return ctx, "inbox", "inbox", nil
	}
	fb.mailboxBoundaryFunc = func(string, string) (string, uint32, uint32, error) { return "imap", 1, 100, nil }
	bus := mail.NewEventBus(time.Minute)
	svc := NewVerificationService(fb, st, bus, nil)
	w := NewMailSyncWorker(fb, st, bus, time.Second)
	w.RegisterAliasAccount(alias, "acc")
	svc.syncWorker = w
	principal := auth.Principal{Kind: auth.PrincipalAdmin, ID: "admin", Scopes: []string{"admin"}}
	req, err := svc.CreateVerificationRequest(ctx, principal, alias)
	if err != nil {
		t.Fatal(err)
	}
	fb.onCaptureMailboxContext = func(ctx context.Context, id string) (context.Context, string, string, error) {
		return ctx, "", "", fmt.Errorf("账号不存在: %s", id)
	}
	_, err = svc.GetVerificationResult(ctx, principal, req.RequestID, 0, "")
	if !errors.Is(err, ErrUIDValidityChanged) {
		t.Fatalf("deleted account must invalidate request, got err=%v", err)
	}
	got, err := st.GetVerificationRequest(ctx, req.RequestID, "admin", "admin")
	if err != nil || got.Status != "invalidated" {
		t.Fatalf("status=%v err=%v", got, err)
	}
}

// 有效验证码在等待方确认旧事件期间到达：忽略旧事件后必须回查缓存，而不是继续空等到超时。
func TestStaleGenerationFallsBackToCachedMatch(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	alias := "target@icloud.com"
	fb := newScanTestBackend("acc", nil, 100)
	fb.onCaptureMailboxContext = func(ctx context.Context, _ string) (context.Context, string, string, error) {
		return ctx, "inbox", "inbox", nil
	}
	fb.mailboxBoundaryFunc = func(string, string) (string, uint32, uint32, error) { return "imap", 2, 100, nil }
	bus := mail.NewEventBus(time.Minute)
	svc := NewVerificationService(fb, st, bus, nil)
	principal := auth.Principal{Kind: auth.PrincipalAdmin, ID: "admin", Scopes: []string{"admin"}}
	w := NewMailSyncWorker(fb, st, bus, time.Second)
	w.RegisterAliasAccount(alias, "acc")
	svc.syncWorker = w
	req, err := svc.CreateVerificationRequest(ctx, principal, alias)
	if err != nil {
		t.Fatal(err)
	}
	bus.PublishEvent(&mail.CachedOTP{Source: "inbox", EventID: "old", Email: alias, Folder: "INBOX", UIDValidity: 1, UID: 150, OTP: &mail.OTPResult{Code: "111111"}})
	published := false
	fb.mailboxBoundaryFunc = func(string, string) (string, uint32, uint32, error) {
		if !published {
			published = true
			bus.PublishEvent(&mail.CachedOTP{Source: "inbox", EventID: "new", Email: alias, Folder: "INBOX", UIDValidity: 2, UID: 101, OTP: &mail.OTPResult{Code: "222222"}})
		}
		return "imap", 2, 100, nil
	}

	res, err := svc.GetVerificationResult(ctx, principal, req.RequestID, 0, "")
	if err != nil || res == nil || res.Status != "succeeded" || res.Code != "222222" {
		t.Fatalf("valid cached code not recovered after stale event: res=%+v err=%v", res, err)
	}
}

func TestCachedOldGenerationMustNotInvalidateNewRequest(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	alias := "target@icloud.com"
	fb := newScanTestBackend("acc", nil, 100)
	fb.onCaptureMailboxContext = func(ctx context.Context, _ string) (context.Context, string, string, error) {
		return ctx, "same-physical-inbox", "same-physical-inbox", nil
	}
	fb.mailboxBoundaryFunc = func(string, string) (string, uint32, uint32, error) { return "imap", 2, 100, nil }
	bus := mail.NewEventBus(time.Minute)
	// Real cache contains an event published by a previous scan, before mailbox rebuild.
	bus.PublishEvent(&mail.CachedOTP{Source: "same-physical-inbox", Email: alias, Folder: "INBOX", UIDValidity: 1, UID: 150, OTP: &mail.OTPResult{Code: "111111"}})
	w := NewMailSyncWorker(fb, st, bus, time.Second)
	w.RegisterAliasAccount(alias, "acc")
	svc := NewVerificationService(fb, st, bus, w)
	principal := auth.Principal{Kind: auth.PrincipalAdmin, ID: "admin", Scopes: []string{"admin"}}
	req, err := svc.CreateVerificationRequest(ctx, principal, alias)
	if err != nil || req.BaselineUIDValidity != 2 {
		t.Fatalf("create: %+v %v", req, err)
	}
	result, getErr := svc.GetVerificationResult(ctx, principal, req.RequestID, 0, "")
	got, err := st.GetVerificationRequest(ctx, req.RequestID, "admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "ready" || getErr != nil {
		t.Fatalf("old generation cache killed freshly captured generation=2 task: status=%s result=%+v err=%v", got.Status, result, getErr)
	}
}

func TestOldScanBoundaryMustNotInvalidateNewGenerationRequest(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	alias := "target@icloud.com"
	now := time.Now().UTC()
	if err := st.CreateVerificationRequest(ctx, &store.VerificationRequest{RequestID: "old", PrincipalKind: "admin", PrincipalID: "admin", LeaseID: "old", AliasEmail: alias, Status: "ready", CreatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339), BaselineProvider: "imap", BaselineMailbox: "INBOX", BaselineUIDValidity: 1, BaselineUID: 100, BaselineSource: "same-inbox", BaselineAccountID: "acc"}); err != nil {
		t.Fatal(err)
	}
	fb := newScanTestBackend("acc", nil, 100)
	fb.onCaptureMailboxContext = func(ctx context.Context, _ string) (context.Context, string, string, error) {
		return ctx, "same-inbox", "same-inbox", nil
	}
	bus := mail.NewEventBus(time.Minute)
	w := NewMailSyncWorker(fb, st, bus, time.Second)
	w.RegisterAliasAccount(alias, "acc")
	svc := NewVerificationService(fb, st, bus, w)
	principal := auth.Principal{Kind: auth.PrincipalAdmin, ID: "admin", Scopes: []string{"admin"}}
	newID := ""
	fb.mailboxBoundaryFunc = func(string, string) (string, uint32, uint32, error) {
		// Worker has read generation=1. Mailbox rebuild and POST both happen before it consumes that boundary.
		if _, _, err := st.InvalidateVerificationRequest(ctx, "old"); err != nil {
			t.Fatal(err)
		}
		fb.mailboxBoundaryFunc = func(string, string) (string, uint32, uint32, error) { return "imap", 2, 10, nil }
		req, err := svc.CreateVerificationRequest(ctx, principal, alias)
		if err != nil || req.BaselineUIDValidity != 2 {
			t.Fatalf("new generation create: %+v %v", req, err)
		}
		newID = req.RequestID
		return "imap", 1, 100, nil
	}
	w.syncOnce()
	got, err := st.GetVerificationRequest(ctx, newID, "admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "ready" {
		t.Fatalf("old scan generation=1 invalidated freshly created generation=2 task: %+v", got)
	}
}
