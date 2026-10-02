package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	imapclient "github.com/emersion/go-imap/client"
	"icloud-hme/internal/account"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

func TestEmailAllocationDBFailureIsNot404(t *testing.T) {
	s, st, _, ts, token, _ := setupV2TestEnv(t)
	defer s.Close()
	defer ts.Close()
	if _, err := st.DB().Exec("ALTER TABLE alias_allocations RENAME TO round3_unavailable_allocations"); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/verification-requests", strings.NewReader(`{"email":"target_alias@icloud.com"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("email-form allocation DB failure must be 500; got status=%d body=%v", resp.StatusCode, body)
	}
}

func TestShutdownMustDrainHTTPCreateBeforeClosingStore(t *testing.T) {
	s, st, fb, ts, token, lease := setupV2TestEnv(t)
	defer s.Close()
	defer ts.Close()
	entered := make(chan struct{})
	gate := make(chan struct{})
	fb.onGetMailboxBoundaryContext = func(ctx context.Context, _, _ string) (string, uint32, uint32, error) {
		close(entered)
		select {
		case <-gate:
			return "imap", 1, 100, nil
		case <-ctx.Done():
			return "", 0, 0, ctx.Err()
		}
	}
	defer func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	}()
	done := make(chan int, 1)
	go func() {
		status, _, _ := reviewProbePost(ts.Client(), ts.URL, token, lease, "round3-create-shutdown")
		done <- status
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("baseline callback did not enter")
	}
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 30*time.Millisecond)
	drainErr := ts.Config.Shutdown(drainCtx)
	cancelDrain()
	closeCtx, cancelClose := context.WithTimeout(context.Background(), time.Second)
	closeErr := s.CloseContext(closeCtx)
	cancelClose()
	stats := s.requestLimiter.Stats()
	dbErr := st.DB().Ping()
	close(gate)
	var status int
	select {
	case status = <-done:
	case <-time.After(time.Second):
		t.Fatal("POST client did not exit")
	}
	t.Logf("Shutdown=%v CloseContext=%v ActiveInflight=%d ActiveWaiters=%d DBPing=%v POST=%d", drainErr, closeErr, stats.ActiveInflight, stats.ActiveWaiters, dbErr, status)
	if dbErr != nil && stats.ActiveInflight > 0 {
		t.Fatal("Store closed while an accepted HTTP POST still owned its inflight slot and baseline callback")
	}
}

func TestDefaultDrainMustCoverSlowHTTPBody(t *testing.T) {
	s, st, _, ts, token, lease := setupV2TestEnv(t)
	defer s.Close()
	defer ts.Close()
	clientCtx, cancelClient := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelClient()
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	body, _ := json.Marshal(map[string]string{"lease_id": lease})
	req, _ := http.NewRequestWithContext(clientCtx, "POST", ts.URL+"/api/external/v2/verification-requests", reader)
	req.ContentLength = int64(len(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	done := make(chan int, 1)
	go func() {
		resp, _ := ts.Client().Do(req)
		status := 0
		if resp != nil {
			status = resp.StatusCode
			resp.Body.Close()
		}
		done <- status
	}()
	if _, err := writer.Write(body[:1]); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Second)
	for s.requestLimiter.Stats().ActiveInflight != 1 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if s.requestLimiter.Stats().ActiveInflight != 1 {
		cancelClient()
		<-done
		t.Fatal("POST did not enter HTTP admission")
	}
	// Keep the production HTTP drain budget (10s), while the legitimate body budget is 15s.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), defaultShutdownTimeout)
	drainErr := ts.Config.Shutdown(drainCtx)
	cancelDrain()
	closeCtx, cancelClose := context.WithTimeout(context.Background(), time.Second)
	closeErr := s.CloseContext(closeCtx)
	cancelClose()
	stats := s.requestLimiter.Stats()
	dbErr := st.DB().Ping()
	if _, err := writer.Write(body[1:]); err != nil {
		t.Logf("body completion: %v", err)
	}
	writer.Close()
	var status int
	select {
	case status = <-done:
	case <-time.After(time.Second):
		cancelClient()
		<-done
		t.Fatal("slow-body POST client did not exit")
	}
	t.Logf("productionDrainBudget=%v Shutdown=%v CloseContext=%v ActiveInflight=%d DBPing=%v POST=%d", defaultShutdownTimeout, drainErr, closeErr, stats.ActiveInflight, dbErr, status)
	if dbErr != nil && stats.ActiveInflight > 0 {
		t.Fatal("the unchanged production 10s drain closes Store while a body within its 15s reading budget is still accepted")
	}
}

func TestLegacyWaiterShutdownAndCleanupMustComplete(t *testing.T) {
	s, st, _, ts, token, _ := setupV2TestEnv(t)
	defer st.Close()
	defer s.Close()
	defer ts.Close()
	clientCtx, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequestWithContext(clientCtx, "GET", ts.URL+"/mail/code?email=target_alias@icloud.com&timeout=120&fresh=true", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, _ := ts.Client().Do(req)
		if resp != nil {
			resp.Body.Close()
		}
	}()
	until := time.Now().Add(time.Second)
	for s.requestLimiter.Stats().ActiveWaiters != 1 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if s.requestLimiter.Stats().ActiveWaiters != 1 {
		cancelClient()
		<-done
		t.Fatal("legacy waiter did not enter")
	}
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 30*time.Millisecond)
	_ = ts.Config.Shutdown(drainCtx)
	cancelDrain()
	closeCtx, cancelClose := context.WithTimeout(context.Background(), 6*time.Second)
	firstCloseErr := s.CloseContext(closeCtx)
	cancelClose()
	stillWaiting := s.requestLimiter.Stats().ActiveWaiters
	beforePing := st.DB().Ping()
	cancelClient()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("legacy client did not exit after explicit cancellation")
	}
	until = time.Now().Add(time.Second)
	for s.requestLimiter.Stats().ActiveWaiters != 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	secondCtx, cancelSecond := context.WithTimeout(context.Background(), time.Second)
	secondCloseErr := s.CloseContext(secondCtx)
	cancelSecond()
	afterPing := st.DB().Ping()
	t.Logf("firstClose=%v waiters=%d DBPingBeforeCancel=%v; secondClose=%v waitersNow=%d DBPingAfterCancel=%v", firstCloseErr, stillWaiting, beforePing, secondCloseErr, s.requestLimiter.Stats().ActiveWaiters, afterPing)
	if stillWaiting != 0 {
		t.Error("server cancellation did not reach the accepted legacy HTTP waiter")
	}
	if secondCloseErr != nil || afterPing == nil {
		t.Error("after the waiter exits, cleanup must continue to completion; closeOnce currently freezes an incomplete cleanup forever")
	}
}

