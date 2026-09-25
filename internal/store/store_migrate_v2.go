/**
 * [INPUT]: 依赖 database/sql, fmt, icloud-hme/internal/security, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 (s *Store) migrateV1ToV2 方法，负责 V1 -> V2 凭据加密、API Token 表不可逆重构与历史明文清理
 * [POS]: internal/store 的 V1 -> V2 顺序迁移器 (PR-07)
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"database/sql"
	"fmt"

	"icloud-hme/internal/security"
)

// migrateV1ToV2 执行 Version 1 -> Version 2 的安全迁移:
// 1. 验证 Master Key 存在性 (若存在未加密凭据且无 Master Key 则立即 fail closed);
// 2. api_tokens 表重构 (SQLite table rebuild): 转换为 token_hash + token_prefix，彻底剔除 token 明文列;
// 3. accounts 表凭据字段 (cookies, app_password, mailbox, proxy) AES-256-GCM + AAD 认证加密;
// 4. settings.notify_settings 认证加密;
// 5. 写入 user_version = 2;
// 6. 事务提交后执行 wal_checkpoint(TRUNCATE) + VACUUM 彻底消除 live DB 磁盘明文残留。
func (s *Store) migrateV1ToV2() error {
	// 1. 检查是否存在需加密的存量明文凭据
	hasPlaintextCredentials, err := s.hasPlaintextCredentials()
	if err != nil {
		return fmt.Errorf("检查存量凭据加密状态失败: %w", err)
	}

	if hasPlaintextCredentials && s.cipher == nil {
		return fmt.Errorf("master key is required for v1 to v2 migration: encryption key not configured")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("开启 v1 到 v2 迁移事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 2. 重建 api_tokens 表 (清除明文 token 列)
	if err := s.rebuildTokensTableTx(tx); err != nil {
		return fmt.Errorf("重建 api_tokens 表失败: %w", err)
	}

	// 3. 迁移 accounts 凭据
	if s.cipher != nil {
		if err := s.encryptAccountsTx(tx); err != nil {
			return fmt.Errorf("加密 accounts 凭据失败: %w", err)
		}
		if err := s.encryptNotifySettingsTx(tx); err != nil {
			return fmt.Errorf("加密 notify_settings 失败: %w", err)
		}
	}

	// 4. 更新 user_version = 2
	if _, err := tx.Exec("PRAGMA user_version = 2;"); err != nil {
		return fmt.Errorf("写入 schema user_version 2 失败: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 v1 到 v2 迁移事务失败: %w", err)
	}

	// 5. 安全清理磁盘历史明文残留 (WAL 截断 + 页面重整 VACUUM)
	if _, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE);"); err != nil {
		return fmt.Errorf("wal checkpoint failed: %w", err)
	}
	if _, err := s.db.Exec("VACUUM;"); err != nil {
		return fmt.Errorf("vacuum database failed: %w", err)
	}
	if _, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE);"); err != nil {
		return fmt.Errorf("post-vacuum wal checkpoint failed: %w", err)
	}

	return nil
}

func (s *Store) hasPlaintextCredentials() (bool, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM accounts
		WHERE (cookies != '' AND cookies NOT LIKE 'enc:v1:%')
		   OR (app_password != '' AND app_password NOT LIKE 'enc:v1:%')
		   OR (mailbox != '' AND mailbox NOT LIKE 'enc:v1:%')
		   OR (proxy != '' AND proxy NOT LIKE 'enc:v1:%')
	`).Scan(&count)
	if err != nil {
		return false, err
	}
	if count > 0 {
		return true, nil
	}

	var notifyCount int
	err = s.db.QueryRow(`
		SELECT COUNT(*) FROM settings
		WHERE key = 'notify_settings' AND value != '' AND value NOT LIKE 'enc:v1:%'
	`).Scan(&notifyCount)
	if err != nil {
		return false, err
	}
	return notifyCount > 0, nil
}

func (s *Store) rebuildTokensTableTx(tx *sql.Tx) error {
	hasOldTokenCol, err := tableHasColumn(tx, "api_tokens", "token")
	if err != nil {
		return err
	}
	if !hasOldTokenCol {
		// 表中已无旧 token 列，可能已经是 V2 结构
		return nil
	}

	// 创建 V2 临时表
	v2DDL := `
	CREATE TABLE api_tokens_v2 (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		token_hash TEXT NOT NULL UNIQUE,
		token_prefix TEXT NOT NULL,
		created_at TEXT NOT NULL,
		last_used_at TEXT,
		scopes TEXT NOT NULL DEFAULT 'admin',
		expires_at TEXT,
		revoked_at TEXT,
		rotated_at TEXT,
		needs_rotation INTEGER NOT NULL DEFAULT 0
	);
	`
	if _, err := tx.Exec(v2DDL); err != nil {
		return fmt.Errorf("创建 api_tokens_v2 失败: %w", err)
	}

	hasLastUsed, err := tableHasColumn(tx, "api_tokens", "last_used_at")
	if err != nil {
		return err
	}
	hasScopes, err := tableHasColumn(tx, "api_tokens", "scopes")
	if err != nil {
		return err
	}

	lastUsedCol := "''"
	if hasLastUsed {
		lastUsedCol = "COALESCE(last_used_at, '')"
	}
	scopesCol := "'admin'"
	if hasScopes {
		scopesCol = "COALESCE(scopes, 'admin')"
	}

	// 查询旧表全部记录
	query := fmt.Sprintf("SELECT id, name, token, created_at, %s, %s FROM api_tokens", lastUsedCol, scopesCol)
	rows, err := tx.Query(query)
	if err != nil {
		return fmt.Errorf("查询旧 api_tokens 失败: %w", err)
	}
	defer rows.Close()

	type oldTok struct {
		id, name, token, createdAt, lastUsedAt, scopes string
	}
	var oldTokens []oldTok
	for rows.Next() {
		var ot oldTok
		if err := rows.Scan(&ot.id, &ot.name, &ot.token, &ot.createdAt, &ot.lastUsedAt, &ot.scopes); err != nil {
			return err
		}
		oldTokens = append(oldTokens, ot)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	insertStmt, err := tx.Prepare(`
		INSERT INTO api_tokens_v2 (id, name, token_hash, token_prefix, created_at, last_used_at, scopes, needs_rotation)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1)
	`)
	if err != nil {
		return fmt.Errorf("prepare api_tokens_v2 insert failed: %w", err)
	}
	defer insertStmt.Close()

	for _, ot := range oldTokens {
		tokenHash := HashToken(ot.token)
		tokenPrefix := SafeTokenPrefix(ot.token)
		if _, err := insertStmt.Exec(ot.id, ot.name, tokenHash, tokenPrefix, ot.createdAt, ot.lastUsedAt, ot.scopes); err != nil {
			return fmt.Errorf("插入迁移令牌失败 (id=%s): %w", ot.id, err)
		}
	}

	// 删除旧表并重命名
	if _, err := tx.Exec("DROP TABLE api_tokens;"); err != nil {
		return fmt.Errorf("删除旧 api_tokens 表失败: %w", err)
	}
	if _, err := tx.Exec("ALTER TABLE api_tokens_v2 RENAME TO api_tokens;"); err != nil {
		return fmt.Errorf("重命名 api_tokens_v2 失败: %w", err)
	}
	if _, err := tx.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_api_tokens_hash ON api_tokens (token_hash);"); err != nil {
		return fmt.Errorf("创建 idx_api_tokens_hash 失败: %w", err)
	}

	return nil
}

func (s *Store) encryptAccountsTx(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT id, cookies, app_password, mailbox, proxy FROM accounts`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type accRow struct {
		id, cookies, appPassword, mailbox, proxy string
	}
	var accs []accRow
	for rows.Next() {
		var a accRow
		if err := rows.Scan(&a.id, &a.cookies, &a.appPassword, &a.mailbox, &a.proxy); err != nil {
			return err
		}
		accs = append(accs, a)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	updateStmt, err := tx.Prepare(`UPDATE accounts SET cookies = ?, app_password = ?, mailbox = ?, proxy = ? WHERE id = ?`)
	if err != nil {
		return err
	}
	defer updateStmt.Close()

	for _, a := range accs {
		encCookies, err := s.ensureFieldEncrypted(a.cookies, security.AccountAAD(a.id, "cookies"))
		if err != nil {
			return fmt.Errorf("account %s cookies 加密失败: %w", a.id, err)
		}

		encAppPass, err := s.ensureFieldEncrypted(a.appPassword, security.AccountAAD(a.id, "app_password"))
		if err != nil {
			return fmt.Errorf("account %s app_password 加密失败: %w", a.id, err)
		}

		encMailbox, err := s.ensureFieldEncrypted(a.mailbox, security.AccountAAD(a.id, "mailbox"))
		if err != nil {
			return fmt.Errorf("account %s mailbox 加密失败: %w", a.id, err)
		}

		encProxy, err := s.ensureFieldEncrypted(a.proxy, security.AccountAAD(a.id, "proxy"))
		if err != nil {
			return fmt.Errorf("account %s proxy 加密失败: %w", a.id, err)
		}

		if _, err := updateStmt.Exec(encCookies, encAppPass, encMailbox, encProxy, a.id); err != nil {
			return err
		}
	}

	return nil
}

func (s *Store) encryptNotifySettingsTx(tx *sql.Tx) error {
	var rawVal string
	err := tx.QueryRow(`SELECT value FROM settings WHERE key = 'notify_settings'`).Scan(&rawVal)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}

	if rawVal == "" {
		return nil
	}

	encVal, err := s.ensureFieldEncrypted(rawVal, security.NotifySettingsAAD())
	if err != nil {
		return fmt.Errorf("加密 notify_settings 失败: %w", err)
	}

	_, err = tx.Exec(`UPDATE settings SET value = ? WHERE key = 'notify_settings'`, encVal)
	return err
}

func (s *Store) ensureFieldEncrypted(val string, aad []byte) (string, error) {
	if val == "" {
		return "", nil
	}
	if security.IsEncrypted(val) {
		// 已是 enc:v1: 信封，自检验证当前密钥能否解密
		if _, err := s.cipher.Decrypt(val, aad); err != nil {
			return "", fmt.Errorf("既有密文无法用当前 Master Key 解密 (AAD 或 Key 不匹配): %w", err)
		}
		return val, nil
	}
	// 明文字符串，执行单次加密
	return s.cipher.Encrypt([]byte(val), aad)
}
