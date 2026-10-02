package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/auth"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

func TestAggregatedMessageRefOwnership(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const alias = "owned-by-b@icloud.com"
	now := time.Now().UTC()
	req := &store.VerificationRequest{
		RequestID: "probe-vreq-b", PrincipalKind: "token", PrincipalID: "probe-token",
		LeaseID: "probe-lease-b", AliasEmail: alias, Status: "ready",
		CreatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339),
		BaselineProvider: "imap", BaselineMailbox: "INBOX", BaselineUIDValidity: 1, BaselineUID: 100,
	}
	if err := st.CreateVerificationRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	fb := &fakeBackend{}
	fb.onGetMailboxBoundaryContext = func(context.Context, string, string) (string, uint32, uint32, error) {
		return "imap", 1, 102, nil
	}
	fb.onScanMailboxUIDPage = func(context.Context, ScanPageQuery) (ScanPageResult, error) {
		return ScanPageResult{UIDValidity: 1, NextUID: 102, Messages: []mail.Message{{
			UID: 101, UIDValidity: 1, Folder: "INBOX", Provider: "imap", To: alias,
		}}}, nil
	}
	fb.onGetMessagesContext = func(_ context.Context, _ string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
		ref := refs[0]
		return []*mail.FullMessage{{Message: mail.Message{
			UID: ref.UID, UIDValidity: ref.UIDValidity, Folder: ref.Mailbox, Provider: ref.Provider,
			AccountID: ref.AccountID, MessageRef: ref.Encode(), To: alias,
			Subject: "Your verification code is 123456", Body: "Verification code: 123456",
		}}}, nil
	}
	w := NewMailSyncWorker(fb, st, mail.NewEventBus(time.Minute), time.Second)
	batch := &aggregatedInboxBatch{fingerprint: "shared", repAccountID: "acc-a",
		allAliases: []string{alias}, aliasRealAccount: map[string]string{alias: "acc-b"}}
	if _, err := w.fetchAndPublishAggregatedBatchResult(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	rec, err := st.GetVerificationRequest(context.Background(), req.RequestID, req.PrincipalKind, req.PrincipalID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != "succeeded" {
		t.Fatalf("unexpected status: %s", rec.Status)
	}
	if _, err := mail.ParseMessageRef(rec.MatchedEventRef, "acc-b"); err != nil {
		t.Fatalf("completed task for acc-b contains representative account reference: %v", err)
	}
}

func reviewProbePost(client *http.Client, baseURL, token, lease, key string) (int, map[string]any, error) {
	body, _ := json.Marshal(map[string]string{"lease_id": lease})
	req, err := http.NewRequest("POST", baseURL+"/api/external/v2/verification-requests", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	var parsed map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, parsed, nil
}

func TestExpiredIdempotencyReplay(t *testing.T) {
	s, st, fb, ts, token, lease := setupV2TestEnv(t)
	defer s.Close()
	defer ts.Close()
	fb.onGetMailboxBoundaryContext = func(context.Context, string, string) (string, uint32, uint32, error) {
		return "imap", 1, 100, nil
	}
	code, parsed, err := reviewProbePost(ts.Client(), ts.URL, token, lease, "probe-expired")
	if err != nil || code != 200 {
		t.Fatalf("initial create: code=%d err=%v body=%v", code, err, parsed)
	}
	id := parsed["data"].(map[string]any)["request_id"].(string)
	if _, err := st.DB().Exec("UPDATE verification_requests SET expires_at=? WHERE request_id=?", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), id); err != nil {
		t.Fatal(err)
	}
	code, parsed, err = reviewProbePost(ts.Client(), ts.URL, token, lease, "probe-expired")
	if err != nil || code != 200 {
		t.Fatalf("replay: code=%d err=%v body=%v", code, err, parsed)
	}
	data := parsed["data"].(map[string]any)
	if data["baseline_ready"] == true {
		t.Fatalf("expired replay is incorrectly advertised as ready to send: status=%v baseline_ready=%v", data["status"], data["baseline_ready"])
	}
}

func TestConcurrentSameKeyDifferentLease(t *testing.T) {
	s, st, fb, ts, token, lease := setupV2TestEnv(t)
	defer s.Close()
	defer ts.Close()
	ctx := context.Background()
	alloc, err := st.GetPrincipalAllocationByID(ctx, lease, "token", "tok_v06_test")
	if err != nil || alloc == nil {
		t.Fatalf("allocation: %v", err)
	}
	copyAlloc := *alloc
	copyAlloc.AllocationID = "probe-second-lease"
	copyAlloc.AliasEmail = "probe-second@icloud.com"
	if err := st.AddInventoryAlias(copyAlloc.AccountID, hme.Alias{Email: copyAlloc.AliasEmail, Active: true}, "replenish", true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordAllocation(&copyAlloc, "probe"); err != nil {
		t.Fatal(err)
	}
	arrivals := make(chan struct{}, 2)
	gate := make(chan struct{})
	var calls atomic.Int32
	fb.onGetMailboxBoundaryContext = func(ctx context.Context, _, _ string) (string, uint32, uint32, error) {
		calls.Add(1)
		arrivals <- struct{}{}
		select {
		case <-gate:
			return "imap", 1, 100, nil
		case <-ctx.Done():
			return "", 0, 0, ctx.Err()
		}
	}
	type response struct {
		code int
		body map[string]any
		err  error
	}
	results := make(chan response, 2)
	client := &http.Client{Timeout: 3 * time.Second}
	for _, l := range []string{lease, copyAlloc.AllocationID} {
		go func(l string) {
			code, body, err := reviewProbePost(client, ts.URL, token, l, "probe-concurrent")
			results <- response{code, body, err}
		}(l)
	}
	select {
	case <-arrivals:
	case <-time.After(time.Second):
		close(gate)
		t.Fatal("first request never reached baseline")
	}
	select {
	case <-arrivals:
	case <-time.After(150 * time.Millisecond):
	}
	close(gate)
	counts := map[int]int{}
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			if r.err != nil {
				t.Errorf("request failed: %v", r.err)
			}
			counts[r.code]++
			if r.code != 200 && r.code != 409 {
				t.Errorf("expected explicit idempotency conflict, got HTTP %d: %v", r.code, r.body)
			}
		case <-time.After(4 * time.Second):
			t.Fatal("request did not finish")
		}
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Errorf("expected one success and one 409, got %v", counts)
	}
	if calls.Load() != 1 {
		t.Errorf("same key triggered %d baseline reads; expected only one", calls.Load())
	}
}

