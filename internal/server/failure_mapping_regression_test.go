package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestPOSTBaselineNeedsFiniteDeadline(t *testing.T) {
	s, _, fb, ts, token, lease := setupV2TestEnv(t)
	defer s.Close()
	defer ts.Close()
	var bounded atomic.Bool
	fb.onGetMailboxBoundaryContext = func(ctx context.Context, _, _ string) (string, uint32, uint32, error) {
		_, ok := ctx.Deadline()
		bounded.Store(ok)
		return "imap", 1, 100, nil
	}
	code, _, err := reviewProbePost(ts.Client(), ts.URL, token, lease, "rereview-deadline")
	if err != nil || code != 200 {
		t.Fatalf("create failed: %d %v", code, err)
	}
	if !bounded.Load() {
		t.Fatal("actual HTTP POST baseline receives no independent server phase deadline; HTTP body read deadline is not a baseline/commit stage budget")
	}
}

func TestExpiredReplayMustNotHideWriteFailure(t *testing.T) {
	s, st, fb, ts, token, lease := setupV2TestEnv(t)
	defer s.Close()
	defer ts.Close()
	fb.onGetMailboxBoundaryContext = func(context.Context, string, string) (string, uint32, uint32, error) {
		return "imap", 1, 100, nil
	}
	code, body, err := reviewProbePost(ts.Client(), ts.URL, token, lease, "rereview-expire-write")
	if err != nil || code != 200 {
		t.Fatalf("create failed: %d %v %v", code, err, body)
	}
	id := body["data"].(map[string]any)["request_id"].(string)
	if _, err := st.DB().Exec("UPDATE verification_requests SET expires_at=? WHERE request_id=?", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec("CREATE TRIGGER rereview_expire_failure BEFORE UPDATE OF status ON verification_requests WHEN NEW.status='expired' BEGIN SELECT RAISE(ABORT,'injected expiry persistence failure'); END"); err != nil {
		t.Fatal(err)
	}
	code, body, err = reviewProbePost(ts.Client(), ts.URL, token, lease, "rereview-expire-write")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := st.GetVerificationRequest(context.Background(), id, "token", "tok_v06_test")
	if err != nil {
		t.Fatal(err)
	}
	if code < 500 {
		t.Fatalf("expiry write failed but HTTP returned %d body=%v while durable status=%s", code, body, rec.Status)
	}
}

func TestAllocationDatabaseFailureIsNot404(t *testing.T) {
	s, st, fb, ts, token, lease := setupV2TestEnv(t)
	defer s.Close()
	defer ts.Close()
	fb.onGetMailboxBoundaryContext = func(context.Context, string, string) (string, uint32, uint32, error) {
		return "imap", 1, 100, nil
	}
	if _, err := st.DB().Exec("ALTER TABLE alias_allocations RENAME TO rereview_unavailable_allocations"); err != nil {
		t.Fatal(err)
	}
	code, body, err := reviewProbePost(ts.Client(), ts.URL, token, lease, "rereview-alloc-error")
	if err != nil {
		t.Fatal(err)
	}
	if code < 500 {
		t.Fatalf("allocation DB failure incorrectly becomes HTTP %d: %v", code, body)
	}
}

func TestUpstreamBusyCarriesHTTPRetryAfter(t *testing.T) {
	s, _, fb, ts, token, lease := setupV2TestEnv(t)
	defer s.Close()
	defer ts.Close()
	fb.onGetMailboxBoundaryContext = func(context.Context, string, string) (string, uint32, uint32, error) {
		return "", 0, 0, &BackendError{Status: 503, Code: "SERVER_BUSY", Message: "injected busy", Data: map[string]any{"retry_after": 2}}
	}
	b, _ := json.Marshal(map[string]string{"lease_id": lease})
	req, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/verification-requests", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("expected 503 got %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") != "2" {
		t.Fatalf("busy HTTP response has Retry-After=%q; backend Data is discarded", resp.Header.Get("Retry-After"))
	}
}

func TestShutdownMustCancelAcceptedHTTPWaiters(t *testing.T) {
	s, st, fb, ts, token, lease := setupV2TestEnv(t)
	defer s.Close()
	defer ts.Close()
	fb.onGetMailboxBoundaryContext = func(context.Context, string, string) (string, uint32, uint32, error) {
		return "imap", 1, 100, nil
	}
	code, body, err := reviewProbePost(ts.Client(), ts.URL, token, lease, "rereview-shutdown")
	if err != nil || code != 200 {
		t.Fatalf("create: %d %v", code, err)
	}
	id := body["data"].(map[string]any)["request_id"].(string)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/external/v2/verification-requests/"+id+"?timeout=120", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, _ := ts.Client().Do(req)
		if resp != nil {
			resp.Body.Close()
		}
	}()
	until := time.Now().Add(2 * time.Second)
	for s.requestLimiter.Stats().ActiveWaiters != 1 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if s.requestLimiter.Stats().ActiveWaiters != 1 {
		cancel()
		<-done
		t.Fatal("waiter never entered")
	}
	// Execute the same Shutdown -> CloseContext ordering as Server.Run, with a shortened HTTP wait budget.
	httpCtx, httpCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	_ = ts.Config.Shutdown(httpCtx)
	httpCancel()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
	closeErr := s.CloseContext(closeCtx)
	closeCancel()
	if closeErr != nil {
		cancel()
		<-done
		t.Fatalf("cleanup failed: %v", closeErr)
	}
	dbErr := st.DB().Ping()
	stillWaiting := s.requestLimiter.Stats().ActiveWaiters
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("client did not exit after explicit cancellation")
	}
	if stillWaiting != 0 {
		t.Fatalf("Run shutdown ordering closed storage (%v) while %d accepted HTTP long poll remained live; service cancellation does not reach request context", dbErr, stillWaiting)
	}
}
