/**
 * [INPUT]: 依赖 github.com/emersion/go-imap, golang.org/x/net/proxy
 * [OUTPUT]: 对外提供 Client、NewClient、NewClientWithServer、Message、FullMessage
 * [POS]: internal/mail 的 IMAP 邮件读取客户端核心，连接建立与列表搜索；内容拉取由 client_fetch.go 承载，隧道拨号由 dial.go 承载
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

// Package mail 实现 iCloud 邮件 IMAP 读取客户端。
//
// 通过 Apple 应用专用密码连接 imap.mail.me.com:993,
// 拉取隐私邮箱别名收到的邮件。对应原 Python 项目 icloud_mail.py。
package mail

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"golang.org/x/net/proxy"
)

const (
	IMAPServer = "imap.mail.me.com"
	IMAPPort   = 993

	// IMAP 命令无内建超时: 不设截止时间的话, Apple IMAP 偶发挂起会永久占死
	// 该账号的连接池并堆积 goroutine。拨号/握手/登录/单批命令全部限时。
	//
	// 导出供连接池与外部转寄邮箱(QQ/163 等)链路统一复用。
	IMAPDialTimeout    = 10 * time.Second
	IMAPCommandTimeout = 90 * time.Second
)

// Message 是一封邮件的摘要信息。
type Message struct {
	ID          string `json:"id"`
	AccountID   string `json:"account_id,omitempty"`  // 归属母号 ID
	MessageRef  string `json:"message_ref,omitempty"` // 规范化全局唯一邮件引用
	Folder      string `json:"folder,omitempty"`
	From        string `json:"from"`
	To          string `json:"to"`
	Subject     string `json:"subject"`
	Date        string `json:"date"`
	Preview     string `json:"preview"`
	Unread      *bool  `json:"unread,omitempty"`
	Provider    string `json:"provider,omitempty"`     // "imap" 或 "webmail"
	UIDValidity uint32 `json:"uid_validity,omitempty"` // IMAP 邮箱 UIDVALIDITY
	UID         uint32 `json:"uid,omitempty"`          // IMAP UID
	ThreadID    string `json:"thread_id,omitempty"`    // WebMail 线程 ID
	match       string
}

// Folder describes a selectable IMAP mailbox.
type Folder struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

// FullMessage 是一封邮件的完整内容(含正文)。
type FullMessage struct {
	Message
	Body         string `json:"body"`
	ContentType  string `json:"content_type"`
	BodyComplete bool   `json:"body_complete"`      // 是否为完整邮件正文 (WebMail 预览为 false)
	Provider     string `json:"provider,omitempty"` // 实际数据来源: "imap" 或 "webmail"
	Method       string `json:"method,omitempty"`   // 实际调用方式: "imap" 或 "web_api"
}

// Client 是 iCloud 邮件 IMAP 客户端。
type Client struct {
	username       string
	password       string
	server         string
	port           int
	proxyURL       string
	cli            *client.Client
	conn           net.Conn // 底层连接, 用于设置读写截止时间
	curMailbox     string
	curUIDValidity uint32
}

// SetDeadline 给底层连接设置绝对读写截止时间; 零值清除。
// 到期触发 i/o timeout, 连接会被连接池判定为坏连接丢弃重建。
func (c *Client) SetDeadline(t time.Time) {
	if c.conn != nil {
		_ = c.conn.SetDeadline(t)
	}
}

// NewClient 创建 IMAP 客户端。需在调用其它方法前先 Connect。
func NewClient(appleID, appPassword string) *Client {
	return NewClientWithServer(appleID, appPassword, IMAPServer, IMAPPort)
}

// NewClientWithProxy 创建携带 SOCKS5 代理的 IMAP 客户端。
func NewClientWithProxy(appleID, appPassword, proxyURL string) *Client {
	c := NewClientWithServer(appleID, appPassword, IMAPServer, IMAPPort)
	c.proxyURL = strings.TrimSpace(proxyURL)
	return c
}

// NewClientWithServer creates an IMAP client for a custom server.
func NewClientWithServer(username, password, server string, port int) *Client {
	return &Client{username: username, password: password, server: server, port: port}
}

// NewClientForTesting 供测试构造已初始化的合法 Client，支持在本地测试中通过生产 Ping 检查
func NewClientForTesting(appleID, appPassword string, conn net.Conn, cli *client.Client) *Client {
	return &Client{
		username: appleID,
		password: appPassword,
		conn:     conn,
		cli:      cli,
	}
}

// SetProxy 设置或更新代理配置。
func (c *Client) SetProxy(proxyURL string) {
	c.proxyURL = strings.TrimSpace(proxyURL)
}

// Connect 连接并登录 IMAP 服务器。已连接且存活时直接复用。
func (c *Client) Connect() error {
	if c.cli != nil {
		c.SetDeadline(time.Now().Add(IMAPCommandTimeout))
		err := c.cli.Noop()
		c.SetDeadline(time.Time{})
		if err == nil {
			return nil
		}
		c.forceClose()
	}
	addr := net.JoinHostPort(c.server, strconv.Itoa(c.port))
	var rawConn net.Conn
	var err error

	if c.proxyURL != "" {
		u, parseErr := url.Parse(c.proxyURL)
		if parseErr != nil {
			return fmt.Errorf("解析 IMAP 代理失败: %w", parseErr)
		}
		if strings.EqualFold(u.Scheme, "socks5h") {
			u.Scheme = "socks5"
		}
		if strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https") {
			rawConn, err = dialHTTPConnect(u, addr, IMAPDialTimeout)
			if err != nil {
				return fmt.Errorf("通过 HTTP 代理连接 IMAP 失败: %w", err)
			}
		} else {
			dialer, proxyErr := proxy.FromURL(u, &net.Dialer{Timeout: IMAPDialTimeout})
			if proxyErr != nil {
				return fmt.Errorf("创建 IMAP 代理拨号器失败: %w", proxyErr)
			}
			rawConn, err = dialer.Dial("tcp", addr)
			if err != nil {
				return fmt.Errorf("通过代理连接 IMAP 失败: %w", err)
			}
		}
	} else {
		rawConn, err = net.DialTimeout("tcp", addr, IMAPDialTimeout)
		if err != nil {
			return fmt.Errorf("IMAP 连接失败: %w", err)
		}
	}

	tlsConn := tls.Client(rawConn, &tls.Config{ServerName: c.server})
	_ = rawConn.SetDeadline(time.Now().Add(IMAPDialTimeout))
	if err := tlsConn.Handshake(); err != nil {
		_ = rawConn.Close()
		return fmt.Errorf("IMAP TLS 握手失败: %w", err)
	}
	cli, err := client.New(tlsConn)
	if err != nil {
		_ = rawConn.Close()
		return fmt.Errorf("IMAP 初始化失败: %w", err)
	}
	_ = rawConn.SetDeadline(time.Now().Add(IMAPCommandTimeout))
	if err := cli.Login(c.username, c.password); err != nil {
		_ = cli.Logout()
		_ = rawConn.Close()
		return fmt.Errorf("IMAP 登录失败 — 请检查邮箱账号、授权码和服务器地址: %w", err)
	}
	_ = rawConn.SetDeadline(time.Time{})
	c.conn = tlsConn
	c.cli = cli
	return nil
}

// Ping 探测连接是否仍可用(NOOP)。
func (c *Client) Ping() error {
	if c.cli == nil {
		return fmt.Errorf("未连接")
	}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	defer c.SetDeadline(time.Time{})
	return c.cli.Noop()
}

// Disconnect 登出并关闭连接。
//
// 【资源红线】go-imap 的 Logout() 只发送 LOGOUT 命令，真正的 conn.Close() 发生在
// 服务端回 BYE 的分支里。若服务器不回 BYE(假死/半开)，Logout 超时返回后 reader
// 协程会永久阻塞在 ReadResp 上，且持有 conn 引用使 finalizer 无法回收。
// 因此这里必须先保留 deadline(不清零)，并在 Logout 失败时强制 Terminate。
func (c *Client) Disconnect() {
	if c.cli != nil {
		c.SetDeadline(time.Now().Add(IMAPCommandTimeout))
		if err := c.cli.Logout(); err != nil {
			_ = c.cli.Terminate()
		}
		c.SetDeadline(time.Time{})
		c.cli = nil
	}
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.conn = nil
}

// ForceClose 不发 LOGOUT, 直接掐断底层网络连接 (坏连接/取消时用，Issue 13)。
func (c *Client) ForceClose() {
	c.forceClose()
}

// forceClose 不发 LOGOUT, 直接掐断底层网络连接(坏连接/池丢弃时用)。
// 注意：严禁在此处将 c.cli 或 c.conn 置为 nil，因为异步超时中断协程会并发调用此方法；
// 提前置空会导致正在进行中的 IMAP 方法发生 nil pointer dereference panic。
// 掐断底层的 net.Conn 即可使所有阻塞读写安全报错返回。
func (c *Client) forceClose() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
	if c.cli != nil {
		_ = c.cli.Terminate()
	}
}

// InboxCount 返回收件箱邮件总数。
func (c *Client) InboxCount() (int, error) {
	if c.cli == nil {
		return 0, fmt.Errorf("未连接")
	}
	mbox, err := c.cli.Select("INBOX", false)
	if err != nil {
		return 0, err
	}
	return int(mbox.Messages), nil
}

// ListMailboxes returns selectable folders with normalized roles.
func (c *Client) ListMailboxes() ([]Folder, error) {
	if c.cli == nil {
		return nil, fmt.Errorf("未连接")
	}

	ch := make(chan *imap.MailboxInfo, 32)
	done := make(chan error, 1)
	go func() {
		done <- c.cli.List("", "*", ch)
	}()

	var folders []Folder
	for info := range ch {
		if hasAttr(info.Attributes, imap.NoSelectAttr) {
			continue
		}
		folders = append(folders, Folder{
			Name: info.Name,
			Role: folderRole(info.Name, info.Attributes),
		})
	}
	if err := <-done; err != nil {
		return nil, err
	}

	sort.SliceStable(folders, func(i, j int) bool {
		return folderSortRank(folders[i]) < folderSortRank(folders[j])
	})
	return folders, nil
}

// ListInbox 拉取收件箱最近 limit 封邮件摘要 (默认不拉正文，毫秒级响应)。
func (c *Client) ListInbox(limit int, days int) ([]Message, error) {
	return c.ListFolder("inbox", limit, days)
}

// ListInboxWithBodies 拉取收件箱最近邮件并解析正文 (供 OTP 识别)。
func (c *Client) ListInboxWithBodies(limit int, days int) ([]Message, error) {
	return c.ListFolderWithBodies("inbox", limit, days)
}

// ListFolder 拉取指定文件夹的最近邮件摘要 (支持 "all"、"inbox"、"junk" 或具体文件夹名，默认不拉正文)。
func (c *Client) ListFolder(folder string, limit int, days int) ([]Message, error) {
	return c.listFolder(folder, limit, days, 0, false)
}

// ListFolderWithBodies 拉取指定文件夹的最近邮件并拉取正文。
func (c *Client) ListFolderWithBodies(folder string, limit int, days int) ([]Message, error) {
	return c.listFolder(folder, limit, days, 0, true)
}

// ListFolderSince 拉取指定文件夹中 UID >= sinceUID 的邮件 (支持带/不带正文)。
func (c *Client) ListFolderSince(folder string, limit int, days int, sinceUID uint32, includeBody bool) ([]Message, error) {
	return c.listFolder(folder, limit, days, sinceUID, includeBody)
}

func (c *Client) listFolder(folder string, limit int, days int, sinceUID uint32, includeBody bool) ([]Message, error) {
	if c.cli == nil {
		return nil, fmt.Errorf("未连接")
	}
	if limit <= 0 {
		limit = 50
	}

	folders, err := c.resolveFolders(folder)
	if err != nil {
		return nil, err
	}

	var all []Message
	var folderErrors []error
	for _, name := range folders {
		messages, err := c.listMailbox(name, limit, days, sinceUID, includeBody)
		if err != nil {
			folderErrors = append(folderErrors, fmt.Errorf("%s: %w", name, err))
			continue
		}
		all = append(all, messages...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Date > all[j].Date })
	if len(all) > limit {
		all = all[:limit]
	}
	return all, errors.Join(folderErrors...)
}

func (c *Client) listMailbox(folder string, limit int, days int, sinceUID uint32, includeBody bool) ([]Message, error) {
	mbox, err := c.cli.Select(folder, true)
	if err != nil {
		return nil, err
	}
	total := int(mbox.Messages)
	if total == 0 {
		return []Message{}, nil
	}

	seqset := new(imap.SeqSet)
	isUID := false
	if sinceUID > 0 {
		if mbox.UidNext > 0 && sinceUID >= mbox.UidNext {
			return []Message{}, nil
		}
		criteria := imap.NewSearchCriteria()
		criteria.Uid = new(imap.SeqSet)
		criteria.Uid.AddRange(sinceUID, 0)
		if days > 0 {
			criteria.Since = time.Now().AddDate(0, 0, -days)
		}
		foundUIDs, searchErr := c.cli.UidSearch(criteria)
		if searchErr != nil {
			return nil, searchErr
		}
		if len(foundUIDs) == 0 {
			return []Message{}, nil
		}
		sort.Slice(foundUIDs, func(i, j int) bool { return foundUIDs[i] < foundUIDs[j] })
		if limit > 0 && len(foundUIDs) > limit {
			foundUIDs = newestUIDs(foundUIDs, limit)
		}
		for _, u := range foundUIDs {
			seqset.AddNum(u)
		}
		isUID = true
	} else {
		from := uint32(1)
		if uint32(limit) < mbox.Messages {
			from = mbox.Messages - uint32(limit) + 1
		}
		seqset.AddRange(from, mbox.Messages)
	}

	items := []imap.FetchItem{
		imap.FetchUid,
		imap.FetchEnvelope,
		imap.FetchInternalDate,
		imap.FetchFlags,
	}
	parser := toMessage
	if includeBody {
		section := &imap.BodySectionName{Peek: true}
		items = append(items, section.FetchItem())
		parser = toMessageWithBody
	}

	messages := make(chan *imap.Message, limit)
	done := make(chan error, 1)
	go func() {
		if isUID {
			done <- c.cli.UidFetch(seqset, items, messages)
		} else {
			done <- c.cli.Fetch(seqset, items, messages)
		}
	}()

	var out []Message
	for msg := range messages {
		m := parser(msg, folder)
		if sinceUID > 0 && m.UID < sinceUID {
			continue
		}
		m.UIDValidity = mbox.UidValidity
		m.Provider = "imap"
		ref := MessageRef{
			Provider:    "imap",
			Mailbox:     folder,
			UIDValidity: mbox.UidValidity,
			UID:         m.UID,
		}
		m.MessageRef = ref.Encode()

		if days > 0 {
			if t, err := time.Parse(time.RFC3339, m.Date); err == nil {
				if time.Since(t) > time.Duration(days)*24*time.Hour {
					continue
				}
			}
		}
		out = append(out, m)
	}
	if err := <-done; err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	return out, nil
}

// FindByRecipient 查找发给指定隐私邮箱别名的最近 limit 封邮件 (默认检索 inbox 文件夹)。
func (c *Client) FindByRecipient(recipient string, limit int, days int) ([]Message, error) {
	return c.FindByRecipientInFolder(recipient, "inbox", limit, days)
}

// FindByRecipientInFolder 在指定文件夹查找发给指定别名的最近邮件。
func (c *Client) FindByRecipientInFolder(recipient string, folder string, limit int, days int) ([]Message, error) {
	var out []Message
	err := c.ForEachByRecipientInFolder(recipient, folder, limit, days, func(m Message) bool {
		out = append(out, m)
		return len(out) < limit
	})
	return out, err
}

// FindByRecipientInFolderSince 在指定文件夹中按 UID lower bound 增量查找邮件 (Issue 14)。
func (c *Client) FindByRecipientInFolderSince(recipient string, folder string, limit int, days int, sinceUID uint32) ([]Message, error) {
	var out []Message
	err := c.ForEachByRecipientInFolderSince(recipient, folder, limit, days, sinceUID, func(m Message) bool {
		out = append(out, m)
		return len(out) < limit
	})
	return out, err
}

// ForEachByRecipient 按新→旧遍历发给 recipient 的最近 limit 封邮件 (默认在 inbox 文件夹查找)。
func (c *Client) ForEachByRecipient(recipient string, limit int, days int, onMsg func(Message) bool) error {
	return c.ForEachByRecipientInFolder(recipient, "inbox", limit, days, onMsg)
}

// ForEachByRecipientInFolder 在指定文件夹中按新→旧遍历发给 recipient 的邮件。
func (c *Client) ForEachByRecipientInFolder(recipient string, folder string, limit int, days int, onMsg func(Message) bool) error {
	return c.ForEachByRecipientInFolderSince(recipient, folder, limit, days, 0, onMsg)
}

// ForEachByRecipientInFolderSince 在指定文件夹中按 UID lower bound 增量遍历邮件 (Issue 14)。
func (c *Client) ForEachByRecipientInFolderSince(recipient string, folder string, limit int, days int, sinceUID uint32, onMsg func(Message) bool) error {
	if c.cli == nil {
		return fmt.Errorf("未连接")
	}
	if onMsg == nil {
		return fmt.Errorf("onMsg 不能为空")
	}
	if limit <= 0 {
		limit = 5
	}

	folders, err := c.resolveFolders(folder)
	if err != nil {
		return err
	}

	var folderErrors []string
	successFolders := 0
	stop := false
	wrappedOnMsg := func(m Message) bool {
		cont := onMsg(m)
		if !cont {
			stop = true
		}
		return cont
	}
	for _, name := range folders {
		if stop {
			break
		}
		if err := c.forEachByRecipientInMailbox(recipient, name, limit, days, sinceUID, wrappedOnMsg); err != nil {
			folderErrors = append(folderErrors, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		successFolders++
	}
	if len(folders) > 0 && successFolders == 0 {
		return fmt.Errorf("所有文件夹检索均失败: %s", strings.Join(folderErrors, "; "))
	}
	return nil
}

func (c *Client) forEachByRecipientInMailbox(recipient string, folder string, limit int, days int, sinceUID uint32, onMsg func(Message) bool) error {
	mbox, err := c.cli.Select(folder, true)
	if err != nil {
		return err
	}

	// 1) 服务端按 Header 检索并 Union 去重
	// QQ 邮箱服务端不支持 Delivered-To 等非标 Header，强行搜索会导致全箱扫描并返回数千 UID 造成网络浪费与延迟。
	// 因此对 QQ 邮箱仅搜索标准 To 标头，未命中时秒级穿透至本地快速比对。
	headers := []string{"To"}
	if !strings.Contains(strings.ToLower(c.server), "qq.com") {
		headers = append(headers, "Delivered-To", "X-Original-To", "Envelope-To")
	}

	seen := make(map[uint32]struct{})
	var allUIDs []uint32
	for _, header := range headers {
		criteria := imap.NewSearchCriteria()
		criteria.Header.Add(header, recipient)
		if days > 0 {
			criteria.Since = time.Now().AddDate(0, 0, -days)
		}
		if sinceUID > 0 {
			criteria.Uid = new(imap.SeqSet)
			criteria.Uid.AddRange(sinceUID, 0) // IMAP: UID sinceUID:*
		}
		found, err := c.cli.UidSearch(criteria)
		if err == nil {
			// 防御非标准 IMAP 服务器: 当请求不存在的标头时，若错误返回全箱邮件，果断丢弃并直接短路退出，
			// 避免继续尝试其它非标标头带来多轮无谓网络往返与耗时。
			if mbox.Messages > 5 && len(found) >= int(mbox.Messages) {
				break
			}
			for _, u := range found {
				if _, ok := seen[u]; !ok {
					seen[u] = struct{}{}
					allUIDs = append(allUIDs, u)
				}
			}
		}
		// 性能关键优化：如果当前已搜出足够数量的 UID（>= limit），立即短路返回
		if len(allUIDs) >= limit {
			break
		}
	}
	sort.Slice(allUIDs, func(i, j int) bool { return allUIDs[i] < allUIDs[j] })
	uids := allUIDs

	if len(uids) > 0 {
		if sinceUID == 0 {
			uids = newestUIDs(uids, limit)
		}
		seqset := new(imap.SeqSet)
		for _, u := range uids {
			seqset.AddNum(u)
		}
		section := &imap.BodySectionName{Peek: true}
		items := []imap.FetchItem{imap.FetchUid, imap.FetchEnvelope, imap.FetchInternalDate, imap.FetchFlags, section.FetchItem()}
		messages := make(chan *imap.Message, len(uids))
		done := make(chan error, 1)
		go func() {
			done <- c.cli.UidFetch(seqset, items, messages)
		}()

		var fetched []Message
		for msg := range messages {
			if msg == nil {
				continue
			}
			m := toMessageWithBody(msg, folder)
			m.UIDValidity = mbox.UidValidity
			m.UID = msg.Uid
			m.Provider = "imap"
			ref := MessageRef{
				Provider:    "imap",
				Mailbox:     folder,
				UIDValidity: mbox.UidValidity,
				UID:         m.UID,
			}
			m.MessageRef = ref.Encode()
			fetched = append(fetched, m)
		}
		if err := <-done; err != nil {
			return err
		}
		// 按 UID 从大到小 (新到旧) 排序触发回调；严格核验收件人匹配
		sort.SliceStable(fetched, func(i, j int) bool { return fetched[i].UID > fetched[j].UID })
		matchedCount := 0
		for _, m := range fetched {
			if !m.matches(recipient) {
				continue
			}
			matchedCount++
			if !onMsg(m) {
				return nil
			}
		}
		// 只有当至少命中一封真正匹配目标别名的邮件时才提前结束；
		// 若因非标 IMAP 返回了无关历史邮件导致 matchedCount == 0，必须穿透执行 fallback 深度比对
		if matchedCount > 0 {
			return nil
		}
	}

	// 2) fallback: 扫最近 N 封信, 本地全文与 Header 深度比对 (解决 Apple 内部转寄重写 To 导致的漏信)
	return c.forEachRecentMatching(folder, recipient, limit, days, sinceUID, onMsg)
}

// newestUIDs 保留 UID 列表中最新的 limit 个(假定 UID 升序)。
func newestUIDs(uids []uint32, limit int) []uint32 {
	if limit <= 0 || len(uids) <= limit {
		return uids
	}
	return uids[len(uids)-limit:]
}

// forEachRecentMatching 拉取 folder 最近 scan 封信件, 本地比对 To/Headers/Body/Subject。
func (c *Client) forEachRecentMatching(folder, recipient string, limit int, days int, sinceUID uint32, onMsg func(Message) bool) error {
	mbox, err := c.cli.Select(folder, true)
	if err != nil {
		return err
	}
	total := int(mbox.Messages)
	if total == 0 {
		return nil
	}
	scan := limit * 3
	if scan < 10 {
		scan = 10
	}
	if scan > 30 {
		scan = 30
	}
	if scan > total {
		scan = total
	}
	from := mbox.Messages - uint32(scan) + 1
	seqset := new(imap.SeqSet)
	seqset.AddRange(from, mbox.Messages)

	section := &imap.BodySectionName{Peek: true}
	items := []imap.FetchItem{
		imap.FetchUid,
		imap.FetchEnvelope,
		imap.FetchInternalDate,
		imap.FetchFlags,
		section.FetchItem(),
	}
	messages := make(chan *imap.Message, scan)
	done := make(chan error, 1)
	go func() {
		done <- c.cli.Fetch(seqset, items, messages)
	}()

	var cands []Message
	for msg := range messages {
		if msg == nil {
			continue
		}
		m := toMessageWithBody(msg, folder)
		if days > 0 {
			if t, err := time.Parse(time.RFC3339, m.Date); err == nil {
				if time.Since(t) > time.Duration(days)*24*time.Hour {
					continue
				}
			}
		}
		if sinceUID > 0 && m.UID < sinceUID {
			continue
		}
		if m.matches(recipient) {
			cands = append(cands, m)
		}
	}
	if err := <-done; err != nil {
		return err
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Date > cands[j].Date })
	for i, m := range cands {
		if i >= limit {
			break
		}
		if !onMsg(m) {
			return nil
		}
	}
	return nil
}

// fetchOneUID 拉取单封邮件(含 body preview), 使用 BODY.PEEK 不标已读。
func (c *Client) fetchOneUID(folder string, uid uint32) (Message, error) {
	seqset := new(imap.SeqSet)
	seqset.AddNum(uid)
	section := &imap.BodySectionName{Peek: true}
	items := []imap.FetchItem{imap.FetchUid, imap.FetchEnvelope, imap.FetchInternalDate, imap.FetchFlags, section.FetchItem()}
	messages := make(chan *imap.Message, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.cli.UidFetch(seqset, items, messages)
	}()
	var msg *imap.Message
	for m := range messages {
		if msg == nil && m != nil {
			msg = m
		}
	}
	if err := <-done; err != nil {
		return Message{}, err
	}
	if msg == nil {
		return Message{}, fmt.Errorf("邮件不存在 (uid=%d)", uid)
	}
	return toMessageWithBody(msg, folder), nil
}
