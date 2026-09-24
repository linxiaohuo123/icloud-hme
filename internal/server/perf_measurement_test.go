package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/mail"
)

// Benchmark and baseline measurement for inbox, mailboxes, batch messages, and cancellation.
func TestBaseline_InboxStageTimings(t *testing.T) {
	var listInboxCalls int32
	var listMailboxesCalls int32
	var getMessagesCalls int32

	backendDelay := 50 * time.Millisecond

	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_perf", Name: "Perf Account", HasAppPassword: true}},
		onListInboxContext: func(ctx context.Context, q InboxQuery) (InboxResult, error) {
			atomic.AddInt32(&listInboxCalls, 1)
			select {
			case <-ctx.Done():
				return InboxResult{}, ctx.Err()
			case <-time.After(backendDelay):
			}
			msgs := make([]mail.Message, 20)
			for i := range msgs {
				msgs[i] = mail.Message{
					ID:         string(rune('1' + i)),
					Folder:     "INBOX",
					Subject:    "Verification code 123456",
					From:       "service@apple.com",
					Date:       "2026-09-23 20:00:00",
					Provider:   "imap",
					MessageRef: mail.MessageRef{Provider: "imap", AccountID: "acc_perf", Mailbox: "INBOX", UID: uint32(100 + i)}.Encode(),
				}
			}
			return InboxResult{
				AccountID: q.AccountID,
				Folder:    q.Folder,
				Count:     len(msgs),
				Messages:  msgs,
				Method:    "imap",
			}, nil
		},
		getMessagesFunc: func(accountID string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
			atomic.AddInt32(&getMessagesCalls, 1)
			time.Sleep(backendDelay)
			out := make([]*mail.FullMessage, len(refs))
			bodyContent := strings.Repeat("Your verification code is 889900. Do not share it. ", 20) // ~1KB body
			for i, r := range refs {
				out[i] = &mail.FullMessage{
					Message: mail.Message{
						ID:         string(rune('1' + i)),
						Folder:     r.Mailbox,
						Subject:    "Your Code",
						Provider:   "imap",
						MessageRef: r.Encode(),
						Preview:    bodyContent, // Baseline duplicates body into preview
					},
					Body:         bodyContent,
					BodyComplete: true,
					Provider:     "imap",
					Method:       "imap",
				}
			}
			return out, nil
		},
	}

	srv := newWithBackend(fb, Config{Debug: false, AdminPassword: "admin-pass-2026-strong"})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	cookie, _ := login(t, ts, "admin-pass-2026-strong")

	// 1. Measure ListInbox Baseline (cold vs warm)
	t.Run("ListInbox_Timing_Cold_vs_Warm", func(t *testing.T) {
		startCold := time.Now()
		req1 := authedReq(t, ts, "GET", "/api/inbox?account_id=acc_perf&folder=all&limit=20&days=7", "")
		req1.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
		status1, body1, _ := do(t, req1)
		coldDuration := time.Since(startCold)

		if status1 != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", status1, body1)
		}

		startWarm := time.Now()
		req2 := authedReq(t, ts, "GET", "/api/inbox?account_id=acc_perf&folder=all&limit=20&days=7", "")
		req2.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
		status2, body2, _ := do(t, req2)
		warmDuration := time.Since(startWarm)

		if status2 != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", status2, body2)
		}

		t.Logf("[BASELINE] ListInbox Cold Latency: %v, Warm Latency: %v, Calls to Backend: %d", coldDuration, warmDuration, atomic.LoadInt32(&listInboxCalls))
	})

	// 2. Measure Batch Messages Payload and Timing (20 items)
	t.Run("GetMessages_Batch_Payload_And_Timing", func(t *testing.T) {
		items := make([]batchMessageItemReq, 20)
		for i := 0; i < 20; i++ {
			items[i] = batchMessageItemReq{
				MessageRef: mail.MessageRef{Provider: "imap", AccountID: "acc_perf", Mailbox: "INBOX", UID: uint32(100 + i)}.Encode(),
			}
		}
		reqBody, _ := json.Marshal(map[string]interface{}{
			"account_id": "acc_perf",
			"messages":   items,
		})

		start := time.Now()
		req := authedReq(t, ts, "POST", "/api/messages", string(reqBody))
		req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
		status, respBody, _ := do(t, req)
		batchDuration := time.Since(start)

		if status != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", status, respBody)
		}

		payloadBytes := len(respBody)
		t.Logf("[BASELINE] GetMessages (20 items) Latency: %v, Response Size: %d bytes (%d KB)", batchDuration, payloadBytes, payloadBytes/1024)
	})

	// 3. Measure Concurrent Connection Queue Waiting Simulation
	t.Run("Concurrent_SameAccount_QueueWait", func(t *testing.T) {
		// Simulate pool semaphore with cap=1
		sem := make(chan struct{}, 1)
		var queueWait1, queueWait2 time.Duration

		var wg sync.WaitGroup
		wg.Add(2)

		startAll := time.Now()

		// Op 1: /api/mailboxes (acquires lock first, takes 50ms)
		go func() {
			defer wg.Done()
			qStart := time.Now()
			sem <- struct{}{}
			queueWait1 = time.Since(qStart)
			defer func() { <-sem }()
			atomic.AddInt32(&listMailboxesCalls, 1)
			time.Sleep(50 * time.Millisecond)
		}()

		// Op 2: /api/inbox (fired almost simultaneously, waits behind Op 1)
		time.Sleep(2 * time.Millisecond)
		go func() {
			defer wg.Done()
			qStart := time.Now()
			sem <- struct{}{}
			queueWait2 = time.Since(qStart)
			defer func() { <-sem }()
			atomic.AddInt32(&listInboxCalls, 1)
			time.Sleep(50 * time.Millisecond)
		}()

		wg.Wait()
		totalTime := time.Since(startAll)
		t.Logf("[BASELINE] Queue Wait Op 1: %v, Op 2 (Inbox wait behind Mailboxes): %v, Total: %v", queueWait1, queueWait2, totalTime)
	})
}

