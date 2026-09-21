/**
 * [INPUT]: 依赖 github.com/emersion/go-imap, golang.org/x/net/proxy, net/mail
 * [OUTPUT]: 对外提供 Client、NewClient、NewClientWithServer、Message、FullMessage
 * [POS]: internal/mail 的 IMAP 邮件读取客户端核心，支持标准 IMAP 连接复用与外部转寄收件箱
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

// Package mail 实现 iCloud 邮件 IMAP 读取客户端。
//
// 通过 Apple 应用专用密码连接 imap.mail.me.com:993,
// 拉取隐私邮箱别名收到的邮件。对应原 Python 项目 icloud_mail.py。
package mail

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/mail"
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
	ID      string `json:"id"`
	Folder  string `json:"folder,omitempty"`
	From    string `json:"from"`
	To      string `json:"to"`
	Subject string `json:"subject"`
	Date    string `json:"date"`
	Preview string `json:"preview"`
	Unread  *bool  `json:"unread,omitempty"`
	match   string
}

// Folder describes a selectable IMAP mailbox.
type Folder struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

// FullMessage 是一封邮件的完整内容(含正文)。
type FullMessage struct {
	Message
	Body        string `json:"body"`
	ContentType string `json:"content_type"`
}

// Client 是 iCloud 邮件 IMAP 客户端。
type Client struct {
	username string
	password string
	server   string
	port     int
	proxyURL string
	cli      *client.Client
	conn     net.Conn // 底层连接, 用于设置读写截止时间
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

// forceClose 不发 LOGOUT, 直接掐断(坏连接/池丢弃时用)。
func (c *Client) forceClose() {
	if c.cli != nil {
		_ = c.cli.Terminate()
		c.cli = nil
	}
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.conn = nil
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

// ListInbox 拉取收件箱最近 limit 封邮件摘要 (默认检索全部文件夹: INBOX + Junk)。
func (c *Client) ListInbox(limit int, days int) ([]Message, error) {
	return c.ListFolder("all", limit, days)
}

// ListFolder 拉取指定文件夹的最近邮件摘要 (支持 "all"、"inbox"、"junk" 或具体文件夹名)。
func (c *Client) ListFolder(folder string, limit int, days int) ([]Message, error) {
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
		messages, err := c.listMailbox(name, limit, days)
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

func (c *Client) listMailbox(folder string, limit int, days int) ([]Message, error) {
	mbox, err := c.cli.Select(folder, true)
	if err != nil {
		return nil, err
	}
	total := int(mbox.Messages)
	if total == 0 {
		return []Message{}, nil
	}

	from := uint32(1)
	if uint32(limit) < mbox.Messages {
		from = mbox.Messages - uint32(limit) + 1
	}

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

	messages := make(chan *imap.Message, limit)
	done := make(chan error, 1)
	go func() {
		done <- c.cli.Fetch(seqset, items, messages)
	}()

	var out []Message
	for msg := range messages {
		m := toMessageWithBody(msg, folder)
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

// FindByRecipient 查找发给指定隐私邮箱别名的最近 limit 封邮件 (默认通扫 INBOX 与 Junk)。
func (c *Client) FindByRecipient(recipient string, limit int, days int) ([]Message, error) {
	return c.FindByRecipientInFolder(recipient, "all", limit, days)
}

// FindByRecipientInFolder 在指定文件夹查找发给指定别名的最近邮件。
func (c *Client) FindByRecipientInFolder(recipient string, folder string, limit int, days int) ([]Message, error) {
	var out []Message
	err := c.ForEachByRecipientInFolder(recipient, folder, limit, days, func(m Message) bool {
		out = append(out, m)
		return true
	})
	return out, err
}

// ForEachByRecipient 按新→旧遍历发给 recipient 的最近 limit 封邮件 (默认在 all 文件夹查找)。
func (c *Client) ForEachByRecipient(recipient string, limit int, days int, onMsg func(Message) bool) error {
	return c.ForEachByRecipientInFolder(recipient, "all", limit, days, onMsg)
}

// ForEachByRecipientInFolder 在指定文件夹中按新→旧遍历发给 recipient 的邮件。
func (c *Client) ForEachByRecipientInFolder(recipient string, folder string, limit int, days int, onMsg func(Message) bool) error {
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

	for _, name := range folders {
		if err := c.forEachByRecipientInMailbox(recipient, name, limit, days, onMsg); err != nil {
			continue
		}
	}
	return nil
}

func (c *Client) forEachByRecipientInMailbox(recipient string, folder string, limit int, days int, onMsg func(Message) bool) error {
	if _, err := c.cli.Select(folder, true); err != nil {
		return err
	}

	// 1) 服务端按多 Header 检索: To, Delivered-To, X-Original-To, Envelope-To
	headers := []string{"To", "Delivered-To", "X-Original-To", "Envelope-To"}
	var uids []uint32
	for _, header := range headers {
		criteria := imap.NewSearchCriteria()
		criteria.Header.Add(header, recipient)
		if days > 0 {
			criteria.Since = time.Now().AddDate(0, 0, -days)
		}
		found, err := c.cli.UidSearch(criteria)
		if err == nil && len(found) > 0 {
			uids = found
			break
		}
	}

	if len(uids) > 0 {
		uids = newestUIDs(uids, limit)
		for i := len(uids) - 1; i >= 0; i-- {
			m, ferr := c.fetchOneUID(folder, uids[i])
			if ferr != nil {
				return ferr
			}
			if !onMsg(m) {
				return nil
			}
		}
		return nil
	}

	// 2) fallback: 扫最近 N 封信, 本地全文与 Header 深度比对 (解决 Apple 内部转寄重写 To 导致的漏信)
	return c.forEachRecentMatching(folder, recipient, limit, days, onMsg)
}

// newestUIDs 保留 UID 列表中最新的 limit 个(假定 UID 升序)。
func newestUIDs(uids []uint32, limit int) []uint32 {
	if limit <= 0 || len(uids) <= limit {
		return uids
	}
	return uids[len(uids)-limit:]
}

// forEachRecentMatching 拉取 folder 最近 scan 封信件, 本地比对 To/Headers/Body/Subject。
func (c *Client) forEachRecentMatching(folder, recipient string, limit int, days int, onMsg func(Message) bool) error {
	mbox, err := c.cli.Select(folder, true)
	if err != nil {
		return err
	}
	total := int(mbox.Messages)
	if total == 0 {
		return nil
	}
	scan := limit * 4
	if scan < 20 {
		scan = 20
	}
	if scan > 80 {
		scan = 80
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

// GetFull 获取单封邮件的完整内容 (默认 INBOX 与 Junk 自动容错)。
func (c *Client) GetFull(uid uint32) (*FullMessage, error) {
	return c.GetFullInFolder("all", uid)
}

// GetFullInFolder 获取指定文件夹中单封邮件的完整内容。
func (c *Client) GetFullInFolder(folder string, uid uint32) (*FullMessage, error) {
	if c.cli == nil {
		return nil, fmt.Errorf("未连接")
	}
	folders, err := c.resolveFolders(folder)
	if err != nil {
		return nil, err
	}

	for _, name := range folders {
		if _, err := c.cli.Select(name, true); err != nil {
			continue
		}
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
		if err := <-done; err == nil && msg != nil {
			full := &FullMessage{Message: toMessage(msg, name)}
			if r := msg.GetBody(section); r != nil {
				if em, err := mail.ReadMessage(r); err == nil {
					body, _ := readBody(em)
					full.Body = strings.TrimSpace(body)
					full.ContentType = em.Header.Get("Content-Type")
				}
			}
			return full, nil
		}
	}
	return nil, fmt.Errorf("邮件不存在 (uid=%d)", uid)
}

// GetFullBatchInFolder 批量获取指定文件夹中的完整邮件内容 (单次 IMAP 命令往返)。
func (c *Client) GetFullBatchInFolder(folder string, uids []uint32) ([]*FullMessage, error) {
	if c.cli == nil {
		return nil, fmt.Errorf("未连接")
	}
	if len(uids) == 0 {
		return []*FullMessage{}, nil
	}
	if folder == "" || strings.EqualFold(folder, "all") {
		folder = "INBOX"
	}
	if _, err := c.cli.Select(folder, true); err != nil {
		return nil, err
	}

	seqset := new(imap.SeqSet)
	for _, uid := range uids {
		seqset.AddNum(uid)
	}

	section := &imap.BodySectionName{Peek: true}
	items := []imap.FetchItem{imap.FetchUid, imap.FetchEnvelope, imap.FetchInternalDate, imap.FetchFlags, section.FetchItem()}
	messages := make(chan *imap.Message, len(uids))
	done := make(chan error, 1)
	go func() {
		done <- c.cli.UidFetch(seqset, items, messages)
	}()

	var out []*FullMessage
	for msg := range messages {
		if msg == nil {
			continue
		}
		message := toMessage(msg, folder)
		full := &FullMessage{Message: message}
		if r := msg.GetBody(section); r != nil {
			if em, err := mail.ReadMessage(r); err == nil {
				if body, err := readBody(em); err == nil {
					full.Body = strings.TrimSpace(body)
					full.Preview = full.Body
				}
				full.ContentType = em.Header.Get("Content-Type")
			}
		}
		out = append(out, full)
	}
	if err := <-done; err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Date > out[j].Date
	})
	return out, nil
}

// Delete 删除收件箱中指定 UID 的邮件。
func (c *Client) Delete(uid uint32) error {
	return c.DeleteInFolder("INBOX", uid)
}

// DeleteInFolder 删除指定文件夹中指定 UID 的邮件。
func (c *Client) DeleteInFolder(folder string, uid uint32) error {
	if c.cli == nil {
		return fmt.Errorf("未连接")
	}
	if uid == 0 {
		return fmt.Errorf("邮件 UID 无效")
	}
	if folder == "" || strings.EqualFold(folder, "all") {
		folder = "INBOX"
	}
	if _, err := c.cli.Select(folder, false); err != nil {
		return err
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(uid)
	item := imap.FormatFlagsOp(imap.AddFlags, true)
	if err := c.cli.UidStore(seqset, item, []interface{}{imap.DeletedFlag}, nil); err != nil {
		return err
	}
	return c.cli.Expunge(nil)
}

func (c *Client) resolveFolders(folder string) ([]string, error) {
	folder = strings.TrimSpace(folder)
	role := strings.ToLower(folder)
	if folder == "" || role == "inbox" {
		return []string{"INBOX"}, nil
	}

	mailboxes, err := c.ListMailboxes()
	if err != nil {
		if role == "all" {
			return []string{"INBOX", "Junk"}, nil
		}
		if role == "junk" || role == "spam" {
			return []string{"Junk"}, nil
		}
		return []string{folder}, nil
	}

	var names []string
	for _, mbox := range mailboxes {
		switch role {
		case "all":
			if mbox.Role == "inbox" || mbox.Role == "junk" {
				names = append(names, mbox.Name)
			}
		case "junk", "spam":
			if mbox.Role == "junk" {
				names = append(names, mbox.Name)
			}
		default:
			if strings.EqualFold(mbox.Name, folder) || strings.EqualFold(mbox.Role, role) {
				names = append(names, mbox.Name)
			}
		}
	}
	if len(names) == 0 {
		if role == "all" {
			return []string{"INBOX", "Junk"}, nil
		}
		if role == "junk" || role == "spam" {
			return []string{"Junk"}, nil
		}
		return []string{folder}, nil
	}
	return names, nil
}

// dialHTTPConnect 通过 HTTP/HTTPS 代理发起 CONNECT 请求建立到达目标 targetAddr 的裸 TCP 隧道。
func dialHTTPConnect(proxyURL *url.URL, targetAddr string, timeout time.Duration) (net.Conn, error) {
	proxyAddr := proxyURL.Host
	if !strings.Contains(proxyAddr, ":") {
		if strings.EqualFold(proxyURL.Scheme, "https") {
			proxyAddr = net.JoinHostPort(proxyAddr, "443")
		} else {
			proxyAddr = net.JoinHostPort(proxyAddr, "80")
		}
	}

	dialer := &net.Dialer{Timeout: timeout}
	var conn net.Conn
	var err error

	if strings.EqualFold(proxyURL.Scheme, "https") {
		conn, err = tls.DialWithDialer(dialer, "tcp", proxyAddr, &tls.Config{
			ServerName: proxyURL.Hostname(),
		})
	} else {
		conn, err = dialer.Dial("tcp", proxyAddr)
	}
	if err != nil {
		return nil, err
	}

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: targetAddr},
		Host:   targetAddr,
		Header: make(http.Header),
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	if proxyURL.User != nil {
		user := proxyURL.User.Username()
		pass, _ := proxyURL.User.Password()
		auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		req.Header.Set("Proxy-Authorization", "Basic "+auth)
	}

	_ = conn.SetDeadline(time.Now().Add(timeout))
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("HTTP CONNECT 握手失败: HTTP %d %s", resp.StatusCode, resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})

	return conn, nil
}
