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
	mu          sync.Mutex
	dataDir     string
	db          *sql.DB
	activityCh  chan string
	stopCh      chan struct{}
	flusherDone chan struct{}
	closed      bool
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

	if err := s.initSchema(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化表结构失败: %w", err)
	}
	// initSchema 落库后 WAL/SHM 边车文件才真正产生，此处再收紧一轮
	restrictFileMode(dataDir, "icloud_hme.db")
	restrictFileMode(dataDir, "icloud_hme.db-wal")
	restrictFileMode(dataDir, "icloud_hme.db-shm")

	// 自动无损迁移遗留的 JSON 数据
	s.migrateLegacyJSON()

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
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.stopCh)
	s.mu.Unlock()

	// 等待最后一次批量刷盘完全执行完毕，杜绝与 db.Close() 竞态
	<-s.flusherDone

	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// Ping 探测底层数据库连通性 (PR-01 用于健康检查 readyz 探针)。
func (s *Store) Ping(ctx context.Context) error {
	s.mu.Lock()
	if s.closed || s.db == nil {
		s.mu.Unlock()
		return errors.New("store is closed")
	}
	db := s.db
	s.mu.Unlock()
	return db.PingContext(ctx)
}

// tableHasColumn 使用 PRAGMA table_info 精准探测表字段，绝不盲目依赖忽略 ALTER 报错
func tableHasColumn(db *sql.DB, tableName, colName string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", tableName))
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dfltValue any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err == nil {
			if name == colName {
				return true, nil
			}
		}
	}
	return false, rows.Err()
}

func (s *Store) initSchema() error {
	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA busy_timeout = 5000;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA foreign_keys = ON;",
	}
	for _, p := range pragmas {
		if _, err := s.db.Exec(p); err != nil {
			return err
		}
	}

	// 1. 创建基础表 (暂不包含依赖扩展字段的索引)
	baseDDL := `
	CREATE TABLE IF NOT EXISTS business_tags (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		tag TEXT NOT NULL UNIQUE,
		description TEXT,
		status TEXT DEFAULT 'active',
		created_at TEXT NOT NULL,
		last_assigned_at TEXT
	);

	CREATE TABLE IF NOT EXISTS api_tokens (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		token TEXT NOT NULL UNIQUE,
		created_at TEXT NOT NULL,
		last_used_at TEXT
	);

	CREATE TABLE IF NOT EXISTS lease_records (
		id TEXT PRIMARY KEY,
		email TEXT NOT NULL,
		account_id TEXT NOT NULL,
		tag TEXT NOT NULL,
		status TEXT NOT NULL,
		allocated_at TEXT NOT NULL,
		completed_at TEXT
	);

	CREATE TABLE IF NOT EXISTS schedules (
		account_id TEXT PRIMARY KEY,
		enabled INTEGER NOT NULL DEFAULT 0,
		hourly_quota INTEGER NOT NULL DEFAULT 5,
		current_hour_count INTEGER NOT NULL DEFAULT 0,
		last_hour_window INTEGER NOT NULL DEFAULT 0,
		last_run_at TEXT
	);

	CREATE TABLE IF NOT EXISTS settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS alias_routes (
		email      TEXT PRIMARY KEY,
		account_id TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS accounts (
		id            TEXT PRIMARY KEY,
		name          TEXT NOT NULL DEFAULT '',
		real_email    TEXT DEFAULT '',
		icloud_email  TEXT DEFAULT '',
		cookies       TEXT DEFAULT '{}',
		host          TEXT DEFAULT 'icloud.com',
		service_url   TEXT DEFAULT '',
		proxy         TEXT DEFAULT '',
		app_password  TEXT DEFAULT '',
		mailbox       TEXT DEFAULT '',
		status        TEXT DEFAULT 'pending',
		alias_total   INTEGER DEFAULT 0,
		alias_active  INTEGER DEFAULT 0,
		last_validated TEXT DEFAULT '',
		last_error    TEXT DEFAULT '',
		created_at    TEXT NOT NULL,
		tags          TEXT DEFAULT '[]',
		updated_at    TEXT NOT NULL
	);
	`
	if _, err := s.db.Exec(baseDDL); err != nil {
		return fmt.Errorf("创建基础表失败: %w", err)
	}

	// 2. 使用 PRAGMA table_info 探测并补充历史遗留库缺失的字段
	hasTokenName, err := tableHasColumn(s.db, "lease_records", "token_name")
	if err != nil {
		return err
	}
	if !hasTokenName {
		if _, err := s.db.Exec(`ALTER TABLE lease_records ADD COLUMN token_name TEXT DEFAULT ''`); err != nil {
			return fmt.Errorf("alter lease_records add token_name failed: %w", err)
		}
	}

	hasScopes, err := tableHasColumn(s.db, "api_tokens", "scopes")
	if err != nil {
		return err
	}
	if !hasScopes {
		if _, err := s.db.Exec(`ALTER TABLE api_tokens ADD COLUMN scopes TEXT NOT NULL DEFAULT 'admin'`); err != nil {
			return fmt.Errorf("alter api_tokens add scopes failed: %w", err)
		}
	}

	scheduleCols := []struct {
		col string
		def string
	}{
		{"alias_label", "TEXT DEFAULT 'scheduled'"},
		{"mode", "TEXT DEFAULT 'always'"},
		{"start_time", "TEXT DEFAULT ''"},
		{"end_time", "TEXT DEFAULT ''"},
		{"duration_hours", "INTEGER DEFAULT 0"},
		{"started_at", "TEXT DEFAULT ''"},
	}
	for _, sc := range scheduleCols {
		hasCol, err := tableHasColumn(s.db, "schedules", sc.col)
		if err != nil {
			return err
		}
		if !hasCol {
			if _, err := s.db.Exec(fmt.Sprintf(`ALTER TABLE schedules ADD COLUMN %s %s`, sc.col, sc.def)); err != nil {
				return fmt.Errorf("alter schedules add %s failed: %w", sc.col, err)
			}
		}
	}

	// 3. 字段补充完毕后，安全创建基础表索引 (包括依赖 token_name 的覆盖复合索引)
	indexesDDL := `
	CREATE INDEX IF NOT EXISTS idx_leases_allocated_at ON lease_records (allocated_at DESC);
	CREATE INDEX IF NOT EXISTS idx_leases_email ON lease_records (email);
	CREATE INDEX IF NOT EXISTS idx_leases_email_lower ON lease_records (LOWER(email));
	CREATE INDEX IF NOT EXISTS idx_leases_email_lower_token ON lease_records (LOWER(email), token_name);
	CREATE INDEX IF NOT EXISTS idx_leases_tag ON lease_records (tag);
	CREATE INDEX IF NOT EXISTS idx_leases_status ON lease_records (status);
	CREATE INDEX IF NOT EXISTS idx_alias_routes_account ON alias_routes (account_id);
	CREATE INDEX IF NOT EXISTS idx_accounts_status ON accounts(status);
	`
	if _, err := s.db.Exec(indexesDDL); err != nil {
		return fmt.Errorf("创建基础表索引失败: %w", err)
	}

	// 4. 初始化领域库存与操作表
	return s.initInventorySchema()
}
