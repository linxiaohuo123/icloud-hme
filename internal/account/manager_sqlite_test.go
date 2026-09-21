/**
 * [INPUT]: 依赖 testing, os, filepath, icloud-hme/internal/store
 * [OUTPUT]: 测试 Manager 与 SQLite 后端的全生命周期：落盘、细粒度写、自动无损迁移与重载
 * [POS]: internal/account 的集成测试，验证千号规模化存储升级的正确性
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package account

import (
	"os"
	"path/filepath"
	"testing"

	"icloud-hme/internal/store"
)

func TestManagerWithSQLitePersistenceAndMigration(t *testing.T) {
	dir := t.TempDir()

	// 1. 模拟遗留的 accounts.json 数据
	legacyJSON := `{
		"accounts": {
			"acc_legacy_1": {
				"id": "acc_legacy_1",
				"name": "老版账号1",
				"icloud_email": "legacy1@icloud.com",
				"real_email": "legacy1@icloud.com",
				"status": "active",
				"alias_total": 42,
				"alias_active": 10,
				"created_at": "2026-01-01T00:00:00Z"
			}
		}
	}`
	jsonPath := filepath.Join(dir, "accounts.json")
	if err := os.WriteFile(jsonPath, []byte(legacyJSON), 0600); err != nil {
		t.Fatalf("写入测试 accounts.json 失败: %v", err)
	}

	// 2. 初始化 Store 与绑定 SQLite 的 Manager
	st, err := store.NewStore(dir)
	if err != nil {
		t.Fatalf("初始化 Store 失败: %v", err)
	}
	defer st.Close()

	mgr, err := NewManager(dir, st)
	if err != nil {
		t.Fatalf("初始化 Manager 失败: %v", err)
	}
	defer mgr.Close()

	// 验证老数据是否已被加载到内存
	if acc, ok := mgr.GetAccount("acc_legacy_1"); !ok || acc.AliasTotal != 42 {
		t.Fatalf("未正确从老数据加载账号: ok=%v, acc=%+v", ok, acc)
	}

	// 验证老 accounts.json 是否已无损归档为 .migrated
	if _, err := os.Stat(jsonPath + ".migrated"); err != nil {
		t.Fatalf("老 accounts.json 未被重命名为 .migrated: %v", err)
	}

	// 3. 添加新账号到 SQLite (单行写)
	sum, err := mgr.AddAccountWithInput(AddAccountInput{
		Name:        "新账号2",
		ICloudEmail: "new2@icloud.com",
		Tags:        []string{"vip", "asia"},
	})
	if err != nil {
		t.Fatalf("添加新账号失败: %v", err)
	}

	// 4. 细粒度修改别名配额
	if err := mgr.AdjustAliasCounts(sum.ID, 5, 5); err != nil {
		t.Fatalf("调整别名配额失败: %v", err)
	}

	// 5. 验证底层 SQLite 数据表真实存在这两条数据
	recs, err := st.ListAllAccounts()
	if err != nil {
		t.Fatalf("查询 SQLite 账号表失败: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("SQLite 应有 2 个账号，实际得到 %d", len(recs))
	}

	// 6. 销毁 Manager，直接由新的 Manager 重新从 SQLite 加载（验证零依赖 JSON 重启）
	mgr2, err := NewManager(dir, st)
	if err != nil {
		t.Fatalf("第二次启动 Manager 失败: %v", err)
	}
	defer mgr2.Close()

	if len(mgr2.ListAccounts()) != 2 {
		t.Fatalf("从 SQLite 重启后账号总数应为 2，实际为 %d", len(mgr2.ListAccounts()))
	}

	newAcc, ok := mgr2.GetAccount(sum.ID)
	if !ok {
		t.Fatalf("重启后未找到新账号: %s", sum.ID)
	}
	if newAcc.AliasTotal != 5 || newAcc.AliasActive != 5 {
		t.Fatalf("重启后新账号配额不符: total=%d, active=%d", newAcc.AliasTotal, newAcc.AliasActive)
	}
	if len(newAcc.Tags) != 2 || newAcc.Tags[0] != "vip" {
		t.Fatalf("重启后新账号标签不符: %+v", newAcc.Tags)
	}

	// 7. 删除账号测试
	if ok := mgr2.RemoveAccount("acc_legacy_1"); !ok {
		t.Fatalf("删除老账号失败")
	}
	recsAfterDel, _ := st.ListAllAccounts()
	if len(recsAfterDel) != 1 || recsAfterDel[0].ID != sum.ID {
		t.Fatalf("SQLite 中老账号未被物理删除: %+v", recsAfterDel)
	}
}
