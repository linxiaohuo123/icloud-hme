/**
 * [INPUT]: 依赖 testing, database/sql, os, path/filepath, fmt, modernc.org/sqlite
 * [OUTPUT]: 对外提供 TestReleaseValidationSnapshot_IncludesUncheckpointedWAL 与 TestReleaseValidation_QueryFailureCannotPass 回归单测
 * [POS]: scripts/ 的生产发布数据库只读验收回归门禁
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestReleaseValidationSnapshot_IncludesUncheckpointedWAL 验证快照机制具备真实 WAL 一致性，
// 绝不能丢弃位于 WAL 中尚未 checkpoint 的 committed 数据，且源数据库内容不得被修改。
func TestReleaseValidationSnapshot_IncludesUncheckpointedWAL(t *testing.T) {
	tempDir := t.TempDir()
	srcPath := filepath.Join(tempDir, "source_wal.db")

	// 1. 创建 SQLite DB
	db, err := sql.Open("sqlite", srcPath)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}

	// 2. 启用 WAL 模式
	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode=WAL;").Scan(&journalMode); err != nil || journalMode != "wal" {
		t.Fatalf("开启 WAL 模式失败: mode=%s, err=%v", journalMode, err)
	}

	// 3. 禁用自动 checkpoint，确保新增提交数据驻留在 WAL 中
	if _, err := db.Exec("PRAGMA wal_autocheckpoint=0;"); err != nil {
		t.Fatalf("禁用自动 checkpoint 失败: %v", err)
	}

	// 4. 写入基线数据并强制截断 checkpoint
	if _, err := db.Exec("CREATE TABLE accounts (id TEXT PRIMARY KEY, name TEXT);"); err != nil {
		t.Fatalf("创建基线表失败: %v", err)
	}
	if _, err := db.Exec("INSERT INTO accounts VALUES ('acc_base', 'Base Account');"); err != nil {
		t.Fatalf("写入基线数据失败: %v", err)
	}
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE);"); err != nil {
		t.Fatalf("checkpoint 截断失败: %v", err)
	}

	// 5. 写入一条新 committed 数据，使其明确位于 WAL 文件中且未 checkpoint 回主 db
	if _, err := db.Exec("INSERT INTO accounts VALUES ('acc_wal_fresh', 'Committed In WAL');"); err != nil {
		t.Fatalf("写入 WAL 数据失败: %v", err)
	}

	// 验证 WAL 文件存在且有内容
	walFi, err := os.Stat(srcPath + "-wal")
	if err != nil || walFi.Size() == 0 {
		t.Fatalf("WAL 文件未生成或大小为 0: %v", err)
	}

	// 6. 保持源数据库有效（保持长连接或正常打开）
	// 7. 调用 release validation snapshot helper
	snapPath := filepath.Join(tempDir, "target_snapshot.db")
	if err := CreateConsistentSnapshot(srcPath, snapPath); err != nil {
		t.Fatalf("CreateConsistentSnapshot 失败: %v", err)
	}

	// 8. 打开生成的 snapshot
	snapDB, err := sql.Open("sqlite", snapPath)
	if err != nil {
		t.Fatalf("打开生成的快照失败: %v", err)
	}
	defer snapDB.Close()

	// 9. 必须能看到 WAL 中刚刚 committed 的数据
	var count int
	if err := snapDB.QueryRow("SELECT COUNT(*) FROM accounts;").Scan(&count); err != nil {
		t.Fatalf("查询快照总数失败: %v", err)
	}
	if count != 2 {
		t.Fatalf("快照数据丢失! 预期包含基线与未 checkpoint 的 WAL 数据共 2 条，实际: %d", count)
	}

	var walAccName string
	if err := snapDB.QueryRow("SELECT name FROM accounts WHERE id = 'acc_wal_fresh';").Scan(&walAccName); err != nil {
		t.Fatalf("快照中未能查询到 WAL 中的 committed 数据: %v", err)
	}
	if walAccName != "Committed In WAL" {
		t.Fatalf("快照中的 WAL 数据内容损坏: %s", walAccName)
	}

	// 10. 确认源数据库在 snapshot 过程前后完全未被修改
	var srcCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM accounts;").Scan(&srcCount); err != nil {
		t.Fatalf("查询源数据库失败: %v", err)
	}
	if srcCount != 2 {
		t.Fatalf("源数据库行数被篡改! 预期 2，实际: %d", srcCount)
	}
	_ = db.Close()
}

// TestReleaseValidation_QueryFailureCannotPass 验证当遇到损坏或缺失必要字段的非法 schema 时，
// 任何 SQL 报错均必须 fail-closed 判定为 NOT_READY，绝不能误判为 VALIDATION_PASSED。
func TestReleaseValidation_QueryFailureCannotPass(t *testing.T) {
	tempDir := t.TempDir()
	corruptDBPath := filepath.Join(tempDir, "corrupt_schema.db")

	db, err := sql.Open("sqlite", corruptDBPath)
	if err != nil {
		t.Fatalf("创建数据库失败: %v", err)
	}

	// 故意创建破损的 alias_inventory 表：缺少 allocation_state 字段
	// 当 Check 3 执行 SELECT COUNT(*) FROM alias_inventory WHERE allocation_state = 'available' 时必定触发 SQL error
	corruptDDL := `
		CREATE TABLE accounts (id TEXT PRIMARY KEY, name TEXT);
		CREATE TABLE alias_inventory (
			email TEXT PRIMARY KEY,
			account_id TEXT
			-- 故意缺失 allocation_state
		);
	`
	if _, err := db.Exec(corruptDDL); err != nil {
		t.Fatalf("初始化测试数据失败: %v", err)
	}
	_ = db.Close()

	// 执行完整验收巡检
	passed, err := RunValidation(corruptDBPath)
	// 无论返回 err 还是 passed=false，系统都绝对不能判定为通过
	if passed {
		t.Fatalf("致命错误: 存在 SQL 执行失败的损坏数据库竟然返回了 VALIDATION_PASSED (Fail Closed 失效)!")
	}
	if err == nil && passed {
		t.Fatalf("必须返回 NOT_READY 或 error")
	}
}
