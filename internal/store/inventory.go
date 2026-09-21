/**
 * [INPUT]: 依赖 database/sql, context, time, fmt, errors, strings
 * [OUTPUT]: 对外提供 AliasInventory, AliasAllocation, Operation, VerificationRequest 模型及 ClaimInventoryAlias, CompleteVerificationRequest, ExpireVerificationRequest, InvalidateVerificationRequest, GetMinBaselineUIDByEmail, GetPrincipalAllocation, CreateVerificationRequest, GetVerificationRequest, UpdateVerificationRequestResult 等原子持久化与 CAS 能力
 * [POS]: internal/store 的领域状态与库存隔离层 (PR-03/PR-05-1)，分离 remote_state 与 allocation_state，提供 SQLite 事务级唯一约束、幂等认领与终态原子 CAS 持久化取码请求
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"context"
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

// VerificationRequest 持久化取码请求实体 (Section VI)
type VerificationRequest struct {
	RequestID           string `json:"request_id"`
	PrincipalKind       string `json:"principal_kind"`
	PrincipalID         string `json:"principal_id"`
	LeaseID             string `json:"lease_id"`
	AliasEmail          string `json:"alias_email"`
	Status              string `json:"status"` // "pending", "ready", "succeeded", "expired", "invalidated"
	CreatedAt           string `json:"created_at"`
	ExpiresAt           string `json:"expires_at"`
	BaselineProvider    string `json:"baseline_provider,omitempty"`
	BaselineMailbox     string `json:"baseline_mailbox,omitempty"`
	BaselineUIDValidity uint32 `json:"baseline_uidvalidity,omitempty"`
	BaselineUID         uint32 `json:"baseline_uid,omitempty"`
	MatchedEventRef     string `json:"matched_event_ref,omitempty"`
	Code                string `json:"code,omitempty"`
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
		) VALUES (?, ?, ?, ?, 'unknown', 'legacy_unknown', ?, 1)
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

// ClaimInventoryAlias 在单事务内原子认领可用库存，保障 SQLite 级唯一性与操作幂等性 (PR-06 §9.4, Issue 11)
func (s *Store) ClaimInventoryAlias(
	ctx context.Context,
	principalKind, principalID, operationKind, idempKey, reqHash, tag string,
	allowedAccountIDs []string,
) (*AliasAllocation, *Operation, error) {
	principalKind = strings.TrimSpace(principalKind)
	principalID = strings.TrimSpace(principalID)
	operationKind = strings.TrimSpace(operationKind)
	idempKey = strings.TrimSpace(idempKey)
	now := time.Now().UTC().Format(time.RFC3339)

	s.mu.Lock()
	defer s.mu.Unlock()

	// 1. 幂等预检：相同幂等键直出原结果或阻断冲突
	if idempKey != "" {
		var op Operation
		err := s.db.QueryRowContext(ctx, `
			SELECT operation_id, principal_kind, principal_id, operation_kind, idempotency_key, request_hash, state, candidate_email, result_ref, error_code, created_at, updated_at
			FROM operations
			WHERE principal_kind = ? AND principal_id = ? AND operation_kind = ? AND idempotency_key = ?
		`, principalKind, principalID, operationKind, idempKey).Scan(
			&op.OperationID, &op.PrincipalKind, &op.PrincipalID, &op.OperationKind, &op.IdempotencyKey, &op.RequestHash, &op.State, &op.CandidateEmail, &op.ResultRef, &op.ErrorCode, &op.CreatedAt, &op.UpdatedAt,
		)

		if err == nil {
			if reqHash != "" && op.RequestHash != "" && op.RequestHash != reqHash {
				return nil, nil, ErrIdempotencyConflict
			}
			if op.State == "succeeded" {
				var alloc AliasAllocation
				qErr := s.db.QueryRowContext(ctx, `
					SELECT allocation_id, alias_email, account_id, owner_kind, owner_id, business_tag, allocated_at, status
					FROM alias_allocations
					WHERE allocation_id = ? OR alias_email = ?
				`, op.ResultRef, op.CandidateEmail).Scan(&alloc.AllocationID, &alloc.AliasEmail, &alloc.AccountID, &alloc.OwnerKind, &alloc.OwnerID, &alloc.BusinessTag, &alloc.AllocatedAt, &alloc.Status)
				if qErr == nil {
					return &alloc, &op, nil
				}
			}
			if op.State == "pending" {
				return nil, &op, ErrOperationPending
			}
			if op.State == "failed" {
				if op.ErrorCode == "NO_AVAILABLE_INVENTORY" {
					return nil, &op, ErrNoAvailableInventory
				}
				if op.ErrorCode != "" {
					return nil, &op, fmt.Errorf("operation failed with code: %s", op.ErrorCode)
				}
				return nil, &op, errors.New("previous operation failed")
			}
		}
	}

	// 2. 开启原子认领事务
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	opID := newOpaqueID("op_")
	var currentOp *Operation
	if idempKey != "" {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO operations (
				operation_id, principal_kind, principal_id, operation_kind, idempotency_key, request_hash, state, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, ?)
		`, opID, principalKind, principalID, operationKind, idempKey, reqHash, now, now)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				// 并发重入，查询既有操作
				var existingOp Operation
				_ = s.db.QueryRowContext(ctx, `
					SELECT operation_id, principal_kind, principal_id, operation_kind, idempotency_key, request_hash, state, candidate_email, result_ref, error_code, created_at, updated_at
					FROM operations
					WHERE principal_kind = ? AND principal_id = ? AND operation_kind = ? AND idempotency_key = ?
				`, principalKind, principalID, operationKind, idempKey).Scan(
					&existingOp.OperationID, &existingOp.PrincipalKind, &existingOp.PrincipalID, &existingOp.OperationKind, &existingOp.IdempotencyKey, &existingOp.RequestHash, &existingOp.State, &existingOp.CandidateEmail, &existingOp.ResultRef, &existingOp.ErrorCode, &existingOp.CreatedAt, &existingOp.UpdatedAt,
				)
				return nil, &existingOp, ErrOperationPending
			}
			return nil, nil, fmt.Errorf("记录操作失败: %w", err)
		}
		currentOp = &Operation{
			OperationID:    opID,
			PrincipalKind:  principalKind,
			PrincipalID:    principalID,
			OperationKind:  operationKind,
			IdempotencyKey: idempKey,
			RequestHash:    reqHash,
			State:          "pending",
			CreatedAt:      now,
			UpdatedAt:      now,
		}
	} else {
		currentOp = &Operation{
			OperationID:   opID,
			PrincipalKind: principalKind,
			PrincipalID:   principalID,
			OperationKind: operationKind,
			State:         "succeeded",
			CreatedAt:     now,
			UpdatedAt:     now,
		}
	}

	// 3. 遴选可用别名 (必须满足 remote_state = active 且 allocation_state = available)
	// 若 allowedAccountIDs 明确给出了集合但集合为空，直接返回库存为空，绝不跨账号越权发放 (Issue 11)
	if allowedAccountIDs != nil && len(allowedAccountIDs) == 0 {
		if idempKey != "" {
			_, _ = tx.ExecContext(ctx, `UPDATE operations SET state = 'failed', error_code = 'NO_AVAILABLE_INVENTORY', updated_at = ? WHERE operation_id = ?`, now, opID)
			_ = tx.Commit()
			currentOp.State = "failed"
			currentOp.ErrorCode = "NO_AVAILABLE_INVENTORY"
		}
		return nil, currentOp, ErrNoAvailableInventory
	}

	query := `
		SELECT email, account_id
		FROM alias_inventory
		WHERE allocation_state = 'available' AND remote_state = 'active'
	`
	var args []any
	if len(allowedAccountIDs) == 1 && allowedAccountIDs[0] != "" {
		query += " AND account_id = ? ORDER BY ROWID ASC LIMIT 1"
		args = append(args, allowedAccountIDs[0])
	} else if len(allowedAccountIDs) > 1 {
		var caseExpr strings.Builder
		caseExpr.WriteString("CASE account_id ")
		placeholders := make([]string, len(allowedAccountIDs))
		for i, a := range allowedAccountIDs {
			placeholders[i] = "?"
			args = append(args, a)
			caseExpr.WriteString(fmt.Sprintf("WHEN ? THEN %d ", i))
		}
		caseExpr.WriteString("ELSE 9999 END")
		for _, a := range allowedAccountIDs {
			args = append(args, a)
		}
		query += fmt.Sprintf(" AND account_id IN (%s) ORDER BY %s, ROWID ASC LIMIT 1", strings.Join(placeholders, ","), caseExpr.String())
	} else {
		query += " ORDER BY ROWID ASC LIMIT 1"
	}

	var candEmail, candAccountID string
	err = tx.QueryRowContext(ctx, query, args...).Scan(&candEmail, &candAccountID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if idempKey != "" {
				_, _ = tx.ExecContext(ctx, `UPDATE operations SET state = 'failed', error_code = 'NO_AVAILABLE_INVENTORY', updated_at = ? WHERE operation_id = ?`, now, opID)
				_ = tx.Commit()
				currentOp.State = "failed"
				currentOp.ErrorCode = "NO_AVAILABLE_INVENTORY"
			}
			return nil, currentOp, ErrNoAvailableInventory
		}
		return nil, currentOp, err
	}

	// 4. 条件更新库存：利用受影响行数防重 (CAS)
	res, err := tx.ExecContext(ctx, `
		UPDATE alias_inventory
		SET allocation_state = 'allocated'
		WHERE email = ? AND allocation_state = 'available'
	`, candEmail)
	if err != nil {
		return nil, currentOp, err
	}
	rowsAffected, _ := res.RowsAffected()
	if rowsAffected == 0 {
		return nil, currentOp, ErrNoAvailableInventory
	}

	// 5. 插入分配凭据
	allocID := newOpaqueID("alloc_")
	alloc := &AliasAllocation{
		AllocationID: allocID,
		AliasEmail:   candEmail,
		AccountID:    candAccountID,
		OwnerKind:    principalKind,
		OwnerID:      principalID,
		BusinessTag:  tag,
		AllocatedAt:  now,
		Status:       "allocated",
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO alias_allocations (
			allocation_id, alias_email, account_id, owner_kind, owner_id, business_tag, allocated_at, status
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, alloc.AllocationID, alloc.AliasEmail, alloc.AccountID, alloc.OwnerKind, alloc.OwnerID, alloc.BusinessTag, alloc.AllocatedAt, alloc.Status)
	if err != nil {
		return nil, currentOp, fmt.Errorf("写入分配关系失败: %w", err)
	}

	// 6. 兼容性写入 lease_records (确保既有管理控制台及审计视图无缝运作)
	tokenName := principalID
	if principalKind == "token" {
		var name string
		_ = tx.QueryRowContext(ctx, `SELECT name FROM api_tokens WHERE id = ?`, principalID).Scan(&name)
		if name != "" {
			tokenName = name
		}
	}
	_, _ = tx.ExecContext(ctx, `
		INSERT INTO lease_records (id, email, account_id, tag, status, allocated_at, token_name)
		VALUES (?, ?, ?, ?, 'leased', ?, ?)
	`, allocID, candEmail, candAccountID, tag, now, tokenName)

	// 7. 更新操作记录为 succeeded
	if idempKey != "" {
		_, _ = tx.ExecContext(ctx, `
			UPDATE operations
			SET state = 'succeeded', result_ref = ?, candidate_email = ?, updated_at = ?
			WHERE operation_id = ?
		`, allocID, candEmail, now, opID)
		currentOp.State = "succeeded"
		currentOp.ResultRef = allocID
		currentOp.CandidateEmail = candEmail
		currentOp.UpdatedAt = now
	}

	if err := tx.Commit(); err != nil {
		return nil, currentOp, fmt.Errorf("事务提交失败: %w", err)
	}

	return alloc, currentOp, nil
}

// RecordAllocation 原子持久化新建别名的分配凭据与审计流水
func (s *Store) RecordAllocation(alloc *AliasAllocation, tokenName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 1. 确保 inventory 为 allocated
	_, _ = tx.Exec(`
		UPDATE alias_inventory
		SET allocation_state = 'allocated'
		WHERE email = ?
	`, alloc.AliasEmail)

	// 2. 写入 alias_allocations
	_, err = tx.Exec(`
		INSERT INTO alias_allocations (allocation_id, alias_email, account_id, owner_kind, owner_id, business_tag, allocated_at, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(alias_email) DO UPDATE SET
			owner_kind = excluded.owner_kind,
			owner_id = excluded.owner_id,
			status = excluded.status
	`, alloc.AllocationID, alloc.AliasEmail, alloc.AccountID, alloc.OwnerKind, alloc.OwnerID, alloc.BusinessTag, alloc.AllocatedAt, alloc.Status)
	if err != nil {
		return err
	}

	// 3. 写入 lease_records 审计流水
	_, err = tx.Exec(`
		INSERT INTO lease_records (id, email, account_id, tag, status, allocated_at, completed_at, token_name)
		VALUES (?, ?, ?, ?, 'completed', ?, ?, ?)
	`, alloc.AllocationID, alloc.AliasEmail, alloc.AccountID, alloc.BusinessTag, alloc.AllocatedAt, alloc.AllocatedAt, tokenName)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// GetPrincipalAllocation 根据邮箱与主体核验分配归属
func (s *Store) GetPrincipalAllocation(ctx context.Context, email, ownerKind, ownerID string) (*AliasAllocation, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	var alloc AliasAllocation
	err := s.db.QueryRowContext(ctx, `
		SELECT allocation_id, alias_email, account_id, owner_kind, owner_id, business_tag, allocated_at, status
		FROM alias_allocations
		WHERE alias_email = ? AND owner_kind = ? AND owner_id = ?
	`, email, ownerKind, ownerID).Scan(
		&alloc.AllocationID, &alloc.AliasEmail, &alloc.AccountID, &alloc.OwnerKind, &alloc.OwnerID, &alloc.BusinessTag, &alloc.AllocatedAt, &alloc.Status,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAllocationNotFound
		}
		return nil, err
	}
	return &alloc, nil
}

// GetPrincipalAllocationByID 根据 AllocationID 与主体核验分配归属
func (s *Store) GetPrincipalAllocationByID(ctx context.Context, allocationID, ownerKind, ownerID string) (*AliasAllocation, error) {
	allocationID = strings.TrimSpace(allocationID)
	var alloc AliasAllocation
	err := s.db.QueryRowContext(ctx, `
		SELECT allocation_id, alias_email, account_id, owner_kind, owner_id, business_tag, allocated_at, status
		FROM alias_allocations
		WHERE allocation_id = ? AND owner_kind = ? AND owner_id = ?
	`, allocationID, ownerKind, ownerID).Scan(
		&alloc.AllocationID, &alloc.AliasEmail, &alloc.AccountID, &alloc.OwnerKind, &alloc.OwnerID, &alloc.BusinessTag, &alloc.AllocatedAt, &alloc.Status,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAllocationNotFound
		}
		return nil, err
	}
	return &alloc, nil
}

// IsEmailOwnedByToken 校验指定别名邮箱是否归属于该令牌 (仅基于不可变 token_id 与 alias_allocations)
func (s *Store) IsEmailOwnedByToken(ctx context.Context, email, tokenID string) bool {
	email = strings.TrimSpace(strings.ToLower(email))
	tokenID = strings.TrimSpace(tokenID)
	if email == "" || tokenID == "" {
		return false
	}
	var cnt int
	_ = s.db.QueryRowContext(ctx, `
		SELECT COUNT(1) FROM alias_allocations
		WHERE alias_email = ? AND owner_kind = 'token' AND owner_id = ?
	`, email, tokenID).Scan(&cnt)
	return cnt > 0
}

// GetOperation 查询幂等操作记录
func (s *Store) GetOperation(ctx context.Context, operationID, principalKind, principalID string) (*Operation, error) {
	var op Operation
	err := s.db.QueryRowContext(ctx, `
		SELECT operation_id, principal_kind, principal_id, operation_kind, idempotency_key, request_hash, state, candidate_email, result_ref, error_code, created_at, updated_at
		FROM operations
		WHERE operation_id = ? AND principal_kind = ? AND principal_id = ?
	`, operationID, principalKind, principalID).Scan(
		&op.OperationID, &op.PrincipalKind, &op.PrincipalID, &op.OperationKind, &op.IdempotencyKey, &op.RequestHash, &op.State, &op.CandidateEmail, &op.ResultRef, &op.ErrorCode, &op.CreatedAt, &op.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("operation not found")
		}
		return nil, err
	}
	return &op, nil
}

var (
	ErrConflictActiveRequest = errors.New("active verification request already exists for this lease")
	ErrServerBusy            = errors.New("global active verification requests limit exceeded")
	ErrTooManyRequests       = errors.New("per-principal active verification requests limit exceeded")
)

// CreateVerificationRequestAtomic 在单事务/临界区内原子完成过期清理、活跃冲突检查、容量判定与任务插入 (PR-08 Final Hardening §4)
func (s *Store) CreateVerificationRequestAtomic(
	ctx context.Context,
	req *VerificationRequest,
	maxGlobalActive int,
	maxPerTokenActive int,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	nowStr := time.Now().UTC().Format(time.RFC3339)

	// 1. 清理已过期的任务状态 (不再阻塞同 lease 的新任务，也不占活跃配额)
	_, err = tx.ExecContext(ctx, `
		UPDATE verification_requests
		SET status = 'expired'
		WHERE status IN ('pending', 'ready') AND expires_at < ?
	`, nowStr)
	if err != nil {
		return err
	}

	// 2. 检查同 lease 是否存在活跃任务 (保证并发请求不能在同 lease 创建两个 active request)
	var leaseActiveCount int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(1)
		FROM verification_requests
		WHERE lease_id = ? AND status IN ('pending', 'ready') AND expires_at >= ?
	`, req.LeaseID, nowStr).Scan(&leaseActiveCount)
	if err != nil {
		return err
	}
	if leaseActiveCount > 0 {
		return ErrConflictActiveRequest
	}

	// 3. 检查 principal 活跃任务上限 (不超过 maxPerTokenActive)
	if maxPerTokenActive > 0 {
		var tokenActiveCount int
		err = tx.QueryRowContext(ctx, `
			SELECT COUNT(1)
			FROM verification_requests
			WHERE principal_kind = ? AND principal_id = ? AND status IN ('pending', 'ready') AND expires_at >= ?
		`, req.PrincipalKind, req.PrincipalID, nowStr).Scan(&tokenActiveCount)
		if err != nil {
			return err
		}
		if tokenActiveCount >= maxPerTokenActive {
			return ErrTooManyRequests
		}
	}

	// 4. 检查全局活跃任务上限 (不超过 maxGlobalActive)
	if maxGlobalActive > 0 {
		var globalActiveCount int
		err = tx.QueryRowContext(ctx, `
			SELECT COUNT(1)
			FROM verification_requests
			WHERE status IN ('pending', 'ready') AND expires_at >= ?
		`, nowStr).Scan(&globalActiveCount)
		if err != nil {
			return err
		}
		if globalActiveCount >= maxGlobalActive {
			return ErrServerBusy
		}
	}

	if req.BaselineMailbox == "" {
		req.BaselineMailbox = "INBOX"
	}

	// 5. 原子插入
	q := `
	INSERT INTO verification_requests (
		request_id, principal_kind, principal_id, lease_id, alias_email, status, created_at, expires_at,
		baseline_provider, baseline_mailbox, baseline_uidvalidity, baseline_uid, matched_event_ref, code
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err = tx.ExecContext(ctx, q,
		req.RequestID, req.PrincipalKind, req.PrincipalID, req.LeaseID, req.AliasEmail, req.Status, req.CreatedAt, req.ExpiresAt,
		req.BaselineProvider, req.BaselineMailbox, req.BaselineUIDValidity, req.BaselineUID, req.MatchedEventRef, req.Code,
	)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// CreateVerificationRequest 创建持久化取码请求 (Section VI)
func (s *Store) CreateVerificationRequest(ctx context.Context, req *VerificationRequest) error {
	if req.BaselineMailbox == "" {
		req.BaselineMailbox = "INBOX"
	}
	q := `
	INSERT INTO verification_requests (
		request_id, principal_kind, principal_id, lease_id, alias_email, status, created_at, expires_at,
		baseline_provider, baseline_mailbox, baseline_uidvalidity, baseline_uid, matched_event_ref, code
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err := s.db.ExecContext(ctx, q,
		req.RequestID, req.PrincipalKind, req.PrincipalID, req.LeaseID, req.AliasEmail, req.Status, req.CreatedAt, req.ExpiresAt,
		req.BaselineProvider, req.BaselineMailbox, req.BaselineUIDValidity, req.BaselineUID, req.MatchedEventRef, req.Code,
	)
	return err
}

// GetVerificationRequest 获取持久化取码请求 (Section VI)
func (s *Store) GetVerificationRequest(ctx context.Context, requestID, principalKind, principalID string) (*VerificationRequest, error) {
	requestID = strings.TrimSpace(requestID)
	var req VerificationRequest
	q := `
	SELECT request_id, principal_kind, principal_id, lease_id, alias_email, status, created_at, expires_at,
	       baseline_provider, COALESCE(baseline_mailbox, 'INBOX'), baseline_uidvalidity, baseline_uid, matched_event_ref, code
	FROM verification_requests
	WHERE request_id = ? AND principal_kind = ? AND principal_id = ?
	`
	err := s.db.QueryRowContext(ctx, q, requestID, principalKind, principalID).Scan(
		&req.RequestID, &req.PrincipalKind, &req.PrincipalID, &req.LeaseID, &req.AliasEmail, &req.Status, &req.CreatedAt, &req.ExpiresAt,
		&req.BaselineProvider, &req.BaselineMailbox, &req.BaselineUIDValidity, &req.BaselineUID, &req.MatchedEventRef, &req.Code,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrVerificationRequestNotFound
		}
		return nil, err
	}
	return &req, nil
}

// getVerificationRequestByID 按 request_id 查询实体 (供 CAS 失败后获取最新终态)
func (s *Store) getVerificationRequestByID(ctx context.Context, requestID string) (*VerificationRequest, error) {
	requestID = strings.TrimSpace(requestID)
	var req VerificationRequest
	q := `
	SELECT request_id, principal_kind, principal_id, lease_id, alias_email, status, created_at, expires_at,
	       baseline_provider, COALESCE(baseline_mailbox, 'INBOX'), baseline_uidvalidity, baseline_uid, matched_event_ref, code
	FROM verification_requests
	WHERE request_id = ?
	`
	err := s.db.QueryRowContext(ctx, q, requestID).Scan(
		&req.RequestID, &req.PrincipalKind, &req.PrincipalID, &req.LeaseID, &req.AliasEmail, &req.Status, &req.CreatedAt, &req.ExpiresAt,
		&req.BaselineProvider, &req.BaselineMailbox, &req.BaselineUIDValidity, &req.BaselineUID, &req.MatchedEventRef, &req.Code,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrVerificationRequestNotFound
		}
		return nil, err
	}
	return &req, nil
}

// CompleteVerificationRequest 原子 CAS 将取码任务标记为成功 (Section VI, Issue 6 & 7)
// 仅允许从 ready 或 pending 变迁为 succeeded；如果已经处于终态或已过 expires_at，返回当前终态与 won=false，绝不覆盖已有结果。
func (s *Store) CompleteVerificationRequest(ctx context.Context, requestID, code, matchedEventRef string, now ...time.Time) (*VerificationRequest, bool, error) {
	requestID = strings.TrimSpace(requestID)
	currentTime := time.Now().UTC()
	if len(now) > 0 && !now[0].IsZero() {
		currentTime = now[0].UTC()
	}
	nowStr := currentTime.Format(time.RFC3339)

	q := `
	UPDATE verification_requests
	SET status = 'succeeded', code = ?, matched_event_ref = ?
	WHERE request_id = ? AND status IN ('ready', 'pending') AND expires_at > ?
	`
	res, err := s.db.ExecContext(ctx, q, code, matchedEventRef, requestID, nowStr)
	if err != nil {
		return nil, false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	req, err := s.getVerificationRequestByID(ctx, requestID)
	if err != nil {
		return nil, false, err
	}

	// CAS 未中且数据库仍为非终态但已过 expires_at 时，原子级收敛为 expired
	if rows == 0 && req != nil && (req.Status == "ready" || req.Status == "pending") && req.ExpiresAt <= nowStr {
		_, _, _ = s.ExpireVerificationRequest(ctx, requestID)
		req, _ = s.getVerificationRequestByID(ctx, requestID)
	}

	return req, rows > 0, nil
}

// ExpireVerificationRequest 原子 CAS 将取码任务标记为超时过期 (Issue 6)
func (s *Store) ExpireVerificationRequest(ctx context.Context, requestID string) (*VerificationRequest, bool, error) {
	requestID = strings.TrimSpace(requestID)
	q := `
	UPDATE verification_requests
	SET status = 'expired'
	WHERE request_id = ? AND status IN ('ready', 'pending')
	`
	res, err := s.db.ExecContext(ctx, q, requestID)
	if err != nil {
		return nil, false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	req, err := s.getVerificationRequestByID(ctx, requestID)
	if err != nil {
		return nil, false, err
	}
	return req, rows > 0, nil
}

// InvalidateVerificationRequest 原子 CAS 将取码任务标记为因代际突变失效 (Issue 6)
func (s *Store) InvalidateVerificationRequest(ctx context.Context, requestID string) (*VerificationRequest, bool, error) {
	requestID = strings.TrimSpace(requestID)
	q := `
	UPDATE verification_requests
	SET status = 'invalidated'
	WHERE request_id = ? AND status IN ('ready', 'pending')
	`
	res, err := s.db.ExecContext(ctx, q, requestID)
	if err != nil {
		return nil, false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	req, err := s.getVerificationRequestByID(ctx, requestID)
	if err != nil {
		return nil, false, err
	}
	return req, rows > 0, nil
}

// UpdateVerificationRequestResult 更新取码任务结果 (保证原子 CAS，拒绝终态覆盖)
func (s *Store) UpdateVerificationRequestResult(ctx context.Context, requestID, status, code, matchedEventRef string) error {
	switch status {
	case "succeeded":
		_, _, err := s.CompleteVerificationRequest(ctx, requestID, code, matchedEventRef)
		return err
	case "expired":
		_, _, err := s.ExpireVerificationRequest(ctx, requestID)
		return err
	case "invalidated":
		_, _, err := s.InvalidateVerificationRequest(ctx, requestID)
		return err
	default:
		q := `
		UPDATE verification_requests
		SET status = ?, code = ?, matched_event_ref = ?
		WHERE request_id = ? AND status IN ('ready', 'pending')
		`
		_, err := s.db.ExecContext(ctx, q, status, code, matchedEventRef, requestID)
		return err
	}
}

// GetActiveVerificationRequestByLease 查询指定 lease 是否已有进行中的取码任务 (status IN ('ready', 'pending') 且未过期) (PR-06 Section 9.2)
func (s *Store) GetActiveVerificationRequestByLease(ctx context.Context, leaseID string) (*VerificationRequest, error) {
	leaseID = strings.TrimSpace(leaseID)
	now := time.Now().UTC().Format(time.RFC3339)
	var req VerificationRequest
	q := `
	SELECT request_id, principal_kind, principal_id, lease_id, alias_email, status, created_at, expires_at,
	       baseline_provider, COALESCE(baseline_mailbox, 'INBOX'), baseline_uidvalidity, baseline_uid, matched_event_ref, code
	FROM verification_requests
	WHERE lease_id = ? AND status IN ('ready', 'pending') AND expires_at > ?
	ORDER BY created_at DESC
	LIMIT 1
	`
	err := s.db.QueryRowContext(ctx, q, leaseID, now).Scan(
		&req.RequestID, &req.PrincipalKind, &req.PrincipalID, &req.LeaseID, &req.AliasEmail, &req.Status, &req.CreatedAt, &req.ExpiresAt,
		&req.BaselineProvider, &req.BaselineMailbox, &req.BaselineUIDValidity, &req.BaselineUID, &req.MatchedEventRef, &req.Code,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &req, nil
}

// CountActiveVerificationRequests 查询当前全局活跃进行中的取码任务数 (PR-07 §10.2)
func (s *Store) CountActiveVerificationRequests(ctx context.Context) (int, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	var count int
	q := `
	SELECT count(*)
	FROM verification_requests
	WHERE status IN ('ready', 'pending') AND (expires_at IS NULL OR expires_at > ?)
	`
	err := s.db.QueryRowContext(ctx, q, now).Scan(&count)
	return count, err
}

// CountActiveVerificationRequestsByPrincipal 查询指定主体当前活跃进行中的取码任务数 (PR-07 §10.2)
func (s *Store) CountActiveVerificationRequestsByPrincipal(ctx context.Context, principalKind, principalID string) (int, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	var count int
	q := `
	SELECT count(*)
	FROM verification_requests
	WHERE principal_kind = ? AND principal_id = ? AND status IN ('ready', 'pending') AND (expires_at IS NULL OR expires_at > ?)
	`
	err := s.db.QueryRowContext(ctx, q, principalKind, principalID, now).Scan(&count)
	return count, err
}

// GetMinBaselineUIDByEmail 查询指定别名邮箱当前所有活跃取码任务的最小 baseline_uid (Issue 14).
// 若无活跃任务或有任何活跃任务缺少有效的 IMAP baseline UID，则返回 0 以退回全量拉取。
func (s *Store) GetMinBaselineUIDByEmail(ctx context.Context, email string) (uint32, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	now := time.Now().UTC().Format(time.RFC3339)
	q := `
	SELECT 
		count(*),
		count(CASE WHEN baseline_uid > 0 AND baseline_provider = 'imap' THEN 1 END),
		COALESCE(min(CASE WHEN baseline_uid > 0 AND baseline_provider = 'imap' THEN baseline_uid END), 0)
	FROM verification_requests
	WHERE alias_email = ? AND status IN ('ready', 'pending') AND (expires_at IS NULL OR expires_at > ?)
	`
	var totalCount, validCount int
	var minUID uint32
	err := s.db.QueryRowContext(ctx, q, email, now).Scan(&totalCount, &validCount, &minUID)
	if err != nil {
		return 0, err
	}
	if totalCount == 0 || totalCount != validCount {
		return 0, nil
	}
	return minUID, nil
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

