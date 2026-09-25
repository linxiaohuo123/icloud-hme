/**
 * [INPUT]: 依赖 fmt, sort, strings, time, github.com/emersion/go-imap
 * [OUTPUT]: 对外提供 ScanPageOptions, ScanPageResult, (*Client).ScanMailboxUIDPage
 * [POS]: internal/mail 的增量 UID 分页扫描核心 (PR-04A F07)，支持固定 upper bound、UID 升序检索、最旧 pageSize 切片与 metadata-first 小标头拉取，接入 MailPerf 观测
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package mail

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap"
)

// ScanPageOptions 定义 UID 增量分页扫描选项 (PR-04A)。
type ScanPageOptions struct {
	Folder           string
	FromUIDInclusive uint32
	ToUIDInclusive   uint32
	PageSize         int
}

// ScanPageResult 是 UID 增量分页扫描的单页结果 (PR-04A F07)。
type ScanPageResult struct {
	UIDValidity uint32
	Messages    []Message // Metadata-first, 包含 UID, Envelope, InternalDate, Flags, 以及从 header 解析出的结构化收件人 (To, Cc, Delivered-To, X-Original-To, Envelope-To)，不包含完整正文
	NextUID     uint32    // 下一个扫描游标 (本页实际最大 UID + 1，或在空页时为 ToUIDInclusive + 1)
	HasMore     bool      // 在指定 [FromUIDInclusive, ToUIDInclusive] 范围内是否还有后续未扫描 UID
}

// ScanMailboxUIDPage 增量顺序扫描指定邮箱指定 UID 范围内的单页邮件元数据 (PR-04A F07)。
// 严格按 UID 升序推进，只取最旧的 PageSize 封邮件，第一阶段仅拉取 Envelope 与结构化收件人 Header，
// 绝不截断丢弃早期未处理 UID，绝不全量拉取邮件正文。
func (c *Client) ScanMailboxUIDPage(opts ScanPageOptions) (res ScanPageResult, retErr error) {
	if c.cli == nil {
		return ScanPageResult{}, fmt.Errorf("未连接")
	}
	folder := strings.TrimSpace(opts.Folder)
	if folder == "" || strings.EqualFold(folder, "all") {
		folder = "INBOX"
	}
	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = 50
	}

	opStart := time.Now()
	var selectMS, searchMS, fetchMS int64
	returned := 0
	folderName := folder
	defer func() {
		LogMailPerf("scan_uid_page",
			"server", c.perfServer(),
			"folder", folderName,
			"from_uid", opts.FromUIDInclusive,
			"to_uid", opts.ToUIDInclusive,
			"page_size", pageSize,
			"select_ms", selectMS,
			"search_ms", searchMS,
			"fetch_ms", fetchMS,
			"messages", returned,
			"total_ms", time.Since(opStart).Milliseconds(),
			"err", retErr != nil,
		)
	}()

	mailboxStart := time.Now()
	mbox, err := c.cli.Select(folder, true)
	selectMS = time.Since(mailboxStart).Milliseconds()
	if err != nil {
		return ScanPageResult{}, err
	}
	if mbox.Messages == 0 {
		return ScanPageResult{
			UIDValidity: mbox.UidValidity,
			NextUID:     opts.ToUIDInclusive + 1,
			HasMore:     false,
		}, nil
	}

	toUID := opts.ToUIDInclusive
	if toUID == 0 && mbox.UidNext > 0 {
		toUID = mbox.UidNext - 1
	}

	if opts.FromUIDInclusive > toUID && toUID > 0 {
		return ScanPageResult{
			UIDValidity: mbox.UidValidity,
			NextUID:     opts.FromUIDInclusive,
			HasMore:     false,
		}, nil
	}

	criteria := imap.NewSearchCriteria()
	criteria.Uid = new(imap.SeqSet)
	criteria.Uid.AddRange(opts.FromUIDInclusive, toUID)

	searchStart := time.Now()
	foundUIDs, searchErr := c.cli.UidSearch(criteria)
	searchMS = time.Since(searchStart).Milliseconds()
	if searchErr != nil {
		return ScanPageResult{}, searchErr
	}
	if len(foundUIDs) == 0 {
		nextUID := toUID + 1
		if nextUID < opts.FromUIDInclusive {
			nextUID = opts.FromUIDInclusive
		}
		return ScanPageResult{
			UIDValidity: mbox.UidValidity,
			NextUID:     nextUID,
			HasMore:     false,
		}, nil
	}

	// 严格按 UID 从旧到新升序排序 (UID ascending)
	sort.Slice(foundUIDs, func(i, j int) bool { return foundUIDs[i] < foundUIDs[j] })

	hasMore := false
	pageUIDs := foundUIDs
	if len(foundUIDs) > pageSize {
		pageUIDs = foundUIDs[:pageSize]
		hasMore = true
	}

	// 下一游标必须根据本页实际最大 UID + 1 推进
	nextUID := pageUIDs[len(pageUIDs)-1] + 1

	// Metadata-first: 仅拉取小数据 (UID, Envelope, InternalDate, Flags 以及收件人相关 Header)
	seqset := new(imap.SeqSet)
	for _, u := range pageUIDs {
		seqset.AddNum(u)
	}

	section := &imap.BodySectionName{
		BodyPartName: imap.BodyPartName{
			Specifier: imap.HeaderSpecifier,
			Fields: []string{
				"To", "Cc", "Delivered-To", "X-Original-To", "Envelope-To", "X-Forwarded-To", "Resent-To", "X-Envelope-To", "Original-Recipient", "X-Apple-Original-To", "X-Apple-Recipient", "Subject", "From",
			},
		},
		Peek: true,
	}

	items := []imap.FetchItem{
		imap.FetchUid,
		imap.FetchEnvelope,
		imap.FetchInternalDate,
		imap.FetchFlags,
		section.FetchItem(),
	}

	messages := make(chan *imap.Message, len(pageUIDs))
	done := make(chan error, 1)
	fetchStart := time.Now()
	go func() {
		done <- c.cli.UidFetch(seqset, items, messages)
	}()

	var fetched []Message
	for msg := range messages {
		if msg == nil {
			continue
		}
		m := toMessageWithHeaderOnly(msg, section, folder)
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
		fetchMS = time.Since(fetchStart).Milliseconds()
		return ScanPageResult{}, err
	}
	fetchMS = time.Since(fetchStart).Milliseconds()
	returned = len(fetched)

	// 保证按 UID 升序排列
	sort.Slice(fetched, func(i, j int) bool { return fetched[i].UID < fetched[j].UID })

	return ScanPageResult{
		UIDValidity: mbox.UidValidity,
		Messages:    fetched,
		NextUID:     nextUID,
		HasMore:     hasMore,
	}, nil
}
