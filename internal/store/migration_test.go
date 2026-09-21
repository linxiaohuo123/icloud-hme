/**
 * [INPUT]: 依赖 testing, database/sql, path/filepath, internal/store
 * [OUTPUT]: 提供 MIG01~MIG06 迁移与 DDL 拓扑顺序、幂等性及历史所有权安全映射单测
 * [POS]: internal/store 的迁移验证套件 (PR-05-1 Correctness Gate)
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// MIG01: 空库迁移生成完整 DDL 与初始版本
func TestMIG01_EmptyDBInit(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer st.Close()

	db, err := sql.Open("sqlite", filepath.Join(dir, "icloud_hme.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	tables := []string{"alias_inventory", "alias_allocations", "operations", "verification_requests", "lease_records", "alias_routes", "api_tokens"}
	for _, tbl := range tables {
		var count int
		err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", tbl).Scan(&count)
		if err != nil || count != 1 {
			t.Fatalf("MIG01 失败: 表 %s 未生成", tbl)
		}
	}
}

// MIG02: 含 legacy lease_records 库迁移，无歧义 token 成功关联 token_id
func TestMIG02_LegacyLeasesUnambiguousToken(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "icloud_hme.db")
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// 模拟旧库结构与数据
	_, err = rawDB.Exec(`
		CREATE TABLE api_tokens (id TEXT PRIMARY KEY, name TEXT, token TEXT, created_at TEXT, scopes TEXT);
		CREATE TABLE lease_records (id TEXT PRIMARY KEY, email TEXT, account_id TEXT, tag TEXT, status TEXT, allocated_at TEXT, token_name TEXT);
		INSERT INTO api_tokens (id, name, token, created_at, scopes) VALUES ('tok_bot1', 'faka_bot', 'secret', '2026-01-01T00:00:00Z', 'allocate');
		INSERT INTO lease_records (id, email, account_id, tag, status, allocated_at, token_name) VALUES ('lease_1', 'user1@icloud.com', 'acc_1', 'chatgpt', 'completed', '2026-01-01T00:00:00Z', 'faka_bot');
	`)
	if err != nil {
		t.Fatal(err)
	}
	rawDB.Close()

	st, err := NewStore(dir)
	if err != nil {
		t.Fatalf("加载旧库失败: %v", err)
	}
	defer st.Close()

	// 验证迁移结果
	rawDB, _ = sql.Open("sqlite", dbPath)
	defer rawDB.Close()

	var ownerKind, ownerID string
	err = rawDB.QueryRow(`SELECT owner_kind, owner_id FROM alias_allocations WHERE alias_email = 'user1@icloud.com'`).Scan(&ownerKind, &ownerID)
	if err != nil {
		t.Fatalf("MIG02 失败: 查询 alias_allocations 失败: %v", err)
	}
	if ownerKind != "token" || ownerID != "tok_bot1" {
		t.Fatalf("MIG02 失败: 期望关联 tok_bot1, 实际 ownerKind=%s ownerID=%s", ownerKind, ownerID)
	}
}

// MIG03: 同名重名 token 迁移标记为 legacy_unknown，禁止跨 token 越权
func TestMIG03_DuplicateTokenNamesMappedToLegacyUnknown(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "icloud_hme.db")
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}

	_, err = rawDB.Exec(`
		CREATE TABLE api_tokens (id TEXT PRIMARY KEY, name TEXT, token TEXT, created_at TEXT, scopes TEXT);
		CREATE TABLE lease_records (id TEXT PRIMARY KEY, email TEXT, account_id TEXT, tag TEXT, status TEXT, allocated_at TEXT, token_name TEXT);
		-- 写入两个重名 token
		INSERT INTO api_tokens (id, name, token, created_at, scopes) VALUES ('tok_1', 'faka_bot', 'sec1', '2026-01-01T00:00:00Z', 'allocate');
		INSERT INTO api_tokens (id, name, token, created_at, scopes) VALUES ('tok_2', 'faka_bot', 'sec2', '2026-01-02T00:00:00Z', 'allocate');
		-- 写入出号流水
		INSERT INTO lease_records (id, email, account_id, tag, status, allocated_at, token_name) VALUES ('lease_dup', 'dup@icloud.com', 'acc_1', 'chatgpt', 'completed', '2026-01-01T00:00:00Z', 'faka_bot');
		INSERT INTO lease_records (id, email, account_id, tag, status, allocated_at, token_name) VALUES ('lease_sched', 'sched@icloud.com', 'acc_1', 'chatgpt', 'completed', '2026-01-01T00:00:00Z', 'scheduler');
	`)
	if err != nil {
		t.Fatal(err)
	}
	rawDB.Close()

	st, err := NewStore(dir)
	if err != nil {
		t.Fatalf("加载旧库失败: %v", err)
	}
	defer st.Close()

	rawDB, _ = sql.Open("sqlite", dbPath)
	defer rawDB.Close()

	var ownerIDDup, ownerIDSched string
	_ = rawDB.QueryRow(`SELECT owner_id FROM alias_allocations WHERE alias_email = 'dup@icloud.com'`).Scan(&ownerIDDup)
	_ = rawDB.QueryRow(`SELECT owner_id FROM alias_allocations WHERE alias_email = 'sched@icloud.com'`).Scan(&ownerIDSched)

	if ownerIDDup != "legacy_unknown" {
		t.Fatalf("MIG03 失败: 重名 token 应当降级为 legacy_unknown, 实际: %s", ownerIDDup)
	}
	if ownerIDSched != "legacy_unknown" {
		t.Fatalf("MIG03 失败: scheduler 应当降级为 legacy_unknown, 实际: %s", ownerIDSched)
	}
}

// MIG04: 仅含 alias_routes 的别名迁移为 allocation_state='unknown'，禁止直接设为 available
func TestMIG04_RoutesOnlyMigratedAsUnknown(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "icloud_hme.db")
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}

	_, err = rawDB.Exec(`
		CREATE TABLE alias_routes (email TEXT PRIMARY KEY, account_id TEXT, updated_at TEXT);
		INSERT INTO alias_routes (email, account_id, updated_at) VALUES ('route_only@icloud.com', 'acc_1', '2026-01-01T00:00:00Z');
	`)
	if err != nil {
		t.Fatal(err)
	}
	rawDB.Close()

	st, err := NewStore(dir)
	if err != nil {
		t.Fatalf("加载旧库失败: %v", err)
	}
	defer st.Close()

	rawDB, _ = sql.Open("sqlite", dbPath)
	defer rawDB.Close()

	var allocState string
	err = rawDB.QueryRow(`SELECT allocation_state FROM alias_inventory WHERE email = 'route_only@icloud.com'`).Scan(&allocState)
	if err != nil {
		t.Fatalf("MIG04 失败: 查询 alias_inventory 失败: %v", err)
	}
	if allocState != "unknown" {
		t.Fatalf("MIG04 失败: 仅含 routes 的别名必须为 unknown, 实际: %s", allocState)
	}
}

// MIG05: 重复执行迁移幂等，不破坏既有数据与状态
func TestMIG05_MigrationIdempotency(t *testing.T) {
	dir := t.TempDir()
	st1, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = st1.SaveToken(APIToken{Name: "bot", Token: "tok-123"})
	st1.Close()

	// 再次打开执行重复迁移
	st2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("MIG05 失败: 重复加载 Store 报错: %v", err)
	}
	defer st2.Close()

	tokens := st2.ListTokens()
	if len(tokens) != 1 || tokens[0].Name != "bot" {
		t.Fatalf("MIG05 失败: 重复迁移破坏原有数据")
	}
}

// MIG06: DDL 初始化执行顺序严格按依赖拓扑，无表不存在异常
func TestMIG06_DDLInitializationTopology(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(dir)
	if err != nil {
		t.Fatalf("MIG06 失败: DDL 执行拓扑异常: %v", err)
	}
	defer st.Close()

	// 确认核心表关联完整且索引就绪
	db, err := sql.Open("sqlite", filepath.Join(dir, "icloud_hme.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var idxCount int
	err = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_alias_inv_alloc_state'`).Scan(&idxCount)
	if err != nil || idxCount != 1 {
		t.Fatalf("MIG06 失败: 认领状态索引 idx_alias_inv_alloc_state 未正确创建")
	}
}
