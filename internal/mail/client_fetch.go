/**
 * [INPUT]: 依赖 fmt, net/mail, sort, strings, time, github.com/emersion/go-imap
 * [OUTPUT]: 对外提供 (*Client).GetFull, (*Client).GetFullInFolder, (*Client).GetFullInFolderWithValidity, (*Client).GetFullBatchInFolder, (*Client).GetFullBatchInFolderWithValidity, (*Client).GetMailboxBoundary, (*Client).Delete, (*Client).DeleteInFolder
 * [POS]: internal/mail 的邮件正文提取与邮箱管理逻辑，保留结构化收件人、严格校验 UIDVALIDITY，批量读取逐项返回正文错误并保留成功邮件
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package mail

import (
	"errors"
	"fmt"
	"net/mail"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap"
)

// GetFull 获取单封邮件的完整内容 (默认 INBOX 与 Junk 自动容错)。
func (c *Client) GetFull(uid uint32) (*FullMessage, error) {
	return c.GetFullInFolder("all", uid)
}

// GetFullInFolder 获取指定文件夹中单封邮件的完整内容。
func (c *Client) GetFullInFolder(folder string, uid uint32) (*FullMessage, error) {
	return c.GetFullInFolderWithValidity(folder, 0, uid)
}

// GetFullInFolderWithValidity 获取指定文件夹中单封邮件的完整内容，支持严格校验 UIDVALIDITY。
func (c *Client) GetFullInFolderWithValidity(folder string, uidValidity uint32, uid uint32) (full *FullMessage, retErr error) {
	if c.cli == nil {
		return nil, fmt.Errorf("未连接")
	}
	opStart := time.Now()
	bodyRequested := 0
	bodyReceived := 0
	defer func() {
		LogMailPerf("get_full", "server", c.perfServer(), "folder", folder, "uid", uid, "body_fetch_requested", bodyRequested, "body_fetch_received", bodyReceived, "total_ms", time.Since(opStart).Milliseconds(), "err", retErr != nil)
	}()
	folders, err := c.resolveFolders(folder)
	if err != nil {
		return nil, err
	}

	for _, name := range folders {
		status, err := c.cli.Select(name, true)
		if err != nil {
			continue
		}
		if uidValidity > 0 && status.UidValidity != uidValidity {
			return nil, ErrUIDValidityMismatch
		}
		seqset := new(imap.SeqSet)
		seqset.AddNum(uid)
		section := &imap.BodySectionName{Peek: true}
		items := []imap.FetchItem{imap.FetchUid, imap.FetchEnvelope, imap.FetchInternalDate, imap.FetchFlags, section.FetchItem()}
		messages := make(chan *imap.Message, 1)
		done := make(chan error, 1)
		// FIX-8: 只统计真正发出 BODY FETCH 的次数 (folder=all 会在多个 folder 依次尝试)
		bodyRequested++
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
			return nil, fmt.Errorf("fetch message body: %w", err)
		}
		if msg != nil {
			msgModel := toMessage(msg, name)
			msgModel.UIDValidity = status.UidValidity
			msgModel.UID = uid
			msgModel.Provider = "imap"
			ref := MessageRef{
				Provider:    "imap",
				Mailbox:     name,
				UIDValidity: status.UidValidity,
				UID:         uid,
			}
			msgModel.MessageRef = ref.Encode()

			full = &FullMessage{
				Message:  msgModel,
				Provider: "imap",
				Method:   "imap",
			}
			if msg.GetBody(section) != nil {
				bodyReceived++
			}
			if err := decodeFullMessageBody(full, msg, section); err != nil {
				return nil, err
			}
			return full, nil
		}
	}
	return nil, fmt.Errorf("邮件不存在 (uid=%d)", uid)
}

// GetFullBatchInFolder 批量获取指定文件夹中的完整邮件内容 (单次 IMAP 命令往返)。
func (c *Client) GetFullBatchInFolder(folder string, uids []uint32) ([]*FullMessage, error) {
	return c.GetFullBatchInFolderWithValidity(folder, 0, uids)
}

// GetFullBatchInFolderWithValidity 批量获取指定文件夹中的完整邮件内容，并校验 UIDVALIDITY。
func (c *Client) GetFullBatchInFolderWithValidity(folder string, uidValidity uint32, uids []uint32) (out []*FullMessage, retErr error) {
	if c.cli == nil {
		return nil, fmt.Errorf("未连接")
	}
	if len(uids) == 0 {
		return []*FullMessage{}, nil
	}
	if folder == "" || strings.EqualFold(folder, "all") {
		folder = "INBOX"
	}
	opStart := time.Now()
	bodyRequested := 0
	bodyReceived := 0
	defer func() {
		LogMailPerf("get_full_batch", "server", c.perfServer(), "folder", folder, "requested", len(uids), "body_fetch_requested", bodyRequested, "body_fetch_received", bodyReceived, "total_ms", time.Since(opStart).Milliseconds(), "err", retErr != nil)
	}()
	status, err := c.cli.Select(folder, true)
	if err != nil {
		return nil, err
	}
	if uidValidity > 0 && status.UidValidity != uidValidity {
		return nil, ErrUIDValidityMismatch
	}

	seqset := new(imap.SeqSet)
	for _, uid := range uids {
		seqset.AddNum(uid)
	}

	section := &imap.BodySectionName{Peek: true}
	items := []imap.FetchItem{imap.FetchUid, imap.FetchEnvelope, imap.FetchInternalDate, imap.FetchFlags, section.FetchItem()}
	messages := make(chan *imap.Message, len(uids))
	done := make(chan error, 1)
	// FIX-8: SELECT / UIDVALIDITY 校验通过、真正即将执行 UidFetch 时才计入 requested
	bodyRequested = len(uids)
	go func() {
		done <- c.cli.UidFetch(seqset, items, messages)
	}()

	var failures []MessageReadFailure
	for msg := range messages {
		if msg == nil {
			continue
		}
		if msgHasBodySection(msg) {
			bodyReceived++
		}
		message := toMessage(msg, folder)
		message.UIDValidity = status.UidValidity
		message.UID = msg.Uid
		message.Provider = "imap"
		ref := MessageRef{
			Provider:    "imap",
			Mailbox:     folder,
			UIDValidity: status.UidValidity,
			UID:         msg.Uid,
		}
		message.MessageRef = ref.Encode()

		full := &FullMessage{
			Message:  message,
			Provider: "imap",
			Method:   "imap",
		}
		if err := decodeFullMessageBody(full, msg, section); err != nil {
			failures = append(failures, MessageReadFailure{
				Ref: ref, Err: err, Permanent: !errors.Is(err, ErrMissingMessageBody),
			})
			// Drain FETCH before returning so the pooled connection stays usable.
			continue
		}
		full.Preview = full.Body
		out = append(out, full)
	}
	if err := <-done; err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Date > out[j].Date
	})
	if len(failures) > 0 {
		return out, &BatchReadError{Failures: failures}
	}
	return out, nil
}

// decodeFullMessageBody only marks a message complete after its body was read
// and decoded. Missing bodies remain retryable; malformed content is reported per message.
func decodeFullMessageBody(full *FullMessage, msg *imap.Message, section *imap.BodySectionName) error {
	r := msg.GetBody(section)
	if r == nil {
		return fmt.Errorf("%w (uid=%d)", ErrMissingMessageBody, msg.Uid)
	}
	em, err := mail.ReadMessage(r)
	if err != nil {
		// net/mail errors may embed the offending header, including private data.
		return fmt.Errorf("parse message headers (uid=%d): invalid or unreadable headers", msg.Uid)
	}
	body, err := readBody(em)
	if err != nil {
		return fmt.Errorf("decode message body (uid=%d): %w", msg.Uid, err)
	}
	full.match = strings.Join(extractStructuralRecipients(full.To, em.Header), "\n")
	full.Body = strings.TrimSpace(body)
	full.ContentType = em.Header.Get("Content-Type")
	full.BodyComplete = true
	return nil
}

// GetMailboxBoundary 获取指定邮箱的 UIDVALIDITY 与 UIDNEXT 基线边界 (PR-06 Section 9.2)。
func (c *Client) GetMailboxBoundary(folder string) (uint32, uint32, error) {
	if c.cli == nil {
		return 0, 0, fmt.Errorf("未连接")
	}
	if folder == "" || strings.EqualFold(folder, "all") {
		folder = "INBOX"
	}
	status, err := c.cli.Select(folder, true)
	if err != nil {
		return 0, 0, err
	}
	return status.UidValidity, status.UidNext, nil
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
			if strings.EqualFold(mbox.Name, folder) || strings.EqualFold(mbox.DisplayName, folder) || strings.EqualFold(mbox.Role, role) {
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
