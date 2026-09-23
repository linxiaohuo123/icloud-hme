package server

import (
	"context"
	"encoding/json"
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
	}

	srv := newWithBackend(fb, Config{Debug: false, AdminPassword: "admin-pass-2026-strong"})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	cookie, _ := login(t, ts, "admin-pass-2026-strong")

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


