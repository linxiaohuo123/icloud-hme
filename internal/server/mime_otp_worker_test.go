// [POS]: 本机 IMAP 到 worker 持久化结果及事件交付回归
// [PROTOCOL]: 变更时检查 CLAUDE.md
package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

func TestMIMEOTPRealIMAPToDurableWorkerResult(t *testing.T) {
	mbox, pool, host, port := newCapacityIMAP(t)
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const alias = "mime@icloud.com"
	if err := mbox.Mailbox.CreateMessage(nil, time.Now(), strings.NewReader("Subject: Earlier mail\r\n\r\nNo code\r\n")); err != nil {
		t.Fatal(err)
	}
	const accountID = "mime-account"
	raw := "From: verification@example.invalid\r\nTo: " + alias + "\r\nSubject: Verification code\r\nContent-Type: multipart/alternative; boundary=otp\r\n\r\n--otp\r\nContent-Type: text/plain\r\n\r\nYour verification code is 482019\r\n--otp\r\nContent-Type: text/html\r\n\r\n<p>482019</p><p>Expires in ten minutes</p>\r\n--otp--\r\n"
	if err := mbox.Mailbox.CreateMessage(nil, time.Now(), strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	do := func(ctx context.Context, fn func(*mail.Client) error) error {
		return pool.DoContextWithServer(ctx, "shared@invalid.example", "password", host, port, "", fn)
	}
	var validity uint32
	if err := do(context.Background(), func(c *mail.Client) error {
		var err error
		validity, _, err = c.GetMailboxBoundary("INBOX")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	fb := &fakeBackend{accounts: []account.Summary{{ID: accountID}}}
	fb.onGetMailboxEndpointFingerprint = func(string) (string, bool) { return "mime-source", true }
	fb.onGetMailboxBoundaryContext = func(ctx context.Context, _ string, folder string) (string, uint32, uint32, error) {
		var v, n uint32
		err := do(ctx, func(c *mail.Client) error { var err error; v, n, err = c.GetMailboxBoundary(folder); return err })
		return "imap", v, n, err
	}
	scans := 0
	fb.onScanMailboxUIDPage = func(ctx context.Context, q ScanPageQuery) (ScanPageResult, error) {
		scans++
		var page mail.ScanPageResult
		err := do(ctx, func(c *mail.Client) error {
			var err error
			page, err = c.ScanMailboxUIDPage(mail.ScanPageOptions{Folder: q.Folder, FromUIDInclusive: q.FromUIDInclusive, ToUIDInclusive: q.ToUIDInclusive, PageSize: q.PageSize})
			return err
		})
		for i := range page.Messages {
			page.Messages[i].AccountID = accountID
			page.Messages[i].Provider = "imap"
		}
		return ScanPageResult{Messages: page.Messages, UIDValidity: page.UIDValidity, NextUID: page.NextUID, HasMore: page.HasMore}, err
	}
	fb.onGetMessagesContext = func(ctx context.Context, _ string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
		var messages []*mail.FullMessage
		uids := make([]uint32, len(refs))
		for i, ref := range refs {
			uids[i] = ref.UID
		}
		err := do(ctx, func(c *mail.Client) error {
			var err error
			messages, err = c.GetFullBatchInFolderWithValidity("INBOX", validity, uids)
			return err
		})
		for _, msg := range messages {
			msg.AccountID = accountID
			msg.Provider = "imap"
		}
		return messages, err
	}
	bus := mail.NewEventBus(time.Minute)
	worker := NewMailSyncWorker(fb, st, bus, time.Second)
	worker.RegisterAliasAccount(alias, accountID)
	_, ch := bus.SubscribeWithBoundary(alias, "INBOX", validity, 1)
	now := time.Now().UTC()
	if err := st.CreateVerificationRequest(context.Background(), &store.VerificationRequest{
		RequestID: "mime-request", PrincipalKind: "token", PrincipalID: "owner", AliasEmail: alias, LeaseID: "mime-lease", Status: "ready",
		CreatedAt: now.Add(-time.Second).Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339),
		BaselineProvider: "imap", BaselineMailbox: "INBOX", BaselineUIDValidity: validity, BaselineUID: 1,
	}); err != nil {
		t.Fatal(err)
	}
	worker.syncOnce()
	worker.syncOnce()
	req, err := st.GetVerificationRequest(context.Background(), "mime-request", "token", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != "succeeded" || req.Code != "482019" || req.MatchedEventRef == "" {
		t.Fatalf("real MIME worker result: %+v scans=%d", req, scans)
	}
	select {
	case event := <-ch:
		if event.OTP == nil || event.OTP.Code != req.Code {
			t.Fatalf("event=%+v", event)
		}
	default:
		t.Fatal("no durable completion event")
	}
	if scans != 1 {
		t.Fatalf("completed message rescanned %d times", scans)
	}
}
