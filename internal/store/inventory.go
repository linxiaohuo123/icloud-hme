/**
 * [INPUT]: 依赖 database/sql, fmt, errors, strings, time, icloud-hme/internal/hme
 * [OUTPUT]: 对外提供 AliasInventory, AliasAllocation, Operation 模型及 initInventorySchema, migrateInventory, AddInventoryAlias, GetInventoryAlias, SyncAliasInventory, CountAuthoritativeAvailableAliases, UpdateAliasRemoteState
 * [POS]: internal/store 的别名库存实体与迁移定义层，维护 alias_inventory, alias_allocations, operations 表结构与元数据
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"icloud-hme/internal/hme"
)

var (
	// ErrNoAvailableInventory 无可分配的空闲别名库存
	ErrNoAvailableInventory = errors.New("no available inventory")
	// ErrIdempotencyConflict 相同幂等键使用不同请求参数冲突
	ErrIdempotencyConflict = errors.New("idempotency conflict: request hash mismatch")
	// ErrOperationPending 幂等操作仍在执行中
	ErrOperationPending = errors.New("operation is pending")
	// ErrAllocationNotFound 未找到分配记录
	ErrAllocationNotFound = errors.New("allocation not found")
	// ErrAllocationConflict 别名已归属于其他主体或历史分配冲突
	ErrAllocationConflict = errors.New("allocation conflict: email already allocated")
	// ErrVerificationRequestNotFound 未找到取码任务记录
	ErrVerificationRequestNotFound = errors.New("verification request not found")
)

// RemoteState 远端上游真实状态
type RemoteState string

const (
	RemoteActive   RemoteState = "active"
	RemoteInactive RemoteState = "inactive"
	RemoteDeleted  RemoteState = "deleted"
	RemoteUnknown  RemoteState = "unknown"
)

// AllocationState 本地业务分配生命周期状态
type AllocationState string

const (
	AllocationUnknown     AllocationState = "unknown"
	AllocationAvailable   AllocationState = "available"
	AllocationAllocated   AllocationState = "allocated"
	AllocationQuarantined AllocationState = "quarantined"
)

// AliasInventory 别名库存实体
type AliasInventory struct {
	Email           string          `json:"email"`
	AccountID       string          `json:"account_id"`
	ProviderAliasID string          `json:"provider_alias_id"`
	RemoteState     RemoteState     `json:"remote_state"`
	AllocationState AllocationState `json:"allocation_state"`
	SourceType      string          `json:"source_type"` // "replenish", "manual", "on_demand", "legacy_unknown"
	LastVerifiedAt  string          `json:"last_verified_at"`
	SnapshotVersion int             `json:"snapshot_version"`
}

// AliasAllocation 别名发放唯一归属记录
type AliasAllocation struct {
	AllocationID string `json:"allocation_id"`
	AliasEmail   string `json:"alias_email"`
	AccountID    string `json:"account_id"`
	OwnerKind    string `json:"owner_kind"` // "token" 或 "admin"
	OwnerID      string `json:"owner_id"`   // token_id 或 "admin"
	BusinessTag  string `json:"business_tag"`
	AllocatedAt  string `json:"allocated_at"`
	Status       string `json:"status"` // "allocated", "released", "quarantined"
}

// Operation 外部幂等操作日志
type Operation struct {
	OperationID    string `json:"operation_id"`
	PrincipalKind  string `json:"principal_kind"`
	PrincipalID    string `json:"principal_id"`
	OperationKind  string `json:"operation_kind"`
	IdempotencyKey string `json:"idempotency_key"`
	RequestHash    string `json:"request_hash"`
	State          string `json:"state"` // "pending", "succeeded", "failed", "unknown"
	CandidateEmail string `json:"candidate_email"`
	ResultRef      string `json:"result_ref"`
	ErrorCode      string `json:"error_code"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

func ensureColumn(db *sql.DB, tableName, colName, colDef string) error {
	has, err := tableHasColumn(db, tableName, colName)
	if err != nil {
		return fmt.Errorf("check column %s.%s failed: %w", tableName, colName, err)
	}
	if !has {
		query := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", tableName, colName, colDef)
		if _, err := db.Exec(query); err != nil {
			return fmt.Errorf("add column %s.%s failed: %w", tableName, colName, err)
		}
	}
	return nil
}

func (s *Store) initInventorySchema() error {
	ddl := `
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
		code TEXT DEFAULT ''
	);
	`
	if _, err := s.db.Exec(ddl); err != nil {
		return err
	}

	// 1. alias_inventory 补列 (容错与自愈)
	invCols := [][2]string{
		{"provider_alias_id", "TEXT DEFAULT ''"},
		{"remote_state", "TEXT NOT NULL DEFAULT 'unknown'"},
		{"allocation_state", "TEXT NOT NULL DEFAULT 'unknown'"},
		{"source_type", "TEXT NOT NULL DEFAULT 'legacy_unknown'"},
		{"last_verified_at", "TEXT"},
		{"snapshot_version", "INTEGER NOT NULL DEFAULT 1"},
	}
	for _, col := range invCols {
		if err := ensureColumn(s.db, "alias_inventory", col[0], col[1]); err != nil {
			return err
		}
	}

	// 2. alias_allocations 补列与 backfill (MIG-01)
	allocCols := [][2]string{
		{"account_id", "TEXT NOT NULL DEFAULT ''"},
		{"business_tag", "TEXT DEFAULT ''"},
		{"status", "TEXT NOT NULL DEFAULT 'allocated'"},
	}
	for _, col := range allocCols {
		if err := ensureColumn(s.db, "alias_allocations", col[0], col[1]); err != nil {
			return err
		}
	}
	// 针对中间态无 account_id 补充时的平滑 backfill 策略
	_, _ = s.db.Exec(`
		UPDATE alias_allocations
		SET account_id = (
			SELECT account_id FROM alias_inventory WHERE alias_inventory.email = alias_allocations.alias_email
		)
		WHERE (account_id IS NULL OR account_id = '') AND EXISTS (
			SELECT 1 FROM alias_inventory WHERE alias_inventory.email = alias_allocations.alias_email AND alias_inventory.account_id != ''
		)
	`)
	_, _ = s.db.Exec(`
		UPDATE alias_allocations
		SET account_id = (
			SELECT account_id FROM alias_routes WHERE LOWER(TRIM(alias_routes.email)) = alias_allocations.alias_email
		)
		WHERE (account_id IS NULL OR account_id = '') AND EXISTS (
			SELECT 1 FROM alias_routes WHERE LOWER(TRIM(alias_routes.email)) = alias_allocations.alias_email AND alias_routes.account_id != ''
		)
	`)
	_, _ = s.db.Exec(`
		UPDATE alias_allocations
		SET account_id = (
			SELECT account_id FROM lease_records WHERE LOWER(TRIM(lease_records.email)) = alias_allocations.alias_email
		)
		WHERE (account_id IS NULL OR account_id = '') AND EXISTS (
			SELECT 1 FROM lease_records WHERE LOWER(TRIM(lease_records.email)) = alias_allocations.alias_email AND lease_records.account_id != ''
		)
	`)

	// 3. operations 补列
	opCols := [][2]string{
		{"request_hash", "TEXT NOT NULL DEFAULT ''"},
		{"candidate_email", "TEXT DEFAULT ''"},
		{"result_ref", "TEXT DEFAULT ''"},
		{"error_code", "TEXT DEFAULT ''"},
	}
	for _, col := range opCols {
		if err := ensureColumn(s.db, "operations", col[0], col[1]); err != nil {
			return err
		}
	}

	// 4. verification_requests 补列
	vreqCols := [][2]string{
		{"baseline_provider", "TEXT DEFAULT ''"},
		{"baseline_mailbox", "TEXT DEFAULT 'INBOX'"},
		{"baseline_uidvalidity", "INTEGER DEFAULT 0"},
		{"baseline_uid", "INTEGER DEFAULT 0"},
		{"matched_event_ref", "TEXT DEFAULT ''"},
		{"code", "TEXT DEFAULT ''"},
	}
	for _, col := range vreqCols {
		if err := ensureColumn(s.db, "verification_requests", col[0], col[1]); err != nil {
			return err
		}
	}

	// 5. 索引收敛
	indices := []string{
		"CREATE INDEX IF NOT EXISTS idx_alias_inv_acc_alloc ON alias_inventory (account_id, allocation_state);",
		"CREATE INDEX IF NOT EXISTS idx_alias_inv_alloc_state ON alias_inventory (allocation_state);",
		"CREATE INDEX IF NOT EXISTS idx_alias_alloc_owner ON alias_allocations (owner_kind, owner_id);",
		"CREATE INDEX IF NOT EXISTS idx_alias_alloc_email ON alias_allocations (alias_email);",
		"CREATE INDEX IF NOT EXISTS idx_operations_lookup ON operations (principal_kind, principal_id, operation_kind, idempotency_key);",
		"CREATE INDEX IF NOT EXISTS idx_vreq_principal ON verification_requests (principal_kind, principal_id);",
		"CREATE INDEX IF NOT EXISTS idx_vreq_lease ON verification_requests (lease_id);",
		"CREATE INDEX IF NOT EXISTS idx_vreq_email ON verification_requests (alias_email);",
	}
	for _, idx := range indices {
		if _, err := s.db.Exec(idx); err != nil {
			return err
		}
	}

	return nil
}

// ReconcileAvailableInventory 安全收敛库存状态：严禁无凭据激活 unknown 存量别名，隔离保护账号与孤儿资产
func (s *Store) ReconcileAvailableInventory() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var totalAffected int64

	// 1. 将属于受保护账号的非已分配别名归纳为 reserved 保护状态，杜绝误入可用库存
	resProt, err := s.db.Exec(`
		UPDATE alias_inventory
		SET allocation_state = 'reserved'
		WHERE allocation_state IN ('unknown', 'available')
		  AND account_id IN (
		      SELECT id FROM accounts 
		      WHERE name LIKE '%大号%'
		         OR (
		             CASE 
		                 WHEN json_valid(tags) THEN EXISTS (
		                     SELECT 1 FROM json_each(tags) 
		                     WHERE LOWER(TRIM(value)) IN ('personal', 'private', 'protected')
		                 )
		                 ELSE (
		                     tags LIKE '%"personal"%' COLLATE NOCASE OR
		                     tags LIKE '%"private"%' COLLATE NOCASE OR
		                     tags LIKE '%"protected"%' COLLATE NOCASE OR
		                     tags LIKE '%personal%' COLLATE NOCASE OR
		                     tags LIKE '%private%' COLLATE NOCASE OR
		                     tags LIKE '%protected%' COLLATE NOCASE
		                 )
		             END
		         )
		  )
	`)
	if err != nil {
		return 0, err
	}
	if n, _ := resProt.RowsAffected(); n > 0 {
		totalAffected += n
	}

	// 2. 账号缺失(孤儿资产)：account_id 在 accounts 表中不存在时，收敛隔离为 quarantined
	// 无论当前系统中是否存在其他账号，孤儿资产均必须隔离
	resOrphan, err := s.db.Exec(`
		UPDATE alias_inventory
		SET allocation_state = 'quarantined'
		WHERE allocation_state IN ('unknown', 'available')
		  AND (account_id IS NULL OR account_id = '' OR account_id NOT IN (SELECT id FROM accounts))
	`)
	if err != nil {
		return 0, err
	}
	if n, _ := resOrphan.RowsAffected(); n > 0 {
		totalAffected += n
	}

	// 3. 历史已分配别名收敛：确保已有 allocation 记录的资产必定处于 allocated
	resAlloc, err := s.db.Exec(`
		UPDATE alias_inventory
		SET allocation_state = 'allocated'
		WHERE allocation_state != 'allocated'
		  AND email IN (SELECT alias_email FROM alias_allocations)
	`)
	if err != nil {
		return 0, err
	}
	if n, _ := resAlloc.RowsAffected(); n > 0 {
		totalAffected += n
	}

	return totalAffected, nil
}

func (s *Store) migrateInventory() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

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
	// 历史无 immutable token_id 的记录一律归属于 legacy_unknown，严禁按 name 模糊匹配继承 (PR-06 §9.4, Issue 16)
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
	_ = leaseRows.Close()

	// 4. 迁移 alias_routes 中未有流水记录的别名，默认 unknown (严禁直接设为 available)
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

	return tx.Commit()
}

// AddInventoryAlias 将新生成或补货的别名登记入库
func (s *Store) AddInventoryAlias(accountID string, alias hme.Alias, sourceType string, isAvailable bool) error {
	email := strings.TrimSpace(strings.ToLower(alias.Email))
	if email == "" {
		return errors.New("empty email")
	}
	now := time.Now().UTC().Format(time.RFC3339)

	remoteState := RemoteActive
	if !alias.Active {
		remoteState = RemoteInactive
	}

	allocState := AllocationUnknown
	if isAvailable {
		allocState = AllocationAvailable
	}

	q := `
	INSERT INTO alias_inventory (
		email, account_id, provider_alias_id, remote_state, allocation_state, source_type, last_verified_at, snapshot_version
	) VALUES (?, ?, ?, ?, ?, ?, ?, 1)
	ON CONFLICT(email) DO UPDATE SET
		remote_state = excluded.remote_state,
		provider_alias_id = excluded.provider_alias_id,
		last_verified_at = excluded.last_verified_at
	`
	_, err := s.db.Exec(q, email, accountID, alias.AnonymousID, remoteState, allocState, sourceType, now)
	return err
}

// GetInventoryAlias 按邮箱查询库存记录
func (s *Store) GetInventoryAlias(email string) (*AliasInventory, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	var inv AliasInventory
	err := s.db.QueryRow(`
		SELECT email, account_id, provider_alias_id, remote_state, allocation_state, source_type, last_verified_at, snapshot_version
		FROM alias_inventory
		WHERE email = ?
	`, email).Scan(
		&inv.Email, &inv.AccountID, &inv.ProviderAliasID, &inv.RemoteState, &inv.AllocationState, &inv.SourceType, &inv.LastVerifiedAt, &inv.SnapshotVersion,
	)
	if err != nil {
		return nil, err
	}
	return &inv, nil
}

// SyncAliasInventory 同步远端别名快照；严禁将 allocated 覆盖为 available
func (s *Store) SyncAliasInventory(accountID string, aliases []hme.Alias) error {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO alias_inventory (
			email, account_id, provider_alias_id, remote_state, allocation_state, source_type, last_verified_at, snapshot_version
		) VALUES (?, ?, ?, ?, 'unknown', 'synced', ?, 1)
		ON CONFLICT(email) DO UPDATE SET
			remote_state = excluded.remote_state,
			provider_alias_id = excluded.provider_alias_id,
			last_verified_at = excluded.last_verified_at
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, a := range aliases {
		email := strings.TrimSpace(strings.ToLower(a.Email))
		if email == "" {
			continue
		}
		rState := RemoteActive
		if !a.Active {
			rState = RemoteInactive
		}
		if _, err := stmt.Exec(email, accountID, a.AnonymousID, rState, now); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// CountAuthoritativeAvailableAliases 权威统计处于 active 且 available 的可用别名库存数 (PR-08 P1-A)
func (s *Store) CountAuthoritativeAvailableAliases() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int
	_ = s.db.QueryRow(`
		SELECT COUNT(*)
		FROM alias_inventory
		WHERE remote_state = 'active' AND allocation_state = 'available'
	`).Scan(&count)
	return count
}

// QuarantineInventoryForAccount 将指定账号名下的可用库存标记为隔离/删除状态 (母号注销时级联清理，防幽灵出号)。
func (s *Store) QuarantineInventoryForAccount(accountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`
		UPDATE alias_inventory
		SET remote_state = 'deleted',
		    allocation_state = CASE WHEN allocation_state = 'available' THEN 'quarantined' ELSE allocation_state END
		WHERE account_id = ?
	`, accountID)
	return err
}

// UpdateAliasRemoteState 更新指定别名的远端状态与对应的本地分配资格 (PR-08 工作包 C1)。
// 停用：remote_state = 'inactive' (不可被认领分配)。
// 删除：remote_state = 'deleted'，若原为 available 则置为 quarantined。
// 激活：remote_state = 'active'；注意：绝不改变已有的 allocation_state (已分配/保留/隔离不可退回 available)。
// 约束：
// 1. 禁止空标识退化为整账号批量覆写；
// 2. 校验精确命中且仅命中目标一行 (RowsAffected == 1)，零行或多行均报错，杜绝假成功与身份歧义。
func (s *Store) UpdateAliasRemoteState(accountID, providerAliasID, email string, remoteState RemoteState) error {
	accountID = strings.TrimSpace(accountID)
	providerAliasID = strings.TrimSpace(providerAliasID)
	email = strings.TrimSpace(strings.ToLower(email))

	if accountID == "" {
		return errors.New("accountID cannot be empty")
	}
	if providerAliasID == "" && email == "" {
		return errors.New("cannot update alias remote state: neither providerAliasID nor email provided")
	}

	now := time.Now().UTC().Format(time.RFC3339)
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(`
		UPDATE alias_inventory
		SET remote_state = ?,
		    allocation_state = CASE 
		        WHEN ? = 'deleted' AND allocation_state = 'available' THEN 'quarantined'
		        ELSE allocation_state 
		    END,
		    provider_alias_id = CASE
		        WHEN (provider_alias_id = '' OR provider_alias_id IS NULL) AND ? != '' THEN ?
		        ELSE provider_alias_id
		    END,
		    last_verified_at = ?
		WHERE account_id = ? AND (
		    (? != '' AND provider_alias_id = ?) OR
		    (? != '' AND email = ?)
		)
	`, string(remoteState), string(remoteState), providerAliasID, providerAliasID, now, accountID, providerAliasID, providerAliasID, email, email)
	if err != nil {
		return err
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("no inventory alias found for account %s matching target (id=%s, email=%s)", accountID, providerAliasID, email)
	}
	if rows > 1 {
		return fmt.Errorf("ambiguous inventory update: %d rows matched for account %s (id=%s, email=%s)", rows, accountID, providerAliasID, email)
	}
	return nil
}

