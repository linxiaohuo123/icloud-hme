/**
 * [INPUT]: 依赖 database/sql, context, time, fmt, errors, strings
 * [OUTPUT]: 对外提供 AliasInventory, AliasAllocation, Operation 模型及 ClaimInventoryAlias, SyncAliasInventory, GetPrincipalAllocation 等原子持久化能力
 * [POS]: internal/store 的领域状态与库存隔离层 (PR-03)，分离 remote_state 与 allocation_state，提供 SQLite 事务级唯一约束与幂等认领
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
	CREATE INDEX IF NOT EXISTS idx_alias_inv_acc_alloc ON alias_inventory (account_id, allocation_state);
	CREATE INDEX IF NOT EXISTS idx_alias_inv_alloc_state ON alias_inventory (allocation_state);

	CREATE TABLE IF NOT EXISTS alias_allocations (
		allocation_id TEXT PRIMARY KEY,
		alias_email TEXT NOT NULL UNIQUE,
		owner_kind TEXT NOT NULL,
		owner_id TEXT NOT NULL,
		business_tag TEXT DEFAULT '',
		allocated_at TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'allocated'
	);
	CREATE INDEX IF NOT EXISTS idx_alias_alloc_owner ON alias_allocations (owner_kind, owner_id);
	CREATE INDEX IF NOT EXISTS idx_alias_alloc_email ON alias_allocations (alias_email);

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
	CREATE INDEX IF NOT EXISTS idx_operations_lookup ON operations (principal_kind, principal_id, operation_kind, idempotency_key);
	`
	_, err := s.db.Exec(ddl)
	return err
}

func (s *Store) migrateInventory() error {
	// 1. 迁移已有流水：将已交付别名入库，标记为已分配 (allocated)
	qLeases := `
	INSERT INTO alias_inventory (email, account_id, remote_state, allocation_state, source_type, snapshot_version)
	SELECT LOWER(TRIM(email)), account_id, 'unknown', 'allocated', 'legacy_unknown', 1
	FROM lease_records
	WHERE TRIM(email) != ''
	ON CONFLICT(email) DO UPDATE SET
		allocation_state = 'allocated'
	`
	if _, err := s.db.Exec(qLeases); err != nil {
		return fmt.Errorf("迁移 lease_records 到 alias_inventory 失败: %w", err)
	}

	// 2. 补全 alias_allocations 唯一分配关系
	qAlloc := `
	INSERT INTO alias_allocations (allocation_id, alias_email, owner_kind, owner_id, business_tag, allocated_at, status)
	SELECT id, LOWER(TRIM(email)), 'token', COALESCE(NULLIF(token_name, ''), 'legacy_token'), tag, allocated_at, 'allocated'
	FROM lease_records
	WHERE TRIM(email) != ''
	ON CONFLICT(alias_email) DO NOTHING
	`
	if _, err := s.db.Exec(qAlloc); err != nil {
		return fmt.Errorf("迁移 lease_records 到 alias_allocations 失败: %w", err)
	}

	// 3. 迁移 alias_routes 中未有流水记录的别名，默认 unknown (严禁直接设为 available)
	qRoutes := `
	INSERT INTO alias_inventory (email, account_id, remote_state, allocation_state, source_type, snapshot_version)
	SELECT LOWER(TRIM(email)), account_id, 'unknown', 'unknown', 'legacy_unknown', 1
	FROM alias_routes
	WHERE TRIM(email) != ''
	ON CONFLICT(email) DO NOTHING
	`
	if _, err := s.db.Exec(qRoutes); err != nil {
		return fmt.Errorf("迁移 alias_routes 到 alias_inventory 失败: %w", err)
	}

	return nil
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

// ClaimInventoryAlias 在单事务内原子认领可用库存，保障 SQLite 级唯一性与操作幂等性
func (s *Store) ClaimInventoryAlias(
	ctx context.Context,
	principalKind, principalID, operationKind, idempKey, reqHash, tag string,
	accountID string,
) (*AliasAllocation, error) {
	principalKind = strings.TrimSpace(principalKind)
	principalID = strings.TrimSpace(principalID)
	operationKind = strings.TrimSpace(operationKind)
	idempKey = strings.TrimSpace(idempKey)
	now := time.Now().UTC().Format(time.RFC3339)

	// 1. 幂等预检：相同幂等键直出原结果或阻断冲突
	if idempKey != "" {
		var opState, opReqHash, resultRef, candEmail string
		err := s.db.QueryRowContext(ctx, `
			SELECT state, request_hash, result_ref, candidate_email
			FROM operations
			WHERE principal_kind = ? AND principal_id = ? AND operation_kind = ? AND idempotency_key = ?
		`, principalKind, principalID, operationKind, idempKey).Scan(&opState, &opReqHash, &resultRef, &candEmail)

		if err == nil {
			if reqHash != "" && opReqHash != "" && opReqHash != reqHash {
				return nil, ErrIdempotencyConflict
			}
			if opState == "succeeded" {
				var alloc AliasAllocation
				qErr := s.db.QueryRowContext(ctx, `
					SELECT allocation_id, alias_email, owner_kind, owner_id, business_tag, allocated_at, status
					FROM alias_allocations
					WHERE allocation_id = ? OR alias_email = ?
				`, resultRef, candEmail).Scan(&alloc.AllocationID, &alloc.AliasEmail, &alloc.OwnerKind, &alloc.OwnerID, &alloc.BusinessTag, &alloc.AllocatedAt, &alloc.Status)
				if qErr == nil {
					return &alloc, nil
				}
			}
			if opState == "pending" {
				return nil, ErrOperationPending
			}
			if opState == "failed" {
				return nil, errors.New("previous operation failed")
			}
		}
	}

	// 2. 开启原子认领事务
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	opID := newOpaqueID("op_")
	if idempKey != "" {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO operations (
				operation_id, principal_kind, principal_id, operation_kind, idempotency_key, request_hash, state, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, ?)
		`, opID, principalKind, principalID, operationKind, idempKey, reqHash, now, now)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				return nil, ErrOperationPending
			}
			return nil, fmt.Errorf("记录操作失败: %w", err)
		}
	}

	// 3. 遴选可用别名 (必须满足 remote_state = active 且 allocation_state = available)
	query := `
		SELECT email, account_id
		FROM alias_inventory
		WHERE allocation_state = 'available' AND remote_state = 'active'
	`
	var args []any
	if accountID != "" {
		query += " AND account_id = ?"
		args = append(args, accountID)
	}
	query += " ORDER BY ROWID ASC LIMIT 1"

	var candEmail, candAccountID string
	err = tx.QueryRowContext(ctx, query, args...).Scan(&candEmail, &candAccountID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if idempKey != "" {
				_, _ = tx.ExecContext(ctx, `UPDATE operations SET state = 'failed', error_code = 'NO_AVAILABLE_INVENTORY', updated_at = ? WHERE operation_id = ?`, now, opID)
				_ = tx.Commit()
			}
			return nil, ErrNoAvailableInventory
		}
		return nil, err
	}

	// 4. 条件更新库存：利用受影响行数防重 (CAS)
	res, err := tx.ExecContext(ctx, `
		UPDATE alias_inventory
		SET allocation_state = 'allocated'
		WHERE email = ? AND allocation_state = 'available'
	`, candEmail)
	if err != nil {
		return nil, err
	}
	rowsAffected, _ := res.RowsAffected()
	if rowsAffected == 0 {
		return nil, ErrNoAvailableInventory
	}

	// 5. 插入分配凭据
	allocID := newOpaqueID("alloc_")
	alloc := &AliasAllocation{
		AllocationID: allocID,
		AliasEmail:   candEmail,
		OwnerKind:    principalKind,
		OwnerID:      principalID,
		BusinessTag:  tag,
		AllocatedAt:  now,
		Status:       "allocated",
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO alias_allocations (
			allocation_id, alias_email, owner_kind, owner_id, business_tag, allocated_at, status
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`, alloc.AllocationID, alloc.AliasEmail, alloc.OwnerKind, alloc.OwnerID, alloc.BusinessTag, alloc.AllocatedAt, alloc.Status)
	if err != nil {
		return nil, fmt.Errorf("写入分配关系失败: %w", err)
	}

	// 6. 兼容性写入 lease_records (确保既有管理控制台及审计视图无缝运作)
	_, _ = tx.ExecContext(ctx, `
		INSERT INTO lease_records (id, email, account_id, tag, status, allocated_at, token_name)
		VALUES (?, ?, ?, ?, 'leased', ?, ?)
	`, allocID, candEmail, candAccountID, tag, now, principalID)

	// 7. 更新操作记录为 succeeded
	if idempKey != "" {
		_, _ = tx.ExecContext(ctx, `
			UPDATE operations
			SET state = 'succeeded', result_ref = ?, candidate_email = ?, updated_at = ?
			WHERE operation_id = ?
		`, allocID, candEmail, now, opID)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("事务提交失败: %w", err)
	}

	return alloc, nil
}

// GetPrincipalAllocation 根据邮箱与主体核验分配归属
func (s *Store) GetPrincipalAllocation(ctx context.Context, email, ownerKind, ownerID string) (*AliasAllocation, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	var alloc AliasAllocation
	err := s.db.QueryRowContext(ctx, `
		SELECT allocation_id, alias_email, owner_kind, owner_id, business_tag, allocated_at, status
		FROM alias_allocations
		WHERE alias_email = ? AND owner_kind = ? AND owner_id = ?
	`, email, ownerKind, ownerID).Scan(
		&alloc.AllocationID, &alloc.AliasEmail, &alloc.OwnerKind, &alloc.OwnerID, &alloc.BusinessTag, &alloc.AllocatedAt, &alloc.Status,
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
		SELECT allocation_id, alias_email, owner_kind, owner_id, business_tag, allocated_at, status
		FROM alias_allocations
		WHERE allocation_id = ? AND owner_kind = ? AND owner_id = ?
	`, allocationID, ownerKind, ownerID).Scan(
		&alloc.AllocationID, &alloc.AliasEmail, &alloc.OwnerKind, &alloc.OwnerID, &alloc.BusinessTag, &alloc.AllocatedAt, &alloc.Status,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAllocationNotFound
		}
		return nil, err
	}
	return &alloc, nil
}

// IsEmailOwnedByToken 校验指定别名邮箱是否归属于该令牌 (同时兼容 v2 alias_allocations 与 v1 lease_records)
func (s *Store) IsEmailOwnedByToken(ctx context.Context, email, tokenID, tokenName string) bool {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return false
	}
	// 1. 优先查 PR-03/PR-04 alias_allocations
	var cnt int
	_ = s.db.QueryRowContext(ctx, `
		SELECT COUNT(1) FROM alias_allocations
		WHERE alias_email = ? AND owner_kind = 'token' AND (owner_id = ? OR owner_id = ?)
	`, email, tokenID, tokenName).Scan(&cnt)
	if cnt > 0 {
		return true
	}

	// 2. 回退兼容 v1 lease_records
	_ = s.db.QueryRowContext(ctx, `
		SELECT COUNT(1) FROM lease_records
		WHERE LOWER(email) = ? AND (token_name = ? OR token_name = ?)
	`, email, tokenName, tokenID).Scan(&cnt)
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
