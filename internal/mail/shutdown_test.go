// [INPUT]: Local IMAP protocol peers and the production Client/Pool.
// [OUTPUT]: Deadline, idle-pool shutdown and independent-operation regressions.
// [POS]: internal/mail bounded network lifecycle tests.
package mail

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	imapclient "github.com/emersion/go-imap/client"
)

// localProtocolClient uses a real go-imap reader and joins the peer on cleanup.
func localProtocolClient(t *testing.T, email string, blackhole bool) *Client {
	t.Helper()
	conn, peer := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer peer.Close()
		fmt.Fprint(peer, "* PREAUTH [CAPABILITY IMAP4rev1] local-test\r\n")
		r := bufio.NewReader(peer)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			parts := strings.Fields(line)
			if len(parts) < 2 {
				return
			}
			if blackhole {
				continue
			}
			if strings.EqualFold(parts[1], "SELECT") {
				fmt.Fprintf(peer, "* 7 EXISTS\r\n* OK [UIDVALIDITY 1] valid\r\n%s OK [READ-WRITE] SELECT completed\r\n", parts[0])
			} else {
				fmt.Fprintf(peer, "%s OK command completed\r\n", parts[0])
			}
		}
	}()
	cli, err := imapclient.New(conn)
	if err != nil {
		peer.Close()
		<-done
		t.Fatal(err)
	}
	c := NewClientForTesting(email, "fake-password", conn, cli)
	t.Cleanup(func() { c.ForceClose(); peer.Close(); <-done })
	return c
}

func TestIMAPCommandDeadlineSurvivesExecute(t *testing.T) {
	c := localProtocolClient(t, "timeout@invalid.example", true)
	c.SetDeadline(time.Now().Add(60 * time.Millisecond))
	done := make(chan error, 1)
	go func() { done <- c.cli.Noop() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("unanswered command unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		c.ForceClose()
		<-done
		t.Fatal("go-imap cleared the configured command deadline")
	}
}

func TestDisconnectBlackholeIsBounded(t *testing.T) {
	c := localProtocolClient(t, "logout@invalid.example", true)
	done := make(chan struct{})
	go func() { c.Disconnect(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		c.ForceClose()
		<-done
		t.Fatal("Disconnect exceeded the finite logout budget")
	}
}

func TestPoolCloseDoesNotWaitForLogout(t *testing.T) {
	p := NewPool()
	c := localProtocolClient(t, "idle@invalid.example", true)
	p.SetClientForTesting("idle@invalid.example", "fake-password", c)
	done := make(chan struct{}, 2)
	for range 2 {
		go func() { p.Close(); done <- struct{}{} }()
	}
	for range 2 {
		select {
		case <-done:
		case <-time.After(time.Second):
			c.ForceClose()
			t.Fatal("idle-pool close blocked on a network command")
		}
	}
	conns, active, fg := p.Stats()
	if conns != 0 || active != 0 || fg != 0 {
		t.Fatalf("pool did not drain: %d %d %d", conns, active, fg)
	}
	select {
	case <-p.reaperDone:
	default:
		t.Fatal("pool reaper survived successful Close")
	}
}

func assertLocalIMAPOperation(t *testing.T, p *Pool, email string, background bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	called, count := false, 0
	err := p.DoContextWithServerAndKind(ctx, email, "fake-password", IMAPServer, IMAPPort, "", background, func(c *Client) error {
		called = true
		var err error
		count, err = c.InboxCountContext(ctx)
		return err
	})
	if err != nil || !called || count != 7 {
		t.Fatalf("local SELECT: called=%v count=%d err=%v", called, count, err)
	}
	_, active, fg := p.Stats()
	if active != 0 || fg != 0 {
		t.Fatalf("operation slots leaked: %d %d", active, fg)
	}
}
