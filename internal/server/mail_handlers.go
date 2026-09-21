/**
 * [INPUT]: 依赖 gin, net/http, strings, strconv, time, fmt, errors, icloud-hme/internal/mail
 * [OUTPUT]: 对外提供 listInboxHandler, listMailboxesHandler, getMessageHandler, getMessagePrimeHandler, getMessagesHandler, deleteMessageHandler, checkProxyHandler 等 HTTP 端点
 * [POS]: internal/server 的邮件收件箱、消息详情缓存与代理连通性检测路由处理器；PR-01 物理删信统一安全阻断 400 MAIL_DELETE_UNSUPPORTED，批量拉取兼容 id 作为 uid 别名
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/mail"
)

// ====================================================================
// 核心接口 2: 读取邮件
//   GET /api/inbox?account_id=acc_xxx[&alias=xxx@icloud.com][&folder=all][&limit=20][&days=7]
// ====================================================================

func (s *Server) listInboxHandler(c *gin.Context) {
	accountID := c.Query("account_id")
	if accountID == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数缺失: account_id")
		return
	}
	alias := strings.TrimSpace(c.Query("alias"))
	folder := c.DefaultQuery("folder", "all")
	limit, err := parseInboxInt(c.DefaultQuery("limit", "20"), 1, 100)
	if err != nil {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: limit 需为 1-100 的整数")
		return
	}
	days, err := parseInboxInt(c.DefaultQuery("days", "7"), 1, 90)
	if err != nil {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: days 需为 1-90 的整数")
		return
	}
	folderSpecified := c.Query("folder") != "" && c.Query("folder") != "all" && !strings.EqualFold(c.Query("folder"), "INBOX")
	daysSpecified := c.Query("days") != ""
	withBody := c.Query("body") == "1" || strings.EqualFold(c.Query("body"), "true")

	result, err := s.be.ListInbox(InboxQuery{
		AccountID:       accountID,
		Alias:           alias,
		Folder:          folder,
		Limit:           limit,
		Days:            days,
		WithBody:        withBody,
		FolderSpecified: folderSpecified,
		DaysSpecified:   daysSpecified,
	})
	if err != nil {
		backendFail(c, err)
		return
	}
	ok(c, result)
}

func (s *Server) listMailboxesHandler(c *gin.Context) {
	accountID := strings.TrimSpace(c.Query("account_id"))
	if accountID == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数缺失: account_id")
		return
	}
	folders, err := s.be.ListMailboxes(accountID)
	if err != nil {
		backendFail(c, err)
		return
	}
	ok(c, gin.H{
		"account_id": accountID,
		"folders":    folders,
	})
}

func (s *Server) handleGetMessageDetail(c *gin.Context, rawID string) {
	accountID := strings.TrimSpace(c.Query("account_id"))
	rawID = strings.TrimSpace(rawID)
	if accountID == "" || rawID == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数缺失: account_id 或邮件 ID 无效")
		return
	}

	ref, err := mail.ParseMessageRef(rawID, accountID)
	if err != nil {
		if errors.Is(err, mail.ErrAccountMismatch) {
			failCode(c, http.StatusForbidden, "FORBIDDEN", "跨账号邮件读取被拒绝")
			return
		}
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "邮件引用格式无效")
		return
	}
	if ref.AccountID != "" && ref.AccountID != accountID {
		failCode(c, http.StatusForbidden, "FORBIDDEN", "跨账号邮件读取被拒绝")
		return
	}

	cacheKey := ref.CacheKey()
	s.msgCacheMu.RLock()
	entry, hit := s.msgCache[cacheKey]
	s.msgCacheMu.RUnlock()

	if hit && time.Now().Before(entry.expiresAt) {
		ok(c, gin.H{
			"account_id": accountID,
			"message":    entry.msg,
			"provider":   entry.provider,
			"method":     entry.method,
			"cached":     true,
		})
		return
	}

	message, err := s.be.GetMessage(accountID, rawID)
	if err != nil {
		backendFail(c, err)
		return
	}

	provider := message.Provider
	if provider == "" {
		provider = message.Message.Provider
	}
	if provider == "" {
		provider = "imap"
	}
	message.Provider = provider
	message.Message.Provider = provider
	method := message.Method
	if method == "" {
		if provider == "webmail" {
			method = "web_api"
		} else {
			method = "imap"
		}
	}

	s.putMessageCache([]string{cacheKey}, message, provider, method)

	ok(c, gin.H{
		"account_id": accountID,
		"message":    message,
		"provider":   provider,
		"method":     method,
		"cached":     false,
	})
}

func (s *Server) getMessageHandler(c *gin.Context) {
	s.handleGetMessageDetail(c, c.Param("message_id"))
}

func (s *Server) getMessagePrimeHandler(c *gin.Context) {
	s.handleGetMessageDetail(c, c.Param("id"))
}

type batchMessageItemReq struct {
	Folder     string `json:"folder"`
	UID        string `json:"uid"`
	ID         string `json:"id"`
	MessageRef string `json:"message_ref"`
}

type getMessagesReq struct {
	AccountID string                `json:"account_id"`
	Messages  []batchMessageItemReq `json:"messages"`
}

type BatchItemResult struct {
	RequestedRef string            `json:"requested_ref"`
	Message      *mail.FullMessage `json:"message,omitempty"`
	Error        string            `json:"error,omitempty"`
}

func (s *Server) getMessagesHandler(c *gin.Context) {
	var req getMessagesReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: account_id, messages 必填 — "+err.Error())
		return
	}
	req.AccountID = strings.TrimSpace(req.AccountID)
	if req.AccountID == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "account_id 必填")
		return
	}
	if len(req.Messages) == 0 {
		ok(c, gin.H{
			"account_id": req.AccountID,
			"messages":   []*mail.FullMessage{},
			"items":      []BatchItemResult{},
			"count":      0,
		})
		return
	}
	if len(req.Messages) > 50 {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "单次最多批量获取 50 封邮件")
		return
	}

	var items []BatchItemResult
	var out []*mail.FullMessage
	now := time.Now()

	type pendingItem struct {
		rawRef  string
		refObj  mail.MessageRef
		imapRef MessageRef
	}
	var pendingIMAP []pendingItem

	for _, m := range req.Messages {
		rawRef := strings.TrimSpace(m.MessageRef)
		folder := strings.TrimSpace(m.Folder)
		if folder == "" {
			folder = "INBOX"
		}
		rawUID := strings.TrimSpace(m.UID)
		if rawUID == "" {
			rawUID = strings.TrimSpace(m.ID)
		}

		var refObj mail.MessageRef
		var refErr error
		if rawRef != "" {
			refObj, refErr = mail.ParseMessageRef(rawRef, req.AccountID)
		} else if uid, err := strconv.ParseUint(rawUID, 10, 32); err == nil && uid > 0 {
			refObj = mail.MessageRef{
				Provider:  "imap",
				AccountID: req.AccountID,
				Mailbox:   folder,
				UID:       uint32(uid),
			}
			rawRef = refObj.Encode()
		} else if rawUID != "" {
			refObj = mail.MessageRef{
				Provider:  "webmail",
				AccountID: req.AccountID,
				ThreadID:  rawUID,
			}
			rawRef = refObj.Encode()
		} else {
			refErr = errors.New("missing message_ref or uid")
		}

		if refErr != nil {
			items = append(items, BatchItemResult{
				RequestedRef: rawRef,
				Error:        refErr.Error(),
			})
			continue
		}

		cacheKey := refObj.CacheKey()
		s.msgCacheMu.RLock()
		entry, hit := s.msgCache[cacheKey]
		s.msgCacheMu.RUnlock()

		if hit && now.Before(entry.expiresAt) {
			items = append(items, BatchItemResult{
				RequestedRef: rawRef,
				Message:      entry.msg,
			})
			out = append(out, entry.msg)
			continue
		}

		if refObj.Provider == "webmail" {
			msg, err := s.be.GetMessage(req.AccountID, refObj.ThreadID)
			if err != nil {
				items = append(items, BatchItemResult{
					RequestedRef: rawRef,
					Error:        err.Error(),
				})
			} else {
				msg.Provider = "webmail"
				msg.Method = "web_api"
				s.putMessageCache([]string{cacheKey}, msg, "webmail", "web_api")
				items = append(items, BatchItemResult{
					RequestedRef: rawRef,
					Message:      msg,
				})
				out = append(out, msg)
			}
			continue
		}

		pendingIMAP = append(pendingIMAP, pendingItem{
			rawRef:  rawRef,
			refObj:  refObj,
			imapRef: MessageRef{Folder: refObj.Mailbox, UID: refObj.UID},
		})
	}

	if len(pendingIMAP) > 0 {
		var imapRefs []MessageRef
		for _, pi := range pendingIMAP {
			imapRefs = append(imapRefs, pi.imapRef)
		}
		fetched, err := s.be.GetMessages(req.AccountID, imapRefs)
		if err != nil {
			if len(out) == 0 && len(req.Messages) == len(pendingIMAP) {
				backendFail(c, err)
				return
			}
			for _, pi := range pendingIMAP {
				items = append(items, BatchItemResult{
					RequestedRef: pi.rawRef,
					Error:        err.Error(),
				})
			}
		} else {
			for i, pi := range pendingIMAP {
				if i < len(fetched) && fetched[i] != nil {
					msg := fetched[i]
					provider := msg.Provider
					if provider == "" {
						provider = "imap"
					}
					method := msg.Method
					if method == "" {
						method = "imap"
					}
					msg.Provider = provider
					msg.Method = method
					s.putMessageCache([]string{pi.refObj.CacheKey()}, msg, provider, method)
					items = append(items, BatchItemResult{
						RequestedRef: pi.rawRef,
						Message:      msg,
					})
					out = append(out, msg)
				} else {
					items = append(items, BatchItemResult{
						RequestedRef: pi.rawRef,
						Error:        "message not found",
					})
				}
			}
		}
	}

	ok(c, gin.H{
		"account_id": req.AccountID,
		"messages":   out,
		"items":      items,
		"count":      len(out),
	})
}

const maxMessageCacheEntries = 1000

func (s *Server) putMessageCache(keys []string, msg *mail.FullMessage, provider, method string) {
	s.msgCacheMu.Lock()
	defer s.msgCacheMu.Unlock()
	if s.msgCache == nil {
		s.msgCache = make(map[string]messageCacheEntry)
	}
	now := time.Now()
	if len(s.msgCache) >= maxMessageCacheEntries {
		for k, e := range s.msgCache {
			if now.After(e.expiresAt) {
				delete(s.msgCache, k)
			}
		}
		if len(s.msgCache) >= maxMessageCacheEntries {
			s.msgCache = make(map[string]messageCacheEntry)
		}
	}
	entry := messageCacheEntry{
		msg:       msg,
		expiresAt: now.Add(10 * time.Minute),
		provider:  provider,
		method:    method,
	}
	for _, k := range keys {
		s.msgCache[k] = entry
	}
}

func (s *Server) deleteMessageHandler(c *gin.Context) {
	// 【PR-01 安全止损】在邮件身份模型与目标 UID 精确物理删除能力未完善前，
	// 服务端物理阻断删信入口，杜绝普通 EXPUNGE 连带误删或并发冲突。
	failCode(c, http.StatusBadRequest, "MAIL_DELETE_UNSUPPORTED", "物理邮件删除功能因安全性考量暂不可用，已安全阻断")
}

func (s *Server) clearMessageCache(accountID, rawID, idPart string) {
	cacheKey := fmt.Sprintf("%s:%s", accountID, rawID)
	s.msgCacheMu.Lock()
	defer s.msgCacheMu.Unlock()
	if s.msgCache == nil {
		return
	}
	delete(s.msgCache, cacheKey)
	prefix := accountID + ":"
	suffix := ":" + idPart
	for k := range s.msgCache {
		if strings.HasPrefix(k, prefix) && strings.HasSuffix(k, suffix) {
			delete(s.msgCache, k)
		}
	}
}

func parseInboxInt(raw string, min, max int) (int, error) {
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, errors.New("invalid integer")
	}
	if v < min || v > max {
		return 0, errors.New("out of range")
	}
	return v, nil
}

type checkProxyReq struct {
	Proxy string `json:"proxy"`
}

func (s *Server) checkProxyHandler(c *gin.Context) {
	var req checkProxyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: proxy 必填")
		return
	}
	proxy := strings.TrimSpace(req.Proxy)
	if proxy == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "代理地址不能为空")
		return
	}

	okRes, latencyMs, msg, err := s.be.CheckProxy(proxy)
	if err != nil {
		// 不回显上游错误原文:错误文本差异会变成内网端口探测的盲 SSRF oracle，
		// 且可能带出代理 URL 中的账号密码。详细原因只写服务端日志。
		log.Printf("[ProxyCheck] 代理探测失败: %v", err)
		failCode(c, http.StatusBadGateway, "PROXY_CHECK_FAILED", "代理连接失败，请检查地址、协议与凭据是否正确")
		return
	}
	ok(c, gin.H{
		"ok":         okRes,
		"latency_ms": latencyMs,
		"message":    msg,
	})
}
