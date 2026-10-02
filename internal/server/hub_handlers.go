/**
 * [INPUT]: 依赖 gin, icloud-hme/internal/store, icloud-hme/internal/scheduler
 * [OUTPUT]: 对外提供 Tags, Tokens, Leases, Schedules 的 HTTP Handler，令牌创建拒绝非法 JSON，支持原子标识创建/更新、母号引用保护、描述清空与调度参数校验
 * [POS]: internal/server 的中台功能路由处理器集合；scheduledTag 优先继承母号业务标签并回退 scheduled，切断与 AliasLabel 模板耦合
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"errors"
	"fmt"
	"io"
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
	tags, err := s.store.ListTags()
	if err != nil {
		failCode(c, http.StatusInternalServerError, "PERSISTENCE_ERROR", "读取业务标识失败")
		return
	}
	ok(c, tags)
}

func (s *Server) createTagHandler(c *gin.Context) {
	var req struct {
		Name        string `json:"name"`
		Tag         string `json:"tag"`
		Description string `json:"description"`
		Status      string `json:"status"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Tag) == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "业务标识 (tag) 不能为空")
		return
	}
	tag := store.BusinessTag{ID: store.NewBusinessTagID(), Tag: strings.TrimSpace(req.Tag),
		Name: strings.TrimSpace(req.Name), Description: req.Description, Status: req.Status,
		CreatedAt: time.Now().Format(time.RFC3339)}
	if tag.Name == "" {
		tag.Name = tag.Tag
	}
	if tag.Status == "" {
		tag.Status = "active"
	}
	if err := s.store.CreateTag(tag); err != nil {
		tagMutationFail(c, err)
		return
	}
	ok(c, tag)
}

func (s *Server) updateTagHandler(c *gin.Context) {
	var req struct {
		Tag         string  `json:"tag"`
		Name        string  `json:"name"`
		Status      string  `json:"status"`
		Description *string `json:"description"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "无效参数")
		return
	}
	tag, err := s.store.UpdateTag(c.Param("id"), func(tag *store.BusinessTag) {
		if req.Status != "" {
			tag.Status = req.Status
		}
		if req.Description != nil {
			tag.Description = *req.Description
		}
		if key := strings.TrimSpace(req.Tag); key != "" {
			tag.Tag = key
		}
		if name := strings.TrimSpace(req.Name); name != "" {
			tag.Name = name
		}
	})
	if err != nil {
		tagMutationFail(c, err)
		return
	}
	ok(c, tag)
}

func tagMutationFail(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrTagExists), isUniqueViolation(err):
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "业务标识已存在")
	case errors.Is(err, store.ErrTagNotFound):
		failCode(c, http.StatusNotFound, "NOT_FOUND", err.Error())
	case errors.Is(err, store.ErrTagInUse):
		failCode(c, http.StatusConflict, "TAG_IN_USE", err.Error())
	default:
		backendFail(c, err)
	}
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
	// 只回显安全前缀: 令牌本体仅在创建/轮换响应中出现一次，杜绝令牌泄露或互相收割
	tokens, err := s.store.ListTokens()
	if err != nil {
		failCode(c, http.StatusInternalServerError, "PERSISTENCE_ERROR", "读取令牌失败")
		return
	}
	ok(c, tokens)
}

type createTokenRequest struct {
	Name          string `json:"name"`
	Token         string `json:"token"`
	Scopes        string `json:"scopes"`
	ExpiresAt     string `json:"expires_at"`
	ExpiresInDays int    `json:"expires_in_days"`
}

func (s *Server) createTokenHandler(c *gin.Context) {
	var req createTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: 请求体必须为有效的 JSON")
		return
	}

	// 【安全红线】禁止继续接受管理员自定义 Token (防止 123456 / test 等弱口令)
	if strings.TrimSpace(req.Token) != "" {
		failCode(c, http.StatusBadRequest, "CUSTOM_TOKEN_NOT_ALLOWED", "不允许自定义令牌，系统将自动生成高熵令牌")
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		req.Name = "外部接入令牌"
	}

	// 缺省按最小权限发放: 对外令牌默认只能出号与取码，无法触达管理面
	scopes, err := normalizeScopes(req.Scopes)
	if err != nil {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	expiresAt := strings.TrimSpace(req.ExpiresAt)
	if req.ExpiresInDays > 0 && expiresAt == "" {
		expiresAt = time.Now().UTC().AddDate(0, 0, req.ExpiresInDays).Format(time.RFC3339)
	}
	if expiresAt != "" {
		if _, err := time.Parse(time.RFC3339, expiresAt); err != nil {
			failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "过期时间格式必须为 RFC3339")
			return
		}
	}

	created, err := s.store.CreateToken(req.Name, scopes, expiresAt)
	if err != nil {
		backendFail(c, err)
		return
	}
	ok(c, created)
}

func (s *Server) rotateTokenHandler(c *gin.Context) {
	id := c.Param("id")
	created, err := s.store.RotateToken(id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			failCode(c, http.StatusNotFound, "NOT_FOUND", "令牌不存在")
			return
		}
		if strings.Contains(err.Error(), "revoked") {
			failCode(c, http.StatusBadRequest, "TOKEN_REVOKED", "已注销令牌无法轮换")
			return
		}
		backendFail(c, err)
		return
	}
	ok(c, created)
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
	purge := c.Query("purge") == "true"
	var (
		affected bool
		err      error
	)
	if purge {
		// 物理清除 (Hard Delete): 从数据库中永久删除已作废历史令牌
		affected, err = s.store.PurgeToken(id)
	} else {
		// 软注销 (Soft Revoke): 保持 token identity 与历史审计记录，同时立即使认证失效
		affected, err = s.store.DeleteToken(id)
	}
	if err != nil {
		backendFail(c, err)
		return
	}
	if !affected {
		failCode(c, http.StatusNotFound, "NOT_FOUND", "令牌不存在")
		return
	}
	ok(c, gin.H{"deleted": true, "purged": purge})
}

// --- 已用别名流水 Leases ---

// recordLease 统一写出一条「已用别名」流水，是全站出号链路的唯一入账口径。
// 定时调度 / 一键出号 / 手动建号都必须经过这里，否则审计账本与业务 tag 对账会漏统计。
func (s *Server) recordLease(accountID, email, tag, tokenName string) error {
	if s.store == nil || email == "" {
		return fmt.Errorf("出号流水存储不可用或邮箱为空")
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
		return err
	}
	s.store.UpdateTagLastAssigned(tag)
	return nil
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

	records, total, err := s.store.ListLeases(aliasQ, tagQ, statusQ, limit, offset)
	if err != nil {
		backendFail(c, err)
		return
	}
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
	configs, err := s.store.ListScheduleConfigs()
	if err != nil {
		failCode(c, http.StatusInternalServerError, "PERSISTENCE_ERROR", "读取调度配置失败")
		return
	}
	ok(c, configs)
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

func validateEnabledScheduleConfig(cfg store.ScheduleConfig) error {
	if !cfg.Enabled {
		return nil
	}
	if err := validateJobScheduleParams(&upsertJobReq{
		Mode: cfg.Mode, DurationHours: cfg.DurationHours, StartTime: cfg.StartTime, EndTime: cfg.EndTime,
	}); err != nil {
		return err
	}
	if cfg.Mode == "duration" {
		if _, err := time.Parse(time.RFC3339, cfg.StartedAt); err != nil {
			return httpError("started_at 必须为 RFC3339 格式")
		}
	}
	return nil
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

	if req.HourlyQuota != nil {
		if *req.HourlyQuota < 1 || *req.HourlyQuota > 500 {
			failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "hourly_quota 必须在 1-500 之间")
			return
		}
	}
	if req.Mode != nil {
		mode := strings.ToLower(strings.TrimSpace(*req.Mode))
		switch mode {
		case "always", "daily_window", "duration":
		default:
			failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "mode 必须是 always / daily_window / duration")
			return
		}
	}
	for _, clock := range []*string{req.StartTime, req.EndTime} {
		if clock != nil && strings.TrimSpace(*clock) != "" && !clockPattern.MatchString(strings.TrimSpace(*clock)) {
			failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "start_time 和 end_time 必须为 HH:mm 格式")
			return
		}
	}
	if req.DurationHours != nil && (*req.DurationHours < 0 || *req.DurationHours > maxScheduleDurationHours) {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "duration_hours 超出有效时长范围")
		return
	}
	if req.StartedAt != nil && strings.TrimSpace(*req.StartedAt) != "" {
		if _, err := time.Parse(time.RFC3339, strings.TrimSpace(*req.StartedAt)); err != nil {
			failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "started_at 必须为 RFC3339 格式")
			return
		}
	}
	cfg, err := s.store.UpdateScheduleConfig(accountID, func(cfg *store.ScheduleConfig) error {
		if req.Enabled != nil {
			cfg.Enabled = *req.Enabled
		}
		if req.HourlyQuota != nil {
			cfg.HourlyQuota = *req.HourlyQuota
		}
		if req.AliasLabel != nil {
			cfg.AliasLabel = strings.TrimSpace(*req.AliasLabel)
		}
		if req.Mode != nil {
			cfg.Mode = strings.ToLower(strings.TrimSpace(*req.Mode))
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
		return validateEnabledScheduleConfig(*cfg)
	})
	if err != nil {
		var parameterErr jobParamError
		if errors.As(err, &parameterErr) {
			failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", parameterErr.Error())
			return
		}
		failCode(c, http.StatusInternalServerError, "PERSISTENCE_ERROR", "保存调度配置失败")
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
