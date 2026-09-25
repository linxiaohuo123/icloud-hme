/**
 * [INPUT]: 依赖 gin, net/http, encoding/json, icloud-hme/internal/account
 * [OUTPUT]: 对外提供 listAccountsHandler, addAccountHandler, updateAccountHandler 等 HTTP 端点
 * [POS]: internal/server 的账号层路由适配器，负责请求反序列化、入参校验与安全 Summary 响应包装
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

// Package server - 账号管理 handler。
//
// 只做绑定、校验、调用 Backend 和响应映射;账号接口统一返回无秘密的
// account.Summary。任何响应不得包含 cookies、app_password、proxy。
package server

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/account"
)

// listAccountsHandler 处理 GET /api/accounts。
// 支持可选的分页参数: ?limit=50&offset=0
// 未指定 limit 时，返回全量数组（向后兼容）。
// 指定 limit 时，返回分页包: { items: [...], total: n, limit: l, offset: o }
func (s *Server) listAccountsHandler(c *gin.Context) {
	limitStr := c.Query("limit")
	if limitStr == "" {
		ok(c, s.be.ListAccounts())
		return
	}
	limit, _ := strconv.Atoi(limitStr)
	// 封顶:既防超大 limit 一次性把全量账号拉进内存，也避免 offset+limit 整数溢出
	// 变成负数导致切片越界 panic(如 ?limit=9223372036854775807&offset=1)。
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	offset, _ := strconv.Atoi(c.Query("offset"))
	if offset < 0 {
		offset = 0
	}

	all := s.be.ListAccounts()
	total := len(all)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	paged := all[offset:end]

	ok(c, gin.H{
		"items":  paged,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// getAccountHandler 处理 GET /api/accounts/:id。
func (s *Server) getAccountHandler(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		failCode(c, http.StatusBadRequest, "INVALID_ID", "缺少账号 ID")
		return
	}
	sum, err := s.be.GetAccount(id)
	if err != nil {
		backendFail(c, err)
		return
	}
	ok(c, sum)
}

// addAccountReq 是 POST /api/accounts 请求体。
type addAccountReq struct {
	Name        string   `json:"name"`
	ICloudEmail string   `json:"icloud_email"`
	Cookies     string   `json:"cookies"`
	Host        string   `json:"host"`
	Proxy       string   `json:"proxy"`
	Tags        []string `json:"tags"`
}

// addAccountHandler 处理 POST /api/accounts。
func (s *Server) addAccountHandler(c *gin.Context) {
	var req addAccountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误")
		return
	}
	sum, err := s.be.AddAccount(account.AddAccountInput{
		Name:        req.Name,
		ICloudEmail: req.ICloudEmail,
		CookieInput: req.Cookies,
		Host:        req.Host,
		Proxy:       req.Proxy,
		Tags:        req.Tags,
	})
	if err != nil {
		backendFail(c, err)
		return
	}
	c.JSON(http.StatusCreated, apiResp{Success: true, Data: sum})
}

// updateAccountReq 是 PATCH /api/accounts/:id 请求体。
type updateAccountReq struct {
	Name        *string   `json:"name"`
	ICloudEmail *string   `json:"icloud_email"`
	Host        *string   `json:"host"`
	Tags        *[]string `json:"tags"`
}

// updateAccountHandler 处理 PATCH /api/accounts/:id。
func (s *Server) updateAccountHandler(c *gin.Context) {
	id := c.Param("id")
	var req updateAccountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误")
		return
	}
	sum, err := s.be.UpdateAccount(id, account.UpdateAccountInput{
		Name:        req.Name,
		ICloudEmail: req.ICloudEmail,
		Host:        req.Host,
		Tags:        req.Tags,
	})
	if err != nil {
		backendFail(c, err)
		return
	}
	if s.mailReadService != nil {
		s.mailReadService.InvalidateAccount(id)
	}
	ok(c, sum)
}

// proxyReq 是 PUT /api/accounts/:id/proxy 请求体。
type proxyReq struct {
	Proxy string `json:"proxy"`
}

// updateProxyHandler 处理 PUT /api/accounts/:id/proxy。
func (s *Server) updateProxyHandler(c *gin.Context) {
	id := c.Param("id")
	var req proxyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误")
		return
	}
	sum, err := s.be.UpdateProxy(id, req.Proxy)
	if err != nil {
		backendFail(c, err)
		return
	}
	if s.mailReadService != nil {
		s.mailReadService.InvalidateAccount(id)
	}
	ok(c, sum)
}

// updateCookiesHandler 处理 PUT /api/accounts/:id/cookies。
//
// cookies 同时兼容字符串与对象;两种输入最终都交给 account.ParseCookieInput。
func (s *Server) updateCookiesHandler(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Cookies json.RawMessage `json:"cookies"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.Cookies) == 0 {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: cookies 必填")
		return
	}

	var raw string
	var asText string
	if json.Unmarshal(req.Cookies, &asText) == nil {
		raw = asText
	} else {
		var asMap map[string]string
		if err := json.Unmarshal(req.Cookies, &asMap); err != nil {
			failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: cookies 格式无效")
			return
		}
		raw = cookieInputToJSON(asMap)
	}

	sum, err := s.be.UpdateCookies(id, raw)
	if err != nil {
		backendFail(c, err)
		return
	}
	if s.mailReadService != nil {
		s.mailReadService.InvalidateAccount(id)
	}
	ok(c, sum)
}

// setAppPasswordReq 是 POST /api/accounts/:id/password 请求体。
type setAppPasswordReq struct {
	ICloudEmail string `json:"icloud_email"`
	AppPassword string `json:"app_password"`
}

// setAppPasswordHandler 处理 POST /api/accounts/:id/password。
func (s *Server) setAppPasswordHandler(c *gin.Context) {
	id := c.Param("id")
	var req setAppPasswordReq
	if err := c.ShouldBindJSON(&req); err != nil || req.ICloudEmail == "" || req.AppPassword == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: icloud_email, app_password 必填")
		return
	}
	sum, err := s.be.SetAppPassword(id, req.ICloudEmail, req.AppPassword)
	if err != nil {
		backendFail(c, err)
		return
	}
	if s.mailReadService != nil {
		s.mailReadService.InvalidateAccount(id)
	}
	ok(c, sum)
}

type setMailboxReq struct {
	Provider          string `json:"provider"`
	Email             string `json:"email"`
	IMAPHost          string `json:"imap_host"`
	IMAPPort          int    `json:"imap_port"`
	AuthorizationCode string `json:"authorization_code"`
}

func (s *Server) setMailboxHandler(c *gin.Context) {
	var req setMailboxReq
	if err := c.ShouldBindJSON(&req); err != nil || req.Email == "" || req.IMAPHost == "" || req.IMAPPort < 1 {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: 收件邮箱、IMAP 服务器和端口必填")
		return
	}
	accountID := c.Param("id")
	sum, err := s.be.SetMailbox(accountID, account.MailboxConfig{
		Provider: req.Provider, Email: req.Email, IMAPHost: req.IMAPHost, IMAPPort: req.IMAPPort, Password: req.AuthorizationCode,
	})
	if err != nil {
		backendFail(c, err)
		return
	}
	if s.mailReadService != nil {
		s.mailReadService.InvalidateAccount(accountID)
	}
	ok(c, sum)
}

// loginAccountReq 是 POST /api/accounts/:id/login 请求体。
type loginAccountReq struct {
	Password string `json:"password"`
	OTPCode  string `json:"otp_code"`
}

// loginAccountHandler 处理 POST /api/accounts/:id/login。
//
// 成功只返回 Summary,绝不返回 Cookies。
func (s *Server) loginAccountHandler(c *gin.Context) {
	id := c.Param("id")
	var req loginAccountReq
	if err := c.ShouldBindJSON(&req); err != nil || req.Password == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: password 必填")
		return
	}
	sum, err := s.be.LoginAccount(id, req.Password, req.OTPCode)
	if err != nil {
		backendFail(c, err)
		return
	}
	if s.mailReadService != nil {
		s.mailReadService.InvalidateAccount(id)
	}
	ok(c, sum)
}

// removeAccountHandler 处理 DELETE /api/accounts/:id。
func (s *Server) removeAccountHandler(c *gin.Context) {
	id := c.Param("id")
	if !s.be.RemoveAccount(id) {
		failCode(c, http.StatusNotFound, "ACCOUNT_NOT_FOUND", "账号不存在")
		return
	}
	if s.mailReadService != nil {
		s.mailReadService.InvalidateAccount(id)
	}
	if s.store != nil {
		_ = s.store.DeleteScheduleConfig(id)
		// 【BUG-14 修复】级联清理路由表,防止残留路由指向已删除账号
		_ = s.store.DeleteAliasRoutesForAccount(id)
		// 级联隔离预存库存,防止幽灵别名出号
		_ = s.store.QuarantineInventoryForAccount(id)
	}
	ok(c, gin.H{"id": id})
}
