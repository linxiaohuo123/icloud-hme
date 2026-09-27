package mail

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"
	"testing"
	"time"

	imapclient "github.com/emersion/go-imap/client"
)

func TestConnectContextCancelsExistingConnectionCheck(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	serverClosed := make(chan struct{})
	go func() {
		defer close(serverClosed)
		_, _ = serverConn.Write([]byte("* OK [CAPABILITY IMAP4rev1] ready\r\n"))
		_, _ = bufio.NewReader(serverConn).ReadString('\n')
		var one [1]byte
		_, _ = serverConn.Read(one[:])
	}()
	imapClient, err := imapclient.New(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	c := NewClientForTesting("test@example.com", "password", clientConn, imapClient)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.ConnectContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline during NOOP, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("NOOP cancellation took %v", elapsed)
	}
	if _, err := c.conn.Write([]byte("x")); err == nil {
		t.Fatal("canceled connection must be closed")
	}
	select {
	case <-serverClosed:
	case <-time.After(time.Second):
		t.Fatal("existing connection remained open after cancellation")
	}
}

func TestInboxCountContextCancelsSelect(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	serverClosed := make(chan struct{})
	go func() {
		defer close(serverClosed)
		_, _ = serverConn.Write([]byte("* PREAUTH [CAPABILITY IMAP4rev1] ready\r\n"))
		_, _ = bufio.NewReader(serverConn).ReadString('\n')
		var one [1]byte
		_, _ = serverConn.Read(one[:])
	}()
	imapClient, err := imapclient.New(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	c := NewClientForTesting("test@example.com", "password", clientConn, imapClient)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.InboxCountContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline during SELECT, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("SELECT cancellation took %v", elapsed)
	}
	select {
	case <-serverClosed:
	case <-time.After(time.Second):
		t.Fatal("SELECT connection remained open after cancellation")
	}
}

func TestConnectContextCancelsTLSHandshake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	closed := make(chan struct{})
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			close(closed)
			return
		}
		defer conn.Close()
		buf := make([]byte, 1024)
		for {
			if _, readErr := conn.Read(buf); readErr != nil {
				close(closed)
				return
			}
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	client := NewClientWithServer("test@example.com", "password", "127.0.0.1", port)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := client.ConnectContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline during TLS handshake, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("TLS cancellation took %v", elapsed)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("TLS connection remained open after cancellation")
	}
}

func TestPoolCanceledProxyDoesNotAttemptDirectFallback(t *testing.T) {
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyLn.Close()
	proxyClosed := make(chan struct{})
	go func() {
		conn, acceptErr := proxyLn.Accept()
		if acceptErr != nil {
			close(proxyClosed)
			return
		}
		defer conn.Close()
		buf := make([]byte, 1024)
		for {
			if _, readErr := conn.Read(buf); readErr != nil {
				close(proxyClosed)
				return
			}
		}
	}()
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer targetLn.Close()
	targetAccepted := make(chan struct{}, 1)
	go func() {
		conn, acceptErr := targetLn.Accept()
		if acceptErr == nil {
			targetAccepted <- struct{}{}
			_ = conn.Close()
		}
	}()

	pool := NewPool()
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	proxyURL := (&url.URL{Scheme: "http", Host: proxyLn.Addr().String()}).String()
	port := targetLn.Addr().(*net.TCPAddr).Port
	start := time.Now()
	err = pool.DoContextWithServer(ctx, "test@example.com", "password", "127.0.0.1", port, proxyURL, func(*Client) error {
		t.Fatal("callback must not run after connect cancellation")
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("proxy cancellation took %v", elapsed)
	}
	select {
	case <-proxyClosed:
	case <-time.After(time.Second):
		t.Fatal("proxy connection remained open")
	}
	select {
	case <-targetAccepted:
		t.Fatal("direct fallback ran after context deadline")
	default:
	}
	key := poolKey("test@example.com", "127.0.0.1", port)
	pc := pool.getOrCreateWithServer("test@example.com", "127.0.0.1", port)
	if pc == nil || !pc.tryLock() {
		t.Fatalf("pool slot %s was not released", strconv.Quote(key))
	}
	pc.unlock()
}
