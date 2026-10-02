package server

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"icloud-hme/internal/auth"
)

func TestRequestLimiter_Waiters(t *testing.T) {
	cfg := RequestLimiterConfig{
		MaxWaitersGlobal:       10,
		MaxWaitersPerPrincipal: 5,
		MaxWaitersPerKey:       2,
	}
	limiter := NewRequestLimiter(cfg)

	p1 := auth.Principal{Kind: auth.PrincipalToken, ID: "token-1"}
	p2 := auth.Principal{Kind: auth.PrincipalToken, ID: "token-2"}

	// 1. 同一个 key 最多 2 个
	rel1, err := limiter.AcquireWaiter(p1, "vreq:1")
	if err != nil {
		t.Fatalf("acquire 1 failed: %v", err)
	}
	rel2, err := limiter.AcquireWaiter(p1, "vreq:1")
	if err != nil {
		t.Fatalf("acquire 2 failed: %v", err)
	}

	_, err = limiter.AcquireWaiter(p1, "vreq:1")
	if err == nil {
		t.Fatalf("expected error for 3rd waiter on same key")
	}
	if be, ok := err.(*BackendError); !ok || be.Status != http.StatusTooManyRequests || be.Code != "VERIFY_WAITER_LIMIT" {
		t.Fatalf("expected 429 VERIFY_WAITER_LIMIT, got %v", err)
	}

	// 释放一个后，可以再次获取
	rel1()
	rel3, err := limiter.AcquireWaiter(p1, "vreq:1")
	if err != nil {
		t.Fatalf("acquire after release failed: %v", err)
	}
	rel2()
	rel3()

	// 2. 单主体上限为 5
	var releases []func()
	for i := 0; i < 5; i++ {
		rel, err := limiter.AcquireWaiter(p1, fmt.Sprintf("vreq:key-%d", i))
		if err != nil {
			t.Fatalf("acquire key-%d failed: %v", i, err)
		}
		releases = append(releases, rel)
	}
	// 第 6 个应被单主体限制挡住
	_, err = limiter.AcquireWaiter(p1, "vreq:key-6")
	if err == nil {
		t.Fatalf("expected error for 6th waiter of principal")
	}
	if be, ok := err.(*BackendError); !ok || be.Status != http.StatusTooManyRequests || be.Code != "TOKEN_CONCURRENCY_LIMIT" {
		t.Fatalf("expected 429 TOKEN_CONCURRENCY_LIMIT, got %v", err)
	}

	// p2 仍可获取
	relP2, err := limiter.AcquireWaiter(p2, "vreq:p2-1")
	if err != nil {
		t.Fatalf("p2 acquire failed: %v", err)
	}
	relP2()

	for _, r := range releases {
		r()
	}

	// 3. 全局上限
	var allReleases []func()
	for i := 0; i < 10; i++ {
		p := auth.Principal{Kind: auth.PrincipalToken, ID: fmt.Sprintf("tok-%d", i)}
		rel, err := limiter.AcquireWaiter(p, fmt.Sprintf("vreq:%d", i))
		if err != nil {
			t.Fatalf("global acquire %d failed: %v", i, err)
		}
		allReleases = append(allReleases, rel)
	}
	_, err = limiter.AcquireWaiter(auth.Principal{Kind: auth.PrincipalToken, ID: "tok-overflow"}, "vreq:overflow")
	if err == nil {
		t.Fatalf("expected error for 11th global waiter")
	}
	if be, ok := err.(*BackendError); !ok || be.Status != http.StatusServiceUnavailable || be.Code != "SERVER_BUSY" {
		t.Fatalf("expected 503 SERVER_BUSY, got %v", err)
	}

	for _, r := range allReleases {
		r()
	}

	stats := limiter.Stats()
	if stats.ActiveWaiters != 0 {
		t.Fatalf("expected 0 active waiters after all releases, got %d", stats.ActiveWaiters)
	}
}

