/**
 * [INPUT]: 依赖 bufio, bytes, context, errors, fmt, log, net, strings, testing, time, github.com/emersion/go-imap, github.com/emersion/go-imap/client
 * [OUTPUT]: MaskEmailForLog / LogMailPerf 启停与格式 / pool_op 日志与 semaphore 释放顺序 / 建连失败指标 / BODY requested-received 语义 / conn_err 分类 的单元测试
 * [POS]: perf.go 的 MailPerf 观测中心单测套件，覆盖邮箱脱敏边界、开关门禁、kv 成对输出格式、FIX-1 时序保证 (LogMailPerf 执行期间 pc.sem 已释放)、FIX-7/8/9 指标准确性
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package mail

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"
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

// findKV 取出 sink 捕获的 kv 中指定 key 的字符串值。
func findKV(kv []any, key string) (string, bool) {
	for i := 0; i+1 < len(kv); i += 2 {
		if k, _ := kv[i].(string); k == key {
			return fmt.Sprint(kv[i+1]), true
		}
	}
	return "", false
}

// spinAcceptCloseServer 启动一个"接受 TCP 连接后立即关闭"的本地服务，
// 使客户端 TLS 握手以 EOF 失败 —— 典型的连接类建连错误。
func spinAcceptCloseServer(t *testing.T) (port int, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, func() { _ = ln.Close(); <-done }
}

// TestPoolEnsure_DirectConnectFailureRecordsConnMS (FIX-7):
// 无 proxy 直连失败时 ConnectMS 必须有真实值, 不得输出 conn_ms=0 + err=true 的错误指标。
func TestPoolEnsure_DirectConnectFailureRecordsConnMS(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer ln.Close()
	accepted := make(chan struct{})
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		close(accepted)
		// 服务端保持连接 60ms 后再关闭: 让客户端 TLS 握手确定性地耗时 > 0ms
		time.Sleep(60 * time.Millisecond)
		_ = conn.Close()
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	pc := &pooledConn{
		appleID:     "t1@icloud.com",
		appPassword: "dummy",
		server:      "127.0.0.1",
		port:        port,
		sem:         make(chan struct{}, 1),
	}
	stats, ensureErr := pc.ensure(0)
	if ensureErr == nil {
		t.Fatalf("直连一个不完成 TLS 握手的服务端应当失败")
	}
	select {
	case <-accepted:
	default:
		t.Fatalf("服务端未被连接过, 测试无效")
	}
	if stats.ProxyFallback {
		t.Fatalf("未配置 proxy 不应发生 fallback")
	}
	if stats.DirectConnectMS <= 0 {
		t.Fatalf("direct_connect_ms=%d, 直连失败路径必须记录真实耗时 (FIX-7)", stats.DirectConnectMS)
	}
	if stats.ConnectMS <= 0 {
		t.Fatalf("conn_ms=%d, 直连失败路径不得为 0 (FIX-7)", stats.ConnectMS)
	}
}

// TestPoolOpEnsureConnErrFlagged (FIX-9):
// ensure 连接类失败 (TLS 握手 EOF) 时 pool_op 必须输出 ensure_err=true err=true conn_err=true。
func TestPoolOpEnsureConnErrFlagged(t *testing.T) {
	port, stop := spinAcceptCloseServer(t)
	defer stop()

	oldEnabled := mailPerfEnabled
	oldSink := mailPerfSink
	mailPerfEnabled = true
	var gotKV []any
	mailPerfSink = func(op string, kv ...any) {
		if op == "pool_op" {
			gotKV = kv
		}
	}
	defer func() {
		mailPerfEnabled = oldEnabled
		mailPerfSink = oldSink
	}()

	p := NewPool()
	defer p.Close()
	err := p.DoContextWithServer(context.Background(), "connerr@icloud.com", "dummy", "127.0.0.1", port, "", func(c *Client) error {
		return nil
	})
	if err == nil {
		t.Fatalf("ensure 失败时 DoContextWithServer 应返回错误")
	}
	if gotKV == nil {
		t.Fatalf("未观测到 pool_op 性能日志")
	}
	for key, want := range map[string]string{"ensure_err": "true", "err": "true", "conn_err": "true"} {
		if got, _ := findKV(gotKV, key); got != want {
			t.Fatalf("pool_op %s=%s, 期望 %s (FIX-9)", key, got, want)
		}
	}
}

// TestIsLikelyConnErrClassification (FIX-9):
// 连接类错误判别为 true, 认证等业务错误保持 false, 严禁全量归为 conn_err。
func TestIsLikelyConnErrClassification(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("IMAP 登录失败 (Authentication Failed) — 请确认授权码"), false},
		{errors.New("IMAP 登录失败 — 请检查邮箱账号、授权码和服务器地址: invalid credentials"), false},
		{errors.New("IMAP TLS 握手失败: tls: handshake failure"), true},
		{errors.New("dial tcp 127.0.0.1:993: connectex: No connection could be made because the target machine actively refused it."), true},
		{errors.New("dial tcp: lookup imap.example.invalid: no such host"), true},
		{errors.New("read tcp 127.0.0.1:1->127.0.0.1:2: use of closed network connection"), true},
		{errors.New("net/http: request canceled while waiting for connection (Client.Timeout exceeded i/o timeout)"), true},
	}
	for _, c := range cases {
		if got := isLikelyConnErr(c.err); got != c.want {
			t.Fatalf("isLikelyConnErr(%v) = %v, 期望 %v", c.err, got, c.want)
		}
	}
}

// TestMsgHasBodySection (FIX-8):
// received 计数依据的确定性单测 —— 仅当响应真实携带 BODY section 时为 true。
func TestMsgHasBodySection(t *testing.T) {
	if msgHasBodySection(nil) {
		t.Fatalf("nil message 不应计入 received")
	}
	noBody := &imap.Message{}
	if msgHasBodySection(noBody) {
		t.Fatalf("无 Body map 不应计入 received")
	}
	emptyMap := &imap.Message{Body: map[*imap.BodySectionName]imap.Literal{}}
	if msgHasBodySection(emptyMap) {
		t.Fatalf("空 Body map 不应计入 received")
	}
	withBody := &imap.Message{
		Body: map[*imap.BodySectionName]imap.Literal{
			{}: bytes.NewReader([]byte("body")),
		},
	}
	if !msgHasBodySection(withBody) {
		t.Fatalf("携带 BODY[] section 的消息应计入 received")
	}
}

// spinMockIMAPServer 启动一个本地 mock IMAP 服务: 发送欢迎语并按 cmd 分派应答。
// handler 返回应答行 (可多行); 返回 "LOGOUT" 分支由公共逻辑统一处理。
func spinMockIMAPServer(t *testing.T, respond func(tag, cmd string) []string) (port int, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
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
			tag, cmd := fields[0], strings.ToUpper(fields[1])
			if cmd == "LOGOUT" {
				_, _ = conn.Write([]byte("* BYE Mock IMAP Server logging out\r\n"))
				_, _ = conn.Write([]byte(tag + " OK LOGOUT completed\r\n"))
				return
			}
			if cmd == "LOGIN" {
				_, _ = conn.Write([]byte(tag + " OK [CAPABILITY IMAP4rev1] Logged in\r\n"))
				continue
			}
			for _, resp := range respond(tag, cmd) {
				if _, werr := conn.Write([]byte(resp + "\r\n")); werr != nil {
					return
				}
			}
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, func() { _ = ln.Close(); <-done }
}

// TestGetFullRequestedZeroWhenSelectFails (FIX-8 C):
// SELECT 失败时根本未发出 BODY FETCH, 必须输出 body_fetch_requested=0 而非固定 1。
func TestGetFullRequestedZeroWhenSelectFails(t *testing.T) {
	port, stop := spinMockIMAPServer(t, func(tag, cmd string) []string {
		switch cmd {
		case "NOOP":
			return []string{tag + " OK NOOP completed"}
		case "SELECT", "EXAMINE": // GetFullInFolderWithValidity 以 readOnly=true 发送 EXAMINE
			return []string{tag + " NO " + cmd + " failed"}
		default:
			return nil
		}
	})
	defer stop()

	clientConn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		t.Fatalf("dial mock IMAP server failed: %v", err)
	}
	imapCli, err := client.New(clientConn)
	if err != nil {
		t.Fatalf("imap client.New failed: %v", err)
	}
	if lerr := imapCli.Login("user", "dummy"); lerr != nil {
		t.Fatalf("mock Login failed: %v", lerr)
	}
	const testAccount = "getfull0@icloud.com"
	p := NewPool()
	defer p.Close()
	p.SetClientForTesting(testAccount, "dummy", NewClientForTesting(testAccount, "dummy", clientConn, imapCli))

	oldEnabled := mailPerfEnabled
	oldSink := mailPerfSink
	mailPerfEnabled = true
	var gotKV []any
	mailPerfSink = func(op string, kv ...any) {
		if op == "get_full" {
			gotKV = kv
		}
	}
	defer func() {
		mailPerfEnabled = oldEnabled
		mailPerfSink = oldSink
	}()

	err = p.DoContextWithServer(context.Background(), testAccount, "dummy", "", 0, "", func(c *Client) error {
		_, ferr := c.GetFullInFolderWithValidity("INBOX", 0, 42)
		return ferr
	})
	if err == nil {
		t.Fatalf("SELECT 失败时 get_full 应返回错误")
	}
	if gotKV == nil {
		t.Fatalf("未观测到 get_full 性能日志")
	}
	if got, _ := findKV(gotKV, "body_fetch_requested"); got != "0" {
		t.Fatalf("body_fetch_requested=%s, SELECT 失败时必须为 0 (FIX-8 C)", got)
	}
	if got, _ := findKV(gotKV, "body_fetch_received"); got != "0" {
		t.Fatalf("body_fetch_received=%s, 期望 0", got)
	}
	if got, _ := findKV(gotKV, "err"); got != "true" {
		t.Fatalf("err=%s, 期望 true", got)
	}
}

// TestGetFullBatchUIDValidityMismatchRequestedZero (FIX-8 D):
// UIDVALIDITY mismatch 发生在 UidFetch 之前, 必须输出 body_fetch_requested=0 而非 len(uids)。
func TestGetFullBatchUIDValidityMismatchRequestedZero(t *testing.T) {
	port, stop := spinMockIMAPServer(t, func(tag, cmd string) []string {
		switch cmd {
		case "NOOP":
			return []string{tag + " OK NOOP completed"}
		case "SELECT", "EXAMINE": // GetFullBatchInFolderWithValidity 以 readOnly=true 发送 EXAMINE
			// 返回 UIDVALIDITY=999, 与请求的 12345 构成 mismatch
			return []string{
				"* 2 EXISTS",
				"* OK [UIDVALIDITY 999] UIDs valid",
				tag + " OK [READ-ONLY] SELECT completed",
			}
		default:
			return nil
		}
	})
	defer stop()

	clientConn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		t.Fatalf("dial mock IMAP server failed: %v", err)
	}
	imapCli, err := client.New(clientConn)
	if err != nil {
		t.Fatalf("imap client.New failed: %v", err)
	}
	if lerr := imapCli.Login("user", "dummy"); lerr != nil {
		t.Fatalf("mock Login failed: %v", lerr)
	}
	const testAccount = "batchmismatch@icloud.com"
	p := NewPool()
	defer p.Close()
	p.SetClientForTesting(testAccount, "dummy", NewClientForTesting(testAccount, "dummy", clientConn, imapCli))

	oldEnabled := mailPerfEnabled
	oldSink := mailPerfSink
	mailPerfEnabled = true
	var gotKV []any
	mailPerfSink = func(op string, kv ...any) {
		if op == "get_full_batch" {
			gotKV = kv
		}
	}
	defer func() {
		mailPerfEnabled = oldEnabled
		mailPerfSink = oldSink
	}()

	err = p.DoContextWithServer(context.Background(), testAccount, "dummy", "", 0, "", func(c *Client) error {
		_, ferr := c.GetFullBatchInFolderWithValidity("INBOX", 12345, []uint32{7, 8})
		return ferr
	})
	if !errors.Is(err, ErrUIDValidityMismatch) {
		t.Fatalf("期望 UIDVALIDITY mismatch 错误, 实际: %v", err)
	}
	if gotKV == nil {
		t.Fatalf("未观测到 get_full_batch 性能日志")
	}
	if got, _ := findKV(gotKV, "body_fetch_requested"); got != "0" {
		t.Fatalf("body_fetch_requested=%s, UIDVALIDITY mismatch 时必须为 0 (FIX-8 D)", got)
	}
	if got, _ := findKV(gotKV, "body_fetch_received"); got != "0" {
		t.Fatalf("body_fetch_received=%s, 期望 0", got)
	}
	if got, _ := findKV(gotKV, "err"); got != "true" {
		t.Fatalf("err=%s, 期望 true", got)
	}
}
