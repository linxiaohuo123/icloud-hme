/**
 * [INPUT]: 依赖 context, database/sql, errors, fmt, strings, time, icloud-hme/internal/store (Store, AliasAllocation, Operation, ErrIdempotencyConflict, ErrOperationPending, ErrNoAvailableInventory, ErrAllocationNotFound)
 * [OUTPUT]: 对外提供 ClaimInventoryAlias, RecordAllocation, GetPrincipalAllocation, GetPrincipalAllocationByID, IsEmailOwnedByToken, GetOperation
 * [POS]: internal/store 的别名认领与发放审计领域，支持 SQLite 事务级唯一约束、多注册机原子认领与幂等操作日志
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
)

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
					WHERE allocation_id = ?
				`, op.ResultRef).Scan(&alloc.AllocationID, &alloc.AliasEmail, &alloc.AccountID, &alloc.OwnerKind, &alloc.OwnerID, &alloc.BusinessTag, &alloc.AllocatedAt, &alloc.Status)
				if qErr != nil {
					return nil, &op, fmt.Errorf("inconsistent operation state: allocation record not found for result_ref %s: %w", op.ResultRef, qErr)
				}
				// 严格核对主体与邮箱一致性
				if alloc.OwnerKind != op.PrincipalKind || alloc.OwnerID != op.PrincipalID {
					return nil, &op, fmt.Errorf("inconsistent operation state: principal mismatch (%s:%s vs %s:%s)", alloc.OwnerKind, alloc.OwnerID, op.PrincipalKind, op.PrincipalID)
				}
				if op.CandidateEmail != "" && !strings.EqualFold(alloc.AliasEmail, op.CandidateEmail) {
					return nil, &op, fmt.Errorf("inconsistent operation state: email mismatch (%s vs %s)", alloc.AliasEmail, op.CandidateEmail)
				}
				return &alloc, &op, nil
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
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("idempotency pre-check query failed: %w", err)
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
				_ = tx.Rollback()
				// 并发重入，释放事务后可靠查询既有操作
				var existingOp Operation
				qErr := s.db.QueryRowContext(ctx, `
					SELECT operation_id, principal_kind, principal_id, operation_kind, idempotency_key, request_hash, state, candidate_email, result_ref, error_code, created_at, updated_at
					FROM operations
					WHERE principal_kind = ? AND principal_id = ? AND operation_kind = ? AND idempotency_key = ?
				`, principalKind, principalID, operationKind, idempKey).Scan(
					&existingOp.OperationID, &existingOp.PrincipalKind, &existingOp.PrincipalID, &existingOp.OperationKind, &existingOp.IdempotencyKey, &existingOp.RequestHash, &existingOp.State, &existingOp.CandidateEmail, &existingOp.ResultRef, &existingOp.ErrorCode, &existingOp.CreatedAt, &existingOp.UpdatedAt,
				)
				if qErr != nil {
					return nil, nil, fmt.Errorf("failed to read existing operation after unique conflict: %w", qErr)
				}
				if reqHash != "" && existingOp.RequestHash != "" && existingOp.RequestHash != reqHash {
					return nil, &existingOp, ErrIdempotencyConflict
				}
				if existingOp.State == "succeeded" {
					var alloc AliasAllocation
					allocErr := s.db.QueryRowContext(ctx, `
						SELECT allocation_id, alias_email, account_id, owner_kind, owner_id, business_tag, allocated_at, status
						FROM alias_allocations
						WHERE allocation_id = ?
					`, existingOp.ResultRef).Scan(&alloc.AllocationID, &alloc.AliasEmail, &alloc.AccountID, &alloc.OwnerKind, &alloc.OwnerID, &alloc.BusinessTag, &alloc.AllocatedAt, &alloc.Status)
					if allocErr != nil {
						return nil, &existingOp, fmt.Errorf("inconsistent operation state: allocation record not found for result_ref %s: %w", existingOp.ResultRef, allocErr)
					}
					return &alloc, &existingOp, nil
				}
				if existingOp.State == "pending" {
					return nil, &existingOp, ErrOperationPending
				}
				if existingOp.State == "failed" {
					if existingOp.ErrorCode == "NO_AVAILABLE_INVENTORY" {
						return nil, &existingOp, ErrNoAvailableInventory
					}
					if existingOp.ErrorCode != "" {
						return nil, &existingOp, fmt.Errorf("operation failed with code: %s", existingOp.ErrorCode)
					}
					return nil, &existingOp, errors.New("previous operation failed")
				}
				return nil, &existingOp, fmt.Errorf("unknown operation state: %s", existingOp.State)
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
			if _, execErr := tx.ExecContext(ctx, `UPDATE operations SET state = 'failed', error_code = 'NO_AVAILABLE_INVENTORY', updated_at = ? WHERE operation_id = ?`, now, opID); execErr != nil {
				return nil, nil, fmt.Errorf("update failed operation failed: %w", execErr)
			}
			if cErr := tx.Commit(); cErr != nil {
				return nil, nil, fmt.Errorf("commit failed operation failed: %w", cErr)
			}
			currentOp.State = "failed"
			currentOp.ErrorCode = "NO_AVAILABLE_INVENTORY"
		}
		return nil, currentOp, ErrNoAvailableInventory
	}

	query := `
		SELECT email, account_id
		FROM alias_inventory
		WHERE allocation_state = 'available' AND remote_state = 'active'
		  AND account_id IN (
		      SELECT id FROM accounts
		      WHERE status = 'active'
		        AND (name NOT LIKE '%大号%')
		        AND (
		            CASE 
		                WHEN json_valid(tags) THEN NOT EXISTS (
		                    SELECT 1 FROM json_each(tags) 
		                    WHERE LOWER(TRIM(value)) IN ('personal', 'private', 'protected')
		                )
		                ELSE (
		                    tags NOT LIKE '%"personal"%' COLLATE NOCASE AND
		                    tags NOT LIKE '%"private"%' COLLATE NOCASE AND
		                    tags NOT LIKE '%"protected"%' COLLATE NOCASE AND
		                    tags NOT LIKE '%personal%' COLLATE NOCASE AND
		                    tags NOT LIKE '%private%' COLLATE NOCASE AND
		                    tags NOT LIKE '%protected%' COLLATE NOCASE
		                )
		            END
		        )
		  )
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
				if _, execErr := tx.ExecContext(ctx, `UPDATE operations SET state = 'failed', error_code = 'NO_AVAILABLE_INVENTORY', updated_at = ? WHERE operation_id = ?`, now, opID); execErr != nil {
					return nil, nil, fmt.Errorf("update failed operation failed: %w", execErr)
				}
				if cErr := tx.Commit(); cErr != nil {
					return nil, nil, fmt.Errorf("commit failed operation failed: %w", cErr)
				}
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
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return nil, currentOp, fmt.Errorf("%w: %v", ErrAllocationConflict, err)
		}
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
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO lease_records (id, email, account_id, tag, status, allocated_at, token_name)
		VALUES (?, ?, ?, ?, 'leased', ?, ?)
	`, allocID, candEmail, candAccountID, tag, now, tokenName); err != nil {
		return nil, currentOp, fmt.Errorf("写入 lease_records 失败: %w", err)
	}

	// 7. 更新操作记录为 succeeded
	if idempKey != "" {
		res, err := tx.ExecContext(ctx, `
			UPDATE operations
			SET state = 'succeeded', result_ref = ?, candidate_email = ?, updated_at = ?
			WHERE operation_id = ?
		`, allocID, candEmail, now, opID)
		if err != nil {
			return nil, currentOp, fmt.Errorf("更新 operation 失败: %w", err)
		}
		rows, err := res.RowsAffected()
		if err != nil || rows == 0 {
			return nil, currentOp, fmt.Errorf("更新 operation 影响行数不符 (rows=%d): %w", rows, err)
		}
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
// 硬约束：严禁通过 ON CONFLICT 覆盖历史 allocation_id、owner、account、business_tag 或 allocated_at。
// 行为约定：
// - 全新 alias：可靠保存并返回持久化后的分配记录；
// - 同主体同一业务操作的合法重试：幂等返回已有租约，不生成第二条审计流水；
// - 跨主体冲突、来源不明(如 legacy_unknown)或不同业务标签：明确报错冲突，事务回滚。
func (s *Store) RecordAllocation(alloc *AliasAllocation, tokenName string) (*AliasAllocation, error) {
	if alloc == nil || alloc.AliasEmail == "" {
		return nil, errors.New("invalid allocation: nil or empty email")
	}
	normEmail := strings.TrimSpace(strings.ToLower(alloc.AliasEmail))
	alloc.AliasEmail = normEmail

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 1. 检查是否存在已有分配记录
	var existing AliasAllocation
	err = tx.QueryRow(`
		SELECT allocation_id, alias_email, account_id, owner_kind, owner_id, business_tag, allocated_at, status
		FROM alias_allocations
		WHERE alias_email = ?
	`, normEmail).Scan(
		&existing.AllocationID, &existing.AliasEmail, &existing.AccountID, &existing.OwnerKind, &existing.OwnerID, &existing.BusinessTag, &existing.AllocatedAt, &existing.Status,
	)

	if err == nil {
		// 已存在分配记录
		// 跨主体冲突或历史不明归属(legacy_unknown)或已被隔离(quarantined)：严禁接管与覆盖！
		if existing.OwnerKind != alloc.OwnerKind || existing.OwnerID != alloc.OwnerID || existing.OwnerID == "legacy_unknown" || existing.Status == "quarantined" {
			return nil, fmt.Errorf("alias %s is already allocated to owner (%s:%s): %w", normEmail, existing.OwnerKind, existing.OwnerID, ErrAllocationConflict)
		}
		// 同主体核验 account: 必须核验 account
		if alloc.AccountID != "" && existing.AccountID != "" && existing.AccountID != alloc.AccountID {
			return nil, fmt.Errorf("alias %s belongs to same owner but has different account (%s vs %s): %w", normEmail, existing.AccountID, alloc.AccountID, ErrAllocationConflict)
		}
		// 同主体核验：业务标签不一致时无法证明为同一操作重试，拒绝覆盖
		if existing.BusinessTag != alloc.BusinessTag {
			return nil, fmt.Errorf("alias %s belongs to same owner but has different business tag (%q vs %q): %w", normEmail, existing.BusinessTag, alloc.BusinessTag, ErrAllocationConflict)
		}
		// 同主体核验已有状态：必须处于合法的 allocated 状态
		if existing.Status != "allocated" {
			return nil, fmt.Errorf("alias %s existing allocation status is %s (expected allocated): %w", normEmail, existing.Status, ErrAllocationConflict)
		}
		// 同主体合法重试：原样返回持久化结果，不插入第二条 lease_records 审计
		return &existing, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("query existing allocation failed: %w", err)
	}

	// 2. 显式校验库存存在、账号匹配、状态允许、来源可靠；禁止直接 UPDATE 未经证明的 legacy_unknown、reserved 或 quarantined
	var inv AliasInventory
	invErr := tx.QueryRow(`
		SELECT email, account_id, provider_alias_id, remote_state, allocation_state, source_type
		FROM alias_inventory
		WHERE email = ?
	`, normEmail).Scan(
		&inv.Email, &inv.AccountID, &inv.ProviderAliasID, &inv.RemoteState, &inv.AllocationState, &inv.SourceType,
	)
	if invErr != nil {
		if errors.Is(invErr, sql.ErrNoRows) {
			return nil, fmt.Errorf("cannot allocate alias %s: inventory record does not exist: %w", normEmail, ErrNoAvailableInventory)
		}
		return nil, fmt.Errorf("query alias_inventory failed: %w", invErr)
	}

	// 账号匹配校验
	if alloc.AccountID != "" && inv.AccountID != "" && inv.AccountID != alloc.AccountID {
		return nil, fmt.Errorf("cannot allocate alias %s: account mismatch (%s vs %s): %w", normEmail, inv.AccountID, alloc.AccountID, ErrAllocationConflict)
	}

	// 来源与状态校验：禁止直接 UPDATE 未经证明的 legacy_unknown、reserved 或 quarantined
	if inv.AllocationState == "reserved" || inv.AllocationState == "quarantined" {
		return nil, fmt.Errorf("cannot allocate alias %s: inventory state %s is not allocatable: %w", normEmail, inv.AllocationState, ErrAllocationConflict)
	}
	if inv.SourceType == "legacy_unknown" {
		return nil, fmt.Errorf("cannot allocate alias %s: unproven inventory source %s is not allocatable: %w", normEmail, inv.SourceType, ErrAllocationConflict)
	}
	if inv.AllocationState == "unknown" && inv.SourceType != "created" {
		return nil, fmt.Errorf("cannot allocate alias %s: unknown inventory state without created provenance: %w", normEmail, ErrAllocationConflict)
	}
	if inv.RemoteState != RemoteActive {
		return nil, fmt.Errorf("cannot allocate alias %s: remote state is %s (must be active): %w", normEmail, inv.RemoteState, ErrAllocationConflict)
	}

	// 3. 全新 alias：确保 inventory 为 allocated，校验 RowsAffected == 1
	res, err := tx.Exec(`
		UPDATE alias_inventory
		SET allocation_state = 'allocated'
		WHERE email = ? AND (allocation_state = 'available' OR (allocation_state = 'unknown' AND source_type = 'created'))
	`, normEmail)
	if err != nil {
		return nil, fmt.Errorf("update alias_inventory failed: %w", err)
	}
	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("check rows affected failed: %w", err)
	}
	if rowsAffected != 1 {
		return nil, fmt.Errorf("expected 1 row affected updating alias_inventory for %s, got %d: %w", normEmail, rowsAffected, ErrAllocationConflict)
	}

	// 3. 写入 alias_allocations (全新插入，绝不使用覆盖更新)
	if _, err = tx.Exec(`
		INSERT INTO alias_allocations (allocation_id, alias_email, account_id, owner_kind, owner_id, business_tag, allocated_at, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, alloc.AllocationID, alloc.AliasEmail, alloc.AccountID, alloc.OwnerKind, alloc.OwnerID, alloc.BusinessTag, alloc.AllocatedAt, alloc.Status); err != nil {
		return nil, fmt.Errorf("insert alias_allocations failed: %w", err)
	}

	// 4. 写入 lease_records 审计流水
	if _, err = tx.Exec(`
		INSERT INTO lease_records (id, email, account_id, tag, status, allocated_at, completed_at, token_name)
		VALUES (?, ?, ?, ?, 'completed', ?, ?, ?)
	`, alloc.AllocationID, alloc.AliasEmail, alloc.AccountID, alloc.BusinessTag, alloc.AllocatedAt, alloc.AllocatedAt, tokenName); err != nil {
		return nil, fmt.Errorf("insert lease_records failed: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit allocation transaction failed: %w", err)
	}

	return alloc, nil
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
