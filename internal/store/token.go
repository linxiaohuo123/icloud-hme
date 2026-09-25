/**
 * [INPUT]: 依赖 context, crypto/rand, crypto/sha256, database/sql, encoding/base64, encoding/hex, errors, fmt, log, strings, time, icloud-hme/internal/store (Store, newOpaqueID)
 * [OUTPUT]: 对外提供 APIToken, CreatedToken, Scope 常量, HasScope, NewAPITokenID, GenerateSecureToken, HashToken, SafeTokenPrefix, ListTokens, ListTokensMasked, SaveToken, CreateToken, RotateToken, DeleteToken, ValidateToken, ValidateTokenWithName, ValidateTokenPrincipal, GetToken
 * [POS]: internal/store 的外部 API 令牌安全领域 (PR-07: 不可逆哈希存储、生命周期管理与 CSPRNG 令牌生成)
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"
)

// APIToken 外部接入长效令牌安全记录 (不含明文令牌)
type APIToken struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Token         string `json:"token,omitempty"` // 仅返回脱敏掩码，数据库中无此列
	TokenPrefix   string `json:"token_prefix"`
	TokenHash     string `json:"-"`
	CreatedAt     string `json:"created_at"`
	LastUsedAt    string `json:"last_used_at,omitempty"`
	Scopes        string `json:"scopes,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	RevokedAt     string `json:"revoked_at,omitempty"`
	RotatedAt     string `json:"rotated_at,omitempty"`
	NeedsRotation bool   `json:"needs_rotation"`
}

// CreatedToken 令牌创建或轮换时的唯一可见响应载体
type CreatedToken struct {
	APIToken
	Token string `json:"token"`
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
	want = strings.ToLower(strings.TrimSpace(want))
	for _, s := range strings.Split(scopes, ",") {
		part := strings.ToLower(strings.TrimSpace(s))
		if part == want || part == ScopeAdmin {
			return true
		}
	}
	return false
}

// NewAPITokenID 生成外部令牌主键。
func NewAPITokenID() string { return newOpaqueID("tok_") }

// GenerateSecureToken 生成具有 256-bit 密码学强度的安全令牌 (am_<base64url>)
func GenerateSecureToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("生成安全随机令牌失败: %w", err)
	}
	return "am_" + base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashToken 计算令牌的不可逆 SHA-256 哈希值
func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// SafeTokenPrefix 提取令牌的安全前缀 (用于管理面审计与识别)
func SafeTokenPrefix(token string) string {
	token = strings.TrimSpace(token)
	if strings.HasPrefix(token, "am_") {
		if len(token) >= 7 {
			return token[:7]
		}
		return token
	}
	if len(token) > 7 {
		return token[:7]
	}
	return token
}

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

// ListTokens 查询全部令牌记录 (不返回明文 Token 及 TokenHash)
func (s *Store) ListTokens() []APIToken {
	rows, err := s.db.Query(`
		SELECT id, name, token_prefix, created_at, COALESCE(last_used_at, ''),
		       COALESCE(scopes, ''), COALESCE(expires_at, ''), COALESCE(revoked_at, ''),
		       COALESCE(rotated_at, ''), needs_rotation
		FROM api_tokens
		ORDER BY created_at DESC
	`)
	if err != nil {
		return []APIToken{}
	}
	defer rows.Close()

	res := make([]APIToken, 0)
	for rows.Next() {
		var tok APIToken
		var needsRot int
		if err := rows.Scan(
			&tok.ID, &tok.Name, &tok.TokenPrefix, &tok.CreatedAt, &tok.LastUsedAt,
			&tok.Scopes, &tok.ExpiresAt, &tok.RevokedAt, &tok.RotatedAt, &needsRot,
		); err == nil {
			tok.NeedsRotation = (needsRot == 1)
			tok.Token = tok.TokenPrefix + "****"
			res = append(res, tok)
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("[Store] ListTokens 迭代中断: %v", err)
	}
	return res
}

// ListTokensMasked 返回令牌列表 (ListTokens 已保证只返回安全前缀)
func (s *Store) ListTokensMasked() []APIToken {
	return s.ListTokens()
}

// MaskToken 只保留令牌的前 7 位与长度提示，其余打码。
func MaskToken(token string) string {
	if len(token) <= 7 {
		return "****"
	}
	return token[:7] + "****" + fmt.Sprintf("(%d位)", len(token))
}

// SaveToken 插入或更新令牌记录 (兼容旧测试，自动根据明文 Token 计算不可逆 Hash)
func (s *Store) SaveToken(token APIToken) error {
	if token.ID == "" {
		token.ID = NewAPITokenID()
	}
	if token.CreatedAt == "" {
		token.CreatedAt = time.Now().Format(time.RFC3339)
	}

	tokenHash := ""
	tokenPrefix := token.TokenPrefix
	if token.Token != "" {
		tokenHash = HashToken(token.Token)
		if tokenPrefix == "" {
			tokenPrefix = SafeTokenPrefix(token.Token)
		}
	}

	scopes := strings.TrimSpace(token.Scopes)
	if scopes == "" {
		scopes = ScopeAdmin
	}

	needsRot := 0
	if token.NeedsRotation {
		needsRot = 1
	}

	query := `
	INSERT INTO api_tokens (id, name, token_hash, token_prefix, created_at, last_used_at, scopes, expires_at, revoked_at, rotated_at, needs_rotation)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		name = excluded.name,
		token_hash = CASE WHEN excluded.token_hash != '' THEN excluded.token_hash ELSE api_tokens.token_hash END,
		token_prefix = CASE WHEN excluded.token_prefix != '' THEN excluded.token_prefix ELSE api_tokens.token_prefix END,
		scopes = excluded.scopes,
		expires_at = excluded.expires_at,
		revoked_at = excluded.revoked_at,
		rotated_at = excluded.rotated_at,
		needs_rotation = excluded.needs_rotation,
		last_used_at = CASE WHEN excluded.last_used_at != '' THEN excluded.last_used_at ELSE api_tokens.last_used_at END;
	`
	_, err := s.db.Exec(query,
		token.ID, token.Name, tokenHash, tokenPrefix, token.CreatedAt, token.LastUsedAt,
		scopes, token.ExpiresAt, token.RevokedAt, token.RotatedAt, needsRot,
	)
	return err
}

// CreateToken 创建新的高熵 API 令牌，仅在返回值中一次性暴露完整明文令牌
func (s *Store) CreateToken(name, scopes, expiresAt string) (*CreatedToken, error) {
	expiresAt = strings.TrimSpace(expiresAt)
	if expiresAt != "" {
		if _, err := time.Parse(time.RFC3339, expiresAt); err != nil {
			return nil, fmt.Errorf("invalid expires_at format (must be RFC3339): %w", err)
		}
	}

	rawToken, err := GenerateSecureToken()
	if err != nil {
		return nil, err
	}

	tokID := NewAPITokenID()
	tokenHash := HashToken(rawToken)
	tokenPrefix := SafeTokenPrefix(rawToken)
	createdAt := time.Now().UTC().Format(time.RFC3339)

	scopes = strings.TrimSpace(scopes)
	if scopes == "" {
		scopes = DefaultExternalScopes
	}

	query := `
	INSERT INTO api_tokens (id, name, token_hash, token_prefix, created_at, scopes, expires_at, needs_rotation)
	VALUES (?, ?, ?, ?, ?, ?, ?, 0);
	`
	if _, err := s.db.Exec(query, tokID, name, tokenHash, tokenPrefix, createdAt, scopes, expiresAt); err != nil {
		return nil, fmt.Errorf("持久化新令牌失败: %w", err)
	}

	created := &CreatedToken{
		APIToken: APIToken{
			ID:            tokID,
			Name:          name,
			TokenPrefix:   tokenPrefix,
			CreatedAt:     createdAt,
			Scopes:        scopes,
			ExpiresAt:     expiresAt,
			NeedsRotation: false,
		},
		Token: rawToken,
	}
	return created, nil
}

// RotateToken 轮换指定令牌的主密钥，保持 ID 与历史归属不变，新令牌仅在返回值中一次性暴露
func (s *Store) RotateToken(id string) (*CreatedToken, error) {
	id = strings.TrimSpace(id)
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("开启轮换事务失败: %w", err)
	}
	defer tx.Rollback()

	var name, scopes, expiresAt, revokedAt string
	err = tx.QueryRow(`
		SELECT name, COALESCE(scopes, ''), COALESCE(expires_at, ''), COALESCE(revoked_at, '')
		FROM api_tokens WHERE id = ?
	`, id).Scan(&name, &scopes, &expiresAt, &revokedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("token not found")
		}
		return nil, err
	}

	if revokedAt != "" {
		return nil, errors.New("cannot rotate a revoked token")
	}

	if expiresAt != "" {
		if _, err := time.Parse(time.RFC3339, expiresAt); err != nil {
			return nil, fmt.Errorf("cannot rotate token with malformed expires_at: %w", err)
		}
	}

	newToken, err := GenerateSecureToken()
	if err != nil {
		return nil, err
	}

	newHash := HashToken(newToken)
	newPrefix := SafeTokenPrefix(newToken)
	rotatedAt := time.Now().UTC().Format(time.RFC3339)

	query := `
	UPDATE api_tokens
	SET token_hash = ?, token_prefix = ?, rotated_at = ?, needs_rotation = 0
	WHERE id = ?
	`
	if _, err := tx.Exec(query, newHash, newPrefix, rotatedAt, id); err != nil {
		return nil, fmt.Errorf("更新轮换令牌哈希失败: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交轮换事务失败: %w", err)
	}

	return &CreatedToken{
		APIToken: APIToken{
			ID:            id,
			Name:          name,
			TokenPrefix:   newPrefix,
			Scopes:        scopes,
			ExpiresAt:     expiresAt,
			RotatedAt:     rotatedAt,
			NeedsRotation: false,
		},
		Token: newToken,
	}, nil
}

// DeleteToken 软注销 (Soft Revoke) API 令牌。返回 true 表示令牌存在且已标记注销，false 表示记录不存在。
func (s *Store) DeleteToken(id string) (bool, error) {
	id = strings.TrimSpace(id)
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.Exec(`UPDATE api_tokens SET revoked_at = COALESCE(NULLIF(revoked_at, ''), ?) WHERE id = ?`, now, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ValidateToken 检查令牌有效性
func (s *Store) ValidateToken(tokenStr string) bool {
	_, _, ok := s.ValidateTokenWithName(tokenStr)
	return ok
}

// ValidateTokenWithName 校验令牌并返回名称与作用域集合
func (s *Store) ValidateTokenWithName(tokenStr string) (name string, scopes string, ok bool) {
	_, name, scopes, ok = s.ValidateTokenPrincipal(tokenStr)
	return name, scopes, ok
}

// ValidateTokenPrincipal 校验令牌并返回不可变 ID、名称与作用域集合 (PR-04/PR-07)。
// 校验规则：
// 1. token_hash 匹配；
// 2. revoked_at 必须为空；
// 3. expires_at 为空或大于当前时间；
// 只有在认证完全成功时，才允许将 ID 写入 activityCh 推进活跃时间。
func (s *Store) ValidateTokenPrincipal(tokenStr string) (id, name, scopes string, ok bool) {
	tokenStr = strings.TrimSpace(tokenStr)
	if tokenStr == "" {
		return "", "", "", false
	}

	tokenHash := HashToken(tokenStr)
	var expiresAt, revokedAt string

	err := s.db.QueryRow(`
		SELECT id, name, COALESCE(scopes, ''), COALESCE(expires_at, ''), COALESCE(revoked_at, '')
		FROM api_tokens WHERE token_hash = ?
	`, tokenHash).Scan(&id, &name, &scopes, &expiresAt, &revokedAt)
	if err != nil {
		return "", "", "", false
	}

	// 1. 检查是否已被注销
	if revokedAt != "" {
		return "", "", "", false
	}

	// 2. 检查是否已过期 (malformed expires_at 必须 fail closed，绝不能把格式错误当作永不过期)
	if expiresAt != "" {
		expTime, parseErr := time.Parse(time.RFC3339, expiresAt)
		if parseErr != nil || !time.Now().UTC().Before(expTime) {
			return "", "", "", false
		}
	}

	// 认证通过：异步轻量更新最后使用时间
	select {
	case s.activityCh <- id:
	default:
	}
	return id, name, scopes, true
}

// GetToken 按 ID 查询 API 令牌记录 (PR-06/PR-07)
func (s *Store) GetToken(ctx context.Context, id string) (*APIToken, error) {
	id = strings.TrimSpace(id)
	var tok APIToken
	var needsRot int
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, token_prefix, created_at, COALESCE(last_used_at, ''),
		       COALESCE(scopes, ''), COALESCE(expires_at, ''), COALESCE(revoked_at, ''),
		       COALESCE(rotated_at, ''), needs_rotation
		FROM api_tokens WHERE id = ?
	`, id).Scan(
		&tok.ID, &tok.Name, &tok.TokenPrefix, &tok.CreatedAt, &tok.LastUsedAt,
		&tok.Scopes, &tok.ExpiresAt, &tok.RevokedAt, &tok.RotatedAt, &needsRot,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("token not found")
		}
		return nil, err
	}
	if tok.RevokedAt != "" {
		return nil, errors.New("token revoked")
	}
	if tok.ExpiresAt != "" {
		expTime, parseErr := time.Parse(time.RFC3339, tok.ExpiresAt)
		if parseErr != nil {
			return nil, fmt.Errorf("token has malformed expires_at: %w", parseErr)
		}
		if !time.Now().UTC().Before(expTime) {
			return nil, errors.New("token expired")
		}
	}
	tok.NeedsRotation = (needsRot == 1)
	tok.Token = tok.TokenPrefix + "****"
	return &tok, nil
}
