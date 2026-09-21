/**
 * [INPUT]: 依赖 internal/mail, internal/account
 * [OUTPUT]: 对外提供 managerBackend 的邮件收发与邮箱管理方法 (ListInbox, ListMailboxes, GetMessage, GetMessages, DeleteMessage)、parseMessageID 与 InboxQuery, InboxResult, MessageRef 类型
 * [POS]: internal/server 的邮件业务门面实现；IMAP folder:uid 与 WebMail ThreadID 分流，批量详情降级尽力返回 WebMail 列表
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"icloud-hme/internal/mail"
)

// InboxQuery 是收件箱查询参数。
type InboxQuery struct {
	AccountID string
	Alias     string
	Folder    string
	Limit     int
	Days      int
	WithBody  bool
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

// MessageRef 邮件引用标识 (文件夹 + UID)
type MessageRef struct {
	Folder string `json:"folder"`
	UID    uint32 `json:"uid"`
}

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

// ListInbox 读取收件箱摘要:IMAP (App Password) 优先,Web API (Cookie) 回退。
func (b *managerBackend) ListInbox(q InboxQuery) (InboxResult, error) {
	if q.Folder == "" {
		q.Folder = "all"
	}
	// 优先使用 IMAP 连接池 (App Password 认证,复用长连接)
	var imapMessages []mail.Message
	poolErr := b.mgr.WithMailClient(q.AccountID, func(mc *mail.Client) error {
		var e error
		if q.Alias != "" {
			imapMessages, e = mc.FindByRecipientInFolder(q.Alias, q.Folder, q.Limit, q.Days)
		} else {
			// 优先在 IMAP 中按 @icloud.com 搜索，直接提取真实的 iCloud 别名邮件，
			// 避免被个人主邮箱的原生无关杂信（如淘宝、账单）挤占导致别名邮件遗漏
			imapMessages, e = mc.FindByRecipientInFolder("@icloud.com", q.Folder, q.Limit, q.Days)
			if e != nil || len(imapMessages) == 0 {
				imapMessages, e = mc.ListFolder(q.Folder, q.Limit*2, q.Days)
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
			ref := mail.MessageRef{
				Provider:    "imap",
				AccountID:   q.AccountID,
				Mailbox:     imapMessages[i].Folder,
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
	// IMAP 失败,继续尝试 Web API

	// 回退到 Web API (Cookie 认证,无需 App Password)
	wmc, err := b.mgr.WebMailClient(q.AccountID)
	if err != nil {
		return InboxResult{}, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: "无可用邮件客户端: 需要 App Password 或 Cookie"}
	}

	var messages []mail.Message
	if q.Alias != "" {
		messages, err = wmc.FindByAlias(q.Alias, q.Limit)
		if err != nil {
			return InboxResult{}, classifyInboxErr(err)
		}
	} else {
		messages, err = wmc.ListInbox(q.Limit)
		if err != nil {
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

func (b *managerBackend) ListMailboxes(accountID string) ([]mail.Folder, error) {
	var folders []mail.Folder
	err := b.mgr.WithMailClient(accountID, func(mc *mail.Client) error {
		var e error
		folders, e = mc.ListMailboxes()
		return e
	})
	if err != nil {
		if strings.Contains(err.Error(), "不存在") || strings.Contains(err.Error(), "未设置") {
			return nil, mapAccountErr(err)
		}
		return nil, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "获取文件夹列表失败"}
	}
	return folders, nil
}

func (b *managerBackend) GetMessage(accountID string, rawID string) (*mail.FullMessage, error) {
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
		msgs, errList := wmc.ListInbox(webMailLookupLimit)
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
	imapErr := b.mgr.WithMailClient(accountID, func(mc *mail.Client) error {
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
		if message.MessageRef == "" {
			fullRef := mail.MessageRef{
				Provider:    "imap",
				AccountID:   accountID,
				Mailbox:     message.Folder,
				UIDValidity: message.UIDValidity,
				UID:         message.UID,
			}
			message.MessageRef = fullRef.Encode()
		}
		return message, nil
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

func (b *managerBackend) GetMessages(accountID string, refs []MessageRef) ([]*mail.FullMessage, error) {
	if len(refs) == 0 {
		return []*mail.FullMessage{}, nil
	}

	byFolder := make(map[string][]uint32)
	for _, r := range refs {
		f := strings.TrimSpace(r.Folder)
		if f == "" || strings.EqualFold(f, "all") {
			f = "INBOX"
		}
		if r.UID > 0 {
			byFolder[f] = append(byFolder[f], r.UID)
		}
	}

	var allMessages []*mail.FullMessage
	err := b.mgr.WithMailClient(accountID, func(mc *mail.Client) error {
		for f, uids := range byFolder {
			msgs, e := mc.GetFullBatchInFolder(f, uids)
			if e != nil {
				return e
			}
			for _, m := range msgs {
				m.Provider = "imap"
				m.Method = "imap"
				m.BodyComplete = true
				allMessages = append(allMessages, m)
			}
		}
		return nil
	})
	if err == nil {
		return allMessages, nil
	}

	// 严禁在 IMAP 失败后拿 WebMail 最近邮件冒充所请求的邮件！
	if strings.Contains(err.Error(), "不存在") || strings.Contains(err.Error(), "未设置") {
		return nil, mapAccountErr(err)
	}
	return nil, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "批量读取邮件详情失败"}
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
