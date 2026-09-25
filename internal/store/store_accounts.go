/**
 * [INPUT]: 依赖 database/sql, encoding/json, time
 * [OUTPUT]: 对外提供 AccountRecord 结构及 Store 对 accounts 表的完整 CRUD 能力
 * [POS]: internal/store 的账号持久化层，为 account.Manager 提供 SQLite 原子化单行读写，替代 JSON 全量重写
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"icloud-hme/internal/security"
)

// ────────────────────────────────────────────────────────────────
// AccountRecord — SQLite 行与 Go 结构体的映射载体
// ────────────────────────────────────────────────────────────────

// AccountRecord 是 accounts 表的一行，所有 JSON 复合字段以原始字符串传输，
// 由 account.Manager 负责序列化/反序列化，Store 层不感知业务语义。
type AccountRecord struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	RealEmail     string `json:"real_email"`
	ICloudEmail   string `json:"icloud_email"`
	CookiesJSON   string `json:"cookies"` // JSON map[string]string
	Host          string `json:"host"`
	ServiceURL    string `json:"service_url"`
	Proxy         string `json:"proxy"`
	AppPassword   string `json:"app_password"`
	MailboxJSON   string `json:"mailbox"` // JSON MailboxConfig or ""
	Status        string `json:"status"`
	AliasTotal    int    `json:"alias_total"`
	AliasActive   int    `json:"alias_active"`
	LastValidated string `json:"last_validated"`
	LastError     string `json:"last_error"`
	CreatedAt     string `json:"created_at"`
	TagsJSON      string `json:"tags"` // JSON []string
	UpdatedAt     string `json:"updated_at"`
}

// ────────────────────────────────────────────────────────────────
// 写操作
// ────────────────────────────────────────────────────────────────

// SaveAccount 插入或全量替换一个账号记录 (UPSERT)。写库前对敏感凭据字段执行 AES-256-GCM + AAD 认证加密。
func (s *Store) SaveAccount(rec *AccountRecord) error {
	now := time.Now().Format(time.RFC3339)
	if rec.UpdatedAt == "" {
		rec.UpdatedAt = now
	}

	cookies := rec.CookiesJSON
	appPassword := rec.AppPassword
	mailbox := rec.MailboxJSON
	proxy := rec.Proxy

	if cookies != "" || appPassword != "" || mailbox != "" || proxy != "" {
		if s.cipher == nil {
			return fmt.Errorf("master key is required to encrypt account credentials")
		}
		var err error
		if cookies, err = s.ensureFieldEncrypted(cookies, security.AccountAAD(rec.ID, "cookies")); err != nil {
			return fmt.Errorf("encrypt account cookies failed: %w", err)
		}
		if appPassword, err = s.ensureFieldEncrypted(appPassword, security.AccountAAD(rec.ID, "app_password")); err != nil {
			return fmt.Errorf("encrypt account app_password failed: %w", err)
		}
		if mailbox, err = s.ensureFieldEncrypted(mailbox, security.AccountAAD(rec.ID, "mailbox")); err != nil {
			return fmt.Errorf("encrypt account mailbox failed: %w", err)
		}
		if proxy, err = s.ensureFieldEncrypted(proxy, security.AccountAAD(rec.ID, "proxy")); err != nil {
			return fmt.Errorf("encrypt account proxy failed: %w", err)
		}
	}

	query := `
	INSERT INTO accounts (id, name, real_email, icloud_email, cookies, host, service_url,
		proxy, app_password, mailbox, status, alias_total, alias_active,
		last_validated, last_error, created_at, tags, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		name=excluded.name, real_email=excluded.real_email, icloud_email=excluded.icloud_email,
		cookies=excluded.cookies, host=excluded.host, service_url=excluded.service_url,
		proxy=excluded.proxy, app_password=excluded.app_password, mailbox=excluded.mailbox,
		status=excluded.status, alias_total=excluded.alias_total, alias_active=excluded.alias_active,
		last_validated=excluded.last_validated, last_error=excluded.last_error,
		tags=excluded.tags, updated_at=excluded.updated_at;
	`
	_, err := s.db.Exec(query,
		rec.ID, rec.Name, rec.RealEmail, rec.ICloudEmail, cookies,
		rec.Host, rec.ServiceURL, proxy, appPassword, mailbox,
		rec.Status, rec.AliasTotal, rec.AliasActive,
		rec.LastValidated, rec.LastError, rec.CreatedAt, rec.TagsJSON, rec.UpdatedAt,
	)
	return err
}

var validAccountColumns = map[string]bool{
	"name": true, "real_email": true, "icloud_email": true, "cookies": true,
	"host": true, "service_url": true, "proxy": true, "app_password": true,
	"mailbox": true, "status": true, "alias_total": true, "alias_active": true,
	"last_validated": true, "last_error": true, "created_at": true, "tags": true,
	"updated_at": true,
}

// UpdateAccountFields 原子更新指定账号的一组字段（细粒度写，避免全量 UPSERT）。
// fields 的 key 必须是 accounts 表的合法列名。如果包含敏感凭据，自动加密。
func (s *Store) UpdateAccountFields(id string, fields map[string]interface{}) error {
	if len(fields) == 0 {
		return nil
	}
	// 自动追加 updated_at
	fields["updated_at"] = time.Now().Format(time.RFC3339)

	sensitiveCols := []string{"cookies", "app_password", "mailbox", "proxy"}
	for _, col := range sensitiveCols {
		if rawVal, ok := fields[col]; ok {
			strVal, isStr := rawVal.(string)
			if isStr && strVal != "" {
				if s.cipher == nil {
					return fmt.Errorf("master key is required to encrypt account %s", col)
				}
				encVal, err := s.ensureFieldEncrypted(strVal, security.AccountAAD(id, col))
				if err != nil {
					return fmt.Errorf("encrypt account field %s failed: %w", col, err)
				}
				fields[col] = encVal
			}
		}
	}

	setClauses := make([]string, 0, len(fields))
	args := make([]interface{}, 0, len(fields)+1)
	for col, val := range fields {
		if !validAccountColumns[col] {
			return fmt.Errorf("invalid account column: %s", col)
		}
		setClauses = append(setClauses, col+"=?")
		args = append(args, val)
	}
	args = append(args, id)

	query := fmt.Sprintf("UPDATE accounts SET %s WHERE id=?",
		joinStrings(setClauses, ", "))
	_, err := s.db.Exec(query, args...)
	return err
}

// DeleteAccount 物理删除一个账号，并级联清理其别名路由与预存库存。
//
// 必须级联:残留路由会把取码请求指向一个已不存在的账号，导致
// 定向拉取永远失败且不再回退到盲扫兜底。
// 必须隔离库存:防止未领取的预存别名继续被出号逻辑选中 (幽灵号)。
func (s *Store) DeleteAccount(id string) error {
	if _, err := s.db.Exec(`DELETE FROM accounts WHERE id=?`, id); err != nil {
		return err
	}
	_ = s.DeleteAliasRoutesForAccount(id)
	_ = s.QuarantineInventoryForAccount(id)
	return nil
}

// SaveAccountsBatch 批量写入账号（用于迁移）。
func (s *Store) SaveAccountsBatch(recs []*AccountRecord) error {
	if len(recs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`
	INSERT OR IGNORE INTO accounts (id, name, real_email, icloud_email, cookies, host,
		service_url, proxy, app_password, mailbox, status, alias_total, alias_active,
		last_validated, last_error, created_at, tags, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range recs {
		if r == nil {
			continue
		}
		cookies := r.CookiesJSON
		appPassword := r.AppPassword
		mailbox := r.MailboxJSON
		proxy := r.Proxy

		if cookies != "" || appPassword != "" || mailbox != "" || proxy != "" {
			if s.cipher == nil {
				return fmt.Errorf("master key is required to encrypt account credentials")
			}
			var encErr error
			if cookies, encErr = s.ensureFieldEncrypted(cookies, security.AccountAAD(r.ID, "cookies")); encErr != nil {
				return fmt.Errorf("encrypt account cookies failed: %w", encErr)
			}
			if appPassword, encErr = s.ensureFieldEncrypted(appPassword, security.AccountAAD(r.ID, "app_password")); encErr != nil {
				return fmt.Errorf("encrypt account app_password failed: %w", encErr)
			}
			if mailbox, encErr = s.ensureFieldEncrypted(mailbox, security.AccountAAD(r.ID, "mailbox")); encErr != nil {
				return fmt.Errorf("encrypt account mailbox failed: %w", encErr)
			}
			if proxy, encErr = s.ensureFieldEncrypted(proxy, security.AccountAAD(r.ID, "proxy")); encErr != nil {
				return fmt.Errorf("encrypt account proxy failed: %w", encErr)
			}
		}

		if _, err := stmt.Exec(
			r.ID, r.Name, r.RealEmail, r.ICloudEmail, cookies,
			r.Host, r.ServiceURL, proxy, appPassword, mailbox,
			r.Status, r.AliasTotal, r.AliasActive,
			r.LastValidated, r.LastError, r.CreatedAt, r.TagsJSON, r.UpdatedAt,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ────────────────────────────────────────────────────────────────
// 读操作
// ────────────────────────────────────────────────────────────────

// GetAccount 按 ID 查询单个账号。未找到返回 nil, nil。
func (s *Store) GetAccount(id string) (*AccountRecord, error) {
	row := s.db.QueryRow(`SELECT id, name, real_email, icloud_email, cookies, host,
		service_url, proxy, app_password, mailbox, status, alias_total, alias_active,
		last_validated, last_error, created_at, tags, updated_at
		FROM accounts WHERE id=?`, id)
	rec := &AccountRecord{}
	err := scanAccountRow(row, rec)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := s.decryptAccountRecord(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// ListAllAccounts 返回全部账号记录（内存缓存初始化用）。
func (s *Store) ListAllAccounts() ([]*AccountRecord, error) {
	rows, err := s.db.Query(`SELECT id, name, real_email, icloud_email, cookies, host,
		service_url, proxy, app_password, mailbox, status, alias_total, alias_active,
		last_validated, last_error, created_at, tags, updated_at
		FROM accounts ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanAccountRows(rows)
}

// ListAccountsPaged 分页查询账号列表，返回 (记录, 总数, 错误)。
func (s *Store) ListAccountsPaged(offset, limit int) ([]*AccountRecord, int, error) {
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT id, name, real_email, icloud_email, cookies, host,
		service_url, proxy, app_password, mailbox, status, alias_total, alias_active,
		last_validated, last_error, created_at, tags, updated_at
		FROM accounts ORDER BY created_at ASC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	recs, err := s.scanAccountRows(rows)
	return recs, total, err
}

// AccountCount 返回账号总数。
func (s *Store) AccountCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&n)
	return n, err
}

// ────────────────────────────────────────────────────────────────
// 内部扫描与安全解密辅助
// ────────────────────────────────────────────────────────────────

type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanAccountRow(row rowScanner, rec *AccountRecord) error {
	return row.Scan(
		&rec.ID, &rec.Name, &rec.RealEmail, &rec.ICloudEmail, &rec.CookiesJSON,
		&rec.Host, &rec.ServiceURL, &rec.Proxy, &rec.AppPassword, &rec.MailboxJSON,
		&rec.Status, &rec.AliasTotal, &rec.AliasActive,
		&rec.LastValidated, &rec.LastError, &rec.CreatedAt, &rec.TagsJSON, &rec.UpdatedAt,
	)
}

func (s *Store) scanAccountRows(rows *sql.Rows) ([]*AccountRecord, error) {
	var result []*AccountRecord
	for rows.Next() {
		rec := &AccountRecord{}
		if err := scanAccountRow(rows, rec); err != nil {
			return nil, err
		}
		if err := s.decryptAccountRecord(rec); err != nil {
			return nil, err
		}
		result = append(result, rec)
	}
	return result, rows.Err()
}

// decryptAccountRecord 使用绑定 AAD 解密敏感凭据字段。解密失败或未加密明文均 fail closed。
func (s *Store) decryptAccountRecord(rec *AccountRecord) error {
	if rec == nil {
		return nil
	}
	fields := []struct {
		name string
		val  *string
	}{
		{"cookies", &rec.CookiesJSON},
		{"app_password", &rec.AppPassword},
		{"mailbox", &rec.MailboxJSON},
		{"proxy", &rec.Proxy},
	}

	for _, f := range fields {
		if *f.val == "" {
			continue
		}
		if !security.IsEncrypted(*f.val) {
			return fmt.Errorf("account %s field %s is not encrypted", rec.ID, f.name)
		}
		if s.cipher == nil {
			return fmt.Errorf("master key is required to decrypt account %s field %s", rec.ID, f.name)
		}
		dec, err := s.cipher.Decrypt(*f.val, security.AccountAAD(rec.ID, f.name))
		if err != nil {
			return fmt.Errorf("decrypt account %s field %s failed: %w", rec.ID, f.name, err)
		}
		*f.val = string(dec)
	}
	return nil
}

// GetEncryptedSetting 解密并读取设置项。若未加密或解密失败则 fail closed。
func (s *Store) GetEncryptedSetting(key string, aad []byte) (string, error) {
	raw := s.GetSetting(key)
	if raw == "" {
		return "", nil
	}
	if !security.IsEncrypted(raw) {
		return "", fmt.Errorf("setting %s is not encrypted", key)
	}
	if s.cipher == nil {
		return "", fmt.Errorf("master key is required to decrypt setting %s", key)
	}
	dec, err := s.cipher.Decrypt(raw, aad)
	if err != nil {
		return "", fmt.Errorf("decrypt setting %s failed: %w", key, err)
	}
	return string(dec), nil
}

// SaveEncryptedSetting 认证加密并持久化设置项。
func (s *Store) SaveEncryptedSetting(key, plaintext string, aad []byte) error {
	if plaintext == "" {
		return s.SaveSetting(key, "")
	}
	if s.cipher == nil {
		return fmt.Errorf("master key is required to encrypt setting %s", key)
	}
	enc, err := s.cipher.Encrypt([]byte(plaintext), aad)
	if err != nil {
		return fmt.Errorf("encrypt setting %s failed: %w", key, err)
	}
	return s.SaveSetting(key, enc)
}

// joinStrings 简单拼接，避免引入 strings 包的额外依赖（store.go 已有 strings 导入）。
func joinStrings(elems []string, sep string) string {
	if len(elems) == 0 {
		return ""
	}
	result := elems[0]
	for _, e := range elems[1:] {
		result += sep + e
	}
	return result
}

// ────────────────────────────────────────────────────────────────
// Account ↔ AccountRecord 序列化工具（供 account.Manager 使用）
// ────────────────────────────────────────────────────────────────

// MarshalCookies 将 cookies map 序列化为 JSON 字符串。
func MarshalCookies(cookies map[string]string) string {
	if len(cookies) == 0 {
		return "{}"
	}
	b, err := json.Marshal(cookies)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// UnmarshalCookies 将 JSON 字符串反序列化为 cookies map。
func UnmarshalCookies(raw string) map[string]string {
	if raw == "" || raw == "{}" {
		return make(map[string]string)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return make(map[string]string)
	}
	return m
}

// MarshalTags 将 tags 切片序列化为 JSON 字符串。
func MarshalTags(tags []string) string {
	if len(tags) == 0 {
		return "[]"
	}
	b, err := json.Marshal(tags)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// UnmarshalTags 将 JSON 字符串反序列化为 tags 切片。
func UnmarshalTags(raw string) []string {
	if raw == "" || raw == "[]" {
		return nil
	}
	var tags []string
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		return nil
	}
	return tags
}
