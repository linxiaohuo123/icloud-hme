package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/client"

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

// TestOptimized_ServerSideSocketCancellation 通过生产连接池 mail.Pool.DoContext 验证真实 Context 取消链路与套接字熔断
func TestOptimized_ServerSideSocketCancellation(t *testing.T) {
	// 1. 本地 Mock IMAP 服务端，完全本地闭环，严禁连接外部真实 Apple 资产
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()

	serverConnClosed := make(chan struct{})
	serverReceivedCmds := make(chan string, 10)

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		// 发送标准 IMAP 欢迎问候
		_, _ = conn.Write([]byte("* OK [CAPABILITY IMAP4rev1] Mock IMAP Server Ready\r\n"))

		reader := bufio.NewReader(conn)
		// 处理 ensure -> Ping 发送的初始 NOOP
		for {
			line, rerr := reader.ReadString('\n')
			if rerr != nil {
				close(serverConnClosed)
				return
			}
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				tag := fields[0]
				cmd := strings.ToUpper(fields[1])
				serverReceivedCmds <- cmd
				if cmd == "NOOP" {
					_, _ = conn.Write([]byte(tag + " OK NOOP completed\r\n"))
					break
				}
			}
		}

		// 处理业务回调中的命令：读取后故意不应答，保持阻塞等待客户端被取消关闭
		for {
			line, rerr := reader.ReadString('\n')
			if rerr != nil {
				close(serverConnClosed)
				return
			}
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				serverReceivedCmds <- strings.ToUpper(fields[1])
			}
		}
	}()

	clientConn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}

	imapCli, err := client.New(clientConn)
	if err != nil {
		t.Fatalf("imap client.New failed: %v", err)
	}

	mockClient := mail.NewClientForTesting("perf_cancel@icloud.com", "dummy_pass", clientConn, imapCli)

	p := mail.NewPool()
	defer p.Close()

	p.SetClientForTesting("perf_cancel@icloud.com", "dummy_pass", mockClient)
	p.SetLastUsedForTesting("perf_cancel@icloud.com", time.Now().Add(-1*time.Minute))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	callbackEntered := make(chan struct{})
	var doErr error
	var cancelDuration time.Duration

	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		doErr = p.DoContext(ctx, "perf_cancel@icloud.com", "dummy_pass", "", func(cli *mail.Client) error {
			close(callbackEntered)
			// 阻塞在实际网络读取：发送 NOOP 并等待服务端响应 (服务端挂起不应答)
			return cli.Ping()
		})
	}()

	// 确认业务回调已经进入
	select {
	case <-callbackEntered:
	case <-time.After(2 * time.Second):
		t.Fatalf("业务回调未能在限时内进入")
	}

	// 确认服务端已收到 ensure NOOP
	select {
	case cmd := <-serverReceivedCmds:
		if cmd != "NOOP" {
			t.Fatalf("服务端收到未知前置命令: %s", cmd)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("服务端未收到 initial ensure NOOP")
	}

	// 确认业务回调内部发起的第二条 NOOP 已到达服务端并阻塞在网络读取
	select {
	case cmd := <-serverReceivedCmds:
		if cmd != "NOOP" {
			t.Fatalf("服务端收到非预期命令: %s", cmd)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("业务回调内部发起的 NOOP 命令未到达服务端")
	}

	// 断言：取消前连接保持打开
	select {
	case <-serverConnClosed:
		t.Fatalf("连接在取消前已被提前关闭")
	default:
	}

	// 由测试取消请求 Context，开始高精度计时
	cancelStart := time.Now()
	cancel()

	select {
	case <-doneCh:
		cancelDuration = time.Since(cancelStart)
		t.Logf("[OPTIMIZED] Context cancel 到读取退出实际测量耗时: %v", cancelDuration)
	case <-time.After(2 * time.Second):
		t.Fatalf("DoContext 未能在 Context 取消后及时退出")
	}

	// 断言 1: 取消错误类型必须为 context.Canceled，拒绝任意拨号/认证/语法错误
	if !errors.Is(doErr, context.Canceled) {
		t.Fatalf("期望错误为 context.Canceled，实际得到: %v", doErr)
	}

	// 断言 2: 取消后底层套接字被强制掐断，服务端感知到连接断开
	select {
	case <-serverConnClosed:
		t.Logf("[OPTIMIZED] Server confirmed socket was severed by production Pool.DoContext watchdog")
	case <-time.After(2 * time.Second):
		t.Fatalf("服务端未能在 2s 内检测到套接字关闭，连接发生泄漏")
	}

	// 断言 3: 取消后已中断的 client 必须从连接池中丢弃
	if cli := p.ClientForTesting("perf_cancel@icloud.com"); cli != nil {
		t.Fatalf("取消后 client 应当从连接池中丢弃，但依然存在")
	}

	// 断言 4: 账号槽位信号量已释放，下一次请求可正常执行
	if !p.TryLockForTesting("perf_cancel@icloud.com") {
		t.Fatalf("取消后单账号槽位信号量未释放，槽位发生泄漏死锁")
	}
	p.UnlockForTesting("perf_cancel@icloud.com")
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
}

// TestMailReadService_InvalidateAccount_CheckCommitRace 验证在检查与提交边界发生 InvalidateAccount 竞争时旧值绝无法落库
func TestMailReadService_InvalidateAccount_CheckCommitRace(t *testing.T) {
	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_race_acc", Name: "Race Acc"}},
	}
	svc := NewMailReadService(fb)

	// 1. 目录缓存提交与 InvalidateAccount 竞争
	startGen := svc.getAccountGen("acc_race_acc")
	// 设置钩子：在准备检查世代并提交目录缓存的瞬间，并发触发 InvalidateAccount
	hookFired := false
	svc.SetBeforeCommitHookForTesting(func() {
		hookFired = true
		svc.InvalidateAccount("acc_race_acc")
	})

	committed := svc.commitMailboxCache("acc_race_acc", startGen, []mail.Folder{{Name: "STALE_DIR"}})
	if !hookFired {
		t.Fatalf("beforeCommitHook 未能触发")
	}
	if committed {
		t.Fatalf("在失效竞争后，commitMailboxCache 应当被原子屏障拒绝返回 false")
	}

	// 核心断言：失效完成时，旧目录绝对无法在缓存中命中
	svc.mailboxMu.RLock()
	_, hitMb := svc.mailboxCache["acc_race_acc"]
	svc.mailboxMu.RUnlock()
	if hitMb {
		t.Fatalf("失效完成时，旧目录仍然落库命中 mailboxCache！存在原子性漏洞")
	}

	// 2. 详情缓存单条提交与 InvalidateAccount 竞争
	svc.SetBeforeCommitHookForTesting(nil) // 重置
	startGen2 := svc.getAccountGen("acc_race_acc")
	targetRef := mail.MessageRef{Provider: "imap", AccountID: "acc_race_acc", Mailbox: "INBOX", UID: 888}

	svc.SetBeforeCommitHookForTesting(func() {
		svc.InvalidateAccount("acc_race_acc")
	})

	committedMsg := svc.commitMessageCache(targetRef.CacheKey(), "acc_race_acc", startGen2, &mail.FullMessage{
		Message: mail.Message{ID: "stale_detail", MessageRef: targetRef.Encode()},
		Body:    "STALE_DETAIL_BODY",
	}, "imap", "imap")

	if committedMsg {
		t.Fatalf("在失效竞争后，commitMessageCache 应当被原子屏障拒绝返回 false")
	}

	svc.cacheMu.RLock()
	_, hitCache := svc.cache[targetRef.CacheKey()]
	svc.cacheMu.RUnlock()
	if hitCache {
		t.Fatalf("失效完成时，旧邮件详情仍然落库命中 cache！存在原子性漏洞")
	}

	// 3. 详情缓存批量提交与 InvalidateAccount 竞争
	svc.SetBeforeCommitHookForTesting(nil)
	startGen3 := svc.getAccountGen("acc_race_acc")
	targetRefBatch := mail.MessageRef{Provider: "imap", AccountID: "acc_race_acc", Mailbox: "INBOX", UID: 777}

	svc.SetBeforeCommitHookForTesting(func() {
		svc.InvalidateAccount("acc_race_acc")
	})

	committedBatch := svc.commitMessagesBatch("acc_race_acc", startGen3, []batchCommitItem{
		{
			Key: targetRefBatch.CacheKey(),
			Msg: &mail.FullMessage{
				Message: mail.Message{ID: "stale_batch", MessageRef: targetRefBatch.Encode()},
				Body:    "STALE_BATCH_BODY",
			},
			Provider: "imap",
			Method:   "imap",
		},
	})

	if committedBatch {
		t.Fatalf("在失效竞争后，commitMessagesBatch 应当被原子屏障拒绝返回 false")
	}

	svc.cacheMu.RLock()
	_, hitBatchCache := svc.cache[targetRefBatch.CacheKey()]
	svc.cacheMu.RUnlock()
	if hitBatchCache {
		t.Fatalf("失效完成时，旧批量详情仍然落库命中 cache！存在原子性漏洞")
	}
}