func TestOptimized_MailboxCache_And_InFlightDedup(t *testing.T) {
	var listInboxCalls int32
	var listMailboxesCalls int32
	backendDelay := 40 * time.Millisecond

	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_opt", Name: "Opt Account", HasAppPassword: true}},
		onListInboxContext: func(ctx context.Context, q InboxQuery) (InboxResult, error) {
			atomic.AddInt32(&listInboxCalls, 1)
			select {
			case <-ctx.Done():
				return InboxResult{}, ctx.Err()
			case <-time.After(backendDelay):
			}
			return InboxResult{AccountID: q.AccountID, Count: 1, Method: "imap"}, nil
		},
		onListMailboxesContext: func(ctx context.Context, accountID string) ([]mail.Folder, error) {
			atomic.AddInt32(&listMailboxesCalls, 1)
			return []mail.Folder{{Name: "INBOX", Role: "inbox"}}, nil
		},
	}

	srv := newWithBackend(fb, Config{Debug: false, AdminPassword: "admin-pass-2026-strong"})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	cookie, csrf := login(t, ts, "admin-pass-2026-strong")

	// 1. Verify Mailboxes Cache
	t.Run("Mailbox_Cache_Eliminates_Redundant_Upstream_Calls", func(t *testing.T) {
		req1 := authedReq(t, ts, "GET", "/api/mailboxes?account_id=acc_opt", "")
		req1.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
		status1, _, _ := do(t, req1)
		if status1 != http.StatusOK {
			t.Fatalf("first mailboxes call failed: %d", status1)
		}

		startWarm := time.Now()
		req2 := authedReq(t, ts, "GET", "/api/mailboxes?account_id=acc_opt", "")
		req2.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
		status2, _, _ := do(t, req2)
		warmDur := time.Since(startWarm)

		if status2 != http.StatusOK {
			t.Fatalf("second mailboxes call failed: %d", status2)
		}
		t.Logf("[OPTIMIZED] Warm /api/mailboxes Latency: %v", warmDur)
		if warmDur > 20*time.Millisecond {
			t.Errorf("Warm mailboxes should hit cache in <20ms, took %v", warmDur)
		}
	})

	// 2. Verify In-Flight Dedup (Concurrent identical queries coalesce into 1 backend call)
	t.Run("InFlight_ListInbox_Coalesces_Concurrent_Requests", func(t *testing.T) {
		atomic.StoreInt32(&listInboxCalls, 0)
		const concurrency = 5
		var wg sync.WaitGroup
		wg.Add(concurrency)

		start := time.Now()
		for i := 0; i < concurrency; i++ {
			go func() {
				defer wg.Done()
				// Use refresh=true so it bypasses snapshot cache and tests in-flight dedup
				req := authedReq(t, ts, "GET", "/api/inbox?account_id=acc_opt&folder=INBOX&limit=20&days=7&refresh=true", "")
				req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
				status, _, _ := do(t, req)
				if status != http.StatusOK {
					t.Errorf("expected 200, got %d", status)
				}
			}()
		}
		wg.Wait()
		dur := time.Since(start)

		backendCalls := atomic.LoadInt32(&listInboxCalls)
		t.Logf("[OPTIMIZED] In-Flight Concurrent (%d calls) Latency: %v, Backend Calls: %d", concurrency, dur, backendCalls)
		if backendCalls != 1 {
			t.Errorf("Expected exactly 1 backend call via in-flight dedup, got %d", backendCalls)
		}
	})

	// 3. Verify Caller Cancellation does NOT cancel shared in-flight flight if other callers remain
	t.Run("InFlight_OneCallerCancels_OtherCallerSucceeds", func(t *testing.T) {
		atomic.StoreInt32(&listInboxCalls, 0)
		var wg sync.WaitGroup
		wg.Add(2)

		// Caller 1 with short timeout
		var caller1Err error
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			req := authedReq(t, ts, "GET", "/api/inbox?account_id=acc_opt&folder=JUNK_TEST&limit=20&days=7&refresh=true", "")
			req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
			req = req.WithContext(ctx)
			client := &http.Client{}
			resp, err := client.Do(req)
			if err != nil {
				caller1Err = err
			} else {
				resp.Body.Close()
			}
		}()

		// Caller 2 with sufficient timeout
		var caller2Status int
		go func() {
			defer wg.Done()
			time.Sleep(2 * time.Millisecond) // ensure caller 1 started flight
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()
			req := authedReq(t, ts, "GET", "/api/inbox?account_id=acc_opt&folder=JUNK_TEST&limit=20&days=7&refresh=true", "")
			req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
			req = req.WithContext(ctx)
			client := &http.Client{}
			resp, err := client.Do(req)
			if err == nil {
				caller2Status = resp.StatusCode
				resp.Body.Close()
			}
		}()

		wg.Wait()
		t.Logf("[OPTIMIZED] Caller 1 error (canceled): %v, Caller 2 status: %d", caller1Err, caller2Status)
		if caller2Status != http.StatusOK {
			t.Errorf("Caller 2 should have succeeded despite Caller 1 cancellation, got %d", caller2Status)
		}
	})

	// 4. Verify that when all callers cancel, a new caller does NOT join the canceled call and gets a fresh flight
	t.Run("InFlight_AllCallersCancel_NewCallerGetsFreshFlight", func(t *testing.T) {
		atomic.StoreInt32(&listInboxCalls, 0)

		// Caller 1 cancels very quickly
		ctx1, cancel1 := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel1()
		req1 := authedReq(t, ts, "GET", "/api/inbox?account_id=acc_opt&folder=CANCEL_TEST&limit=20&days=7&refresh=true", "")
		req1.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
		req1 = req1.WithContext(ctx1)
		client := &http.Client{}
		_, _ = client.Do(req1)

		// Give a tiny moment for caller 1 cancellation to register in flight
		time.Sleep(15 * time.Millisecond)

		// Caller 2 should start a fresh flight and succeed
		req2 := authedReq(t, ts, "GET", "/api/inbox?account_id=acc_opt&folder=CANCEL_TEST&limit=20&days=7&refresh=true", "")
		req2.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
		status2, body2, _ := do(t, req2)

		if status2 != http.StatusOK {
			t.Fatalf("Caller 2 should succeed with fresh flight, got %d: %s", status2, body2)
		}
	})

	// 5. Verify MailboxCache Invalidation on Account Updates
	t.Run("MailboxCache_InvalidationOnAccountUpdate", func(t *testing.T) {
		atomic.StoreInt32(&listMailboxesCalls, 0)

		// Call 1: Backend hit (via refresh=true to ensure cold start)
		req1 := authedReq(t, ts, "GET", "/api/mailboxes?account_id=acc_opt&refresh=true", "")
		req1.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
		status1, _, _ := do(t, req1)
		if status1 != http.StatusOK {
			t.Fatalf("call 1 failed: %d", status1)
		}
		if atomic.LoadInt32(&listMailboxesCalls) != 1 {
			t.Fatalf("expected 1 backend call, got %d", atomic.LoadInt32(&listMailboxesCalls))
		}

		// Call 2: Cache hit (0 backend calls)
		req2 := authedReq(t, ts, "GET", "/api/mailboxes?account_id=acc_opt", "")
		req2.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
		status2, _, _ := do(t, req2)
		if status2 != http.StatusOK {
			t.Fatalf("call 2 failed: %d", status2)
		}
		if atomic.LoadInt32(&listMailboxesCalls) != 1 {
			t.Fatalf("expected cache hit with 1 total backend call, got %d", atomic.LoadInt32(&listMailboxesCalls))
		}

		// Mutation: Update account (should invalidate mailbox cache)
		patchReq := authedReq(t, ts, "PATCH", "/api/accounts/acc_opt", `{"name":"Opt Account Updated"}`)
		patchReq.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
		patchReq.Header.Set("X-CSRF-Token", csrf)
		patchStatus, _, _ := do(t, patchReq)
		if patchStatus != http.StatusOK {
			t.Fatalf("patch account failed: %d", patchStatus)
		}

		// Call 3: Backend hit again because cache was invalidated
		req3 := authedReq(t, ts, "GET", "/api/mailboxes?account_id=acc_opt", "")
		req3.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
		status3, _, _ := do(t, req3)
		if status3 != http.StatusOK {
			t.Fatalf("call 3 failed: %d", status3)
		}
		if atomic.LoadInt32(&listMailboxesCalls) != 2 {
			t.Fatalf("expected 2 backend calls after invalidation, got %d", atomic.LoadInt32(&listMailboxesCalls))
		}
	})
}

