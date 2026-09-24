/**
 * [INPUT]: 依赖 database/sql, modernc.org/sqlite, os, path/filepath, sync, time, encoding/hex, crypto/rand
 * [OUTPUT]: 对外提供 Store 结构定义、NewStore、Close 引擎生命周期与 initSchema SQLite 数据库与 DDL 初始化
 * [POS]: internal/store 的核心存储引擎，基于纯 Go 嵌入式 SQLite 驱动与 WAL 模式维护底层数据流与生命周期
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

var opaqueIDSeq uint64

// restrictFileMode 把数据目录下的指定文件收紧为 0600(不存在或平台不支持时静默跳过)。
// 注意 os.Chmod 在 Windows 上只映射只读位、不写 ACL，Windows 部署需另行收紧 ACL。
func restrictFileMode(dir, name string) {
	_ = os.Chmod(filepath.Join(dir, name), 0600)
}

// NewOpaqueID 用 CSPRNG 生成带前缀的唯一主键。
//
// 【设计红线】绝不能用 time.Now().UnixNano() 之类的时钟值当主键：Windows 上
// 时钟粒度约 15ms，同一刻度内并发生成的 ID 完全相同，再叠加 UPSERT 语义就会
// 静默覆盖掉刚写入的记录(接口照常返回 200)。crypto/rand 失败属极罕见情况，
// 此时退回「纳秒 + 进程内原子递增」仍能保证同一进程内不重复。
func NewOpaqueID(prefix string) string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s%d_%d", prefix, time.Now().UnixNano(), atomic.AddUint64(&opaqueIDSeq, 1))
	}
	return prefix + hex.EncodeToString(buf)
}

func newOpaqueID(prefix string) string {
	return NewOpaqueID(prefix)
}

// Store 统管中台数据 (基于嵌入式 SQLite)
type Store struct {
	mu             sync.Mutex
	backupMu       sync.Mutex
	dataDir        string
	db             *sql.DB
	activityCh     chan string
	stopCh         chan struct{}
	flusherDone    chan struct{}
	closed         atomic.Bool
	hookMu         sync.RWMutex
	beforePingHook func(ctx context.Context)
}

// DB 返回底层数据库句柄 (仅用于测试/诊断注入)
func (s *Store) DB() *sql.DB {
	return s.db
}

// NewStore 创建并加载 Store
func NewStore(dataDir string) (*Store, error) {
	if dataDir == "" {
		dataDir = "data"
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	// 目录可能由旧版本以 0755 创建，这里强制收紧，避免同机其他用户读取凭据库
	_ = os.Chmod(dataDir, 0700)

	dbPath := filepath.Join(dataDir, "icloud_hme.db")
	dbStat, statErr := os.Stat(dbPath)
	dbExistedBefore := statErr == nil && dbStat.Size() > 0

	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)", filepath.ToSlash(dbPath))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite 数据库失败: %w", err)
	}
	// 凭据库含明文 Apple Cookie/App 专用密码，只允许属主读写（含 WAL/SHM 边车文件）
	restrictFileMode(dataDir, "icloud_hme.db")
	restrictFileMode(dataDir, "icloud_hme.db-wal")
	restrictFileMode(dataDir, "icloud_hme.db-shm")

	// 针对 SQLite WAL 模式优化连接池
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(time.Hour)

	s := &Store{
		dataDir:     dataDir,
		db:          db,
		activityCh:  make(chan string, 256),
		stopCh:      make(chan struct{}),
		flusherDone: make(chan struct{}),
	}

	if err := s.initSchema(dbExistedBefore); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化表结构失败: %w", err)
	}
	// initSchema 落库后 WAL/SHM 边车文件才真正产生，此处再收紧一轮
	restrictFileMode(dataDir, "icloud_hme.db")
	restrictFileMode(dataDir, "icloud_hme.db-wal")
	restrictFileMode(dataDir, "icloud_hme.db-shm")

	// 自动无损迁移遗留的 JSON 数据 (Fail-Closed: 失败直接拒绝启动)
	if err := s.migrateLegacyJSON(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("迁移遗留 JSON 失败: %w", err)
	}

	// 用出号流水回填别名路由(覆盖本功能上线前已分配的别名)
	s.backfillAliasRoutes()

	// 自动迁移历史别名库存与唯一分配关系 (PR-03)
	if err := s.migrateInventory(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("迁移库存失败: %w", err)
	}

	// 启动令牌活跃度异步批量刷新器
	go s.activityFlusher()

	return s, nil
}

// Close 关闭底层数据库连接与后台协程
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed.Swap(true) {
		s.mu.Unlock()
		return nil
	}
	close(s.stopCh)
	s.mu.Unlock()

	// 等待最后一次批量刷盘完全执行完毕，杜绝与 db.Close() 竞态
	<-s.flusherDone

	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// LockForTest 测试专用：模拟业务长事务占有业务大锁。
func (s *Store) LockForTest() {
	s.mu.Lock()
}

// UnlockForTest 测试专用：释放模拟业务大锁。
func (s *Store) UnlockForTest() {
	s.mu.Unlock()
}

// SetBeforePingHookForTest 设置探活前置挂钩 (仅用于单元测试中的可控同步点)。
func (s *Store) SetBeforePingHookForTest(hook func(ctx context.Context)) {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	s.beforePingHook = hook
}

// Ping 探测底层数据库连通性 (PR-01 用于健康检查 readyz 探针)。
// 严格避免争抢 s.mu 业务大锁，通过 atomic.Bool 安全读取关闭状态并直接透传给 db.PingContext(ctx)，
// 原生保证 context 超时与取消能立即从驱动层返回，杜绝探针在业务互斥锁上死等。
func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.closed.Load() {
		return errors.New("store is closed")
	}
	s.hookMu.RLock()
	hook := s.beforePingHook
	s.hookMu.RUnlock()
	if hook != nil {
		hook(ctx)
	}
	db := s.db
	if db == nil {
		return errors.New("store is closed")
	}
	return db.PingContext(ctx)
}

func (s *Store) initSchema(dbExistedBefore bool) error {
	// 1. Startup Integrity Gate: 启动阶段物理损坏硬拦截 (必须先于任何写入性 PRAGMA 执行)
	if dbExistedBefore {
		if err := quickCheck(s.db); err != nil {
			return err
		}
	}

	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA busy_timeout = 5000;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA foreign_keys = ON;",
	}
	for _, p := range pragmas {
		if _, err := s.db.Exec(p); err != nil {
			if dbExistedBefore {
				return fmt.Errorf("database integrity check failed: %w", err)
			}
			return err
		}
	}

	// 2. 检查 user_version
	v, err := getUserVersion(s.db)
	if err != nil {
		return fmt.Errorf("read schema version failed: %w", err)
	}

	// 3. 拒绝未来版本数据库
	if v > CurrentSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", v, CurrentSchemaVersion)
	}

	// 4. 迁移前自动生成一致性快照 (仅在数据库文件已存在且 user_version < CurrentSchemaVersion 时)
	if dbExistedBefore && v < CurrentSchemaVersion {
		backupsDir := filepath.Join(s.dataDir, "backups")
		if err := os.MkdirAll(backupsDir, 0700); err != nil {
			return fmt.Errorf("create backups directory failed: %w", err)
		}
		_ = os.Chmod(backupsDir, 0700)
		baseBackupName := fmt.Sprintf("pre-migrate-v%d-to-v%d-%s", v, CurrentSchemaVersion, time.Now().UTC().Format("20060102T150405Z"))
		backupPath := filepath.Join(backupsDir, baseBackupName+".db")
		for seq := 1; ; seq++ {
			if _, err := os.Stat(backupPath); os.IsNotExist(err) {
				break
			}
			backupPath = filepath.Join(backupsDir, fmt.Sprintf("%s_%d.db", baseBackupName, seq))
		}
		if err := createOnlineBackup(context.Background(), s.db, backupPath); err != nil {
			return fmt.Errorf("pre-migration backup failed: %w", err)
		}
	}

	// 5. Version 0 -> Version 1 事务化迁移
	if v < CurrentSchemaVersion {
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration tx failed: %w", err)
		}
		if err := migrateV0ToV1(tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migrate v0 to v1 failed: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration tx failed: %w", err)
		}
	}

	// 6. Schema 完整性终态校验门禁
	if err := validateSchema(s.db); err != nil {
		return err
	}

	return nil
}
