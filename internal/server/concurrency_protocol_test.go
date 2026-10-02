// [INPUT]: Production Config/Server/Manager, local TCP HTTP/IMAP and temporary SQLite.
// [OUTPUT]: 4x500/1x2000 HTTP-created tasks, mixed-load delivery and simultaneous 2000-result regression.
// [POS]: internal/server protocol capacity regression; does not test provider TLS or Linux signals.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/memory"
	imapclient "github.com/emersion/go-imap/client"
	imapserver "github.com/emersion/go-imap/server"
	"icloud-hme/internal/account"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

type capacityMailbox struct {
	backend.Mailbox
	mu                                       sync.Mutex
	statusCalls, searches, fetches, failures atomic.Int64
}

func (m *capacityMailbox) Status(items []imap.StatusItem) (*imap.MailboxStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statusCalls.Add(1)
	s, err := m.Mailbox.Status(items)
	if err != nil {
		m.failures.Add(1)
	}
	return s, err
}
func (m *capacityMailbox) SearchMessages(uid bool, c *imap.SearchCriteria) ([]uint32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.searches.Add(1)
	ids, err := m.Mailbox.SearchMessages(uid, c)
	if err != nil {
		m.failures.Add(1)
	}
	return ids, err
}
func (m *capacityMailbox) ListMessages(uid bool, set *imap.SeqSet, items []imap.FetchItem, ch chan<- *imap.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fetches.Add(1)
	err := m.Mailbox.ListMessages(uid, set, items, ch)
	if err != nil {
		m.failures.Add(1)
	}
	return err
}
func (m *capacityMailbox) appendCodes(aliases []string, codes []string) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, alias := range aliases {
		body := fmt.Sprintf("From: verification@example.invalid\r\nTo: %s\r\nDelivered-To: %s\r\nSubject: Your verification code\r\nDate: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nYour verification code is %s.\r\n", alias, alias, time.Now().Format(time.RFC1123Z), codes[i])
		if err := m.Mailbox.CreateMessage(nil, time.Now(), strings.NewReader(body)); err != nil {
			return time.Time{}, err
		}
	}
	// All messages are available atomically before the worker can read this mailbox.
	return time.Now(), nil
}

type capacityIMAPUser struct {
	backend.User
	mailbox *capacityMailbox
}

func (u *capacityIMAPUser) GetMailbox(name string) (backend.Mailbox, error) {
	if name != "INBOX" {
		return nil, backend.ErrNoSuchMailbox
	}
	return u.mailbox, nil
}
func (u *capacityIMAPUser) ListMailboxes(bool) ([]backend.Mailbox, error) {
	return []backend.Mailbox{u.mailbox}, nil
}

type capacityIMAPBackend struct{ user *capacityIMAPUser }

func (b *capacityIMAPBackend) Login(_ *imap.ConnInfo, username, password string) (backend.User, error) {
	if username != "shared@invalid.example" || password != "password" {
		return nil, errors.New("invalid fixture credentials")
	}
	return b.user, nil
}

func newCapacityIMAP(t *testing.T) (*capacityMailbox, *mail.Pool, string, int) {
	t.Helper()
	base := memory.New()
	u, err := base.Login(nil, "username", "password")
	if err != nil {
		t.Fatal(err)
	}
	mbox, err := u.GetMailbox("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	mbox.(*memory.Mailbox).Messages = nil
	m := &capacityMailbox{Mailbox: mbox}
	imaps := imapserver.New(&capacityIMAPBackend{user: &capacityIMAPUser{User: u, mailbox: m}})
	imaps.AllowInsecureAuth = true
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = imaps.Serve(ln) }()
	t.Cleanup(func() { imaps.Close(); ln.Close(); <-done })
	conn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	cli, err := imapclient.New(conn)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	cli.Timeout = 3 * time.Second
	if err := cli.Login("shared@invalid.example", "password"); err != nil {
		cli.Terminate()
		t.Fatal(err)
	}
	conn.SetDeadline(time.Time{})
	p := mail.NewPool()
	host, port := "127.0.0.1", ln.Addr().(*net.TCPAddr).Port
	p.SetClientForTestingWithServer("shared@invalid.example", "password", host, port, mail.NewClientForTesting("shared@invalid.example", "password", conn, cli))
	t.Cleanup(p.Close)
	return m, p, host, port
}