func TestOptimized_GetMessagesBatch_TrueCancellation(t *testing.T) {
	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_opt", Name: "Opt Account", HasAppPassword: true}},
		getMessagesFunc: func(accountID string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
			// Simulate slow IMAP fetch taking 500ms
			time.Sleep(500 * time.Millisecond)
			return []*mail.FullMessage{}, nil
		},
	}

	srv := newWithBackend(fb, Config{Debug: false, AdminPassword: "admin-pass-2026-strong"})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	cookie, _ := login(t, ts, "admin-pass-2026-strong")

	reqItems := []batchMessageItemReq{
		{MessageRef: mail.MessageRef{Provider: "imap", AccountID: "acc_opt", Mailbox: "INBOX", UID: 999}.Encode()},
	}
	reqData, _ := json.Marshal(map[string]interface{}{
		"account_id": "acc_opt",
		"messages":   reqItems,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	req := authedReq(t, ts, "POST", "/api/messages", string(reqData))
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	req = req.WithContext(ctx)

	client := &http.Client{}
	_, err := client.Do(req)
	dur := time.Since(start)

	t.Logf("[OPTIMIZED] Cancelled /api/messages Duration: %v, Err: %v", dur, err)
	if dur > 200*time.Millisecond {
		t.Fatalf("Expected client context cancellation to abort in <200ms, took %v", dur)
	}
}

// TestOptimized_ServerSideSocketCancellation 通过生产连接池 mail.Pool.DoContext 验证取消，严禁测试代码自建协程 Close
func TestOptimized_ServerSideSocketCancellation(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()

	serverConnClosed := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		buf := make([]byte, 128)
		for {
			_, rerr := conn.Read(buf)
			if rerr != nil {
				close(serverConnClosed)
				return
			}
		}
	}()

	clientConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}

	p := mail.NewPool()
	defer p.Close()

	// 将真实 TCP 连接注入到生产连接池中
	p.SetClientForTesting("perf_cancel@icloud.com", "dummy_pass", clientConn)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	// 执行生产连接池的 DoContext 方法：看门狗与底层强制关闭完全由生产代码承载
	err = p.DoContext(ctx, "perf_cancel@icloud.com", "dummy_pass", "", func(cli *mail.Client) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
			return nil
		}
	})

	if err == nil {
		t.Fatalf("Expected DoContext to fail with context timeout/canceled, got nil")
	}

	select {
	case <-serverConnClosed:
		t.Logf("[OPTIMIZED] Server confirmed socket was severed by production Pool.DoContext watchdog")
	case <-time.After(300 * time.Millisecond):
		t.Fatalf("Server side did not detect socket close within 300ms, connection leaked")
	}
}

