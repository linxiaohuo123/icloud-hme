/**
 * [INPUT]: 依赖 strings
 * [OUTPUT]: 对外提供 IsProtectedAccount 纯判定函数
 * [POS]: internal/account 的安全保护策略纯函数定义层，提供多包复用且无循环依赖的保护账号判定
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package account

import "strings"

// IsProtectedAccount 严格判定指定账号名称或标签是否属于受保护账号。
// 规则：
// 1. personal / private / protected 做大小写和空格规范化；
// 2. 名称含 "大号" 作为兼容保护；
// 3. 同时有业务标签(如 gpt)与保护标签时，保护优先；
// 4. len(tags) == 0 绝不被视为非保护的充分证据(名称可能含大号)；
func IsProtectedAccount(name string, tags []string) bool {
	if strings.Contains(name, "大号") {
		return true
	}
	for _, t := range tags {
		norm := strings.ToLower(strings.TrimSpace(t))
		if norm == "personal" || norm == "private" || norm == "protected" {
			return true
		}
	}
	return false
}
