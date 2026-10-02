// [INPUT]: Production Worker, verification service and real HTTP over controlled mailbox/SQLite fixtures.
// [OUTPUT]: Review regression for new tasks, mailbox switches and cancellation responses.
// [POS]: internal/server verification lifecycle correctness.
// [PROTOCOL]: Update this header and CLAUDE.md when changing this file.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/auth"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

func TestRecreatedAliasNeedsCheckpointCoverage(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	add := func(id, alias string, uid uint32) {
		now := time.Now().UTC()
		err := st.CreateVerificationRequest(ctx, &store.VerificationRequest{RequestID: id, PrincipalKind: "token", PrincipalID: "owner", LeaseID: id, AliasEmail: alias, Status: "ready", CreatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(10 * time.Minute).Format(time.RFC3339), BaselineProvider: "imap", BaselineMailbox: "INBOX", BaselineUIDValidity: 1, BaselineUID: uid})
		if err != nil {
			t.Fatal(err)
		}
	}
	aliasA, aliasB := "a@icloud.com", "b@icloud.com"
	add("old-a", aliasA, 10)
	add("still-b", aliasB, 10)
	msg := func(uid uint32, alias, code string) mail.Message {
		return mail.Message{UID: uid, UIDValidity: 1, Folder: "INBOX", Provider: "imap", To: alias, Subject: "Your verification code is " + code, Preview: "Code: " + code}
	}
	fb := newScanTestBackend("acc", []mail.Message{msg(10, aliasA, "111111"), msg(20, "noise@icloud.com", "555555")}, 21)
	w := NewMailSyncWorker(fb, st, mail.NewEventBus(time.Minute), time.Second)
	w.RegisterAliasAccount(aliasA, "acc")
	w.RegisterAliasAccount(aliasB, "acc")
	w.syncOnce()
	first, err := st.GetVerificationRequest(ctx, "old-a", "token", "owner")
	if err != nil || first.Status != "succeeded" {
		t.Fatalf("first task: %+v %v", first, err)
	}
	// A's next creation captures UIDNEXT=21. The worker snapshots only B.
	// A's INSERT then completes before the next mailbox boundary read, and
	// its new code arrives as UID 21. This round still scans only B's alias set.
	later := newScanTestBackend("acc", []mail.Message{msg(21, aliasA, "222222"), msg(30, "noise@icloud.com", "555555")}, 31)
	inserted := false
	svc := NewVerificationService(fb, st, w.eventBus, w)
	newID := ""
	fb.mailboxBoundaryFunc = func(acc, folder string) (string, uint32, uint32, error) {
		if !inserted {
			inserted = true
			fb.mailboxBoundaryFunc = func(string, string) (string, uint32, uint32, error) { return "imap", 1, 21, nil }
			req, err := svc.CreateVerificationRequest(ctx, auth.Principal{Kind: auth.PrincipalAdmin, ID: "admin", Scopes: []string{"admin"}}, aliasA)
			if err != nil {
				t.Fatal(err)
			}
			newID = req.RequestID
			fb.mailboxBoundaryFunc = later.mailboxBoundaryFunc
		}
		return later.mailboxBoundaryFunc(acc, folder)
	}
	fb.onScanMailboxUIDPage = later.onScanMailboxUIDPage
	fb.onGetMessagesContext = later.onGetMessagesContext
	w.syncOnce()
	w.syncOnce()
	got, err := st.GetVerificationRequest(ctx, newID, "admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" || got.Code != "222222" {
		t.Fatalf("new generation missed UID 21: status=%s code=%q checkpoint=%+v", got.Status, got.Code, w.checkpoints[checkpointKey{accountID: "acc", mailbox: "INBOX", uidValidity: 1}])
	}
}

func TestCompatibilityShutdownIsExplicit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &Server{ctx: ctx, eventBus: mail.NewEventBus(time.Minute)}
	r := gin.New()
	r.GET("/mail/code", func(c *gin.Context) {
		c.Set("principal", auth.Principal{Kind: auth.PrincipalAdmin, ID: "admin", Scopes: []string{"admin"}})
		s.verifyCodeHandler(c)
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/mail/code?email=a@icloud.com&timeout=120", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("shutdown falsely looks successful: HTTP %d body=%q", rec.Code, rec.Body.String())
	}
}

func TestMailboxChangeMustNotReuseUIDBoundary(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	alias := "target@icloud.com"
	err = st.CreateVerificationRequest(ctx, &store.VerificationRequest{RequestID: "old-inbox-task", PrincipalKind: "token", PrincipalID: "owner", LeaseID: "lease", AliasEmail: alias, Status: "ready", CreatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339), BaselineProvider: "imap", BaselineMailbox: "INBOX", BaselineUIDValidity: 1, BaselineUID: 100})
	if err != nil {
		t.Fatal(err)
	}
	fb := newScanTestBackend("acc", nil, 100)
	endpoint := "inbox-one"
	fb.onCaptureMailboxContext = func(ctx context.Context, _ string) (context.Context, string, string, error) {
		return ctx, endpoint, endpoint, nil
	}
	if _, err := st.DB().Exec(`UPDATE verification_requests SET baseline_source=?,baseline_account_id='acc'`, endpoint); err != nil {
		t.Fatal(err)
	}
	fb.onGetMailboxEndpointFingerprint = func(string) (string, bool) { return endpoint, true }
	w := NewMailSyncWorker(fb, st, mail.NewEventBus(time.Minute), time.Second)
	w.RegisterAliasAccount(alias, "acc")
	w.syncOnce()
	before, err := st.GetVerificationRequest(ctx, "old-inbox-task", "token", "owner")
	if err != nil || before.Status != "ready" {
		t.Fatalf("first scan: %+v %v", before, err)
	}
	// Another physical inbox may legitimately have the same UIDVALIDITY.
	// Its old UID 150 has no relationship to inbox one's captured UIDNEXT 100.
	endpoint = "inbox-two"
	other := newScanTestBackend("acc", []mail.Message{{UID: 150, UIDValidity: 1, Folder: "INBOX", Provider: "imap", To: alias, Subject: "Your verification code is 777777", Preview: "Code: 777777", Date: now.Add(-24 * time.Hour).Format(time.RFC3339)}}, 201)
	fb.mailboxBoundaryFunc = other.mailboxBoundaryFunc
	fb.onScanMailboxUIDPage = other.onScanMailboxUIDPage
	fb.onGetMessagesContext = other.onGetMessagesContext
	w.syncOnce()
	got, err := st.GetVerificationRequest(ctx, "old-inbox-task", "token", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "invalidated" || got.Code != "" {
		t.Fatalf("old task accepted another inbox's historical code: status=%s code=%q", got.Status, got.Code)
	}
}

// A configuration change and new INSERT happen while an older batch captures its config.
func TestOldMailboxBatchCannotInvalidateNewTask(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	alias := "target@icloud.com"
	add := func(id, source string) {
		err := st.CreateVerificationRequest(ctx, &store.VerificationRequest{RequestID: id, PrincipalKind: "admin", PrincipalID: "admin", LeaseID: id, AliasEmail: alias, Status: "ready", CreatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339), BaselineProvider: "imap", BaselineMailbox: "INBOX", BaselineUIDValidity: 1, BaselineUID: 100, BaselineSource: source, BaselineAccountID: "acc"})
		if err != nil {
			t.Fatal(err)
		}
	}
	add("old", "old-inbox")
	fb := newScanTestBackend("acc", []mail.Message{{UID: 150, UIDValidity: 1, Folder: "INBOX", Provider: "imap", To: alias, Subject: "Your verification code is 111111"}}, 201)
	type snapshotKey struct{}
	switched := false
	fb.onCaptureMailboxContext = func(ctx context.Context, _ string) (context.Context, string, string, error) {
		source := "new-inbox"
		if !switched {
			source = "old-inbox"
			switched = true
			add("new", "new-inbox")
		}
		return context.WithValue(ctx, snapshotKey{}, source), source, source, nil
	}
	boundary := fb.mailboxBoundaryFunc
	fb.onGetMailboxBoundaryContext = func(ctx context.Context, acc, folder string) (string, uint32, uint32, error) {
		if ctx.Value(snapshotKey{}) != "old-inbox" {
			t.Errorf("boundary lost frozen config")
		}
		return boundary(acc, folder)
	}
	get := fb.onGetMessagesContext
	fb.onGetMessagesContext = func(ctx context.Context, acc string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
		if ctx.Value(snapshotKey{}) != "old-inbox" {
			t.Errorf("body lost frozen config")
		}
		return get(ctx, acc, refs)
	}
	w := NewMailSyncWorker(fb, st, mail.NewEventBus(time.Minute), time.Second)
	w.RegisterAliasAccount(alias, "acc")
	w.syncOnce()
	got, err := st.GetVerificationRequest(ctx, "new", "admin", "admin")
	if err != nil || got.Status != "ready" || got.Code != "" {
		t.Fatalf("old batch touched new task: %+v %v", got, err)
	}
}