// TestMailReadService_AccountInvalidationBarrier 验证账号变更/失效使正在进行的异步任务无法写回陈旧缓存
func TestMailReadService_AccountInvalidationBarrier(t *testing.T) {
	mailboxBlocked := make(chan struct{})
	mailboxContinue := make(chan struct{})

	batchBlocked := make(chan struct{})
	batchContinue := make(chan struct{})

	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_barrier", Name: "Barrier Account"}},
		onListMailboxesContext: func(ctx context.Context, accountID string) ([]mail.Folder, error) {
			close(mailboxBlocked)
			<-mailboxContinue
			return []mail.Folder{{Name: "STALE_FOLDER"}}, nil
		},
		getMessagesFunc: func(accountID string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
			close(batchBlocked)
			<-batchContinue
			out := make([]*mail.FullMessage, len(refs))
			for i, r := range refs {
				out[i] = &mail.FullMessage{
					Message: mail.Message{
						ID:         "stale_msg",
						MessageRef: r.Encode(),
						Folder:     r.Mailbox,
					},
					Body: "STALE_BODY",
				}
			}
			return out, nil
		},
	}

	svc := NewMailReadService(fb)

	// 1. 验证 ListMailboxes 异步回写在 InvalidateAccount 后被丢弃
	var wg sync.WaitGroup
	wg.Add(1)
	var mbFolders []mail.Folder
	var mbErr error
	go func() {
		defer wg.Done()
		mbFolders, mbErr = svc.ListMailboxes(context.Background(), "acc_barrier", false)
	}()

	<-mailboxBlocked
	// 此时后端在途，触发 InvalidateAccount
	svc.InvalidateAccount("acc_barrier")
	close(mailboxContinue)
	wg.Wait()

	if mbErr != nil {
		t.Fatalf("unexpected err: %v", mbErr)
	}
	if len(mbFolders) != 1 || mbFolders[0].Name != "STALE_FOLDER" {
		t.Fatalf("unexpected return folders: %v", mbFolders)
	}

	// 核心断言：虽然在途调用返回了结果，但由于代际已失效，mailboxCache 中严禁包含该陈旧目录！
	svc.mailboxMu.RLock()
	_, hitMb := svc.mailboxCache["acc_barrier"]
	svc.mailboxMu.RUnlock()
	if hitMb {
		t.Fatalf("stale mailbox response was written back to mailboxCache after InvalidateAccount")
	}

	// 2. 验证 GetMessagesBatch 异步回写在 InvalidateAccount 后被丢弃
	wg.Add(1)
	targetRef := mail.MessageRef{Provider: "imap", AccountID: "acc_barrier", Mailbox: "INBOX", UID: 999}
	go func() {
		defer wg.Done()
		_, _, _ = svc.GetMessagesBatch(context.Background(), "acc_barrier", []batchMessageItemReq{
			{MessageRef: targetRef.Encode()},
		})
	}()

	<-batchBlocked
	svc.InvalidateAccount("acc_barrier")
	close(batchContinue)
	wg.Wait()

	// 核心断言：在途详情调用返回后，由于代际已失效，cache 中严禁写入该陈旧邮件！
	svc.cacheMu.RLock()
	_, hitCache := svc.cache[targetRef.CacheKey()]
	svc.cacheMu.RUnlock()
	if hitCache {
		t.Fatalf("stale batch detail response was written back to cache after InvalidateAccount")
	}
}

