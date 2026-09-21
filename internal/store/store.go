/**
 * [INPUT]: 依赖 database/sql, modernc.org/sqlite, os, path/filepath, sync, time, encoding/json
 * [OUTPUT]: 对外提供 Store 结构及其对 Tags, Tokens, Leases, Schedules, Pool-First 别名池原子认领、CountConsumedPoolAliases 与覆盖索引的高性能持久化能力
 * [POS]: internal/store 的持久化层，基于纯 Go 嵌入式 SQLite 引擎提供零依赖、无损事务存储；ClaimPoolAlias 支持并发安全原子认领与多注册机防重号
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

var leaseSeq uint64
var opaqueIDSeq uint64

// restrictFileMode 把数据目录下的指定文件收紧为 0600(不存在或平台不支持时静默跳过)。
// 注意 os.Chmod 在 Windows 上只映射只读位、不写 ACL，Windows 部署需另行收紧 ACL。
func restrictFileMode(dir, name string) {
	_ = os.Chmod(filepath.Join(dir, name), 0600)
}

// NewBusinessTagID 生成业务标识主键。
func NewBusinessTagID() string { return newOpaqueID("tag_") }

// NewAPITokenID 生成外部令牌主键。
func NewAPITokenID() string { return newOpaqueID("tok_") }

// newOpaqueID 用 CSPRNG 生成带前缀的唯一主键。
//
// 【设计红线】绝不能用 time.Now().UnixNano() 之类的时钟值当主键：Windows 上
// 时钟粒度约 15ms，同一刻度内并发生成的 ID 完全相同，再叠加 UPSERT 语义就会
// 静默覆盖掉刚写入的记录(接口照常返回 200)。crypto/rand 失败属极罕见情况，
// 此时退回「纳秒 + 进程内原子递增」仍能保证同一进程内不重复。
func newOpaqueID(prefix string) string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s%d_%d", prefix, time.Now().UnixNano(), atomic.AddUint64(&opaqueIDSeq, 1))
	}
	return prefix + hex.EncodeToString(buf)
}

// BusinessTag 业务标识
type BusinessTag struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Tag            string `json:"tag"`
	Description    string `json:"description"`
	Status         string `json:"status"` // "active" | "disabled"
	CreatedAt      string `json:"created_at"`
	LastAssignedAt string `json:"last_assigned_at,omitempty"`
}

// APIToken 外部接入长效令牌
type APIToken struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Token      string `json:"token"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at,omitempty"`
	// Scopes 是逗号分隔的授权作用域。空值等同 "admin"(兼容升级前的历史令牌)，
	// 对外发放的令牌应显式收窄为 "allocate,verify"。
	Scopes string `json:"scopes,omitempty"`
}

// ScopeAdmin 是管理员级作用域(账号/令牌/设置/代理等管理面操作)。
const ScopeAdmin = "admin"

// ScopeAllocate 允许调用出号接口。
const ScopeAllocate = "allocate"

// ScopeVerify 允许调用验证码提取接口。
const ScopeVerify = "verify"

// DefaultExternalScopes 是对外发放令牌的默认最小权限集合。
const DefaultExternalScopes = ScopeAllocate + "," + ScopeVerify

// HasScope 判断作用域集合是否包含目标作用域；空集合视为 admin(历史令牌兼容)。
func HasScope(scopes, want string) bool {
	scopes = strings.TrimSpace(scopes)
	if scopes == "" || scopes == ScopeAdmin {
		return true
	}
	if want == ScopeAdmin {
		return false
	}
	for _, s := range strings.Split(scopes, ",") {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case want:
			return true
		case ScopeAdmin:
			return true
		}
	}
	return false
}

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

// ScheduleConfig 账号定时补货配置
type ScheduleConfig struct {
	AccountID        string `json:"account_id"`
	Enabled          bool   `json:"enabled"`
	HourlyQuota      int    `json:"hourly_quota"`
	AliasLabel       string `json:"alias_label"`
	CurrentHourCount int    `json:"current_hour_count"`
	LastHourWindow   int64  `json:"last_hour_window"`
	LastRunAt        string `json:"last_run_at,omitempty"`
	Mode             string `json:"mode,omitempty"`           // "always" | "daily_window" | "duration"
	StartTime        string `json:"start_time,omitempty"`     // "09:00"
	EndTime          string `json:"end_time,omitempty"`       // "18:00"
	DurationHours    int    `json:"duration_hours,omitempty"` // 持续运行时长(小时)
	StartedAt        string `json:"started_at,omitempty"`     // 启动时间 (RFC3339)
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

	// 启动令牌活跃度异步批量刷新器
	go s.activityFlusher()

	return s, nil
}

func (s *Store) activityFlusher() {
	defer close(s.flusherDone)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	dirty := make(map[string]struct{})

	flush := func() {
		if len(dirty) == 0 {
			return
		}
		now := time.Now().Format(time.RFC3339)
		tx, err := s.db.Begin()
		if err != nil {
			return
		}
		defer tx.Rollback()
		stmt, err := tx.Prepare(`UPDATE api_tokens SET last_used_at = ? WHERE id = ?`)
		if err == nil {
			for id := range dirty {
				_, _ = stmt.Exec(now, id)
			}
			_ = stmt.Close()
			if err := tx.Commit(); err == nil {
				dirty = make(map[string]struct{})
			}
		}
	}

	for {
		select {
		case <-s.stopCh:
			flush()
			return
		case id := <-s.activityCh:
			dirty[id] = struct{}{}
			if len(dirty) >= 100 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
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

	ddl := `
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
		last_used_at TEXT,
		scopes TEXT NOT NULL DEFAULT 'admin'
	);

	CREATE TABLE IF NOT EXISTS lease_records (
		id TEXT PRIMARY KEY,
		email TEXT NOT NULL,
		account_id TEXT NOT NULL,
		tag TEXT NOT NULL,
		status TEXT NOT NULL,
		allocated_at TEXT NOT NULL,
		completed_at TEXT,
		token_name TEXT DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_leases_allocated_at ON lease_records (allocated_at DESC);
	CREATE INDEX IF NOT EXISTS idx_leases_email ON lease_records (email);
	-- 表达式索引: 兼容历史混合大小写行的 LOWER(email) 等值反查(FindLeaseAccount)
	CREATE INDEX IF NOT EXISTS idx_leases_email_lower ON lease_records (LOWER(email));
	-- 覆盖复合索引: 支撑 ClaimPoolAlias 与 CountConsumedPoolAliases 的纯索引只读扫描 (Index-Only Scan)，彻底消除回表
	CREATE INDEX IF NOT EXISTS idx_leases_email_lower_token ON lease_records (LOWER(email), token_name);
	CREATE INDEX IF NOT EXISTS idx_leases_tag ON lease_records (tag);
	CREATE INDEX IF NOT EXISTS idx_leases_status ON lease_records (status);

	CREATE TABLE IF NOT EXISTS schedules (
		account_id TEXT PRIMARY KEY,
		enabled INTEGER NOT NULL DEFAULT 0,
		hourly_quota INTEGER NOT NULL DEFAULT 5,
		alias_label TEXT DEFAULT 'scheduled',
		current_hour_count INTEGER NOT NULL DEFAULT 0,
		last_hour_window INTEGER NOT NULL DEFAULT 0,
		last_run_at TEXT,
		mode TEXT DEFAULT 'always',
		start_time TEXT DEFAULT '',
		end_time TEXT DEFAULT '',
		duration_hours INTEGER DEFAULT 0,
		started_at TEXT DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	-- alias_routes: 「别名邮箱 → 母号」路由表。
	--
	-- 存在的意义: 取验证码时必须先知道某个别名属于哪个母号，否则只能遍历全部账号
	-- 逐个向上游 IMAP 拉取(2000 账号 = 每轮 2000 次请求，必然触发 Apple 风控)。
	-- 该表让归属解析变成一次主键点查，且跨进程重启依然完整。
	-- email 写入前一律归一为小写，主键即索引。
	CREATE TABLE IF NOT EXISTS alias_routes (
		email      TEXT PRIMARY KEY,
		account_id TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_alias_routes_account ON alias_routes (account_id);

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
	CREATE INDEX IF NOT EXISTS idx_accounts_status ON accounts(status);
	`
	if _, err := s.db.Exec(ddl); err != nil {
		return err
	}
	_, _ = s.db.Exec(`ALTER TABLE lease_records ADD COLUMN token_name TEXT DEFAULT ''`)
	// 升级前的历史令牌补 'admin' 作用域，保证既有外部接入不被收窄打断
	_, _ = s.db.Exec(`ALTER TABLE api_tokens ADD COLUMN scopes TEXT NOT NULL DEFAULT 'admin'`)
	_, _ = s.db.Exec(`ALTER TABLE schedules ADD COLUMN alias_label TEXT DEFAULT 'scheduled'`)
	_, _ = s.db.Exec(`ALTER TABLE schedules ADD COLUMN mode TEXT DEFAULT 'always'`)
	_, _ = s.db.Exec(`ALTER TABLE schedules ADD COLUMN start_time TEXT DEFAULT ''`)
	_, _ = s.db.Exec(`ALTER TABLE schedules ADD COLUMN end_time TEXT DEFAULT ''`)
	_, _ = s.db.Exec(`ALTER TABLE schedules ADD COLUMN duration_hours INTEGER DEFAULT 0`)
	_, _ = s.db.Exec(`ALTER TABLE schedules ADD COLUMN started_at TEXT DEFAULT ''`)
	return nil
}

// ────────────────────────────────────────────────────────────────
// Business Tags
// ────────────────────────────────────────────────────────────────
func (s *Store) ListTags() []BusinessTag {
	rows, err := s.db.Query(`SELECT id, name, tag, description, status, created_at, COALESCE(last_assigned_at, '') FROM business_tags ORDER BY created_at DESC`)
	if err != nil {
		return []BusinessTag{}
	}
	defer rows.Close()

	res := make([]BusinessTag, 0)
	for rows.Next() {
		var t BusinessTag
		if err := rows.Scan(&t.ID, &t.Name, &t.Tag, &t.Description, &t.Status, &t.CreatedAt, &t.LastAssignedAt); err == nil {
			res = append(res, t)
		}
	}
	return res
}

func (s *Store) SaveTag(tag BusinessTag) error {
	if tag.ID == "" {
		tag.ID = NewBusinessTagID()
	}
	if tag.CreatedAt == "" {
		tag.CreatedAt = time.Now().Format(time.RFC3339)
	}
	if tag.Status == "" {
		tag.Status = "active"
	}
	query := `
	INSERT INTO business_tags (id, name, tag, description, status, created_at, last_assigned_at)
	VALUES (?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		name = excluded.name,
		tag = excluded.tag,
		description = excluded.description,
		status = excluded.status,
		last_assigned_at = CASE WHEN excluded.last_assigned_at != '' THEN excluded.last_assigned_at ELSE business_tags.last_assigned_at END;
	`
	_, err := s.db.Exec(query, tag.ID, tag.Name, tag.Tag, tag.Description, tag.Status, tag.CreatedAt, tag.LastAssignedAt)
	return err
}

func (s *Store) DeleteTag(id string) error {
	_, err := s.db.Exec(`DELETE FROM business_tags WHERE id = ?`, id)
	return err
}

func (s *Store) UpdateTagLastAssigned(tagStr string) {
	now := time.Now().Format(time.RFC3339)
	_, _ = s.db.Exec(`UPDATE business_tags SET last_assigned_at = ? WHERE LOWER(tag) = LOWER(?)`, now, tagStr)
}

// --- 外部令牌 Tokens ---

func (s *Store) ListTokens() []APIToken {
	rows, err := s.db.Query(`SELECT id, name, token, created_at, COALESCE(last_used_at, ''), COALESCE(scopes, '') FROM api_tokens ORDER BY created_at DESC`)
	if err != nil {
		return []APIToken{}
	}
	defer rows.Close()

	res := make([]APIToken, 0)
	for rows.Next() {
		var tok APIToken
		if err := rows.Scan(&tok.ID, &tok.Name, &tok.Token, &tok.CreatedAt, &tok.LastUsedAt, &tok.Scopes); err == nil {
			res = append(res, tok)
		}
	}
	return res
}

// ListTokensMasked 返回令牌列表，但把令牌本体替换为前缀掩码，避免管理台截图/日志外泄可用凭据。
func (s *Store) ListTokensMasked() []APIToken {
	tokens := s.ListTokens()
	for i := range tokens {
		tokens[i].Token = MaskToken(tokens[i].Token)
	}
	return tokens
}

// MaskToken 只保留令牌的前 7 位(如 am_1a2b)与长度提示，其余打码。
func MaskToken(token string) string {
	if len(token) <= 7 {
		return "****"
	}
	return token[:7] + "****" + fmt.Sprintf("(%d位)", len(token))
}

func (s *Store) SaveToken(token APIToken) error {
	if token.ID == "" {
		token.ID = NewAPITokenID()
	}
	if token.CreatedAt == "" {
		token.CreatedAt = time.Now().Format(time.RFC3339)
	}
	query := `
	INSERT INTO api_tokens (id, name, token, created_at, last_used_at, scopes)
	VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		name = excluded.name,
		token = excluded.token,
		scopes = excluded.scopes,
		last_used_at = CASE WHEN excluded.last_used_at != '' THEN excluded.last_used_at ELSE api_tokens.last_used_at END;
	`
	scopes := strings.TrimSpace(token.Scopes)
	if scopes == "" {
		scopes = ScopeAdmin
	}
	_, err := s.db.Exec(query, token.ID, token.Name, token.Token, token.CreatedAt, token.LastUsedAt, scopes)
	return err
}

func (s *Store) DeleteToken(id string) error {
	_, err := s.db.Exec(`DELETE FROM api_tokens WHERE id = ?`, id)
	return err
}

func (s *Store) ValidateToken(tokenStr string) bool {
	_, _, ok := s.ValidateTokenWithName(tokenStr)
	return ok
}

// ValidateTokenWithName 校验令牌并返回名称与作用域集合。
func (s *Store) ValidateTokenWithName(tokenStr string) (name string, scopes string, ok bool) {
	var id string
	err := s.db.QueryRow(`SELECT id, name, COALESCE(scopes, '') FROM api_tokens WHERE token = ?`, tokenStr).Scan(&id, &name, &scopes)
	if err != nil {
		return "", "", false
	}
	// 异步轻量更新最后使用时间，进入批处理缓冲通道，零锁争用
	select {
	case s.activityCh <- id:
	default:
	}
	return name, scopes, true
}

// ────────────────────────────────────────────────────────────────
// 别名路由 (alias_routes)
// ────────────────────────────────────────────────────────────────

// UpsertAliasRoutes 批量登记「别名邮箱 → 母号」归属(单事务)。
//
// 这是取验证码链路的关键索引:没有它就只能遍历全部账号去上游盲找。凡是确知归属的
// 时刻(出号成功、拉取到账号别名列表)都应调用本方法，写入即归一为小写。
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

// deleteAliasRoutesForAccount 清理某账号的全部路由(账号注销时级联)。
func (s *Store) deleteAliasRoutesForAccount(accountID string) error {
	_, err := s.db.Exec(`DELETE FROM alias_routes WHERE account_id = ?`, accountID)
	return err
}

// settingKeyAliasRoutesBackfilled 标记路由表是否已从流水回填过，避免每次启动都全表扫流水。
const settingKeyAliasRoutesBackfilled = "alias_routes_backfilled"

// backfillAliasRoutes 用出号流水回填别名路由。
//
// 单条 INSERT...SELECT 完成(不经过 Go 循环)。用 setting 标记保证常规情况下只跑一次:
// 流水表在几千账号下可达千万行，每次启动都全表扫是不可接受的。
//
// 额外加一道自愈判断 —— 只要路由表还是空的就重跑一次。这样即使标记被提前写入
// (例如空库首启)或路由表被清空，下一次启动也能自动补回来，避免"有流水却无路由"。
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
//
// 保留为路由表的兜底来源(回填尚未覆盖的历史数据)。
func (s *Store) FindLeaseAccount(email string) (string, bool) {
	email = normalizeEmail(email)
	if email == "" {
		return "", false
	}
	var accountID string
	// 用 LOWER(email) 而非裸列，以兼容新写入前遗留的混合大小写历史行；
	// 对应的表达式索引见 initSchema 的 idx_leases_email_lower，避免退化成全表扫。
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
//
// 只清流水，**绝不动 alias_routes** —— 路由表是取码长轮询的必需索引，
// 与流水的保留期无关，必须长期保留。
//
// 分批(LIMIT)是为了避免一次删除数百万行时长时间持有写锁。
// 注意 allocated_at 以 RFC3339 文本存储且按字符串比较，索引 idx_leases_allocated_at
// 可被命中；同一部署内时区偏移恒定，故字典序与时间序一致。
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

// --- 已用别名流水 Leases ---

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

	// 1. 获取总数
	var total int
	countQuery := "SELECT COUNT(*) FROM lease_records WHERE " + whereSQL
	_ = s.db.QueryRow(countQuery, args...).Scan(&total)

	if total == 0 || offset >= total {
		return []LeaseRecord{}, total
	}

	// 2. 分页获取数据
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
	return records, total
}

func (s *Store) RecordLease(rec LeaseRecord) error {
	if rec.ID == "" {
		rec.ID = fmt.Sprintf("lease_%d_%d", time.Now().UnixNano(), atomic.AddUint64(&leaseSeq, 1))
	}
	if rec.AllocatedAt == "" {
		rec.AllocatedAt = time.Now().Format(time.RFC3339)
	}
	if rec.Status == "" {
		rec.Status = "completed"
	}
	// 邮箱一律归一为小写:别名邮箱的大小写并不由我们保证(Apple 返回值、
	// 历史数据、外部导入都可能含大写)，而 FindLeaseAccount/邮件路由依赖它做等值匹配。
	rec.Email = normalizeEmail(rec.Email)
	query := `INSERT INTO lease_records (id, email, account_id, tag, status, allocated_at, completed_at, token_name) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := s.db.Exec(query, rec.ID, rec.Email, rec.AccountID, rec.Tag, rec.Status, rec.AllocatedAt, rec.CompletedAt, rec.TokenName); err != nil {
		return err
	}
	// 出号是"确知归属"的时刻:顺带把路由表写穿，取码时即可主键点查。
	// 路由写失败不影响出号结果(回填与列表拉取还有两条自愈路径)。
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

// PoolCandidate 待从别名池领用的候选别名
type PoolCandidate struct {
	AccountID string
	Email     string
}

// ClaimPoolAlias 原子地从候选别名列表中挑选第一个未被外部消费者领用的别名并生成领用记录。
// 并发安全：多协程并发领号时，同一别名绝不会被重复分发。
// 若无可用别名，返回 nil, nil。
func (s *Store) ClaimPoolAlias(candidates []PoolCandidate, tag, tokenName string) (*LeaseRecord, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	if tag == "" {
		tag = "default"
	}
	if tokenName == "" {
		tokenName = "admin_console"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

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
			return nil, err
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
			if norm == "" || consumed[norm] {
				continue
			}

			// 命中首个可用别名，原子插入领用流水！
			rec := LeaseRecord{
				ID:          fmt.Sprintf("lease_%d_%d", time.Now().UnixNano(), atomic.AddUint64(&leaseSeq, 1)),
				Email:       norm,
				AccountID:   c.AccountID,
				Tag:         tag,
				Status:      "completed",
				AllocatedAt: time.Now().Format(time.RFC3339),
				CompletedAt: time.Now().Format(time.RFC3339),
				TokenName:   tokenName,
			}
			insQuery := `INSERT INTO lease_records (id, email, account_id, tag, status, allocated_at, completed_at, token_name) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
			if _, insertErr := s.db.Exec(insQuery, rec.ID, rec.Email, rec.AccountID, rec.Tag, rec.Status, rec.AllocatedAt, rec.CompletedAt, rec.TokenName); insertErr != nil {
				return nil, insertErr
			}
			if rec.AccountID != "" {
				_ = s.UpsertAliasRoutes(rec.AccountID, []string{rec.Email})
			}
			s.UpdateTagLastAssigned(tag)
			return &rec, nil
		}
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
// 得益于 idx_leases_email_lower_token 覆盖复合索引，该查询由 SQLite 纯索引只读引擎在亚毫秒内完成。
func (s *Store) CountConsumedPoolAliases() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	var n int
	_ = s.db.QueryRow(`SELECT COUNT(DISTINCT LOWER(email)) FROM lease_records WHERE COALESCE(token_name, '') != 'scheduler'`).Scan(&n)
	return n
}

// --- 定时配置 Schedules ---

func (s *Store) GetScheduleConfig(accountID string) ScheduleConfig {
	var cfg ScheduleConfig
	var enabledInt int
	query := `SELECT account_id, enabled, hourly_quota, COALESCE(alias_label, 'scheduled'), current_hour_count, last_hour_window, COALESCE(last_run_at, ''), COALESCE(mode, 'always'), COALESCE(start_time, ''), COALESCE(end_time, ''), COALESCE(duration_hours, 0), COALESCE(started_at, '') FROM schedules WHERE account_id = ?`
	err := s.db.QueryRow(query, accountID).Scan(&cfg.AccountID, &enabledInt, &cfg.HourlyQuota, &cfg.AliasLabel, &cfg.CurrentHourCount, &cfg.LastHourWindow, &cfg.LastRunAt, &cfg.Mode, &cfg.StartTime, &cfg.EndTime, &cfg.DurationHours, &cfg.StartedAt)
	if err != nil {
		return ScheduleConfig{
			AccountID:   accountID,
			Enabled:     false,
			HourlyQuota: 5,
			AliasLabel:  "scheduled",
			Mode:        "always",
		}
	}
	cfg.Enabled = (enabledInt == 1)
	if strings.TrimSpace(cfg.AliasLabel) == "" {
		cfg.AliasLabel = "scheduled"
	}
	if strings.TrimSpace(cfg.Mode) == "" {
		cfg.Mode = "always"
	}
	currentHour := time.Now().Unix() / 3600
	if cfg.LastHourWindow != currentHour {
		cfg.CurrentHourCount = 0
	}
	return cfg
}

func (s *Store) ListScheduleConfigs() []ScheduleConfig {
	rows, err := s.db.Query(`SELECT account_id, enabled, hourly_quota, COALESCE(alias_label, 'scheduled'), current_hour_count, last_hour_window, COALESCE(last_run_at, ''), COALESCE(mode, 'always'), COALESCE(start_time, ''), COALESCE(end_time, ''), COALESCE(duration_hours, 0), COALESCE(started_at, '') FROM schedules`)
	if err != nil {
		return []ScheduleConfig{}
	}
	defer rows.Close()

	currentHour := time.Now().Unix() / 3600
	res := make([]ScheduleConfig, 0)
	for rows.Next() {
		var cfg ScheduleConfig
		var enabledInt int
		if err := rows.Scan(&cfg.AccountID, &enabledInt, &cfg.HourlyQuota, &cfg.AliasLabel, &cfg.CurrentHourCount, &cfg.LastHourWindow, &cfg.LastRunAt, &cfg.Mode, &cfg.StartTime, &cfg.EndTime, &cfg.DurationHours, &cfg.StartedAt); err == nil {
			cfg.Enabled = (enabledInt == 1)
			if strings.TrimSpace(cfg.AliasLabel) == "" {
				cfg.AliasLabel = "scheduled"
			}
			if strings.TrimSpace(cfg.Mode) == "" {
				cfg.Mode = "always"
			}
			if cfg.LastHourWindow != currentHour {
				cfg.CurrentHourCount = 0
			}
			res = append(res, cfg)
		}
	}
	return res
}

// SaveScheduleConfig 保存调度配置(线程安全)。
// 与 TryReserveQuota 共用同一把锁，避免"读-改-写"把已扣减的小时计数回退成旧值。
func (s *Store) SaveScheduleConfig(cfg ScheduleConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveScheduleConfigLocked(cfg)
}

// saveScheduleConfigLocked 是 SaveScheduleConfig 的加锁内核，调用方必须持有 s.mu。
func (s *Store) saveScheduleConfigLocked(cfg ScheduleConfig) error {
	if cfg.HourlyQuota <= 0 {
		cfg.HourlyQuota = 5
	}
	if strings.TrimSpace(cfg.AliasLabel) == "" {
		cfg.AliasLabel = "scheduled"
	}
	if strings.TrimSpace(cfg.Mode) == "" {
		cfg.Mode = "always"
	}
	if cfg.Mode == "duration" && cfg.Enabled && strings.TrimSpace(cfg.StartedAt) == "" {
		cfg.StartedAt = time.Now().Format(time.RFC3339)
	}
	enabledInt := 0
	if cfg.Enabled {
		enabledInt = 1
	}
	query := `
	INSERT INTO schedules (account_id, enabled, hourly_quota, alias_label, current_hour_count, last_hour_window, last_run_at, mode, start_time, end_time, duration_hours, started_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(account_id) DO UPDATE SET
		enabled = excluded.enabled,
		hourly_quota = excluded.hourly_quota,
		alias_label = excluded.alias_label,
		current_hour_count = CASE WHEN excluded.last_hour_window > 0 THEN excluded.current_hour_count ELSE schedules.current_hour_count END,
		last_hour_window = CASE WHEN excluded.last_hour_window > 0 THEN excluded.last_hour_window ELSE schedules.last_hour_window END,
		last_run_at = CASE WHEN excluded.last_run_at != '' THEN excluded.last_run_at ELSE schedules.last_run_at END,
		mode = excluded.mode,
		start_time = excluded.start_time,
		end_time = excluded.end_time,
		duration_hours = excluded.duration_hours,
		started_at = CASE WHEN excluded.mode != 'duration' THEN '' WHEN excluded.started_at != '' THEN excluded.started_at ELSE schedules.started_at END;
	`
	_, err := s.db.Exec(query, cfg.AccountID, enabledInt, cfg.HourlyQuota, cfg.AliasLabel, cfg.CurrentHourCount, cfg.LastHourWindow, cfg.LastRunAt, cfg.Mode, cfg.StartTime, cfg.EndTime, cfg.DurationHours, cfg.StartedAt)
	return err
}

// DeleteScheduleConfig 物理删除指定账号的定时调度配置，杜绝幽灵记录残留。
func (s *Store) DeleteScheduleConfig(accountID string) error {
	_, err := s.db.Exec(`DELETE FROM schedules WHERE account_id = ?`, accountID)
	return err
}

// TryReserveQuota 尝试原子预留指定数量的配额（单次/批量/调度统一入口）
func (s *Store) TryReserveQuota(accountID string, count int) (allowed bool, remaining int) {
	if count <= 0 {
		return true, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg := s.GetScheduleConfig(accountID)
	currentHour := time.Now().Unix() / 3600
	if cfg.LastHourWindow != currentHour {
		cfg.LastHourWindow = currentHour
		cfg.CurrentHourCount = 0
	}

	rem := cfg.HourlyQuota - cfg.CurrentHourCount
	if rem < count {
		if rem < 0 {
			rem = 0
		}
		return false, rem
	}

	cfg.CurrentHourCount += count
	cfg.LastRunAt = time.Now().Format(time.RFC3339)
	if err := s.saveScheduleConfigLocked(cfg); err != nil {
		// 落库失败则回滚内存计数，拒绝本次预留，避免重启后配额归零超额出号
		cfg.CurrentHourCount -= count
		return false, cfg.HourlyQuota - cfg.CurrentHourCount
	}
	return true, cfg.HourlyQuota - cfg.CurrentHourCount
}

// ReleaseQuota 当别名创建失败时回滚配额
func (s *Store) ReleaseQuota(accountID string, count int) {
	if count <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg := s.GetScheduleConfig(accountID)
	currentHour := time.Now().Unix() / 3600
	if cfg.LastHourWindow == currentHour {
		cfg.CurrentHourCount -= count
		if cfg.CurrentHourCount < 0 {
			cfg.CurrentHourCount = 0
		}
		_ = s.saveScheduleConfigLocked(cfg)
	}
}

// RemainingQuota 查询指定账号当前小时的剩余创建配额
func (s *Store) RemainingQuota(accountID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg := s.GetScheduleConfig(accountID)
	currentHour := time.Now().Unix() / 3600
	if cfg.LastHourWindow != currentHour {
		return cfg.HourlyQuota
	}
	rem := cfg.HourlyQuota - cfg.CurrentHourCount
	if rem < 0 {
		return 0
	}
	return rem
}

func (s *Store) IncrementHourlyQuota(accountID string) (allowed bool, current int) {
	ok, _ := s.TryReserveQuota(accountID, 1)
	cfg := s.GetScheduleConfig(accountID)
	return ok, cfg.CurrentHourCount
}

// GetSetting 读取系统级 KV 设置,不存在返回空串。
func (s *Store) GetSetting(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var value string
	_ = s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	return value
}

// SaveSetting 写入系统级 KV 设置 (upsert)。
func (s *Store) SaveSetting(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().Format(time.RFC3339),
	)
	return err
}