func TestCompatibilityShutdownOverHTTP(t *testing.T) {
	for _, route := range []string{"/mail/code", "/mail/code/:email"} {
		t.Run(route, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := &Server{ctx: ctx, eventBus: mail.NewEventBus(time.Minute)}
			r := gin.New()
			r.Use(s.requestTrackingMiddleware())
			r.GET(route, func(c *gin.Context) {
				c.Set("principal", auth.Principal{Kind: auth.PrincipalAdmin, ID: "admin", Scopes: []string{"admin"}})
				s.verifyCodeHandler(c)
			})
			ts := httptest.NewServer(r)
			defer ts.Close()
			done := make(chan error, 1)
			go func() {
				resp, err := ts.Client().Get(ts.URL + strings.ReplaceAll(route, ":email", "a@icloud.com") + "?email=a@icloud.com&timeout=120")
				if err != nil {
					done <- err
					return
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				if err == nil && (resp.StatusCode != 503 || !strings.Contains(string(body), "SERVER_SHUTTING_DOWN")) {
					err = fmt.Errorf("HTTP %d %s", resp.StatusCode, body)
				}
				done <- err
			}()
			deadline := time.Now().Add(3 * time.Second)
			for !s.eventBus.HasSubscribers() && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if !s.eventBus.HasSubscribers() {
				cancel()
				t.Fatal("request did not enter wait")
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("shutdown did not release waiter")
			}
		})
	}
}

