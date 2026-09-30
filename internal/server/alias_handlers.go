/**
 * [INPUT]: 依赖 errors, io, gin, net/http, strings, encoding/csv, time, fmt, icloud-hme/internal/hme
 * [OUTPUT]: 对外提供 createAliasHandler, createAliasBatchHandler, listAliasesHandler, updateAliasHandler, batchUpdateAliasHandler, deactivateAliasHandler, reactivateAliasHandler, deleteAliasHandler, exportAliasesHandler, promoteAliasesToPoolHandler 等 HTTP 端点
 * [POS]: internal/server 的别名生命周期与号池管理路由处理器，修改与生命周期操作均拒绝非法 JSON，生命周期操作保留空请求体查询参数用法
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/hme"
)

type createAliasReq struct {
	AccountID string `json:"account_id"`
	Label     string `json:"label"`
}

func (s *Server) createAliasHandler(c *gin.Context) {
	var req createAliasReq
	if err := c.ShouldBindJSON(&req); err != nil || req.AccountID == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: account_id 必填")
		return
	}
	if len([]rune(req.Label)) > 200 {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: label 最长 200 字符")
		return
	}

	result, err := s.be.CreateAliasContext(c.Request.Context(), req.AccountID, req.Label)
	if err != nil {
		backendFail(c, err)
		return
	}
	if s.syncWorker != nil {
		s.syncWorker.RegisterAliasAccount(result.Email, req.AccountID)
	}
	auditErr := s.recordLease(req.AccountID, result.Email, "default", "admin_console")
	ok(c, gin.H{
		"email":          result.Email,
		"label":          result.Label,
		"created_at":     result.CreatedAt,
		"account_id":     req.AccountID,
		"audit_recorded": auditErr == nil,
	})
}

type createAliasBatchReq struct {
	AccountID   string `json:"account_id"`
	Count       int    `json:"count"`
	LabelPrefix string `json:"label_prefix"`
}

func (s *Server) createAliasBatchHandler(c *gin.Context) {
	var req createAliasBatchReq
	if err := c.ShouldBindJSON(&req); err != nil || req.AccountID == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: account_id 必填")
		return
	}
	if req.Count < 1 || req.Count > 5 {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: count 必须在 1-5 之间")
		return
	}
	if len([]rune(req.LabelPrefix)) > 100 {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: label_prefix 最长 100 字符")
		return
	}

	result, err := s.be.BatchCreateAliasContext(c.Request.Context(), req.AccountID, req.Count, req.LabelPrefix)
	if err != nil {
		backendFail(c, err)
		return
	}
	if s.syncWorker != nil {
		for _, item := range result.Created {
			s.syncWorker.RegisterAliasAccount(item.Email, req.AccountID)
		}
	}
	for _, item := range result.Created {
		if err := s.recordLease(req.AccountID, item.Email, "default", "admin_console"); err != nil {
			result.AuditFailed = append(result.AuditFailed, item.Email)
		}
	}
	ok(c, result)
}

func (s *Server) listAliasesHandler(c *gin.Context) {
	accountID := strings.TrimSpace(c.Query("account_id"))
	refresh := c.Query("refresh") == "true"

	// 全局号池聚合模式: account_id 为空或为 "all"
	if accountID == "" || strings.EqualFold(accountID, "all") {
		allAliases, failedAccounts := s.gatherAliases(c.Request.Context(), refresh)

		if refresh && s.store != nil {
			if n, err := s.store.ReconcileAvailableInventory(); err == nil && n > 0 {
				log.Printf("[Aliases] 刷新后库存安全对齐: 已隔离/收敛 %d 个受保护或异常别名", n)
			}
		}

		ok(c, gin.H{
			"account_id":      "all",
			"count":           len(allAliases),
			"aliases":         allAliases,
			"complete":        len(failedAccounts) == 0,
			"failed_accounts": failedAccounts,
		})
		return
	}

	// 单账号拉取模式
	var aliases []hme.Alias
	var err error
	if refresh {
		aliases, err = s.be.RefreshAliasesContext(c.Request.Context(), accountID)
	} else {
		aliases, err = s.be.ListAliasesContext(c.Request.Context(), accountID)
	}
	if err != nil {
		backendFail(c, err)
		return
	}

	accName := accountID
	for _, a := range s.be.ListAccounts() {
		if a.ID == accountID {
			if a.Name != "" {
				accName = a.Name
			} else {
				accName = a.RealEmail
			}
			break
		}
	}
	// 单账号模式同样不就地改写: Backend 返回的可能就是内部缓存快照。
	enriched := make([]hme.Alias, 0, len(aliases))
	for i := range aliases {
		a := aliases[i]
		a.AccountID = accountID
		a.AccountName = accName
		enriched = append(enriched, a)
	}

	ok(c, gin.H{
		"account_id": accountID,
		"count":      len(enriched),
		"aliases":    enriched,
	})
}

// gatherAliases returns the available rows and the accounts that could not be read.
func (s *Server) gatherAliases(ctx context.Context, refresh bool) ([]hme.Alias, []string) {
	accounts := s.be.ListAccounts()
	results := make([][]hme.Alias, len(accounts))
	failed := make([]bool, len(accounts))
	tasks := make([]func(), 0, len(accounts))
	for i, acc := range accounts {
		failed[i] = true
		if acc.Status == "error" || !acc.HasCookies {
			continue
		}
		tasks = append(tasks, func() {
			var aliases []hme.Alias
			var err error
			if refresh {
				aliases, err = s.be.RefreshAliasesContext(ctx, acc.ID)
			} else {
				aliases, err = s.be.ListAliasesContext(ctx, acc.ID)
			}
			if err != nil {
				return
			}
			results[i] = aliases
			failed[i] = false
		})
	}
	runBounded(10, "alias-scatter-gather", tasks)
	var all []hme.Alias
	var failedAccounts []string
	for i, batch := range results {
		if failed[i] {
			failedAccounts = append(failedAccounts, accounts[i].ID)
			continue
		}
		accName := accounts[i].Name
		if accName == "" {
			accName = accounts[i].RealEmail
		}
		for _, item := range batch {
			item.AccountID = accounts[i].ID
			item.AccountName = accName
			all = append(all, item)
		}
	}
	return all, failedAccounts
}

type aliasActionReq struct {
	AccountID string `json:"account_id"`
}

// validateAliasAction 校验别名操作的匿名 ID 与请求体。
func validateAliasAction(c *gin.Context) (accountID, anonymousID string, valid bool) {
	anonymousID = c.Param("id")
	var req aliasActionReq
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: 请求体必须为有效的 JSON")
		return "", "", false
	}
	accID := req.AccountID
	if accID == "" {
		accID = c.Query("account_id")
	}
	if accID == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: account_id 必填")
		return "", "", false
	}
	if anonymousID == "" || len(anonymousID) > 256 {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: anonymous id 无效")
		return "", "", false
	}
	return accID, anonymousID, true
}

type updateAliasReq struct {
	AccountID string `json:"account_id"`
	Label     string `json:"label"`
	Note      string `json:"note"`
}

func (s *Server) updateAliasHandler(c *gin.Context) {
	anonymousID := c.Param("id")
	if anonymousID == "" || len(anonymousID) > 256 {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: anonymous id 无效")
		return
	}
	var req updateAliasReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: 请求体必须为有效的 JSON")
		return
	}
	if req.AccountID == "" {
		req.AccountID = c.Query("account_id")
	}
	if req.AccountID == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: account_id 必填")
		return
	}
	if len([]rune(req.Label)) > 200 {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "label 最长 200 字符")
		return
	}
	if len([]rune(req.Note)) > 500 {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "note 最长 500 字符")
		return
	}
	if err := s.be.UpdateAlias(req.AccountID, anonymousID, req.Label, req.Note); err != nil {
		backendFail(c, err)
		return
	}
	ok(c, gin.H{
		"anonymous_id": anonymousID,
		"label":        req.Label,
		"note":         req.Note,
	})
}

type batchUpdateAliasReq struct {
	AccountID    string   `json:"account_id"`
	AnonymousIDs []string `json:"anonymous_ids"`
	Label        string   `json:"label"`
	Note         string   `json:"note"`
}

func (s *Server) batchUpdateAliasHandler(c *gin.Context) {
	var req batchUpdateAliasReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "请求格式错误")
		return
	}
	if req.AccountID == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: account_id 必填")
		return
	}
	if len(req.AnonymousIDs) == 0 {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: anonymous_ids 不能为空")
		return
	}
	if len(req.AnonymousIDs) > 100 {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "单次最多支持批量修改 100 个别名")
		return
	}
	if len([]rune(req.Label)) > 200 {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "label 最长 200 字符")
		return
	}
	if len([]rune(req.Note)) > 500 {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "note 最长 500 字符")
		return
	}

	res, err := s.be.BatchUpdateAliases(req.AccountID, req.AnonymousIDs, req.Label, req.Note)
	if err != nil {
		backendFail(c, err)
		return
	}
	ok(c, res)
}

func (s *Server) deactivateAliasHandler(c *gin.Context) {
	accountID, anonymousID, valid := validateAliasAction(c)
	if !valid {
		return
	}
	success, err := s.be.SetAliasActive(accountID, anonymousID, false)
	if err != nil {
		backendFail(c, err)
		return
	}
	if !success {
		failCode(c, http.StatusBadGateway, "UPSTREAM_FAILED", "上游停用别名失败")
		return
	}
	ok(c, gin.H{"anonymous_id": anonymousID, "success": success})
}

func (s *Server) reactivateAliasHandler(c *gin.Context) {
	accountID, anonymousID, valid := validateAliasAction(c)
	if !valid {
		return
	}
	success, err := s.be.SetAliasActive(accountID, anonymousID, true)
	if err != nil {
		backendFail(c, err)
		return
	}
	if !success {
		failCode(c, http.StatusBadGateway, "UPSTREAM_FAILED", "上游激活别名失败")
		return
	}
	ok(c, gin.H{"anonymous_id": anonymousID, "success": success})
}

func (s *Server) deleteAliasHandler(c *gin.Context) {
	accountID, anonymousID, valid := validateAliasAction(c)
	if !valid {
		return
	}
	if err := s.be.DeleteAlias(accountID, anonymousID); err != nil {
		backendFail(c, err)
		return
	}
	ok(c, gin.H{"anonymous_id": anonymousID})
}

func (s *Server) exportAliasesHandler(c *gin.Context) {
	accountID := strings.TrimSpace(c.Query("account_id"))
	format := strings.ToLower(strings.TrimSpace(c.DefaultQuery("format", "csv")))

	var aliases []hme.Alias
	if accountID == "" || strings.EqualFold(accountID, "all") {
		var failedAccounts []string
		aliases, failedAccounts = s.gatherAliases(c.Request.Context(), false)
		if len(failedAccounts) > 0 {
			failCode(c, http.StatusBadGateway, "INCOMPLETE_EXPORT", "部分账号别名读取失败，导出已取消: "+strings.Join(failedAccounts, ", "))
			return
		}
	} else {
		list, err := s.be.ListAliasesContext(c.Request.Context(), accountID)
		if err != nil {
			backendFail(c, err)
			return
		}
		accName := accountID
		for _, a := range s.be.ListAccounts() {
			if a.ID == accountID {
				if a.Name != "" {
					accName = a.Name
				} else {
					accName = a.RealEmail
				}
				break
			}
		}
		aliases = make([]hme.Alias, 0, len(list))
		for i := range list {
			enriched := list[i]
			enriched.AccountID = accountID
			enriched.AccountName = accName
			aliases = append(aliases, enriched)
		}
	}

	nowStr := time.Now().Format("20060102_150405")
	if format == "json" {
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"icloud_aliases_%s.json\"", nowStr))
		c.JSON(http.StatusOK, aliases)
		return
	}

	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"icloud_aliases_%s.csv\"", nowStr))
	_, _ = c.Writer.Write([]byte("\xef\xbb\xbf"))
	w := csv.NewWriter(c.Writer)
	_ = w.Write([]string{"别名邮箱", "母账号名称", "母账号ID", "备注标签", "状态", "创建时间"})
	for _, a := range aliases {
		statusStr := "已启用"
		if !a.Active {
			statusStr = "已停用"
		}
		_ = w.Write([]string{
			csvSafe(a.Email),
			csvSafe(a.AccountName),
			csvSafe(a.AccountID),
			csvSafe(a.Label),
			statusStr,
			csvSafe(a.CreatedAt),
		})
	}
	w.Flush()
}

// csvSafe 中和电子表格公式注入：Excel/Sheets 会把以 = + - @ 或制表符/回车开头的
// 单元格当公式执行，别名备注可由调用方任意填写，因此导出前必须前置单引号。
func csvSafe(v string) string {
	if v == "" {
		return v
	}
	switch v[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + v
	}
	return v
}

type promoteAliasesToPoolReq struct {
	AccountID string `json:"account_id"`
	Limit     int    `json:"limit"`
}

// promoteAliasesToPoolHandler 处理 POST /api/aliases/promote-to-pool（管理员作用域）
// 将未被消费过的存量别名受控批量激活为可用号池库存，并刷新水位
func (s *Server) promoteAliasesToPoolHandler(c *gin.Context) {
	if s.store == nil {
		failCode(c, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "存储层未就绪")
		return
	}

	var req promoteAliasesToPoolReq
	if err := c.ShouldBindJSON(&req); err != nil && err.Error() != "EOF" && !errors.Is(err, io.EOF) {
		failCode(c, http.StatusBadRequest, "INVALID_JSON", "请求体 JSON 格式错误: "+err.Error())
		return
	}

	promoted, err := s.store.PromoteUnknownToAvailable(req.AccountID, req.Limit)
	if err != nil {
		failCode(c, http.StatusBadRequest, "PROMOTION_FAILED", err.Error())
		return
	}

	available := s.store.CountAuthoritativeAvailableAliases()
	dormant := s.store.CountDormantPoolAliases()

	ok(c, gin.H{
		"promoted_count":    promoted,
		"available_aliases": available,
		"dormant_aliases":   dormant,
	})
}
