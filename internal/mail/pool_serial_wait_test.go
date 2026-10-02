// [INPUT]: Production pool and a responsive local IMAP protocol peer.
// [OUTPUT]: Independent mailbox completes SELECT while hot-mailbox callers only queue.
// [POS]: internal/mail active-operation admission regression.
package mail

import (
	"context"
	"testing"
	"time"
)

func TestIMAPSerialWaitersMustNotSpendGlobalNetworkSlots(t *testing.T) {
	p := NewPoolWithLimits(50, 3, 1)
	defer p.Close()
	pc, unpin, err := p.getOrCreateWithServerAndPin("hot@invalid.example", IMAPServer, IMAPPort)
	if err != nil {
		t.Fatal(err)
	}
	defer unpin()
	pc.lock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 2)
	defer func() {
		cancel()
		for range 2 {
			<-done
		}
		pc.unlock()
	}()
	for i := 0; i < 2; i++ {
		go func() {
			done <- p.DoContext(ctx, "hot@invalid.example", "fake-password", "", func(*Client) error { return nil })
		}()
	}
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		p.mu.Lock()
		elem := p.items["hot@invalid.example"]
		pinCount := 0
		if elem != nil {
			pinCount = elem.Value.(*pooledConn).pinCount
		}
		p.mu.Unlock()
		if pinCount == 3 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	_, active, fg := p.Stats()
	if fg >= 2 {
		t.Fatalf("serial waiters incorrectly consumed foreground operation slots: active=%d fg=%d", active, fg)
	}
	p.SetClientForTesting("independent@invalid.example", "fake-password", localProtocolClient(t, "independent@invalid.example", false))
	assertLocalIMAPOperation(t, p, "independent@invalid.example", false)
}
