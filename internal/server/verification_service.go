/**
 * [INPUT]: 依赖 context, fmt, time, errors, icloud-hme/internal/auth, icloud-hme/internal/mail, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 VerificationService, NewVerificationService, VerificationResult
 * [POS]: internal/server 的取码与基线状态机应用服务 (PR-06 & PR-08 §11.2)，封装边界准备、冲突仲裁、事件总线唤醒与单事件精准消费
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"icloud-hme/internal/auth"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

// PR-07 & PR-08 错误定义
var (
	ErrServerBusy            = &BackendError{Status: http.StatusServiceUnavailable, Code: "SERVER_BUSY", Message: "全局活跃取码任务数已达上限，请稍后重试"}
	ErrTooManyRequests       = &BackendError{Status: http.StatusTooManyRequests, Code: "TOO_MANY_REQUESTS", Message: "当前令牌活跃取码任务数已达上限"}
	ErrConflictActiveRequest = &BackendError{Status: http.StatusConflict, Code: "CONFLICT", Message: "该租约已存在活跃的取码任务"}
	ErrBaselineUnavailable   = &BackendError{Status: http.StatusServiceUnavailable, Code: "BASELINE_UNAVAILABLE", Message: "无法获取邮件基线游标"}
	ErrScopeDenied           = &BackendError{Status: http.StatusForbidden, Code: "SCOPE_DENIED", Message: "主体无权操作验证码任务"}
	ErrTokenRevoked          = &BackendError{Status: http.StatusUnauthorized, Code: "TOKEN_REVOKED", Message: "令牌已失效或被撤销"}
	ErrUIDValidityChanged    = &BackendError{Status: http.StatusConflict, Code: "UIDVALIDITY_CHANGED", Message: "邮箱 UIDVALIDITY 改变，基线失效"}
	ErrVReqNotFound          = &BackendError{Status: http.StatusNotFound, Code: "RESOURCE_NOT_FOUND", Message: "未找到验证任务或无权访问"}
)

// VerificationResult 取码状态结果
type VerificationResult struct {
	RequestID  string `json:"request_id"`
	LeaseID    string `json:"lease_id"`
	AliasEmail string `json:"alias_email"`
	Status     string `json:"status"` // ready / pending / succeeded / expired / invalidated
	Code       string `json:"code,omitempty"`
	MagicLink  string `json:"magic_link,omitempty"`
	MessageRef string `json:"message_ref,omitempty"`
}

// VerificationService 统一管理验证码任务生命周期与事件唤醒
type VerificationService struct {
	be         Backend
	store      *store.Store
	eventBus   *mail.EventBus
	syncWorker *MailSyncWorker
}

func NewVerificationService(be Backend, st *store.Store, eb *mail.EventBus, sw *MailSyncWorker) *VerificationService {
	return &VerificationService{
		be:         be,
		store:      st,
		eventBus:   eb,
		syncWorker: sw,
	}
}

// CreateVerificationRequest 创建持久化取码任务并采集初始基线
func (s *VerificationService) CreateVerificationRequest(ctx context.Context, p auth.Principal, leaseID string) (*store.VerificationRequest, error) {
	if !p.CanVerify() {
		return nil, ErrScopeDenied
	}
	leaseID = strings.TrimSpace(leaseID)
	if leaseID == "" {
		return nil, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: "lease_id 不能为空"}
	}
	if s.store == nil {
		return nil, &BackendError{Status: http.StatusServiceUnavailable, Code: "SERVICE_UNAVAILABLE", Message: "存储层未就绪"}
	}

	// 1. 核验租约归属 (优先按 AllocationID 查询，兼容直接传入 email)
	var alloc *store.AliasAllocation
	var err error
	if strings.Contains(leaseID, "@") {
		alloc, err = s.store.GetPrincipalAllocation(ctx, leaseID, string(p.Kind), p.ID)
	} else {
		alloc, err = s.store.GetPrincipalAllocationByID(ctx, leaseID, string(p.Kind), p.ID)
		if alloc == nil {
			alloc, err = s.store.GetPrincipalAllocation(ctx, leaseID, string(p.Kind), p.ID)
		}
	}
	if (err != nil || alloc == nil) && p.IsAdmin() && strings.Contains(leaseID, "@") {
		normEmail := strings.ToLower(strings.TrimSpace(leaseID))
		accID := ""
		if s.syncWorker != nil {
			accID = s.syncWorker.GetAliasAccount(normEmail)
		}
		if accID == "" && s.store != nil {
			accID, _ = s.store.FindAliasRoute(normEmail)
		}
		if accID != "" {
			alloc = &store.AliasAllocation{
				AllocationID: "admin_" + normEmail,
				AliasEmail:   normEmail,
				AccountID:    accID,
				OwnerKind:    string(p.Kind),
				OwnerID:      p.ID,
				Status:       "allocated",
			}
			err = nil
		}
	}
	if err != nil || alloc == nil {
		return nil, ErrVReqNotFound
	}

	// 2. 采集基线游标
	provider, uidValidity, uidNext, bErr := s.be.GetMailboxBoundary(alloc.AccountID, "INBOX")
	if bErr != nil {
		var be *BackendError
		if errors.As(bErr, &be) {
			return nil, be
		}
		return nil, &BackendError{Status: http.StatusServiceUnavailable, Code: "BASELINE_UNAVAILABLE", Message: "无法获取邮件基线游标: " + bErr.Error()}
	}

	now := time.Now().UTC()
	vreq := &store.VerificationRequest{
		RequestID:           store.NewOpaqueID("vreq_"),
		PrincipalKind:       string(p.Kind),
		PrincipalID:         p.ID,
		LeaseID:             alloc.AllocationID,
		AliasEmail:          alloc.AliasEmail,
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    provider,
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: uidValidity,
		BaselineUID:         uidNext,
	}

	// 3. 原子化检查同 lease 冲突、容量上限与任务插入 (PR-08 Final Hardening §4: 消除 TOCTOU 竞争)
	if err := s.store.CreateVerificationRequestAtomic(ctx, vreq, maxGlobalActiveVerificationRequests, maxPerTokenActiveVerificationRequests); err != nil {
		if errors.Is(err, store.ErrConflictActiveRequest) {
			return nil, ErrConflictActiveRequest
		}
		if errors.Is(err, store.ErrServerBusy) {
			return nil, ErrServerBusy
		}
		if errors.Is(err, store.ErrTooManyRequests) {
			return nil, ErrTooManyRequests
		}
		return nil, &BackendError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "创建持久化验证任务失败: " + err.Error()}
	}

	return vreq, nil
}

// GetVerificationResult 读取验证码（支持超时长轮询与边界唤醒）
func (s *VerificationService) GetVerificationResult(ctx context.Context, p auth.Principal, requestID string, timeoutSec int) (*VerificationResult, error) {
	if !p.CanVerify() {
		return nil, ErrScopeDenied
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: "request_id 不能为空"}
	}
	if s.store == nil {
		return nil, &BackendError{Status: http.StatusServiceUnavailable, Code: "SERVICE_UNAVAILABLE", Message: "存储层未就绪"}
	}

	// 唤醒前复查令牌撤销状态 (PR-06 V09, PR-04 A05)
	if p.Kind == auth.PrincipalToken {
		tok, tokErr := s.store.GetToken(ctx, p.ID)
		if tokErr != nil || tok == nil {
			return nil, ErrTokenRevoked
		}
	}

	vreq, err := s.store.GetVerificationRequest(ctx, requestID, string(p.Kind), p.ID)
	if err != nil || vreq == nil {
		return nil, ErrVReqNotFound
	}

	// 1. 计算受限等待窗口 (PR-06 §9.3, Issue 8: 等待时间严格受限于 expires_at)
	maxTimeout := 120 * time.Second
	waitDuration := time.Duration(timeoutSec) * time.Second
	if waitDuration < 0 {
		waitDuration = 0
	}
	if waitDuration > maxTimeout {
		waitDuration = maxTimeout
	}

	if vreq.ExpiresAt != "" {
		if expTime, parseErr := time.Parse(time.RFC3339, vreq.ExpiresAt); parseErr == nil {
			remaining := time.Until(expTime)
			if remaining <= 0 {
				if vreq.Status != "succeeded" && vreq.Status != "expired" && vreq.Status != "invalidated" {
					curReq, won, expErr := s.store.ExpireVerificationRequest(ctx, vreq.RequestID)
					if expErr != nil {
						return nil, &BackendError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "更新取码过期状态失败: " + expErr.Error()}
					}
					if curReq != nil {
						vreq = curReq
					} else if won {
						vreq.Status = "expired"
					}
				}
			} else if remaining < waitDuration {
				waitDuration = remaining
			}
		}
	}

	// 2. 幂等返回已有最终态
	if vreq.Status == "succeeded" || vreq.Status == "expired" || vreq.Status == "invalidated" {
		return mapVerificationRecordToResult(vreq)
	}

	// 3. 基于 INBOX Mailbox、UIDVALIDITY 与 UIDNEXT 边界订阅事件 (PR-06 §9.2, 9.5, Issue 4 & 5)
	mailbox := vreq.BaselineMailbox
	if mailbox == "" {
		mailbox = "INBOX"
	}
	subID, ch := s.eventBus.SubscribeWithBoundary(vreq.AliasEmail, mailbox, uint32(vreq.BaselineUIDValidity), uint32(vreq.BaselineUID))
	defer s.eventBus.Unsubscribe(vreq.AliasEmail, subID)

	if s.syncWorker != nil {
		s.syncWorker.Trigger()
	}

	handleItem := func(item *mail.CachedOTP) (*VerificationResult, error) {
		// 原子核查 Token 撤销状态
		if p.Kind == auth.PrincipalToken {
			tok, tokErr := s.store.GetToken(ctx, p.ID)
			if tokErr != nil || tok == nil {
				return nil, ErrTokenRevoked
			}
		}
		// UIDVALIDITY 突变检测 (Issue 5 & 6)
		if item.UIDValidity != 0 && vreq.BaselineUIDValidity != 0 && item.UIDValidity != uint32(vreq.BaselineUIDValidity) {
			curReq, won, invErr := s.store.InvalidateVerificationRequest(ctx, vreq.RequestID)
			if invErr != nil {
				return nil, &BackendError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "持久化代际失效失败: " + invErr.Error()}
			}
			if won {
				return nil, ErrUIDValidityChanged
			}
			// 代际失效 CAS 输给并发操作 (expired 或 succeeded)，严格返回胜出的真实终态，不得覆盖掩盖
			if curReq != nil {
				return mapVerificationRecordToResult(curReq)
			}
			return nil, ErrUIDValidityChanged
		}

		code := item.OTP.Code
		magicLink := item.OTP.MagicLink
		// 终态原子 CAS (P0-3, PR-04B): 独立持久化 Code 与 MagicLink，不再粗暴相互覆盖
		nowUTC := time.Now().UTC()
		curReq, won, err := s.store.CompleteVerificationRequestResult(ctx, vreq.RequestID, store.VerificationCompletion{
			Code:            code,
			MagicLink:       magicLink,
			MatchedEventRef: item.EventID,
		}, nowUTC)
		if err != nil {
			return nil, &BackendError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "持久化验证码终态失败: " + err.Error()}
		}
		if won {
			// 仅在明确落库成功后才消费事件
			s.eventBus.ConsumeEvent(vreq.AliasEmail, item.EventID)
			return &VerificationResult{
				RequestID:  vreq.RequestID,
				LeaseID:    vreq.LeaseID,
				AliasEmail: vreq.AliasEmail,
				Code:       code,
				MagicLink:  magicLink,
				MessageRef: item.EventID,
				Status:     "succeeded",
			}, nil
		}

		// CAS 未中 (已被并发完成或已过期/失效，返回数据库真实终态)
		if curReq != nil {
			return mapVerificationRecordToResult(curReq)
		}
		return &VerificationResult{
			RequestID:  vreq.RequestID,
			LeaseID:    vreq.LeaseID,
			AliasEmail: vreq.AliasEmail,
			Status:     "pending",
		}, nil
	}

	if waitDuration <= 0 {
		select {
		case item := <-ch:
			return handleItem(item)
		default:
			if freshReq, ferr := s.store.GetVerificationRequest(ctx, requestID, string(p.Kind), p.ID); ferr == nil && freshReq != nil {
				if freshReq.Status == "succeeded" || freshReq.Status == "expired" || freshReq.Status == "invalidated" {
					return mapVerificationRecordToResult(freshReq)
				}
			}
			return &VerificationResult{
				RequestID:  vreq.RequestID,
				LeaseID:    vreq.LeaseID,
				AliasEmail: vreq.AliasEmail,
				Status:     "pending",
			}, nil
		}
	}

	timer := time.NewTimer(waitDuration)
	defer timer.Stop()

	select {
	case item := <-ch:
		return handleItem(item)
	case <-timer.C:
		// 定时器触发后再次校验是否超时过期 (Issue 8)
		if vreq.ExpiresAt != "" {
			if expTime, parseErr := time.Parse(time.RFC3339, vreq.ExpiresAt); parseErr == nil {
				if time.Now().UTC().After(expTime) {
					curReq, _, expErr := s.store.ExpireVerificationRequest(ctx, vreq.RequestID)
					if expErr != nil {
						return nil, &BackendError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "更新取码过期状态失败: " + expErr.Error()}
					}
					if curReq != nil {
						return mapVerificationRecordToResult(curReq)
					}
					return &VerificationResult{
						RequestID:  vreq.RequestID,
						LeaseID:    vreq.LeaseID,
						AliasEmail: vreq.AliasEmail,
						Status:     "expired",
					}, nil
				}
			}
		}
		// 即使未到 ExpiresAt，定时器触发后核查 DB 是否已产生并发落地的真实终态 (统一 handleItem 终态映射)
		if freshReq, ferr := s.store.GetVerificationRequest(ctx, requestID, string(p.Kind), p.ID); ferr == nil && freshReq != nil {
			if freshReq.Status == "succeeded" || freshReq.Status == "expired" || freshReq.Status == "invalidated" {
				return mapVerificationRecordToResult(freshReq)
			}
		}
		return &VerificationResult{
			RequestID:  vreq.RequestID,
			LeaseID:    vreq.LeaseID,
			AliasEmail: vreq.AliasEmail,
			Status:     "pending",
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// mapVerificationRecordToResult 将数据库 VerificationRequest 映射为 VerificationResult 或失效错误
func mapVerificationRecordToResult(rec *store.VerificationRequest) (*VerificationResult, error) {
	if rec == nil {
		return nil, ErrVReqNotFound
	}
	switch rec.Status {
	case "succeeded":
		magicLink := rec.MagicLink
		if magicLink == "" && (strings.HasPrefix(rec.Code, "http://") || strings.HasPrefix(rec.Code, "https://")) {
			magicLink = rec.Code
		}
		return &VerificationResult{
			RequestID:  rec.RequestID,
			LeaseID:    rec.LeaseID,
			AliasEmail: rec.AliasEmail,
			Code:       rec.Code,
			MagicLink:  magicLink,
			MessageRef: rec.MatchedEventRef,
			Status:     "succeeded",
		}, nil
	case "expired":
		return &VerificationResult{
			RequestID:  rec.RequestID,
			LeaseID:    rec.LeaseID,
			AliasEmail: rec.AliasEmail,
			Status:     "expired",
		}, nil
	case "invalidated":
		return nil, ErrUIDValidityChanged
	default:
		return &VerificationResult{
			RequestID:  rec.RequestID,
			LeaseID:    rec.LeaseID,
			AliasEmail: rec.AliasEmail,
			Status:     rec.Status,
		}, nil
	}
}
