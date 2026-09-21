/**
 * [INPUT]: 依赖 gin, net/http, strings, strconv, errors, icloud-hme/internal/mail
 * [OUTPUT]: 对外提供 listInboxHandler, listMailboxesHandler, getMessageHandler, getMessagePrimeHandler, getMessagesHandler, deleteMessageHandler, checkProxyHandler 等 HTTP 端点
 * [POS]: internal/server 的邮件收件箱与消息详情路由处理器，统一由 MailReadService 驱动并消除冗余私有缓存
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

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

	msg, provider, method, cached, err := s.mailReadService.GetMessageDetail(c.Request.Context(), accountID, rawID)
	if err != nil {
		backendFail(c, err)
		return
	}

	ok(c, gin.H{
		"account_id": accountID,
		"message":    msg,
		"provider":   provider,
		"method":     method,
		"cached":     cached,
	})
}

func (s *Server) getMessageHandler(c *gin.Context) {
	s.handleGetMessageDetail(c, c.Param("message_id"))
}

func (s *Server) getMessagePrimeHandler(c *gin.Context) {
	s.handleGetMessageDetail(c, c.Param("id"))
}

type getMessagesReq struct {
	AccountID string                `json:"account_id"`
	Messages  []batchMessageItemReq `json:"messages"`
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

	out, items, err := s.mailReadService.GetMessagesBatch(c.Request.Context(), req.AccountID, req.Messages)
	if err != nil {
		backendFail(c, err)
		return
	}

	ok(c, gin.H{
		"account_id": req.AccountID,
		"messages":   out,
		"items":      items,
		"count":      len(out),
	})
}

func (s *Server) deleteMessageHandler(c *gin.Context) {
	// 【PR-01 安全止损】在邮件身份模型与目标 UID 精确物理删除能力未完善前，
	// 服务端物理阻断删信入口，杜绝普通 EXPUNGE 连带误删或并发冲突。
	failCode(c, http.StatusBadRequest, "MAIL_DELETE_UNSUPPORTED", "物理邮件删除功能因安全性考量暂不可用，已安全阻断")
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