func TestCompatibilityClientCancellation(t *testing.T) {
	ctx, cancelService := context.WithCancel(context.Background())
	defer cancelService()
	s := &Server{ctx: ctx, eventBus: mail.NewEventBus(time.Minute)}
	r := gin.New()
	r.Use(s.requestTrackingMiddleware())
	r.GET("/mail/code", func(c *gin.Context) {
		c.Set("principal", auth.Principal{Kind: auth.PrincipalAdmin, ID: "admin", Scopes: []string{"admin"}})
		s.verifyCodeHandler(c)
	})
	ts := httptest.NewServer(r)
	defer ts.Close()
	reqCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	req, err := http.NewRequestWithContext(reqCtx, "GET", ts.URL+"/mail/code?email=a@icloud.com&timeout=120", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		resp, err := ts.Client().Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for !s.eventBus.HasSubscribers() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !s.eventBus.HasSubscribers() {
		t.Fatal("request did not enter wait")
	}
	cancelRequest()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("client cancel: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client did not cancel")
	}
	for s.eventBus.HasSubscribers() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.eventBus.HasSubscribers() {
		t.Fatal("cancelled request leaked subscriber")
	}
	if s.ctx.Err() != nil {
		t.Fatal("client cancellation cancelled server")
	}
}

func TestMailboxSourceScopesPublishDedup(t *testing.T) {
	w := NewMailSyncWorker(&fakeBackend{}, nil, mail.NewEventBus(time.Minute), time.Second)
	if !w.markPublished("acc", "INBOX", 1, 150, "", "imap", "target@icloud.com", "old") {
		t.Fatal("first source not published")
	}
	if w.markPublished("acc", "INBOX", 1, 150, "", "imap", "target@icloud.com", "old") {
		t.Fatal("same source published twice")
	}
	if !w.markPublished("acc", "INBOX", 1, 150, "", "imap", "target@icloud.com", "new") {
		t.Fatal("new physical source suppressed")
	}
}