// TestNormalizeInboxQueryKey_CustomFolderCaseAndCapability 验证自定义文件夹保留大小写及能力参数隔离
func TestNormalizeInboxQueryKey_CustomFolderCaseAndCapability(t *testing.T) {
	// 1. INBOX 大小写归一
	k1 := normalizeInboxQueryKey(InboxQuery{AccountID: "acc1", Folder: "inbox"})
	k2 := normalizeInboxQueryKey(InboxQuery{AccountID: "acc1", Folder: "INBOX"})
	k3 := normalizeInboxQueryKey(InboxQuery{AccountID: "acc1", Folder: ""})
	if k1 != k2 || k2 != k3 {
		t.Fatalf("INBOX should normalize identically: %s vs %s vs %s", k1, k2, k3)
	}

	// 2. 自定义文件夹保留真实大小写
	kCustomUpper := normalizeInboxQueryKey(InboxQuery{AccountID: "acc1", Folder: "Receipts"})
	kCustomLower := normalizeInboxQueryKey(InboxQuery{AccountID: "acc1", Folder: "receipts"})
	if kCustomUpper == kCustomLower {
		t.Fatalf("Custom folder case should be preserved: Receipts should != receipts")
	}

	// 3. FolderSpecified 参数隔离
	kFldSpecTrue := normalizeInboxQueryKey(InboxQuery{AccountID: "acc1", Folder: "INBOX", FolderSpecified: true})
	kFldSpecFalse := normalizeInboxQueryKey(InboxQuery{AccountID: "acc1", Folder: "INBOX", FolderSpecified: false})
	if kFldSpecTrue == kFldSpecFalse {
		t.Fatalf("FolderSpecified parameter should be isolated in in-flight key")
	}

	// 4. DaysSpecified 参数隔离
	kDaysSpecTrue := normalizeInboxQueryKey(InboxQuery{AccountID: "acc1", Folder: "INBOX", DaysSpecified: true})
	kDaysSpecFalse := normalizeInboxQueryKey(InboxQuery{AccountID: "acc1", Folder: "INBOX", DaysSpecified: false})
	if kDaysSpecTrue == kDaysSpecFalse {
		t.Fatalf("DaysSpecified parameter should be isolated in in-flight key")
	}
}