func TestBaselineQueryErrorMustNotReadLegacyMail(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.DB().Exec("ALTER TABLE verification_requests RENAME COLUMN baseline_uid TO review_hidden_baseline_uid"); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	fb := &fakeBackend{}
	fb.onListInboxContext = func(context.Context, InboxQuery) (InboxResult, error) {
		calls.Add(1)
		return InboxResult{}, nil
	}
	w := NewMailSyncWorker(fb, st, mail.NewEventBus(time.Minute), time.Second)
	_, err = w.fetchAndPublishAggregatedBatchResult(context.Background(), &aggregatedInboxBatch{
		fingerprint: "shared", repAccountID: "acc-a", allAliases: []string{"probe@icloud.com"},
		aliasRealAccount: map[string]string{"probe@icloud.com": "acc-a"},
	})
	if err == nil {
		t.Fatal("expected injected baseline query error")
	}
	if calls.Load() != 0 {
		t.Fatalf("baseline query failed, but legacy mail read still ran %d times: %v", calls.Load(), err)
	}
}

func TestProxyCredentialsInSharedInboxLog(t *testing.T) {
	mgr, err := account.NewManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	acc, err := mgr.AddAccount("probe", "", "icloud.com", "socks5://probe-user:probe-password@example.invalid:1080")
	if err != nil {
		t.Fatal(err)
	}
	acc.ICloudEmail = "probe@icloud.com"
	acc.AppPassword = "fake-not-real-app-password"
	b := &managerBackend{mgr: mgr}
	fp, ok := b.GetMailboxEndpointFingerprint(acc.ID)
	if !ok {
		t.Fatal("no fingerprint")
	}
	fb := &fakeBackend{}
	fb.onGetMailboxBoundaryContext = func(context.Context, string, string) (string, uint32, uint32, error) {
		return "", 0, 0, errors.New("injected temporary IMAP failure")
	}
	w := NewMailSyncWorker(fb, nil, mail.NewEventBus(time.Minute), time.Second)
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	_, _ = w.scanAndPublishPagesAggregated(context.Background(), &aggregatedInboxBatch{
		fingerprint: fp, repAccountID: acc.ID, allAliases: []string{"probe@icloud.com"},
	}, []string{"probe@icloud.com"}, map[string]uint32{"probe@icloud.com": 100}, "INBOX", 100)
	if strings.Contains(output.String(), "probe-password") {
		t.Fatal("shared inbox failure log contains plaintext proxy password")
	}
}

func TestCredentialDatabaseFailureIsNotRevocation(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tok, err := st.CreateToken("probe", store.ScopeVerify, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec("DROP TABLE api_tokens"); err != nil {
		t.Fatal(err)
	}
	svc := NewVerificationService(&fakeBackend{}, st, mail.NewEventBus(time.Minute), nil)
	_, err = svc.GetVerificationResult(context.Background(), auth.Principal{
		Kind: auth.PrincipalToken, ID: tok.ID, Scopes: []string{store.ScopeVerify},
	}, "irrelevant", 0, tok.Token)
	var backendErr *BackendError
	if errors.As(err, &backendErr) && backendErr.Code == "TOKEN_REVOKED" {
		t.Fatal("credential DB read failure is incorrectly reported as TOKEN_REVOKED (401)")
	}
	if err == nil {
		t.Fatal("expected credential database failure")
	}
}
