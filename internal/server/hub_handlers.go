/**
 * [INPUT]: 依赖 gin, icloud-hme/internal/store, icloud-hme/internal/scheduler
 * [OUTPUT]: 对外提供 Tags, Tokens, Leases, Schedules 的 HTTP Handler 方法
 * [POS]: internal/server 的中台功能路由处理器集合；scheduledTag 优先继承母号业务标签并回退 scheduled，切断与 AliasLabel 模板耦合
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/store"
)

// --- 业务标识 Tags ---

func (s *Server) listTagsHandler(c *gin.Context) {
	ok(c, s.store.ListTags())
}

// tagExistsFor 检查业务标识名是否已被其他标签占用 (excludeID 为编辑时排除自身)
func (s *Server) tagExistsFor(tag string, excludeID string) bool {
	for _, t := range s.store.ListTags() {
		if strings.EqualFold(t.Tag, tag) && t.ID != excludeID {
			return true
		}
	}
	return false
}

func (s *Server) createTagHandler(c *gin.Context) {
	var req store.BusinessTag
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Tag) == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "业务标识 (tag) 不能为空")
		return
	}
	req.Tag = strings.TrimSpace(req.Tag)
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		req.Name = req.Tag
	}
	// 主键与时间戳必须在此处补全: Store.SaveTag 是值接收者，内部生成的字段不会回传给调用方
	if req.ID == "" {
		req.ID = store.NewBusinessTagID()
	}
	if req.CreatedAt == "" {
		req.CreatedAt = time.Now().Format(time.RFC3339)
	}
	if req.Status == "" {
		req.Status = "active"
	}
	// tag 列有 UNIQUE 约束: 预检重名, 避免落库报"内部错误"
	if s.tagExistsFor(req.Tag, "") {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "业务标识已存在: "+req.Tag)
		return
	}
	if err := s.store.SaveTag(req); err != nil {
		if isUniqueViolation(err) {
			failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "业务标识已存在: "+req.Tag)
			return
		}
		backendFail(c, err)
		return
	}
	ok(c, req)
}

func (s *Server) updateTagHandler(c *gin.Context) {
	id := c.Param("id")
	var req store.BusinessTag
	if err := c.ShouldBindJSON(&req); err != nil {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "无效参数")
		return
	}
	// 真 PATCH 语义: 以库中现有记录为基线，只覆盖显式提交的字段，
	// 避免"只改描述"把 tag/name/status 清空。
	existing, found := s.findTagByID(id)
	if !found {
		failCode(c, http.StatusNotFound, "NOT_FOUND", "业务标识不存在")
		return
	}
	req.ID = id
	if req.CreatedAt == "" {
		req.CreatedAt = existing.CreatedAt
	}
	if req.Status == "" {
		req.Status = existing.Status
	}
	if req.Description == "" {
		req.Description = existing.Description
	}
	if strings.TrimSpace(req.Tag) == "" {
		req.Tag = existing.Tag
	}
	if strings.TrimSpace(req.Name) == "" {
		req.Name = existing.Name
	}
	req.Tag = strings.TrimSpace(req.Tag)
	req.Name = strings.TrimSpace(req.Name)
	if s.tagExistsFor(req.Tag, id) {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "业务标识已存在: "+req.Tag)
		return
	}
	if err := s.store.SaveTag(req); err != nil {
		backendFail(c, err)
		return
	}
	ok(c, req)
}

// findTagByID 按主键定位业务标识。
func (s *Server) findTagByID(id string) (store.BusinessTag, bool) {
	for _, t := range s.store.ListTags() {
		if t.ID == id {
			return t, true
		}
	}
	return store.BusinessTag{}, false
}

func (s *Server) deleteTagHandler(c *gin.Context) {
	id := c.Param("id")
	// 【BUG-13 修复】区分"成功删除"和"本就不存在"
	affected, err := s.store.DeleteTag(id)
	if err != nil {
		backendFail(c, err)
		return
	}
	if !affected {
		failCode(c, http.StatusNotFound, "NOT_FOUND", "业务标识不存在")
		return
	}
	ok(c, gin.H{"deleted": true})
}

// --- 外部令牌 Tokens ---

func (s *Server) listTokensHandler(c *gin.Context) {
	// 只回显掩码: 令牌本体仅在创建响应中出现一次，避免管理台被读取后批量收割
	ok(c, s.store.ListTokensMasked())
}

func (s *Server) createTokenHandler(c *gin.Context) {
	var req store.APIToken
	_ = c.ShouldBindJSON(&req)
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		req.Name = "外部接入令牌"
	}
	if strings.TrimSpace(req.Token) == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		req.Token = "am_" + hex.EncodeToString(b)
	}
	if req.ID == "" {
		req.ID = store.NewAPITokenID()
	}
	if req.CreatedAt == "" {
		req.CreatedAt = time.Now().Format(time.RFC3339)
	}
	// 缺省按最小权限发放: 对外令牌默认只能出号与取码，无法触达管理面
	scopes, err := normalizeScopes(req.Scopes)
	if err != nil {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	req.Scopes = scopes
	if err := s.store.SaveToken(req); err != nil {
		if isUniqueViolation(err) {
			failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "该令牌值已存在，请勿重复使用同一个令牌")
			return
		}
		backendFail(c, err)
		return
	}
	ok(c, req)
}

// isUniqueViolation 判断是否命中 SQLite 唯一约束，用于把重复值映射成可读的 400，
// 而不是笼统的「内部错误」。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") || strings.Contains(msg, "constraint failed")
}

// normalizeScopes 校验并归一化作用域集合；空值取对外最小权限默认值。
func normalizeScopes(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return store.DefaultExternalScopes, nil
	}
	seen := make(map[string]bool)
	out := make([]string, 0, 3)
	for _, part := range strings.Split(raw, ",") {
		p := strings.ToLower(strings.TrimSpace(part))
		if p == "" {
			continue
		}
		switch p {
		case store.ScopeAdmin, store.ScopeAllocate, store.ScopeVerify:
		default:
			return "", fmt.Errorf("不支持的作用域: %s (可选 admin / allocate / verify)", p)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return store.DefaultExternalScopes, nil
	}
	return strings.Join(out, ","), nil
}

func (s *Server) deleteTokenHandler(c *gin.Context) {
	id := c.Param("id")
	// 【BUG-13 修复】区分"成功删除"和"本就不存在"
	affected, err := s.store.DeleteToken(id)
	if err != nil {
		backendFail(c, err)
		return
	}
	if !affected {
		failCode(c, http.StatusNotFound, "NOT_FOUND", "令牌不存在")
		return
	}
	ok(c, gin.H{"deleted": true})
}

// --- 已用别名流水 Leases ---

// recordLease 统一写出一条「已用别名」流水，是全站出号链路的唯一入账口径。
// 定时调度 / 一键出号 / 手动建号都必须经过这里，否则审计账本与业务 tag 对账会漏统计。
func (s *Server) recordLease(accountID, email, tag, tokenName string) {
	if s.store == nil || email == "" {
		return
	}
	if tag == "" {
		tag = "default"
	}
	now := time.Now().Format(time.RFC3339)
	if err := s.store.RecordLease(store.LeaseRecord{
		Email:       email,
		AccountID:   accountID,
		Tag:         tag,
		Status:      "completed",
		AllocatedAt: now,
		CompletedAt: now,
		TokenName:   tokenName,
	}); err != nil {
		log.Printf("[Lease] 流水写入失败 email=%s account=%s: %v", email, accountID, err)
	}
	s.store.UpdateTagLastAssigned(tag)
}

// scheduledTag 返回账号定时补货使用的业务标识(优先继承母号首个标签，缺省 scheduled)。
func (s *Server) scheduledTag(accountID string) string {
	if s.be != nil {
		if acc, err := s.be.GetAccount(accountID); err == nil && len(acc.Tags) > 0 {
			if tag := strings.TrimSpace(acc.Tags[0]); tag != "" {
				return tag
			}
		}
	}
	return "scheduled"
}

func (s *Server) listLeasesHandler(c *gin.Context) {
	aliasQ := strings.TrimSpace(c.Query("alias"))
	tagQ := strings.TrimSpace(c.Query("tag"))
	statusQ := strings.TrimSpace(c.Query("status"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	// 上限 500:防止 ?limit=10000000 把上万条流水一次性拉进内存
	if limit <= 0 || limit > 500 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	records, total := s.store.ListLeases(aliasQ, tagQ, statusQ, limit, offset)
	ok(c, gin.H{
		"records": records,
		"total":   total,
	})
}

func (s *Server) updateLeaseStatusHandler(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Status string `json:"status"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Status == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "状态不能为空")
		return
	}
	// 状态白名单:否则任意字符串都会落库，前端状态映射与统计口径随之失效
	switch req.Status {
	case "completed", "leased", "abandoned":
	default:
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "状态必须是 completed / leased / abandoned")
		return
	}
	if err := s.store.UpdateLeaseStatus(id, req.Status); err != nil {
		backendFail(c, err)
		return
	}
	ok(c, gin.H{"updated": true})
}

// --- 定时调度 Schedules ---

func (s *Server) listScheduleConfigsHandler(c *gin.Context) {
	ok(c, s.store.ListScheduleConfigs())
}

// updateScheduleConfigReq 使用指针字段实现真 PATCH 语义:
// 未提交的字段保持库中现值，不会被零值静默覆盖。
//
// 注意: current_hour_count / last_hour_window 是服务端配额仲裁状态，
// 一律不接受客户端提交(即使提交也被忽略)，防止把已扣减的小时配额回退成旧值。
type updateScheduleConfigReq struct {
	Enabled       *bool   `json:"enabled"`
	HourlyQuota   *int    `json:"hourly_quota"`
	AliasLabel    *string `json:"alias_label"`
	Mode          *string `json:"mode"`
	StartTime     *string `json:"start_time"`
	EndTime       *string `json:"end_time"`
	DurationHours *int    `json:"duration_hours"`
	StartedAt     *string `json:"started_at"`
}

func (s *Server) updateScheduleConfigHandler(c *gin.Context) {
	accountID := c.Param("account_id")
	if !s.hasAccount(accountID) {
		failCode(c, http.StatusNotFound, "NOT_FOUND", "账号不存在")
		return
	}
	var req updateScheduleConfigReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "无效参数")
		return
	}

	cfg := s.store.GetScheduleConfig(accountID)
	if req.Enabled != nil {
		cfg.Enabled = *req.Enabled
	}
	if req.HourlyQuota != nil {
		if *req.HourlyQuota < 1 || *req.HourlyQuota > 500 {
			failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "hourly_quota 必须在 1-500 之间")
			return
		}
		cfg.HourlyQuota = *req.HourlyQuota
	}
	if req.AliasLabel != nil {
		cfg.AliasLabel = strings.TrimSpace(*req.AliasLabel)
	}
	if req.Mode != nil {
		mode := strings.ToLower(strings.TrimSpace(*req.Mode))
		switch mode {
		case "always", "daily_window", "duration":
		default:
			failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "mode 必须是 always / daily_window / duration")
			return
		}
		cfg.Mode = mode
	}
	if req.StartTime != nil {
		cfg.StartTime = strings.TrimSpace(*req.StartTime)
	}
	if req.EndTime != nil {
		cfg.EndTime = strings.TrimSpace(*req.EndTime)
	}
	if req.DurationHours != nil {
		cfg.DurationHours = *req.DurationHours
	}
	if req.StartedAt != nil {
		cfg.StartedAt = strings.TrimSpace(*req.StartedAt)
	}
	if cfg.Mode == "duration" && cfg.Enabled && strings.TrimSpace(cfg.StartedAt) == "" {
		cfg.StartedAt = time.Now().Format(time.RFC3339)
	}

	// 归零配额仲裁字段: SaveScheduleConfig 的 UPSERT 在 last_hour_window<=0 时
	// 会保留库中原值，从而杜绝陈旧快照把 current_hour_count 回退。
	cfg.CurrentHourCount = 0
	cfg.LastHourWindow = 0

	if err := s.store.SaveScheduleConfig(cfg); err != nil {
		backendFail(c, err)
		return
	}
	ok(c, cfg)
}

// runScheduleNowHandler 处理 POST /api/schedule/run-now。
// 缺省只跑已启用的定时任务；显式 ?all=true 才对全部账号强推一轮。
func (s *Server) runScheduleNowHandler(c *gin.Context) {
	forceAll := c.Query("all") == "true" || c.Query("all") == "1"
	if forceAll {
		goSafe("schedule-run-now", func() { s.scheduler.RunAllNow(1) })
	} else {
		goSafe("schedule-run-now", func() { s.scheduler.RunOnce(true, 1) })
	}
	ok(c, gin.H{
		"triggered": true,
		"scope":     map[bool]string{true: "all_accounts", false: "enabled_only"}[forceAll],
	})
}

func (s *Server) getScheduleLogsHandler(c *gin.Context) {
	ok(c, s.scheduler.Logs())
}

func (s *Server) scheduleStatusHandler(c *gin.Context) {
	ok(c, s.scheduler.Status())
}
