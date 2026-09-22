/**
 * [INPUT]: 依赖 database/sql, errors, fmt, log, strings, sync/atomic, time, icloud-hme/internal/store (Store)
 * [OUTPUT]: 对外提供 LeaseRecord 类型, PoolCandidate 类型, UpsertAliasRoutes, FindAliasRoute, CountAliasRoutes, DeleteAliasRoutesForAccount, FindLeaseAccount, CountLeases, PruneLeases, ListLeases, RecordLease, UpdateLeaseStatus, ClaimPoolAlias, ClaimPoolAliasByRoutes, CountAvailablePoolAliases, CountConsumedPoolAliases
 * [POS]: internal/store 的别名认领与出号流水审计领域，维护 alias_routes 路由表与 lease_records 审计日志
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// LeaseRecord 已用别名流水记录
type LeaseRecord struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	AccountID   string `json:"account_id"`
	Tag         string `json:"tag"`
	Status      string `json:"status"` // "completed" | "leased" | "abandoned"
	AllocatedAt string `json:"allocated_at"`
	CompletedAt string `json:"completed_at,omitempty"`
	TokenName   string `json:"token_name,omitempty"`
}

// PoolCandidate 待从别名池领用的候选别名
type PoolCandidate struct {
	AccountID string
	Email     string
}

// ────────────────────────────────────────────────────────────────
// 别名路由 (alias_routes)
// ────────────────────────────────────────────────────────────────

// UpsertAliasRoutes 批量登记「别名邮箱 → 母号」归属(单事务)。
func (s *Store) UpsertAliasRoutes(accountID string, emails []string) error {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" || len(emails) == 0 {
		return nil
	}
	now := time.Now().Format(time.RFC3339)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`
		INSERT INTO alias_routes (email, account_id, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(email) DO UPDATE SET
			account_id = excluded.account_id,
			updated_at = excluded.updated_at`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()

	for _, email := range emails {
		email = normalizeEmail(email)
		if email == "" {
			continue
		}
		if _, err := stmt.Exec(email, accountID, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FindAliasRoute 按别名邮箱反查归属母号(主键点查)。
func (s *Store) FindAliasRoute(email string) (string, bool) {
	email = normalizeEmail(email)
	if email == "" {
		return "", false
	}
	var accountID string
	if err := s.db.QueryRow(`SELECT account_id FROM alias_routes WHERE email = ?`, email).Scan(&accountID); err != nil {
		return "", false
	}
	if accountID == "" {
		return "", false
	}
	return accountID, true
}

// CountAliasRoutes 返回已登记的路由条数，供健康检查与日志观测。
func (s *Store) CountAliasRoutes() int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM alias_routes`).Scan(&n)
	return n
}

// DeleteAliasRoutesForAccount 清理某账号的全部路由(账号注销时级联)。
func (s *Store) DeleteAliasRoutesForAccount(accountID string) error {
	_, err := s.db.Exec(`DELETE FROM alias_routes WHERE account_id = ?`, accountID)
	return err
}

// settingKeyAliasRoutesBackfilled 标记路由表是否已从流水回填过，避免每次启动都全表扫流水。
const settingKeyAliasRoutesBackfilled = "alias_routes_backfilled"

// backfillAliasRoutes 用出号流水回填别名路由。
func (s *Store) backfillAliasRoutes() {
	if s.GetSetting(settingKeyAliasRoutesBackfilled) == "1" && s.hasAnyAliasRoute() {
		return
	}
	res, err := s.db.Exec(`
		INSERT OR IGNORE INTO alias_routes (email, account_id, updated_at)
		SELECT LOWER(email), account_id, COALESCE(NULLIF(allocated_at, ''), datetime('now'))
		FROM lease_records
		WHERE account_id != '' AND email != ''`)
	if err != nil {
		log.Printf("[Store] 别名路由回填失败(下次启动重试；取码链路另有按需自愈): %v", err)
		return
	}
	if err := s.SaveSetting(settingKeyAliasRoutesBackfilled, "1"); err != nil {
		log.Printf("[Store] 路由回填标记写入失败(下次启动会重复回填，结果幂等): %v", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[Store] 已从出号流水回填 %d 条别名路由，当前共 %d 条", n, s.CountAliasRoutes())
	}
}

// hasAnyAliasRoute 判断路由表是否为空(O(1)，走主键索引)。
func (s *Store) hasAnyAliasRoute() bool {
	var one int
	return s.db.QueryRow(`SELECT 1 FROM alias_routes LIMIT 1`).Scan(&one) == nil
}

// FindLeaseAccount 按别名邮箱反查最近一次出号归属的账号 ID。
func (s *Store) FindLeaseAccount(email string) (string, bool) {
	email = normalizeEmail(email)
	if email == "" {
		return "", false
	}
	var accountID string
	err := s.db.QueryRow(
		`SELECT account_id FROM lease_records WHERE LOWER(email) = ? AND account_id != '' ORDER BY allocated_at DESC LIMIT 1`,
		email,
	).Scan(&accountID)
	if err != nil || accountID == "" {
		return "", false
	}
	return accountID, true
}

// CountLeases 返回流水总条数。
func (s *Store) CountLeases() int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM lease_records`).Scan(&n)
	return n
}

// PruneLeases 分批删除早于 cutoff 的已用别名流水，返回本次删除条数。
func (s *Store) PruneLeases(cutoff time.Time, batch int) (int, error) {
	if batch <= 0 {
		batch = 5000
	}
	res, err := s.db.Exec(`
		DELETE FROM lease_records WHERE id IN (
			SELECT id FROM lease_records WHERE allocated_at < ? ORDER BY allocated_at ASC LIMIT ?
		)`, cutoff.Format(time.RFC3339), batch)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ────────────────────────────────────────────────────────────────
// 已用别名流水 Leases
// ────────────────────────────────────────────────────────────────

func (s *Store) ListLeases(aliasQuery, tagQuery, statusQuery string, limit, offset int) ([]LeaseRecord, int) {
	whereClauses := []string{"1=1"}
	args := []any{}

	if aliasQuery != "" {
		whereClauses = append(whereClauses, "LOWER(email) LIKE ?")
		args = append(args, "%"+strings.ToLower(aliasQuery)+"%")
	}
	if tagQuery != "" && tagQuery != "all" {
		whereClauses = append(whereClauses, "LOWER(tag) = LOWER(?)")
		args = append(args, tagQuery)
	}
	if statusQuery != "" && statusQuery != "all" {
		whereClauses = append(whereClauses, "LOWER(status) = LOWER(?)")
		args = append(args, statusQuery)
	}

	whereSQL := strings.Join(whereClauses, " AND ")

	var total int
	countQuery := "SELECT COUNT(*) FROM lease_records WHERE " + whereSQL
	_ = s.db.QueryRow(countQuery, args...).Scan(&total)

	if total == 0 || offset >= total {
		return []LeaseRecord{}, total
	}

	if limit <= 0 {
		limit = 50
	}
	query := fmt.Sprintf("SELECT id, email, account_id, tag, status, allocated_at, COALESCE(completed_at, ''), COALESCE(token_name, '') FROM lease_records WHERE %s ORDER BY allocated_at DESC LIMIT ? OFFSET ?", whereSQL)
	queryArgs := append(args, limit, offset)

	rows, err := s.db.Query(query, queryArgs...)
	if err != nil {
		return []LeaseRecord{}, total
	}
	defer rows.Close()

	records := make([]LeaseRecord, 0, limit)
	for rows.Next() {
		var rec LeaseRecord
		if err := rows.Scan(&rec.ID, &rec.Email, &rec.AccountID, &rec.Tag, &rec.Status, &rec.AllocatedAt, &rec.CompletedAt, &rec.TokenName); err == nil {
			records = append(records, rec)
		}
	}
	// 【BUG-05 修复】迭代中断时记录日志
	if err := rows.Err(); err != nil {
		log.Printf("[Store] ListLeases 迭代中断: %v", err)
	}
	return records, total
}

func (s *Store) RecordLease(rec LeaseRecord) error {
	if rec.ID == "" {
		rec.ID = NewOpaqueID("lease_")
	}
	if rec.AllocatedAt == "" {
		rec.AllocatedAt = time.Now().Format(time.RFC3339)
	}
	if rec.Status == "" {
		rec.Status = "completed"
	}
	rec.Email = normalizeEmail(rec.Email)
	query := `INSERT INTO lease_records (id, email, account_id, tag, status, allocated_at, completed_at, token_name) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := s.db.Exec(query, rec.ID, rec.Email, rec.AccountID, rec.Tag, rec.Status, rec.AllocatedAt, rec.CompletedAt, rec.TokenName); err != nil {
		return err
	}
	if rec.AccountID != "" && rec.Email != "" {
		_ = s.UpsertAliasRoutes(rec.AccountID, []string{rec.Email})
	}
	return nil
}

// normalizeEmail 归一化邮箱用于等值比较与索引匹配。
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func (s *Store) UpdateLeaseStatus(id, status string) error {
	target := strings.TrimSpace(id)
	targetEmail := normalizeEmail(target)
	var res sql.Result
	var execErr error
	if status == "completed" {
		now := time.Now().Format(time.RFC3339)
		res, execErr = s.db.Exec(`UPDATE lease_records SET status = ?, completed_at = ? WHERE id = ? OR LOWER(email) = ?`, status, now, target, targetEmail)
	} else {
		res, execErr = s.db.Exec(`UPDATE lease_records SET status = ? WHERE id = ? OR LOWER(email) = ?`, status, target, targetEmail)
	}
	if execErr != nil {
		return execErr
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return fmt.Errorf("记录不存在: %s", id)
	}
	return nil
}

// ClaimPoolAlias 原子地从候选别名列表中挑选第一个未被外部消费者领用的别名并生成领用记录。
func (s *Store) ClaimPoolAlias(candidates []PoolCandidate, tag, principalKind, principalID, tokenDisplayName string) (*LeaseRecord, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	if tag == "" {
		tag = "default"
	}
	if principalKind == "" {
		principalKind = "token"
	}
	if principalID == "" {
		principalID = tokenDisplayName
	}
	if tokenDisplayName == "" {
		tokenDisplayName = principalID
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := time.Now().Format(time.RFC3339)

	prepInv, err := tx.Prepare(`
		INSERT INTO alias_inventory (email, account_id, remote_state, allocation_state, source_type, snapshot_version)
		VALUES (?, ?, 'active', 'available', 'pool', 1)
		ON CONFLICT(email) DO NOTHING
	`)
	if err != nil {
		return nil, err
	}
	defer prepInv.Close()

	for _, c := range candidates {
		norm := normalizeEmail(c.Email)
		if norm != "" {
			if _, err := prepInv.Exec(norm, c.AccountID); err != nil {
				return nil, err
			}
		}
	}

	const batchSize = 500
	for i := 0; i < len(candidates); i += batchSize {
		end := i + batchSize
		if end > len(candidates) {
			end = len(candidates)
		}
		chunk := candidates[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for j, c := range chunk {
			placeholders[j] = "?"
			args[j] = normalizeEmail(c.Email)
		}

		query := fmt.Sprintf(
			`SELECT email, account_id FROM alias_inventory
			 WHERE email IN (%s) AND allocation_state = 'available' AND remote_state = 'active'
			 LIMIT 1`,
			strings.Join(placeholders, ","),
		)
		var candEmail, candAccountID string
		qErr := tx.QueryRow(query, args...).Scan(&candEmail, &candAccountID)
		if qErr != nil {
			if errors.Is(qErr, sql.ErrNoRows) {
				continue
			}
			return nil, qErr
		}

		res, err := tx.Exec(`
			UPDATE alias_inventory
			SET allocation_state = 'allocated'
			WHERE email = ? AND allocation_state = 'available'
		`, candEmail)
		if err != nil {
			return nil, err
		}
		rowsAffected, _ := res.RowsAffected()
		if rowsAffected == 0 {
			continue
		}

		ownerKind := principalKind
		ownerID := principalID

		allocID := NewOpaqueID("lease_")

		_, err = tx.Exec(`
			INSERT INTO alias_allocations (allocation_id, alias_email, account_id, owner_kind, owner_id, business_tag, allocated_at, status)
			VALUES (?, ?, ?, ?, ?, ?, ?, 'allocated')
			ON CONFLICT(alias_email) DO UPDATE SET
				owner_kind = excluded.owner_kind,
				owner_id = excluded.owner_id,
				status = 'allocated'
		`, allocID, candEmail, candAccountID, ownerKind, ownerID, tag, now)
		if err != nil {
			return nil, fmt.Errorf("写入 alias_allocations 失败: %w", err)
		}

		rec := LeaseRecord{
			ID:          allocID,
			Email:       candEmail,
			AccountID:   candAccountID,
			Tag:         tag,
			Status:      "completed",
			AllocatedAt: now,
			CompletedAt: now,
			TokenName:   tokenDisplayName,
		}
		insQuery := `INSERT INTO lease_records (id, email, account_id, tag, status, allocated_at, completed_at, token_name) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
		if _, insertErr := tx.Exec(insQuery, rec.ID, rec.Email, rec.AccountID, rec.Tag, rec.Status, rec.AllocatedAt, rec.CompletedAt, rec.TokenName); insertErr != nil {
			return nil, insertErr
		}

		if rec.AccountID != "" {
			_, _ = tx.Exec(`INSERT INTO alias_routes (email, account_id, updated_at) VALUES (?, ?, ?) ON CONFLICT(email) DO UPDATE SET account_id=excluded.account_id`, rec.Email, rec.AccountID, now)
		}

		if err := tx.Commit(); err != nil {
			return nil, err
		}

		s.UpdateTagLastAssigned(tag)
		return &rec, nil
	}

	return nil, nil
}

// ClaimPoolAliasByRoutes 直接从持久化库存表中查找未被消费的可用别名并原子认领。
func (s *Store) ClaimPoolAliasByRoutes(accountIDs []string, tag, principalKind, principalID, tokenDisplayName string) (*LeaseRecord, error) {
	if len(accountIDs) == 0 {
		return nil, nil
	}
	if tag == "" {
		tag = "default"
	}
	if principalKind == "" {
		principalKind = "token"
	}
	if principalID == "" {
		principalID = tokenDisplayName
	}
	if tokenDisplayName == "" {
		tokenDisplayName = principalID
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := time.Now().Format(time.RFC3339)

	const batchSize = 200
	for i := 0; i < len(accountIDs); i += batchSize {
		end := i + batchSize
		if end > len(accountIDs) {
			end = len(accountIDs)
		}
		chunk := accountIDs[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for j, id := range chunk {
			placeholders[j] = "?"
			args[j] = id
		}

		query := fmt.Sprintf(`
			SELECT email, account_id
			FROM alias_inventory
			WHERE account_id IN (%s)
			  AND allocation_state = 'available'
			  AND remote_state = 'active'
			LIMIT 1`,
			strings.Join(placeholders, ","),
		)

		var candEmail, candAccountID string
		err := tx.QueryRow(query, args...).Scan(&candEmail, &candAccountID)
		if err != nil {
			continue
		}

		res, err := tx.Exec(`
			UPDATE alias_inventory
			SET allocation_state = 'allocated'
			WHERE email = ? AND allocation_state = 'available'
		`, candEmail)
		if err != nil {
			return nil, err
		}
		rowsAffected, _ := res.RowsAffected()
		if rowsAffected == 0 {
			continue
		}

		ownerKind := principalKind
		ownerID := principalID

		allocID := NewOpaqueID("lease_")

		_, err = tx.Exec(`
			INSERT INTO alias_allocations (allocation_id, alias_email, account_id, owner_kind, owner_id, business_tag, allocated_at, status)
			VALUES (?, ?, ?, ?, ?, ?, ?, 'allocated')
			ON CONFLICT(alias_email) DO UPDATE SET
				owner_kind = excluded.owner_kind,
				owner_id = excluded.owner_id,
				status = 'allocated'
		`, allocID, candEmail, candAccountID, ownerKind, ownerID, tag, now)
		if err != nil {
			return nil, fmt.Errorf("写入 alias_allocations 失败: %w", err)
		}

		rec := LeaseRecord{
			ID:          allocID,
			Email:       candEmail,
			AccountID:   candAccountID,
			Tag:         tag,
			Status:      "completed",
			AllocatedAt: now,
			CompletedAt: now,
			TokenName:   tokenDisplayName,
		}
		insQuery := `INSERT INTO lease_records (id, email, account_id, tag, status, allocated_at, completed_at, token_name) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
		if _, insertErr := tx.Exec(insQuery, rec.ID, rec.Email, rec.AccountID, rec.Tag, rec.Status, rec.AllocatedAt, rec.CompletedAt, rec.TokenName); insertErr != nil {
			return nil, insertErr
		}

		if rec.AccountID != "" {
			_, _ = tx.Exec(`INSERT INTO alias_routes (email, account_id, updated_at) VALUES (?, ?, ?) ON CONFLICT(email) DO UPDATE SET account_id=excluded.account_id`, rec.Email, rec.AccountID, now)
		}

		if err := tx.Commit(); err != nil {
			return nil, err
		}

		s.UpdateTagLastAssigned(tag)
		return &rec, nil
	}

	return nil, nil
}

// CountAvailablePoolAliases 统计候选别名列表中未被消费的可用数量。
func (s *Store) CountAvailablePoolAliases(candidates []PoolCandidate) (int, error) {
	if len(candidates) == 0 {
		return 0, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	totalAvailable := 0
	const batchSize = 500
	for i := 0; i < len(candidates); i += batchSize {
		end := i + batchSize
		if end > len(candidates) {
			end = len(candidates)
		}
		chunk := candidates[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for j, c := range chunk {
			placeholders[j] = "?"
			args[j] = normalizeEmail(c.Email)
		}

		query := fmt.Sprintf(
			`SELECT LOWER(email) FROM lease_records WHERE LOWER(email) IN (%s) AND COALESCE(token_name, '') != 'scheduler'`,
			strings.Join(placeholders, ","),
		)
		rows, err := s.db.Query(query, args...)
		if err != nil {
			return 0, err
		}
		consumed := make(map[string]bool, len(chunk))
		for rows.Next() {
			var em string
			if err := rows.Scan(&em); err == nil {
				consumed[normalizeEmail(em)] = true
			}
		}
		_ = rows.Close()

		for _, c := range chunk {
			norm := normalizeEmail(c.Email)
			if norm != "" && !consumed[norm] {
				totalAvailable++
			}
		}
	}

	return totalAvailable, nil
}

// CountConsumedPoolAliases 统计已被外部消费者认领（非 scheduler 占位）的唯一别名数。
func (s *Store) CountConsumedPoolAliases() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	var n int
	_ = s.db.QueryRow(`SELECT COUNT(DISTINCT LOWER(email)) FROM lease_records WHERE COALESCE(token_name, '') != 'scheduler'`).Scan(&n)
	return n
}
