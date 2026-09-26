/**
 * [INPUT]: 依赖 bufio, fmt, net, strconv, strings, sync, testing, time, github.com/emersion/go-imap/client
 * [OUTPUT]: 对外提供 Direct SEARCH 与 Recent Fallback 模式下 metadata-first 行为、网络往返命令检验、MailPerf 指标与边界条件的单元测试
 * [POS]: internal/mail 的别名检索两阶段元数据优先架构单元测试 (PR-MAIL-02)
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package mail

import (
	"bufio"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/client"
)

type mockMailItem struct {
	SeqNum      uint32
	UID         uint32
	DateStr     string // "26-Sep-2026 00:00:00 +0000"
	Subject     string
	FromAddr    string
	ToAddr      string
	DeliveredTo string
	Body        string
}

func (m mockMailItem) headerLiteral() string {
	var b strings.Builder
	if m.ToAddr != "" {
		b.WriteString(fmt.Sprintf("To: %s\r\n", m.ToAddr))
	}
	if m.DeliveredTo != "" {
		b.WriteString(fmt.Sprintf("Delivered-To: %s\r\n", m.DeliveredTo))
	}
	if m.FromAddr != "" {
		b.WriteString(fmt.Sprintf("From: %s\r\n", m.FromAddr))
	}
	if m.Subject != "" {
		b.WriteString(fmt.Sprintf("Subject: %s\r\n", m.Subject))
	}
	b.WriteString(fmt.Sprintf("Date: %s\r\n", m.DateStr))
	b.WriteString("\r\n")
	return b.String()
}

func (m mockMailItem) fullLiteral() string {
	hdr := m.headerLiteral()
	return hdr + m.Body
}

func seqsetContainsUID(seqsetStr string, uid uint32) bool {
	parts := strings.Split(seqsetStr, ",")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if strings.Contains(p, ":") {
			rangeParts := strings.Split(p, ":")
			if len(rangeParts) == 2 {
				start, err1 := strconv.ParseUint(rangeParts[0], 10, 32)
				end, err2 := strconv.ParseUint(rangeParts[1], 10, 32)
				if err1 == nil && err2 == nil {
					if start > end {
						start, end = end, start
					}
					if uint32(start) <= uid && uid <= uint32(end) {
						return true
					}
				}
			}
		} else {
			val, err := strconv.ParseUint(p, 10, 32)
			if err == nil && uint32(val) == uid {
				return true
			}
		}
	}
	return false
}

func parseAddrParts(addr string) (string, string) {
	addr = strings.TrimSpace(addr)
	at := strings.Index(addr, "@")
	if at < 0 {
		if addr == "" {
			return "none", "example.com"
		}
		return addr, "example.com"
	}
	return addr[:at], addr[at+1:]
}

type mockServerOption func(*mockServerSettings)

type mockServerSettings struct {
	failUIDMetadataAfterN   int
	failFetchMetadataAfterN int
	searchHeaderUIDs        map[string][]uint32
	folderMails             map[string]map[uint32]mockMailItem
	folderSearchUIDs        map[string][]uint32
}

func withFailUIDMetadataAfter(n int) mockServerOption {
	return func(s *mockServerSettings) { s.failUIDMetadataAfterN = n }
}

func withFailFetchMetadataAfter(n int) mockServerOption {
	return func(s *mockServerSettings) { s.failFetchMetadataAfterN = n }
}

func withSearchHeaderUIDs(m map[string][]uint32) mockServerOption {
	return func(s *mockServerSettings) { s.searchHeaderUIDs = m }
}

func withFolderMails(fm map[string]map[uint32]mockMailItem, fs map[string][]uint32) mockServerOption {
	return func(s *mockServerSettings) {
		s.folderMails = fm
		s.folderSearchUIDs = fs
	}
}

func spinMockMailServer(t *testing.T, totalMessages int, searchUIDs []uint32, mails map[uint32]mockMailItem, opts ...mockServerOption) (port int, getCommands func() []string, stop func()) {
	t.Helper()
	var settings mockServerSettings
	for _, opt := range opts {
		opt(&settings)
	}

	var sortedMails []mockMailItem
	for _, m := range mails {
		sortedMails = append(sortedMails, m)
	}
	sort.Slice(sortedMails, func(i, j int) bool {
		return sortedMails[i].SeqNum < sortedMails[j].SeqNum
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}

	var mu sync.Mutex
	var commands []string

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer conn.Close()

		if _, werr := conn.Write([]byte("* OK [CAPABILITY IMAP4rev1] Mock IMAP Ready\r\n")); werr != nil {
			return
		}

		reader := bufio.NewReader(conn)
		currentFolder := "INBOX"

		getActiveSortedMails := func() []mockMailItem {
			if settings.folderMails != nil && settings.folderMails[currentFolder] != nil {
				var list []mockMailItem
				for _, m := range settings.folderMails[currentFolder] {
					list = append(list, m)
				}
				sort.Slice(list, func(i, j int) bool { return list[i].SeqNum < list[j].SeqNum })
				return list
			}
			return sortedMails
		}

		getActiveSearchUIDs := func(trimmedLine string) []uint32 {
			if settings.folderSearchUIDs != nil && settings.folderSearchUIDs[currentFolder] != nil {
				return settings.folderSearchUIDs[currentFolder]
			}
			if len(settings.searchHeaderUIDs) > 0 {
				upperLine := strings.ToUpper(trimmedLine)
				for h, uids := range settings.searchHeaderUIDs {
					if strings.Contains(upperLine, "HEADER "+strings.ToUpper(h)) {
						return uids
					}
				}
				return nil
			}
			return searchUIDs
		}

		for {
			line, rerr := reader.ReadString('\n')
			if rerr != nil {
				return
			}
			trimmed := strings.TrimRight(line, "\r\n")
			fields := strings.Fields(trimmed)
			if len(fields) < 2 {
				continue
			}

			mu.Lock()
			commands = append(commands, trimmed)
			mu.Unlock()

			tag := fields[0]
			cmd := strings.ToUpper(fields[1])

			switch cmd {
			case "LOGOUT":
				_, _ = conn.Write([]byte("* BYE logging out\r\n" + tag + " OK LOGOUT completed\r\n"))
				return
			case "LOGIN":
				_, _ = conn.Write([]byte(tag + " OK Logged in\r\n"))
			case "LIST":
				_, _ = conn.Write([]byte("* LIST (\\HasNoChildren) \"/\" \"INBOX\"\r\n* LIST (\\HasNoChildren \\Junk) \"/\" \"Junk\"\r\n" + tag + " OK LIST completed\r\n"))
			case "SELECT", "EXAMINE":
				if len(fields) >= 3 {
					currentFolder = strings.Trim(fields[2], "\"")
				}
				folderCount := totalMessages
				if settings.folderMails != nil && settings.folderMails[currentFolder] != nil {
					folderCount = len(settings.folderMails[currentFolder])
				}
				resp := fmt.Sprintf("* %d EXISTS\r\n* OK [UIDVALIDITY 1000] UIDs valid\r\n* OK [UIDNEXT 9999] Predicted next UID\r\n%s OK [READ-ONLY] %s completed\r\n", folderCount, tag, cmd)
				_, _ = conn.Write([]byte(resp))
			case "UID":
				if len(fields) >= 3 && strings.ToUpper(fields[2]) == "SEARCH" {
					uids := getActiveSearchUIDs(trimmed)
					var uidStrs []string
					for _, u := range uids {
						uidStrs = append(uidStrs, strconv.Itoa(int(u)))
					}
					resp := "* SEARCH"
					if len(uidStrs) > 0 {
						resp += " " + strings.Join(uidStrs, " ")
					}
					resp += "\r\n" + tag + " OK UID SEARCH completed\r\n"
					_, _ = conn.Write([]byte(resp))
				} else if len(fields) >= 3 && strings.ToUpper(fields[2]) == "FETCH" {
					// UID FETCH <seqset> (...)
					isBody := strings.Contains(strings.ToUpper(trimmed), "BODY.PEEK[]") || strings.Contains(strings.ToUpper(trimmed), "BODY[]")
					var resps []string
					metadataSent := 0
					failed := false
					for _, mail := range getActiveSortedMails() {
						if seqsetContainsUID(fields[3], mail.UID) {
							fUser, fHost := parseAddrParts(mail.FromAddr)
							tUser, tHost := parseAddrParts(mail.ToAddr)
							if isBody {
								lit := mail.fullLiteral()
								resps = append(resps, fmt.Sprintf("* %d FETCH (UID %d FLAGS () INTERNALDATE %q ENVELOPE (%q %q ((NIL NIL %q %q)) NIL NIL ((NIL NIL %q %q)) NIL NIL NIL NIL) BODY[] {%d}\r\n%s)\r\n",
									mail.SeqNum, mail.UID, mail.DateStr, mail.DateStr, mail.Subject, fUser, fHost, tUser, tHost, len(lit), lit))
							} else {
								lit := mail.headerLiteral()
								resps = append(resps, fmt.Sprintf("* %d FETCH (UID %d FLAGS () INTERNALDATE %q ENVELOPE (%q %q ((NIL NIL %q %q)) NIL NIL ((NIL NIL %q %q)) NIL NIL NIL NIL) BODY[HEADER.FIELDS (TO CC DELIVERED-TO X-ORIGINAL-TO ENVELOPE-TO X-FORWARDED-TO RESENT-TO X-ENVELOPE-TO ORIGINAL-RECIPIENT X-APPLE-ORIGINAL-TO X-APPLE-RECIPIENT SUBJECT FROM)] {%d}\r\n%s)\r\n",
									mail.SeqNum, mail.UID, mail.DateStr, mail.DateStr, mail.Subject, fUser, fHost, tUser, tHost, len(lit), lit))
								metadataSent++
								if settings.failUIDMetadataAfterN > 0 && metadataSent >= settings.failUIDMetadataAfterN {
									failed = true
									break
								}
							}
						}
					}
					if failed {
						resps = append(resps, tag+" NO [SERVERBUG] partial metadata fetch connection dropped\r\n")
					} else {
						resps = append(resps, tag+" OK UID FETCH completed\r\n")
					}
					for _, r := range resps {
						_, _ = conn.Write([]byte(r))
					}
				}
			case "FETCH":
				// FETCH <seqset> (...)
				isBody := strings.Contains(strings.ToUpper(trimmed), "BODY.PEEK[]") || strings.Contains(strings.ToUpper(trimmed), "BODY[]")
				var resps []string
				metadataSent := 0
				failed := false
				for _, mail := range getActiveSortedMails() {
					if seqsetContainsUID(fields[2], mail.SeqNum) {
						fUser, fHost := parseAddrParts(mail.FromAddr)
						tUser, tHost := parseAddrParts(mail.ToAddr)
						if isBody {
							lit := mail.fullLiteral()
							resps = append(resps, fmt.Sprintf("* %d FETCH (UID %d FLAGS () INTERNALDATE %q ENVELOPE (%q %q ((NIL NIL %q %q)) NIL NIL ((NIL NIL %q %q)) NIL NIL NIL NIL) BODY[] {%d}\r\n%s)\r\n",
								mail.SeqNum, mail.UID, mail.DateStr, mail.DateStr, mail.Subject, fUser, fHost, tUser, tHost, len(lit), lit))
						} else {
							lit := mail.headerLiteral()
							resps = append(resps, fmt.Sprintf("* %d FETCH (UID %d FLAGS () INTERNALDATE %q ENVELOPE (%q %q ((NIL NIL %q %q)) NIL NIL ((NIL NIL %q %q)) NIL NIL NIL NIL) BODY[HEADER.FIELDS (TO CC DELIVERED-TO X-ORIGINAL-TO ENVELOPE-TO X-FORWARDED-TO RESENT-TO X-ENVELOPE-TO ORIGINAL-RECIPIENT X-APPLE-ORIGINAL-TO X-APPLE-RECIPIENT SUBJECT FROM)] {%d}\r\n%s)\r\n",
								mail.SeqNum, mail.UID, mail.DateStr, mail.DateStr, mail.Subject, fUser, fHost, tUser, tHost, len(lit), lit))
							metadataSent++
							if settings.failFetchMetadataAfterN > 0 && metadataSent >= settings.failFetchMetadataAfterN {
								failed = true
								break
							}
						}
					}
				}
				if failed {
					resps = append(resps, tag+" NO [SERVERBUG] partial metadata fetch connection dropped\r\n")
				} else {
					resps = append(resps, tag+" OK FETCH completed\r\n")
				}
				for _, r := range resps {
					_, _ = conn.Write([]byte(r))
				}
			default:
				_, _ = conn.Write([]byte(tag + " OK " + cmd + " completed\r\n"))
			}
		}
	}()

	return ln.Addr().(*net.TCPAddr).Port, func() []string {
		mu.Lock()
		defer mu.Unlock()
		cp := make([]string, len(commands))
		copy(cp, commands)
		return cp
	}, func() {
		_ = ln.Close()
		<-done
	}
}

func createTestClient(t *testing.T, port int) *Client {
	t.Helper()
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
	return NewClientForTesting("test@icloud.com", "dummy", clientConn, imapCli)
}

// TestDirectSearch_MetadataFirst_CandidateOnlyBody 验证 Direct SEARCH 路径：
// SEARCH 返回 2 个 UID，但只有 1 个匹配目标 alias：
// 1. 第一阶段仅发 metadata UID FETCH (包含 HEADER.FIELDS，不含 BODY[])
// 2. 第二阶段仅针对匹配的 UID 发送单次 UID FETCH BODY.PEEK[]，绝不拉取无关 UID 的正文
// 3. MailPerf 记录 metadata_fetch_requested=2, candidate_count=1, body_fetch_requested=1, body_fetch_received=1
func TestDirectSearch_MetadataFirst_CandidateOnlyBody(t *testing.T) {
	mails := map[uint32]mockMailItem{
		101: {
			SeqNum:      1,
			UID:         101,
			DateStr:     "26-Sep-2026 00:00:00 +0000",
			Subject:     "Verification Code",
			FromAddr:    "service@example.com",
			ToAddr:      "alias@icloud.com",
			DeliveredTo: "alias@icloud.com",
			Body:        "Your code is 654321",
		},
		102: {
			SeqNum:      2,
			UID:         102,
			DateStr:     "26-Sep-2026 00:01:00 +0000",
			Subject:     "Newsletter",
			FromAddr:    "news@example.com",
			ToAddr:      "unrelated@other.com",
			DeliveredTo: "unrelated@other.com",
			Body:        "Big newsletter body content",
		},
	}

	port, getCommands, stop := spinMockMailServer(t, 2, []uint32{101, 102}, mails)
	defer stop()

	c := createTestClient(t, port)
	defer c.ForceClose()

	oldEnabled := mailPerfEnabled
	oldSink := mailPerfSink
	mailPerfEnabled = true
	var perfKV []any
	mailPerfSink = func(op string, kv ...any) {
		if op == "find_by_recipient" {
			perfKV = kv
		}
	}
	defer func() {
		mailPerfEnabled = oldEnabled
		mailPerfSink = oldSink
	}()

	receivedMsgs, err := c.FindByRecipientInFolder("alias@icloud.com", "INBOX", 10, 0)
	if err != nil {
		t.Fatalf("FindByRecipientInFolder failed: %v", err)
	}

	if len(receivedMsgs) != 1 {
		t.Fatalf("期望命中 1 封邮件, 实际命中 %d 封", len(receivedMsgs))
	}
	if receivedMsgs[0].UID != 101 {
		t.Fatalf("期望命中 UID 101, 实际: %d", receivedMsgs[0].UID)
	}
	if !strings.Contains(receivedMsgs[0].Preview, "654321") {
		t.Fatalf("期望 Preview 包含正文验证码 654321, 实际: %q", receivedMsgs[0].Preview)
	}
	if receivedMsgs[0].UIDValidity != 1000 {
		t.Fatalf("UIDValidity = %d, want 1000", receivedMsgs[0].UIDValidity)
	}
	if receivedMsgs[0].MessageRef == "" {
		t.Fatalf("MessageRef 不应为空")
	}

	cmds := getCommands()
	var metadataFetchCmd, bodyFetchCmd string
	for _, cmd := range cmds {
		u := strings.ToUpper(cmd)
		if strings.Contains(u, "UID FETCH") {
			if strings.Contains(u, "HEADER.FIELDS") {
				metadataFetchCmd = cmd
			} else if strings.Contains(u, "BODY.PEEK[]") || strings.Contains(u, "BODY[]") {
				bodyFetchCmd = cmd
			}
		}
	}

	if metadataFetchCmd == "" {
		t.Fatalf("未观察到第一阶段 metadata UID FETCH 命令: %v", cmds)
	}
	if strings.Contains(strings.ToUpper(metadataFetchCmd), "BODY.PEEK[]") || strings.Contains(strings.ToUpper(metadataFetchCmd), "BODY[]") {
		t.Fatalf("第一阶段 metadata FETCH 绝不应请求完整正文: %s", metadataFetchCmd)
	}

	if bodyFetchCmd == "" {
		t.Fatalf("未观察到第二阶段候选 BODY UID FETCH 命令: %v", cmds)
	}
	fields := strings.Fields(bodyFetchCmd)
	if len(fields) >= 4 {
		seqsetArg := fields[3]
		if seqsetArg != "101" {
			t.Fatalf("第二阶段候选正文拉取必须且仅请求匹配的 UID 101，实际请求: %s", seqsetArg)
		}
	}

	if perfKV == nil {
		t.Fatalf("未输出 find_by_recipient MailPerf 日志")
	}
	assertKV := map[string]string{
		"uids_found":               "2",
		"metadata_fetch_requested": "2",
		"metadata_fetch_received":  "2",
		"candidate_count":          "1",
		"body_fetch_requested":     "1",
		"body_fetch_received":      "1",
		"matched":                  "1",
		"fallback":                 "false",
		"err":                      "false",
	}
	for k, want := range assertKV {
		if got, _ := findKV(perfKV, k); got != want {
			t.Errorf("MailPerf %s = %q, want %q", k, got, want)
		}
	}
}

// TestDirectSearch_NoCandidate_FallsBackToRecent 验证 Direct SEARCH 路径：
// SEARCH 返回 2 个 UID (101, 102)，但 metadata 显示都不匹配：
// 1. 第一阶段 metadata FETCH 正常执行；
// 2. candidate_count = 0，绝不发出针对 101, 102 的 BODY FETCH；
// 3. fallback 标志置为 true，穿透到 recent_fallback 阶段。
func TestDirectSearch_NoCandidate_FallsBackToRecent(t *testing.T) {
	mails := map[uint32]mockMailItem{
		101: {
			SeqNum:      1,
			UID:         101,
			DateStr:     "26-Sep-2026 00:00:00 +0000",
			Subject:     "Promo 1",
			ToAddr:      "promo1@other.com",
			DeliveredTo: "promo1@other.com",
			Body:        "Promo body 1",
		},
		102: {
			SeqNum:      2,
			UID:         102,
			DateStr:     "26-Sep-2026 00:01:00 +0000",
			Subject:     "Promo 2",
			ToAddr:      "promo2@other.com",
			DeliveredTo: "promo2@other.com",
			Body:        "Promo body 2",
		},
	}

	port, getCommands, stop := spinMockMailServer(t, 2, []uint32{101, 102}, mails)
	defer stop()

	c := createTestClient(t, port)
	defer c.ForceClose()

	oldEnabled := mailPerfEnabled
	oldSink := mailPerfSink
	mailPerfEnabled = true
	var findPerfKV, recentPerfKV []any
	mailPerfSink = func(op string, kv ...any) {
		if op == "find_by_recipient" {
			findPerfKV = kv
		} else if op == "recent_fallback" {
			recentPerfKV = kv
		}
	}
	defer func() {
		mailPerfEnabled = oldEnabled
		mailPerfSink = oldSink
	}()

	receivedMsgs, err := c.FindByRecipientInFolder("target_alias@icloud.com", "INBOX", 10, 0)
	if err != nil {
		t.Fatalf("FindByRecipientInFolder failed: %v", err)
	}
	if len(receivedMsgs) != 0 {
		t.Fatalf("期望命中 0 封邮件, 实际命中 %d 封", len(receivedMsgs))
	}

	// 验证未发出任何针对 101, 102 的 UID FETCH BODY
	cmds := getCommands()
	for _, cmd := range cmds {
		u := strings.ToUpper(cmd)
		if strings.Contains(u, "UID FETCH") && (strings.Contains(u, "BODY.PEEK[]") || strings.Contains(u, "BODY[]")) {
			t.Fatalf("SEARCH 无候选时绝不应发出针对 SEARCH 结果的 UID BODY FETCH: %s", cmd)
		}
	}

	// 验证 find_by_recipient 的 fallback 指标
	if findPerfKV == nil {
		t.Fatalf("未输出 find_by_recipient 性能日志")
	}
	if got, _ := findKV(findPerfKV, "fallback"); got != "true" {
		t.Fatalf("find_by_recipient fallback = %q, want true", got)
	}
	if got, _ := findKV(findPerfKV, "candidate_count"); got != "0" {
		t.Fatalf("find_by_recipient candidate_count = %q, want 0", got)
	}
	if got, _ := findKV(findPerfKV, "body_fetch_requested"); got != "0" {
		t.Fatalf("find_by_recipient body_fetch_requested = %q, want 0", got)
	}

	// 验证穿透到了 recent_fallback
	if recentPerfKV == nil {
		t.Fatalf("未输出 recent_fallback 性能日志")
	}
}

// TestRecentFallback_ZeroCandidates_ZeroBodyFetch 验证 Recent Fallback 路径：
// 邮箱扫描 10 封信件，0 封匹配目标别名：
// 1. 第一阶段 FETCH 10 封 metadata；
// 2. 第二阶段 candidate_count = 0，绝对不发出任何 BODY FETCH 命令；
// 3. MailPerf: metadata_fetch_requested=10, candidate_count=0, body_fetch_requested=0, body_fetch_received=0。
func TestRecentFallback_ZeroCandidates_ZeroBodyFetch(t *testing.T) {
	mails := make(map[uint32]mockMailItem)
	for i := uint32(1); i <= 10; i++ {
		mails[i] = mockMailItem{
			SeqNum:      i,
			UID:         i,
			DateStr:     "26-Sep-2026 00:00:00 +0000",
			Subject:     fmt.Sprintf("Other Mail %d", i),
			ToAddr:      fmt.Sprintf("other%d@domain.com", i),
			DeliveredTo: fmt.Sprintf("other%d@domain.com", i),
			Body:        "Heavy body content",
		}
	}

	// searchUIDs 为空 -> 直接进入 recent_fallback
	port, getCommands, stop := spinMockMailServer(t, 10, nil, mails)
	defer stop()

	c := createTestClient(t, port)
	defer c.ForceClose()

	oldEnabled := mailPerfEnabled
	oldSink := mailPerfSink
	mailPerfEnabled = true
	var recentPerfKV []any
	mailPerfSink = func(op string, kv ...any) {
		if op == "recent_fallback" {
			recentPerfKV = kv
		}
	}
	defer func() {
		mailPerfEnabled = oldEnabled
		mailPerfSink = oldSink
	}()

	receivedMsgs, err := c.FindByRecipientInFolder("target_alias@icloud.com", "INBOX", 5, 0)
	if err != nil {
		t.Fatalf("FindByRecipientInFolder failed: %v", err)
	}
	if len(receivedMsgs) != 0 {
		t.Fatalf("期望命中 0 封邮件, 实际命中 %d 封", len(receivedMsgs))
	}

	// 关键验证：整条链路中绝对没有发送任何 BODY FETCH 命令！
	cmds := getCommands()
	for _, cmd := range cmds {
		u := strings.ToUpper(cmd)
		if strings.Contains(u, "BODY.PEEK[]") || strings.Contains(u, "BODY[]") {
			t.Fatalf("无候选时绝不应发送任何包含完整正文的 FETCH 命令: %s", cmd)
		}
	}

	if recentPerfKV == nil {
		t.Fatalf("未记录 recent_fallback 性能日志")
	}
	assertKV := map[string]string{
		"metadata_fetch_requested": "10",
		"metadata_fetch_received":  "10",
		"candidate_count":          "0",
		"body_fetch_requested":     "0",
		"body_fetch_received":      "0",
		"matched":                  "0",
		"err":                      "false",
	}
	for k, want := range assertKV {
		if got, _ := findKV(recentPerfKV, k); got != want {
			t.Errorf("MailPerf %s = %q, want %q", k, got, want)
		}
	}
}

// TestRecentFallback_OneCandidateOutOfTen_FetchesOnlyCandidateBody 验证 Recent Fallback 路径：
// 邮箱扫描 10 封信件，仅 UID 8 的 Delivered-To 属于目标别名：
// 1. 第一阶段拉取 10 封 metadata；
// 2. 第二阶段只针对 UID 8 发出单次 UID FETCH 拉取完整正文；
// 3. MailPerf: metadata_fetch_requested=10, candidate_count=1, body_fetch_requested=1, body_fetch_received=1。
func TestRecentFallback_OneCandidateOutOfTen_FetchesOnlyCandidateBody(t *testing.T) {
	mails := make(map[uint32]mockMailItem)
	for i := uint32(1); i <= 10; i++ {
		to := fmt.Sprintf("other%d@domain.com", i)
		delivered := to
		body := "Unrelated body"
		if i == 8 {
			delivered = "myalias@icloud.com"
			body = "Your Apple verification code is 314159"
		}
		mails[i] = mockMailItem{
			SeqNum:      i,
			UID:         i,
			DateStr:     "26-Sep-2026 00:00:00 +0000",
			Subject:     fmt.Sprintf("Mail %d", i),
			ToAddr:      to,
			DeliveredTo: delivered,
			Body:        body,
		}
	}

	port, getCommands, stop := spinMockMailServer(t, 10, nil, mails)
	defer stop()

	c := createTestClient(t, port)
	defer c.ForceClose()

	oldEnabled := mailPerfEnabled
	oldSink := mailPerfSink
	mailPerfEnabled = true
	var recentPerfKV []any
	mailPerfSink = func(op string, kv ...any) {
		if op == "recent_fallback" {
			recentPerfKV = kv
		}
	}
	defer func() {
		mailPerfEnabled = oldEnabled
		mailPerfSink = oldSink
	}()

	receivedMsgs, err := c.FindByRecipientInFolder("myalias@icloud.com", "INBOX", 5, 0)
	if err != nil {
		t.Fatalf("FindByRecipientInFolder failed: %v", err)
	}

	if len(receivedMsgs) != 1 {
		t.Fatalf("期望命中 1 封邮件, 实际命中 %d 封", len(receivedMsgs))
	}
	if receivedMsgs[0].UID != 8 {
		t.Fatalf("期望命中 UID 8, 实际: %d", receivedMsgs[0].UID)
	}
	if !strings.Contains(receivedMsgs[0].Preview, "314159") {
		t.Fatalf("期望 Preview 包含验证码 314159, 实际: %q", receivedMsgs[0].Preview)
	}

	// 协议与网络形态验证
	cmds := getCommands()
	var bodyFetchCmd string
	for _, cmd := range cmds {
		u := strings.ToUpper(cmd)
		if strings.Contains(u, "BODY.PEEK[]") || strings.Contains(u, "BODY[]") {
			bodyFetchCmd = cmd
		}
	}
	if bodyFetchCmd == "" {
		t.Fatalf("第二阶段必须为候选 UID 8 发出正文 FETCH 命令")
	}
	if !strings.HasPrefix(strings.ToUpper(bodyFetchCmd), "A") && !strings.Contains(strings.ToUpper(bodyFetchCmd), "UID FETCH 8 ") {
		// 校验命令包含 UID FETCH 8
		if !strings.Contains(bodyFetchCmd, " 8 ") {
			t.Fatalf("正文拉取命令必须且仅针对 UID 8，实际命令: %s", bodyFetchCmd)
		}
	}

	if recentPerfKV == nil {
		t.Fatalf("未输出 recent_fallback 性能日志")
	}
	assertKV := map[string]string{
		"metadata_fetch_requested": "10",
		"metadata_fetch_received":  "10",
		"candidate_count":          "1",
		"body_fetch_requested":     "1",
		"body_fetch_received":      "1",
		"matched":                  "1",
	}
	for k, want := range assertKV {
		if got, _ := findKV(recentPerfKV, k); got != want {
			t.Errorf("MailPerf %s = %q, want %q", k, got, want)
		}
	}
}

// TestAliasSearch_LimitAndNewestFirstOrder 验证 Limit 语义与 newest-first 排序保证：
// 3 封匹配别名的邮件 (UID 10, 20, 30)，limit = 2：
// 1. 第一阶段 metadata 发现 3 封匹配；
// 2. 第二阶段仅对最新的 2 封 (UID 20 和 UID 30) 发出批量 BODY FETCH (单次 UID FETCH 20,30)；
// 3. 回调收到的邮件严格保持 newest-first 排序 (UID 30 -> UID 20)。
func TestAliasSearch_LimitAndNewestFirstOrder(t *testing.T) {
	mails := map[uint32]mockMailItem{
		10: {
			SeqNum:      1,
			UID:         10,
			DateStr:     "24-Sep-2026 00:00:00 +0000",
			Subject:     "Old Code",
			ToAddr:      "alias@icloud.com",
			DeliveredTo: "alias@icloud.com",
			Body:        "Code 10",
		},
		20: {
			SeqNum:      2,
			UID:         20,
			DateStr:     "25-Sep-2026 00:00:00 +0000",
			Subject:     "Mid Code",
			ToAddr:      "alias@icloud.com",
			DeliveredTo: "alias@icloud.com",
			Body:        "Code 20",
		},
		30: {
			SeqNum:      3,
			UID:         30,
			DateStr:     "26-Sep-2026 00:00:00 +0000",
			Subject:     "New Code",
			ToAddr:      "alias@icloud.com",
			DeliveredTo: "alias@icloud.com",
			Body:        "Code 30",
		},
	}

	port, getCommands, stop := spinMockMailServer(t, 3, []uint32{10, 20, 30}, mails)
	defer stop()

	c := createTestClient(t, port)
	defer c.ForceClose()

	receivedMsgs, err := c.FindByRecipientInFolder("alias@icloud.com", "INBOX", 2, 0)
	if err != nil {
		t.Fatalf("FindByRecipientInFolder failed: %v", err)
	}

	if len(receivedMsgs) != 2 {
		t.Fatalf("limit=2 期望返回 2 封邮件, 实际: %d", len(receivedMsgs))
	}
	// 验证 newest-first 顺序
	if receivedMsgs[0].UID != 30 || receivedMsgs[1].UID != 20 {
		t.Fatalf("期望 newest-first 顺序 [UID 30, UID 20], 实际: [%d, %d]", receivedMsgs[0].UID, receivedMsgs[1].UID)
	}

	// 验证网络命令：第二阶段正文拉取单次发出，仅请求 UID 20 和 30，绝不拉取 UID 10
	cmds := getCommands()
	var bodyFetchCmd string
	for _, cmd := range cmds {
		u := strings.ToUpper(cmd)
		if strings.Contains(u, "UID FETCH") && (strings.Contains(u, "BODY.PEEK[]") || strings.Contains(u, "BODY[]")) {
			bodyFetchCmd = cmd
		}
	}
	if bodyFetchCmd == "" {
		t.Fatalf("未观察到第二阶段 BODY FETCH 命令")
	}
	fields := strings.Fields(bodyFetchCmd)
	if len(fields) >= 4 {
		seqsetArg := fields[3]
		if strings.Contains(seqsetArg, "10") && !strings.Contains(seqsetArg, "20") && !strings.Contains(seqsetArg, "30") {
			t.Fatalf("第二阶段绝对不应拉取被 limit 截断的旧邮件 UID 10: %s", seqsetArg)
		}
	}
}

// TestAliasSearch_SinceUIDEnforced 验证 sinceUID 严格过滤：
// 存在 UID 5 和 UID 15，均匹配目标别名；设置 sinceUID = 10：
// 1. UID 5 在第一阶段即被 sinceUID 过滤，不得成为 candidate；
// 2. 第二阶段只对 UID 15 拉取正文。
func TestAliasSearch_SinceUIDEnforced(t *testing.T) {
	mails := map[uint32]mockMailItem{
		5: {
			SeqNum:      1,
			UID:         5,
			DateStr:     "26-Sep-2026 00:00:00 +0000",
			Subject:     "Old UID Mail",
			ToAddr:      "alias@icloud.com",
			DeliveredTo: "alias@icloud.com",
			Body:        "Old body",
		},
		15: {
			SeqNum:      2,
			UID:         15,
			DateStr:     "26-Sep-2026 00:01:00 +0000",
			Subject:     "New UID Mail",
			ToAddr:      "alias@icloud.com",
			DeliveredTo: "alias@icloud.com",
			Body:        "New body",
		},
	}

	port, getCommands, stop := spinMockMailServer(t, 2, []uint32{5, 15}, mails)
	defer stop()

	c := createTestClient(t, port)
	defer c.ForceClose()

	receivedMsgs, err := c.FindByRecipientInFolderSince("alias@icloud.com", "INBOX", 10, 0, 10)
	if err != nil {
		t.Fatalf("FindByRecipientInFolderSince failed: %v", err)
	}

	if len(receivedMsgs) != 1 {
		t.Fatalf("期望命中 1 封邮件, 实际命中 %d 封", len(receivedMsgs))
	}
	if receivedMsgs[0].UID != 15 {
		t.Fatalf("期望命中 UID 15, 实际: %d", receivedMsgs[0].UID)
	}

	// 验证第二阶段网络请求绝未包含 UID 5
	cmds := getCommands()
	for _, cmd := range cmds {
		u := strings.ToUpper(cmd)
		if strings.Contains(u, "UID FETCH") && (strings.Contains(u, "BODY.PEEK[]") || strings.Contains(u, "BODY[]")) {
			fields := strings.Fields(cmd)
			if len(fields) >= 4 {
				if fields[3] == "5" {
					t.Fatalf("sinceUID=10 生效时绝不能对 UID 5 发起正文拉取: %s", cmd)
				}
			}
		}
	}
}

// TestAliasSearch_ImmunityAgainstFromSubjectAndBody 验证第八节红线：
// 1. From 等于目标 alias，但 To 是 unrelated@other.com -> 不匹配；
// 2. Subject 包含 "Mail for target@icloud.com"，但 To 是 unrelated@other.com -> 不匹配；
// 3. Body 明确包含 "target@icloud.com"，但结构化收件人头不匹配 -> 不匹配；
// 4. 绝不能因为 From、Subject 或 Body 命中字串而判定匹配并拉取正文。
func TestAliasSearch_ImmunityAgainstFromSubjectAndBody(t *testing.T) {
	mails := map[uint32]mockMailItem{
		101: {
			SeqNum:      1,
			UID:         101,
			DateStr:     "26-Sep-2026 00:00:00 +0000",
			Subject:     "Normal Subject",
			FromAddr:    "target@icloud.com", // From 等于别名
			ToAddr:      "unrelated@other.com",
			DeliveredTo: "unrelated@other.com",
			Body:        "Body 101",
		},
		102: {
			SeqNum:      2,
			UID:         102,
			DateStr:     "26-Sep-2026 00:01:00 +0000",
			Subject:     "Mail for target@icloud.com", // Subject 包含别名
			FromAddr:    "sender@other.com",
			ToAddr:      "unrelated@other.com",
			DeliveredTo: "unrelated@other.com",
			Body:        "Body 102",
		},
		103: {
			SeqNum:      3,
			UID:         103,
			DateStr:     "26-Sep-2026 00:02:00 +0000",
			Subject:     "Unrelated newsletter",
			FromAddr:    "newsletter@other.com",
			ToAddr:      "unrelated@other.com",
			DeliveredTo: "unrelated@other.com",
			Body:        "Please send feedback to target@icloud.com for support", // Body 包含别名
		},
	}

	port, getCommands, stop := spinMockMailServer(t, 3, []uint32{101, 102, 103}, mails)
	defer stop()

	c := createTestClient(t, port)
	defer c.ForceClose()

	receivedMsgs, err := c.FindByRecipientInFolder("target@icloud.com", "INBOX", 10, 0)
	if err != nil {
		t.Fatalf("FindByRecipientInFolder failed: %v", err)
	}
	if len(receivedMsgs) != 0 {
		t.Fatalf("From, Subject 或 Body 匹配伪阳性防守失败: 期望命中 0 封, 实际命中 %d 封", len(receivedMsgs))
	}

	// 验证未发出任何正文拉取命令
	cmds := getCommands()
	for _, cmd := range cmds {
		u := strings.ToUpper(cmd)
		if strings.Contains(u, "BODY.PEEK[]") || strings.Contains(u, "BODY[]") {
			t.Fatalf("From/Subject/Body 误判防守失败: 不应发出正文拉取命令: %s", cmd)
		}
	}
}

// TestDirectSearch_PartialMetadataFetchFailure_PreservesCandidateCount 验证 FIX-1：
// Direct SEARCH metadata 阶段多封邮件，已确认 1 封 candidate，随后 FETCH 报错中断：
// 1. metadata_fetch_received > 0 (等于 1)
// 2. candidate_count > 0 (等于 1，不因 FETCH error 被清零)
// 3. body_fetch_requested == 0 (未完整成功，绝不进入第二阶段)
// 4. err == true (整个调用正常向调用方返回错误)
func TestDirectSearch_PartialMetadataFetchFailure_PreservesCandidateCount(t *testing.T) {
	mails := map[uint32]mockMailItem{
		101: {
			SeqNum:      1,
			UID:         101,
			DateStr:     "26-Sep-2026 00:00:00 +0000",
			Subject:     "Candidate 1",
			ToAddr:      "target@icloud.com",
			DeliveredTo: "target@icloud.com",
			Body:        "Body 101",
		},
		102: {
			SeqNum:      2,
			UID:         102,
			DateStr:     "26-Sep-2026 00:01:00 +0000",
			Subject:     "Candidate 2",
			ToAddr:      "target@icloud.com",
			DeliveredTo: "target@icloud.com",
			Body:        "Body 102",
		},
	}

	port, getCommands, stop := spinMockMailServer(t, 2, []uint32{101, 102}, mails, withFailUIDMetadataAfter(1))
	defer stop()

	c := createTestClient(t, port)
	defer c.ForceClose()

	oldEnabled := mailPerfEnabled
	oldSink := mailPerfSink
	mailPerfEnabled = true
	var perfKV []any
	mailPerfSink = func(op string, kv ...any) {
		if op == "find_by_recipient" {
			perfKV = kv
		}
	}
	defer func() {
		mailPerfEnabled = oldEnabled
		mailPerfSink = oldSink
	}()

	_, err := c.FindByRecipientInFolder("target@icloud.com", "INBOX", 10, 0)
	if err == nil {
		t.Fatalf("partial metadata fetch 失败时应当返回错误")
	}

	// 验证未进入第二阶段 BODY fetch
	cmds := getCommands()
	for _, cmd := range cmds {
		u := strings.ToUpper(cmd)
		if strings.Contains(u, "BODY.PEEK[]") || strings.Contains(u, "BODY[]") {
			t.Fatalf("metadata fetch 出错时绝不得进入第二阶段 BODY fetch: %s", cmd)
		}
	}

	if perfKV == nil {
		t.Fatalf("未记录 find_by_recipient 性能日志")
	}
	assertKV := map[string]string{
		"metadata_fetch_requested": "2",
		"metadata_fetch_received":  "1",
		"candidate_count":          "1",
		"body_fetch_requested":     "0",
		"body_fetch_received":      "0",
		"err":                      "true",
	}
	for k, want := range assertKV {
		if got, _ := findKV(perfKV, k); got != want {
			t.Errorf("MailPerf %s = %q, want %q", k, got, want)
		}
	}
}

// TestRecentFallback_PartialMetadataFetchFailure_PreservesCandidateCount 验证 FIX-1 在 Fallback 路径的表现：
// 邮箱扫描 10 封，已收到 1 封匹配 candidate 后 FETCH 中断：
// candidate_count 保留为 1，body_fetch_requested=0, err=true。
func TestRecentFallback_PartialMetadataFetchFailure_PreservesCandidateCount(t *testing.T) {
	mails := make(map[uint32]mockMailItem)
	for i := uint32(1); i <= 10; i++ {
		to := fmt.Sprintf("other%d@domain.com", i)
		delivered := to
		if i == 1 {
			delivered = "target@icloud.com"
		}
		mails[i] = mockMailItem{
			SeqNum:      i,
			UID:         i,
			DateStr:     "26-Sep-2026 00:00:00 +0000",
			Subject:     fmt.Sprintf("Mail %d", i),
			ToAddr:      to,
			DeliveredTo: delivered,
			Body:        "Body",
		}
	}

	// searchUIDs 为空直接进 fallback，设置在返回 1 封 metadata 后断开
	port, getCommands, stop := spinMockMailServer(t, 10, nil, mails, withFailFetchMetadataAfter(1))
	defer stop()

	c := createTestClient(t, port)
	defer c.ForceClose()

	oldEnabled := mailPerfEnabled
	oldSink := mailPerfSink
	mailPerfEnabled = true
	var perfKV []any
	mailPerfSink = func(op string, kv ...any) {
		if op == "recent_fallback" {
			perfKV = kv
		}
	}
	defer func() {
		mailPerfEnabled = oldEnabled
		mailPerfSink = oldSink
	}()

	_, err := c.FindByRecipientInFolder("target@icloud.com", "INBOX", 5, 0)
	if err == nil {
		t.Fatalf("partial metadata fetch 失败时应当返回错误")
	}

	cmds := getCommands()
	for _, cmd := range cmds {
		u := strings.ToUpper(cmd)
		if strings.Contains(u, "BODY.PEEK[]") || strings.Contains(u, "BODY[]") {
			t.Fatalf("metadata fetch 出错时绝不得进入第二阶段 BODY fetch: %s", cmd)
		}
	}

	if perfKV == nil {
		t.Fatalf("未记录 recent_fallback 性能日志")
	}
	assertKV := map[string]string{
		"metadata_fetch_requested": "10",
		"metadata_fetch_received":  "1",
		"candidate_count":          "1",
		"body_fetch_requested":     "0",
		"body_fetch_received":      "0",
		"err":                      "true",
	}
	for k, want := range assertKV {
		if got, _ := findKV(perfKV, k); got != want {
			t.Errorf("MailPerf %s = %q, want %q", k, got, want)
		}
	}
}

// TestAliasSearch_MultiHeaderUnionAndNewestFirst (FIX-2) 验证 Alias 搜索遍历全部计划 Header 且不提前 break：
// limit=2
// To SEARCH 命中 UID 10, 20
// Delivered-To SEARCH 命中 UID 30 (最新邮件)
// 必须完成所有 Header SEARCH，Union 并按 newest-first 排序后返回 UID 30, 20，绝不因 To 搜满 limit 漏掉 30。
func TestAliasSearch_MultiHeaderUnionAndNewestFirst(t *testing.T) {
	mails := map[uint32]mockMailItem{
		10: {SeqNum: 1, UID: 10, DateStr: "26-Sep-2026 01:00:00 +0000", Subject: "旧邮件10", ToAddr: "alias@icloud.com", Body: "正文10"},
		20: {SeqNum: 2, UID: 20, DateStr: "26-Sep-2026 02:00:00 +0000", Subject: "较新邮件20", ToAddr: "alias@icloud.com", Body: "正文20"},
		30: {SeqNum: 3, UID: 30, DateStr: "26-Sep-2026 03:00:00 +0000", Subject: "最新邮件30", DeliveredTo: "alias@icloud.com", Body: "正文30"},
	}

	headerUIDs := map[string][]uint32{
		"To":           {10, 20},
		"Delivered-To": {30},
	}

	port, _, stop := spinMockMailServer(t, 3, nil, mails, withSearchHeaderUIDs(headerUIDs))
	defer stop()

	c := createTestClient(t, port)
	defer c.Disconnect()

	msgs, err := c.FindByRecipientInFolder("alias@icloud.com", "INBOX", 2, 7)
	if err != nil {
		t.Fatalf("FindByRecipientInFolder failed: %v", err)
	}

	if len(msgs) != 2 {
		t.Fatalf("期望返回 2 封邮件, 实际返回 %d", len(msgs))
	}

	if msgs[0].UID != 30 {
		t.Errorf("第一封期望是最新邮件 UID 30, 实际得到 UID %d", msgs[0].UID)
	}
	if msgs[1].UID != 20 {
		t.Errorf("第二封期望是 UID 20, 实际得到 UID %d", msgs[1].UID)
	}
}

// TestFolderAll_GlobalNewestFirst (FIX-4) 验证 folder=all 多文件夹聚合按全局 newest-first 排序：
// INBOX 包含旧邮件 (UID 10, 01:00:00)
// Junk 包含更新的邮件 (UID 20, 02:00:00)
// limit=1 时，必须检索全部文件夹候选并全局排序，返回 Junk 中的更新邮件 UID 20，绝不因 INBOX 已达 limit 截断。
func TestFolderAll_GlobalNewestFirst(t *testing.T) {
	inboxMails := map[uint32]mockMailItem{
		10: {SeqNum: 1, UID: 10, DateStr: "26-Sep-2026 01:00:00 +0000", Subject: "旧邮件INBOX", ToAddr: "alias@icloud.com", Body: "正文旧"},
	}
	junkMails := map[uint32]mockMailItem{
		20: {SeqNum: 1, UID: 20, DateStr: "26-Sep-2026 02:00:00 +0000", Subject: "新邮件Junk", ToAddr: "alias@icloud.com", Body: "正文新"},
	}

	folderMails := map[string]map[uint32]mockMailItem{
		"INBOX": inboxMails,
		"Junk":  junkMails,
	}
	folderSearchUIDs := map[string][]uint32{
		"INBOX": {10},
		"Junk":  {20},
	}

	port, _, stop := spinMockMailServer(t, 1, nil, nil,
		withFolderMails(folderMails, folderSearchUIDs),
	)
	defer stop()

	c := createTestClient(t, port)
	defer c.Disconnect()

	msgs, err := c.FindByRecipientInFolder("alias@icloud.com", "all", 1, 7)
	if err != nil {
		t.Fatalf("FindByRecipientInFolder failed: %v", err)
	}

	if len(msgs) != 1 {
		t.Fatalf("期望返回 1 封邮件, 实际返回 %d", len(msgs))
	}

	if msgs[0].UID != 20 {
		t.Errorf("期望返回 Junk 中更新的邮件 UID 20, 实际得到 UID %d", msgs[0].UID)
	}
	if msgs[0].Subject != "新邮件Junk" {
		t.Errorf("期望返回主题「新邮件Junk」, 实际得到 %s", msgs[0].Subject)
	}
}
