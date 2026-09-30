package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

func TestMailboxOnlyAccountUnknownAliasProbe(t *testing.T) {
	const alias = "original@icloud.com"
	queries := 0
	backend := &fakeBackend{
		accounts: []account.Summary{{ID: "account", Mailbox: &account.MailboxSummary{Email: "final@example.com", IMAPHost: "imap.example.com", IMAPPort: 993}}},
		onListInboxContext: func(_ context.Context, query InboxQuery) (InboxResult, error) {
			queries++
			if query.Alias != alias || query.WithBody != (queries == 2) {
				return InboxResult{}, fmt.Errorf("unexpected ownership-probe/body-fetch sequence: query=%+v calls=%d", query, queries)
			}
			return InboxResult{Messages: []mail.Message{{To: alias, Provider: "imap", Folder: "INBOX", UIDValidity: 1, UID: 105, Subject: "Your verification code is 654321"}}}, nil
		},
	}
	bus := mail.NewEventBus(time.Minute)
	worker := NewMailSyncWorker(backend, nil, bus, time.Second)
	defer worker.Stop()
	subscriptionID, events := bus.Subscribe(alias)
	defer bus.Unsubscribe(alias, subscriptionID)
	worker.syncOnce()
	if queries != 2 || worker.GetAliasAccount(alias) != "account" {
		t.Fatalf("mailbox-only account not routed: queries=%d route=%q", queries, worker.GetAliasAccount(alias))
	}
	select {
	case event := <-events:
		if event.OTP.Code != "654321" {
			t.Fatalf("unexpected code: %+v", event)
		}
	default:
		t.Fatal("mailbox-only account delivered no verification event")
	}
}

func TestMailDirectWebmailQueryDoesNotRequireIMAPDays(t *testing.T) {
	ref := mail.MessageRef{Provider: "webmail", AccountID: "account", ThreadID: "thread"}
	backend := &fakeBackend{onListInboxContext: func(_ context.Context, query InboxQuery) (InboxResult, error) {
		if query.DaysSpecified || query.FolderSpecified {
			return InboxResult{}, &BackendError{Status: http.StatusBadRequest, Code: "CAPABILITY_UNSUPPORTED", Message: "WebMail does not support these filters"}
		}
		if query.Days != 30 || query.Folder != "INBOX" || !query.WithBody {
			return InboxResult{}, fmt.Errorf("IMAP query behavior changed: %+v", query)
		}
		return InboxResult{AccountID: "account", Method: "web_api", Messages: []mail.Message{{ID: "thread", MessageRef: ref.Encode(), To: query.Alias, Preview: "Your verification code is 654321"}}}, nil
	}}
	server := &Server{be: backend, mailReadService: NewMailReadService(backend)}
	for _, limit := range []int{1, 20} {
		items, err := server.fetchRecentMessagesForAlias(context.Background(), "account", "original@icloud.com", limit)
		if err != nil || len(items) != 1 || items[0].Code != "654321" {
			t.Fatalf("limit=%d: WebMail direct view failed: items=%+v err=%v", limit, items, err)
		}
	}
}

func TestExternalMailboxBoundaryPreservesUpstreamFailure(t *testing.T) {
	for _, credentials := range []string{"mailbox only", "app password", "cookies"} {
		t.Run(credentials, func(t *testing.T) {
			directory := t.TempDir()
			storage, err := store.NewStore(directory)
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			record := &store.AccountRecord{ID: "account", ICloudEmail: "owner@icloud.com", Status: "active", MailboxJSON: `{"provider":"custom","email":"inbox@example.com","imap_host":"127.0.0.1","imap_port":1,"password":"test-only"}`}
			if credentials == "app password" {
				record.AppPassword = "test-only"
			} else if credentials == "cookies" {
				record.CookiesJSON = `{"X-APPLE-WEBAUTH-TOKEN":"test-only"}`
			}
			if err := storage.SaveAccount(record); err != nil {
				t.Fatal(err)
			}
			manager, err := account.NewManager(directory, storage)
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close()
			backend := &managerBackend{mgr: manager, store: storage}
			provider, _, _, err := backend.GetMailboxBoundaryContext(context.Background(), "account", "INBOX")
			var backendError *BackendError
			if !errors.As(err, &backendError) || provider != "imap" || backendError.Code != "UPSTREAM_FAILURE" || backendError.Status != http.StatusBadGateway || !strings.Contains(backendError.Message, "127.0.0.1") {
				t.Fatalf("external IMAP failure misclassified: provider=%s error=%v", provider, err)
			}
		})
	}
}