type capacityReply struct {
	Success bool   `json:"success"`
	Code    string `json:"code"`
	Data    struct {
		RequestID  string `json:"request_id"`
		Status     string `json:"status"`
		Code       string `json:"code"`
		MessageRef string `json:"message_ref"`
	} `json:"data"`
	status     int
	retryAfter string
}
type capacityCounters struct {
	calls, dials, dialFailures, requestFailures, decodeFailures, unexpectedStatuses, cancellations atomic.Int64
	shutdown503, shutdown499                                                                       atomic.Int64
	stopping                                                                                       atomic.Bool
}

func (m *capacityCounters) call(ctx context.Context, c *http.Client, baseURL, token, method, path, body string, expected int) (capacityReply, error) {
	m.calls.Add(1)
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, strings.NewReader(body))
	if err != nil {
		return capacityReply{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			m.cancellations.Add(1)
		} else {
			m.requestFailures.Add(1)
		}
		return capacityReply{}, err
	}
	defer resp.Body.Close()
	var result capacityReply
	result.status, result.retryAfter = resp.StatusCode, resp.Header.Get("Retry-After")
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		if ctx.Err() != nil {
			m.cancellations.Add(1)
		} else {
			m.decodeFailures.Add(1)
		}
		return result, err
	}
	if resp.StatusCode != expected {
		switch {
		case m.stopping.Load() && resp.StatusCode == 503 && result.Code == "SERVER_SHUTTING_DOWN":
			m.shutdown503.Add(1)
		case m.stopping.Load() && resp.StatusCode == 499 && result.Code == "REQUEST_CANCELED":
			m.shutdown499.Add(1)
		default:
			m.unexpectedStatuses.Add(1)
		}
	}
	return result, nil
}

func TestConcurrencyScale_ProductionProtocol2000Tasks60Seconds(t *testing.T) {
	runProductionProtocolCapacity(t, 4, 60*time.Second, false)
}

func TestConcurrencyScale_SinglePrincipal2000TasksBurst(t *testing.T) {
	runProductionProtocolCapacity(t, 1, 60*time.Second, true)
}

