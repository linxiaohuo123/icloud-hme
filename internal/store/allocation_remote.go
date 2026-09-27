package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func splitAllocationHash(requestHash string) (string, string) {
	if idx := strings.Index(requestHash, "|legacy:"); idx >= 0 {
		return requestHash[:idx], requestHash[idx+len("|legacy:"):]
	}
	return requestHash, ""
}

func allocationHashMatches(stored, state, currentHash, legacyHash string) bool {
	if stored == "" || currentHash == "" {
		return false
	}
	if stored == currentHash {
		return true
	}
	return !strings.HasPrefix(stored, "v2:") && state == "succeeded" && legacyHash != "" && stored == legacyHash
}

func scanRemoteOperation(row *sql.Row) (*Operation, error) {
	var op Operation
	err := row.Scan(
		&op.OperationID, &op.PrincipalKind, &op.PrincipalID, &op.OperationKind,
		&op.IdempotencyKey, &op.RequestHash, &op.State, &op.CandidateEmail,
		&op.ResultRef, &op.ErrorCode, &op.BusinessTag, &op.TokenName,
		&op.ResultSource, &op.CreatedAt, &op.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &op, nil
}

const remoteOperationColumns = `operation_id, principal_kind, principal_id, operation_kind, idempotency_key,
	request_hash, state, COALESCE(candidate_email, ''), COALESCE(result_ref, ''),
	COALESCE(error_code, ''), COALESCE(business_tag, ''), COALESCE(token_name, ''),
	COALESCE(result_source, 'pool'), created_at, updated_at`

// BeginRemoteAllocation claims an idempotency key before any upstream write.
// A successful replay returns an allocation; a new pending operation returns only the operation.
func (s *Store) BeginRemoteAllocation(
	ctx context.Context, principalKind, principalID, operationKind, idempotencyKey,
	requestHash, tag, tokenName string,
) (*AliasAllocation, *Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	currentHash, legacyHash := splitAllocationHash(requestHash)
	if currentHash == "" {
		return nil, nil, errors.New("allocation request hash is required")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	if idempotencyKey != "" {
		row := tx.QueryRowContext(ctx, `SELECT `+remoteOperationColumns+`
			FROM operations WHERE principal_kind = ? AND principal_id = ? AND operation_kind = ? AND idempotency_key = ?`,
			principalKind, principalID, operationKind, idempotencyKey)
		op, queryErr := scanRemoteOperation(row)
		if queryErr == nil {
			if !allocationHashMatches(op.RequestHash, op.State, currentHash, legacyHash) {
				return nil, op, ErrIdempotencyConflict
			}
			switch op.State {
			case "succeeded":
				var alloc AliasAllocation
				err = tx.QueryRowContext(ctx, `SELECT allocation_id, alias_email, account_id, owner_kind, owner_id, business_tag, allocated_at, status
					FROM alias_allocations WHERE allocation_id = ?`, op.ResultRef).Scan(
					&alloc.AllocationID, &alloc.AliasEmail, &alloc.AccountID, &alloc.OwnerKind,
					&alloc.OwnerID, &alloc.BusinessTag, &alloc.AllocatedAt, &alloc.Status)
				if err != nil {
					return nil, op, fmt.Errorf("load succeeded allocation for operation %s: %w", op.OperationID, err)
				}
				if alloc.OwnerKind != principalKind || alloc.OwnerID != principalID ||
					(op.CandidateEmail != "" && !strings.EqualFold(alloc.AliasEmail, op.CandidateEmail)) {
					return nil, op, fmt.Errorf("inconsistent succeeded allocation operation %s", op.OperationID)
				}
				return &alloc, op, nil
			case "pending":
				return nil, op, ErrOperationPending
			case "outcome_unknown":
				return nil, op, ErrOperationOutcomeUnknown
			case "failed":
				if op.ErrorCode != "NO_AVAILABLE_INVENTORY" && op.ErrorCode != "SAFE_BEFORE_RESERVE" && op.ErrorCode != "CONFIRMED_NOT_APPLIED" {
					return nil, op, fmt.Errorf("previous operation failed with code: %s", op.ErrorCode)
				}
				res, updateErr := tx.ExecContext(ctx, `UPDATE operations
					SET state = 'pending', request_hash = ?, business_tag = ?, token_name = ?,
						result_source = 'created', candidate_email = '', error_code = '', updated_at = ?
					WHERE operation_id = ? AND state = 'failed'`, currentHash, tag, tokenName, now, op.OperationID)
				if updateErr != nil {
					return nil, op, updateErr
				}
				rows, rowsErr := res.RowsAffected()
				if rowsErr != nil || rows != 1 {
					return nil, op, fmt.Errorf("retry operation %s was not claimed: rows=%d, error=%v", op.OperationID, rows, rowsErr)
				}
				op.State, op.RequestHash, op.BusinessTag, op.TokenName = "pending", currentHash, tag, tokenName
				op.ResultSource, op.CandidateEmail, op.ErrorCode, op.UpdatedAt = "created", "", "", now
				if err := tx.Commit(); err != nil {
					return nil, op, err
				}
				return nil, op, nil
			default:
				return nil, op, fmt.Errorf("unknown operation state: %s", op.State)
			}
		}
		if !errors.Is(queryErr, sql.ErrNoRows) {
			return nil, nil, queryErr
		}
	}

	opID := NewOpaqueID("op_")
	storedKey := idempotencyKey
	if storedKey == "" {
		storedKey = "none:" + opID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO operations
		(operation_id, principal_kind, principal_id, operation_kind, idempotency_key, request_hash,
		 state, business_tag, token_name, result_source, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, ?, 'created', ?, ?)`,
		opID, principalKind, principalID, operationKind, storedKey, currentHash, tag, tokenName, now, now)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return nil, &Operation{
		OperationID: opID, PrincipalKind: principalKind, PrincipalID: principalID,
		OperationKind: operationKind, IdempotencyKey: idempotencyKey, RequestHash: currentHash,
		State: "pending", BusinessTag: tag, TokenName: tokenName, ResultSource: "created",
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

// MarkRemoteAllocationError leaves any operation with an unresolved Reserve blocked from replay.
func (s *Store) MarkRemoteAllocationError(ctx context.Context, operationID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var operationState string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM operations WHERE operation_id = ?`, operationID).Scan(&operationState); err != nil {
		return err
	}
	if operationState == "succeeded" {
		return nil
	}
	var intentState string
	err = tx.QueryRowContext(ctx, `SELECT state FROM hme_reserve_intents WHERE operation_id = ? ORDER BY rowid DESC LIMIT 1`, operationID).Scan(&intentState)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	state, code := "failed", "SAFE_BEFORE_RESERVE"
	switch intentState {
	case IntentStateConfirmedFailed:
		code = "CONFIRMED_NOT_APPLIED"
	case "":
	default:
		state, code = "outcome_unknown", "UPSTREAM_OUTCOME_UNKNOWN"
	}
	res, err := tx.ExecContext(ctx, `UPDATE operations SET state = ?, error_code = ?, updated_at = ?
		WHERE operation_id = ? AND state IN ('pending', 'outcome_unknown')`, state, code, time.Now().UTC().Format(time.RFC3339), operationID)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil || rows != 1 {
		return fmt.Errorf("update remote allocation error state: rows=%d, error=%v", rows, err)
	}
	return tx.Commit()
}

// ListRemoteAllocationRecoveries returns operations whose remote creation was not finalized locally.
func (s *Store) ListRemoteAllocationRecoveries(ctx context.Context) ([]Operation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+remoteOperationColumns+`
		FROM operations WHERE result_source = 'created' AND state IN ('pending', 'outcome_unknown') ORDER BY created_at, operation_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var operations []Operation
	for rows.Next() {
		var op Operation
		if err := rows.Scan(&op.OperationID, &op.PrincipalKind, &op.PrincipalID, &op.OperationKind,
			&op.IdempotencyKey, &op.RequestHash, &op.State, &op.CandidateEmail, &op.ResultRef,
			&op.ErrorCode, &op.BusinessTag, &op.TokenName, &op.ResultSource, &op.CreatedAt, &op.UpdatedAt); err != nil {
			return nil, err
		}
		operations = append(operations, op)
	}
	return operations, rows.Err()
}

func (s *Store) GetLatestReserveIntentForOperation(ctx context.Context, operationID string) (*HmeReserveIntent, error) {
	var intent HmeReserveIntent
	err := s.db.QueryRowContext(ctx, `SELECT intent_id, account_id, candidate_email, COALESCE(label, ''), state,
		COALESCE(anonymous_id, ''), COALESCE(result_ref, ''), COALESCE(error_message, ''), COALESCE(operation_id, ''), created_at, updated_at
		FROM hme_reserve_intents WHERE operation_id = ? ORDER BY rowid DESC LIMIT 1`, operationID).Scan(
		&intent.IntentID, &intent.AccountID, &intent.CandidateEmail, &intent.Label, &intent.State,
		&intent.AnonymousID, &intent.ResultRef, &intent.ErrorMessage, &intent.OperationID,
		&intent.CreatedAt, &intent.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &intent, nil
}

func (s *Store) GetRemoteAllocationOperation(ctx context.Context, operationID string) (*Operation, error) {
	return scanRemoteOperation(s.db.QueryRowContext(ctx, `SELECT `+remoteOperationColumns+` FROM operations WHERE operation_id = ?`, operationID))
}

// RecoverRemoteAllocationOperation finalizes only a candidate whose Reserve intent is proven successful.
func (s *Store) RecoverRemoteAllocationOperation(ctx context.Context, operationID string) (*AliasAllocation, error) {
	op, err := s.GetRemoteAllocationOperation(ctx, operationID)
	if err != nil {
		return nil, err
	}
	if op.State == "succeeded" {
		return nil, nil
	}
	if op.State != "pending" && op.State != "outcome_unknown" {
		return nil, nil
	}
	intent, err := s.GetLatestReserveIntentForOperation(ctx, operationID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, s.MarkRemoteAllocationError(ctx, operationID)
	}
	if err != nil {
		return nil, err
	}
	switch intent.State {
	case IntentStateSucceeded:
		return s.ReconcileUnknownOperation(ctx, operationID, ReconciliationFound,
			intent.CandidateEmail, intent.AccountID, op.BusinessTag, op.PrincipalKind, op.PrincipalID, op.TokenName)
	case IntentStateConfirmedFailed:
		return nil, s.MarkRemoteAllocationError(ctx, operationID)
	default:
		return nil, s.MarkOperationOutcomeUnknown(ctx, operationID, intent.CandidateEmail, "UPSTREAM_OUTCOME_UNKNOWN")
	}
}

func (s *Store) RecoverRemoteAllocationOperations(ctx context.Context) (int, error) {
	operations, err := s.ListRemoteAllocationRecoveries(ctx)
	if err != nil {
		return 0, err
	}
	var recovered int
	var failures []error
	for _, op := range operations {
		alloc, err := s.RecoverRemoteAllocationOperation(ctx, op.OperationID)
		if err != nil {
			failures = append(failures, fmt.Errorf("recover operation %s: %w", op.OperationID, err))
			continue
		}
		if alloc != nil {
			recovered++
		}
	}
	return recovered, errors.Join(failures...)
}
