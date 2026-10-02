package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"icloud-hme/internal/hme"
)

func rereviewEventually(t *testing.T, predicate func() bool) {
	t.Helper()
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		if predicate() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("barrier was not reached")
}

// TestHMEDropWaitMustRespondToCancellation 验证 drop 同键关闭等待接受 Context 取消 (S03)
func TestHMEDropWaitMustRespondToCancellation(t *testing.T) {
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
	dropDone := make(chan struct{})
	go func() {
		m.hmePool.drop(id)
		close(dropDone)
	}()
	rereviewEventually(t, func() bool {
		m.hmePool.mu.Lock()
		defer m.hmePool.mu.Unlock()
		return m.hmePool.closingEntries[id] != nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	nextDone := make(chan error, 1)
	go func() {
		nextDone <- m.WithHMEClientContext(ctx, id, func(*hme.Client) error { return nil })
	}()
	var returned bool
	select {
	case <-nextDone:
		returned = true
	case <-time.After(120 * time.Millisecond):
	}
	close(gate)
	<-firstDone
	<-dropDone
	if !returned {
		<-nextDone
		t.Fatal("cancelled HME request remains blocked on closingEntries.closedDone until another operation finishes")
	}
}

// TestHMECloseMustCoverQueuedBorrowers 验证 Close 覆盖已借出排队者且严禁在 Close 返回后重建客户端 (S03)
func TestHMECloseMustCoverQueuedBorrowers(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	queuedDone := make(chan error, 1)
	closeDone := make(chan struct{})
	var calledAfterClose bool
	go func() {
		queuedDone <- m.WithHMEClientContext(ctx, id, func(*hme.Client) error {
			select {
			case <-closeDone:
				calledAfterClose = true
			default:
			}
			return nil
		})
	}()
	rereviewEventually(t, func() bool {
		m.hmePool.mu.Lock()
		defer m.hmePool.mu.Unlock()
		return e.pinCount == 2
	})
	go func() {
		m.hmePool.Close()
		close(closeDone)
	}()
	rereviewEventually(t, func() bool {
		m.hmePool.mu.Lock()
		defer m.hmePool.mu.Unlock()
		return m.hmePool.closed
	})
	close(gate)
	<-firstDone
	<-closeDone
	queuedErr := <-queuedDone
	e.mu.Lock()
	orphan := e.client != nil
	if e.client != nil {
		e.client.Close()
		e.client = nil
	}
	e.mu.Unlock()
	if calledAfterClose || orphan {
		t.Fatalf("queued operation ran after pool Close: calledAfterClose=%v orphanClient=%v queuedError=%v", calledAfterClose, orphan, queuedErr)
	}
}

// TestHMEWaitingSameAccountMustNotSpendGlobalNetworkSlots 验证等待单账号锁不占用全局网络操作槽位，独立账号不受影响 (S02)
func TestHMEWaitingSameAccountMustNotSpendGlobalNetworkSlots(t *testing.T) {
	m := newPoolTestManager(t, 2)
	ids := firstAccountIDs(m, 2)
	gate := make(chan struct{})
	entered := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- m.WithHMEClientContext(context.Background(), ids[0], func(*hme.Client) error {
			close(entered)
			<-gate
			return nil
		})
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 15)
	for i := 0; i < 15; i++ {
		go func() {
			done <- m.WithHMEClientContext(ctx, ids[0], func(*hme.Client) error { return nil })
		}()
	}
	rereviewEventually(t, func() bool {
		m.hmePool.mu.Lock()
		defer m.hmePool.mu.Unlock()
		e := m.hmePool.entries[ids[0]]
		return e != nil && e.pinCount == 16
	})
	_, hotActive := m.hmePool.Stats()
	if hotActive >= 16 {
		t.Fatalf("serialized waiters incorrectly consumed global network slots: active=%d", hotActive)
	}
	err := m.WithHMEClientContext(context.Background(), ids[1], func(*hme.Client) error { return nil })
	cancel()
	for i := 0; i < 15; i++ {
		<-done
	}
	close(gate)
	<-firstDone
	if errors.Is(err, ErrHMEOpBusy) {
		t.Fatalf("one executing account plus 15 serialized waiters consumes all %d global network slots and rejects an independent account: %v", hotActive, err)
	}
	if err != nil {
		t.Fatal(err)
	}
}
