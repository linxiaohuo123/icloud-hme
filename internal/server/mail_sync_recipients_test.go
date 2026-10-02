// [INPUT]: Shared-inbox worker, structural recipient parsing and real temporary SQLite.
// [OUTPUT]: Multiple-recipient ownership/isolation regression and actual worker dispatch benchmark.
// [POS]: internal/server shared-inbox recipient matching correctness and cost.
// [PROTOCOL]: Update this header and CLAUDE.md when changing this file.
package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

func TestMailSync_SharedInboxMultipleStructuralRecipients(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	bus := mail.NewEventBus(time.Minute)
	fb := &fakeBackend{accounts: []account.Summary{{ID: "recipient_acc_0"}, {ID: "recipient_acc_1"}, {ID: "recipient_acc_2"}}}
	fb.onGetMailboxEndpointFingerprint = func(string) (string, bool) { return "shared-recipients", true }
	fb.onGetMailboxBoundaryContext = func(context.Context, string, string) (string, uint32, uint32, error) {
		return "imap", 7, 13, nil
	}
	msg := mail.Message{AccountID: "representative", Provider: "imap", Folder: "INBOX", UIDValidity: 7, UID: 12,
		To:      `First <recipient_0@icloud.com>, Second <RECIPIENT_1@icloud.com>, recipient_0@icloud.com`,
		Subject: "Your verification code is 567890"}
	full := &mail.FullMessage{Message: msg, Body: "Code: 567890. Mention recipient_2@icloud.com only in the body.", BodyComplete: true}
	fb.onScanMailboxUIDPage = func(context.Context, ScanPageQuery) (ScanPageResult, error) {
		return ScanPageResult{UIDValidity: 7, NextUID: 13, Messages: []mail.Message{msg}}, nil
	}
	fb.onGetMessagesContext = func(context.Context, string, []mail.MessageRef) ([]*mail.FullMessage, error) {
		return []*mail.FullMessage{full}, nil
	}
	worker := NewMailSyncWorker(fb, st, bus, time.Second)
	channels := make([]chan *mail.CachedOTP, 3)
	now := time.Now().UTC()
	for i := range channels {
		alias := fmt.Sprintf("recipient_%d@icloud.com", i)
		accountID := fmt.Sprintf("recipient_acc_%d", i)
		worker.RegisterAliasAccount(alias, accountID)
		_, channels[i] = bus.SubscribeWithBoundary(alias, "INBOX", 7, 10)
		if err := st.CreateVerificationRequest(context.Background(), &store.VerificationRequest{
			RequestID: fmt.Sprintf("recipient_req_%d", i), PrincipalKind: "token", PrincipalID: "recipient-owner",
			LeaseID: fmt.Sprintf("recipient_lease_%d", i), AliasEmail: alias, Status: "ready",
			CreatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339),
			BaselineProvider: "imap", BaselineMailbox: "INBOX", BaselineUIDValidity: 7, BaselineUID: 10,
		}); err != nil {
			t.Fatal(err)
		}
	}
	worker.syncOnce()
	for i, ch := range channels {
		req, err := st.GetVerificationRequest(context.Background(), fmt.Sprintf("recipient_req_%d", i), "token", "recipient-owner")
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			if req.Status != "ready" || req.Code != "" || req.MatchedEventRef != "" {
				t.Fatalf("body-only address received a durable result: %+v", req)
			}
			select {
			case ev := <-ch:
				t.Fatalf("body-only address received an event: %+v", ev)
			default:
			}
			continue
		}
		accountID := fmt.Sprintf("recipient_acc_%d", i)
		ref, err := mail.ParseMessageRef(req.MatchedEventRef, accountID)
		if err != nil || ref.AccountID != accountID || req.Status != "succeeded" || req.Code != "567890" {
			t.Fatalf("incorrect durable recipient ownership: request=%+v ref=%+v err=%v", req, ref, err)
		}
		select {
		case ev := <-ch:
			if ev.AccountID != accountID || ev.OTP == nil || ev.OTP.Code != req.Code {
				t.Fatalf("incorrect recipient event: %+v", ev)
			}
		default:
			t.Fatal("structural recipient did not receive an event")
		}
	}
	if full.AccountID != msg.AccountID || full.To != msg.To {
		t.Fatal("dispatch mutated the shared source message")
	}
}

func BenchmarkMailSyncSharedInboxRecipients(b *testing.B) {
	const aliasCount = 2000
	aliases := make([]string, aliasCount)
	for i := range aliases {
		aliases[i] = fmt.Sprintf("bench_%04d@icloud.com", i)
	}
	messages := make([]mail.Message, 40)
	for i := range messages {
		messages[i] = mail.Message{To: aliases[i], UID: uint32(i + 1), UIDValidity: 1,
			Folder: "INBOX", Provider: "imap", Subject: "Your verification code is 123456"}
	}
	fb := &fakeBackend{inbox: InboxResult{Messages: messages}}
	worker := NewMailSyncWorker(fb, nil, mail.NewEventBus(time.Minute), time.Second)
	batch := &aggregatedInboxBatch{repAccountID: "bench-account", allAliases: aliases}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := worker.fetchAndPublishLegacyBatch(context.Background(), batch, aliases); err != nil {
			b.Fatal(err)
		}
	}
}
