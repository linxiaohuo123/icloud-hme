/**
 * [INPUT]: 依赖 crypto/hmac, crypto/sha256, golang.org/x/crypto/argon2
 * [OUTPUT]: 对外提供 Manager, NewManager, Session, Options 等会话管理与 CSRF 防御能力
 * [POS]: internal/auth 的管理员认证与会话签名核心
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

// Package auth 实现管理员密码校验、内存会话、CSRF 与登录限流。
//
// 不依赖 Gin:HTTP 中间件只负责 Cookie/Header 与 HTTP 状态映射。
// 会话采用基于管理员密码派生密钥的 HMAC 签名，既不落盘写日志，又能安全跨越服务端重启。
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	// DefaultTTL 是默认会话有效期。
	DefaultTTL = 12 * time.Hour
	// maxSessions 是同时有效会话上限,超出时淘汰最早过期的会话。
	maxSessions = 32
)

// Options 是 Manager 的构造选项。
type Options struct {
	Password string
	TTL      time.Duration
	Now      func() time.Time
	Random   io.Reader
}

// Session 是一次管理员会话的公开信息。
type Session struct {
	CSRFToken string
	ExpiresAt time.Time
}

// sessionRecord 是内存会话记录,map key 为 session ID 的 SHA-256。
type sessionRecord struct {
	csrfHash  [32]byte
	csrfToken string
	expiresAt time.Time
}

// Manager 管理管理员会话,线程安全。
type Manager struct {
	mu          sync.Mutex
	salt        []byte
	password    []byte
	signKey     []byte
	ttl         time.Duration
	now         func() time.Time
	random      io.Reader
	sessions    map[[32]byte]sessionRecord
	blacklisted map[[32]byte]time.Time
}

// NewManager 创建会话管理器。
//
// 拒绝空密码与短于 8 字符的密码;启动时用随机 salt + argon2.IDKey
// 派生密码,登录时使用常量时间比较。
func NewManager(opts Options) (*Manager, error) {
	if len(opts.Password) < 8 {
		return nil, errors.New("管理员密码长度不能少于 8 个字符")
	}
	if opts.TTL <= 0 {
		opts.TTL = DefaultTTL
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Random == nil {
		opts.Random = rand.Reader
	}

	salt := make([]byte, 16)
	if _, err := io.ReadFull(opts.Random, salt); err != nil {
		return nil, fmt.Errorf("生成密码 salt 失败: %w", err)
	}
	// argon2id: time=1, memory=64*1024 KiB, threads=4, keyLen=32
	derived := argon2.IDKey([]byte(opts.Password), salt, 1, 64*1024, 4, 32)

	signKeyHash := sha256.Sum256([]byte("icloud-hme-session-sign-key:" + opts.Password))
	signKey := signKeyHash[:]

	return &Manager{
		salt:        salt,
		password:    derived,
		signKey:     signKey,
		ttl:         opts.TTL,
		now:         opts.Now,
		random:      opts.Random,
		sessions:    make(map[[32]byte]sessionRecord),
		blacklisted: make(map[[32]byte]time.Time),
	}, nil
}

// Login 校验管理员密码;成功时创建会话并返回 session ID。
func (m *Manager) Login(password string) (sessionID string, session Session, ok bool) {
	derived := argon2.IDKey([]byte(password), m.salt, 1, 64*1024, 4, 32)
	if subtle.ConstantTimeCompare(derived, m.password) != 1 {
		return "", Session{}, false
	}

	idBytes := make([]byte, 24)
	if _, err := io.ReadFull(m.random, idBytes); err != nil {
		return "", Session{}, false
	}
	expiresAt := m.now().Add(m.ttl)
	expStr := strconv.FormatInt(expiresAt.Unix(), 10)
	rawPayload := base64.RawURLEncoding.EncodeToString(idBytes) + "." + expStr
	mac := hmac.New(sha256.New, m.signKey)
	mac.Write([]byte(rawPayload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	sessionID = rawPayload + "." + sig
	token := deriveCSRFToken(m.signKey, sessionID)

	key := sha256.Sum256([]byte(sessionID))
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked()
	// 会话数达到上限时淘汰最早过期的会话,防止内存无界增长
	if len(m.sessions) >= maxSessions {
		m.evictOldestLocked()
	}
	if _, exists := m.sessions[key]; exists {
		return "", Session{}, false
	}
	m.sessions[key] = sessionRecord{
		csrfHash:  sha256.Sum256([]byte(token)),
		csrfToken: token,
		expiresAt: expiresAt,
	}

	return sessionID, Session{CSRFToken: token, ExpiresAt: expiresAt}, true
}

// Validate 校验会话是否有效;支持服务重启后的签名自愈。
func (m *Manager) Validate(sessionID string) (Session, bool) {
	key := sha256.Sum256([]byte(sessionID))
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked()

	// 1. 检查登出黑名单
	if exp, black := m.blacklisted[key]; black {
		if m.now().Before(exp) {
			return Session{}, false
		}
		delete(m.blacklisted, key)
	}

	// 2. 内存命中
	rec, exists := m.sessions[key]
	if exists {
		return Session{CSRFToken: rec.csrfToken, ExpiresAt: rec.expiresAt}, true
	}

	// 3. 服务端重启自愈：校验密码派生密钥的 HMAC 签名
	parts := strings.Split(sessionID, ".")
	if len(parts) != 3 {
		return Session{}, false
	}
	expUnix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return Session{}, false
	}
	expiresAt := time.Unix(expUnix, 0)
	if !m.now().Before(expiresAt) {
		return Session{}, false
	}

	rawPayload := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, m.signKey)
	mac.Write([]byte(rawPayload))
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(parts[2]), []byte(expectedSig)) != 1 {
		return Session{}, false
	}

	// 签名合法，派生确定性 CSRF Token 并写回内存缓存
	csrfToken := deriveCSRFToken(m.signKey, sessionID)

	m.sessions[key] = sessionRecord{
		csrfHash:  sha256.Sum256([]byte(csrfToken)),
		csrfToken: csrfToken,
		expiresAt: expiresAt,
	}
	return Session{CSRFToken: csrfToken, ExpiresAt: expiresAt}, true
}

func deriveCSRFToken(signKey []byte, sessionID string) string {
	csrfMac := hmac.New(sha256.New, signKey)
	csrfMac.Write([]byte("csrf:" + sessionID))
	return base64.RawURLEncoding.EncodeToString(csrfMac.Sum(nil))
}

// ValidateCSRF 校验 CSRF token(常量时间比较)。
func (m *Manager) ValidateCSRF(sessionID, token string) bool {
	key := sha256.Sum256([]byte(sessionID))
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked()
	rec, exists := m.sessions[key]
	if !exists {
		return false
	}
	want := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(rec.csrfHash[:], want[:]) == 1
}

// Logout 删除会话。
func (m *Manager) Logout(sessionID string) {
	key := sha256.Sum256([]byte(sessionID))
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec, exists := m.sessions[key]; exists {
		m.blacklisted[key] = rec.expiresAt
		delete(m.sessions, key)
	} else {
		m.blacklisted[key] = m.now().Add(m.ttl)
	}
}

// pruneLocked 清理所有过期会话,须持锁调用。
func (m *Manager) pruneLocked() {
	now := m.now()
	for key, rec := range m.sessions {
		if !now.Before(rec.expiresAt) {
			delete(m.sessions, key)
		}
	}
	for key, exp := range m.blacklisted {
		if !now.Before(exp) {
			delete(m.blacklisted, key)
		}
	}
}

// evictOldestLocked 淘汰最早过期的会话,须持锁调用。
func (m *Manager) evictOldestLocked() {
	oldest := time.Time{}
	var oldestKey [32]byte
	for key, rec := range m.sessions {
		if oldest.IsZero() || rec.expiresAt.Before(oldest) {
			oldest = rec.expiresAt
			oldestKey = key
		}
	}
	if !oldest.IsZero() {
		m.blacklisted[oldestKey] = oldest
		delete(m.sessions, oldestKey)
	}
}