func TestBatchReadServiceReportsIndividualBodyFailures(t *testing.T) {
	badRef := mail.MessageRef{Provider: "imap", AccountID: "account", Mailbox: "INBOX", UIDValidity: 1, UID: 105}
	goodRef := badRef
	goodRef.UID = 106
	wrongGeneration := badRef
	wrongGeneration.UIDValidity = 2
	bodyError := errors.New("decode message body: invalid structure")
	backend := &fakeBackend{onGetMessagesContext: func(context.Context, string, []mail.MessageRef) ([]*mail.FullMessage, error) {
		return []*mail.FullMessage{{Message: mail.Message{MessageRef: goodRef.Encode(), Folder: "INBOX", Provider: "imap", UIDValidity: 1, UID: 106}, Body: "Your verification code is 654321", BodyComplete: true}}, &mail.BatchReadError{Failures: []mail.MessageReadFailure{{Ref: badRef, Err: bodyError, Permanent: true}}}
	}}
	service := NewMailReadService(backend)
	requests := []batchMessageItemReq{{MessageRef: badRef.Encode()}, {MessageRef: goodRef.Encode()}, {Folder: "INBOX", UID: "105"}, {MessageRef: wrongGeneration.Encode()}}
	messages, items, err := service.GetMessagesBatch(context.Background(), "account", requests)
	if err != nil || len(messages) != 1 || len(items) != len(requests) || service.CacheLen() != 1 {
		t.Fatalf("partial batch lost healthy messages: messages=%d items=%+v cache=%d err=%v", len(messages), items, service.CacheLen(), err)
	}
	if items[0].Error != bodyError.Error() || items[2].Error != bodyError.Error() || items[1].Message == nil || items[3].Error != "message not found" {
		t.Fatalf("incorrect per-item identity join: %+v", items)
	}
}