func runProductionProtocolCapacity(t *testing.T, principals int, steadyDuration time.Duration, burstAll bool) {
	t.Helper()
	if testing.Short() {
		t.Skip("protocol capacity test")
	}
	perPrincipal := 2000 / principals
	globalTaskLimit := 2000
	if principals == 1 {
		globalTaskLimit = 4000 // Business setting: one token at 2000 tasks, with global headroom.
	}
	mbox, pool, host, port := newCapacityIMAP(t)
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mailboxJSON, _ := json.Marshal(account.MailboxConfig{Provider: "imap", Email: "shared@invalid.example", IMAPHost: host, IMAPPort: port, Password: "password"})
	for i := 0; i < 10; i++ {
		if err := st.SaveAccount(&store.AccountRecord{ID: fmt.Sprintf("capacity_acc_%d", i), Name: fmt.Sprintf("capacity_%d", i), RealEmail: "shared@invalid.example", MailboxJSON: string(mailboxJSON), Host: "icloud.com", Status: "active", CookiesJSON: "{}", TagsJSON: "[]"}); err != nil {
			t.Fatal(err)
		}
	}
	mgr, err := account.NewManager(t.TempDir(), st)
	if err != nil {
		t.Fatal(err)
	}
	// Close the constructor's unused pool before substituting the local protocol connection.
	// All subsequent reconnections target 127.0.0.1, never a public provider.
	mgr.SetIMAPPoolForTest(pool)
	t.Cleanup(mgr.Close)
	cfg := Config{AdminPassword: "capacity-test-password", MailPollInterval: 2 * time.Second,
		MaxInflightGlobal: 128, MaxInflightPerPrincipal: 64, MaxGlobalActiveVReq: globalTaskLimit, MaxPerPrincipalActiveVReq: perPrincipal,
		MaxWaitersGlobal: 4000, MaxWaitersPerPrincipal: 2000, MaxWaitersPerKey: 8}
	s, err := New(mgr, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	s.syncWorker.Start()

	var metrics capacityCounters
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	tr := &http.Transport{MaxConnsPerHost: 4000, MaxIdleConns: 4000, MaxIdleConnsPerHost: 4000,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			metrics.dials.Add(1)
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				metrics.dialFailures.Add(1)
			}
			return conn, err
		}}
	client := &http.Client{Transport: tr, Timeout: 130 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	tokens := make([]*store.CreatedToken, principals+1)
	for i := range tokens {
		tokens[i], err = st.CreateToken(fmt.Sprintf("capacity_%d", i), store.ScopeVerify, "")
		if err != nil {
			t.Fatal(err)
		}
	}
	type task struct {
		id, alias, lease, accountID string
		token                       *store.CreatedToken
	}
	tasks := make([]task, 2000)
	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := range tasks {
		tasks[i] = task{alias: fmt.Sprintf("capacity_%04d@icloud.com", i), lease: fmt.Sprintf("capacity_lease_%d", i), accountID: fmt.Sprintf("capacity_acc_%d", i%10), token: tokens[i/perPrincipal]}
		task := tasks[i]
		if _, err := tx.ExecContext(ctx, `INSERT INTO alias_allocations(allocation_id,alias_email,account_id,owner_kind,owner_id,status,allocated_at) VALUES(?,?,?,'token',?,'allocated',?)`, task.lease, task.alias, task.accountID, task.token.ID, time.Now().UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO alias_routes(email,account_id,updated_at) VALUES(?,?,?)`, task.alias, task.accountID, time.Now().UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	for i, tok := range []*store.CreatedToken{tokens[0], tokens[principals]} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO alias_allocations(allocation_id,alias_email,account_id,owner_kind,owner_id,status,allocated_at) VALUES(?,?,'capacity_acc_0','token',?,'allocated',?)`, fmt.Sprintf("overflow_%d", i), fmt.Sprintf("overflow_%d@icloud.com", i), tok.ID, time.Now().UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	create := func(i int) error {
		reply, err := metrics.call(ctx, client, ts.URL, tasks[i].token.Token, "POST", "/api/external/v2/verification-requests", fmt.Sprintf(`{"lease_id":%q}`, tasks[i].lease), 200)
		if err != nil {
			return err
		}
		if !reply.Success || reply.status != 200 || reply.Data.Status != "ready" || reply.Data.RequestID == "" {
			return fmt.Errorf("create %d failed: %+v", i, reply)
		}
		tasks[i].id = reply.Data.RequestID
		return nil
	}
	creationStart := time.Now()
	for principal := 0; principal < principals; principal++ {
		jobs := make(chan int, perPrincipal)
		failures := make(chan error, perPrincipal)
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range jobs {
					if err := create(i); err != nil {
						failures <- err
					}
				}
			}()
		}
		for i := principal * perPrincipal; i < (principal+1)*perPrincipal; i++ {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		close(failures)
		for err := range failures {
			t.Error(err)
		}
		if t.Failed() {
			t.FailNow()
		}
		if principal == 0 {
			reply, err := metrics.call(ctx, client, ts.URL, tokens[0].Token, "POST", "/api/external/v2/verification-requests", `{"lease_id":"overflow_0"}`, 429)
			if err != nil || reply.status != 429 || reply.Code != "TOO_MANY_REQUESTS" || reply.retryAfter != "2" {
				t.Fatalf("principal task %d: %+v err=%v", perPrincipal+1, reply, err)
			}
		}
	}
	creationDuration := time.Since(creationStart)
	count, err := st.CountActiveVerificationRequests(ctx)
	if err != nil || count != 2000 {
		t.Fatalf("active tasks=%d err=%v", count, err)
	}
	for _, tok := range tokens[:principals] {
		count, err := st.CountActiveVerificationRequestsByPrincipal(ctx, "token", tok.ID)
		if err != nil || count != perPrincipal {
			t.Fatalf("principal tasks=%d err=%v", count, err)
		}
	}
	if principals > 1 {
		reply, err := metrics.call(ctx, client, ts.URL, tokens[principals].Token, "POST", "/api/external/v2/verification-requests", `{"lease_id":"overflow_1"}`, 503)
		if err != nil || reply.status != 503 || reply.Code != "SERVER_BUSY" || reply.retryAfter != "2" {
			t.Fatalf("2001st global task: %+v err=%v", reply, err)
		}
	}
	reply, err := metrics.call(ctx, client, ts.URL, tokens[principals].Token, "GET", "/api/external/v2/verification-requests/"+tasks[0].id+"?timeout=0", "", 404)
	if err != nil || reply.status != 404 {
		t.Fatalf("cross-principal read: %+v err=%v", reply, err)
	}

	type delivery struct {
		reply capacityReply
		err   error
	}
	results := make([]chan delivery, 2000)
	cancels := make([]context.CancelFunc, 2000)
	var clients sync.WaitGroup
	launch := func(i int) {
		waitCtx, stop := context.WithCancel(ctx)
		cancels[i] = stop
		ch := make(chan delivery, 1)
		results[i] = ch
		id, token := tasks[i].id, tasks[i].token.Token
		clients.Add(1)
		go func() {
			defer clients.Done()
			r, e := metrics.call(waitCtx, client, ts.URL, token, "GET", "/api/external/v2/verification-requests/"+id+"?timeout=120", "", 200)
			ch <- delivery{r, e}
		}()
	}
	t.Cleanup(func() {
		cancel()
		done := make(chan struct{})
		go func() { clients.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			ts.CloseClientConnections()
			t.Error("capacity clients did not join")
		}
	})
	awaitWaiters := func(want int) {
		until := time.Now().Add(10 * time.Second)
		for time.Now().Before(until) {
			stats := s.requestLimiter.Stats()
			if stats.ActiveWaiters == want && stats.ActiveInflight == 0 {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("waiters=%d want=%d; request failures=%d statuses=%d", s.requestLimiter.Stats().ActiveWaiters, want, metrics.requestFailures.Load(), metrics.unexpectedStatuses.Load())
	}
	launch(0)
	awaitWaiters(1)
	// One original waiter + seven duplicate readers fill the per-task limit of eight.
	duplicateCtx, stopDuplicates := context.WithCancel(ctx)
	var duplicates sync.WaitGroup
	t.Cleanup(func() { stopDuplicates(); duplicates.Wait() })
	for range 7 {
		duplicates.Add(1)
		go func() {
			defer duplicates.Done()
			metrics.call(duplicateCtx, client, ts.URL, tokens[0].Token, "GET", "/api/external/v2/verification-requests/"+tasks[0].id+"?timeout=120", "", 200)
		}()
	}
	awaitWaiters(8)
	reply, err = metrics.call(ctx, client, ts.URL, tokens[0].Token, "GET", "/api/external/v2/verification-requests/"+tasks[0].id+"?timeout=120", "", 429)
	stopDuplicates()
	duplicates.Wait()
	awaitWaiters(1)
	if err != nil || reply.status != 429 || reply.Code != "VERIFY_WAITER_LIMIT" || reply.retryAfter != "2" {
		t.Fatalf("9th waiter: %+v err=%v", reply, err)
	}
	for i := 1; i < len(tasks); i++ {
		launch(i)
		if i%10 == 9 {
			awaitWaiters(i + 1) // Measure held capacity without overrunning short-stage admission.
		}
	}
	awaitWaiters(2000)
	if principals == 1 {
		reply, err := metrics.call(ctx, client, ts.URL, tokens[0].Token, "GET", "/api/external/v2/verification-requests/"+tasks[0].id+"?timeout=120", "", 429)
		if err != nil || reply.status != 429 || reply.Code != "TOKEN_CONCURRENCY_LIMIT" || reply.retryAfter != "2" {
			t.Fatalf("2001st principal waiter: %+v err=%v", reply, err)
		}
	}

	steadyStart := time.Now()
	var latencies []time.Duration
	completedRounds, reconnected, shortCalls, foregroundCalls, snapshots := 0, false, 0, 0, 0
	for time.Since(steadyStart) < steadyDuration {
		elapsed := time.Since(steadyStart)
		if (completedRounds == 0 && elapsed >= steadyDuration/6) || (completedRounds == 1 && elapsed >= steadyDuration/2) {
			aliases, codes := make([]string, 100), make([]string, 100)
			for i := range aliases {
				aliases[i] = tasks[i].alias
				codes[i] = fmt.Sprintf("%06d", 640000+completedRounds*1000+i)
			}
			available, err := mbox.appendCodes(aliases, codes)
			if err != nil {
				t.Fatal(err)
			}
			// Do not call syncOnce or Trigger: actual Worker.Start periodic polling must deliver.
			deadline := time.NewTimer(10 * time.Second)
			for i := range aliases {
				select {
				case d := <-results[i]:
					if d.err != nil || !d.reply.Success || d.reply.Data.Status != "succeeded" || d.reply.Data.Code != codes[i] {
						t.Fatalf("round %d delivery %d: %+v err=%v", completedRounds+1, i, d.reply, d.err)
					}
					ref, err := mail.ParseMessageRef(d.reply.Data.MessageRef, tasks[i].accountID)
					if err != nil || ref.AccountID != tasks[i].accountID {
						t.Fatalf("cross-account delivery %d ref=%+v err=%v", i, ref, err)
					}
					latencies = append(latencies, time.Since(available))
					cancels[i]()
				case <-deadline.C:
					t.Fatalf("periodic worker delivery timed out in round %d", completedRounds+1)
				}
			}
			deadline.Stop()
			for i := range aliases {
				if err := create(i); err != nil {
					t.Fatal(err)
				}
				launch(i)
			}
			awaitWaiters(2000)
			completedRounds++
		}
		if !reconnected && elapsed >= steadyDuration*3/4 {
			for i := 300; i < 320; i++ {
				cancels[i]()
				<-results[i]
			}
			awaitWaiters(1980)
			for i := 300; i < 320; i++ {
				launch(i)
			}
			awaitWaiters(2000)
			reconnected = true
		}
		shortCtx, stopShort := context.WithTimeout(ctx, 2*time.Second)
		reply, err := metrics.call(shortCtx, client, ts.URL, tasks[500].token.Token, "GET", "/api/external/v2/verification-requests/"+tasks[500].id+"?timeout=0", "", 200)
		stopShort()
		if err != nil || !reply.Success || reply.Data.Status != "pending" {
			t.Fatalf("mixed short request: %+v err=%v", reply, err)
		}
		shortCalls++
		if shortCalls%4 == 0 {
			fgCtx, stopFG := context.WithTimeout(ctx, 2*time.Second)
			_, _, _, err := s.be.GetMailboxBoundaryContext(fgCtx, tasks[500].accountID, "INBOX")
			stopFG()
			if err != nil {
				t.Fatal(err)
			}
			foregroundCalls++
		}
		stats := s.requestLimiter.Stats()
		if stats.ActiveWaiters != 2000 || stats.ActiveInflight != 0 {
			t.Fatalf("steady resources: %+v", stats)
		}
		snapshots++
		time.Sleep(250 * time.Millisecond)
	}
	minShort, minForeground := 8, 2
	if steadyDuration >= 60*time.Second {
		minShort, minForeground = 100, 25
	}
	if completedRounds != 2 || !reconnected || shortCalls < minShort || foregroundCalls < minForeground {
		t.Fatalf("incomplete workload rounds=%d reconnect=%v short=%d foreground=%d", completedRounds, reconnected, shortCalls, foregroundCalls)
	}
	if metrics.dialFailures.Load() != 0 || metrics.requestFailures.Load() != 0 || metrics.decodeFailures.Load() != 0 || metrics.unexpectedStatuses.Load() != 0 || mbox.failures.Load() != 0 {
		t.Fatalf("capacity failures: dial=%d request=%d decode=%d status=%d imap=%d", metrics.dialFailures.Load(), metrics.requestFailures.Load(), metrics.decodeFailures.Load(), metrics.unexpectedStatuses.Load(), mbox.failures.Load())
	}
	count, err = st.CountActiveVerificationRequests(ctx)
	if err != nil || count != 2000 {
		t.Fatalf("steady active task count=%d err=%v", count, err)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	t.Logf("CAPACITY created_by_POST=2200 active=2000 principals=%dx%d global_task_limit=%d accounts=10 shared_IMAP_connections=1 initial_creation=%v steady=%v full_waiter_snapshots=%d short_HTTP=%d foreground_IMAP=%d deliveries=200 reconnects=20", principals, perPrincipal, globalTaskLimit, creationDuration, time.Since(steadyStart), snapshots, shortCalls, foregroundCalls)
	t.Logf("DELIVERY batch_available_to_decoded_and_validated_response p50=%v p99=%v max=%v; IMAP status_calls=%d searches=%d fetches=%d", latencies[len(latencies)/2], latencies[len(latencies)*99/100], latencies[len(latencies)-1], mbox.statusCalls.Load(), mbox.searches.Load(), mbox.fetches.Load())
	expectedShutdown := 2000
	if burstAll {
		aliases, codes := make([]string, len(tasks)), make([]string, len(tasks))
		for i := range tasks {
			aliases[i], codes[i] = tasks[i].alias, fmt.Sprintf("%06d", 750000+i)
		}
		available, err := mbox.appendCodes(aliases, codes)
		if err != nil {
			t.Fatal(err)
		}
		burstLatencies := make([]time.Duration, 0, len(tasks))
		deadline := time.NewTimer(30 * time.Second)
		defer deadline.Stop()
		for i := range tasks {
			select {
			case d := <-results[i]:
				if d.err != nil || !d.reply.Success || d.reply.Data.Status != "succeeded" || d.reply.Data.Code != codes[i] {
					t.Fatalf("burst delivery %d: %+v err=%v", i, d.reply, d.err)
				}
				ref, err := mail.ParseMessageRef(d.reply.Data.MessageRef, tasks[i].accountID)
				if err != nil || ref.AccountID != tasks[i].accountID {
					t.Fatalf("burst cross-account delivery %d ref=%+v err=%v", i, ref, err)
				}
				burstLatencies = append(burstLatencies, time.Since(available))
				cancels[i]()
			case <-deadline.C:
				t.Fatalf("2000-result burst timed out after %d deliveries", len(burstLatencies))
			}
		}
		awaitWaiters(0)
		count, err := st.CountActiveVerificationRequests(ctx)
		if err != nil || count != 0 {
			t.Fatalf("burst did not release task capacity: active=%d err=%v", count, err)
		}
		sort.Slice(burstLatencies, func(i, j int) bool { return burstLatencies[i] < burstLatencies[j] })
		t.Logf("BURST deliveries=2000 remaining_active_tasks=0 waiters=0 p50=%v p99=%v max=%v", burstLatencies[1000], burstLatencies[1980], burstLatencies[1999])
		// Prove quota exhaustion recovers through a fresh POST and a new baseline.
		if err := create(0); err != nil {
			t.Fatal(err)
		}
		launch(0)
		awaitWaiters(1)
		expectedShutdown = 1
	}

	// Remaining 120-second waiters are canceled through production shutdown.
	metrics.stopping.Store(true)
	if err := s.shutdownHTTP(ts.Config); err != nil {
		t.Fatal(err)
	}
	joined := make(chan struct{})
	go func() { clients.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(3 * time.Second):
		t.Fatal("clients survived completed shutdown")
	}
	stats := s.requestLimiter.Stats()
	conns, active, fg := pool.Stats()
	if stats.ActiveWaiters != 0 || stats.ActiveInflight != 0 || conns != 0 || active != 0 || fg != 0 || st.DB().Ping() == nil {
		t.Fatalf("shutdown resources waiters=%d inflight=%d imap=%d/%d/%d", stats.ActiveWaiters, stats.ActiveInflight, conns, active, fg)
	}
	if metrics.dialFailures.Load() != 0 || metrics.requestFailures.Load() != 0 || metrics.decodeFailures.Load() != 0 || metrics.unexpectedStatuses.Load() != 0 || metrics.shutdown503.Load()+metrics.shutdown499.Load() != int64(expectedShutdown) {
		t.Fatalf("shutdown responses: unexpected=%d shutdown503=%d shutdown499=%d failures=%d/%d/%d", metrics.unexpectedStatuses.Load(), metrics.shutdown503.Load(), metrics.shutdown499.Load(), metrics.dialFailures.Load(), metrics.requestFailures.Load(), metrics.decodeFailures.Load())
	}
	t.Logf("ATTEMPTS HTTP=%d TCP_dials=%d TCP_failures=%d HTTP_failures=%d decode_failures=%d unexpected_status=%d expected_client_cancellations=%d shutdown_503=%d shutdown_499=%d; shutdown_waiters=0 shutdown_inflight=0 IMAP_pool=0 client_goroutines_joined=true", metrics.calls.Load(), metrics.dials.Load(), metrics.dialFailures.Load(), metrics.requestFailures.Load(), metrics.decodeFailures.Load(), metrics.unexpectedStatuses.Load(), metrics.cancellations.Load(), metrics.shutdown503.Load(), metrics.shutdown499.Load())
}
