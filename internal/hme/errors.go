/**
 * [INPUT]: 依赖 errors
 * [OUTPUT]: 对外提供 ErrInvalidResponseSchema, ErrOutcomeUnknown, ErrAuthFailed, ErrRateLimited
 * [POS]: internal/hme 的统一错误类型定义层
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package hme

import "errors"

var (
	// ErrInvalidResponseSchema 上游响应格式异常 (HTML/非法JSON/结构缺失/success=false)
	ErrInvalidResponseSchema = errors.New("invalid upstream response schema")

	// ErrOutcomeUnknown 上游写入结果不明 (网络中断/超时，需异步核对)
	ErrOutcomeUnknown = errors.New("upstream mutation outcome unknown")

	// ErrAuthFailed 凭据无效或已失效 (401/403)
	ErrAuthFailed = errors.New("upstream authentication failed")

	// ErrRateLimited 上游频率受限 (429)
	ErrRateLimited = errors.New("upstream rate limited")
)
