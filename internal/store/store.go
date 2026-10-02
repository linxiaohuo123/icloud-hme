/**
 * [INPUT]: 依赖 database/sql, modernc.org/sqlite, os, path/filepath, sync, time, encoding/hex, crypto/rand
 * [OUTPUT]: 对外提供 Store 结构定义、NewStore、Close 引擎生命周期与 initSchema SQLite 数据库与 DDL 初始化
 * [POS]: internal/store 的核心引擎，维护 WAL 与事务化迁移，v12 持久化取码物理来源，事务升级并保留迁移前快照
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
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"icloud-hme/internal/security"
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

// contextMutex 提供兼具 LockContext(ctx) 与标准 Lock()/Unlock() 的单一真相源互斥锁 (T4)。
// 支持在等待写锁期间响应 Context 取消退出，且零值与并发初始化绝对安全。
type contextMutex struct {
	once sync.Once
	ch   chan struct{}
}

func (m *contextMutex) init() {
	m.once.Do(func() {
		m.ch = make(chan struct{}, 1)
		m.ch <- struct{}{}
	})
}

func (m *contextMutex) Lock() {
	m.init()
	<-m.ch
}

func (m *contextMutex) Unlock() {
	m.init()
	select {
	case m.ch <- struct{}{}:
	default:
		panic("unlock of unlocked mutex")
	}
}

func (m *contextMutex) LockContext(ctx context.Context) error {
	m.init()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.ch:
		if err := ctx.Err(); err != nil {
			m.ch <- struct{}{}
			return err
		}
		return nil
	}
}

// Store 统管中台数据 (基于嵌入式 SQLite)
type Store struct {
	mu             contextMutex
	backupMu       sync.Mutex
	instanceLock   dataDirLock
	dataDir        string
	db             *sql.DB
	cipher         *security.SecretCipher
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

// Cipher 返回底层加密机
func (s *Store) Cipher() *security.SecretCipher {
	return s.cipher
}

// SetCipherForTest 测试专用：注入加密机
func (s *Store) SetCipherForTest(cipher *security.SecretCipher) {
	s.cipher = cipher
}

// NewStore 创建并加载 Store (优先从环境变量读取 Master Key, 测试环境下缺省允许空 cipher 运行非凭据流程)
func NewStore(dataDir string) (*Store, error) {
	return NewStoreWithCipher(dataDir, nil)
}

// NewStoreWithCipher 创建并加载 Store，显式注入 SecretCipher 加密机
func NewStoreWithCipher(dataDir string, cipher *security.SecretCipher) (*Store, error) {
	if dataDir == "" {
		dataDir = "data"
	}
	lock, err := acquireDataDirLock(dataDir)
	if err != nil {
		return nil, fmt.Errorf("acquire data directory lock failed: %w", err)
	}

	st, err := newStoreWithLock(dataDir, lock, cipher)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	return st, nil
}

// newStoreWithoutLockForTest 测试专用：创建连接到同一目录但不申请目录排他锁的 Store (用于同进程多连接并发事务测试)
func newStoreWithoutLockForTest(dataDir string) (*Store, error) {
	return newStoreWithLock(dataDir, nil, nil)
}

func newStoreWithLock(dataDir string, lock dataDirLock, cipher *security.SecretCipher) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	// 目录可能由旧版本以 0755 创建，这里强制收紧，避免同机其他用户读取凭据库
	_ = os.Chmod(dataDir, 0700)

	// 如果外部未显式传 cipher，则尝试从环境变量加载
	if cipher == nil {
		if k, err := security.LoadMasterKey(); err == nil {
			cipher, _ = security.NewSecretCipher(k)
		}
	}

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
		instanceLock: lock,
		dataDir:      dataDir,
		db:           db,
		cipher:       cipher,
		activityCh:   make(chan string, 256),
		stopCh:       make(chan struct{}),
		flusherDone:  make(chan struct{}),
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

	var dbErr error
	if s.db != nil {
		dbErr = s.db.Close()
	}
	if s.instanceLock != nil {
		if lockErr := s.instanceLock.Close(); lockErr != nil && dbErr == nil {
			dbErr = lockErr
		}
		s.instanceLock = nil
	}
	return dbErr
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
// 严格避免争抢 s.mu 业务大锁，通过 atomic.Bool 安全读取关闭状态并透传给 db.PingContext(ctx)。
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
	if err := ctx.Err(); err != nil {
		return err
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
		log.Printf("[Security] 存在敏感 rollback backup: %s (包含迁移前明文凭据，上线验证完成后请安全归档或销毁)", backupPath)
	}

	// 5. 顺序迁移调度器 (Version 0 -> Version 1 -> Version 2 ...)
	for currentV := v; currentV < CurrentSchemaVersion; currentV++ {
		switch currentV {
		case 0:
			tx, err := s.db.Begin()
			if err != nil {
				return fmt.Errorf("begin migration v0 to v1 tx failed: %w", err)
			}
			if err := migrateV0ToV1(tx); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migrate v0 to v1 failed: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit migration v0 to v1 tx failed: %w", err)
			}
		case 1:
			if err := s.migrateV1ToV2(); err != nil {
				return fmt.Errorf("migrate v1 to v2 failed: %w", err)
			}
		case 2:
			tx, err := s.db.Begin()
			if err != nil {
				return fmt.Errorf("begin migration v2 to v3 tx failed: %w", err)
			}
			if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS revoked_sessions (
				session_hash BLOB PRIMARY KEY,
				expires_at INTEGER NOT NULL
			)`); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migrate v2 to v3 failed: %w", err)
			}
			if _, err := tx.Exec("PRAGMA user_version = 3;"); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("set schema version 3 failed: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit migration v2 to v3 tx failed: %w", err)
			}
		case 3:
			tx, err := s.db.Begin()
			if err != nil {
				return fmt.Errorf("begin migration v3 to v4 tx failed: %w", err)
			}
			if err := ensureColumn(tx, "accounts", "apple_dsid", "TEXT DEFAULT ''"); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migrate v3 to v4 failed: %w", err)
			}
			if _, err := tx.Exec("PRAGMA user_version = 4;"); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("set schema version 4 failed: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit migration v3 to v4 tx failed: %w", err)
			}
		case 4:
			tx, err := s.db.Begin()
			if err != nil {
				return fmt.Errorf("begin migration v4 to v5 tx failed: %w", err)
			}
			for _, col := range []struct{ table, name, def string }{
				{"hme_reserve_intents", "operation_id", "TEXT DEFAULT ''"},
				{"operations", "business_tag", "TEXT DEFAULT ''"},
				{"operations", "token_name", "TEXT DEFAULT ''"},
				{"operations", "result_source", "TEXT NOT NULL DEFAULT 'pool'"},
			} {
				if err := ensureColumn(tx, col.table, col.name, col.def); err != nil {
					_ = tx.Rollback()
					return fmt.Errorf("migrate v4 to v5 failed: %w", err)
				}
			}
			if _, err := tx.Exec("PRAGMA user_version = 5;"); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("set schema version 5 failed: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit migration v4 to v5 tx failed: %w", err)
			}
		case 5:
			tx, err := s.db.Begin()
			if err != nil {
				return fmt.Errorf("begin migration v5 to v6 tx failed: %w", err)
			}
			// Match the timezone-aware ordering used by lease queries. Keep the
			// original timestamp text intact for historical records and API clients.
			if _, err := tx.Exec(`
				CREATE INDEX IF NOT EXISTS idx_leases_allocated_time ON lease_records (julianday(allocated_at) DESC, id DESC);
				CREATE INDEX IF NOT EXISTS idx_leases_email_time ON lease_records (LOWER(email), julianday(allocated_at) DESC, id DESC);
				PRAGMA user_version = 6;
			`); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migrate v5 to v6 failed: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit migration v5 to v6 tx failed: %w", err)
			}
		case 6:
			tx, err := s.db.Begin()
			if err != nil {
				return fmt.Errorf("begin migration v6 to v7 tx failed: %w", err)
			}
			if _, err := tx.Exec(`
				CREATE TABLE IF NOT EXISTS camoufox_tasks (
					account_id TEXT PRIMARY KEY,
					task_id TEXT NOT NULL UNIQUE,
					base_url TEXT NOT NULL,
					created_at TEXT NOT NULL,
					updated_at TEXT NOT NULL
				);
				CREATE INDEX IF NOT EXISTS idx_camoufox_tasks_task ON camoufox_tasks (task_id);
				PRAGMA user_version = 7;
			`); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migrate v6 to v7 failed: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit migration v6 to v7 tx failed: %w", err)
			}
		case 7:
			// v8 stores a versioned browser session in the existing encrypted
			// cookies column. Legacy maps remain readable; old binaries must not
			// open and silently discard the richer credential format.
			if _, err := s.db.Exec(`PRAGMA user_version = 8;`); err != nil {
				return fmt.Errorf("migrate v7 to v8 failed: %w", err)
			}
		case 8:
			tx, err := s.db.Begin()
			if err != nil {
				return fmt.Errorf("begin migration v8 to v9: %w", err)
			}
			// Historical requests have unknown purposes and must remain isolated.
			if err := ensureColumn(tx, "hme_reserve_intents", "purpose", "TEXT NOT NULL DEFAULT ''"); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migrate v8 to v9: %w", err)
			}
			if _, err := tx.Exec(`PRAGMA user_version = 9;`); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("set schema version 9: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit migration v8 to v9: %w", err)
			}
		case 9:
			tx, err := s.db.Begin()
			if err != nil {
				return fmt.Errorf("begin migration v9 to v10: %w", err)
			}
			if err := ensureColumn(tx, "verification_requests", "idempotency_key", "TEXT DEFAULT ''"); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migrate v9 to v10 add idempotency_key: %w", err)
			}
			if err := ensureColumn(tx, "verification_requests", "idempotency_hash", "TEXT DEFAULT ''"); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migrate v9 to v10 add idempotency_hash: %w", err)
			}
			if _, err := tx.Exec(`
				CREATE INDEX IF NOT EXISTS idx_vreq_status_exp ON verification_requests (status, expires_at);
				CREATE UNIQUE INDEX IF NOT EXISTS uidx_vreq_idempotency ON verification_requests (principal_kind, principal_id, idempotency_key) WHERE idempotency_key != '';
				PRAGMA user_version = 10;
			`); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migrate v9 to v10 exec failed: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit migration v9 to v10 failed: %w", err)
			}
		case 10:
			tx, err := s.db.Begin()
			if err != nil {
				return fmt.Errorf("begin migration v10 to v11: %w", err)
			}
			if err := migrateV10ToV11(tx); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migrate v10 to v11: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit migration v10 to v11: %w", err)
			}
		case 11:
			tx, err := s.db.Begin()
			if err != nil {
				return fmt.Errorf("begin migration v11 to v12: %w", err)
			}
			if err := migrateV11ToV12(tx); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migrate v11 to v12: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit migration v11 to v12: %w", err)
			}
		default:
			return fmt.Errorf("unsupported migration path from version %d", currentV)
		}
	}

	// 5.1 启动期幂等补齐与自愈: 针对可能因历史版本已处于 10 但缺少 v10 索引的数据库自愈修复
	if err := repairV10SchemaIfNeeded(s.db); err != nil {
		return fmt.Errorf("repair v10 schema failed: %w", err)
	}

	// 6. Schema 完整性终态校验门禁
	if err := validateSchema(s.db); err != nil {
		return err
	}

	return nil
}

func repairV10SchemaIfNeeded(db *sql.DB) error {
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='verification_requests'").Scan(&count); err != nil || count == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := ensureColumn(tx, "verification_requests", "idempotency_key", "TEXT DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureColumn(tx, "verification_requests", "idempotency_hash", "TEXT DEFAULT ''"); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		CREATE INDEX IF NOT EXISTS idx_vreq_status_exp ON verification_requests (status, expires_at);
		CREATE UNIQUE INDEX IF NOT EXISTS uidx_vreq_idempotency ON verification_requests (principal_kind, principal_id, idempotency_key) WHERE idempotency_key != '';
	`); err != nil {
		return err
	}
	return tx.Commit()
}