// TestMailReadService_InvalidateAll_CheckCommitRace 验证在检查与提交边界发生全局 InvalidateAll 竞争时旧值绝无法落库
func TestMailReadService_InvalidateAll_CheckCommitRace(t *testing.T) {
	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_race_all", Name: "Race All Acc"}},
	}
	svc := NewMailReadService(fb)

	// 1. 目录缓存与 InvalidateAll 竞争
	startGen := svc.getAccountGen("acc_race_all")
	svc.SetBeforeCommitHookForTesting(func() {
		svc.InvalidateAll()
	})

	committedMb := svc.commitMailboxCache("acc_race_all", startGen, []mail.Folder{{Name: "STALE_GLOBAL_DIR"}})
	if committedMb {
		t.Fatalf("在全局失效竞争后，commitMailboxCache 应当返回 false")
	}

	svc.mailboxMu.RLock()
	_, hitMb := svc.mailboxCache["acc_race_all"]
	svc.mailboxMu.RUnlock()
	if hitMb {
		t.Fatalf("全局失效后旧目录依然落库！")
	}

	// 2. 详情缓存与 InvalidateAll 竞争
	svc.SetBeforeCommitHookForTesting(nil)
	startGen2 := svc.getAccountGen("acc_race_all")
	targetRef := mail.MessageRef{Provider: "imap", AccountID: "acc_race_all", Mailbox: "INBOX", UID: 666}

	svc.SetBeforeCommitHookForTesting(func() {
		svc.InvalidateAll()
	})

	committedMsg := svc.commitMessageCache(targetRef.CacheKey(), "acc_race_all", startGen2, &mail.FullMessage{
		Message: mail.Message{ID: "stale_global_msg", MessageRef: targetRef.Encode()},
		Body:    "STALE_GLOBAL_BODY",
	}, "imap", "imap")

	if committedMsg {
		t.Fatalf("在全局失效竞争后，commitMessageCache 应当返回 false")
	}

	svc.cacheMu.RLock()
	_, hitCache := svc.cache[targetRef.CacheKey()]
	svc.cacheMu.RUnlock()
	if hitCache {
		t.Fatalf("全局失效后旧详情依然落库！")
	}
}

