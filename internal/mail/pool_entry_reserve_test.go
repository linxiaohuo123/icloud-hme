// [INPUT]: Production pool and a responsive local IMAP protocol peer.
// [OUTPUT]: Reserved background entry completes SELECT while foreground entries queue.
// [POS]: internal/mail entry-capacity regression.
package mail

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestForegroundPinsMustKeepBackgroundEntryCapacity(t *testing.T) {
	port := IMAPPort

	// 池硬上限 3，活跃上限 3，后台保留 1。
	// 因此前台最多只能占用 3 - 1 = 2 个不同条目，保留 1 个条目给后台 (T03)。
	p := NewPoolWithLimits(3, 3, 1)
	defer p.Close()

	var pcs []*pooledConn
	for i := 0; i < 2; i++ {
		pc, unpin, err := p.getOrCreateWithServerAndPinAndKind(fmt.Sprintf("foreground-%d@invalid.example", i), IMAPServer, port, false)
		if err != nil {
			t.Fatal(err)
		}
		pc.lock()
		unpin()
		pcs = append(pcs, pc)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		email := fmt.Sprintf("foreground-%d@invalid.example", i)
		go func() {
			done <- p.DoContextWithServerAndKind(ctx, email, "fake-password", IMAPServer, port, "", false, func(*Client) error { return nil })
		}()
	}
	defer func() {
		cancel()
		for i := 0; i < 2; i++ {
			<-done
		}
		for _, pc := range pcs {
			pc.unlock()
		}
	}()

	// 等待 2 个前台请求成功获取 pin 并排队等待串行锁，此时前台条目配额已饱和 (2/2)
	until := time.Now().Add(time.Second)
	allPinned := false
	for !allPinned && time.Now().Before(until) {
		p.mu.Lock()
		allPinned = true
		for _, pc := range pcs {
			allPinned = allPinned && pc.pinCount == 1
		}
		p.mu.Unlock()
		if !allPinned {
			time.Sleep(time.Millisecond)
		}
	}
	if !allPinned {
		t.Fatal("foreground serial waiters did not acquire pins")
	}

	// 验证第 3 个不同邮箱的前台请求被前台条目配额准确拦截，不能侵占保留容量
	fg3Err := p.DoContextWithServerAndKind(ctx, "foreground-3@invalid.example", "fake-password", IMAPServer, port, "", false, func(*Client) error { return nil })
	if !errors.Is(fg3Err, ErrIMAPPoolBusy) {
		t.Fatalf("expected foreground-3 to be rejected by foreground entry limit, got %v", fg3Err)
	}

	// 验证后台请求（isBackground=true）不受前台条目上限限制，能够成功借用第 3 个保留条目并进入执行
	pc, unpin, err := p.getOrCreateWithServerAndPinAndKind("independent-background@invalid.example", IMAPServer, port, true)
	if err != nil {
		t.Fatal(err)
	}
	pc.client = localProtocolClient(t, "independent-background@invalid.example", false)
	pc.appPassword = "fake-password"
	pc.lastUsed = time.Now()
	unpin()
	assertLocalIMAPOperation(t, p, "independent-background@invalid.example", true)
}
