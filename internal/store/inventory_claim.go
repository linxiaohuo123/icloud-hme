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
		  AND (
		      account_id IN (
		          SELECT id FROM accounts
		          WHERE status = 'active'
		            AND (name NOT LIKE '%大号%')
		            AND (tags NOT LIKE '%"personal"%' COLLATE NOCASE)
		            AND (tags NOT LIKE '%"private"%' COLLATE NOCASE)
		            AND (tags NOT LIKE '%"protected"%' COLLATE NOCASE)
		      )
		      OR NOT EXISTS (SELECT 1 FROM accounts)
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
