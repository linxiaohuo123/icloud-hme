package server

import (
	"context"
	"errors"
	"testing"

	"icloud-hme/internal/mail"
)

func TestWebMailSearchResultDetailDoesNotRequireRecentInbox(t *testing.T) {
	const accountID = "acc_webmail"
	backend := &fakeBackend{
		onListInboxContext: func(ctx context.Context, q InboxQuery) (InboxResult, error) {
			return InboxResult{
				AccountID: accountID,
				Method:    "web_api",
				Messages: []mail.Message{{
					ID: "older-thread", To: "target@icloud.com", Preview: "older preview", Provider: "webmail",
				}},
			}, nil
		},
		getMessageFunc: func(accountID, id string) (*mail.FullMessage, error) {
			return nil, errors.New("thread not in latest 100")
		},
	}
	service := NewMailReadService(backend)
	if _, err := service.ListInbox(context.Background(), InboxQuery{AccountID: accountID, Alias: "target@icloud.com", Limit: 20}); err != nil {
		t.Fatal(err)
	}
	ref := mail.MessageRef{Provider: "webmail", AccountID: accountID, ThreadID: "older-thread"}.Encode()
	msg, provider, _, cached, err := service.GetMessageDetail(context.Background(), accountID, ref)
	if err != nil || !cached || provider != "webmail" || msg == nil || msg.Preview != "older preview" || msg.BodyComplete {
		t.Fatalf("search result should remain readable as incomplete preview: msg=%+v provider=%q cached=%v err=%v", msg, provider, cached, err)
	}

	service.InvalidateAccount(accountID)
	if _, _, _, _, err := service.GetMessageDetail(context.Background(), accountID, ref); err == nil {
		t.Fatal("account invalidation must remove cached WebMail preview")
	}
}