// TestMailReadService_ConcurrentCommitAndInvalidateRace 高并发下测试世代原子屏障与失效互斥正确性
func TestMailReadService_ConcurrentCommitAndInvalidateRace(t *testing.T) {
	fb := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_conc_1", Name: "Concurrent Acc 1"},
			{ID: "acc_conc_2", Name: "Concurrent Acc 2"},
		},
	}
	svc := NewMailReadService(fb)

	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// 10 个 goroutine 持续尝试提交
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			accountID := "acc_conc_1"
			if workerID%2 == 1 {
				accountID = "acc_conc_2"
			}
			uid := uint32(1000 + workerID)
			ref := mail.MessageRef{Provider: "imap", AccountID: accountID, Mailbox: "INBOX", UID: uid}

			for {
				select {
				case <-ctx.Done():
					return
				default:
					gen := svc.getAccountGen(accountID)
					// 模拟网络耗时
					time.Sleep(time.Duration(workerID%3) * time.Millisecond)

					svc.commitMailboxCache(accountID, gen, []mail.Folder{{Name: "INBOX"}})
					svc.commitMessageCache(ref.CacheKey(), accountID, gen, &mail.FullMessage{
						Message: mail.Message{ID: "msg", MessageRef: ref.Encode()},
						Body:    "body",
					}, "imap", "imap")
				}
			}
		}(i)
	}

	// 4 个 goroutine 持续发起 InvalidateAccount 与 InvalidateAll
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					time.Sleep(2 * time.Millisecond)
					if workerID%2 == 0 {
						svc.InvalidateAccount("acc_conc_1")
					} else {
						svc.InvalidateAll()
					}
				}
			}
		}(i)
	}

	wg.Wait()
}



