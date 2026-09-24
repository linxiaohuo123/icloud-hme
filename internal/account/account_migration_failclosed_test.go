package account

import (
	"os"
	"path/filepath"
	"testing"

	"icloud-hme/internal/store"
)

// TestPR06_AccountMigrateFailClosed 验证当 accounts.json 迁移到 SQLite 失败时，NewManager 必须 Fail-Closed 返回 error 且保留原 JSON 文件，杜绝内存与 SQLite 脑裂
func TestPR06_AccountMigrateFailClosed(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.NewStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 准备 accounts.json 源文件
	accJSON := `[{"id":"acc_fail","name":"Fail Account","real_email":"test@example.com","cookies":{"foo":"bar"}}]`
	jsonPath := filepath.Join(dataDir, "accounts.json")
	if err := os.WriteFile(jsonPath, []byte(accJSON), 0600); err != nil {
		t.Fatal(err)
	}

	// 给 accounts 表增加禁止写入触发器，使得 ListAllAccounts 成功 (0条记录)，但 SaveAccountsBatch 必定失败
	if _, err := st.DB().Exec("CREATE TRIGGER prevent_insert BEFORE INSERT ON accounts BEGIN SELECT RAISE(ABORT, 'insert blocked'); END;"); err != nil {
		t.Fatal(err)
	}

	// 期望：NewManager 必须返回明确的迁移失败错误，绝不能吞错启动
	mgr, err := NewManager(dataDir, st)
	if err == nil {
		mgr.Close()
		t.Fatalf("SQLite 保存失败时 NewManager 未能 fail-closed，导致内存与 SQLite 产生脑裂")
	}

	// 期望：源 accounts.json 必须完整保留，未被归档或删除
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("源 accounts.json 文件应完整保留现场: %v", err)
	}
	if string(raw) != accJSON {
		t.Fatalf("源 accounts.json 内容被意外改动")
	}
}
