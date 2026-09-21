/**
 * [INPUT]: 依赖 gin, net/http, strings, fmt, errors, time, strconv, icloud-hme/internal/auth, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 externalV2AllocateHandler, externalV2CreateVerificationRequestHandler, externalV2GetVerificationRequestHandler, externalV2GetOperationHandler
 * [POS]: internal/server 的 v2 外部自动化 API 规范门面 (PR-04/PR-05-1)，强制令牌幂等键、规范请求哈希、真实持久化取码请求与严格主体资源隔离
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/auth"
	"icloud-hme/internal/store"
)

// PR-07 §10.2: 有界并发与防过载保护
const (
	maxGlobalActiveVerificationRequests   = 1000
	maxPerTokenActiveVerificationRequests = 50
)

type externalV2AllocateReq struct {
	Tag       string `json:"tag"`
	Label     string `json:"label"`
	AccountID string `json:"account_id"`
}

func (s *Server) externalV2AllocateHandler(c *gin.Context) {
	p, exists := getPrincipal(c)
	if !exists || !p.CanAllocate() {
		failCode(c, http.StatusForbidden, "SCOPE_DENIED", "主体无权调用分配接口")
		return
	}

	var req externalV2AllocateReq
	if err := c.ShouldBindJSON(&req); err != nil && err.Error() != "EOF" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "请求体 JSON 格式错误: "+err.Error())
		return
	}

	// 普通外部令牌禁止随意指定 account_id (403 优先级高于参数校验)
	if req.AccountID != "" && p.Kind == auth.PrincipalToken && !p.IsAdmin() {
		failCode(c, http.StatusForbidden, "FORBIDDEN", "普通外部令牌禁止指定母号 account_id")
		return
	}

	idempKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if idempKey == "" {
		idempKey = strings.TrimSpace(c.Query("idempotency_key"))
	}

	// Section V: Idempotency-Key 对外部令牌必须必填
	if p.Kind == auth.PrincipalToken && idempKey == "" {
		failCode(c, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "v2 分配接口对外部令牌强制要求 Idempotency-Key")
		return
	}

	tag := strings.TrimSpace(req.Tag)
	if tag == "" {
		tag = "default"
	}
	req.Tag = tag

	// 业务标签权限范围核验
	if len(p.AllowedTags) > 0 {
		allowed := false
		for _, t := range p.AllowedTags {
			if strings.EqualFold(t, tag) {
				allowed = true
				break
			}
		}
		if !allowed {
			failCode(c, http.StatusForbidden, "FORBIDDEN", "指定的业务标签不在该主体授权范围内")
			return
		}
	}

	if s.store == nil {
		failCode(c, http.StatusServiceUnavailable, "ALLOCATION_STATE_NOT_READY", "存储层未就绪")
		return
	}

	// 规范化 request_hash：基于规范化字段组合，防重入与参数篡改
	reqHash := fmt.Sprintf("tag=%s&account_id=%s&label=%s", tag, strings.TrimSpace(req.AccountID), strings.TrimSpace(req.Label))

	alloc, op, err := s.store.ClaimInventoryAlias(
		c.Request.Context(),
		string(p.Kind),
		p.ID,
		"v2_allocate",
		idempKey,
		reqHash,
		tag,
		req.AccountID,
	)

	if err != nil {
		if errors.Is(err, store.ErrNoAvailableInventory) {
			c.Header("Retry-After", "60")
			failCode(c, http.StatusServiceUnavailable, "POOL_EMPTY", "暂无可用别名库存，请稍后重试")
			return
		}
		if errors.Is(err, store.ErrIdempotencyConflict) {
			failCode(c, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "相同幂等键使用不同请求参数冲突")
			return
		}
		if errors.Is(err, store.ErrOperationPending) {
			opID := ""
			if op != nil {
				opID = op.OperationID
			}
			c.JSON(http.StatusAccepted, gin.H{
				"code":    0,
				"message": "operation is pending",
				"data": gin.H{
					"operation_id": opID,
					"status":       "pending",
				},
			})
			return
		}
		failCode(c, http.StatusInternalServerError, "INTERNAL_ERROR", "认领别名失败: "+err.Error())
		return
	}

	// 注册别名路由
	if s.syncWorker != nil && alloc != nil {
		s.syncWorker.RegisterAliasAccount(alloc.AliasEmail, alloc.AccountID)
	}

	opID := ""
	if op != nil {
		opID = op.OperationID
	}

	ok(c, gin.H{
		"operation_id": opID,
		"lease_id":     alloc.AllocationID,
		"email":        alloc.AliasEmail,
		"alias_email":  alloc.AliasEmail,
		"account_id":   alloc.AccountID,
		"source":       "pool",
		"allocated_at": alloc.AllocatedAt,
		"status":       alloc.Status,
	})
}

type v2VerificationReq struct {
	LeaseID string `json:"lease_id"`
	Email   string `json:"email"`
}

func (s *Server) externalV2CreateVerificationRequestHandler(c *gin.Context) {
	p, exists := getPrincipal(c)
	if !exists || !p.CanVerify() {
		failCode(c, http.StatusForbidden, "SCOPE_DENIED", "主体无权创建验证码查询任务")
		return
	}

	var req v2VerificationReq
	if err := c.ShouldBindJSON(&req); err != nil && err.Error() != "EOF" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "请求体格式错误: "+err.Error())
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	leaseID := strings.TrimSpace(req.LeaseID)

	if email == "" && leaseID == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "lease_id 或 email 必填其一")
		return
	}

	if leaseID == "" && s.store != nil {
		alloc, err := s.store.GetPrincipalAllocation(c.Request.Context(), email, string(p.Kind), p.ID)
		if err != nil || alloc == nil {
			failCode(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "未找到指定别名或无权访问")
			return
		}
		leaseID = alloc.AllocationID
	}

	vreq, err := s.verifyService.CreateVerificationRequest(c.Request.Context(), p, leaseID)
	if err != nil {
		var be *BackendError
		if errors.As(err, &be) {
			failCode(c, be.Status, be.Code, be.Message)
			return
		}
		failCode(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}

	ok(c, gin.H{
		"request_id":           vreq.RequestID,
		"lease_id":             vreq.LeaseID,
		"alias_email":          vreq.AliasEmail,
		"status":               vreq.Status,
		"baseline_ready":       true,
		"baseline_provider":    vreq.BaselineProvider,
		"baseline_uidvalidity": vreq.BaselineUIDValidity,
		"baseline_uid":         vreq.BaselineUID,
		"created_at":           vreq.CreatedAt,
		"expires_at":           vreq.ExpiresAt,
	})
}

func (s *Server) externalV2GetVerificationRequestHandler(c *gin.Context) {
	p, exists := getPrincipal(c)
	if !exists || !p.CanVerify() {
		failCode(c, http.StatusForbidden, "SCOPE_DENIED", "主体无权读取验证码")
		return
	}

	requestID := strings.TrimSpace(c.Param("request_id"))
	if requestID == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "request_id 不能为空")
		return
	}

	timeoutSec := 0
	if raw := c.Query("timeout"); raw != "" {
		if t, err := strconv.Atoi(raw); err == nil && t > 0 {
			timeoutSec = t
		}
	}

	res, err := s.verifyService.GetVerificationResult(c.Request.Context(), p, requestID, timeoutSec)
	if err != nil {
		var be *BackendError
		if errors.As(err, &be) {
			failCode(c, be.Status, be.Code, be.Message)
			return
		}
		failCode(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}

	ok(c, res)
}

func (s *Server) externalV2GetOperationHandler(c *gin.Context) {
	p, exists := getPrincipal(c)
	if !exists {
		failCode(c, http.StatusUnauthorized, "AUTH_REQUIRED", "请先认证")
		return
	}

	opID := strings.TrimSpace(c.Param("operation_id"))
	if opID == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "operation_id 不能为空")
		return
	}
	if s.store == nil {
		failCode(c, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "存储层未就绪")
		return
	}

	op, err := s.store.GetOperation(c.Request.Context(), opID, string(p.Kind), p.ID)
	if err != nil {
		failCode(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "操作记录不存在或无权访问")
		return
	}

	ok(c, op)
}