func TestRequestLimiter_Inflight(t *testing.T) {
	cfg := RequestLimiterConfig{
		MaxInflightGlobal:       3,
		MaxInflightPerPrincipal: 2,
	}
	limiter := NewRequestLimiter(cfg)

	p1 := auth.Principal{Kind: auth.PrincipalToken, ID: "token-1"}

	t1, err := limiter.AcquireGlobalInflight()
	if err != nil {
		t.Fatalf("t1 acquire failed: %v", err)
	}
	if err := t1.BindPrincipalInflight(p1); err != nil {
		t.Fatalf("t1 bind failed: %v", err)
	}

	t2, err := limiter.AcquireGlobalInflight()
	if err != nil {
		t.Fatalf("t2 acquire failed: %v", err)
	}
	if err := t2.BindPrincipalInflight(p1); err != nil {
		t.Fatalf("t2 bind failed: %v", err)
	}

	// p1 达到单主体限制 2
	t3, err := limiter.AcquireGlobalInflight()
	if err != nil {
		t.Fatalf("t3 acquire global failed: %v", err)
	}
	if err := t3.BindPrincipalInflight(p1); err == nil {
		t.Fatalf("expected error binding 3rd inflight to p1")
	}
	t3.Release()

	// 全局上限测试
	p2 := auth.Principal{Kind: auth.PrincipalToken, ID: "token-2"}
	t3, err = limiter.AcquireGlobalInflight()
	if err != nil {
		t.Fatalf("t3 acquire global failed: %v", err)
	}
	if err := t3.BindPrincipalInflight(p2); err != nil {
		t.Fatalf("t3 bind p2 failed: %v", err)
	}

	// 全局达到 3
	_, err = limiter.AcquireGlobalInflight()
	if err == nil {
		t.Fatalf("expected global inflight limit reached")
	}

	t1.Release()
	t2.Release()
	t3.Release()

	stats := limiter.Stats()
	if stats.ActiveInflight != 0 {
		t.Fatalf("expected 0 active inflight after release, got %d", stats.ActiveInflight)
	}
}

func TestRequestLimiter_ConcurrentReleases(t *testing.T) {
	cfg := RequestLimiterConfig{
		MaxWaitersGlobal:       100,
		MaxWaitersPerPrincipal: 100,
		MaxWaitersPerKey:       10,
	}
	limiter := NewRequestLimiter(cfg)
	p := auth.Principal{Kind: auth.PrincipalToken, ID: "tok-test"}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rel, err := limiter.AcquireWaiter(p, fmt.Sprintf("key-%d", idx%10))
			if err != nil {
				t.Errorf("acquire failed: %v", err)
				return
			}
			rel()
			// 多次调用 release 应幂等
			rel()
		}(i)
	}
	wg.Wait()

	stats := limiter.Stats()
	if stats.ActiveWaiters != 0 {
		t.Fatalf("expected 0 active waiters, got %d", stats.ActiveWaiters)
	}
}

// 管理员会话与全局 API Key 共用主体 ID "admin"，限流必须按凭据来源分桶；普通令牌仍按令牌 ID 计数。
func TestRequestLimiterSeparatesAdminCredentialBuckets(t *testing.T) {
	l := NewRequestLimiter(RequestLimiterConfig{MaxInflightGlobal: 10, MaxInflightPerPrincipal: 1, MaxWaitersPerPrincipal: 1})
	session := auth.Principal{Kind: auth.PrincipalAdmin, ID: "admin", TokenName: "admin_session"}
	apiKey := auth.Principal{Kind: auth.PrincipalAdmin, ID: "admin", TokenName: "global_api_key"}

	t1, _ := l.AcquireGlobalInflight()
	if err := t1.BindPrincipalInflight(apiKey); err != nil {
		t.Fatal(err)
	}
	t2, _ := l.AcquireGlobalInflight()
	if err := t2.BindPrincipalInflight(session); err != nil {
		t.Fatalf("global API key exhausted admin session inflight bucket: %v", err)
	}
	rel, err := l.AcquireWaiter(apiKey, "k1")
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	rel2, err := l.AcquireWaiter(session, "k2")
	if err != nil {
		t.Fatalf("global API key exhausted admin session waiter bucket: %v", err)
	}
	defer rel2()

	tokA := auth.Principal{Kind: auth.PrincipalToken, ID: "tok_1", TokenName: "a"}
	tokB := auth.Principal{Kind: auth.PrincipalToken, ID: "tok_1", TokenName: "renamed"}
	t3, _ := l.AcquireGlobalInflight()
	if err := t3.BindPrincipalInflight(tokA); err != nil {
		t.Fatal(err)
	}
	t4, _ := l.AcquireGlobalInflight()
	if err := t4.BindPrincipalInflight(tokB); err == nil {
		t.Fatal("token rename must not create a new inflight bucket")
	}
}
