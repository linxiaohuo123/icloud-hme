/**
 * [INPUT]: 依赖 context, database/sql, errors, strings, time, icloud-hme/internal/store (Store, ErrVerificationRequestNotFound)
 * [OUTPUT]: 对外提供 VerificationRequest, VerificationCompletion, ActiveVerificationWatch, VerificationEventInput 类型, ErrConflictActiveRequest, ErrServerBusy, ErrTooManyRequests 错误及 CreateVerificationRequestAtomic, CreateVerificationRequest, GetVerificationRequest, CompleteVerificationRequest, CompleteVerificationRequestResult, CompleteMatchingVerificationRequests, InvalidateVerificationRequestsForGenerationMismatch, ExpireVerificationRequest, InvalidateVerificationRequest, UpdateVerificationRequestResult, GetActiveVerificationRequestByLease, CountActiveVerificationRequests, CountActiveVerificationRequestsByPrincipal, GetMinBaselineUIDByEmail, ListActiveVerificationWatches, HasActiveVerificationRequests
 * [POS]: internal/store 的持久化取码请求与基线游标状态机领域 (PR-06/PR-08/PR-04B)，提供终态原子 CAS 与容量仲裁、权威持久化和持久化活跃观察查询
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

var (
	ErrConflictActiveRequest = errors.New("active verification request already exists for this lease")
	ErrServerBusy            = errors.New("global active verification requests limit exceeded")
	ErrTooManyRequests       = errors.New("per-principal active verification requests limit exceeded")
)

// VerificationRequest 持久化取码请求实体 (Section VI, PR-04B)
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
	MagicLink           string `json:"magic_link,omitempty"`
}

// VerificationCompletion 包含验证终态的权威结果字段 (PR-04B)
type VerificationCompletion struct {
	Code            string
	MagicLink       string
	MatchedEventRef string
}

// ActiveVerificationWatch 记录当前活跃的验证观察目标 (PR-04B)
type ActiveVerificationWatch struct {
	RequestID           string `json:"request_id"`
	PrincipalKind       string `json:"principal_kind"`
	PrincipalID         string `json:"principal_id"`
	LeaseID             string `json:"lease_id"`
	AliasEmail          string `json:"alias_email"`
	BaselineProvider    string `json:"baseline_provider"`
	BaselineMailbox     string `json:"baseline_mailbox"`
	BaselineUIDValidity uint32 `json:"baseline_uidvalidity"`
	BaselineUID         uint32 `json:"baseline_uid"`
	ExpiresAt           string `json:"expires_at"`
}

// VerificationEventInput 供原子完成匹配的事件入参 (PR-04B)
type VerificationEventInput struct {
	AliasEmail  string
	Provider    string
	Mailbox     string
	UIDValidity uint32
	UID         uint32
	MessageRef  string
	Code        string
	MagicLink   string
	Now         time.Time
}

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
		baseline_provider, baseline_mailbox, baseline_uidvalidity, baseline_uid, matched_event_ref, code, magic_link
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err = tx.ExecContext(ctx, q,
		req.RequestID, req.PrincipalKind, req.PrincipalID, req.LeaseID, req.AliasEmail, req.Status, req.CreatedAt, req.ExpiresAt,
		req.BaselineProvider, req.BaselineMailbox, req.BaselineUIDValidity, req.BaselineUID, req.MatchedEventRef, req.Code, req.MagicLink,
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
		baseline_provider, baseline_mailbox, baseline_uidvalidity, baseline_uid, matched_event_ref, code, magic_link
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err := s.db.ExecContext(ctx, q,
		req.RequestID, req.PrincipalKind, req.PrincipalID, req.LeaseID, req.AliasEmail, req.Status, req.CreatedAt, req.ExpiresAt,
		req.BaselineProvider, req.BaselineMailbox, req.BaselineUIDValidity, req.BaselineUID, req.MatchedEventRef, req.Code, req.MagicLink,
	)
	return err
}

// GetVerificationRequest 获取持久化取码请求 (Section VI)
func (s *Store) GetVerificationRequest(ctx context.Context, requestID, principalKind, principalID string) (*VerificationRequest, error) {
	requestID = strings.TrimSpace(requestID)
	var req VerificationRequest
	q := `
	SELECT request_id, principal_kind, principal_id, lease_id, alias_email, status, created_at, expires_at,
	       baseline_provider, COALESCE(baseline_mailbox, 'INBOX'), baseline_uidvalidity, baseline_uid,
	       matched_event_ref, code, COALESCE(magic_link, '')
	FROM verification_requests
	WHERE request_id = ? AND principal_kind = ? AND principal_id = ?
	`
	err := s.db.QueryRowContext(ctx, q, requestID, principalKind, principalID).Scan(
		&req.RequestID, &req.PrincipalKind, &req.PrincipalID, &req.LeaseID, &req.AliasEmail, &req.Status, &req.CreatedAt, &req.ExpiresAt,
		&req.BaselineProvider, &req.BaselineMailbox, &req.BaselineUIDValidity, &req.BaselineUID, &req.MatchedEventRef, &req.Code, &req.MagicLink,
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
	       baseline_provider, COALESCE(baseline_mailbox, 'INBOX'), baseline_uidvalidity, baseline_uid,
	       matched_event_ref, code, COALESCE(magic_link, '')
	FROM verification_requests
	WHERE request_id = ?
	`
	err := s.db.QueryRowContext(ctx, q, requestID).Scan(
		&req.RequestID, &req.PrincipalKind, &req.PrincipalID, &req.LeaseID, &req.AliasEmail, &req.Status, &req.CreatedAt, &req.ExpiresAt,
		&req.BaselineProvider, &req.BaselineMailbox, &req.BaselineUIDValidity, &req.BaselineUID, &req.MatchedEventRef, &req.Code, &req.MagicLink,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrVerificationRequestNotFound
		}
		return nil, err
	}
	return &req, nil
}

// CompleteVerificationRequestResult 原子 CAS 将取码任务标记为成功，并持久化 Code 与 MagicLink (PR-04B)
func (s *Store) CompleteVerificationRequestResult(ctx context.Context, requestID string, comp VerificationCompletion, now ...time.Time) (*VerificationRequest, bool, error) {
	requestID = strings.TrimSpace(requestID)
	currentTime := time.Now().UTC()
	if len(now) > 0 && !now[0].IsZero() {
		currentTime = now[0].UTC()
	}
	nowStr := currentTime.Format(time.RFC3339)

	q := `
	UPDATE verification_requests
	SET status = 'succeeded', code = ?, magic_link = ?, matched_event_ref = ?
	WHERE request_id = ? AND status IN ('ready', 'pending') AND expires_at > ?
	`
	res, err := s.db.ExecContext(ctx, q, comp.Code, comp.MagicLink, comp.MatchedEventRef, requestID, nowStr)
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

// CompleteVerificationRequest 原子 CAS 将取码任务标记为成功 (向下兼容旧签名)
func (s *Store) CompleteVerificationRequest(ctx context.Context, requestID, code, matchedEventRef string, now ...time.Time) (*VerificationRequest, bool, error) {
	return s.CompleteVerificationRequestResult(ctx, requestID, VerificationCompletion{
		Code:            code,
		MatchedEventRef: matchedEventRef,
	}, now...)
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
	       baseline_provider, COALESCE(baseline_mailbox, 'INBOX'), baseline_uidvalidity, baseline_uid,
	       matched_event_ref, code, COALESCE(magic_link, '')
	FROM verification_requests
	WHERE lease_id = ? AND status IN ('ready', 'pending') AND expires_at > ?
	ORDER BY created_at DESC
	LIMIT 1
	`
	err := s.db.QueryRowContext(ctx, q, leaseID, now).Scan(
		&req.RequestID, &req.PrincipalKind, &req.PrincipalID, &req.LeaseID, &req.AliasEmail, &req.Status, &req.CreatedAt, &req.ExpiresAt,
		&req.BaselineProvider, &req.BaselineMailbox, &req.BaselineUIDValidity, &req.BaselineUID, &req.MatchedEventRef, &req.Code, &req.MagicLink,
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

// ListActiveVerificationWatches 查询当前数据库中所有活跃未过期的取码任务观察列表 (PR-04B)
func (s *Store) ListActiveVerificationWatches(ctx context.Context) ([]ActiveVerificationWatch, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	q := `
	SELECT request_id, principal_kind, principal_id, lease_id, alias_email,
	       COALESCE(baseline_provider, ''), COALESCE(baseline_mailbox, 'INBOX'),
	       COALESCE(baseline_uidvalidity, 0), COALESCE(baseline_uid, 0), expires_at
	FROM verification_requests
	WHERE status IN ('ready', 'pending') AND (expires_at IS NULL OR expires_at > ?)
	ORDER BY created_at ASC
	`
	rows, err := s.db.QueryContext(ctx, q, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var watches []ActiveVerificationWatch
	for rows.Next() {
		var w ActiveVerificationWatch
		if err := rows.Scan(
			&w.RequestID, &w.PrincipalKind, &w.PrincipalID, &w.LeaseID, &w.AliasEmail,
			&w.BaselineProvider, &w.BaselineMailbox, &w.BaselineUIDValidity, &w.BaselineUID, &w.ExpiresAt,
		); err != nil {
			return nil, err
		}
		watches = append(watches, w)
	}
	return watches, rows.Err()
}

// HasActiveVerificationRequests 判断当前数据库是否存在活跃未过期的取码任务 (PR-04B)
func (s *Store) HasActiveVerificationRequests(ctx context.Context) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	q := `
	SELECT 1 FROM verification_requests
	WHERE status IN ('ready', 'pending') AND (expires_at IS NULL OR expires_at > ?)
	LIMIT 1
	`
	var exists int
	err := s.db.QueryRowContext(ctx, q, now).Scan(&exists)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// CompleteMatchingVerificationRequests 原子匹配并完成所有符合条件的活跃取码任务 (PR-04B)
// 满足条件：status IN ('ready','pending'), expires_at > now, alias_email 一致,
// baseline_provider='imap', baseline_mailbox 与 event mailbox 一致, baseline_uidvalidity=event uidvalidity,
// 且 event uid >= baseline_uid。
// 满足时原子 CAS 置为 succeeded，并持久化 code 与 magic_link。返回本次成功更新的任务列表。
func (s *Store) CompleteMatchingVerificationRequests(ctx context.Context, ev VerificationEventInput) ([]*VerificationRequest, error) {
	if ev.Provider == "" {
		ev.Provider = "imap"
	}
	if ev.Mailbox == "" {
		ev.Mailbox = "INBOX"
	}
	normAlias := strings.ToLower(strings.TrimSpace(ev.AliasEmail))
	nowTime := ev.Now
	if nowTime.IsZero() {
		nowTime = time.Now().UTC()
	}
	nowStr := nowTime.Format(time.RFC3339)

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 1. 查询所有符合 baseline 条件的活跃任务 ID
	qSelect := `
	SELECT request_id
	FROM verification_requests
	WHERE status IN ('ready', 'pending')
	  AND (expires_at IS NULL OR expires_at > ?)
	  AND LOWER(TRIM(alias_email)) = ?
	  AND baseline_provider = ?
	  AND (baseline_mailbox = ? OR (baseline_mailbox = '' AND ? = 'INBOX'))
	  AND baseline_uidvalidity = ?
	  AND baseline_uid <= ?
	`
	rows, err := tx.QueryContext(ctx, qSelect,
		nowStr, normAlias, ev.Provider, ev.Mailbox, ev.Mailbox, ev.UIDValidity, ev.UID,
	)
	if err != nil {
		return nil, err
	}
	var matchingIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			matchingIDs = append(matchingIDs, id)
		}
	}
	rows.Close()

	if len(matchingIDs) == 0 {
		return nil, nil
	}

	// 2. 对每个匹配到的 request 进行原子 CAS 更新，拒绝覆盖终态
	qUpdate := `
	UPDATE verification_requests
	SET status = 'succeeded', code = ?, magic_link = ?, matched_event_ref = ?
	WHERE request_id = ? AND status IN ('ready', 'pending') AND (expires_at IS NULL OR expires_at > ?)
	`
	var completed []*VerificationRequest
	for _, reqID := range matchingIDs {
		res, err := tx.ExecContext(ctx, qUpdate, ev.Code, ev.MagicLink, ev.MessageRef, reqID, nowStr)
		if err != nil {
			return nil, err
		}
		ra, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if ra > 0 {
			var req VerificationRequest
			qGet := `
			SELECT request_id, principal_kind, principal_id, lease_id, alias_email, status, created_at, expires_at,
			       baseline_provider, COALESCE(baseline_mailbox, 'INBOX'), baseline_uidvalidity, baseline_uid,
			       matched_event_ref, code, COALESCE(magic_link, '')
			FROM verification_requests
			WHERE request_id = ?
			`
			if err := tx.QueryRowContext(ctx, qGet, reqID).Scan(
				&req.RequestID, &req.PrincipalKind, &req.PrincipalID, &req.LeaseID, &req.AliasEmail, &req.Status, &req.CreatedAt, &req.ExpiresAt,
				&req.BaselineProvider, &req.BaselineMailbox, &req.BaselineUIDValidity, &req.BaselineUID, &req.MatchedEventRef, &req.Code, &req.MagicLink,
			); err == nil {
				completed = append(completed, &req)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return completed, nil
}

// InvalidateVerificationRequestsForGenerationMismatch 原子 CAS 将因代际 (UIDVALIDITY) 突变导致失效的活跃取码任务标记为 invalidated (PR-04B)
// 仅影响 status IN ('ready', 'pending') 的未终态任务，已经处于终态的绝不覆盖。
func (s *Store) InvalidateVerificationRequestsForGenerationMismatch(
	ctx context.Context,
	alias string,
	mailbox string,
	currentUIDValidity uint32,
) (int64, error) {
	if currentUIDValidity == 0 {
		return 0, nil
	}
	if mailbox == "" {
		mailbox = "INBOX"
	}
	normAlias := strings.ToLower(strings.TrimSpace(alias))

	q := `
	UPDATE verification_requests
	SET status = 'invalidated'
	WHERE status IN ('ready', 'pending')
	  AND baseline_provider = 'imap'
	  AND LOWER(TRIM(alias_email)) = ?
	  AND (baseline_mailbox = ? OR (baseline_mailbox = '' AND ? = 'INBOX'))
	  AND baseline_uidvalidity > 0
	  AND baseline_uidvalidity != ?
	`
	res, err := s.db.ExecContext(ctx, q, normAlias, mailbox, mailbox, currentUIDValidity)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
