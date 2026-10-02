/**
 * [INPUT]: 依赖 database/sql, errors, fmt, log, strings, sync/atomic, time, icloud-hme/internal/store (Store)
 * [OUTPUT]: 对外提供 LeaseRecord 类型, UpsertAliasRoutes, FindAliasRoute, CountAliasRoutes, DeleteAliasRoutesForAccount, FindLeaseAccount, CountLeases, PruneLeases, ListLeases, RecordLease, UpdateLeaseStatus, CountConsumedPoolAliases
 * [POS]: internal/store 的别名认领与出号流水审计领域，流水排序与保留期按实际时间比较，兼容历史时区偏移
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

// LeaseRecord 已用别名流水记录
type LeaseRecord struct {
	ID           string `json:"id"`
	Email        string `json:"email"`
	AccountID    string `json:"account_id"`
	AccountName  string `json:"account_name,omitempty"`
	AccountEmail string `json:"account_email,omitempty"`
	Tag          string `json:"tag"`
	Status       string `json:"status"` // "completed" | "leased" | "abandoned"
	AllocatedAt  string `json:"allocated_at"`
	CompletedAt  string `json:"completed_at,omitempty"`
	TokenName    string `json:"token_name,omitempty"`
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

// FindAliasRouteContext 按别名邮箱反查归属母号(主键点查)，支持 Context 传播与区分不存在与 DB 故障 (R07)。
func (s *Store) FindAliasRouteContext(ctx context.Context, email string) (string, error) {
	email = normalizeEmail(email)
	if email == "" {
		return "", sql.ErrNoRows
	}
	var accountID string
	if err := s.db.QueryRowContext(ctx, `SELECT account_id FROM alias_routes WHERE email = ?`, email).Scan(&accountID); err != nil {
		return "", err
	}
	if accountID == "" {
		return "", sql.ErrNoRows
	}
	return accountID, nil
}

// FindAliasRoute 按别名邮箱反查归属母号(主键点查)。
func (s *Store) FindAliasRoute(email string) (string, bool) {
	accID, err := s.FindAliasRouteContext(context.Background(), email)
	return accID, err == nil && accID != ""
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
	marker, err := s.GetSetting(settingKeyAliasRoutesBackfilled)
	if err != nil {
		log.Printf("[Store] 读取别名路由回填标记失败: %v", err)
		return
	}
	if marker == "1" && s.hasAnyAliasRoute() {
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

// FindLeaseAccountContext 按别名邮箱反查最近一次出号归属的账号 ID (支持 Context 传播与区分不存在与 DB 故障)。
func (s *Store) FindLeaseAccountContext(ctx context.Context, email string) (string, error) {
	email = normalizeEmail(email)
	if email == "" {
		return "", sql.ErrNoRows
	}
	var accountID string
	err := s.db.QueryRowContext(ctx,
		`SELECT account_id FROM lease_records WHERE LOWER(email) = ? AND account_id != '' ORDER BY julianday(allocated_at) DESC, id DESC LIMIT 1`,
		email,
	).Scan(&accountID)
	if err != nil {
		return "", err
	}
	if accountID == "" {
		return "", sql.ErrNoRows
	}
	return accountID, nil
}

// FindLeaseAccount 按别名邮箱反查最近一次出号归属的账号 ID。
func (s *Store) FindLeaseAccount(email string) (string, bool) {
	accID, err := s.FindLeaseAccountContext(context.Background(), email)
	return accID, err == nil && accID != ""
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
			SELECT id FROM lease_records WHERE julianday(allocated_at) < julianday(?) ORDER BY julianday(allocated_at) ASC, id ASC LIMIT ?
		)`, cutoff.UTC().Format(time.RFC3339Nano), batch)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ────────────────────────────────────────────────────────────────
// 已用别名流水 Leases
// ────────────────────────────────────────────────────────────────

func (s *Store) ListLeases(aliasQuery, tagQuery, statusQuery string, limit, offset int) ([]LeaseRecord, int, error) {
	whereClauses := []string{"1=1"}
	args := []any{}

	if aliasQuery != "" {
		whereClauses = append(whereClauses, "(LOWER(l.email) LIKE ? OR LOWER(l.account_id) LIKE ? OR LOWER(COALESCE(a.name, '')) LIKE ? OR LOWER(COALESCE(NULLIF(a.icloud_email, ''), NULLIF(a.real_email, ''), '')) LIKE ?)")
		search := "%" + strings.ToLower(aliasQuery) + "%"
		args = append(args, search, search, search, search)
	}
	if tagQuery != "" && tagQuery != "all" {
		whereClauses = append(whereClauses, "LOWER(l.tag) = LOWER(?)")
		args = append(args, tagQuery)
	}
	if statusQuery != "" && statusQuery != "all" {
		whereClauses = append(whereClauses, "LOWER(l.status) = LOWER(?)")
		args = append(args, statusQuery)
	}

	whereSQL := strings.Join(whereClauses, " AND ")

	var total int
	countQuery := "SELECT COUNT(*) FROM lease_records l LEFT JOIN accounts a ON a.id = l.account_id WHERE " + whereSQL
	if err := s.db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count leases: %w", err)
	}

	if total == 0 || offset >= total {
		return []LeaseRecord{}, total, nil
	}

	if limit <= 0 {
		limit = 50
	}
	query := fmt.Sprintf("SELECT l.id, l.email, l.account_id, COALESCE(a.name, ''), COALESCE(NULLIF(a.icloud_email, ''), NULLIF(a.real_email, ''), ''), l.tag, l.status, l.allocated_at, COALESCE(l.completed_at, ''), COALESCE(l.token_name, '') FROM lease_records l LEFT JOIN accounts a ON a.id = l.account_id WHERE %s ORDER BY julianday(l.allocated_at) DESC, l.id DESC LIMIT ? OFFSET ?", whereSQL)
	queryArgs := append(args, limit, offset)

	rows, err := s.db.Query(query, queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("query leases: %w", err)
	}
	defer rows.Close()

	records := make([]LeaseRecord, 0, limit)
	for rows.Next() {
		var rec LeaseRecord
		if err := rows.Scan(&rec.ID, &rec.Email, &rec.AccountID, &rec.AccountName, &rec.AccountEmail, &rec.Tag, &rec.Status, &rec.AllocatedAt, &rec.CompletedAt, &rec.TokenName); err != nil {
			return nil, 0, fmt.Errorf("scan lease: %w", err)
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate leases: %w", err)
	}
	return records, total, nil
}

func (s *Store) RecordLease(rec LeaseRecord) error {
	if rec.ID == "" {
		rec.ID = NewOpaqueID("lease_")
	}
	if rec.AllocatedAt == "" {
		rec.AllocatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if rec.Status == "" {
		rec.Status = "completed"
	}
	rec.Email = normalizeEmail(rec.Email)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	query := `INSERT INTO lease_records (id, email, account_id, tag, status, allocated_at, completed_at, token_name) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := tx.Exec(query, rec.ID, rec.Email, rec.AccountID, rec.Tag, rec.Status, rec.AllocatedAt, rec.CompletedAt, rec.TokenName); err != nil {
		return err
	}
	if rec.AccountID != "" && rec.Email != "" {
		if _, err := tx.Exec(`INSERT INTO alias_routes (email, account_id, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(email) DO UPDATE SET account_id = excluded.account_id, updated_at = excluded.updated_at`,
			rec.Email, rec.AccountID, time.Now().Format(time.RFC3339)); err != nil {
			return err
		}
	}
	return tx.Commit()
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

// CountConsumedPoolAliases 统计已被外部消费者认领（非 scheduler 占位）的唯一别名数。
func (s *Store) CountConsumedPoolAliases() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	var n int
	_ = s.db.QueryRow(`SELECT COUNT(DISTINCT LOWER(email)) FROM lease_records WHERE COALESCE(token_name, '') != 'scheduler'`).Scan(&n)
	return n
}
