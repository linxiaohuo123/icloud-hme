package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type IntentState = string

const (
	IntentStatePrepared        IntentState = "prepared"
	IntentStateReserveSent     IntentState = "reserve_sent"
	IntentStateOutcomeUnknown  IntentState = "outcome_unknown"
	IntentStateSucceeded       IntentState = "succeeded"
	IntentStateConfirmedFailed IntentState = "confirmed_failed"
)

// HmeReserveIntent 记录上游 HME Reserve 写操作的持久化意图状态机 (F03)
type HmeReserveIntent struct {
	IntentID       string `json:"intent_id"`
	AccountID      string `json:"account_id"`
	CandidateEmail string `json:"candidate_email"`
	Label          string `json:"label"`
	State          string `json:"state"`
	AnonymousID    string `json:"anonymous_id"`
	ResultRef      string `json:"result_ref"`
	ErrorMessage   string `json:"error_message"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

// CreateReserveIntent 在向 Apple 发送 Reserve 写请求前持久化 candidate A 的意图 (prepared) 并提交事务
func (s *Store) CreateReserveIntent(ctx context.Context, accountID, candidateEmail, label string) (*HmeReserveIntent, error) {
	accountID = strings.TrimSpace(accountID)
	candidateEmail = strings.TrimSpace(strings.ToLower(candidateEmail))
	if accountID == "" || candidateEmail == "" {
		return nil, errors.New("account_id and candidate_email must not be empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)
	intentID := NewOpaqueID("intent_")

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO hme_reserve_intents (
			intent_id, account_id, candidate_email, label, state, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`, intentID, accountID, candidateEmail, label, IntentStatePrepared, now, now)
	if err != nil {
		return nil, err
	}

	return &HmeReserveIntent{
		IntentID:       intentID,
		AccountID:      accountID,
		CandidateEmail: candidateEmail,
		Label:          label,
		State:          IntentStatePrepared,
		CreatedAt:      now,
		UpdatedAt:      now,
	}, nil
}

// UpdateReserveIntentState 原子更新写操作意图的状态及相关结果或错误
func (s *Store) UpdateReserveIntentState(ctx context.Context, intentID, state, anonymousID, resultRef, errMsg string) error {
	intentID = strings.TrimSpace(intentID)
	state = strings.TrimSpace(state)
	if intentID == "" || state == "" {
		return errors.New("intent_id and state must not be empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		UPDATE hme_reserve_intents
		SET state = ?,
		    anonymous_id = CASE WHEN ? != '' THEN ? ELSE anonymous_id END,
		    result_ref = CASE WHEN ? != '' THEN ? ELSE result_ref END,
		    error_message = CASE WHEN ? != '' THEN ? ELSE error_message END,
		    updated_at = ?
		WHERE intent_id = ?
	`, state, anonymousID, anonymousID, resultRef, resultRef, errMsg, errMsg, now, intentID)
	return err
}

// GetReserveIntent 获取指定意图记录
func (s *Store) GetReserveIntent(ctx context.Context, intentID string) (*HmeReserveIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var intent HmeReserveIntent
	err := s.db.QueryRowContext(ctx, `
		SELECT intent_id, account_id, candidate_email, COALESCE(label, ''), state, COALESCE(anonymous_id, ''), COALESCE(result_ref, ''), COALESCE(error_message, ''), created_at, updated_at
		FROM hme_reserve_intents
		WHERE intent_id = ?
	`, intentID).Scan(
		&intent.IntentID, &intent.AccountID, &intent.CandidateEmail, &intent.Label, &intent.State, &intent.AnonymousID, &intent.ResultRef, &intent.ErrorMessage, &intent.CreatedAt, &intent.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("intent not found")
		}
		return nil, err
	}
	return &intent, nil
}

// ListUnresolvedReserveIntents 查询所有未决 (prepared, reserve_sent, outcome_unknown) 的 Reserve 意图
func (s *Store) ListUnresolvedReserveIntents(ctx context.Context, accountID string) ([]HmeReserveIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	accountID = strings.TrimSpace(accountID)
	var rows *sql.Rows
	var err error

	if accountID != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT intent_id, account_id, candidate_email, COALESCE(label, ''), state, COALESCE(anonymous_id, ''), COALESCE(result_ref, ''), COALESCE(error_message, ''), created_at, updated_at
			FROM hme_reserve_intents
			WHERE account_id = ? AND state IN ('prepared', 'reserve_sent', 'outcome_unknown')
			ORDER BY created_at ASC
		`, accountID)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT intent_id, account_id, candidate_email, COALESCE(label, ''), state, COALESCE(anonymous_id, ''), COALESCE(result_ref, ''), COALESCE(error_message, ''), created_at, updated_at
			FROM hme_reserve_intents
			WHERE state IN ('prepared', 'reserve_sent', 'outcome_unknown')
			ORDER BY created_at ASC
		`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []HmeReserveIntent
	for rows.Next() {
		var it HmeReserveIntent
		if scanErr := rows.Scan(
			&it.IntentID, &it.AccountID, &it.CandidateEmail, &it.Label, &it.State, &it.AnonymousID, &it.ResultRef, &it.ErrorMessage, &it.CreatedAt, &it.UpdatedAt,
		); scanErr != nil {
			return nil, scanErr
		}
		result = append(result, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// FindLatestIntentForCandidate 查找指定候选邮箱最近的一条 Reserve 意图
func (s *Store) FindLatestIntentForCandidate(ctx context.Context, candidateEmail string) (*HmeReserveIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	candidateEmail = strings.TrimSpace(strings.ToLower(candidateEmail))
	var intent HmeReserveIntent
	err := s.db.QueryRowContext(ctx, `
		SELECT intent_id, account_id, candidate_email, COALESCE(label, ''), state, COALESCE(anonymous_id, ''), COALESCE(result_ref, ''), COALESCE(error_message, ''), created_at, updated_at
		FROM hme_reserve_intents
		WHERE candidate_email = ?
		ORDER BY created_at DESC
		LIMIT 1
	`, candidateEmail).Scan(
		&intent.IntentID, &intent.AccountID, &intent.CandidateEmail, &intent.Label, &intent.State, &intent.AnonymousID, &intent.ResultRef, &intent.ErrorMessage, &intent.CreatedAt, &intent.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("intent not found")
		}
		return nil, err
	}
	return &intent, nil
}