func TestGetInvalidatesChangedMailboxWithoutScan(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	req := &store.VerificationRequest{RequestID: "source-get", PrincipalKind: "admin", PrincipalID: "admin", LeaseID: "source-get", AliasEmail: "target@icloud.com", Status: "ready", CreatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339), BaselineProvider: "imap", BaselineMailbox: "INBOX", BaselineUIDValidity: 1, BaselineUID: 100, BaselineSource: "old", BaselineAccountID: "acc"}
	if err := st.CreateVerificationRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	fb := &fakeBackend{onCaptureMailboxContext: func(ctx context.Context, id string) (context.Context, string, string, error) {
		if id != "acc" {
			t.Errorf("wrong account %s", id)
		}
		return ctx, "new", "new", nil
	}}
	svc := NewVerificationService(fb, st, mail.NewEventBus(time.Minute), nil)
	_, err = svc.GetVerificationResult(context.Background(), auth.Principal{Kind: auth.PrincipalAdmin, ID: "admin", Scopes: []string{"admin"}}, req.RequestID, 0, "")
	if !errors.Is(err, ErrUIDValidityChanged) {
		t.Fatalf("changed source GET: %v", err)
	}
	got, err := st.GetVerificationRequest(context.Background(), req.RequestID, "admin", "admin")
	if err != nil || got.Status != "invalidated" {
		t.Fatalf("not durable: %+v %v", got, err)
	}
}

func TestMailboxChangeDuringBaselineCapture(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	alias := "target@icloud.com"
	current := "old"
	type snapshotKey struct{}
	fb := &fakeBackend{}
	fb.onCaptureMailboxContext = func(ctx context.Context, id string) (context.Context, string, string, error) {
		return context.WithValue(ctx, snapshotKey{}, current), current, current, nil
	}
	fb.onGetMailboxBoundaryContext = func(ctx context.Context, id, folder string) (string, uint32, uint32, error) {
		source := ctx.Value(snapshotKey{})
		current = "new"
		if source == "old" {
			return "imap", 1, 100, nil
		}
		return "imap", 1, 200, nil
	}
	bus := mail.NewEventBus(time.Minute)
	w := NewMailSyncWorker(fb, st, bus, time.Second)
	w.RegisterAliasAccount(alias, "acc")
	svc := NewVerificationService(fb, st, bus, w)
	p := auth.Principal{Kind: auth.PrincipalAdmin, ID: "admin", Scopes: []string{"admin"}}
	req, err := svc.CreateVerificationRequest(ctx, p, alias)
	if err != nil || req.BaselineSource != "old" || req.BaselineUID != 100 {
		t.Fatalf("baseline mixed sources: %+v %v", req, err)
	}
	_, err = svc.GetVerificationResult(ctx, p, req.RequestID, 0, "")
	if !errors.Is(err, ErrUIDValidityChanged) {
		t.Fatalf("late old baseline remained usable: %v", err)
	}
	newReq, err := svc.CreateVerificationRequest(ctx, p, alias)
	if err != nil || newReq.BaselineSource != "new" || newReq.BaselineUID != 200 {
		t.Fatalf("new baseline: %+v %v", newReq, err)
	}
	bus.PublishEvent(&mail.CachedOTP{Source: "old", Email: alias, Folder: "INBOX", UIDValidity: 1, UID: 250, OTP: &mail.OTPResult{Code: "111111"}})
	got, err := svc.GetVerificationResult(ctx, p, newReq.RequestID, 0, "")
	if err != nil || got.Status != "pending" || got.Code != "" {
		t.Fatalf("old source accepted: %+v %v", got, err)
	}
	bus.PublishEvent(&mail.CachedOTP{Source: "new", Email: alias, Folder: "INBOX", UIDValidity: 1, UID: 201, OTP: &mail.OTPResult{Code: "222222"}})
	got, err = svc.GetVerificationResult(ctx, p, newReq.RequestID, 0, "")
	if err != nil || got.Status != "succeeded" || got.Code != "222222" {
		t.Fatalf("new source not accepted: %+v %v", got, err)
	}
}
