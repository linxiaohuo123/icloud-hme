/**
 * [INPUT]: 依赖 gin, net/http, strings, fmt, errors, time, strconv, icloud-hme/internal/auth, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 externalV2AllocateHandler, externalV2CreateVerificationRequestHandler, externalV2GetVerificationRequestHandler, externalV2GetOperationHandler
 * [POS]: internal/server 的 v2 自动化 API 门面，支持幂等任务与主体隔离；查询、创建及取码明确区分取消、超时和数据库故障，交付前复查原凭据
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/store"
)

// PR-07 §10.2: 有界并发与防过载保护
const (
	maxGlobalActiveVerificationRequests   = 1000
	maxPerTokenActiveVerificationRequests = 50
)

func getMaxGlobalActiveVerificationRequests() int {
	for _, envKey := range []string{"MAX_GLOBAL_ACTIVE_VREQ", "ICLOUD_HME_MAX_GLOBAL_ACTIVE_VREQ"} {
		if val := strings.TrimSpace(os.Getenv(envKey)); val != "" {
			if n, err := strconv.Atoi(val); err == nil && n > 0 {
				return n
			}
		}
	}
	return maxGlobalActiveVerificationRequests
}

func getMaxPerTokenActiveVerificationRequests() int {
	for _, envKey := range []string{"MAX_PER_TOKEN_ACTIVE_VREQ", "ICLOUD_HME_MAX_PER_TOKEN_ACTIVE_VREQ"} {
		if val := strings.TrimSpace(os.Getenv(envKey)); val != "" {
			if n, err := strconv.Atoi(val); err == nil && n > 0 {
				return n
			}
		}
	}
	return maxPerTokenActiveVerificationRequests
}

type externalV2AllocateReq struct {
	Tag       string `json:"tag"`
	Label     string `json:"label"`
	AccountID string `json:"account_id"`
	Mode      string `json:"mode"`
}

func (s *Server) externalV2AllocateHandler(c *gin.Context) {
	p, exists := getPrincipal(c)
	if !exists {
		failCode(c, http.StatusForbidden, "SCOPE_DENIED", "主体无权调用分配接口")
		return
	}

	var req externalV2AllocateReq
	if err := c.ShouldBindJSON(&req); err != nil && err.Error() != "EOF" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "请求体 JSON 格式错误: "+err.Error())
		return
	}

	idempKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if idempKey == "" {
		idempKey = strings.TrimSpace(c.Query("idempotency_key"))
	}

	allocRes, err := s.allocService.Allocate(c.Request.Context(), p, AllocationRequest{
		Tag:                req.Tag,
		Label:              req.Label,
		AccountID:          req.AccountID,
		Mode:               req.Mode,
		IdempotencyKey:     idempKey,
		RequireIdempotency: true,
	})

	if err != nil {
		if errors.Is(err, ErrInvalidAllocationMode) {
			failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "mode 必须是 pool、pool_only 或 create")
			return
		}
		if errors.Is(err, ErrScopeDenied) {
			failCode(c, http.StatusForbidden, "SCOPE_DENIED", "主体无权调用分配接口")
			return
		}
		if errors.Is(err, ErrIdempotencyKeyRequired) {
			failCode(c, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "v2 分配接口对外部令牌强制要求 Idempotency-Key")
			return
		}
		if errors.Is(err, ErrForbiddenAccountID) {
			failCode(c, http.StatusForbidden, "FORBIDDEN", "普通外部令牌禁止指定母号 account_id")
			return
		}
		if errors.Is(err, ErrTagNotAllowed) {
			failCode(c, http.StatusForbidden, "FORBIDDEN", "指定的业务标签不在该主体授权范围内")
			return
		}
		if errors.Is(err, ErrForbiddenRemoteCreation) {
			failCode(c, http.StatusServiceUnavailable, "ALLOCATION_STATE_NOT_READY", "现场按需创建能力未就绪，当前仅支持已验证库存池分配 (mode=pool)")
			return
		}
		if errors.Is(err, ErrPoolEmpty) || errors.Is(err, store.ErrNoAvailableInventory) {
			c.Header("Retry-After", "60")
			failCode(c, http.StatusServiceUnavailable, "POOL_EMPTY", "暂无可用别名库存，请稍后重试")
			return
		}
		if errors.Is(err, store.ErrIdempotencyConflict) {
			failCode(c, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "相同幂等键使用不同请求参数冲突")
			return
		}
		if errors.Is(err, store.ErrAllocationConflict) {
			failCode(c, http.StatusConflict, "ALLOCATION_CONFLICT", "别名分配发生冲突: "+err.Error())
			return
		}
		if errors.Is(err, store.ErrOperationPending) {
			opID := ""
			if allocRes != nil && allocRes.Operation != nil {
				opID = allocRes.Operation.OperationID
			}
			c.JSON(http.StatusAccepted, gin.H{
				"success": true,
				"data": gin.H{
					"operation_id": opID,
					"status":       "pending",
				},
			})
			return
		}
		if errors.Is(err, store.ErrOperationOutcomeUnknown) {
			opID := ""
			if allocRes != nil && allocRes.Operation != nil {
				opID = allocRes.Operation.OperationID
			}
			c.JSON(http.StatusBadGateway, apiResp{Success: false, Code: "UPSTREAM_OUTCOME_UNKNOWN", Message: "上游建号结果待核对", Data: gin.H{"operation_id": opID}})
			return
		}
		if errors.Is(err, ErrAllocationNotReady) {
			failCode(c, http.StatusServiceUnavailable, "ALLOCATION_STATE_NOT_READY", "存储层未就绪")
			return
		}
		var backendErr *BackendError
		if errors.As(err, &backendErr) {
			backendFail(c, err)
			return
		}
		failCode(c, http.StatusInternalServerError, "INTERNAL_ERROR", "认领别名失败: "+err.Error())
		return
	}

	opID := ""
	if allocRes.Operation != nil {
		opID = allocRes.Operation.OperationID
	}

	ok(c, gin.H{
		"operation_id":  opID,
		"lease_id":      allocRes.Allocation.AllocationID,
		"allocation_id": allocRes.Allocation.AllocationID,
		"email":         allocRes.Allocation.AliasEmail,
		"alias_email":   allocRes.Allocation.AliasEmail,
		"account_id":    allocRes.Allocation.AccountID,
		"source":        allocRes.Source,
		"allocated_at":  allocRes.Allocation.AllocatedAt,
		"status":        allocRes.Allocation.Status,
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

	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if idempotencyKey != "" {
		if len(idempotencyKey) > 128 {
			failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "Idempotency-Key 长度不能超过 128 字符")
			return
		}
		for i := 0; i < len(idempotencyKey); i++ {
			b := idempotencyKey[i]
			if b < 0x21 || b > 0x7E {
				failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "Idempotency-Key 必须全部为 ASCII 可打印字符")
				return
			}
		}
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

	if leaseID == "" {
		if s.store != nil {
			lookupCtx, cancelLookup := withShortPhaseTimeout(c.Request.Context(), 5*time.Second)
			alloc, err := s.store.GetPrincipalAllocation(lookupCtx, email, string(p.Kind), p.ID)
			cancelLookup()
			if err != nil {
				if errors.Is(err, context.Canceled) {
					failBackendError(c, classifyUpstreamErr("查询别名租约失败", err))
					return
				}
				if errors.Is(err, context.DeadlineExceeded) {
					failCode(c, http.StatusGatewayTimeout, "DEADLINE_EXCEEDED", "查询别名租约超时: "+err.Error())
					return
				}
				if !errors.Is(err, store.ErrAllocationNotFound) {
					failCode(c, http.StatusInternalServerError, "INTERNAL_ERROR", "查询别名租约失败: "+err.Error())
					return
				}
			}
			if alloc != nil {
				leaseID = alloc.AllocationID
			} else if p.IsAdmin() {
				leaseID = email
			} else {
				failCode(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "未找到指定别名或无权访问")
				return
			}
		} else {
			leaseID = email
		}
	}

	var idempotencyHash string
	if idempotencyKey != "" {
		sum := sha256.Sum256([]byte(fmt.Sprintf("v1|lease:%s|mailbox:INBOX", leaseID)))
		idempotencyHash = hex.EncodeToString(sum[:])
	}

	vreq, err := s.verifyService.CreateVerificationRequestWithIdempotency(c.Request.Context(), p, leaseID, idempotencyKey, idempotencyHash)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			failBackendError(c, classifyUpstreamErr("创建验证码任务失败", err))
			return
		}
		if errors.Is(err, store.ErrIdempotencyConflict) || errors.Is(err, ErrIdempotencyConflict) {
			failCode(c, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "相同 Idempotency-Key 使用不同请求参数产生冲突")
			return
		}
		var be *BackendError
		if errors.As(err, &be) {
			failBackendError(c, be)
			return
		}
		failCode(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}

	baselineReady := vreq.Status == "ready"
	if baselineReady && vreq.ExpiresAt != "" {
		if exp, perr := time.Parse(time.RFC3339, vreq.ExpiresAt); perr == nil && !time.Now().UTC().Before(exp) {
			baselineReady = false
		}
	}

	ok(c, gin.H{
		"request_id":           vreq.RequestID,
		"lease_id":             vreq.LeaseID,
		"alias_email":          vreq.AliasEmail,
		"status":               vreq.Status,
		"baseline_ready":       baselineReady,
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

	onWaitStart := func() {
		releaseInflight(c)
	}

	res, err := s.verifyService.GetVerificationResult(c.Request.Context(), p, requestID, timeoutSec, requestAPIKey(c), onWaitStart)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			failBackendError(c, classifyUpstreamErr("读取验证码任务失败", err))
			return
		}
		var be *BackendError
		if errors.As(err, &be) {
			failBackendError(c, be)
			return
		}
		failCode(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}

	ok(c, res)
}

func releaseInflight(c *gin.Context) {
	if c == nil {
		return
	}
	rc := http.NewResponseController(c.Writer)
	_ = rc.SetReadDeadline(time.Time{})
	if val, exists := c.Get(InflightTokenContextKey); exists {
		if tok, ok := val.(*InflightToken); ok && tok != nil {
			tok.Release()
		}
	}
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
