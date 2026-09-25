/**
 * [INPUT]: 依赖 context, database/sql, fmt, io, os, path/filepath, strings, time, icloud-hme/internal/security, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 CreateBackup, CreateDatabaseBackup 与 package-level RestoreDatabase / RestoreDatabaseWithCipher 离线恢复能力及 quickCheck 探针
 * [POS]: internal/store 的一致性快照生成与离线恢复容灾层 (PR-09)
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"icloud-hme/internal/security"
)

var (
	backupHookMu                                sync.RWMutex
	beforePreRestoreBackupHookForTest           func() error
	beforeRestoredDatabaseValidationHookForTest func() error
)

// SetBeforePreRestoreBackupHookForTest 设置恢复前置备份注入挂钩 (测试专用)
func SetBeforePreRestoreBackupHookForTest(hook func() error) {
	backupHookMu.Lock()
	defer backupHookMu.Unlock()
	beforePreRestoreBackupHookForTest = hook
}

// SetBeforeRestoredDatabaseValidationHookForTest 设置新库替换后验证前注入挂钩 (测试专用)
func SetBeforeRestoredDatabaseValidationHookForTest(hook func() error) {
	backupHookMu.Lock()
	defer backupHookMu.Unlock()
	beforeRestoredDatabaseValidationHookForTest = hook
}

// quickCheck 执行 PRAGMA quick_check 检查数据库物理完整性
func quickCheck(db *sql.DB) error {
	rows, err := db.Query("PRAGMA quick_check;")
	if err != nil {
		return fmt.Errorf("database integrity check failed: %w", err)
	}
	defer rows.Close()

	var messages []string
	for rows.Next() {
		var msg string
		if err := rows.Scan(&msg); err != nil {
			return fmt.Errorf("database integrity check failed: %w", err)
		}
		if msg != "ok" {
			messages = append(messages, msg)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("database integrity check failed: %w", err)
	}
	if len(messages) > 0 {
		return fmt.Errorf("database integrity check failed: %s", strings.Join(messages, "; "))
	}
	return nil
}

// createOnlineBackup 使用 SQLite VACUUM INTO 创建一致性在线备份
func createOnlineBackup(ctx context.Context, db *sql.DB, destination string) error {
	destAbs, err := filepath.Abs(destination)
	if err != nil {
		return fmt.Errorf("resolve destination path failed: %w", err)
	}

	// 1. 不覆盖已存在的目标文件
	if _, err := os.Stat(destAbs); err == nil {
		return fmt.Errorf("backup destination already exists: %s", destAbs)
	}

	destDir := filepath.Dir(destAbs)
	if err := os.MkdirAll(destDir, 0700); err != nil {
		return fmt.Errorf("create backup directory failed: %w", err)
	}
	_ = os.Chmod(destDir, 0700)

	// 2. 先写唯一临时文件
	tempPath := fmt.Sprintf("%s.tmp.%s", destAbs, NewOpaqueID("bak"))
	defer func() {
		_ = os.Remove(tempPath)
	}()

	// 3. 执行 VACUUM INTO 生成快照
	vacuumQuery := fmt.Sprintf("VACUUM INTO '%s'", strings.ReplaceAll(filepath.ToSlash(tempPath), "'", "''"))
	if _, err := db.ExecContext(ctx, vacuumQuery); err != nil {
		return fmt.Errorf("vacuum into failed: %w", err)
	}

	// 4. 对快照文件执行 PRAGMA quick_check 校验物理一致性
	bakDB, err := sql.Open("sqlite", fmt.Sprintf("%s?mode=ro", filepath.ToSlash(tempPath)))
	if err != nil {
		return fmt.Errorf("open backup for verification failed: %w", err)
	}
	qcErr := quickCheck(bakDB)
	_ = bakDB.Close()
	if qcErr != nil {
		return fmt.Errorf("backup integrity verification failed: %w", qcErr)
	}

	// 5. 权限收敛为 0600
	_ = os.Chmod(tempPath, 0600)

	// 6. 原子重命名到最终目标路径
	if err := os.Rename(tempPath, destAbs); err != nil {
		return fmt.Errorf("atomic rename backup failed: %w", err)
	}
	_ = os.Chmod(destAbs, 0600)

	return nil
}

// CreateBackup 创建一致性数据库快照 (对外 API)
func (s *Store) CreateBackup(ctx context.Context, destination string) error {
	if s == nil || s.closed.Load() {
		return errors.New("store is closed")
	}
	s.backupMu.Lock()
	defer s.backupMu.Unlock()

	return createOnlineBackup(ctx, s.db, destination)
}

// CreateDatabaseBackup 执行完全只读的离线数据库一致性备份，严禁触发任何数据库 schema 迁移或状态变更。
// 契约红线：
// 1. 获取 dataDir 实例独占排他锁；
// 2. 若运行中的 server/store 持锁，立即返回 database in use；
// 3. 检查 dataDir/icloud_hme.db 必须真实存在且为有效文件；
// 4. 直接打开 SQLite 连接 (严禁调用 NewStore / NewStoreWithCipher / schema migration)；
// 5. quick_check 检查物理完整性；
// 6. 使用 VACUUM INTO 创建一致性 snapshot；
// 7. 对 snapshot quick_check；
// 8. 权限收敛为 0600；
// 9. destination 不允许覆盖；
// 10. 成功后退出。
func CreateDatabaseBackup(ctx context.Context, dataDir string, destination string) error {
	// 1. acquire 当前已有的 dataDir instance lock
	// 2. 如果运行中的 server/store 持锁：返回 database in use
	lock, err := acquireDataDirLock(dataDir)
	if err != nil {
		return fmt.Errorf("database is in use; stop icloud-hme before backup: %w", err)
	}
	defer lock.Close()

	// 3. 检查：dataDir/icloud_hme.db 必须真实存在
	liveDBPath := filepath.Join(dataDir, "icloud_hme.db")
	stat, err := os.Stat(liveDBPath)
	if err != nil {
		return fmt.Errorf("source database not found: %w", err)
	}
	if stat.IsDir() || stat.Size() == 0 {
		return fmt.Errorf("source database is empty or not a regular file: %s", liveDBPath)
	}

	// 4. 直接打开 SQLite (严禁调用 NewStore / NewStoreWithCipher / schema migration)
	db, err := sql.Open("sqlite", fmt.Sprintf("%s?_pragma=busy_timeout(5000)", filepath.ToSlash(liveDBPath)))
	if err != nil {
		return fmt.Errorf("open source database failed: %w", err)
	}
	defer db.Close()

	// 5. PRAGMA quick_check 源库完整性
	if err := quickCheck(db); err != nil {
		return fmt.Errorf("source database integrity check failed: %w", err)
	}

	// 6. 使用 VACUUM INTO 创建一致性 snapshot
	// 7. 对 snapshot quick_check
	// 8. 权限 0600
	// 9. destination 不允许覆盖 (createOnlineBackup 内部第一步严格校验 destination 是否已存在并拒绝覆盖)
	if err := createOnlineBackup(ctx, db, destination); err != nil {
		return err
	}

	// 10. 成功后退出
	return nil
}

// restoreFileFromSnapshot 使用临时文件 -> fsync -> 原子 rename 从一致性快照恢复目标文件
func restoreFileFromSnapshot(srcSnapshot, dstFile string) error {
	tmpPath := dstFile + ".rollback.tmp"
	_ = os.Remove(tmpPath)

	src, err := os.Open(srcSnapshot)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}

	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		return err
	}

	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		return err
	}

	if err := dst.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	if err := os.Rename(tmpPath, dstFile); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	_ = os.Chmod(dstFile, 0600)
	return nil
}

// RestoreDatabase 实现离线数据库一致性恢复 (package-level API 兼容封装)
func RestoreDatabase(ctx context.Context, dataDir string, backupPath string) error {
	return RestoreDatabaseWithCipher(ctx, dataDir, backupPath, nil)
}

// RestoreDatabaseWithCipher 实现具备 Master Key 加密感知的离线数据库一致性恢复 (package-level API, PR-09)
func RestoreDatabaseWithCipher(ctx context.Context, dataDir string, backupPath string, cipher *security.SecretCipher) error {
	// 0. 尝试取得数据目录独占排他锁；若已被运行中的 Store/Server 持有则立即硬拒绝，绝不触碰生产库
	lock, err := acquireDataDirLock(dataDir)
	if err != nil {
		return fmt.Errorf("database is in use; stop icloud-hme before restore: %w", err)
	}
	defer lock.Close()

	// 1. 校验 backup 文件存在且为普通文件
	stat, err := os.Stat(backupPath)
	if err != nil {
		return fmt.Errorf("backup file not found: %w", err)
	}
	if stat.IsDir() {
		return fmt.Errorf("backup path is a directory: %s", backupPath)
	}

	// 2. 以 SQLite 只读方式打开备份
	bakDB, err := sql.Open("sqlite", fmt.Sprintf("%s?mode=ro", filepath.ToSlash(backupPath)))
	if err != nil {
		return fmt.Errorf("open backup failed: %w", err)
	}

	// 3. PRAGMA quick_check 必须为 ok
	if err := quickCheck(bakDB); err != nil {
		_ = bakDB.Close()
		return fmt.Errorf("backup integrity check failed: %w", err)
	}

	// 4. 检查 user_version：backup version <= CurrentSchemaVersion, future version 拒绝恢复
	var backupVersion int
	if err := bakDB.QueryRowContext(ctx, "PRAGMA user_version;").Scan(&backupVersion); err != nil {
		_ = bakDB.Close()
		return fmt.Errorf("read backup user_version failed: %w", err)
	}
	_ = bakDB.Close()

	if backupVersion > CurrentSchemaVersion {
		return fmt.Errorf("backup schema version %d is newer than supported version %d", backupVersion, CurrentSchemaVersion)
	}

	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return fmt.Errorf("create dataDir failed: %w", err)
	}
	_ = os.Chmod(dataDir, 0700)

	liveDBPath := filepath.Join(dataDir, "icloud_hme.db")

	// 5. 如果当前 dataDir 已有数据库：先生成一致性的 pre-restore-<timestamp>.db 快照 (Fail-Closed: 失败直接中断，绝不继续破坏现场)
	var preRestorePath string
	if liveStat, err := os.Stat(liveDBPath); err == nil && liveStat.Size() > 0 {
		backupsDir := filepath.Join(dataDir, "backups")
		if err := os.MkdirAll(backupsDir, 0700); err != nil {
			return fmt.Errorf("create backups directory failed: %w", err)
		}
		_ = os.Chmod(backupsDir, 0700)

		baseName := fmt.Sprintf("pre-restore-%s", time.Now().UTC().Format("20060102T150405Z"))
		targetPath := filepath.Join(backupsDir, baseName+".db")
		for seq := 1; ; seq++ {
			if _, err := os.Stat(targetPath); os.IsNotExist(err) {
				break
			}
			targetPath = filepath.Join(backupsDir, fmt.Sprintf("%s_%d.db", baseName, seq))
		}

		backupHookMu.RLock()
		preHook := beforePreRestoreBackupHookForTest
		backupHookMu.RUnlock()
		if preHook != nil {
			if err := preHook(); err != nil {
				return fmt.Errorf("pre-restore backup hook failed: %w", err)
			}
		}

		liveDB, openErr := sql.Open("sqlite", fmt.Sprintf("%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)", filepath.ToSlash(liveDBPath)))
		if openErr != nil {
			return fmt.Errorf("open live database for pre-restore backup failed: %w", openErr)
		}
		bakErr := createOnlineBackup(ctx, liveDB, targetPath)
		closeErr := liveDB.Close()
		if bakErr != nil {
			return fmt.Errorf("pre-restore backup failed: %w", bakErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close live database after pre-restore backup failed: %w", closeErr)
		}
		preRestorePath = targetPath
	}

	// 6. 将 restore source 复制到 icloud_hme.db.restore.tmp，权限 0600
	restoreTmp := filepath.Join(dataDir, "icloud_hme.db.restore.tmp")
	defer func() {
		_ = os.Remove(restoreTmp)
	}()

	srcFile, err := os.Open(backupPath)
	if err != nil {
		return fmt.Errorf("open backup file for reading failed: %w", err)
	}
	defer srcFile.Close()

	tmpFile, err := os.OpenFile(restoreTmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("create restore tmp file failed: %w", err)
	}

	if _, err := io.Copy(tmpFile, srcFile); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("copy backup to tmp failed: %w", err)
	}

	// 7. fsync / close
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("fsync restore tmp file failed: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close restore tmp file failed: %w", err)
	}

	// 8. 原子替换原 liveDBPath (如果原文件存在，先重命名保留 rollback 现场)
	liveBakPath := liveDBPath + ".live.bak"
	_ = os.Remove(liveBakPath)
	hasLiveDB := false
	if _, err := os.Stat(liveDBPath); err == nil {
		hasLiveDB = true
		if err := os.Rename(liveDBPath, liveBakPath); err != nil {
			return fmt.Errorf("backup existing database before swap failed: %w", err)
		}
	}

	if err := os.Rename(restoreTmp, liveDBPath); err != nil {
		if hasLiveDB {
			_ = os.Rename(liveBakPath, liveDBPath)
		}
		return fmt.Errorf("atomic rename restored db failed: %w", err)
	}
	_ = os.Chmod(liveDBPath, 0600)

	// 9. 删除属于旧数据库实例的 stale 边车文件
	_ = os.Remove(filepath.Join(dataDir, "icloud_hme.db-wal"))
	_ = os.Remove(filepath.Join(dataDir, "icloud_hme.db-shm"))

	// 10. Hook & NewStore 验证：若校验失败，必须从一致性快照 preRestorePath 进行回滚
	backupHookMu.RLock()
	valHook := beforeRestoredDatabaseValidationHookForTest
	backupHookMu.RUnlock()

	var validationErr error
	if valHook != nil {
		validationErr = valHook()
	}
	if validationErr == nil {
		// 校验恢复库完整性与架构兼容性 (因当前函数已持有独占锁，newStoreWithLock 传 nil lock 避免重复申请)
		st, err := newStoreWithLock(dataDir, nil, cipher)
		if err != nil {
			validationErr = err
		} else {
			if valErr := st.ValidateProtectedSecrets(); valErr != nil {
				validationErr = valErr
			}
			_ = st.Close()
		}
	}

	if validationErr != nil {
		// 恢复验证失败：必须从权威一致性快照 preRestorePath 进行回滚！
		// 恢复 rollback snapshot 采用: temp -> fsync -> atomic rename，并清理新 restore 产生的 WAL/SHM
		_ = os.Remove(filepath.Join(dataDir, "icloud_hme.db-wal"))
		_ = os.Remove(filepath.Join(dataDir, "icloud_hme.db-shm"))
		_ = os.Remove(liveDBPath)

		if preRestorePath != "" {
			if rErr := restoreFileFromSnapshot(preRestorePath, liveDBPath); rErr != nil {
				return fmt.Errorf("validation failed: %w; rollback to pre-restore snapshot failed: %v", validationErr, rErr)
			}
		} else if hasLiveDB {
			_ = os.Rename(liveBakPath, liveDBPath)
		}
		// preRestorePath 永久保留，绝不删除
		return fmt.Errorf("failed to validate restored database: %w", validationErr)
	}

	// 恢复确认成功，清理临时 rollback 副本 (注意：preRestorePath 必须永久保留，绝不删除)
	_ = os.Remove(liveBakPath)

	return nil
}
