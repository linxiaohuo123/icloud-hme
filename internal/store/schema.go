/**
 * [INPUT]: 依赖 database/sql, fmt, strings, time, sync
 * [OUTPUT]: 对外提供 CurrentSchemaVersion, schemaExecutor 接口, migrateV0ToV1, validateSchema, SetBeforeMigrationStepHookForTest
 * [POS]: internal/store 的版本化架构演进与元数据校验层 (PR-06 Baseline)
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"
)

// CurrentSchemaVersion 数据库正式版本基线 (PR-07 版本为 2)
const CurrentSchemaVersion = 2

type schemaExecutor interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

var (
	migrationHookMu                sync.RWMutex
	beforeMigrationStepHookForTest func(step string) error
)

// SetBeforeMigrationStepHookForTest 设置迁移断点注入挂钩 (仅用于单元测试中的确定性故障注入)
func SetBeforeMigrationStepHookForTest(hook func(step string) error) {
	migrationHookMu.Lock()
	defer migrationHookMu.Unlock()
	beforeMigrationStepHookForTest = hook
}

// MigrateV0ToV1ForTest 仅供测试使用: 构造标准完整的 V1 数据库
func MigrateV0ToV1ForTest(tx *sql.Tx) error {
	return migrateV0ToV1(tx)
}

func callMigrationStepHook(step string) error {
	migrationHookMu.RLock()
	hook := beforeMigrationStepHookForTest
	migrationHookMu.RUnlock()
	if hook != nil {
		return hook(step)
	}
	return nil
}

func getUserVersion(db *sql.DB) (int, error) {
	var v int
	err := db.QueryRow("PRAGMA user_version").Scan(&v)
	return v, err
}

func tableHasColumn(exec schemaExecutor, tableName, colName string) (bool, error) {
	rows, err := exec.Query(fmt.Sprintf("PRAGMA table_info(%s)", tableName))
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dfltValue any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err == nil {
			if name == colName {
				return true, nil
			}
		}
	}
	return false, rows.Err()
}

func ensureColumn(exec schemaExecutor, tableName, colName, colDef string) error {
	has, err := tableHasColumn(exec, tableName, colName)
	if err != nil {
		return fmt.Errorf("check column %s.%s failed: %w", tableName, colName, err)
	}
	if !has {
		query := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", tableName, colName, colDef)
		if _, err := exec.Exec(query); err != nil {
			return fmt.Errorf("add column %s.%s failed: %w", tableName, colName, err)
		}
	}
	return nil
}

// hasUniqueConstraint 检查指定表是否具有覆盖指定列且列顺序完全匹配的 UNIQUE 约束或索引
func hasUniqueConstraint(exec schemaExecutor, table string, columns []string) (bool, error) {
	rows, err := exec.Query(fmt.Sprintf("PRAGMA index_list(%s)", table))
	if err != nil {
		return false, fmt.Errorf("query index_list for %s failed: %w", table, err)
	}
	defer rows.Close()

	var uniqueIndexes []string
	for rows.Next() {
		var seq int
		var name string
		var unique int
		var origin string
		var partial int
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			return false, fmt.Errorf("scan index_list for %s failed: %w", table, err)
		}
		if unique == 1 {
			uniqueIndexes = append(uniqueIndexes, name)
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate index_list for %s failed: %w", table, err)
	}

	for _, idxName := range uniqueIndexes {
		infoRows, err := exec.Query(fmt.Sprintf("PRAGMA index_info('%s')", strings.ReplaceAll(idxName, "'", "''")))
		if err != nil {
			return false, fmt.Errorf("query index_info for %s failed: %w", idxName, err)
		}
		var idxCols []string
		for infoRows.Next() {
			var seqno, cid int
			var colName string
			if err := infoRows.Scan(&seqno, &cid, &colName); err != nil {
				infoRows.Close()
				return false, fmt.Errorf("scan index_info for %s failed: %w", idxName, err)
			}
			idxCols = append(idxCols, colName)
		}
		infoRows.Close()
		if err := infoRows.Err(); err != nil {
			return false, fmt.Errorf("iterate index_info for %s failed: %w", idxName, err)
		}

		if len(idxCols) == len(columns) {
			match := true
			for i := range columns {
				if !strings.EqualFold(idxCols[i], columns[i]) {
					match = false
					break
				}
			}
			if match {
				return true, nil
			}
		}
	}

	// 检查主键 (兼容 INTEGER PRIMARY KEY 或未在 index_list 显式列出的主键)
	pkRows, err := exec.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, fmt.Errorf("query table_info for %s failed: %w", table, err)
	}
	defer pkRows.Close()

	type pkCol struct {
		name string
		pk   int
	}
	var pkCols []pkCol
	for pkRows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := pkRows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, fmt.Errorf("scan table_info for %s failed: %w", table, err)
		}
		if pk > 0 {
			pkCols = append(pkCols, pkCol{name: name, pk: pk})
		}
	}
	if err := pkRows.Err(); err != nil {
		return false, fmt.Errorf("iterate table_info for %s failed: %w", table, err)
	}

	if len(pkCols) == len(columns) {
		for i := 0; i < len(pkCols)-1; i++ {
			for j := i + 1; j < len(pkCols); j++ {
				if pkCols[i].pk > pkCols[j].pk {
					pkCols[i], pkCols[j] = pkCols[j], pkCols[i]
				}
			}
		}
		match := true
		for i := range columns {
			if !strings.EqualFold(pkCols[i].name, columns[i]) {
				match = false
				break
			}
		}
		if match {
			return true, nil
		}
	}

	return false, nil
}

// migrateV0ToV1 事务化执行 Version 0 (Legacy/Unversioned) -> Version 1 的全部表结构、字段补充、索引与数据回填
func migrateV0ToV1(tx *sql.Tx) error {
	if err := callMigrationStepHook("before_base_tables"); err != nil {
		return err
	}

	// 1. 创建基础表 (如果表尚不存在)
	baseDDL := `
	CREATE TABLE IF NOT EXISTS business_tags (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		tag TEXT NOT NULL UNIQUE,
		description TEXT,
		status TEXT DEFAULT 'active',
		created_at TEXT NOT NULL,
		last_assigned_at TEXT
	);

	CREATE TABLE IF NOT EXISTS api_tokens (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		token TEXT NOT NULL UNIQUE,
		created_at TEXT NOT NULL,
		last_used_at TEXT,
		scopes TEXT NOT NULL DEFAULT 'admin'
	);

	CREATE TABLE IF NOT EXISTS lease_records (
		id TEXT PRIMARY KEY,
		email TEXT NOT NULL,
		account_id TEXT NOT NULL,
		tag TEXT NOT NULL,
		status TEXT NOT NULL,
		allocated_at TEXT NOT NULL,
		completed_at TEXT,
		token_name TEXT DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS schedules (
		account_id TEXT PRIMARY KEY,
		enabled INTEGER NOT NULL DEFAULT 0,
		hourly_quota INTEGER NOT NULL DEFAULT 5,
		current_hour_count INTEGER NOT NULL DEFAULT 0,
		last_hour_window INTEGER NOT NULL DEFAULT 0,
		last_run_at TEXT,
		alias_label TEXT DEFAULT 'scheduled',
		mode TEXT DEFAULT 'always',
		start_time TEXT DEFAULT '',
		end_time TEXT DEFAULT '',
		duration_hours INTEGER DEFAULT 0,
		started_at TEXT DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS alias_routes (
		email      TEXT PRIMARY KEY,
		account_id TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS accounts (
		id            TEXT PRIMARY KEY,
		name          TEXT NOT NULL DEFAULT '',
		real_email    TEXT DEFAULT '',
		icloud_email  TEXT DEFAULT '',
		cookies       TEXT DEFAULT '{}',
		host          TEXT DEFAULT 'icloud.com',
		service_url   TEXT DEFAULT '',
		proxy         TEXT DEFAULT '',
		app_password  TEXT DEFAULT '',
		mailbox       TEXT DEFAULT '',
		status        TEXT DEFAULT 'pending',
		alias_total   INTEGER DEFAULT 0,
		alias_active  INTEGER DEFAULT 0,
		last_validated TEXT DEFAULT '',
		last_error    TEXT DEFAULT '',
		created_at    TEXT NOT NULL,
		tags          TEXT DEFAULT '[]',
		updated_at    TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS alias_inventory (
		email TEXT PRIMARY KEY,
		account_id TEXT NOT NULL,
		provider_alias_id TEXT DEFAULT '',
		remote_state TEXT NOT NULL DEFAULT 'unknown',
		allocation_state TEXT NOT NULL DEFAULT 'unknown',
		source_type TEXT NOT NULL DEFAULT 'legacy_unknown',
		last_verified_at TEXT,
		snapshot_version INTEGER NOT NULL DEFAULT 1
	);

	CREATE TABLE IF NOT EXISTS alias_allocations (
		allocation_id TEXT PRIMARY KEY,
		alias_email TEXT NOT NULL UNIQUE,
		account_id TEXT NOT NULL DEFAULT '',
		owner_kind TEXT NOT NULL,
		owner_id TEXT NOT NULL,
		business_tag TEXT DEFAULT '',
		allocated_at TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'allocated'
	);

	CREATE TABLE IF NOT EXISTS operations (
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
		updated_at TEXT NOT NULL,
		CONSTRAINT uq_op_idempotency UNIQUE (principal_kind, principal_id, operation_kind, idempotency_key)
	);

	CREATE TABLE IF NOT EXISTS hme_reserve_intents (
		intent_id TEXT PRIMARY KEY,
		account_id TEXT NOT NULL,
		candidate_email TEXT NOT NULL,
		label TEXT DEFAULT '',
		state TEXT NOT NULL,
		anonymous_id TEXT DEFAULT '',
		result_ref TEXT DEFAULT '',
		error_message TEXT DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS verification_requests (
		request_id TEXT PRIMARY KEY,
		principal_kind TEXT NOT NULL,
		principal_id TEXT NOT NULL,
		lease_id TEXT NOT NULL,
		alias_email TEXT NOT NULL,
		status TEXT NOT NULL,
		created_at TEXT NOT NULL,
		expires_at TEXT NOT NULL,
		baseline_provider TEXT DEFAULT '',
		baseline_mailbox TEXT DEFAULT 'INBOX',
		baseline_uidvalidity INTEGER DEFAULT 0,
		baseline_uid INTEGER DEFAULT 0,
		matched_event_ref TEXT DEFAULT '',
		code TEXT DEFAULT '',
		magic_link TEXT DEFAULT ''
	);
	`
	if _, err := tx.Exec(baseDDL); err != nil {
		return fmt.Errorf("创建基础表结构失败: %w", err)
	}

	if err := callMigrationStepHook("before_ensure_columns"); err != nil {
		return err
	}

	// 2. 针对历史已存在但字段缺失的表执行列平滑补充
	colsToAdd := []struct {
		table string
		col   string
		def   string
	}{
		{"lease_records", "token_name", "TEXT DEFAULT ''"},
		{"api_tokens", "scopes", "TEXT NOT NULL DEFAULT 'admin'"},
		{"schedules", "alias_label", "TEXT DEFAULT 'scheduled'"},
		{"schedules", "mode", "TEXT DEFAULT 'always'"},
		{"schedules", "start_time", "TEXT DEFAULT ''"},
		{"schedules", "end_time", "TEXT DEFAULT ''"},
		{"schedules", "duration_hours", "INTEGER DEFAULT 0"},
		{"schedules", "started_at", "TEXT DEFAULT ''"},
		{"alias_inventory", "provider_alias_id", "TEXT DEFAULT ''"},
		{"alias_inventory", "remote_state", "TEXT NOT NULL DEFAULT 'unknown'"},
		{"alias_inventory", "allocation_state", "TEXT NOT NULL DEFAULT 'unknown'"},
		{"alias_inventory", "source_type", "TEXT NOT NULL DEFAULT 'legacy_unknown'"},
		{"alias_inventory", "last_verified_at", "TEXT"},
		{"alias_inventory", "snapshot_version", "INTEGER NOT NULL DEFAULT 1"},
		{"alias_allocations", "account_id", "TEXT NOT NULL DEFAULT ''"},
		{"alias_allocations", "business_tag", "TEXT DEFAULT ''"},
		{"alias_allocations", "status", "TEXT NOT NULL DEFAULT 'allocated'"},
		{"operations", "request_hash", "TEXT NOT NULL DEFAULT ''"},
		{"operations", "candidate_email", "TEXT DEFAULT ''"},
		{"operations", "result_ref", "TEXT DEFAULT ''"},
		{"operations", "error_code", "TEXT DEFAULT ''"},
		{"verification_requests", "baseline_provider", "TEXT DEFAULT ''"},
		{"verification_requests", "baseline_mailbox", "TEXT DEFAULT 'INBOX'"},
		{"verification_requests", "baseline_uidvalidity", "INTEGER DEFAULT 0"},
		{"verification_requests", "baseline_uid", "INTEGER DEFAULT 0"},
		{"verification_requests", "matched_event_ref", "TEXT DEFAULT ''"},
		{"verification_requests", "code", "TEXT DEFAULT ''"},
		{"verification_requests", "magic_link", "TEXT DEFAULT ''"},
	}

	for _, c := range colsToAdd {
		if err := ensureColumn(tx, c.table, c.col, c.def); err != nil {
			return err
		}
	}

	if err := callMigrationStepHook("before_backfill"); err != nil {
		return err
	}

	// 3. 所有关键 backfill 执行且严格校验错误，绝不静默吞错
	// 3.1 回填 alias_allocations.account_id (分别从 alias_inventory, alias_routes, lease_records 回填)
	qBackfillAllocInv := `
		UPDATE alias_allocations
		SET account_id = (
			SELECT account_id FROM alias_inventory WHERE alias_inventory.email = alias_allocations.alias_email
		)
		WHERE (account_id IS NULL OR account_id = '') AND EXISTS (
			SELECT 1 FROM alias_inventory WHERE alias_inventory.email = alias_allocations.alias_email AND alias_inventory.account_id != ''
		)
	`
	if _, err := tx.Exec(qBackfillAllocInv); err != nil {
		return fmt.Errorf("backfill alias_allocations.account_id from alias_inventory failed: %w", err)
	}

	qBackfillAllocRoutes := `
		UPDATE alias_allocations
		SET account_id = (
			SELECT account_id FROM alias_routes WHERE LOWER(TRIM(alias_routes.email)) = alias_allocations.alias_email
		)
		WHERE (account_id IS NULL OR account_id = '') AND EXISTS (
			SELECT 1 FROM alias_routes WHERE LOWER(TRIM(alias_routes.email)) = alias_allocations.alias_email AND alias_routes.account_id != ''
		)
	`
	if _, err := tx.Exec(qBackfillAllocRoutes); err != nil {
		return fmt.Errorf("backfill alias_allocations.account_id from alias_routes failed: %w", err)
	}

	qBackfillAllocLeases := `
		UPDATE alias_allocations
		SET account_id = (
			SELECT account_id FROM lease_records WHERE LOWER(TRIM(lease_records.email)) = alias_allocations.alias_email
		)
		WHERE (account_id IS NULL OR account_id = '') AND EXISTS (
			SELECT 1 FROM lease_records WHERE LOWER(TRIM(lease_records.email)) = alias_allocations.alias_email AND lease_records.account_id != ''
		)
	`
	if _, err := tx.Exec(qBackfillAllocLeases); err != nil {
		return fmt.Errorf("backfill alias_allocations.account_id from lease_records failed: %w", err)
	}

	// 3.2 回填库存与分配状态 (从 lease_records 与 alias_routes 导入)
	if err := migrateInventoryTx(tx); err != nil {
		return fmt.Errorf("migrate inventory failed: %w", err)
	}

	// 3.3 回填 alias_routes (从 lease_records 补充)
	if err := backfillAliasRoutesTx(tx); err != nil {
		return fmt.Errorf("backfill alias_routes failed: %w", err)
	}

	if err := callMigrationStepHook("before_indexes"); err != nil {
		return err
	}

	// 4. 创建所有正确性与查询加速索引
	indexesDDL := `
	CREATE INDEX IF NOT EXISTS idx_leases_allocated_at ON lease_records (allocated_at DESC);
	CREATE INDEX IF NOT EXISTS idx_leases_email ON lease_records (email);
	CREATE INDEX IF NOT EXISTS idx_leases_email_lower ON lease_records (LOWER(email));
	CREATE INDEX IF NOT EXISTS idx_leases_email_lower_token ON lease_records (LOWER(email), token_name);
	CREATE INDEX IF NOT EXISTS idx_leases_tag ON lease_records (tag);
	CREATE INDEX IF NOT EXISTS idx_leases_status ON lease_records (status);
	CREATE INDEX IF NOT EXISTS idx_alias_routes_account ON alias_routes (account_id);
	CREATE INDEX IF NOT EXISTS idx_accounts_status ON accounts(status);
	CREATE INDEX IF NOT EXISTS idx_alias_inv_acc_alloc ON alias_inventory (account_id, allocation_state);
	CREATE INDEX IF NOT EXISTS idx_alias_inv_alloc_state ON alias_inventory (allocation_state);
	CREATE INDEX IF NOT EXISTS idx_alias_alloc_owner ON alias_allocations (owner_kind, owner_id);
	CREATE INDEX IF NOT EXISTS idx_alias_alloc_email ON alias_allocations (alias_email);
	CREATE INDEX IF NOT EXISTS idx_operations_lookup ON operations (principal_kind, principal_id, operation_kind, idempotency_key);
	CREATE INDEX IF NOT EXISTS idx_vreq_principal ON verification_requests (principal_kind, principal_id);
	CREATE INDEX IF NOT EXISTS idx_vreq_lease ON verification_requests (lease_id);
	CREATE INDEX IF NOT EXISTS idx_vreq_email ON verification_requests (alias_email);
	CREATE INDEX IF NOT EXISTS idx_hme_intents_unresolved ON hme_reserve_intents (account_id, state);
	`
	if _, err := tx.Exec(indexesDDL); err != nil {
		return fmt.Errorf("创建索引失败: %w", err)
	}

	// 4.1 确保关键业务 UNIQUE Contract (如果缺失则创建明确的 UNIQUE INDEX；若存在历史重复数据则由 SQLite 约束直接 fail closed 回滚)
	criticalUniques := []struct {
		table   string
		index   string
		columns []string
	}{
		{
			table:   "operations",
			index:   "uq_operations_idempotency",
			columns: []string{"principal_kind", "principal_id", "operation_kind", "idempotency_key"},
		},
		{
			table:   "api_tokens",
			index:   "uq_api_tokens_token",
			columns: []string{"token"},
		},
		{
			table:   "business_tags",
			index:   "uq_business_tags_tag",
			columns: []string{"tag"},
		},
		{
			table:   "alias_allocations",
			index:   "uq_alias_allocations_alias_email",
			columns: []string{"alias_email"},
		},
	}

	for _, cu := range criticalUniques {
		has, err := hasUniqueConstraint(tx, cu.table, cu.columns)
		if err != nil {
			return fmt.Errorf("check unique constraint for %s failed: %w", cu.table, err)
		}
		if !has {
			q := fmt.Sprintf("CREATE UNIQUE INDEX %s ON %s (%s)", cu.index, cu.table, strings.Join(cu.columns, ", "))
			if _, err := tx.Exec(q); err != nil {
				return fmt.Errorf("create unique index %s on %s failed: %w", cu.index, cu.table, err)
			}
		}
	}

	if err := callMigrationStepHook("before_user_version"); err != nil {
		return err
	}

	// 5. 设置 user_version 为 1
	if _, err := tx.Exec("PRAGMA user_version = 1;"); err != nil {
		return fmt.Errorf("写入 schema user_version 失败: %w", err)
	}

	return nil
}

func migrateInventoryTx(tx *sql.Tx) error {
	// 1. 迁移已有流水：将已交付别名入库，标记为已分配 (allocated)
	qLeases := `
	INSERT INTO alias_inventory (email, account_id, remote_state, allocation_state, source_type, snapshot_version)
	SELECT LOWER(TRIM(email)), account_id, 'unknown', 'allocated', 'legacy_unknown', 1
	FROM lease_records
	WHERE TRIM(email) != ''
	ON CONFLICT(email) DO UPDATE SET
		allocation_state = 'allocated'
	`
	if _, err := tx.Exec(qLeases); err != nil {
		return fmt.Errorf("迁移 lease_records 到 alias_inventory 失败: %w", err)
	}

	// 2. 逐条迁移 lease_records 到 alias_allocations
	leaseRows, err := tx.Query(`SELECT id, LOWER(TRIM(email)), account_id, tag, allocated_at FROM lease_records WHERE TRIM(email) != ''`)
	if err != nil {
		return fmt.Errorf("query lease_records for migration failed: %w", err)
	}
	defer leaseRows.Close()

	allocStmt, err := tx.Prepare(`
		INSERT INTO alias_allocations (allocation_id, alias_email, account_id, owner_kind, owner_id, business_tag, allocated_at, status)
		VALUES (?, ?, ?, 'legacy_unknown', 'legacy_unknown', ?, ?, 'allocated')
		ON CONFLICT(alias_email) DO NOTHING
	`)
	if err != nil {
		return fmt.Errorf("prepare alias_allocations insert failed: %w", err)
	}
	defer allocStmt.Close()

	for leaseRows.Next() {
		var id, email, accountID, tag, allocatedAt string
		if err := leaseRows.Scan(&id, &email, &accountID, &tag, &allocatedAt); err != nil {
			return err
		}
		if _, err := allocStmt.Exec(id, email, accountID, tag, allocatedAt); err != nil {
			return err
		}
	}
	if err := leaseRows.Err(); err != nil {
		return err
	}

	// 3. 迁移 alias_routes 中未有流水记录的别名，默认 unknown (严禁直接设为 available)
	qRoutes := `
	INSERT INTO alias_inventory (email, account_id, remote_state, allocation_state, source_type, snapshot_version)
	SELECT LOWER(TRIM(email)), account_id, 'unknown', 'unknown', 'legacy_unknown', 1
	FROM alias_routes
	WHERE TRIM(email) != ''
	ON CONFLICT(email) DO NOTHING
	`
	if _, err := tx.Exec(qRoutes); err != nil {
		return fmt.Errorf("迁移 alias_routes 到 alias_inventory 失败: %w", err)
	}

	return nil
}

func backfillAliasRoutesTx(tx *sql.Tx) error {
	var backfilledVal string
	_ = tx.QueryRow(`SELECT value FROM settings WHERE key = 'alias_routes_backfilled'`).Scan(&backfilledVal)
	if backfilledVal == "1" {
		return nil
	}

	res, err := tx.Exec(`
		INSERT OR IGNORE INTO alias_routes (email, account_id, updated_at)
		SELECT LOWER(email), account_id, COALESCE(NULLIF(allocated_at, ''), datetime('now'))
		FROM lease_records
		WHERE account_id != '' AND email != ''`)
	if err != nil {
		return fmt.Errorf("backfill alias_routes from lease_records failed: %w", err)
	}
	_ = res

	nowStr := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.Exec(`INSERT INTO settings (key, value, updated_at) VALUES ('alias_routes_backfilled', '1', ?) ON CONFLICT(key) DO UPDATE SET value='1', updated_at=excluded.updated_at`, nowStr); err != nil {
		return fmt.Errorf("save settings alias_routes_backfilled failed: %w", err)
	}
	return nil
}

// validateSchema 校验数据库结构完整性
func validateSchema(db *sql.DB) error {
	// 1. 检查 user_version
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("schema validation failed: check user_version error: %w", err)
	}
	if v != CurrentSchemaVersion {
		return fmt.Errorf("schema validation failed: expected user_version %d, got %d", CurrentSchemaVersion, v)
	}

	// 2. 检查关键表
	requiredTables := []string{
		"accounts",
		"api_tokens",
		"business_tags",
		"lease_records",
		"schedules",
		"settings",
		"alias_routes",
		"alias_inventory",
		"alias_allocations",
		"operations",
		"hme_reserve_intents",
		"verification_requests",
	}
	for _, tbl := range requiredTables {
		var count int
		err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", tbl).Scan(&count)
		if err != nil || count == 0 {
			return fmt.Errorf("schema validation failed: table %s is missing", tbl)
		}
	}

	// 3. 检查关键列
	requiredCols := []struct {
		table string
		col   string
	}{
		{"accounts", "cookies"},
		{"accounts", "app_password"},
		{"accounts", "mailbox"},
		{"accounts", "status"},
		{"api_tokens", "token_hash"},
		{"api_tokens", "token_prefix"},
		{"api_tokens", "scopes"},
		{"api_tokens", "expires_at"},
		{"api_tokens", "revoked_at"},
		{"api_tokens", "rotated_at"},
		{"api_tokens", "needs_rotation"},
		{"lease_records", "token_name"},
		{"schedules", "alias_label"},
		{"schedules", "mode"},
		{"schedules", "start_time"},
		{"schedules", "end_time"},
		{"schedules", "duration_hours"},
		{"schedules", "started_at"},
		{"alias_allocations", "account_id"},
		{"alias_allocations", "alias_email"},
		{"operations", "request_hash"},
		{"operations", "idempotency_key"},
		{"hme_reserve_intents", "candidate_email"},
		{"verification_requests", "magic_link"},
		{"verification_requests", "baseline_uidvalidity"},
		{"verification_requests", "code"},
	}
	for _, rc := range requiredCols {
		has, err := tableHasColumn(db, rc.table, rc.col)
		if err != nil || !has {
			return fmt.Errorf("schema validation failed: column %s.%s is missing", rc.table, rc.col)
		}
	}

	// 3.1 安全隔离硬约束: api_tokens 表严禁存在 token 明文列 (PR-07)
	hasPlaintextTokenCol, err := tableHasColumn(db, "api_tokens", "token")
	if err != nil {
		return fmt.Errorf("schema validation failed: check api_tokens.token error: %w", err)
	}
	if hasPlaintextTokenCol {
		return fmt.Errorf("schema validation failed: api_tokens 表严禁保留明文 token 列")
	}

	// 4. 检查关键索引
	requiredIndexes := []string{
		"idx_leases_allocated_at",
		"idx_leases_email",
		"idx_leases_email_lower",
		"idx_leases_email_lower_token",
		"idx_leases_tag",
		"idx_leases_status",
		"idx_alias_routes_account",
		"idx_accounts_status",
		"idx_alias_inv_acc_alloc",
		"idx_alias_inv_alloc_state",
		"idx_alias_alloc_owner",
		"idx_alias_alloc_email",
		"idx_operations_lookup",
		"idx_vreq_principal",
		"idx_vreq_lease",
		"idx_vreq_email",
		"idx_hme_intents_unresolved",
		"idx_api_tokens_hash",
	}
	for _, idx := range requiredIndexes {
		var count int
		err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?", idx).Scan(&count)
		if err != nil || count == 0 {
			return fmt.Errorf("schema validation failed: index %s is missing", idx)
		}
	}

	// 5. 检查关键 UNIQUE 契约 (绝不在正常启动时自动重建，违背即判定 schema 受损并 fail closed)
	criticalUniques := []struct {
		table   string
		columns []string
	}{
		{"business_tags", []string{"tag"}},
		{"api_tokens", []string{"token_hash"}},
		{"alias_allocations", []string{"alias_email"}},
		{"operations", []string{"principal_kind", "principal_id", "operation_kind", "idempotency_key"}},
		{"alias_inventory", []string{"email"}},
		{"verification_requests", []string{"request_id"}},
		{"hme_reserve_intents", []string{"intent_id"}},
		{"alias_allocations", []string{"allocation_id"}},
		{"operations", []string{"operation_id"}},
	}
	for _, cu := range criticalUniques {
		has, err := hasUniqueConstraint(db, cu.table, cu.columns)
		if err != nil {
			return fmt.Errorf("schema validation failed: check unique constraint on %s(%s) error: %w",
				cu.table, strings.Join(cu.columns, ", "), err)
		}
		if !has {
			return fmt.Errorf("schema validation failed: unique constraint on %s(%s) is missing",
				cu.table, strings.Join(cu.columns, ", "))
		}
	}

	return nil
}