func TestPermanentBodyFailureDoesNotBlockOtherAliasOrLaterPages(t *testing.T) {
	for _, healthyUID := range []uint32{101, 155} {
		t.Run(fmt.Sprint(healthyUID), func(t *testing.T) {
			storage, blockedRequest := newVerificationRegressionStore(t)
			healthyRequest := *blockedRequest
			healthyRequest.RequestID = "healthy_request"
			healthyRequest.LeaseID = "healthy_lease"
			healthyRequest.AliasEmail = "healthy@icloud.com"
			ctx := context.Background()
			if err := storage.CreateVerificationRequest(ctx, &healthyRequest); err != nil {
				t.Fatal(err)
			}
			var metadata []mail.Message
			for uid := uint32(100); uid <= 155; uid++ {
				message := mail.Message{UID: uid, UIDValidity: 1, Folder: "INBOX", Provider: "imap", To: "unwatched@icloud.com"}
				if uid == 100 {
					message.To = blockedRequest.AliasEmail
				} else if uid == healthyUID {
					message.To = healthyRequest.AliasEmail
					message.Subject = "Your verification code is 654321"
				}
				metadata = append(metadata, message)
			}
			backend := newScanTestBackend("account", metadata, 156)
			originalFetch := backend.onGetMessagesContext
			badFetches := 0
			backend.onGetMessagesContext = func(ctx context.Context, accountID string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
				var validRefs []mail.MessageRef
				var failures []mail.MessageReadFailure
				for _, ref := range refs {
					if ref.UID == 100 {
						badFetches++
						failures = append(failures, mail.MessageReadFailure{Ref: ref, Err: errors.New("message body exceeds 524288 byte limit"), Permanent: true})
					} else {
						validRefs = append(validRefs, ref)
					}
				}
				messages, err := originalFetch(ctx, accountID, validRefs)
				if err == nil && len(failures) > 0 {
					err = &mail.BatchReadError{Failures: failures}
				}
				return messages, err
			}
			worker := NewMailSyncWorker(backend, storage, mail.NewEventBus(time.Minute), time.Second)
			defer worker.Stop()
			aliases := []string{blockedRequest.AliasEmail, healthyRequest.AliasEmail}
			matched, err := worker.fetchAndPublishBatchResult(ctx, "account", aliases)
			var batchError *mail.BatchReadError
			if !matched || !errors.As(err, &batchError) {
				t.Fatalf("lost success or explicit failure: matched=%v err=%v", matched, err)
			}
			got, err := storage.GetVerificationRequest(ctx, healthyRequest.RequestID, healthyRequest.PrincipalKind, healthyRequest.PrincipalID)
			if err != nil || got.Status != "succeeded" || got.Code != "654321" {
				t.Fatalf("healthy alias remained blocked: request=%+v err=%v", got, err)
			}
			blocked, err := storage.GetVerificationRequest(ctx, blockedRequest.RequestID, blockedRequest.PrincipalKind, blockedRequest.PrincipalID)
			if err != nil || blocked.Status != "ready" || blocked.Code != "" {
				t.Fatalf("bad body became a successful verification: request=%+v err=%v", blocked, err)
			}
			checkpoint := worker.checkpoints[checkpointKey{accountID: "account", mailbox: "INBOX", uidValidity: 1}]
			if checkpoint == nil || checkpoint.NextUID != 156 {
				t.Fatalf("permanent failure blocked later pages: %+v", checkpoint)
			}
			if _, err := worker.fetchAndPublishBatchResult(ctx, "account", aliases); err != nil || badFetches != 1 {
				t.Fatalf("permanent failure repeatedly fetched: fetches=%d err=%v", badFetches, err)
			}
		})
	}
}

func TestMissingBodyFailureRetainsVerificationCheckpoint(t *testing.T) {
	storage, request := newVerificationRegressionStore(t)
	metadata := []mail.Message{{UID: 105, UIDValidity: 1, Folder: "INBOX", Provider: "imap", To: request.AliasEmail, Subject: "Your verification code is 654321"}}
	backend := newScanTestBackend("account", metadata, 106)
	originalFetch := backend.onGetMessagesContext
	missing := true
	backend.onGetMessagesContext = func(ctx context.Context, accountID string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
		if missing {
			return nil, &mail.BatchReadError{Failures: []mail.MessageReadFailure{{Ref: refs[0], Err: mail.ErrMissingMessageBody}}}
		}
		return originalFetch(ctx, accountID, refs)
	}
	worker := NewMailSyncWorker(backend, storage, mail.NewEventBus(time.Minute), time.Second)
	defer worker.Stop()
	ctx := context.Background()
	if _, err := worker.fetchAndPublishBatchResult(ctx, "account", []string{request.AliasEmail}); err == nil {
		t.Fatal("missing body was silently skipped")
	}
	checkpoint := worker.checkpoints[checkpointKey{accountID: "account", mailbox: "INBOX", uidValidity: 1}]
	if checkpoint.NextUID > 105 {
		t.Fatalf("retryable failure advanced checkpoint: %d", checkpoint.NextUID)
	}
	missing = false
	if matched, err := worker.fetchAndPublishBatchResult(ctx, "account", []string{request.AliasEmail}); !matched || err != nil {
		t.Fatalf("body retry failed: matched=%v error=%v", matched, err)
	}
}
