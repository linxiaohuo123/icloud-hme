/**
 * [INPUT]: 依赖 internal/mail, internal/account
 * [OUTPUT]: 对外提供 managerBackend 的邮件收发与邮箱管理方法 (ListInbox, ListMailboxes, GetMessage, GetMessages, DeleteMessage, ScanMailboxUIDPage, GetMailboxBoundaryContext)、parseMessageID 与 InboxQuery, InboxResult, ScanPageQuery, ScanPageResult, MessageRef 类型
 * [POS]: internal/server 的邮件业务门面实现；IMAP folder:uid 与 WebMail ThreadID 分流，批量详情降级尽力返回 WebMail 列表
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"icloud-hme/internal/mail"
)

// InboxQuery 是收件箱查询参数。
type InboxQuery struct {
	AccountID       string
	Alias           string
	Folder          string
	Limit           int
	Days            int
	WithBody        bool
	FolderSpecified bool
	DaysSpecified   bool
	SinceUID        uint32
	Refresh         bool
}

// InboxResult 是收件箱查询结果。
type InboxResult struct {
	AccountID string         `json:"account_id"`
	Alias     string         `json:"alias,omitempty"`
	Folder    string         `json:"folder,omitempty"`
	Count     int            `json:"count"`
	Messages  []mail.Message `json:"messages"`
	Method    string         `json:"method"`
}

// ScanPageQuery 定义内部专用增量分页扫描参数 (PR-04A F07)。
type ScanPageQuery struct {
	AccountID        string
	Folder           string
	UIDValidity      uint32
	FromUIDInclusive uint32
	ToUIDInclusive   uint32
	PageSize         int
}

// ScanPageResult 定义内部专用增量分页扫描结果 (PR-04A F07)。
type ScanPageResult struct {
	UIDValidity uint32         `json:"uid_validity"`
	Messages    []mail.Message `json:"messages"`
	NextUID     uint32         `json:"next_uid"`
	HasMore     bool           `json:"has_more"`
}

// MessageRef 别名映射至 mail.MessageRef，确保统一规范身份
type MessageRef = mail.MessageRef

// webMailLookupLimit 与 listInboxHandler 的 limit 上限对齐，避免详情回退只扫前 50 封漏信。
const webMailLookupLimit = 100

// parseMessageID 解析邮件 ID。
//
// 仅当冒号后缀能解析为 uint32、且前缀非空时，才视为 IMAP 的 folder:uid；
// 否则整串当作 WebMail ThreadID（允许含冒号），禁止把 "thread:abc" 误切成空 UID。
func parseMessageID(rawID string) (folder, idPart string, uid uint32, hasUID bool) {
	rawID = strings.TrimSpace(rawID)
	if rawID == "" {
		return "", "", 0, false
	}
	if i := strings.IndexByte(rawID, ':'); i > 0 && i < len(rawID)-1 {
		left, right := rawID[:i], rawID[i+1:]
		if u, err := strconv.ParseUint(right, 10, 32); err == nil {
			return left, right, uint32(u), true
		}
	}
	if u, err := strconv.ParseUint(rawID, 10, 32); err == nil {
		return "", rawID, uint32(u), true
	}
	return "", rawID, 0, false
}

// webMailIDMatch 只做精确相等，禁止 strings.Contains：空 idPart 或短数字会命中所有 ThreadID。
func webMailIDMatch(messageID, rawID, idPart string) bool {
	if messageID == "" {
		return false
	}
	if messageID == rawID {
		return true
	}
	return idPart != "" && idPart != rawID && messageID == idPart
}

// ListInboxContext 读取收件箱摘要 (支持 context 上下文超时与真实底层连接中断，支持 SinceUID 增量游标，Issue 13 & 14)。
func (b *managerBackend) ListInboxContext(ctx context.Context, q InboxQuery) (InboxResult, error) {
	if err := ctx.Err(); err != nil {
		return InboxResult{}, err
	}
	if q.Folder == "" {
		q.Folder = "inbox"
	}
	// 优先使用 IMAP 连接池 (App Password 认证, 复用长连接, 严格绑定与传递 ctx)
	var imapMessages []mail.Message
	poolErr := b.mgr.WithMailClientContext(ctx, q.AccountID, func(mc *mail.Client) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		var e error
		if q.Alias != "" {
			if q.SinceUID > 0 {
				imapMessages, e = mc.FindByRecipientInFolderSince(q.Alias, q.Folder, q.Limit, q.Days, q.SinceUID)
			} else {
				imapMessages, e = mc.FindByRecipientInFolder(q.Alias, q.Folder, q.Limit, q.Days)
			}
		} else {
			if q.SinceUID > 0 {
				imapMessages, e = mc.ListFolderSince(q.Folder, q.Limit, q.Days, q.SinceUID, q.WithBody)
			} else if q.WithBody {
				imapMessages, e = mc.ListFolderWithBodies(q.Folder, q.Limit, q.Days)
			} else {
				imapMessages, e = mc.ListFolder(q.Folder, q.Limit, q.Days)
			}
		}
		return e
	})
	if poolErr == nil {
		if imapMessages == nil {
			imapMessages = []mail.Message{}
		}
		for i := range imapMessages {
			imapMessages[i].Provider = "imap"
			f := imapMessages[i].Folder
			if f == "" || strings.EqualFold(f, "inbox") {
				f = "INBOX"
			}
			imapMessages[i].Folder = f
			ref := mail.MessageRef{
				Provider:    "imap",
				AccountID:   q.AccountID,
				Mailbox:     f,
				UIDValidity: imapMessages[i].UIDValidity,
				UID:         imapMessages[i].UID,
			}
			imapMessages[i].MessageRef = ref.Encode()
		}
		return InboxResult{
			AccountID: q.AccountID,
			Alias:     q.Alias,
			Folder:    q.Folder,
			Count:     len(imapMessages),
			Messages:  imapMessages,
			Method:    "imap",
		}, nil
	}

	if ctx.Err() != nil {
		return InboxResult{}, ctx.Err()
	}

	// 检查当前账号配置的邮件凭据
	var hasMailbox, hasAppPassword bool
	if snap, ok := b.mgr.GetAccount(q.AccountID); ok && snap != nil {
		hasMailbox = snap.Mailbox != nil && snap.Mailbox.Email != "" && snap.Mailbox.Password != ""
		hasAppPassword = snap.AppPassword != ""
	}

	// 1. 若配置了外部收件邮箱 (Mailbox: QQ/163/Gmail 等)，邮件在第三方邮箱中，严禁回退到 Apple WebMail，直接报出真实错误
	if hasMailbox {
		return InboxResult{}, &BackendError{
			Status:  http.StatusBadGateway,
			Code:    "UPSTREAM_FAILURE",
			Message: "收件邮箱读取失败: " + poolErr.Error(),
		}
	}

	// 2. 若未开通 @icloud.com 原生邮箱的第三方账号且尝试了 IMAP
	if poolErr != nil && strings.Contains(poolErr.Error(), "未设置 iCloud 邮箱") {
		return InboxResult{}, &BackendError{
			Status:  http.StatusBadRequest,
			Code:    "NON_ICLOUD_MAIL_USER",
			Message: "该 Apple ID 未设置 @icloud.com 原生邮箱。发往别名的邮件已被苹果转寄至您的注册邮箱，请在【账号管理】中点击「接入收件邮箱」（如 QQ 邮箱 IMAP 授权码），或在苹果设备上开启 iCloud 邮件。",
		}
	}

	// IMAP 失败,继续尝试 Web API
	if q.FolderSpecified || q.DaysSpecified {
		return InboxResult{}, &BackendError{
			Status:  http.StatusBadRequest,
			Code:    "CAPABILITY_UNSUPPORTED",
			Message: "WebMail 模式不支持指定文件夹或按天数筛选",
		}
	}

	// 回退到 Web API (Cookie 认证,无需 App Password)
	wmc, err := b.mgr.WebMailClient(q.AccountID)
	if err != nil {
		if hasAppPassword && poolErr != nil {
			return InboxResult{}, &BackendError{
				Status:  http.StatusBadGateway,
				Code:    "UPSTREAM_FAILURE",
				Message: "IMAP 读取失败: " + poolErr.Error(),
			}
		}
		return InboxResult{}, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: "无可用邮件客户端: 需要 App Password 或 Cookie"}
	}

	var messages []mail.Message
	if q.Alias != "" {
		messages, err = wmc.FindByAliasContext(ctx, q.Alias, q.Limit)
		if err != nil {
			if hasAppPassword && poolErr != nil {
				return InboxResult{}, &BackendError{
					Status:  http.StatusBadGateway,
					Code:    "UPSTREAM_FAILURE",
					Message: fmt.Sprintf("IMAP 读取失败 (%s); WebMail 回退亦失败 (%s)", poolErr.Error(), err.Error()),
				}
			}
			return InboxResult{}, classifyInboxErr(err)
		}
	} else {
		messages, err = wmc.ListInboxContext(ctx, q.Limit)
		if err != nil {
			if hasAppPassword && poolErr != nil {
				return InboxResult{}, &BackendError{
					Status:  http.StatusBadGateway,
					Code:    "UPSTREAM_FAILURE",
					Message: fmt.Sprintf("IMAP 读取失败 (%s); WebMail 回退亦失败 (%s)", poolErr.Error(), err.Error()),
				}
			}
			return InboxResult{}, classifyInboxErr(err)
		}
	}
	if messages == nil {
		messages = []mail.Message{}
	}
	for i := range messages {
		messages[i].Provider = "webmail"
		messages[i].ThreadID = messages[i].ID
		ref := mail.MessageRef{
			Provider:  "webmail",
			AccountID: q.AccountID,
			ThreadID:  messages[i].ID,
		}
		messages[i].MessageRef = ref.Encode()
	}
	return InboxResult{AccountID: q.AccountID, Alias: q.Alias, Folder: q.Folder, Count: len(messages), Messages: messages, Method: "web_api"}, nil
}

// ListInbox 读取收件箱摘要 (代理至 ListInboxContext)。
func (b *managerBackend) ListInbox(q InboxQuery) (InboxResult, error) {
	return b.ListInboxContext(context.Background(), q)
}

func (b *managerBackend) ListMailboxesContext(ctx context.Context, accountID string) ([]mail.Folder, error) {
	var folders []mail.Folder
	err := b.mgr.WithMailClientContext(ctx, accountID, func(mc *mail.Client) error {
		var e error
		folders, e = mc.ListMailboxes()
		return e
	})
	if err != nil {
		if strings.Contains(err.Error(), "不存在") || strings.Contains(err.Error(), "未设置") {
			return nil, mapAccountErr(err)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "获取文件夹列表失败"}
	}
	return folders, nil
}

func (b *managerBackend) ListMailboxes(accountID string) ([]mail.Folder, error) {
	return b.ListMailboxesContext(context.Background(), accountID)
}

func (b *managerBackend) GetMessageContext(ctx context.Context, accountID string, rawID string) (*mail.FullMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ref, err := mail.ParseMessageRef(rawID, accountID)
	if err != nil {
		return nil, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: "邮件引用格式无效"}
	}
	if ref.AccountID != "" && ref.AccountID != accountID {
		return nil, &BackendError{Status: http.StatusForbidden, Code: "FORBIDDEN", Message: "跨账号邮件读取被拒绝"}
	}

	// 1. 如果是 WebMail 邮件引用或非数字 ThreadID
	if ref.Provider == "webmail" {
		wmc, werr := b.mgr.WebMailClient(accountID)
		if werr != nil {
			return nil, classifyInboxErr(werr)
		}
		msgs, errList := wmc.ListInboxContext(ctx, webMailLookupLimit)
		if errList != nil {
			return nil, classifyInboxErr(errList)
		}
		for _, m := range msgs {
			if m.ID == ref.ThreadID || (ref.ThreadID != "" && m.ThreadID == ref.ThreadID) {
				fullRef := mail.MessageRef{
					Provider:  "webmail",
					AccountID: accountID,
					ThreadID:  m.ID,
				}
				return &mail.FullMessage{
					Message: mail.Message{
						ID:         m.ID,
						MessageRef: fullRef.Encode(),
						Folder:     "INBOX",
						Subject:    m.Subject,
						From:       m.From,
						To:         m.To,
						Date:       m.Date,
						Preview:    m.Preview,
						Provider:   "webmail",
						ThreadID:   m.ID,
					},
					Body:         m.Preview,
					ContentType:  "text/plain",
					BodyComplete: false, // 显式标识正文不完整（仅预览）
					Provider:     "webmail",
					Method:       "web_api",
				}, nil
			}
		}
		return nil, &BackendError{Status: http.StatusNotFound, Code: "MESSAGE_NOT_FOUND", Message: "邮件不存在"}
	}

	// 2. 如果是 IMAP 邮件引用
	lookupFolder := ref.Mailbox
	if lookupFolder == "" || strings.EqualFold(lookupFolder, "all") {
		lookupFolder = "INBOX"
	}
	var message *mail.FullMessage
	imapErr := b.mgr.WithMailClientContext(ctx, accountID, func(mc *mail.Client) error {
		var e error
		if ref.UIDValidity > 0 {
			message, e = mc.GetFullInFolderWithValidity(lookupFolder, ref.UIDValidity, ref.UID)
		} else {
			message, e = mc.GetFullInFolder(lookupFolder, ref.UID)
		}
		return e
	})
	if imapErr == nil && message != nil {
		message.AccountID = accountID
		message.Provider = "imap"
		message.Method = "imap"
		message.BodyComplete = true
		fullRef := mail.MessageRef{
			Provider:    "imap",
			AccountID:   accountID,
			Mailbox:     message.Folder,
			UIDValidity: message.UIDValidity,
			UID:         message.UID,
		}
		message.MessageRef = fullRef.Encode()
		return message, nil
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if errors.Is(imapErr, mail.ErrUIDValidityMismatch) {
		return nil, &BackendError{Status: http.StatusNotFound, Code: "UIDVALIDITY_MISMATCH", Message: "邮箱 UIDVALIDITY 已变更，原邮件引用失效"}
	}

	if imapErr != nil {
		msg := imapErr.Error()
		if strings.Contains(msg, "不存在") {
			return nil, &BackendError{Status: http.StatusNotFound, Code: "MESSAGE_NOT_FOUND", Message: "邮件不存在"}
		}
		if strings.Contains(msg, "账号不存在") || strings.Contains(msg, "未设置") {
			return nil, mapAccountErr(imapErr)
		}
	}
	return nil, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "读取邮件详情失败"}
}

func (b *managerBackend) GetMessage(accountID string, rawID string) (*mail.FullMessage, error) {
	return b.GetMessageContext(context.Background(), accountID, rawID)
}

func (b *managerBackend) GetMessagesContext(ctx context.Context, accountID string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return []*mail.FullMessage{}, nil
	}

	type folderGroup struct {
		folder      string
		uidValidity uint32
		uids        []uint32
	}
	groups := make(map[string]*folderGroup)
	var webMailRefs []mail.MessageRef

	for _, r := range refs {
		if strings.EqualFold(r.Provider, "webmail") || r.ThreadID != "" {
			webMailRefs = append(webMailRefs, r)
			continue
		}
		f := strings.TrimSpace(r.Mailbox)
		if f == "" || strings.EqualFold(f, "all") || strings.EqualFold(f, "inbox") {
			f = "INBOX"
		}
		if r.UID > 0 {
			key := fmt.Sprintf("%s:%d", f, r.UIDValidity)
			g, ok := groups[key]
			if !ok {
				g = &folderGroup{
					folder:      f,
					uidValidity: r.UIDValidity,
				}
				groups[key] = g
			}
			g.uids = append(g.uids, r.UID)
		}
	}

	var allMessages []*mail.FullMessage
	if len(groups) > 0 {
		err := b.mgr.WithMailClientContext(ctx, accountID, func(mc *mail.Client) error {
			for _, g := range groups {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				var msgs []*mail.FullMessage
				var e error
				if g.uidValidity > 0 {
					msgs, e = mc.GetFullBatchInFolderWithValidity(g.folder, g.uidValidity, g.uids)
				} else {
					msgs, e = mc.GetFullBatchInFolder(g.folder, g.uids)
				}
				if e != nil {
					if errors.Is(e, mail.ErrUIDValidityMismatch) {
						// UIDVALIDITY 不一致表示代际变更，不能读取新代际邮件充当旧邮件，跳过该组
						continue
					}
					return e
				}
				for _, m := range msgs {
					m.AccountID = accountID
					m.Provider = "imap"
					m.Method = "imap"
					m.BodyComplete = true
					fullRef := mail.MessageRef{
						Provider:    "imap",
						AccountID:   accountID,
						Mailbox:     m.Folder,
						UIDValidity: m.UIDValidity,
						UID:         m.UID,
					}
					m.MessageRef = fullRef.Encode()
					allMessages = append(allMessages, m)
				}
			}
			return nil
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if strings.Contains(err.Error(), "不存在") || strings.Contains(err.Error(), "未设置") {
				return nil, mapAccountErr(err)
			}
			return nil, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "批量读取邮件详情失败"}
		}
	}

	for _, wr := range webMailRefs {
		if wr.ThreadID != "" {
			if m, err := b.GetMessageContext(ctx, accountID, wr.ThreadID); err == nil && m != nil {
				allMessages = append(allMessages, m)
			}
		}
	}

	return allMessages, nil
}

func (b *managerBackend) GetMessages(accountID string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
	return b.GetMessagesContext(context.Background(), accountID, refs)
}

func (b *managerBackend) DeleteMessage(accountID string, uid uint32) error {
	err := b.mgr.WithMailClient(accountID, func(mc *mail.Client) error {
		return mc.Delete(uid)
	})
	if err != nil {
		if strings.Contains(err.Error(), "不存在") || strings.Contains(err.Error(), "未设置") {
			return mapAccountErr(err)
		}
		return &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "删除邮件失败"}
	}
	return nil
}

func (b *managerBackend) GetMailboxBoundaryContext(ctx context.Context, accountID, folder string) (string, uint32, uint32, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, 0, err
	}
	if folder == "" {
		folder = "INBOX"
	}
	var uidValidity, uidNext uint32
	var imapErr error
	poolErr := b.mgr.WithMailClientContext(ctx, accountID, func(mc *mail.Client) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		v, n, err := mc.GetMailboxBoundary(folder)
		if err != nil {
			imapErr = err
			return err
		}
		uidValidity = v
		uidNext = n
		return nil
	})
	if poolErr == nil && imapErr == nil {
		return "imap", uidValidity, uidNext, nil
	}
	if ctx.Err() != nil {
		return "", 0, 0, ctx.Err()
	}

	acc, ok := b.mgr.GetAccount(accountID)
	if ok && (len(acc.Cookies) > 0 || acc.AppPassword != "") {
		return "webmail", 0, 0, &BackendError{
			Status:  http.StatusBadRequest,
			Code:    "CAPABILITY_UNSUPPORTED",
			Message: "WebMail 不支持严格时效验证码基线 (无单邮件稳定游标)",
		}
	}
	return "", 0, 0, &BackendError{
		Status:  http.StatusServiceUnavailable,
		Code:    "BASELINE_UNAVAILABLE",
		Message: "无法获取邮件基线边界: 邮箱客户端未就绪",
	}
}

func (b *managerBackend) GetMailboxBoundary(accountID, folder string) (string, uint32, uint32, error) {
	return b.GetMailboxBoundaryContext(context.Background(), accountID, folder)
}

// ScanMailboxUIDPage 执行内部专用增量分页扫描 (PR-04A F07)。
func (b *managerBackend) ScanMailboxUIDPage(ctx context.Context, q ScanPageQuery) (ScanPageResult, error) {
	if err := ctx.Err(); err != nil {
		return ScanPageResult{}, err
	}
	folder := strings.TrimSpace(q.Folder)
	if folder == "" || strings.EqualFold(folder, "all") {
		folder = "INBOX"
	}
	var res ScanPageResult
	poolErr := b.mgr.WithMailClientContext(ctx, q.AccountID, func(mc *mail.Client) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		pageRes, err := mc.ScanMailboxUIDPage(mail.ScanPageOptions{
			Folder:           folder,
			FromUIDInclusive: q.FromUIDInclusive,
			ToUIDInclusive:   q.ToUIDInclusive,
			PageSize:         q.PageSize,
		})
		if err != nil {
			return err
		}
		if q.UIDValidity > 0 && pageRes.UIDValidity != q.UIDValidity {
			return mail.ErrUIDValidityMismatch
		}
		for i := range pageRes.Messages {
			pageRes.Messages[i].AccountID = q.AccountID
			pageRes.Messages[i].Provider = "imap"
			f := pageRes.Messages[i].Folder
			if f == "" || strings.EqualFold(f, "inbox") {
				f = "INBOX"
			}
			pageRes.Messages[i].Folder = f
			ref := mail.MessageRef{
				Provider:    "imap",
				AccountID:   q.AccountID,
				Mailbox:     f,
				UIDValidity: pageRes.UIDValidity,
				UID:         pageRes.Messages[i].UID,
			}
			pageRes.Messages[i].MessageRef = ref.Encode()
		}
		res = ScanPageResult{
			UIDValidity: pageRes.UIDValidity,
			Messages:    pageRes.Messages,
			NextUID:     pageRes.NextUID,
			HasMore:     pageRes.HasMore,
		}
		return nil
	})
	if poolErr == nil {
		return res, nil
	}
	if ctx.Err() != nil {
		return ScanPageResult{}, ctx.Err()
	}
	if errors.Is(poolErr, mail.ErrUIDValidityMismatch) {
		return ScanPageResult{}, mail.ErrUIDValidityMismatch
	}
	return ScanPageResult{}, &BackendError{
		Status:  http.StatusBadRequest,
		Code:    "CAPABILITY_UNSUPPORTED",
		Message: "增量分页扫描仅支持 IMAP 模式: " + poolErr.Error(),
	}
}

// classifyInboxErr 映射收件箱读取错误, 特殊处理未开通 iCloud 邮件的账号。
func classifyInboxErr(err error) *BackendError {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "Non iCloud Mail user") || strings.Contains(msg, "未开通 iCloud 邮件") {
		return &BackendError{
			Status:  http.StatusBadRequest,
			Code:    "NON_ICLOUD_MAIL_USER",
			Message: "该 Apple ID 未开通 @icloud.com 原生邮箱。邮件已被转寄至您的注册邮箱，请在【账号管理】中配置收件邮箱（如 QQ 邮箱 IMAP 授权码），或在苹果设备上开启 iCloud 邮件。",
		}
	}
	if isSessionError(msg) {
		return &BackendError{Status: http.StatusUnauthorized, Code: "UPSTREAM_UNAUTHORIZED", Message: "iCloud 会话失效，请更新 Cookie"}
	}
	return &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "读取邮件失败: " + msg}
}
