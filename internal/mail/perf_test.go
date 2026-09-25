/**
 * [INPUT]: 依赖 bufio, bytes, context, fmt, log, net, strings, testing, time, github.com/emersion/go-imap/client
 * [OUTPUT]: MaskEmailForLog / LogMailPerf 启停与格式 / pool_op 日志与 semaphore 释放顺序的单元测试
 * [POS]: perf.go 的 MailPerf 观测中心单测套件，覆盖邮箱脱敏边界、开关门禁、kv 成对输出格式与 FIX-1 时序保证 (LogMailPerf 执行期间 pc.sem 已释放)
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package mail

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/client"
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

// TestLogMailPerfFormat 验证默认 sink 的精确输出格式。
// 注意: 本测试只证明格式正确；凭据安全靠对所有 LogMailPerf 调用点的人工/静态审计保障 (见 PR-MAIL-00 审核报告)，
// 不应也不会被任何单一用例"证明"。
func TestLogMailPerfFormat(t *testing.T) {
	var buf bytes.Buffer
	oldOut := log.Writer()
	oldFlags := log.Flags()
	oldEnabled := mailPerfEnabled
	mailPerfEnabled = true
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
		mailPerfEnabled = oldEnabled
	}()

	LogMailPerf("list_folder", "server", "imap.qq.com", "limit", 20, "fetch_ms", int64(170))

	got := buf.String()
	want := "[MailPerf] op=list_folder server=imap.qq.com limit=20 fetch_ms=170\n"
	if got != want {
		t.Fatalf("LogMailPerf output = %q, want %q", got, want)
	}
}

// TestLogMailPerfDisabled 验证关闭 (默认) 时 LogMailPerf 直接返回，不触发任何 sink 输出。
func TestLogMailPerfDisabled(t *testing.T) {
	oldEnabled := mailPerfEnabled
	oldSink := mailPerfSink
	mailPerfEnabled = false
	sinkCalls := 0
	mailPerfSink = func(op string, kv ...any) { sinkCalls++ }
	defer func() {
		mailPerfEnabled = oldEnabled
		mailPerfSink = oldSink
	}()

	LogMailPerf("pool_op", "server", "imap.qq.com")

	if sinkCalls != 0 {
		t.Fatalf("MailPerf 关闭时不应产生任何日志输出, 实际 sink 被调用 %d 次", sinkCalls)
	}
}

// TestLogMailPerfEnabled 验证开启时 sink 收到 op 与成对 kv。
func TestLogMailPerfEnabled(t *testing.T) {
	oldEnabled := mailPerfEnabled
	oldSink := mailPerfSink
	mailPerfEnabled = true
	var gotOp string
	var gotKV []any
	mailPerfSink = func(op string, kv ...any) {
		gotOp = op
		gotKV = kv
	}
	defer func() {
		mailPerfEnabled = oldEnabled
		mailPerfSink = oldSink
	}()

	LogMailPerf("scan_uid_page", "folder", "INBOX", "messages", 7, "err", false)

	if gotOp != "scan_uid_page" {
		t.Fatalf("op = %q, want scan_uid_page", gotOp)
	}
	if len(gotKV) != 6 || gotKV[0] != "folder" || gotKV[2] != "messages" || gotKV[4] != "err" {
		t.Fatalf("kv 未成对且失序: %v", gotKV)
	}
}

// TestPoolOpPerfLogAfterSemaphoreRelease 是 FIX-1 的顺序证明:
// LogMailPerf("pool_op") 执行期间，另一个 goroutine 必须已可获得该账号的 pc.sem，
// 即性能日志发生在释放 semaphore 之后，日志 I/O 不延长单账号 IMAP slot 占用。
func TestPoolOpPerfLogAfterSemaphoreRelease(t *testing.T) {
	// 本地 Mock IMAP: 发送欢迎语并对每条 NOOP 应答，保证 ensure -> Ping 复用路径成功
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer conn.Close()
		if _, werr := conn.Write([]byte("* OK [CAPABILITY IMAP4rev1] Mock IMAP Server Ready\r\n")); werr != nil {
			return
		}
		reader := bufio.NewReader(conn)
		for {
			line, rerr := reader.ReadString('\n')
			if rerr != nil {
				return
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			switch strings.ToUpper(fields[1]) {
			case "NOOP":
				if _, werr := conn.Write([]byte(fields[0] + " OK NOOP completed\r\n")); werr != nil {
					return
				}
			case "LOGOUT":
				// 正常应答 LOGOUT，避免 Pool.Close -> Disconnect 在测试收尾时阻塞到命令超时
				_, _ = conn.Write([]byte("* BYE Mock IMAP Server logging out\r\n"))
				_, _ = conn.Write([]byte(fields[0] + " OK LOGOUT completed\r\n"))
				return
			}
		}
	}()

	clientConn, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial mock IMAP server failed: %v", err)
	}
	imapCli, err := client.New(clientConn)
	if err != nil {
		t.Fatalf("imap client.New failed: %v", err)
	}

	const testAccount = "perf_order@icloud.com"
	mockClient := NewClientForTesting(testAccount, "dummy", clientConn, imapCli)

	p := NewPool()
	defer p.Close()
	p.SetClientForTesting(testAccount, "dummy", mockClient)

	oldEnabled := mailPerfEnabled
	oldSink := mailPerfSink
	mailPerfEnabled = true
	semFreeDuringLog := false
	sawPoolOp := false
	sawErrField := ""
	mailPerfSink = func(op string, kv ...any) {
		if op != "pool_op" {
			return
		}
		sawPoolOp = true
		for i := 0; i+1 < len(kv); i += 2 {
			if k, _ := kv[i].(string); k == "err" {
				sawErrField = fmt.Sprint(kv[i+1])
			}
		}
		// 关键断言点: 此刻 (LogMailPerf 执行期间) 探测该账号的 sem 是否可被其他 goroutine 获得
		if p.TryLockForTesting(testAccount) {
			semFreeDuringLog = true
			p.UnlockForTesting(testAccount)
		}
	}
	defer func() {
		mailPerfEnabled = oldEnabled
		mailPerfSink = oldSink
	}()

	if err := p.DoContextWithServer(context.Background(), testAccount, "dummy", "", 0, "", func(c *Client) error {
		return nil
	}); err != nil {
		t.Fatalf("DoContextWithServer 失败: %v", err)
	}

	if !sawPoolOp {
		t.Fatalf("未观测到 pool_op 性能日志")
	}
	if sawErrField != "false" {
		t.Fatalf("pool_op err 字段 = %q, 期望 false", sawErrField)
	}
	if !semFreeDuringLog {
		t.Fatalf("LogMailPerf 执行期间 pc.sem 仍被持有: 性能日志发生在释放 semaphore 之前 (FIX-1 违例)")
	}
}