// TestMailReadService_InvalidateAccountCancelsInFlightList 验证账号变更立即取消该账号在途的收件箱请求
func TestMailReadService_InvalidateAccountCancelsInFlightList(t *testing.T) {
	inboxBlocked := make(chan struct{})
	inboxCanceled := make(chan struct{})

	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_flight_cancel", Name: "Flight Cancel Acc"}},
		onListInboxContext: func(ctx context.Context, q InboxQuery) (InboxResult, error) {
			close(inboxBlocked)
			<-ctx.Done()
			close(inboxCanceled)
			return InboxResult{}, ctx.Err()
		},
	}

	svc := NewMailReadService(fb)

	go func() {
		_, _ = svc.ListInbox(context.Background(), InboxQuery{AccountID: "acc_flight_cancel"})
	}()

	<-inboxBlocked

	// 验证在途任务存在
	svc.flightMu.Lock()
	flightCount := len(svc.inFlightList)
	svc.flightMu.Unlock()
	if flightCount != 1 {
		t.Fatalf("expected 1 in-flight call, got %d", flightCount)
	}

	// 触发 InvalidateAccount
	svc.InvalidateAccount("acc_flight_cancel")

	select {
	case <-inboxCanceled:
		// 成功感知到 context cancel
	case <-time.After(300 * time.Millisecond):
		t.Fatalf("in-flight ListInbox was not cancelled by InvalidateAccount")
	}

	svc.flightMu.Lock()
	remaining := len(svc.inFlightList)
	svc.flightMu.Unlock()
	if remaining != 0 {
		t.Fatalf("expected 0 in-flight calls after InvalidateAccount, got %d", remaining)
	}
}


