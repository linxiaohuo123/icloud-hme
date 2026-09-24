package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestStore_Ping_NotBlockedByBusinessLock 验证在业务大锁被独占持有时，Ping 不被阻塞且快速返回
func TestStore_Ping_NotBlockedByBusinessLock(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer st.Close()

	// 模拟大事务或耗时业务操作长时间占有业务锁 (500ms)
	st.mu.Lock()
	lockReleased := make(chan struct{})
	go func() {
		time.Sleep(500 * time.Millisecond)
		st.mu.Unlock()
		close(lockReleased)
	}()

	// 探针设定超时 1 秒
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	start := time.Now()
	err = st.Ping(ctx)
	elapsed := time.Since(start)

	// 修复前: Ping 第一行调用 s.mu.Lock()，强行等待锁释放，耗时 >= 500ms
	// 修复后: Ping 不依赖 s.mu 业务大锁，直接调用并发安全的 db.PingContext(ctx)，并在 50ms 内快速成功返回
	if elapsed >= 200*time.Millisecond {
		t.Fatalf("Ping was blocked by business lock for %v (expected completion well under 200ms)", elapsed)
	}
	if err != nil {
		t.Fatalf("expected Ping to succeed despite business lock, got %v", err)
	}
	<-lockReleased
}

// TestStore_Ping_ImmediateReturnOnExpiredContext 验证当传入已超时/取消的 context 时，Ping 立即返回驱动层错误
func TestStore_Ping_ImmediateReturnOnExpiredContext(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer st.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	time.Sleep(2 * time.Millisecond) // 确保超时已触发
	defer cancel()

	start := time.Now()
	err = st.Ping(ctx)
	elapsed := time.Since(start)

	if elapsed >= 100*time.Millisecond {
		t.Fatalf("Ping on expired context took too long: %v", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
}
