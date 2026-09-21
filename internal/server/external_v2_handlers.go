/**
 * [INPUT]: 依赖 gin, net/http, strings, fmt, errors, time, icloud-hme/internal/auth, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 externalV2AllocateHandler, externalV2CreateVerificationRequestHandler, externalV2GetVerificationRequestHandler, externalV2GetOperationHandler
 * [POS]: internal/server 的 v2 外部自动化 API 规范门面 (PR-04)，统一主体归属、强幂等性与资源级访问控制
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/auth"
	"icloud-hme/internal/store"
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

	idempKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if idempKey == "" {
		idempKey = strings.TrimSpace(c.Query("idempotency_key"))
	}

	var req externalV2AllocateReq
	if err := c.ShouldBindJSON(&req); err != nil && err.Error() != "EOF" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "请求体 JSON 格式错误: "+err.Error())
		return
	}

	tag := strings.TrimSpace(req.Tag)
	if tag == "" {
		tag = "default"
	}

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

	// 普通外部令牌禁止随意指定 account_id
	if req.AccountID != "" && p.Kind == auth.PrincipalToken && !p.IsAdmin() {
		failCode(c, http.StatusForbidden, "FORBIDDEN", "普通外部令牌禁止指定母号 account_id")
		return
	}

	if s.store == nil {
		failCode(c, http.StatusServiceUnavailable, "ALLOCATION_STATE_NOT_READY", "存储层未就绪")
		return
	}

	reqHash := fmt.Sprintf("alloc:%s:%s", tag, req.AccountID)
	alloc, err := s.store.ClaimInventoryAlias(
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
			c.Header("Retry-After", "30")
			failCode(c, http.StatusConflict, "POOL_EXHAUSTED", "暂无可用别名库存，请稍后重试")
			return
		}
		if errors.Is(err, store.ErrIdempotencyConflict) {
			failCode(c, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "相同幂等键使用不同请求参数冲突")
			return
		}
		if errors.Is(err, store.ErrOperationPending) {
			failCode(c, http.StatusConflict, "OPERATION_PENDING", "相同操作仍在处理中")
			return
		}
		failCode(c, http.StatusInternalServerError, "INTERNAL_ERROR", "认领别名失败: "+err.Error())
		return
	}

	// 注册别名路由
	if s.syncWorker != nil {
		accID, _ := s.store.FindAliasRoute(alloc.AliasEmail)
		if accID != "" {
			s.syncWorker.RegisterAliasAccount(alloc.AliasEmail, accID)
		}
	}

	ok(c, gin.H{
		"lease_id":     alloc.AllocationID,
		"email":        alloc.AliasEmail,
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

	if s.store == nil {
		failCode(c, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "存储层未就绪")
		return
	}

	// 资源归属核验：必须确认该别名归属于当前调用主体
	var alloc *store.AliasAllocation
	var err error
	if email != "" {
		alloc, err = s.store.GetPrincipalAllocation(c.Request.Context(), email, string(p.Kind), p.ID)
	} else {
		alloc, err = s.store.GetPrincipalAllocationByID(c.Request.Context(), leaseID, string(p.Kind), p.ID)
	}

	if err != nil || alloc == nil {
		failCode(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "未找到指定别名或无权访问")
		return
	}

	requestID := fmt.Sprintf("vreq_%d", time.Now().UnixNano())
	ok(c, gin.H{
		"request_id": requestID,
		"lease_id":   alloc.AllocationID,
		"email":      alloc.AliasEmail,
		"status":     "ready",
	})
}

func (s *Server) externalV2GetVerificationRequestHandler(c *gin.Context) {
	p, exists := getPrincipal(c)
	if !exists || !p.CanVerify() {
		failCode(c, http.StatusForbidden, "SCOPE_DENIED", "主体无权读取验证码")
		return
	}

	requestID := c.Param("request_id")
	email := strings.ToLower(strings.TrimSpace(c.Query("email")))

	if email != "" && p.Kind == auth.PrincipalToken && s.store != nil {
		_, err := s.store.GetPrincipalAllocation(c.Request.Context(), email, "token", p.ID)
		if err != nil {
			failCode(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "未找到指定别名或无权访问")
			return
		}
	}

	timeoutSec := 30
	if raw := c.Query("timeout"); raw != "" {
		if t, err := strconv.Atoi(raw); err == nil && t > 0 {
			if t > 120 {
				t = 120
			}
			timeoutSec = t
		}
	}

	if email == "" {
		// 若未提供 email，直接返回任务就绪状态
		ok(c, gin.H{
			"request_id": requestID,
			"status":     "ready",
		})
		return
	}

	subID, ch := s.eventBus.SubscribeWithFresh(email, false)
	defer s.eventBus.Unsubscribe(email, subID)

	if s.syncWorker != nil {
		s.syncWorker.Trigger()
	}

	timer := time.NewTimer(time.Duration(timeoutSec) * time.Second)
	defer timer.Stop()

	select {
	case item := <-ch:
		if p.Kind == auth.PrincipalToken && s.store != nil {
			reqKey := c.GetHeader("X-API-Key")
			if reqKey == "" {
				authHeader := c.GetHeader("Authorization")
				if strings.HasPrefix(authHeader, "Bearer ") {
					reqKey = strings.TrimPrefix(authHeader, "Bearer ")
				}
			}
			if _, _, _, tokOk := s.store.ValidateTokenPrincipal(reqKey); !tokOk {
				failCode(c, http.StatusUnauthorized, "REVOKED_TOKEN", "令牌已被撤销")
				return
			}
		}
		s.eventBus.ConsumeCache(email)
		ok(c, gin.H{
			"request_id": requestID,
			"email":      email,
			"code":       item.OTP.Code,
			"magic_link": item.OTP.MagicLink,
			"status":     "received",
		})
	case <-timer.C:
		ok(c, gin.H{
			"request_id": requestID,
			"email":      email,
			"status":     "pending",
		})
	case <-c.Request.Context().Done():
		return
	}
}

func (s *Server) externalV2GetOperationHandler(c *gin.Context) {
	p, exists := getPrincipal(c)
	if !exists {
		failCode(c, http.StatusUnauthorized, "AUTH_REQUIRED", "请先认证")
		return
	}

	opID := c.Param("operation_id")
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
