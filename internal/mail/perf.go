/**
 * [INPUT]: 依赖 fmt, log, strings
 * [OUTPUT]: 对外提供 LogMailPerf, MaskEmailForLog
 * [POS]: internal/mail 的 MailPerf 性能观测中心 (PR-MAIL-00)，统一 [MailPerf] 结构化埋点输出与邮箱地址脱敏；红线: 只记录耗时/计数/服务器/脱敏邮箱，严禁记录密码/正文/验证码
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package mail

import (
	"fmt"
	"log"
	"strings"
)

// MaskEmailForLog 将邮箱地址脱敏为 ab***@domain 形式，供性能日志输出。
// 空串原样返回；无 @ 的短串一律输出 ***，避免泄露本地部分。
func MaskEmailForLog(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	at := strings.LastIndex(addr, "@")
	if at <= 0 {
		return "***"
	}
	local, domain := addr[:at], addr[at+1:]
	keep := 2
	if len(local) < keep {
		keep = len(local)
	}
	return local[:keep] + "***@" + domain
}

// LogMailPerf 输出一行 [MailPerf] 结构化性能日志 (PR-MAIL-00)。
// kv 必须成对出现: key1, val1, key2, val2 ...；耗时字段由调用方以 *_ms 整数毫秒传入。
// 红线: 只允许传入耗时、计数、服务器地址、脱敏邮箱等非敏感字段，
// 严禁传入 IMAP 授权码、App Password、Cookie、Token、完整邮件正文与验证码。
func LogMailPerf(op string, kv ...any) {
	var b strings.Builder
	b.WriteString("[MailPerf] op=")
	b.WriteString(op)
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(&b, " %s=%v", kv[i], kv[i+1])
	}
	log.Print(b.String())
}

// perfServer 返回用于日志观测的服务器标识；测试注入的 Client 可能未填 server，兜底为标准 iCloud IMAP。
func (c *Client) perfServer() string {
	if c.server == "" {
		return IMAPServer
	}
	return c.server
}
