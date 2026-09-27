package account

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func testManagerWithMailbox(t *testing.T) (*Manager, string, string) {
	t.Helper()
	dir := t.TempDir()
	m, err := NewManager(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	acc, err := m.AddAccount("owner", "", "icloud.com", "")
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	stored := m.accounts[acc.ID]
	stored.ICloudEmail = "owner@icloud.com"
	stored.AppPassword = "native-secret"
	stored.Mailbox = &MailboxConfig{Provider: "qq", Email: "old@qq.com", IMAPHost: "imap.qq.com", IMAPPort: 993, Password: "old-secret"}
	err = m.saveAccount(stored)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	return m, dir, acc.ID
}

func TestSetMailboxRequiresNewCodeWhenEndpointChanges(t *testing.T) {
	m, _, id := testManagerWithMailbox(t)
	cases := []MailboxConfig{
		{Provider: "qq", Email: "new@qq.com", IMAPHost: "imap.qq.com", IMAPPort: 993},
		{Provider: "custom", Email: "old@qq.com", IMAPHost: "imap.example.com", IMAPPort: 993},
		{Provider: "qq", Email: "old@qq.com", IMAPHost: "imap.qq.com", IMAPPort: 143},
	}
	for _, config := range cases {
		err := m.SetMailboxContext(context.Background(), id, config)
		if err == nil || !strings.Contains(err.Error(), "授权码不能为空") {
			t.Fatalf("changed endpoint must require a new code: config=%+v err=%v", config, err)
		}
		acc, _ := m.GetAccount(id)
		if acc.Mailbox == nil || acc.Mailbox.Email != "old@qq.com" || acc.Mailbox.Password != "old-secret" {
			t.Fatalf("failed rebind changed mailbox: %+v", acc.Mailbox)
		}
	}
}

func TestRemoveMailboxPersistsAndRestoresNativeCredentials(t *testing.T) {
	m, dir, id := testManagerWithMailbox(t)
	if err := m.RemoveMailbox(id); err != nil {
		t.Fatal(err)
	}
	acc, _ := m.GetAccount(id)
	if acc.Mailbox != nil || acc.AppPassword != "native-secret" {
		t.Fatalf("unbind should preserve native credentials: %+v", acc)
	}
	if email, password, _, err := m.imapCreds(id); err != nil || email != "owner@icloud.com" || password != "native-secret" {
		t.Fatalf("native IMAP path not restored: email=%q err=%v", email, err)
	}
	reloaded, err := NewManager(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	acc, _ = reloaded.GetAccount(id)
	if acc.Mailbox != nil {
		t.Fatalf("unbind was not persisted: %+v", acc.Mailbox)
	}
}

func TestRemoveMailboxSaveFailureRollsBackMemory(t *testing.T) {
	m, dir, id := testManagerWithMailbox(t)
	m.dataFile = filepath.Join(dir, "missing", "accounts.json")
	if err := m.RemoveMailbox(id); !errors.Is(err, ErrMailConfigPersistence) {
		t.Fatalf("expected persistence failure, got %v", err)
	}
	acc, _ := m.GetAccount(id)
	if acc.Mailbox == nil || acc.Mailbox.Email != "old@qq.com" {
		t.Fatalf("failed unbind changed in-memory mailbox: %+v", acc.Mailbox)
	}
}

func TestMailboxPreflightHonorsCanceledContext(t *testing.T) {
	m, _, id := testManagerWithMailbox(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.SetMailboxContext(ctx, id, MailboxConfig{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("SetMailboxContext returned %v", err)
	}
	if err := m.SetAppPasswordContext(ctx, id, "owner@icloud.com", "secret"); !errors.Is(err, context.Canceled) {
		t.Fatalf("SetAppPasswordContext returned %v", err)
	}
}
