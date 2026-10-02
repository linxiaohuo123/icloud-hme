// [INPUT]: Account manager snapshots and mutable IMAP configuration.
// [OUTPUT]: Physical-source identity and frozen configuration regression.
// [POS]: internal/account mail configuration isolation.
// [PROTOCOL]: Update this header and CLAUDE.md when changing this file.
package account

import (
	"context"
	"testing"
)

func TestCaptureMailContextSourceAndSnapshot(t *testing.T) {
	t.Setenv("ICLOUD_HME_IMAP_DIRECT", "false")
	m := newPoolTestManager(t, 1)
	id := firstAccountIDs(m, 1)[0]
	m.mu.Lock()
	m.accounts[id].Mailbox = &MailboxConfig{Email: "Inbox@Example.com", Password: "first-secret", IMAPHost: "imap.example.com", IMAPPort: 993}
	m.mu.Unlock()
	frozen, source, fp, err := m.CaptureMailContext(context.Background(), id)
	if err != nil || source == "" || fp == "" {
		t.Fatalf("capture: %q %q %v", source, fp, err)
	}
	m.mu.Lock()
	m.accounts[id].Mailbox.Password = "rotated-secret"
	m.accounts[id].Proxy = "socks5://127.0.0.1:9090"
	m.mu.Unlock()
	_, rotatedSource, rotatedFP, err := m.CaptureMailContext(context.Background(), id)
	if err != nil || rotatedSource != source || rotatedFP == fp {
		t.Fatalf("rotation changed physical source or reused connection: %v", err)
	}
	m.mu.Lock()
	m.accounts[id].Mailbox.Email = "new@example.com"
	m.mu.Unlock()
	_, newSource, _, err := m.CaptureMailContext(context.Background(), id)
	if err != nil || newSource == source {
		t.Fatalf("mailbox switch kept old source: %v", err)
	}
	snap, err := m.mailAccountSnapshot(frozen, id)
	if err != nil || snap.Mailbox.Email != "Inbox@Example.com" || snap.Mailbox.Password != "first-secret" || snap.Proxy != "" {
		t.Fatalf("snapshot drifted: %v", err)
	}
	_, againSource, againFP, err := m.CaptureMailContext(frozen, id)
	if err != nil || againSource != source || againFP != fp {
		t.Fatal("frozen context changed during scan")
	}
}
