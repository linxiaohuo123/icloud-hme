package server

import (
	"context"
	"strings"
	"testing"

	"icloud-hme/internal/account"
	"icloud-hme/internal/store"
)

func TestListInboxContext_MailboxFailureNoWebMailFallback(t *testing.T) {
	dir := t.TempDir()
	st, err := store.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer st.Close()

	// 直接将带 Mailbox 的账号持久化到 store
	accID := "acc_mb_test"
	err = st.SaveAccount(&store.AccountRecord{
		ID:          accID,
		Name:        "Test Mailbox Account",
		ICloudEmail: "test@icloud.com",
		MailboxJSON: `{"provider":"qq","email":"invalid@qq.com","imap_host":"127.0.0.1","imap_port":1,"password":"wrongpassword"}`,
		Status:      "active",
	})
	if err != nil {
		t.Fatalf("SaveAccount failed: %v", err)
	}

	mgr, err := account.NewManager(dir, st)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	be := &managerBackend{mgr: mgr, store: st}

	_, err = be.ListInboxContext(context.Background(), InboxQuery{
		AccountID: accID,
		Limit:     10,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	beErr, ok := err.(*BackendError)
	if !ok {
		t.Fatalf("expected *BackendError, got %T: %v", err, err)
	}

	if beErr.Code != "UPSTREAM_FAILURE" {
		t.Errorf("expected code UPSTREAM_FAILURE, got %s", beErr.Code)
	}
	if !strings.Contains(beErr.Message, "收件邮箱读取失败") {
		t.Errorf("expected message to mention '收件邮箱读取失败', got: %s", beErr.Message)
	}
}

func TestListInboxContext_NonICloudDomainHelpfulError(t *testing.T) {
	dir := t.TempDir()
	st, err := store.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer st.Close()

	// 模拟第三方邮箱注册的 Apple ID (如 qq.com)，且设置了 AppPassword
	accID := "acc_thirdparty_test"
	err = st.SaveAccount(&store.AccountRecord{
		ID:          accID,
		Name:        "Test Third Party Account",
		RealEmail:   "user123@qq.com",
		ICloudEmail: "", // 没有原生 icloud 邮箱
		AppPassword: "test-app-password",
		Status:      "active",
	})
	if err != nil {
		t.Fatalf("SaveAccount failed: %v", err)
	}

	mgr, err := account.NewManager(dir, st)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	be := &managerBackend{mgr: mgr, store: st}

	_, err = be.ListInboxContext(context.Background(), InboxQuery{
		AccountID: accID,
		Limit:     10,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	beErr, ok := err.(*BackendError)
	if !ok {
		t.Fatalf("expected *BackendError, got %T: %v", err, err)
	}

	if beErr.Code != "NON_ICLOUD_MAIL_USER" {
		t.Errorf("expected code NON_ICLOUD_MAIL_USER, got %s", beErr.Code)
	}
	if !strings.Contains(beErr.Message, "接入收件邮箱") {
		t.Errorf("expected message to suggest '接入收件邮箱', got: %s", beErr.Message)
	}
}
