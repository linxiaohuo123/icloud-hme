package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"icloud-hme/internal/store"
)

func TestCloseContextCompletesAfterWaitTimeout(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var backendClosed atomic.Int32
	base, cancel := context.WithCancel(context.Background())
	s := &Server{store: st, ctx: base, cancel: cancel, be: &fakeBackend{onClose: func() { backendClosed.Add(1) }}}
	s.autoSyncWg.Add(1)
	ctx, stop := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer stop()
	if err := s.CloseContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected wait timeout, got %v", err)
	}
	if err := st.DB().Ping(); err != nil || backendClosed.Load() != 0 {
		t.Fatalf("resources closed while worker was running: ping=%v backend=%d", err, backendClosed.Load())
	}
	s.autoSyncWg.Done()
	retryCtx, retryCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer retryCancel()
	var waiters sync.WaitGroup
	for i := 0; i < 4; i++ {
		waiters.Add(1)
		go func() {
			defer waiters.Done()
			if err := s.CloseContext(retryCtx); err != nil {
				t.Errorf("shutdown retry failed: %v", err)
			}
		}()
	}
	waiters.Wait()
	if backendClosed.Load() != 1 || st.DB().Ping() == nil {
		t.Fatalf("retry did not finish exactly one cleanup: backend=%d", backendClosed.Load())
	}
	if err := s.CloseContext(context.Background()); err != nil {
		t.Fatalf("completed shutdown is not idempotent: %v", err)
	}
}
