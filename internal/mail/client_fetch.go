/**
 * [INPUT]: 依赖 fmt, net/mail, sort, strings, github.com/emersion/go-imap
 * [OUTPUT]: 对外提供 (*Client).GetFull, (*Client).GetFullInFolder, (*Client).GetFullInFolderWithValidity, (*Client).GetFullBatchInFolder, (*Client).GetFullBatchInFolderWithValidity, (*Client).GetMailboxBoundary, (*Client).Delete, (*Client).DeleteInFolder
 * [POS]: internal/mail 的邮件正文提取与邮箱管理逻辑，支持单封/批量完整内容拉取、UIDVALIDITY 严格校验与邮件物理删除
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package mail

import (
	"fmt"
	"net/mail"
	"sort"
	"strings"

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
func (c *Client) GetFullInFolderWithValidity(folder string, uidValidity uint32, uid uint32) (*FullMessage, error) {
	if c.cli == nil {
		return nil, fmt.Errorf("未连接")
	}
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

			full := &FullMessage{
				Message:      msgModel,
				BodyComplete: true,
				Provider:     "imap",
				Method:       "imap",
			}
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
	return c.GetFullBatchInFolderWithValidity(folder, 0, uids)
}

// GetFullBatchInFolderWithValidity 批量获取指定文件夹中的完整邮件内容，并校验 UIDVALIDITY。
func (c *Client) GetFullBatchInFolderWithValidity(folder string, uidValidity uint32, uids []uint32) ([]*FullMessage, error) {
	if c.cli == nil {
		return nil, fmt.Errorf("未连接")
	}
	if len(uids) == 0 {
		return []*FullMessage{}, nil
	}
	if folder == "" || strings.EqualFold(folder, "all") {
		folder = "INBOX"
	}
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
	go func() {
		done <- c.cli.UidFetch(seqset, items, messages)
	}()

	var out []*FullMessage
	for msg := range messages {
		if msg == nil {
			continue
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
			Message:      message,
			BodyComplete: true,
			Provider:     "imap",
			Method:       "imap",
		}
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
