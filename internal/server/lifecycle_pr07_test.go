/**
 * [INPUT]: 依赖 testing, net/http, net/http/httptest, bytes, context, time, sync/atomic, icloud-hme/internal/auth, icloud-hme/internal/mail, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 PR-07 性能、有界并发、资源隔离与生命周期优雅停机测试套件 (P01~P06)
 * [POS]: internal/server 的 PR-07 自动化验证测试，覆盖稳态0远端调用、同账号增量批次拉取、慢账号隔离、队列与令牌上限、取消释放资源与停机顺序
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

// P01: pool_only 稳态请求上游调用次数必须为 0 (PR-07 §10.1 & §10.5)
func TestPR07_P01_PoolOnlyZeroUpstreamCalls(t *testing.T) {
	tempDir := t.TempDir()
	st, err := store.NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	// 预置库存
	_ = st.AddInventoryAlias("acc_local_1", hme.Alias{Email: "pool_perf@icloud.com", Active: true}, "replenish", true)

	tok := store.APIToken{
		ID:     "tok_perf_test",
		Name:   "PerfToken",
		Token:  "sec_perf_token",
		Scopes: "allocate,verify",
	}
	_ = st.SaveToken(tok)

	var upstreamCalls atomic.Int64
	fb := &fakeBackend{
		onCreateAlias: func(accountID, label string) (*hme.CreateResult, error) {
			upstreamCalls.Add(1)
			return &hme.CreateResult{Email: "remote@icloud.com"}, nil
		},
		onListAliases: func(accountID string) ([]hme.Alias, error) {
			upstreamCalls.Add(1)
			return nil, nil
		},
	}

	_, ts := newTestServerWithStore(fb, st)
	defer ts.Close()

	// 发起 10 次 pool_only 分配
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", strings.NewReader(`{"tag":"default"}`))
		req.Header.Set("Authorization", "Bearer sec_perf_token")
		req.Header.Set("Idempotency-Key", fmt.Sprintf("idemp_perf_%d", i))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		resp.Body.Close()
	}

	// 稳态请求上游远程调用次数必须严格为 0！
	if calls := upstreamCalls.Load(); calls != 0 {
		t.Fatalf("P01 failed: pool_only must have 0 upstream calls, got %d", calls)
	}
}

// P02: 同一账号多别名增量批量拉取，只调用 1 次 ListInbox 而非 N 次 (PR-07 §10.2 & §10.5)
func TestPR07_P02_BatchInboxSyncReducesRedundantScans(t *testing.T) {
	tempDir := t.TempDir()
	st, err := store.NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	eb := mail.NewEventBus(5 * time.Minute)

	var listInboxCalls atomic.Int64
	fb := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_shared", Name: "SharedAcc"},
		},
		onListInbox: func(q InboxQuery) (InboxResult, error) {
			listInboxCalls.Add(1)
			return InboxResult{
				Messages: []mail.Message{
					{
						ID:          "1:100",
						Folder:      "INBOX",
						UID:         100,
						UIDValidity: 1,
						Subject:     "Your Code: 888999",
						To:          "alias1@icloud.com",
					},
					{
						ID:          "1:101",
						Folder:      "INBOX",
						UID:         101,
						UIDValidity: 1,
						Subject:     "Your Code: 111222",
						To:          "alias2@icloud.com",
					},
				},
			}, nil
		},
	}

	worker := NewMailSyncWorker(fb, st, eb, 50*time.Millisecond)

	// 登记归属
	worker.RegisterAliasAccount("alias1@icloud.com", "acc_shared")
	worker.RegisterAliasAccount("alias2@icloud.com", "acc_shared")
	worker.RegisterAliasAccount("alias3@icloud.com", "acc_shared")

	// 三个别名同时在等待事件
	_, ch1 := eb.SubscribeWithBoundary("alias1@icloud.com", 1, 50)
	defer eb.Unsubscribe("alias1@icloud.com", 1)
	_, ch2 := eb.SubscribeWithBoundary("alias2@icloud.com", 1, 50)
	defer eb.Unsubscribe("alias2@icloud.com", 2)
	_, _ = eb.SubscribeWithBoundary("alias3@icloud.com", 1, 50)
	defer eb.Unsubscribe("alias3@icloud.com", 3)

	// 手动触发单轮同步
	worker.syncOnce()

	// 验证同账号3个别名只调用了 1 次 ListInbox
	if calls := listInboxCalls.Load(); calls != 1 {
		t.Fatalf("P02 failed: expected exactly 1 batch ListInbox call for shared account, got %d", calls)
	}

	// 确认邮件正确分发给各自等待者
	select {
	case ev1 := <-ch1:
		if ev1.OTP.Code != "888999" {
			t.Fatalf("alias1 OTP mismatch, expected 888999, got %s", ev1.OTP.Code)
		}
	default:
		t.Fatalf("alias1 failed to receive event")
	}

	select {
	case ev2 := <-ch2:
		if ev2.OTP.Code != "111222" {
			t.Fatalf("alias2 OTP mismatch, expected 111222, got %s", ev2.OTP.Code)
		}
	default:
		t.Fatalf("alias2 failed to receive event")
	}
}

// P03: 慢账号隔离测试：模拟账号 A 超时阻塞，不阻塞账号 B 的邮件同步与验证码分发 (PR-07 §10.2 & §10.5)
func TestPR07_P03_SlowAccountIsolation(t *testing.T) {
	tempDir := t.TempDir()
	st, err := store.NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	eb := mail.NewEventBus(5 * time.Minute)

	fb := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_slow", Name: "SlowAcc"},
			{ID: "acc_fast", Name: "FastAcc"},
		},
		onListInbox: func(q InboxQuery) (InboxResult, error) {
			if q.AccountID == "acc_slow" {
				// 模拟慢账号阻塞 2 秒 (大于测试设定的快速隔离时间或验证并发不被挡死)
				time.Sleep(1 * time.Second)
				return InboxResult{}, nil
			}
			if q.AccountID == "acc_fast" {
				return InboxResult{
					Messages: []mail.Message{
						{
							ID:          "1:200",
							Folder:      "INBOX",
							UID:         200,
							UIDValidity: 1,
							Subject:     "Your Code: 654321",
							To:          "fast@icloud.com",
						},
					},
				}, nil
			}
			return InboxResult{}, nil
		},
	}

	worker := NewMailSyncWorker(fb, st, eb, 50*time.Millisecond)
	worker.RegisterAliasAccount("slow@icloud.com", "acc_slow")
	worker.RegisterAliasAccount("fast@icloud.com", "acc_fast")

	_, _ = eb.SubscribeWithBoundary("slow@icloud.com", 1, 100)
	_, fastCh := eb.SubscribeWithBoundary("fast@icloud.com", 1, 100)

	start := time.Now()
	// 执行单轮同步
	doneCh := make(chan struct{})
	go func() {
		worker.syncOnce()
		close(doneCh)
	}()

	// 正常账号快速拿到事件
	select {
	case ev := <-fastCh:
		if ev.OTP.Code != "654321" {
			t.Fatalf("fast account OTP mismatch: %s", ev.OTP.Code)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("P03 failed: fast account was blocked by slow account!")
	}

	<-doneCh
	_ = start
}

// P04: 活跃任务上限与单 Token 上限保护 (429 / 503) (PR-07 §10.2)
func TestPR07_P04_VerificationQueueAndTokenLimits(t *testing.T) {
	tempDir := t.TempDir()
	st, err := store.NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	tok := store.APIToken{
		ID:     "tok_quota_test",
		Name:   "QuotaToken",
		Token:  "sec_quota",
		Scopes: "allocate,verify",
	}
	_ = st.SaveToken(tok)

	fb := &fakeBackend{}
	_, ts := newTestServerWithStore(fb, st)
	defer ts.Close()

	// 预先向数据库写入 50 条属于 tok_quota_test 的活跃任务，模拟达到单 Token 上限 (maxPerTokenActiveVerificationRequests = 50)
	now := time.Now().UTC()
	for i := 0; i < 50; i++ {
		_ = st.CreateVerificationRequest(context.Background(), &store.VerificationRequest{
			RequestID:     fmt.Sprintf("vreq_mock_%d", i),
			PrincipalKind: "token",
			PrincipalID:   tok.ID,
			LeaseID:       fmt.Sprintf("lease_%d", i),
			AliasEmail:    fmt.Sprintf("alias_%d@icloud.com", i),
			Status:        "ready",
			CreatedAt:     now.Format(time.RFC3339),
			ExpiresAt:     now.Add(10 * time.Minute).Format(time.RFC3339),
		})
	}

	// 此时第 51 个请求应当被 429 TOO_MANY_REQUESTS 拒绝
	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "limit_test@icloud.com", Active: true}, "replenish", true)
	alloc, _, _ := st.ClaimInventoryAlias(context.Background(), "token", tok.ID, "allocate", "k_quota_1", "h", "tag", "")

	body, _ := json.Marshal(map[string]string{"lease_id": alloc.AllocationID})
	req, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/verification-requests", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer sec_quota")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("P04 failed: expected 429 Too Many Requests, got %d", resp.StatusCode)
	}
}

// P05: 客户端断开连接时，Request Cancellation 释放订阅与资源 (PR-07 §10.2)
func TestPR07_P05_RequestCancellationReleasesResources(t *testing.T) {
	tempDir := t.TempDir()
	st, err := store.NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	tok := store.APIToken{
		ID:     "tok_cancel_test",
		Name:   "CancelToken",
		Token:  "sec_cancel",
		Scopes: "allocate,verify",
	}
	_ = st.SaveToken(tok)

	_ = st.AddInventoryAlias("acc_1", hme.Alias{Email: "cancel_target@icloud.com", Active: true}, "replenish", true)
	alloc, _, _ := st.ClaimInventoryAlias(context.Background(), "token", tok.ID, "allocate", "k_cancel", "h", "tag", "")

	fb := &fakeBackend{}
	s, ts := newTestServerWithStore(fb, st)
	defer ts.Close()

	// 1. 创建验证请求
	body, _ := json.Marshal(map[string]string{"lease_id": alloc.AllocationID})
	createReq, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/verification-requests", bytes.NewReader(body))
	createReq.Header.Set("Authorization", "Bearer sec_cancel")
	cResp, _ := http.DefaultClient.Do(createReq)
	cData := parseData(t, cResp)
	vreqID := cData["request_id"].(string)

	// 2. 发起一个带 5 秒超时的 GET 轮询，但客户端在 100ms 后主动断开 Context
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	pollReq, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/api/external/v2/verification-requests/%s?timeout=5", ts.URL, vreqID), nil)
	pollReq.Header.Set("Authorization", "Bearer sec_cancel")

	// 预期返回 context deadline exceeded
	_, _ = http.DefaultClient.Do(pollReq)

	// 等待 100ms 确保 defer s.eventBus.Unsubscribe 执行
	time.Sleep(100 * time.Millisecond)

	// 校验 EventBus 针对该邮箱的订阅者已被全部清理释放，无内存泄漏！
	if s.eventBus.HasSubscribers() {
		t.Fatalf("P05 failed: expected eventBus subscribers to be 0 after cancellation, got active subscribers")
	}
}

// P06: 优雅停机生命周期顺序与幂等性 (PR-07 §10.4)
func TestPR07_P06_GracefulShutdownSequenceAndIdempotence(t *testing.T) {
	tempDir := t.TempDir()
	st, err := store.NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	var backendClosed atomic.Bool
	fb := &fakeBackend{
		onClose: func() {
			backendClosed.Store(true)
		},
	}

	cfg := Config{
		DataDir:               tempDir,
		AdminPassword:         "testpass123",
		MailPollInterval:      100 * time.Millisecond,
		CookieMonitorInterval: 5 * time.Minute,
	}
	s := newWithBackendAndStore(fb, cfg, st)

	// 启动后台引擎
	s.syncWorker.Start()
	s.cookieMon.Start()

	// 第一次 Close
	s.Close()

	if !backendClosed.Load() {
		t.Fatalf("P06 failed: backend client pool was not closed")
	}

	// 验证幂等性：第二次调用 Close 不产生 panic 或错误
	s.Close()
	s.Close()
}
