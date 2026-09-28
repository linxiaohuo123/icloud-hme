package server

import (
	"context"
	"testing"

	"icloud-hme/internal/mail"
)

func TestLegacyUIDBatchUsesFetchedCurrentGeneration(t *testing.T) {
	accountID := "legacy_batch_account"
	ref := mail.MessageRef{Provider: "imap", AccountID: accountID, Mailbox: "INBOX", UIDValidity: 10, UID: 7}
	fb := &fakeBackend{onGetMessagesContext: func(context.Context, string, []mail.MessageRef) ([]*mail.FullMessage, error) {
		return []*mail.FullMessage{{Message: mail.Message{
			UID: 7, UIDValidity: 10, Folder: "INBOX", AccountID: accountID, MessageRef: ref.Encode(),
		}, Body: "current"}}, nil
	}}
	service := NewMailReadService(fb)
	messages, items, err := service.GetMessagesBatch(context.Background(), accountID, []batchMessageItemReq{{UID: "7", Folder: "INBOX"}})
	if err != nil || len(messages) != 1 || items[0].Message == nil {
		t.Fatalf("legacy UID lost current message: messages=%d items=%+v err=%v", len(messages), items, err)
	}
	if items[0].Message.UIDValidity != 10 {
		t.Fatalf("legacy UID result lost fetched generation: %+v", items[0].Message)
	}
}

func TestLegacyUIDDetailDoesNotReuseOldGenerationCache(t *testing.T) {
	generation := uint32(10)
	calls := 0
	fb := &fakeBackend{getMessageFunc: func(accountID, _ string) (*mail.FullMessage, error) {
		calls++
		ref := mail.MessageRef{Provider: "imap", AccountID: accountID, Mailbox: "INBOX", UIDValidity: generation, UID: 7}
		return &mail.FullMessage{Message: mail.Message{
			UID: 7, UIDValidity: generation, Folder: "INBOX", AccountID: accountID, MessageRef: ref.Encode(),
		}, Body: "current"}, nil
	}}
	service := NewMailReadService(fb)
	if _, _, _, cached, err := service.GetMessageDetail(context.Background(), "legacy_detail_account", "7"); err != nil || cached {
		t.Fatalf("initial legacy UID read failed: cached=%v err=%v", cached, err)
	}
	generation = 11
	message, _, _, cached, err := service.GetMessageDetail(context.Background(), "legacy_detail_account", "7")
	if err != nil || cached || message == nil || message.UIDValidity != 11 || calls != 2 {
		generationGot := uint32(0)
		if message != nil {
			generationGot = message.UIDValidity
		}
		t.Fatalf("legacy UID reused stale cache: generation=%d cached=%v calls=%d err=%v", generationGot, cached, calls, err)
	}
}
