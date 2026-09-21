package account

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseAccountsBytes(t *testing.T) {
	// 1. 经典包装对象格式 {"accounts": {"acc_1": {"id": "acc_1", "name": "主号"}}}
	rawObj := []byte(`{"accounts": {"acc_1": {"id": "acc_1", "name": "主号"}}}`)
	res, err := parseAccountsBytes(rawObj)
	if err != nil || len(res) != 1 || res["acc_1"].Name != "主号" {
		t.Fatalf("parseAccountsBytes(rawObj) 失败: %v, res: %+v", err, res)
	}

	// 2. 经典包装数组格式 {"accounts": [{"id": "acc_2", "name": "副号"}]}
	rawWrappedArr := []byte(`{"accounts": [{"id": "acc_2", "name": "副号"}]}`)
	res, err = parseAccountsBytes(rawWrappedArr)
	if err != nil || len(res) != 1 || res["acc_2"].Name != "副号" {
		t.Fatalf("parseAccountsBytes(rawWrappedArr) 失败: %v, res: %+v", err, res)
	}

	// 3. 裸数组格式 [{"id": "acc_3", "name": "测试号"}]
	rawBareArr := []byte(`[{"id": "acc_3", "name": "测试号"}]`)
	res, err = parseAccountsBytes(rawBareArr)
	if err != nil || len(res) != 1 || res["acc_3"].Name != "测试号" {
		t.Fatalf("parseAccountsBytes(rawBareArr) 失败: %v, res: %+v", err, res)
	}

	// 4. 裸字典格式 {"acc_4": {"name": "直接字典"}}
	rawBareDict := []byte(`{"acc_4": {"name": "直接字典"}}`)
	res, err = parseAccountsBytes(rawBareDict)
	if err != nil || len(res) != 1 || res["acc_4"].Name != "直接字典" || res["acc_4"].ID != "acc_4" {
		t.Fatalf("parseAccountsBytes(rawBareDict) 失败: %v, res: %+v", err, res)
	}
}

func TestImportEditedExample(t *testing.T) {
	tmpDir := t.TempDir()
	exampleFile := filepath.Join(tmpDir, "accounts.example.json")

	// 1. 仅包含占位符的 example 文件 -> 不应导入任何账号
	placeholderContent := `{
		"accounts": {
			"acc_example": {
				"id": "acc_example",
				"name": "示例账号",
				"icloud_email": "your_email@icloud.com",
				"cookies": {"X-APPLE-WEBAUTH-TOKEN": "paste_your_cookie_value_here"},
				"app_password": "xxxx-xxxx-xxxx-xxxx"
			}
		}
	}`
	if err := os.WriteFile(exampleFile, []byte(placeholderContent), 0600); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewManager 失败: %v", err)
	}
	if len(mgr.ListAccounts()) != 0 {
		t.Fatalf("占位符账号不应被导入，实际导入了: %d", len(mgr.ListAccounts()))
	}

	// 2. 用户编辑了真实的 Cookie 与邮箱 -> 应当自动识别并导入
	editedContent := `{
		"accounts": {
			"acc_real": {
				"id": "acc_real",
				"name": "真实主号",
				"icloud_email": "linus@icloud.com",
				"cookies": {"X-APPLE-WEBAUTH-TOKEN": "real-token-123456789"},
				"app_password": "abcd-efgh-ijkl-mnop"
			}
		}
	}`
	if err := os.WriteFile(exampleFile, []byte(editedContent), 0600); err != nil {
		t.Fatal(err)
	}

	mgr2, err := NewManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewManager 失败: %v", err)
	}
	accs := mgr2.ListAccounts()
	if len(accs) != 1 || accs[0].ID != "acc_real" {
		t.Fatalf("真实账号应当被成功导入，实际得到: %+v", accs)
	}

	// 校验 accounts.json 是否已生成
	realJSON := filepath.Join(tmpDir, "accounts.json")
	if _, err := os.Stat(realJSON); err != nil {
		t.Fatalf("导入后应当生成正式 accounts.json 文件: %v", err)
	}
}

func TestBackupRecoveryOnCorruptedOrMissingMainFile(t *testing.T) {
	tmpDir := t.TempDir()
	bakFile := filepath.Join(tmpDir, "accounts.json.bak")
	validBackup := []byte(`{"accounts": {"acc_bak": {"id": "acc_bak", "name": "备份账号"}}}`)
	if err := os.WriteFile(bakFile, validBackup, 0600); err != nil {
		t.Fatal(err)
	}

	// 1. accounts.json 缺失时，自动从 .bak 恢复
	mgr, err := NewManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewManager with .bak failed: %v", err)
	}
	accs := mgr.ListAccounts()
	if len(accs) != 1 || accs[0].ID != "acc_bak" {
		t.Fatalf("预期从备份恢复 1 个账号，实际得到: %+v", accs)
	}

	// 验证 accounts.json 是否已自动修复写回
	mainFile := filepath.Join(tmpDir, "accounts.json")
	if _, err := os.Stat(mainFile); err != nil {
		t.Fatalf("备份恢复后主文件未自动写回: %v", err)
	}

	// 2. accounts.json 损坏 (0 字节) 时，依然自动从 .bak 恢复
	if err := os.WriteFile(mainFile, []byte("   "), 0600); err != nil {
		t.Fatal(err)
	}
	mgr2, err := NewManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewManager with 0-byte main file failed: %v", err)
	}
	if len(mgr2.ListAccounts()) != 1 {
		t.Fatalf("0 字节损坏时应从 .bak 恢复")
	}
}
