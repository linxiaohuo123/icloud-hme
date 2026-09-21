/**
 * [INPUT]: 依赖 internal/store, github.com/gin-gonic/gin, time, strings, net/http
 * [OUTPUT]: 对外提供 listCreateJobsHandler, upsertCreateJobHandler, pauseCreateJobHandler, resumeCreateJobHandler, deleteCreateJobHandler
 * [POS]: internal/server 的自动化创建作业标准 RESTful 门面，对外提供兼容主流生态的生命周期控制接口
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/store"
)

var clockPattern = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

type createJobResp struct {
	ID            string `json:"id"`
	AccountID     string `json:"account_id"`
	LabelPrefix   string `json:"label_prefix,omitempty"`
	Mode          string `json:"mode"`
	Status        string `json:"status"`
	DurationHours int    `json:"duration_hours,omitempty"`
	StartTime     string `json:"start_time,omitempty"`
	EndTime       string `json:"end_time,omitempty"`
	CreatedCount  int    `json:"created_count"`
	NextRunAt     string `json:"next_run_at,omitempty"`
	CreatedAt     string `json:"created_at,omitempty"`
	UpdatedAt     string `json:"updated_at,omitempty"`
}

type upsertJobReq struct {
	ID            string `json:"id,omitempty"`
	AccountID     string `json:"account_id" binding:"required"`
	LabelPrefix   string `json:"label_prefix"`
	Mode          string `json:"mode" binding:"required"`
	DurationHours int    `json:"duration_hours,omitempty"`
	StartTime     string `json:"start_time,omitempty"`
	EndTime       string `json:"end_time,omitempty"`
}

// ====================================================================
// 辅助判定与状态推导 (无特殊情况设计)
// ====================================================================

func isJobCompleted(cfg store.ScheduleConfig, now time.Time) bool {
	if cfg.Mode != "duration" || cfg.DurationHours <= 0 || strings.TrimSpace(cfg.StartedAt) == "" {
		return false
	}
	started, err := time.Parse(time.RFC3339, cfg.StartedAt)
	if err != nil {
		return false
	}
	return now.After(started.Add(time.Duration(cfg.DurationHours) * time.Hour))
}

func jobStatus(cfg store.ScheduleConfig, now time.Time) string {
	if !cfg.Enabled {
		return "paused"
	}
	if isJobCompleted(cfg, now) {
		return "completed"
	}
	return "running"
}

func toJobResp(cfg store.ScheduleConfig, now time.Time) createJobResp {
	status := jobStatus(cfg, now)
	nextRun := ""
	if status == "running" {
		nextRun = now.Add(1 * time.Minute).Format(time.RFC3339)
	}
	return createJobResp{
		ID:            "job_" + cfg.AccountID,
		AccountID:     cfg.AccountID,
		LabelPrefix:   cfg.AliasLabel,
		Mode:          cfg.Mode,
		Status:        status,
		DurationHours: cfg.DurationHours,
		StartTime:     cfg.StartTime,
		EndTime:       cfg.EndTime,
		CreatedCount:  cfg.CurrentHourCount,
		NextRunAt:     nextRun,
		CreatedAt:     cfg.StartedAt,
		UpdatedAt:     cfg.LastRunAt,
	}
}

func resolveAccountIDFromJob(jobID string) string {
	return strings.TrimPrefix(strings.TrimSpace(jobID), "job_")
}

// hasAccount 判断账号是否存在。
//
// 必须走 O(1) 的按 ID 直查:此前实现是遍历 ListAccounts()，
// 2000 账号 × 200 别名下单次调用要 5.5ms / 400KB 分配，而这个函数在每个作业接口上都会被调用。
func (s *Server) hasAccount(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	_, err := s.be.GetAccount(id)
	return err == nil
}

// ====================================================================
// Job 门面路由 Handler
// ====================================================================

// listCreateJobsHandler 处理 GET /api/create/jobs。
func (s *Server) listCreateJobsHandler(c *gin.Context) {
	accountID := strings.TrimSpace(c.Query("account_id"))
	now := time.Now()

	if accountID != "" {
		if !s.hasAccount(accountID) {
			failCode(c, http.StatusNotFound, "NOT_FOUND", "账号不存在")
			return
		}
		cfg := s.store.GetScheduleConfig(accountID)
		rem := s.store.RemainingQuota(accountID)
		ok(c, gin.H{
			"remaining_this_hour": rem,
			"jobs":                []createJobResp{toJobResp(cfg, now)},
		})
		return
	}

	accounts := s.be.ListAccounts()
	jobs := make([]createJobResp, 0, len(accounts))
	for _, acc := range accounts {
		cfg := s.store.GetScheduleConfig(acc.ID)
		if cfg.Enabled || cfg.Mode != "always" || cfg.DurationHours > 0 || cfg.StartTime != "" {
			jobs = append(jobs, toJobResp(cfg, now))
		}
	}
	ok(c, gin.H{"jobs": jobs})
}

// upsertCreateJobHandler 处理 POST /api/create/jobs。
func (s *Server) upsertCreateJobHandler(c *gin.Context) {
	var req upsertJobReq
	if err := c.ShouldBindJSON(&req); err != nil {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: account_id 与 mode 必填")
		return
	}
	if !s.hasAccount(req.AccountID) {
		failCode(c, http.StatusNotFound, "NOT_FOUND", "账号不存在")
		return
	}
	if err := validateJobScheduleParams(&req); err != nil {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	now := time.Now()
	cfg := s.store.GetScheduleConfig(req.AccountID)
	cfg.Enabled = true
	cfg.Mode = req.Mode
	cfg.AliasLabel = req.LabelPrefix
	cfg.DurationHours = req.DurationHours
	cfg.StartTime = req.StartTime
	cfg.EndTime = req.EndTime
	if req.Mode == "duration" {
		cfg.StartedAt = now.Format(time.RFC3339)
	} else {
		cfg.StartedAt = ""
	}

	_ = s.store.SaveScheduleConfig(cfg)
	ok(c, toJobResp(cfg, now))
}

func validateJobScheduleParams(req *upsertJobReq) error {
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "time_window" {
		mode = "daily_window"
	} else if mode == "continuous" {
		mode = "always"
	}
	req.Mode = mode

	switch req.Mode {
	case "duration":
		if req.DurationHours <= 0 {
			return httpError("duration_hours 必须大于 0")
		}
	case "daily_window":
		if !clockPattern.MatchString(req.StartTime) || !clockPattern.MatchString(req.EndTime) {
			return httpError("start_time 和 end_time 必须为 HH:mm 格式")
		}
	case "always":
		// 合法
	default:
		return httpError("mode 必须是 duration, daily_window (或 time_window), always (或 continuous)")
	}
	return nil
}

type jobParamError string

func (e jobParamError) Error() string { return string(e) }
func httpError(msg string) error      { return jobParamError(msg) }

// pauseCreateJobHandler 处理 POST /api/create/jobs/:id/pause。
func (s *Server) pauseCreateJobHandler(c *gin.Context) {
	accID := resolveAccountIDFromJob(c.Param("id"))
	if !s.hasAccount(accID) {
		failCode(c, http.StatusNotFound, "NOT_FOUND", "任务或账号不存在")
		return
	}
	cfg := s.store.GetScheduleConfig(accID)
	cfg.Enabled = false
	_ = s.store.SaveScheduleConfig(cfg)
	ok(c, toJobResp(cfg, time.Now()))
}

// resumeCreateJobHandler 处理 POST /api/create/jobs/:id/resume。
func (s *Server) resumeCreateJobHandler(c *gin.Context) {
	accID := resolveAccountIDFromJob(c.Param("id"))
	if !s.hasAccount(accID) {
		failCode(c, http.StatusNotFound, "NOT_FOUND", "任务或账号不存在")
		return
	}
	now := time.Now()
	cfg := s.store.GetScheduleConfig(accID)
	cfg.Enabled = true
	if cfg.Mode == "duration" {
		cfg.StartedAt = now.Format(time.RFC3339)
	}
	_ = s.store.SaveScheduleConfig(cfg)
	ok(c, toJobResp(cfg, now))
}

// deleteCreateJobHandler 处理 DELETE /api/create/jobs/:id。
func (s *Server) deleteCreateJobHandler(c *gin.Context) {
	id := c.Param("id")
	accID := resolveAccountIDFromJob(id)
	if !s.hasAccount(accID) {
		failCode(c, http.StatusNotFound, "NOT_FOUND", "任务或账号不存在")
		return
	}
	if s.store != nil {
		_ = s.store.DeleteScheduleConfig(accID)
	}
	ok(c, gin.H{"id": id, "deleted": true})
}
