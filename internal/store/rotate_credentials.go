/**
 * [INPUT]: 依赖 context, database/sql, fmt, os, path/filepath, time, icloud-hme/internal/security, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 RotateCredentials 离线主密钥轮换函数
 * [POS]: internal/store 的 Master Key 离线轮换容灾与运维工具层 (PR-07)
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"icloud-hme/internal/security"
)

var (
	rotationCleanupHookMu                    sync.RWMutex
	beforeRotationPhysicalCleanupHookForTest func() error
)

// SetBeforeRotationPhysicalCleanupHookForTest 设置凭据轮换物理清理前的故障注入挂钩 (测试专用)
func SetBeforeRotationPhysicalCleanupHookForTest(hook func() error) {
	rotationCleanupHookMu.Lock()
	defer rotationCleanupHookMu.Unlock()
	beforeRotationPhysicalCleanupHookForTest = hook
}

// RotateCredentials 执行离线 Master Key 凭据密钥轮换:
// 1. acquire exclusive lock (确保服务已停止)
// 2. 校验旧 key 与新 key 均合法且不同 (通过密文探针安全比对)
// 3. 创建 pre-rotation consistent backup
// 4. 开启事务
// 5. 使用 oldCipher + 对应 AAD 解密 accounts 表每条记录的 cookies, app_password, mailbox, proxy
// 6. 使用 newCipher + 对应 AAD 加密
// 7. 每个新 ciphertext 在写入前立即用 newCipher 自检解密验证
// 8. 使用 oldCipher 解密 notify_settings，并用 newCipher 加密与自检
// 9. commit 事务 (任何错误立即 rollback，杜绝部分轮换)
// 10. 安全物理清理: wal_checkpoint(TRUNCATE) + VACUUM + wal_checkpoint(TRUNCATE) + quick_check
// 11. 清理阶段若发生任何异常，自动从 pre-rotation 快照全量回滚，确保旧 Master Key 依然为权威 key
func RotateCredentials(dataDir string, oldCipher, newCipher *security.SecretCipher) error {
	if oldCipher == nil || newCipher == nil {
		return errors.New("旧密钥加密机与新密钥加密机均不能为空")
	}

	// 2. 校验旧 key 与新 key 均合法且不同 (通过密文探针安全比对，绝不暴露明文密钥或哈希)
	probePlain := []byte("master-key-equality-probe-sentinel")
	probeAAD := []byte("icloud-hme:internal:key-probe")
	probeCiphertext, err := newCipher.Encrypt(probePlain, probeAAD)
	if err != nil {
		return fmt.Errorf("new master key validation probe failed: %w", err)
	}
	if decrypted, err := oldCipher.Decrypt(probeCiphertext, probeAAD); err == nil && string(decrypted) == string(probePlain) {
		return errors.New("new master key must differ from current master key")
	}

	absDir, err := filepath.Abs(dataDir)
	if err != nil {
		return fmt.Errorf("解析数据目录失败: %w", err)
	}

	// 1. 申请独占排他锁
	lock, err := acquireDataDirLock(absDir)
	if err != nil {
		return fmt.Errorf("服务正在运行或获取数据目录排他锁失败，请停止服务后再执行轮换: %w", err)
	}
	defer lock.Close()

	dbPath := filepath.Join(absDir, "icloud_hme.db")
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("数据库文件不存在 (%s): %w", dbPath, err)
	}

	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)", filepath.ToSlash(dbPath))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("打开数据库失败: %w", err)
	}
	defer db.Close()

	// 验证 quick_check
	if err := quickCheck(db); err != nil {
		return fmt.Errorf("数据库一致性前置检查失败: %w", err)
	}

	// 检查当前 schema version
	v, err := getUserVersion(db)
	if err != nil {
		return fmt.Errorf("读取 user_version 失败: %w", err)
	}
	if v != CurrentSchemaVersion {
		return fmt.Errorf("数据库版本必须为 %d (当前为 %d)，请先完成数据迁移", CurrentSchemaVersion, v)
	}

	// 3. 创建 pre-rotation 一致性快照备份
	backupsDir := filepath.Join(absDir, "backups")
	if err := os.MkdirAll(backupsDir, 0700); err != nil {
		return fmt.Errorf("创建备份目录失败: %w", err)
	}
	backupName := fmt.Sprintf("pre-rotate-credentials-%s.db", time.Now().UTC().Format("20060102T150405Z"))
	backupPath := filepath.Join(backupsDir, backupName)
	if err := createOnlineBackup(context.Background(), db, backupPath); err != nil {
		return fmt.Errorf("创建轮换前一致性备份失败: %w", err)
	}

	// 4. 开启事务
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("开启轮换事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 5. 轮换 accounts 表凭据
	rows, err := tx.Query(`SELECT id, cookies, app_password, mailbox, proxy FROM accounts`)
	if err != nil {
		return fmt.Errorf("查询 accounts 失败: %w", err)
	}
	defer rows.Close()

	type accCred struct {
		id, cookies, appPassword, mailbox, proxy string
	}
	var accList []accCred
	for rows.Next() {
		var a accCred
		if err := rows.Scan(&a.id, &a.cookies, &a.appPassword, &a.mailbox, &a.proxy); err != nil {
			return err
		}
		accList = append(accList, a)
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

	rotateField := func(val string, aad []byte) (string, error) {
		if val == "" {
			return "", nil
		}
		// 用旧密钥解密
		plaintext, err := oldCipher.Decrypt(val, aad)
		if err != nil {
			return "", fmt.Errorf("旧密钥解密失败: %w", err)
		}
		// 用新密钥加密
		newCiphertext, err := newCipher.Encrypt(plaintext, aad)
		if err != nil {
			return "", fmt.Errorf("新密钥加密失败: %w", err)
		}
		// 自检验算新密钥能否正确解密
		if _, err := newCipher.Decrypt(newCiphertext, aad); err != nil {
			return "", fmt.Errorf("新密文自检验算失败: %w", err)
		}
		return newCiphertext, nil
	}

	for _, a := range accList {
		newCookies, err := rotateField(a.cookies, security.AccountAAD(a.id, "cookies"))
		if err != nil {
			return fmt.Errorf("账号 %s cookies 轮换失败: %w", a.id, err)
		}
		newAppPass, err := rotateField(a.appPassword, security.AccountAAD(a.id, "app_password"))
		if err != nil {
			return fmt.Errorf("账号 %s app_password 轮换失败: %w", a.id, err)
		}
		newMailbox, err := rotateField(a.mailbox, security.AccountAAD(a.id, "mailbox"))
		if err != nil {
			return fmt.Errorf("账号 %s mailbox 轮换失败: %w", a.id, err)
		}
		newProxy, err := rotateField(a.proxy, security.AccountAAD(a.id, "proxy"))
		if err != nil {
			return fmt.Errorf("账号 %s proxy 轮换失败: %w", a.id, err)
		}

		if _, err := updateStmt.Exec(newCookies, newAppPass, newMailbox, newProxy, a.id); err != nil {
			return fmt.Errorf("写入账号 %s 轮换密文失败: %w", a.id, err)
		}
	}

	// 6. 轮换 settings.notify_settings
	var rawNotify string
	notifyErr := tx.QueryRow(`SELECT value FROM settings WHERE key = 'notify_settings'`).Scan(&rawNotify)
	if notifyErr == nil && rawNotify != "" {
		newNotify, err := rotateField(rawNotify, security.NotifySettingsAAD())
		if err != nil {
			return fmt.Errorf("notify_settings 轮换失败: %w", err)
		}
		if _, err := tx.Exec(`UPDATE settings SET value = ? WHERE key = 'notify_settings'`, newNotify); err != nil {
			return fmt.Errorf("写入轮换 notify_settings 失败: %w", err)
		}
	} else if notifyErr != nil && notifyErr != sql.ErrNoRows {
		return fmt.Errorf("查询 notify_settings 失败: %w", notifyErr)
	}

	// 8. 提交事务
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交轮换事务失败: %w", err)
	}

	// 9. 安全物理清理 (WAL checkpoint + VACUUM 磁盘重整 + WAL checkpoint + quick_check)
	rotationCleanupHookMu.RLock()
	hook := beforeRotationPhysicalCleanupHookForTest
	rotationCleanupHookMu.RUnlock()

	var cleanupErr error
	if hook != nil {
		cleanupErr = hook()
	}
	if cleanupErr == nil {
		if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE);"); err != nil {
			cleanupErr = fmt.Errorf("rotation wal_checkpoint failed: %w", err)
		}
	}
	if cleanupErr == nil {
		if _, err := db.Exec("VACUUM;"); err != nil {
			cleanupErr = fmt.Errorf("rotation vacuum failed: %w", err)
		}
	}
	if cleanupErr == nil {
		if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE);"); err != nil {
			cleanupErr = fmt.Errorf("rotation post-vacuum wal_checkpoint failed: %w", err)
		}
	}
	if cleanupErr == nil {
		if err := quickCheck(db); err != nil {
			cleanupErr = fmt.Errorf("quick check failed: %w", err)
		}
	}

	// 10. 如果清理失败，绝对不能留下“半成功”的新密文库，必须从 pre-rotation 快照回滚为旧密钥权威库
	if cleanupErr != nil {
		_ = db.Close()

		// 清理因 commit 产生的 WAL/SHM 与 live DB
		_ = os.Remove(filepath.Join(absDir, "icloud_hme.db-wal"))
		_ = os.Remove(filepath.Join(absDir, "icloud_hme.db-shm"))
		_ = os.Remove(dbPath)

		// 从轮换前一致性快照恢复
		if rErr := restoreFileFromSnapshot(backupPath, dbPath); rErr != nil {
			return fmt.Errorf("rotation cleanup failed: %w; rollback to pre-rotation snapshot failed: %v", cleanupErr, rErr)
		}

		// 验证恢复后的数据库完整性
		checkDB, err := sql.Open("sqlite", dsn)
		if err != nil {
			return fmt.Errorf("rotation cleanup failed: %w; open restored db failed: %v", cleanupErr, err)
		}
		defer checkDB.Close()
		if err := quickCheck(checkDB); err != nil {
			return fmt.Errorf("rotation cleanup failed: %w; restored db quick check failed: %v", cleanupErr, err)
		}

		return fmt.Errorf("rotation cleanup failed (rolled back to original key): %w", cleanupErr)
	}

	return nil
}