func TestIMAPPoolBusyMessageDetailHTTPMustReturn503(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveAccount(&store.AccountRecord{ID: "round3_target", Name: "target", ICloudEmail: "round3-target@icloud.com", AppPassword: "fake-password", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	mgr, err := account.NewManager(t.TempDir(), st)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	pool := mail.NewPoolWithLimits(1, 3, 1)
	mgr.SetIMAPPoolForTest(pool)
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		defer serverConn.Close()
		serverConn.Write([]byte("* OK [CAPABILITY IMAP4rev1] Local audit only\r\n"))
		reader := bufio.NewReader(serverConn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			if strings.EqualFold(fields[1], "LOGOUT") {
				serverConn.Write([]byte("* BYE Local audit shutdown\r\n" + fields[0] + " OK LOGOUT\r\n"))
				return
			}
			serverConn.Write([]byte(fields[0] + " OK completed\r\n"))
		}
	}()
	imapConn, err := imapclient.New(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	pool.SetClientForTesting("holder@invalid.example", "fake-password", mail.NewClientForTesting("holder@invalid.example", "fake-password", clientConn, imapConn))
	gate, entered := make(chan struct{}), make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- pool.DoContext(mail.WithBackgroundOp(context.Background()), "holder@invalid.example", "fake-password", "", func(*mail.Client) error {
			close(entered)
			<-gate
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-holderDone:
		t.Fatalf("holder setup failed: %v", err)
	case <-time.After(time.Second):
		t.Fatal("holder did not enter")
	}
	var releaseOnce sync.Once
	releaseHolder := func() { releaseOnce.Do(func() { close(gate); <-holderDone }) }
	defer releaseHolder()
	be := &managerBackend{mgr: mgr, store: st}
	s := newWithBackendAndStore(be, Config{AdminPassword: "round3-admin-password"}, st)
	defer func() { releaseHolder(); s.Close() }()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	token, err := st.CreateToken("round3-admin", store.ScopeAdmin, "")
	if err != nil {
		t.Fatal(err)
	}
	ref := mail.MessageRef{Provider: "imap", AccountID: "round3_target", Mailbox: "INBOX", UIDValidity: 1, UID: 101}
	req, _ := http.NewRequest("GET", ts.URL+"/api/inbox/"+url.PathEscape(ref.Encode())+"?account_id=round3_target", nil)
	req.Header.Set("Authorization", "Bearer "+token.Token)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "2" {
		t.Fatalf("mail pool capacity rejection must be 503 SERVER_BUSY with Retry-After=2; got %d header=%q body=%v", resp.StatusCode, resp.Header.Get("Retry-After"), result)
	}
}
