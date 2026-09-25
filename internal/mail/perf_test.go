/**
 * [INPUT]: 依赖 strings, testing, log, bytes
 * [OUTPUT]: MaskEmailForLog 与 LogMailPerf 的脱敏与结构化输出单元测试
 * [POS]: perf.go 的 MailPerf 观测中心单测套件，覆盖邮箱脱敏边界与 kv 成对输出格式
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package mail

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestMaskEmailForLog(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"ab***@qq.com", "ab***@qq.com"},
		{"abcdefg@qq.com", "ab***@qq.com"},
		{"ab@qq.com", "ab***@qq.com"},
		{"a@qq.com", "a***@qq.com"},
		{"user.name+tag@imap.163.com", "us***@imap.163.com"},
		{"noat", "***"},
		{"@domain.com", "***"},
	}
	for _, c := range cases {
		if got := MaskEmailForLog(c.in); got != c.want {
			t.Fatalf("MaskEmailForLog(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLogMailPerfFormat(t *testing.T) {
	var buf bytes.Buffer
	oldOut := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
	}()

	LogMailPerf("list_folder", "server", "imap.qq.com", "limit", 20, "fetch_ms", int64(170))

	got := buf.String()
	want := "[MailPerf] op=list_folder server=imap.qq.com limit=20 fetch_ms=170\n"
	if got != want {
		t.Fatalf("LogMailPerf output = %q, want %q", got, want)
	}
	if strings.Contains(got, "password") {
		t.Fatalf("性能日志严禁包含凭据字段: %q", got)
	}
}
