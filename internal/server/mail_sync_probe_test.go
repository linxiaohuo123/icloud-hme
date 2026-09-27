/**
 * [INPUT]: 依赖 fakeBackend、MailSyncWorker、EventBus
 * [OUTPUT]: 验证未知别名轮转与并发上限、读取失败重试、后到达邮件与旧版多别名定向补查
 * [POS]: server 邮件同步边界回归测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

func TestMailSyncUnknownAliasProbesBeyondFirstBatch(t *testing.T) {
	const alias = "manual@icloud.com"
	accounts := make([]account.Summary, 25)
	for i := range accounts {
		accounts[i] = account.Summary{ID: fmt.Sprintf("acc_%02d", i), HasAppPassword: true}
	}
	var calls atomic.Int32
	backend := &fakeBackend{
		accounts: accounts,
		onListInboxContext: func(_ context.Context, q InboxQuery) (InboxResult, error) {
			calls.Add(1)
			if q.AccountID != "acc_24" {
				return InboxResult{}, nil
			}
			return InboxResult{Messages: []mail.Message{{
				ID: "24:1", Folder: "INBOX", Provider: "imap", UIDValidity: 1, UID: 1,
				To: alias, Subject: "Your code is 123456",
			}}}, nil
		},
	}
	bus := mail.NewEventBus(time.Minute)
	worker := NewMailSyncWorker(backend, nil, bus, time.Second)
	subID, ch := bus.Subscribe(alias)
	defer bus.Unsubscribe(alias, subID)

	worker.syncOnce()
	if calls.Load() != maxUnknownAliasProbeAccounts || worker.GetAliasAccount(alias) != "" {
		t.Fatalf("first batch: calls=%d route=%q", calls.Load(), worker.GetAliasAccount(alias))
	}
	worker.syncOnce()
	if calls.Load() != 26 || worker.GetAliasAccount(alias) != "acc_24" {
		t.Fatalf("second batch: calls=%d route=%q", calls.Load(), worker.GetAliasAccount(alias))
	}
	select {
	case event := <-ch:
		if event.OTP == nil || event.OTP.Code != "123456" {
			t.Fatalf("unexpected event: %+v", event)
		}
	default:
		t.Fatal("alias did not receive the code")
	}
}

func TestMailSyncUnknownAliasRetriesReadFailureAndLaterArrival(t *testing.T) {
	const alias = "manual@icloud.com"
	calls := 0
	available := false
	backend := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", HasAppPassword: true}},
		onListInboxContext: func(_ context.Context, q InboxQuery) (InboxResult, error) {
			calls++
			if calls == 1 {
				return InboxResult{}, errors.New("temporary read failure")
			}
			if !available {
				return InboxResult{}, nil
			}
			return InboxResult{Messages: []mail.Message{{
				ID: "1:2", Folder: "INBOX", Provider: "imap", UIDValidity: 1, UID: 2,
				To: alias, Subject: "Your code is 654321",
			}}}, nil
		},
	}
	bus := mail.NewEventBus(time.Minute)
	worker := NewMailSyncWorker(backend, nil, bus, time.Second)
	subID, ch := bus.Subscribe(alias)
	defer bus.Unsubscribe(alias, subID)

	worker.syncOnce()
	worker.syncOnce()
	available = true
	worker.syncOnce()
	if calls != 4 || worker.GetAliasAccount(alias) != "acc_1" {
		t.Fatalf("read was not retried after failure or empty inbox: calls=%d route=%q", calls, worker.GetAliasAccount(alias))
	}
	select {
	case event := <-ch:
		if event.OTP == nil || event.OTP.Code != "654321" {
			t.Fatalf("unexpected event: %+v", event)
		}
	default:
		t.Fatal("later code was not delivered")
	}
}

func TestMailSyncUnknownAliasProbeBudgetIsGlobalAndFair(t *testing.T) {
	accounts := make([]account.Summary, 25)
	for i := range accounts {
		accounts[i] = account.Summary{ID: fmt.Sprintf("acc_%02d", i), HasAppPassword: true}
	}
	var callsA, callsB atomic.Int32
	backend := &fakeBackend{
		accounts: accounts,
		onListInboxContext: func(_ context.Context, q InboxQuery) (InboxResult, error) {
			if q.Alias == "a@icloud.com" {
				callsA.Add(1)
			} else {
				callsB.Add(1)
			}
			return InboxResult{}, nil
		},
	}
	bus := mail.NewEventBus(time.Minute)
	worker := NewMailSyncWorker(backend, nil, bus, time.Second)
	for _, alias := range []string{"a@icloud.com", "b@icloud.com"} {
		subID, _ := bus.Subscribe(alias)
		defer bus.Unsubscribe(alias, subID)
	}
	worker.syncOnce()
	firstA, firstB := callsA.Load(), callsB.Load()
	if firstA+firstB != maxUnknownAliasProbeAccounts {
		t.Fatalf("first round exceeded global probe budget: a=%d b=%d", firstA, firstB)
	}
	worker.syncOnce()
	secondA, secondB := callsA.Load(), callsB.Load()
	if secondA+secondB != 2*maxUnknownAliasProbeAccounts {
		t.Fatalf("second round exceeded global probe budget: a=%d b=%d", secondA, secondB)
	}
	if firstA != maxUnknownAliasProbeAccounts || firstB != 0 || secondA != firstA || secondB != maxUnknownAliasProbeAccounts {
		t.Fatalf("second round did not rotate aliases: a=%d->%d b=%d->%d", firstA, secondA, firstB, secondB)
	}
}

func TestMailSyncUnknownAliasProbeConcurrencyIsBounded(t *testing.T) {
	accounts := make([]account.Summary, maxConcurrentAccountSync+1)
	for i := range accounts {
		accounts[i] = account.Summary{ID: fmt.Sprintf("acc_%02d", i), HasAppPassword: true}
	}
	started := make(chan struct{}, len(accounts))
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()
	backend := &fakeBackend{
		accounts: accounts,
		onListInboxContext: func(_ context.Context, _ InboxQuery) (InboxResult, error) {
			started <- struct{}{}
			<-release
			return InboxResult{}, nil
		},
	}
	bus := mail.NewEventBus(time.Minute)
	worker := NewMailSyncWorker(backend, nil, bus, time.Second)
	subID, _ := bus.Subscribe("unknown@icloud.com")
	defer bus.Unsubscribe("unknown@icloud.com", subID)
	done := make(chan struct{})
	go func() {
		worker.syncOnce()
		close(done)
	}()
	for i := 0; i < maxConcurrentAccountSync; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("probe did not start the expected concurrent requests")
		}
	}
	select {
	case <-started:
		t.Fatal("probe exceeded the account concurrency limit")
	default:
	}
	releaseAll()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not finish after requests were released")
	}
	if len(started) != 1 {
		t.Fatalf("remaining account was not probed: started=%d", len(started))
	}
}

func TestMailSyncUnknownStrictAliasChecksGenerationOnlyAfterOwnership(t *testing.T) {
	const alias = "strict@icloud.com"
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	if err := st.CreateVerificationRequest(context.Background(), &store.VerificationRequest{
		RequestID: "strict_probe", PrincipalKind: "token", PrincipalID: "token_1",
		LeaseID: "lease_1", AliasEmail: alias, Status: "ready",
		CreatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339),
		BaselineProvider: "imap", BaselineMailbox: "INBOX", BaselineUIDValidity: 1, BaselineUID: 100,
	}); err != nil {
		t.Fatal(err)
	}
	var boundaryAccounts []string
	backend := &fakeBackend{
		accounts: []account.Summary{
			{ID: "wrong", HasAppPassword: true},
			{ID: "owner", HasAppPassword: true},
		},
		onListInboxContext: func(_ context.Context, q InboxQuery) (InboxResult, error) {
			if q.AccountID != "owner" {
				return InboxResult{}, nil
			}
			return InboxResult{Messages: []mail.Message{{
				ID: "1:100", Folder: "INBOX", To: alias, UIDValidity: 1, UID: 100,
			}}}, nil
		},
		onGetMailboxBoundaryContext: func(_ context.Context, accountID, _ string) (string, uint32, uint32, error) {
			boundaryAccounts = append(boundaryAccounts, accountID)
			if accountID != "owner" {
				return "imap", 2, 101, nil
			}
			return "imap", 1, 101, nil
		},
		onScanMailboxUIDPage: func(_ context.Context, _ ScanPageQuery) (ScanPageResult, error) {
			return ScanPageResult{UIDValidity: 1, NextUID: 101}, nil
		},
	}
	worker := NewMailSyncWorker(backend, st, mail.NewEventBus(time.Minute), time.Second)
	worker.syncOnce()
	if worker.GetAliasAccount(alias) != "owner" || len(boundaryAccounts) != 1 || boundaryAccounts[0] != "owner" {
		t.Fatalf("strict scan ran before ownership was established: route=%q boundaries=%v", worker.GetAliasAccount(alias), boundaryAccounts)
	}
	req, err := st.GetVerificationRequest(context.Background(), "strict_probe", "token", "token_1")
	if err != nil || req == nil || req.Status != "ready" {
		t.Fatalf("verification request was incorrectly invalidated: req=%+v err=%v", req, err)
	}
}

func TestMailSyncLegacyBatchQueriesAliasWhenSharedWindowIsFull(t *testing.T) {
	const firstAlias = "first@icloud.com"
	const targetAlias = "target@icloud.com"
	queries := make([]InboxQuery, 0, 2)
	shared := make([]mail.Message, 10)
	for i := range shared {
		shared[i] = mail.Message{ID: fmt.Sprintf("1:%d", i+10), Folder: "INBOX", To: "other@icloud.com"}
	}
	shared[0].To = firstAlias
	shared[0].Subject = "Your code is 111222"
	backend := &fakeBackend{
		onListInboxContext: func(_ context.Context, q InboxQuery) (InboxResult, error) {
			queries = append(queries, q)
			if q.Alias == targetAlias {
				return InboxResult{Messages: []mail.Message{{
					ID: "1:9", Folder: "INBOX", To: targetAlias, Subject: "Your code is 333444",
				}}}, nil
			}
			return InboxResult{Messages: shared}, nil
		},
	}
	bus := mail.NewEventBus(time.Minute)
	worker := NewMailSyncWorker(backend, nil, bus, time.Second)
	subID, ch := bus.Subscribe(targetAlias)
	defer bus.Unsubscribe(targetAlias, subID)

	if !worker.fetchAndPublishBatch(context.Background(), "acc_1", []string{firstAlias, targetAlias}) {
		t.Fatal("expected legacy messages to match")
	}
	if len(queries) != 2 || queries[0].Alias != "" || queries[1].Alias != targetAlias {
		t.Fatalf("unexpected queries: %+v", queries)
	}
	select {
	case event := <-ch:
		if event.OTP == nil || event.OTP.Code != "333444" {
			t.Fatalf("unexpected event: %+v", event)
		}
	default:
		t.Fatal("target alias was missed outside the shared window")
	}
}
