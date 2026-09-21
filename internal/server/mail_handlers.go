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
	withBody := c.Query("body") == "1" || strings.EqualFold(c.Query("body"), "true")

	result, err := s.be.ListInbox(InboxQuery{
		AccountID: accountID,
		Alias:     alias,
		Folder:    folder,
		Limit:     limit,
		Days:      days,
		WithBody:  withBody,
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

func (s *Server) getMessageHandler(c *gin.Context) {
	accountID := strings.TrimSpace(c.Query("account_id"))
	rawID := strings.TrimSpace(c.Param("message_id"))
	if accountID == "" || rawID == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "account_id 或邮件 ID 无效")
		return
	}

	cacheKey := fmt.Sprintf("%s:%s", accountID, rawID)
	s.msgCacheMu.RLock()
	entry, hit := s.msgCache[cacheKey]
	s.msgCacheMu.RUnlock()
	if hit && time.Now().Before(entry.expiresAt) {
		// 【BUG-08 修复】响应包装对齐 getMessagePrimeHandler，包含 account_id/method/cached
		ok(c, gin.H{
			"account_id": accountID,
			"message":    entry.msg,
			"method":     "cache",
			"cached":     true,
		})
		return
	}

	message, err := s.be.GetMessage(accountID, rawID)
	if err != nil {
		backendFail(c, err)
		return
	}

	s.putMessageCache([]string{cacheKey}, message)

	ok(c, gin.H{
		"account_id": accountID,
		"message":    message,
		"method":     "imap",
		"cached":     false,
	})
}

func (s *Server) getMessagePrimeHandler(c *gin.Context) {
	accountID := strings.TrimSpace(c.Query("account_id"))
	rawID := strings.TrimSpace(c.Param("id"))
	if accountID == "" || rawID == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数缺失: account_id 或邮件 ID")
		return
	}

	cacheKey := fmt.Sprintf("%s:%s", accountID, rawID)
	s.msgCacheMu.RLock()
	entry, hit := s.msgCache[cacheKey]
	s.msgCacheMu.RUnlock()
	if hit && time.Now().Before(entry.expiresAt) {
		ok(c, gin.H{
			"account_id": accountID,
			"message":    entry.msg,
			"method":     "cache",
			"cached":     true,
		})
		return
	}

	message, err := s.be.GetMessage(accountID, rawID)
	if err != nil {
		backendFail(c, err)
		return
	}

	s.putMessageCache([]string{cacheKey}, message)

	ok(c, gin.H{
		"account_id": accountID,
		"message":    message,
		"method":     "imap",
		"cached":     false,
	})
}

type getMessagesReq struct {
	AccountID string `json:"account_id"`
	Messages  []struct {
		Folder string `json:"folder"`
		UID    string `json:"uid"`
		ID     string `json:"id"`
	} `json:"messages"`
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
			"count":      0,
		})
		return
	}
	if len(req.Messages) > 50 {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "单次最多批量获取 50 封邮件")
		return
	}

	var uncachedRefs []MessageRef
	var out []*mail.FullMessage
	now := time.Now()

	s.msgCacheMu.RLock()
	for _, m := range req.Messages {
		rawUID := strings.TrimSpace(m.UID)
		if rawUID == "" {
			rawUID = strings.TrimSpace(m.ID)
		}
		uid, err := strconv.ParseUint(rawUID, 10, 32)
		if err != nil {
			continue
		}
		folder := strings.TrimSpace(m.Folder)
		if folder == "" {
			folder = "INBOX"
		}
		key := fmt.Sprintf("%s:%s:%s", req.AccountID, folder, rawUID)
		if entry, hit := s.msgCache[key]; hit && now.Before(entry.expiresAt) {
			out = append(out, entry.msg)
		} else {
			uncachedRefs = append(uncachedRefs, MessageRef{Folder: folder, UID: uint32(uid)})
		}
	}
	s.msgCacheMu.RUnlock()

	if len(uncachedRefs) > 0 {
		fetched, err := s.be.GetMessages(req.AccountID, uncachedRefs)
		if err != nil {
			backendFail(c, err)
			return
		}
		for _, msg := range fetched {
			folder := msg.Folder
			if folder == "" {
				folder = "INBOX"
			}
			s.putMessageCache([]string{
				fmt.Sprintf("%s:%s:%s", req.AccountID, folder, msg.ID),
				fmt.Sprintf("%s:%s", req.AccountID, msg.ID),
			}, msg)
			out = append(out, msg)
		}
	}

	ok(c, gin.H{
		"account_id": req.AccountID,
		"messages":   out,
		"count":      len(out),
	})
}

const maxMessageCacheEntries = 1000

func (s *Server) putMessageCache(keys []string, msg *mail.FullMessage) {
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
