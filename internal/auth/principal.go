/**
 * [INPUT]: 依赖 strings
 * [OUTPUT]: 对外提供 Principal 结构、PrincipalKind 枚举与权限范围核验方法
 * [POS]: internal/auth 的资源级主体模型 (PR-04)，统一会话、令牌与系统身份
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package auth

import (
	"strings"
)

// PrincipalKind 认证主体类型
type PrincipalKind string

const (
	// PrincipalAdmin 管理员特权主体 (Web 会话或系统管理员 API Key)
	PrincipalAdmin PrincipalKind = "admin"
	// PrincipalToken 外部接入 API Token
	PrincipalToken PrincipalKind = "token"
	// PrincipalSystem 本地系统任务主体
	PrincipalSystem PrincipalKind = "system"
)

// Principal 统一身份与授权主体
type Principal struct {
	Kind            PrincipalKind `json:"kind"`
	ID              string        `json:"id"`
	TokenName       string        `json:"token_name,omitempty"`
	Scopes          []string      `json:"scopes"`
	AllowedTags     []string      `json:"allowed_tags,omitempty"`
	AllowedAccounts []string      `json:"allowed_accounts,omitempty"`
}

// IsAdmin 是否具备全局管理员权限
func (p Principal) IsAdmin() bool {
	if p.Kind == PrincipalAdmin || p.Kind == PrincipalSystem {
		return true
	}
	return p.HasScope("admin")
}

// HasScope 检查是否具备目标作用域 (admin 作用域统管所有动作)
func (p Principal) HasScope(scope string) bool {
	scope = strings.ToLower(strings.TrimSpace(scope))
	for _, s := range p.Scopes {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "admin" || s == scope {
			return true
		}
	}
	return false
}

// CanAllocate 是否被授权执行领号/分配操作
func (p Principal) CanAllocate() bool {
	return p.IsAdmin() || p.HasScope("allocate")
}

// CanVerify 是否被授权执行验证码查询操作
func (p Principal) CanVerify() bool {
	return p.IsAdmin() || p.HasScope("verify")
}
