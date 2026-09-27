package server

import (
	"context"
	"fmt"
	"testing"

	"icloud-hme/internal/mail"
)

func TestBatchMailPreservesTypedWebMailIDsAndRequestOrder(t *testing.T) {
	const accountID = "acc_mail_order"
	imapRef := mail.MessageRef{Provider: "imap", AccountID: accountID, Mailbox: "INBOX", UIDValidity: 10, UID: 7}
	webRefs := []mail.MessageRef{
		{Provider: "webmail", AccountID: accountID, ThreadID: "42"},
		{Provider: "webmail", AccountID: accountID, ThreadID: "folder:23"},
	}
	var requestedWeb []string
	fb := &fakeBackend{
		getMessageFunc: func(_ string, raw string) (*mail.FullMessage, error) {
			ref, err := mail.ParseMessageRef(raw, accountID)
			if err != nil || ref.Provider != "webmail" {
				return nil, fmt.Errorf("WebMail identity was changed: raw=%q ref=%+v err=%v", raw, ref, err)
			}
			requestedWeb = append(requestedWeb, ref.ThreadID)
			return &mail.FullMessage{Message: mail.Message{ID: ref.ThreadID, ThreadID: ref.ThreadID, MessageRef: raw, Provider: "webmail"}}, nil
		},
		onGetMessagesContext: func(_ context.Context, _ string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
			if len(refs) != 1 || refs[0].CacheKey() != imapRef.CacheKey() {
				return nil, fmt.Errorf("wrong IMAP batch: %+v", refs)
			}
			return []*mail.FullMessage{{Message: mail.Message{
				ID: "7", AccountID: accountID, Folder: "INBOX", UIDValidity: 10,
				UID: 7, Provider: "imap", MessageRef: imapRef.Encode(),
			}}}, nil
		},
	}
	svc := NewMailReadService(fb)
	requested := []batchMessageItemReq{{MessageRef: imapRef.Encode()}, {MessageRef: webRefs[0].Encode()}, {MessageRef: webRefs[1].Encode()}}
	messages, items, err := svc.GetMessagesBatch(context.Background(), accountID, requested)
	if err != nil {
		t.Fatal(err)
	}
	if len(requestedWeb) != 2 || requestedWeb[0] != "42" || requestedWeb[1] != "folder:23" {
		t.Fatalf("WebMail thread IDs changed in transit: %v", requestedWeb)
	}
	if len(items) != 3 || len(messages) != 3 {
		t.Fatalf("unexpected batch length: items=%d messages=%d", len(items), len(messages))
	}
	for i, want := range []string{"7", "42", "folder:23"} {
		if items[i].RequestedRef != requested[i].MessageRef || items[i].Message == nil || items[i].Message.ID != want || messages[i].ID != want {
			t.Fatalf("item %d lost request order or identity: item=%+v message=%+v", i, items[i], messages[i])
		}
	}
}
