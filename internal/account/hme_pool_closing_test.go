package account

import (
	"context"
	"testing"
	"time"

	"icloud-hme/internal/hme"
)

// TestHMEClosingAtomicStateSynchronization 验证 drop/Close 与并发借用中 closing 状态原子同步 (T07)
func TestHMEClosingAtomicStateSynchronization(t *testing.T) {
	m := newPoolTestManager(t, 1)
	id := firstAccountIDs(m, 1)[0]

	gate := make(chan struct{})
	entered := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- m.WithHMEClientContext(context.Background(), id, func(*hme.Client) error {
			close(entered)
			<-gate
			return nil
		})
	}()
	<-entered

	m.hmePool.mu.Lock()
	e := m.hmePool.entries[id]
	m.hmePool.mu.Unlock()
	if e == nil {
		t.Fatal("entry was not created")
	}

	// 1. 验证初始活跃状态下 closing 为 false
	if e.closing.Load() {
		t.Fatal("active entry should not be closing")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	queuedDone := make(chan error, 1)
	go func() {
		queuedDone <- m.WithHMEClientContext(ctx, id, func(*hme.Client) error {
			return nil
		})
	}()

	rereviewEventually(t, func() bool {
		m.hmePool.mu.Lock()
		defer m.hmePool.mu.Unlock()
		return e.pinCount == 2
	})

	// 2. 执行 Close 并发关闭
	closeDone := make(chan struct{})
	go func() {
		m.hmePool.Close()
		close(closeDone)
	}()

	rereviewEventually(t, func() bool {
		m.hmePool.mu.Lock()
		defer m.hmePool.mu.Unlock()
		return m.hmePool.closed
	})

	// 3. 断言 entry.closing 已被原子设置为 true
	if !e.closing.Load() {
		t.Fatal("entry.closing was not atomically set to true during Close")
	}

	// 4. 释放正在执行的操作，排队借用者拿到条目锁后必须复查 closing 状态并拒绝重建客户端
	close(gate)
	<-firstDone
	<-closeDone

	select {
	case err := <-queuedDone:
		if err == nil || err.Error() != "HME 客户端池已关闭" {
			t.Fatalf("expected 'HME 客户端池已关闭', got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued borrower did not exit")
	}

	// 5. 断言未重建孤立客户端
	e.mu.Lock()
	orphan := e.client != nil
	if e.client != nil {
		e.client.Close()
		e.client = nil
	}
	e.mu.Unlock()

	if orphan {
		t.Fatal("client was recreated on closed entry")
	}
}

func TestHMEAcquireAfterClose(t *testing.T) {
	p := newHMEClientPool()
	p.Close()
	_, unpin, err := p.acquire("after-close")
	if unpin != nil {
		unpin()
	}
	defer p.Close()
	if err == nil {
		t.Fatal("closed HME pool still accepts new borrowed entries")
	}
}
