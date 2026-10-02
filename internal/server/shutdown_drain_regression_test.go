package server

import (
	"context"
	"testing"
	"time"

	"icloud-hme/internal/auth"
)

// 准入计数泄漏时停机仍须在限定时间内关闭连接池与 Store，不能无限自旋。
func TestShutdownDoesNotHangOnLeakedLimiterCount(t *testing.T) {
	old := limiterDrainTimeout
	limiterDrainTimeout = 100 * time.Millisecond
	defer func() { limiterDrainTimeout = old }()

	s := newWithBackend(&fakeBackend{}, Config{DataDir: t.TempDir()})
	// 模拟某条路径遗漏释放的等待名额。
	if _, err := s.requestLimiter.AcquireWaiter(auth.Principal{Kind: auth.PrincipalToken, ID: "leak"}, "leaked-key"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := s.CloseContext(ctx); err != nil {
		t.Fatalf("shutdown blocked by leaked limiter count: %v (after %v)", err, time.Since(start))
	}
}
