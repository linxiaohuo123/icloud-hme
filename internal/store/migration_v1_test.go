/**
 * [INPUT]: 依赖 testing, database/sql, path/filepath, strings, os, fmt, internal/store
 * [OUTPUT]: 提供 MIG07 至 MIG11 迁移架构版本基线、原子回滚、前置备份、新老 Schema 等价性单测
 * [POS]: internal/store 的版本化架构演进验证套件 (PR-06 Correctness Gate)
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// MIG07: 拒绝未来版本的数据库启动
func TestMIG07_RejectFutureSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "icloud_hme.db")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 99;"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	// 期望：NewStore 拒绝启动并返回包含 version 的明确错误
	st, err := NewStore(dir)
	if err == nil {
		st.Close()
		t.Fatalf("MIG07 失败: NewStore 未能拒绝未来版本的数据库 (user_version=99)")
	}
	expectedSub := "newer than supported version"
	if !strings.Contains(err.Error(), expectedSub) {
		t.Fatalf("MIG07 失败: 错误信息未包含指定契约 '%s': %v", expectedSub, err)
	}
}

// MIG08: 迁移故障时事务完整回滚，不留半迁移脏状态
func TestMIG08_FailedMigrationRollsBackCompletely(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "icloud_hme.db")

	// 构造真实旧 schema (v0, 缺少 token_name, 且 user_version=0)
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	initDDL := `
		CREATE TABLE lease_records (
			id TEXT PRIMARY KEY,
			email TEXT NOT NULL,
			account_id TEXT NOT NULL,
			tag TEXT NOT NULL,
			status TEXT NOT NULL,
			allocated_at TEXT NOT NULL,
			completed_at TEXT
		);
		INSERT INTO lease_records VALUES ('lease_orig', 'orig@icloud.com', 'acc_1', 'tag_1', 'completed', '2026-01-01T00:00:00Z', NULL);
	`
	if _, err := rawDB.Exec(initDDL); err != nil {
		t.Fatal(err)
	}
	_ = rawDB.Close()

	// 注入确定性断点失败：在迁移中间步骤返回错误
	injectedErr := fmt.Errorf("injected fault at before_indexes")
	SetBeforeMigrationStepHookForTest(func(step string) error {
		if step == "before_indexes" {
			return injectedErr
		}
		return nil
	})
	defer SetBeforeMigrationStepHookForTest(nil)

	// 尝试加载 Store，期望返回注入的错误
	st, err := NewStore(dir)
	if err == nil {
		st.Close()
		t.Fatalf("MIG08 失败: 注入故障后 NewStore 竟然未报错")
	}

	// 验证回滚结果：
	// 1. user_version 仍必须为 0
	checkDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer checkDB.Close()

	var userVer int
	if err := checkDB.QueryRow("PRAGMA user_version").Scan(&userVer); err != nil {
		t.Fatalf("MIG08 失败: 查询 user_version 出错: %v", err)
	}
	if userVer != 0 {
		t.Fatalf("MIG08 失败: 迁移失败后 user_version 必须回滚至 0, 实际: %d", userVer)
	}

	// 2. 索引没有半截留下 (before_indexes 前尚未建索引)
	var idxCount int
	_ = checkDB.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_leases_allocated_at'").Scan(&idxCount)
	if idxCount != 0 {
		t.Fatalf("MIG08 失败: 索引不应留在数据库中 (应该随事务回滚)")
	}

	// 3. 原有行完整无损
	var count int
	_ = checkDB.QueryRow("SELECT COUNT(*) FROM lease_records WHERE id='lease_orig'").Scan(&count)
	if count != 1 {
		t.Fatalf("MIG08 失败: 原有数据在回滚后丢失")
	}

	// 4. pre-migration 备份必须已生成存在
	backups, err := os.ReadDir(filepath.Join(dir, "backups"))
	if err != nil || len(backups) == 0 {
		t.Fatalf("MIG08 失败: 迁移前备份文件未生成: %v", err)
	}
	if !strings.HasPrefix(backups[0].Name(), fmt.Sprintf("pre-migrate-v0-to-v%d-", CurrentSchemaVersion)) {
		t.Fatalf("MIG08 失败: 备份文件名不符合规范: %s", backups[0].Name())
	}
	_ = checkDB.Close()

	// 清理注入挂钩后，再次启动必须能正常升级成功
	SetBeforeMigrationStepHookForTest(nil)
	stRetry, err := NewStore(dir)
	if err != nil {
		t.Fatalf("MIG08 失败: 移除故障挂钩后重试迁移失败: %v", err)
	}
	defer stRetry.Close()

	var finalVer int
	if err := stRetry.db.QueryRow("PRAGMA user_version").Scan(&finalVer); err != nil || finalVer != CurrentSchemaVersion {
		t.Fatalf("MIG08 失败: 恢复后版本号期望 %d, 实际: %d (err=%v)", CurrentSchemaVersion, finalVer, err)
	}
}

// MIG09: Legacy unversioned 库平滑迁移到 V1
func TestMIG09_LegacyUnversionedToV1(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "icloud_hme.db")

	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	legacyDDL := `
		CREATE TABLE lease_records (
			id TEXT PRIMARY KEY,
			email TEXT NOT NULL,
			account_id TEXT NOT NULL,
			tag TEXT NOT NULL,
			status TEXT NOT NULL,
			allocated_at TEXT NOT NULL,
			completed_at TEXT
		);
		CREATE TABLE alias_routes (
			email TEXT PRIMARY KEY,
			account_id TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		INSERT INTO lease_records VALUES ('lease_legacy_1', 'mig9@icloud.com', 'acc_1', 'tag_1', 'completed', '2026-01-01T00:00:00Z', NULL);
		INSERT INTO alias_routes VALUES ('route9@icloud.com', 'acc_1', '2026-01-01T00:00:00Z');
	`
	if _, err := rawDB.Exec(legacyDDL); err != nil {
		t.Fatal(err)
	}
	_ = rawDB.Close()

	st, err := NewStore(dir)
	if err != nil {
		t.Fatalf("MIG09 失败: 加载 legacy 库失败: %v", err)
	}
	defer st.Close()

	var ver int
	if err := st.db.QueryRow("PRAGMA user_version").Scan(&ver); err != nil || ver != CurrentSchemaVersion {
		t.Fatalf("MIG09 失败: user_version 期望为 %d, 实际: %d", CurrentSchemaVersion, ver)
	}

	// 验证回填与安全隔离:
	// lease_legacy_1 -> alias_allocations (owner_kind/id 为 legacy_unknown)
	var ownerKind, ownerID string
	err = st.db.QueryRow("SELECT owner_kind, owner_id FROM alias_allocations WHERE alias_email='mig9@icloud.com'").Scan(&ownerKind, &ownerID)
	if err != nil {
		t.Fatalf("MIG09 失败: 查询 alias_allocations 出错: %v", err)
	}
	if ownerKind != "legacy_unknown" || ownerID != "legacy_unknown" {
		t.Fatalf("MIG09 失败: 历史所有权未被隔离为 legacy_unknown, 实际: kind=%s, id=%s", ownerKind, ownerID)
	}

	// route9 -> alias_inventory (allocation_state 必须为 unknown)
	var allocState string
	err = st.db.QueryRow("SELECT allocation_state FROM alias_inventory WHERE email='route9@icloud.com'").Scan(&allocState)
	if err != nil {
		t.Fatalf("MIG09 失败: 查询 alias_inventory 出错: %v", err)
	}
	if allocState != "unknown" {
		t.Fatalf("MIG09 失败: 仅有 route 的别名状态必须为 unknown, 实际: %s", allocState)
	}
}

// MIG10: 迁移前自动生成一致性备份且正常启动不再重复备份
func TestMIG10_BackupBeforeMigration(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "icloud_hme.db")

	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.Exec("CREATE TABLE dummy (id INT); INSERT INTO dummy VALUES (42);"); err != nil {
		t.Fatal(err)
	}
	_ = rawDB.Close()

	// 首次启动：触发迁移
	st1, err := NewStore(dir)
	if err != nil {
		t.Fatalf("首次启动失败: %v", err)
	}
	st1.Close()

	backupDir := filepath.Join(dir, "backups")
	entries, err := os.ReadDir(backupDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("MIG10 失败: 期望恰好生成 1 个迁移备份, 实际: %d (err=%v)", len(entries), err)
	}

	bakFile := filepath.Join(backupDir, entries[0].Name())
	// 验证备份有效性
	bakDB, err := sql.Open("sqlite", bakFile+"?mode=ro")
	if err != nil {
		t.Fatalf("打开备份文件失败: %v", err)
	}
	defer bakDB.Close()
	var dummyVal int
	if err := bakDB.QueryRow("SELECT id FROM dummy").Scan(&dummyVal); err != nil || dummyVal != 42 {
		t.Fatalf("MIG10 失败: 备份数据损坏或缺失: val=%d, err=%v", dummyVal, err)
	}
	var bakVer int
	if err := bakDB.QueryRow("PRAGMA user_version").Scan(&bakVer); err != nil || bakVer != 0 {
		t.Fatalf("MIG10 失败: 迁移前快照的 user_version 必须为 0, 实际: %d", bakVer)
	}
	_ = bakDB.Close()

	// 二次启动：已是 v1，严禁重复生成备份
	st2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("二次启动失败: %v", err)
	}
	st2.Close()

	entries2, _ := os.ReadDir(backupDir)
	if len(entries2) != 1 {
		t.Fatalf("MIG10 失败: 正常启动后不应产生多余备份, 实际备份数量: %d", len(entries2))
	}
}

// schemaSnapshot 包含用于对比的数据库元数据
type schemaSnapshot struct {
	userVersion int
	tables      []string
	columns     map[string][]string // tableName -> []"colName:type:notnull:dflt:pk"
	indexes     map[string]string   // indexName -> sql
}

func extractSchemaSnapshot(t *testing.T, db *sql.DB) schemaSnapshot {
	t.Helper()
	var snap schemaSnapshot
	snap.columns = make(map[string][]string)
	snap.indexes = make(map[string]string)

	if err := db.QueryRow("PRAGMA user_version").Scan(&snap.userVersion); err != nil {
		t.Fatalf("query user_version failed: %v", err)
	}

	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		t.Fatalf("query tables failed: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var tbl string
		if err := rows.Scan(&tbl); err != nil {
			t.Fatal(err)
		}
		snap.tables = append(snap.tables, tbl)
	}
	_ = rows.Close()

	for _, tbl := range snap.tables {
		colRows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", tbl))
		if err != nil {
			t.Fatalf("table_info for %s failed: %v", tbl, err)
		}
		var cols []string
		for colRows.Next() {
			var cid int
			var name, ctype string
			var notnull, pk int
			var dfltValue any
			if err := colRows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
				t.Fatal(err)
			}
			cols = append(cols, fmt.Sprintf("%s:%s:%d:%v:%d", name, strings.ToUpper(ctype), notnull, dfltValue, pk))
		}
		_ = colRows.Close()
		sort.Strings(cols)
		snap.columns[tbl] = cols
	}

	idxRows, err := db.Query("SELECT name, COALESCE(sql, '') FROM sqlite_master WHERE type='index' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		t.Fatalf("query indexes failed: %v", err)
	}
	defer idxRows.Close()
	for idxRows.Next() {
		var name, sqlStr string
		if err := idxRows.Scan(&name, &sqlStr); err != nil {
			t.Fatal(err)
		}
		snap.indexes[name] = sqlStr
	}

	return snap
}

// MIG11: Fresh Install 与 Upgrade 必须得到严格等价的 Schema
func TestMIG11_FreshInstallEqualsMigratedSchema(t *testing.T) {
	// 路径 A: 空目录全新安装
	dirFresh := t.TempDir()
	stFresh, err := NewStore(dirFresh)
	if err != nil {
		t.Fatalf("Fresh install failed: %v", err)
	}
	snapFresh := extractSchemaSnapshot(t, stFresh.db)
	stFresh.Close()

	// 路径 B: 构造旧版 unversioned 数据库升级安装
	dirUpgrade := t.TempDir()
	dbPathUpgrade := filepath.Join(dirUpgrade, "icloud_hme.db")
	rawDB, err := sql.Open("sqlite", dbPathUpgrade)
	if err != nil {
		t.Fatal(err)
	}
	// 构造历史结构：历史真实 baseline 基础表 (v0 unversioned, 缺少操作表与后续增量字段)
	legacyInitDDL := `
		CREATE TABLE business_tags (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			tag TEXT NOT NULL UNIQUE,
			description TEXT,
			status TEXT DEFAULT 'active',
			created_at TEXT NOT NULL,
			last_assigned_at TEXT
		);
		CREATE TABLE api_tokens (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			token TEXT NOT NULL UNIQUE,
			created_at TEXT NOT NULL,
			last_used_at TEXT
		);
		CREATE TABLE lease_records (
			id TEXT PRIMARY KEY,
			email TEXT NOT NULL,
			account_id TEXT NOT NULL,
			tag TEXT NOT NULL,
			status TEXT NOT NULL,
			allocated_at TEXT NOT NULL,
			completed_at TEXT
		);
		CREATE TABLE schedules (
			account_id TEXT PRIMARY KEY,
			enabled INTEGER NOT NULL DEFAULT 0,
			hourly_quota INTEGER NOT NULL DEFAULT 5,
			current_hour_count INTEGER NOT NULL DEFAULT 0,
			last_hour_window INTEGER NOT NULL DEFAULT 0,
			last_run_at TEXT
		);
		CREATE TABLE settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE TABLE alias_routes (
			email TEXT PRIMARY KEY,
			account_id TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE TABLE accounts (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			real_email TEXT DEFAULT '',
			icloud_email TEXT DEFAULT '',
			cookies TEXT DEFAULT '{}',
			host TEXT DEFAULT 'icloud.com',
			service_url TEXT DEFAULT '',
			proxy TEXT DEFAULT '',
			app_password TEXT DEFAULT '',
			mailbox TEXT DEFAULT '',
			status TEXT DEFAULT 'pending',
			alias_total INTEGER DEFAULT 0,
			alias_active INTEGER DEFAULT 0,
			last_validated TEXT DEFAULT '',
			last_error TEXT DEFAULT '',
			created_at TEXT NOT NULL,
			tags TEXT DEFAULT '[]',
			updated_at TEXT NOT NULL
		);
	`
	if _, err := rawDB.Exec(legacyInitDDL); err != nil {
		t.Fatal(err)
	}
	_ = rawDB.Close()

	stMigrated, err := NewStore(dirUpgrade)
	if err != nil {
		t.Fatalf("Upgraded store init failed: %v", err)
	}
	snapMigrated := extractSchemaSnapshot(t, stMigrated.db)
	stMigrated.Close()

	// 1. 对比 user_version
	if snapFresh.userVersion != snapMigrated.userVersion {
		t.Fatalf("MIG11 失败: user_version 不匹配: fresh=%d, migrated=%d", snapFresh.userVersion, snapMigrated.userVersion)
	}

	// 2. 对比所有表
	if len(snapFresh.tables) != len(snapMigrated.tables) {
		t.Fatalf("MIG11 失败: 表数量不匹配: fresh=%d (%v), migrated=%d (%v)",
			len(snapFresh.tables), snapFresh.tables, len(snapMigrated.tables), snapMigrated.tables)
	}
	for i := range snapFresh.tables {
		if snapFresh.tables[i] != snapMigrated.tables[i] {
			t.Fatalf("MIG11 失败: 表清单不一致: fresh=%s, migrated=%s", snapFresh.tables[i], snapMigrated.tables[i])
		}
	}

	// 3. 对比每个表的字段结构
	for tbl, freshCols := range snapFresh.columns {
		migratedCols, exists := snapMigrated.columns[tbl]
		if !exists {
			t.Fatalf("MIG11 失败: 表 %s 在迁移库中缺失", tbl)
		}
		if len(freshCols) != len(migratedCols) {
			t.Fatalf("MIG11 失败: 表 %s 列数不匹配: fresh=%v, migrated=%v", tbl, freshCols, migratedCols)
		}
		for j := range freshCols {
			if freshCols[j] != migratedCols[j] {
				t.Fatalf("MIG11 失败: 表 %s 列结构不匹配: fresh=%s, migrated=%s", tbl, freshCols[j], migratedCols[j])
			}
		}
	}

	// 4. 对比所有索引
	if len(snapFresh.indexes) != len(snapMigrated.indexes) {
		t.Fatalf("MIG11 失败: 索引数量不匹配: fresh=%d, migrated=%d", len(snapFresh.indexes), len(snapMigrated.indexes))
	}
	for idxName := range snapFresh.indexes {
		if _, exists := snapMigrated.indexes[idxName]; !exists {
			t.Fatalf("MIG11 失败: 索引 %s 在迁移库中缺失", idxName)
		}
	}
}

// MIG12: 验证关键 UNIQUE 契约在无约束历史库升级时被强制补全且生效
func TestMIG12_CriticalUniqueConstraintsEnforced(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "icloud_hme.db")
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// 构造 user_version=0 的 operations 表：包含所有字段，但故意没有 UNIQUE 约束与 UNIQUE 索引
	v0DDL := `
		CREATE TABLE operations (
			operation_id TEXT PRIMARY KEY,
			principal_kind TEXT NOT NULL,
			principal_id TEXT NOT NULL,
			operation_kind TEXT NOT NULL,
			idempotency_key TEXT NOT NULL,
			request_hash TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL,
			candidate_email TEXT DEFAULT '',
			result_ref TEXT DEFAULT '',
			error_code TEXT DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE TABLE api_tokens (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			token TEXT NOT NULL,
			created_at TEXT NOT NULL,
			last_used_at TEXT
		);
		PRAGMA user_version = 0;
	`
	if _, err := rawDB.Exec(v0DDL); err != nil {
		t.Fatal(err)
	}

	// 写入无重复的一条数据
	insertOp := `INSERT INTO operations (operation_id, principal_kind, principal_id, operation_kind, idempotency_key, state, created_at, updated_at)
		VALUES ('op_1', 'token', 'tok_1', 'allocate', 'idem_key_1', 'succeeded', '2026-09-24T00:00:00Z', '2026-09-24T00:00:00Z')`
	if _, err := rawDB.Exec(insertOp); err != nil {
		t.Fatal(err)
	}
	_ = rawDB.Close()

	// 启动 NewStore 执行升级
	st, err := NewStore(dir)
	if err != nil {
		t.Fatalf("MIG12 失败: 历史库升级失败: %v", err)
	}
	defer st.Close()

	// 1. 验证 user_version 升为当前版本
	var v int
	if err := st.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != CurrentSchemaVersion {
		t.Fatalf("MIG12 失败: user_version 未升至 %d: %d (%v)", CurrentSchemaVersion, v, err)
	}

	// 2. 验证 UNIQUE 存在
	hasUQ, err := hasUniqueConstraint(st.db, "operations", []string{"principal_kind", "principal_id", "operation_kind", "idempotency_key"})
	if err != nil || !hasUQ {
		t.Fatalf("MIG12 失败: operations 关键 UNIQUE 契约缺失: hasUQ=%v, err=%v", hasUQ, err)
	}

	// 3. 尝试插入第二条相同 idempotency tuple 的记录，必须发生 SQLite 约束违背
	duplicateInsert := `INSERT INTO operations (operation_id, principal_kind, principal_id, operation_kind, idempotency_key, state, created_at, updated_at)
		VALUES ('op_2', 'token', 'tok_1', 'allocate', 'idem_key_1', 'failed', '2026-09-24T00:01:00Z', '2026-09-24T00:01:00Z')`
	_, insertErr := st.db.Exec(duplicateInsert)
	if insertErr == nil {
		t.Fatalf("MIG12 失败: 插入重复 idempotency tuple 成功，UNIQUE 约束未生效")
	}
	if !strings.Contains(insertErr.Error(), "UNIQUE") && !strings.Contains(insertErr.Error(), "constraint") {
		t.Fatalf("MIG12 失败: 错误非预期约束错误: %v", insertErr)
	}
}

// MIG13: 验证历史数据存在重复冲突时升级 fail closed，回滚事务且保留所有数据与预迁移备份
func TestMIG13_DuplicateLegacyRowsFailClosed(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "icloud_hme.db")
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// 构造 v0 库，operations 没有 unique 约束，并且故意写入两条相同 idempotency tuple 的数据
	v0DDL := `
		CREATE TABLE operations (
			operation_id TEXT PRIMARY KEY,
			principal_kind TEXT NOT NULL,
			principal_id TEXT NOT NULL,
			operation_kind TEXT NOT NULL,
			idempotency_key TEXT NOT NULL,
			request_hash TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL,
			candidate_email TEXT DEFAULT '',
			result_ref TEXT DEFAULT '',
			error_code TEXT DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		PRAGMA user_version = 0;
	`
	if _, err := rawDB.Exec(v0DDL); err != nil {
		t.Fatal(err)
	}

	ins1 := `INSERT INTO operations (operation_id, principal_kind, principal_id, operation_kind, idempotency_key, state, created_at, updated_at)
		VALUES ('op_dup_1', 'token', 'tok_shared', 'allocate', 'same_idem_key', 'succeeded', '2026-09-24T00:00:00Z', '2026-09-24T00:00:00Z')`
	ins2 := `INSERT INTO operations (operation_id, principal_kind, principal_id, operation_kind, idempotency_key, state, created_at, updated_at)
		VALUES ('op_dup_2', 'token', 'tok_shared', 'allocate', 'same_idem_key', 'failed', '2026-09-24T00:01:00Z', '2026-09-24T00:01:00Z')`
	if _, err := rawDB.Exec(ins1); err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.Exec(ins2); err != nil {
		t.Fatal(err)
	}
	_ = rawDB.Close()

	// 启动 NewStore 执行升级，预期必须失败 (fail closed)
	st, err := NewStore(dir)
	if err == nil {
		st.Close()
		t.Fatalf("MIG13 失败: 存在重复记录时 NewStore 预期报错，但返回成功")
	}

	// 重新通过原生连接检查数据库状态
	checkDB, err := sql.Open("sqlite", filepath.ToSlash(dbPath)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer checkDB.Close()

	// 1. user_version 仍为 0
	var v int
	if err := checkDB.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != 0 {
		t.Fatalf("MIG13 失败: user_version 未保持 0: %d (%v)", v, err)
	}

	// 2. 原两条 rows 都还在，没有静默删除
	var rowCount int
	if err := checkDB.QueryRow("SELECT COUNT(*) FROM operations WHERE idempotency_key = 'same_idem_key'").Scan(&rowCount); err != nil {
		t.Fatal(err)
	}
	if rowCount != 2 {
		t.Fatalf("MIG13 失败: 重复记录被静默删除或修改，期望 2 行，实际: %d 行", rowCount)
	}

	// 3. pre-migration backup 存在
	backupsDir := filepath.Join(dir, "backups")
	entries, err := os.ReadDir(backupsDir)
	if err != nil {
		t.Fatalf("读取 backups 目录失败: %v", err)
	}
	foundPreMigration := false
	for _, entry := range entries {
		if (strings.HasPrefix(entry.Name(), "pre-migrate-") || strings.HasPrefix(entry.Name(), "pre-migration-")) && strings.HasSuffix(entry.Name(), ".db") {
			foundPreMigration = true
			break
		}
	}
	if !foundPreMigration {
		t.Fatalf("MIG13 失败: 未找到 pre-migration 备份文件")
	}
}
