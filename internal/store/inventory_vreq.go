/**
 * [INPUT]: 依赖 context, database/sql, errors, strings, time, icloud-hme/internal/store (Store, ErrVerificationRequestNotFound)
 * [OUTPUT]: 对外提供 VerificationRequest, VerificationCompletion, ActiveVerificationWatch, VerificationEventInput 类型, ErrConflictActiveRequest, ErrServerBusy, ErrTooManyRequests 错误及 CheckVerificationRequestAdmission, CreateVerificationRequestAtomic, CreateVerificationRequest, GetVerificationRequest, CompleteVerificationRequestResult, CompleteMatchingVerificationRequests, InvalidateVerificationRequestsForGenerationMismatch, ExpireVerificationRequest, InvalidateVerificationRequest, GetActiveVerificationRequestByLease, CountActiveVerificationRequests, CountActiveVerificationRequestsByPrincipal, GetMinBaselineUIDByEmail, ListActiveVerificationWatches, HasActiveVerificationRequests
 * [POS]: internal/store 的持久化取码状态机，提供终态 CAS、容量仲裁、准入预检和活跃观察查询；到期完成显式传播 CAS 错误，完成 CAS 必须显式携带物理来源 (已移除不带来源的旧完成接口)，扫描覆盖绑定当前任务集合，代际失效可限定为已观察任务 ID，批量完成采用先写 UPDATE RETURNING 避免 WAL 快照升级竞争
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
	BaselineSource      string `json:"-"`
	BaselineAccountID   string `json:"-"`
	MatchedEventRef     string `json:"matched_event_ref,omitempty"`
	Code                string `json:"code,omitempty"`
	MagicLink           string `json:"magic_link,omitempty"`
	IdempotencyKey      string `json:"idempotency_key,omitempty"`
	IdempotencyHash     string `json:"idempotency_hash,omitempty"`
}

// VerificationCompletion 包含验证终态的权威结果字段 (PR-04B)
type VerificationCompletion struct {
	Source          string
	Code            string
	MagicLink       string
	MatchedEventRef string
}

// ActiveVerificationWatch 记录当前活跃的验证观察目标 (PR-04B)
type ActiveVerificationWatch struct {
	BaselineSource      string `json:"-"`
	BaselineAccountID   string `json:"-"`
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
	Source      string
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

// CheckVerificationRequestAdmission 在碰 IMAP 前做轻量 SQLite 准入只读预检 (T4)。
// 只做只读条件判定，不持有全局业务写锁，不执行写操作，快速建议性拒绝明显超限请求。
// 最终原子性与权威仲裁由 CreateVerificationRequestAtomic 事务保障。
func (s *Store) CheckVerificationRequestAdmission(
	ctx context.Context,
	principalKind string,
	principalID string,
	leaseID string,
	maxGlobal int,
	maxPerPrincipal int,
) error {
	nowStr := time.Now().UTC().Format(time.RFC3339)

	// 1. 检查同 lease 是否存在活跃任务 (避免同 lease 重复打 IMAP)
	var leaseActiveCount int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(1)
		FROM verification_requests
		WHERE lease_id = ? AND status IN ('pending', 'ready') AND expires_at >= ?
	`, leaseID, nowStr).Scan(&leaseActiveCount)
	if err != nil {
		return err
	}
	if leaseActiveCount > 0 {
		return ErrConflictActiveRequest
	}

	// 2. 检查 principal 活跃任务上限 (不超过 maxPerPrincipal)
	if maxPerPrincipal > 0 {
		var tokenActiveCount int
		err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(1)
			FROM verification_requests
			WHERE principal_kind = ? AND principal_id = ? AND status IN ('pending', 'ready') AND expires_at >= ?
		`, principalKind, principalID, nowStr).Scan(&tokenActiveCount)
		if err != nil {
			return err
		}
		if tokenActiveCount >= maxPerPrincipal {
			return ErrTooManyRequests
		}
	}

	// 3. 检查全局活跃任务上限 (不超过 maxGlobal)
	if maxGlobal > 0 {
		var globalActiveCount int
		err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(1)
			FROM verification_requests
			WHERE status IN ('pending', 'ready') AND expires_at >= ?
		`, nowStr).Scan(&globalActiveCount)
		if err != nil {
			return err
		}
		if globalActiveCount >= maxGlobal {
			return ErrServerBusy
		}
	}

	return nil
}

// CreateVerificationRequestAtomic 在单事务/临界区内原子完成过期清理、活跃冲突检查、容量判定与任务插入 (PR-08 Final Hardening §4)
func (s *Store) CreateVerificationRequestAtomic(
	ctx context.Context,
	req *VerificationRequest,
	maxGlobalActive int,
	maxPerTokenActive int,
) error {
	if err := s.mu.LockContext(ctx); err != nil {
		return err
	}
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

	// 5. 原子插入 (支持 T11 幂等键与指纹字段)
	q := `
	INSERT INTO verification_requests (
		request_id, principal_kind, principal_id, lease_id, alias_email, status, created_at, expires_at,
		baseline_provider, baseline_mailbox, baseline_uidvalidity, baseline_uid, matched_event_ref, code, magic_link,
		idempotency_key, idempotency_hash, baseline_source, baseline_account_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err = tx.ExecContext(ctx, q,
		req.RequestID, req.PrincipalKind, req.PrincipalID, req.LeaseID, req.AliasEmail, req.Status, req.CreatedAt, req.ExpiresAt,
		req.BaselineProvider, req.BaselineMailbox, req.BaselineUIDValidity, req.BaselineUID, req.MatchedEventRef, req.Code, req.MagicLink,
		req.IdempotencyKey, req.IdempotencyHash, req.BaselineSource, req.BaselineAccountID,
	)
	if err != nil {
		if isIdempotencyUniqueConflict(err) {
			return ErrIdempotencyConflict
		}
		return err
	}

	return tx.Commit()
}

func isIdempotencyUniqueConflict(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "uidx_vreq_idempotency") {
		return true
	}
	if strings.Contains(s, "unique constraint failed") && strings.Contains(s, "idempotency_key") {
		return true
	}
	return false
}

// CreateVerificationRequest 创建持久化取码请求 (Section VI)
func (s *Store) CreateVerificationRequest(ctx context.Context, req *VerificationRequest) error {
	if req.BaselineMailbox == "" {
		req.BaselineMailbox = "INBOX"
	}
	q := `
	INSERT INTO verification_requests (
		request_id, principal_kind, principal_id, lease_id, alias_email, status, created_at, expires_at,
		baseline_provider, baseline_mailbox, baseline_uidvalidity, baseline_uid, matched_event_ref, code, magic_link,
		idempotency_key, idempotency_hash, baseline_source, baseline_account_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err := s.db.ExecContext(ctx, q,
		req.RequestID, req.PrincipalKind, req.PrincipalID, req.LeaseID, req.AliasEmail, req.Status, req.CreatedAt, req.ExpiresAt,
		req.BaselineProvider, req.BaselineMailbox, req.BaselineUIDValidity, req.BaselineUID, req.MatchedEventRef, req.Code, req.MagicLink,
		req.IdempotencyKey, req.IdempotencyHash, req.BaselineSource, req.BaselineAccountID,
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
	       matched_event_ref, code, COALESCE(magic_link, ''),
	       COALESCE(idempotency_key, ''), COALESCE(idempotency_hash, ''), COALESCE(baseline_source, ''), COALESCE(baseline_account_id, '')
	FROM verification_requests
	WHERE request_id = ? AND principal_kind = ? AND principal_id = ?
	`
	err := s.db.QueryRowContext(ctx, q, requestID, principalKind, principalID).Scan(
		&req.RequestID, &req.PrincipalKind, &req.PrincipalID, &req.LeaseID, &req.AliasEmail, &req.Status, &req.CreatedAt, &req.ExpiresAt,
		&req.BaselineProvider, &req.BaselineMailbox, &req.BaselineUIDValidity, &req.BaselineUID, &req.MatchedEventRef, &req.Code, &req.MagicLink,
		&req.IdempotencyKey, &req.IdempotencyHash, &req.BaselineSource, &req.BaselineAccountID,
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
	       matched_event_ref, code, COALESCE(magic_link, ''),
	       COALESCE(idempotency_key, ''), COALESCE(idempotency_hash, ''), COALESCE(baseline_source, ''), COALESCE(baseline_account_id, '')
	FROM verification_requests
	WHERE request_id = ?
	`
	err := s.db.QueryRowContext(ctx, q, requestID).Scan(
		&req.RequestID, &req.PrincipalKind, &req.PrincipalID, &req.LeaseID, &req.AliasEmail, &req.Status, &req.CreatedAt, &req.ExpiresAt,
		&req.BaselineProvider, &req.BaselineMailbox, &req.BaselineUIDValidity, &req.BaselineUID, &req.MatchedEventRef, &req.Code, &req.MagicLink,
		&req.IdempotencyKey, &req.IdempotencyHash, &req.BaselineSource, &req.BaselineAccountID,
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
	WHERE request_id = ? AND status IN ('ready', 'pending') AND expires_at > ? AND baseline_source = ?
	`
	res, err := s.db.ExecContext(ctx, q, comp.Code, comp.MagicLink, comp.MatchedEventRef, requestID, nowStr, comp.Source)
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
		expired, _, err := s.ExpireVerificationRequest(ctx, requestID)
		return expired, false, err
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

// GetActiveVerificationRequestByLease 查询指定 lease 是否已有进行中的取码任务 (status IN ('ready', 'pending') 且未过期) (PR-06 Section 9.2)
func (s *Store) GetActiveVerificationRequestByLease(ctx context.Context, leaseID string) (*VerificationRequest, error) {
	leaseID = strings.TrimSpace(leaseID)
	now := time.Now().UTC().Format(time.RFC3339)
	var req VerificationRequest
	q := `
	SELECT request_id, principal_kind, principal_id, lease_id, alias_email, status, created_at, expires_at,
	       baseline_provider, COALESCE(baseline_mailbox, 'INBOX'), baseline_uidvalidity, baseline_uid,
	       matched_event_ref, code, COALESCE(magic_link, ''),
	       COALESCE(idempotency_key, ''), COALESCE(idempotency_hash, ''), COALESCE(baseline_source, ''), COALESCE(baseline_account_id, '')
	FROM verification_requests
	WHERE lease_id = ? AND status IN ('ready', 'pending') AND expires_at > ?
	ORDER BY created_at DESC
	LIMIT 1
	`
	err := s.db.QueryRowContext(ctx, q, leaseID, now).Scan(
		&req.RequestID, &req.PrincipalKind, &req.PrincipalID, &req.LeaseID, &req.AliasEmail, &req.Status, &req.CreatedAt, &req.ExpiresAt,
		&req.BaselineProvider, &req.BaselineMailbox, &req.BaselineUIDValidity, &req.BaselineUID, &req.MatchedEventRef, &req.Code, &req.MagicLink,
		&req.IdempotencyKey, &req.IdempotencyHash, &req.BaselineSource, &req.BaselineAccountID,
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
	       COALESCE(baseline_uidvalidity, 0), COALESCE(baseline_uid, 0), expires_at, baseline_source, baseline_account_id
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
			&w.BaselineProvider, &w.BaselineMailbox, &w.BaselineUIDValidity, &w.BaselineUID, &w.ExpiresAt, &w.BaselineSource, &w.BaselineAccountID,
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

	if err := s.mu.LockContext(ctx); err != nil {
		return nil, err
	}
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 第一条语句直接取得写入权，避免 WAL 读取快照在并发写入后无法升级。
	// 匹配与终态 CAS 在同一条语句中完成；仅在提交成功后返回可发布的结果。
	qUpdate := `
	UPDATE verification_requests
	SET status = 'succeeded', code = ?, magic_link = ?, matched_event_ref = ?
	WHERE status IN ('ready', 'pending')
	  AND (expires_at IS NULL OR expires_at > ?)
	  AND LOWER(TRIM(alias_email)) = ?
	  AND baseline_provider = ?
	  AND (baseline_mailbox = ? OR (baseline_mailbox = '' AND ? = 'INBOX'))
	  AND baseline_uidvalidity = ?
	  AND baseline_uid <= ?
	  AND baseline_source = ?
	RETURNING request_id, principal_kind, principal_id, lease_id, alias_email, status, created_at, expires_at,
	          baseline_provider, COALESCE(baseline_mailbox, 'INBOX'), baseline_uidvalidity, baseline_uid,
	          matched_event_ref, code, COALESCE(magic_link, ''),
	          COALESCE(idempotency_key, ''), COALESCE(idempotency_hash, ''), COALESCE(baseline_source, ''), COALESCE(baseline_account_id, '')
	`
	rows, err := tx.QueryContext(ctx, qUpdate,
		ev.Code, ev.MagicLink, ev.MessageRef, nowStr, normAlias, ev.Provider, ev.Mailbox, ev.Mailbox, ev.UIDValidity, ev.UID, ev.Source,
	)
	if err != nil {
		return nil, err
	}
	var completed []*VerificationRequest
	for rows.Next() {
		var req VerificationRequest
		if err := rows.Scan(
			&req.RequestID, &req.PrincipalKind, &req.PrincipalID, &req.LeaseID, &req.AliasEmail, &req.Status, &req.CreatedAt, &req.ExpiresAt,
			&req.BaselineProvider, &req.BaselineMailbox, &req.BaselineUIDValidity, &req.BaselineUID, &req.MatchedEventRef, &req.Code, &req.MagicLink,
			&req.IdempotencyKey, &req.IdempotencyHash, &req.BaselineSource, &req.BaselineAccountID,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan completed request failed: %w", err)
		}
		completed = append(completed, &req)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate matching requests failed: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close completed requests failed: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return completed, nil
}

// InvalidateVerificationRequestsForSourceMismatch 只更新配置快照之前已观察到的任务，避免旧扫描误伤后来创建的任务。
func (s *Store) InvalidateVerificationRequestsForSourceMismatch(ctx context.Context, requestIDs []string, source string) error {
	const batchSize = 500
	for i := 0; i < len(requestIDs); i += batchSize {
		chunk := requestIDs[i:min(i+batchSize, len(requestIDs))]
		args := []any{source}
		placeholders := make([]string, len(chunk))
		for j, id := range chunk {
			placeholders[j] = "?"
			args = append(args, id)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE verification_requests SET status='invalidated'
   WHERE status IN ('pending','ready') AND baseline_source != ?
   AND request_id IN (`+strings.Join(placeholders, ",")+`)`, args...); err != nil {
			return err
		}
	}
	return nil
}

// InvalidateVerificationRequestsForGenerationMismatch 原子 CAS 将因代际 (UIDVALIDITY) 突变导致失效的活跃取码任务标记为 invalidated (PR-04B)
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

// InvalidateVerificationRequestsForGenerationMismatchByIDs 只失效读取邮箱边界之前已观察到的任务。
// 边界读取与失效之间创建的新代际任务不在 requestIDs 中，旧边界不能误伤它们。
func (s *Store) InvalidateVerificationRequestsForGenerationMismatchByIDs(ctx context.Context, requestIDs []string, mailbox string, uidValidity uint32, source string) (int64, error) {
	if len(requestIDs) == 0 || uidValidity == 0 {
		return 0, nil
	}
	if mailbox == "" {
		mailbox = "INBOX"
	}
	if err := s.mu.LockContext(ctx); err != nil {
		return 0, err
	}
	defer s.mu.Unlock()

	const batchSize = 500
	var totalAffected int64
	for i := 0; i < len(requestIDs); i += batchSize {
		chunk := requestIDs[i:min(i+batchSize, len(requestIDs))]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+4)
		for j, id := range chunk {
			placeholders[j] = "?"
			args = append(args, id)
		}
		args = append(args, mailbox, mailbox, uidValidity, source)
		res, err := s.db.ExecContext(ctx, `
		UPDATE verification_requests
		SET status = 'invalidated'
		WHERE request_id IN (`+strings.Join(placeholders, ",")+`)
		  AND status IN ('ready', 'pending')
		  AND baseline_provider = 'imap'
		  AND (baseline_mailbox = ? OR (baseline_mailbox = '' AND ? = 'INBOX'))
		  AND baseline_uidvalidity > 0
		  AND baseline_uidvalidity != ?
		  AND baseline_source = ?
		`, args...)
		if err != nil {
			return totalAffected, err
		}
		affected, _ := res.RowsAffected()
		totalAffected += affected
	}
	return totalAffected, nil
}

// GetVerificationRequestByIdempotencyKey 根据主体身份和幂等键查询已有任务 (T11)
func (s *Store) GetVerificationRequestByIdempotencyKey(ctx context.Context, principalKind, principalID, key string) (*VerificationRequest, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, nil
	}
	var req VerificationRequest
	q := `
	SELECT request_id, principal_kind, principal_id, lease_id, alias_email, status, created_at, expires_at,
	       baseline_provider, COALESCE(baseline_mailbox, 'INBOX'), baseline_uidvalidity, baseline_uid,
	       matched_event_ref, code, COALESCE(magic_link, ''),
	       COALESCE(idempotency_key, ''), COALESCE(idempotency_hash, ''), COALESCE(baseline_source, ''), COALESCE(baseline_account_id, '')
	FROM verification_requests
	WHERE principal_kind = ? AND principal_id = ? AND idempotency_key = ?
	LIMIT 1
	`
	err := s.db.QueryRowContext(ctx, q, principalKind, principalID, key).Scan(
		&req.RequestID, &req.PrincipalKind, &req.PrincipalID, &req.LeaseID, &req.AliasEmail, &req.Status, &req.CreatedAt, &req.ExpiresAt,
		&req.BaselineProvider, &req.BaselineMailbox, &req.BaselineUIDValidity, &req.BaselineUID, &req.MatchedEventRef, &req.Code, &req.MagicLink,
		&req.IdempotencyKey, &req.IdempotencyHash, &req.BaselineSource, &req.BaselineAccountID,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	// CAS 原子收敛已到期但未被收割的 ready/pending 任务
	if (req.Status == "ready" || req.Status == "pending") && req.ExpiresAt != "" {
		if exp, perr := time.Parse(time.RFC3339, req.ExpiresAt); perr == nil && !time.Now().UTC().Before(exp) {
			updatedReq, _, err := s.ExpireVerificationRequest(ctx, req.RequestID)
			if err != nil {
				return nil, err
			}
			if updatedReq != nil {
				return updatedReq, nil
			}
		}
	}
	return &req, nil
}

// GetMinBaselineUIDsByEmails 批量查询指定别名邮箱集合当前活跃取码任务的最小 baseline_uid (T5)。
// 替换逐邮箱查询的 N+1 循环，分批执行 (每批最多 500 个)。
func (s *Store) GetMinBaselineUIDsByEmails(ctx context.Context, emails []string) (map[string]uint32, error) {
	baselines, _, err := s.GetVerificationScanBaselines(ctx, emails)
	return baselines, err
}

// GetVerificationScanBaselines 将基线和当前任务集合在同一查询中固定；来源过滤隔离新旧邮箱。
func (s *Store) GetVerificationScanBaselines(ctx context.Context, emails []string, source ...string) (map[string]uint32, map[string]string, error) {
	coverage := make(map[string]string)
	result := make(map[string]uint32)
	if len(emails) == 0 {
		return result, coverage, nil
	}

	normSet := make(map[string]struct{}, len(emails))
	var uniqueNorms []string
	for _, e := range emails {
		norm := strings.ToLower(strings.TrimSpace(e))
		if norm != "" {
			if _, exists := normSet[norm]; !exists {
				normSet[norm] = struct{}{}
				uniqueNorms = append(uniqueNorms, norm)
			}
		}
	}
	if len(uniqueNorms) == 0 {
		return result, coverage, nil
	}

	nowStr := time.Now().UTC().Format(time.RFC3339)
	const batchSize = 500

	for i := 0; i < len(uniqueNorms); i += batchSize {
		end := i + batchSize
		if end > len(uniqueNorms) {
			end = len(uniqueNorms)
		}
		chunk := uniqueNorms[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		for j, email := range chunk {
			placeholders[j] = "?"
			args = append(args, email)
		}
		args = append(args, nowStr)
		sourceClause := ""
		if len(source) > 0 {
			sourceClause = " AND baseline_source = ?"
			args = append(args, source[0])
		}

		q := `
		SELECT
			LOWER(TRIM(alias_email)) as norm_email,
			count(*),
			count(CASE WHEN baseline_uid > 0 AND baseline_provider = 'imap' THEN 1 END),
			COALESCE(min(CASE WHEN baseline_uid > 0 AND baseline_provider = 'imap' THEN baseline_uid END), 0),
			json_group_array(request_id)
		FROM verification_requests
		WHERE LOWER(TRIM(alias_email)) IN (` + strings.Join(placeholders, ",") + `)
		  AND status IN ('ready', 'pending')
		  AND (expires_at IS NULL OR expires_at > ?)
		` + sourceClause + ` GROUP BY LOWER(TRIM(alias_email))
		`

		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var normEmail string
			var totalCount, validCount int
			var minUID uint32
			var idsJSON string
			if err := rows.Scan(&normEmail, &totalCount, &validCount, &minUID, &idsJSON); err != nil {
				rows.Close()
				return nil, nil, err
			}
			if totalCount > 0 && totalCount == validCount && minUID > 0 {
				result[normEmail] = minUID
				var ids []string
				if err := json.Unmarshal([]byte(idsJSON), &ids); err != nil {
					rows.Close()
					return nil, nil, err
				}
				sort.Strings(ids)
				encoded, _ := json.Marshal(ids)
				coverage[normEmail] = string(encoded)
			}
		}
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
		rows.Close()
	}

	return result, coverage, nil
}

// InvalidateVerificationRequestsForGenerationMismatchBatch 批量将因代际突变导致失效的任务标记为 invalidated (T5)
func (s *Store) InvalidateVerificationRequestsForGenerationMismatchBatch(ctx context.Context, emails []string, mailbox string, uidValidity uint32, source ...string) (int64, error) {
	if len(emails) == 0 {
		return 0, nil
	}
	if mailbox == "" {
		mailbox = "INBOX"
	}

	normSet := make(map[string]struct{}, len(emails))
	var uniqueNorms []string
	for _, e := range emails {
		norm := strings.ToLower(strings.TrimSpace(e))
		if norm != "" {
			if _, exists := normSet[norm]; !exists {
				normSet[norm] = struct{}{}
				uniqueNorms = append(uniqueNorms, norm)
			}
		}
	}
	if len(uniqueNorms) == 0 {
		return 0, nil
	}

	if err := s.mu.LockContext(ctx); err != nil {
		return 0, err
	}
	defer s.mu.Unlock()

	const batchSize = 500
	var totalAffected int64

	for i := 0; i < len(uniqueNorms); i += batchSize {
		end := i + batchSize
		if end > len(uniqueNorms) {
			end = len(uniqueNorms)
		}
		chunk := uniqueNorms[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+3)
		for j, email := range chunk {
			placeholders[j] = "?"
			args = append(args, email)
		}
		args = append(args, mailbox, mailbox, uidValidity)

		q := `
		UPDATE verification_requests
		SET status = 'invalidated'
		WHERE LOWER(TRIM(alias_email)) IN (` + strings.Join(placeholders, ",") + `)
		  AND status IN ('ready', 'pending')
		  AND baseline_provider = 'imap'
		  AND (baseline_mailbox = ? OR (baseline_mailbox = '' AND ? = 'INBOX'))
		  AND baseline_uidvalidity > 0
		  AND baseline_uidvalidity != ?
		`

		if len(source) > 0 {
			q += " AND baseline_source = ?"
			args = append(args, source[0])
		}
		res, err := s.db.ExecContext(ctx, q, args...)
		if err != nil {
			return totalAffected, err
		}
		affected, _ := res.RowsAffected()
		totalAffected += affected
	}

	return totalAffected, nil
}
