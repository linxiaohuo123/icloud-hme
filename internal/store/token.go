/**
 * [INPUT]: 依赖 context, database/sql, errors, fmt, log, strings, time, icloud-hme/internal/store (Store, newOpaqueID)
 * [OUTPUT]: 对外提供 APIToken 类型, Scope 常量, HasScope, NewAPITokenID, ListTokens, ListTokensMasked, MaskToken, SaveToken, DeleteToken, ValidateToken, ValidateTokenWithName, ValidateTokenPrincipal, GetToken
 * [POS]: internal/store 的外部 API 令牌鉴权与持久化领域，支持作用域隔离与异步刷盘
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

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

// NewAPITokenID 生成外部令牌主键。
func NewAPITokenID() string { return newOpaqueID("tok_") }

// activityFlusher 定期把内存中暂存的令牌最后活跃时间批量刷入 SQLite。
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
	// 【BUG-05 修复】迭代中断时记录日志
	if err := rows.Err(); err != nil {
		log.Printf("[Store] ListTokens 迭代中断: %v", err)
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

// DeleteToken 删除 API 令牌。返回 true 表示实际删除了记录，false 表示本就不存在。
func (s *Store) DeleteToken(id string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM api_tokens WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
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

// ValidateTokenPrincipal 校验令牌并返回 ID、名称与作用域集合 (PR-04)。
func (s *Store) ValidateTokenPrincipal(tokenStr string) (id, name, scopes string, ok bool) {
	err := s.db.QueryRow(`SELECT id, name, COALESCE(scopes, '') FROM api_tokens WHERE token = ?`, tokenStr).Scan(&id, &name, &scopes)
	if err != nil {
		return "", "", "", false
	}
	select {
	case s.activityCh <- id:
	default:
	}
	return id, name, scopes, true
}

// GetToken 按 ID 查询 API 令牌 (未被撤销则返回) (PR-06 V09)。
func (s *Store) GetToken(ctx context.Context, id string) (*APIToken, error) {
	id = strings.TrimSpace(id)
	var tok APIToken
	err := s.db.QueryRowContext(ctx, `SELECT id, name, token, created_at, COALESCE(last_used_at, ''), COALESCE(scopes, '') FROM api_tokens WHERE id = ?`, id).Scan(
		&tok.ID, &tok.Name, &tok.Token, &tok.CreatedAt, &tok.LastUsedAt, &tok.Scopes,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("token not found")
		}
		return nil, err
	}
	return &tok, nil
}
