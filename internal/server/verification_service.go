/**
 * [INPUT]: 依赖 context, fmt, time, errors, sync, icloud-hme/internal/auth, icloud-hme/internal/mail, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 VerificationService, NewVerificationService, VerificationResult
 * [POS]: internal/server 的取码状态机应用服务，数据库与管理员路由查询均使用有限阶段预算，基线准备与原子完成响应取消，持久化物理邮箱来源，订阅和交付前复查来源及令牌凭据，旧代际事件以当前邮箱边界确认后才失效、忽略后回查缓存与数据库，基线母号删除即失效，存储故障显式报错
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
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
	ErrUIDValidityChanged    = &BackendError{Status: http.StatusConflict, Code: "UIDVALIDITY_CHANGED", Message: "邮箱来源或 UIDVALIDITY 改变，基线失效"}
	ErrVReqNotFound          = &BackendError{Status: http.StatusNotFound, Code: "RESOURCE_NOT_FOUND", Message: "未找到验证任务或无权访问"}
	ErrIdempotencyConflict   = &BackendError{Status: http.StatusConflict, Code: "IDEMPOTENCY_CONFLICT", Message: "幂等键参数冲突，与既有任务参数不匹配"}
	ErrTooManyWaiters        = &BackendError{Status: http.StatusTooManyRequests, Code: "TOO_MANY_WAITERS_PER_KEY", Message: "该任务并发等待连接数已达上限 (最大 8)"}
)

// errStaleVerificationEvent 标记已由当前邮箱边界确认早于基线的旧代际事件，等待方继续等待后续事件。
var errStaleVerificationEvent = errors.New("stale verification event")

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

const defaultVerificationBaselineSlots = 8

// keyedMutexEntry 为单个 key 的锁条目，附带引用计数
type keyedMutexEntry struct {
	sem chan struct{}
	ref int
}

// keyedMutex 实现支持 context 取消且无死锁、自动回收的轻量 keyed mutex (PR-05 F09)
type keyedMutex struct {
	mu      sync.Mutex
	entries map[string]*keyedMutexEntry
}

func newKeyedMutex() *keyedMutex {
	return &keyedMutex{
		entries: make(map[string]*keyedMutexEntry),
	}
}

func (km *keyedMutex) Lock(ctx context.Context, key string) (func(), error) {
	km.mu.Lock()
	entry, ok := km.entries[key]
	if !ok {
		entry = &keyedMutexEntry{
			sem: make(chan struct{}, 1),
			ref: 0,
		}
		entry.sem <- struct{}{}
		km.entries[key] = entry
	}
	entry.ref++
	sem := entry.sem
	km.mu.Unlock()

	select {
	case <-sem:
		return func() {
			sem <- struct{}{}
			km.mu.Lock()
			entry.ref--
			if entry.ref <= 0 {
				delete(km.entries, key)
			}
			km.mu.Unlock()
		}, nil
	case <-ctx.Done():
		km.mu.Lock()
		entry.ref--
		if entry.ref <= 0 {
			delete(km.entries, key)
		}
		km.mu.Unlock()
		return nil, ctx.Err()
	}
}

// VerificationService 统一管理验证码任务生命周期与事件唤醒
type VerificationService struct {
	be               Backend
	store            *store.Store
	eventBus         *mail.EventBus
	syncWorker       *MailSyncWorker
	baselineSlots    chan struct{}
	leaseLocks       *keyedMutex
	idempotencyLocks *keyedMutex
	limiter          *RequestLimiter
	maxGlobal        int
	maxPerPrincipal  int
	serverCtx        context.Context
	srvMu            sync.RWMutex
}

func NewVerificationService(be Backend, st *store.Store, eb *mail.EventBus, sw *MailSyncWorker) *VerificationService {
	return &VerificationService{
		be:               be,
		store:            st,
		eventBus:         eb,
		syncWorker:       sw,
		baselineSlots:    make(chan struct{}, defaultVerificationBaselineSlots),
		leaseLocks:       newKeyedMutex(),
		idempotencyLocks: newKeyedMutex(),
		maxGlobal:        getMaxGlobalActiveVerificationRequests(),
		maxPerPrincipal:  getMaxPerTokenActiveVerificationRequests(),
	}
}

// SetServerContext 注入服务端生命周期 Context，用于在优雅停机时主动取消已接受的等待连接 (S04)
func (s *VerificationService) SetServerContext(ctx context.Context) {
	s.srvMu.Lock()
	defer s.srvMu.Unlock()
	s.serverCtx = ctx
}

func (s *VerificationService) getServerContext() context.Context {
	s.srvMu.RLock()
	defer s.srvMu.RUnlock()
	return s.serverCtx
}

// SetRequestLimiter 设置请求与等待限制器
func (s *VerificationService) SetRequestLimiter(limiter *RequestLimiter) {
	s.limiter = limiter
}

// SetBaselineSlotsForTest 允许测试动态注入受限的 baseline 并发槽位数
func (s *VerificationService) SetBaselineSlotsForTest(slots int) {
	s.baselineSlots = make(chan struct{}, slots)
}

// SetMaxLimitsForTest 允许测试动态注入全局与每主体活跃限制
func (s *VerificationService) SetMaxLimitsForTest(maxGlobal, maxPerPrincipal int) {
	s.maxGlobal = maxGlobal
	s.maxPerPrincipal = maxPerPrincipal
}

func (s *VerificationService) limits() (int, int) {
	mg := s.maxGlobal
	if mg <= 0 {
		mg = getMaxGlobalActiveVerificationRequests()
	}
	mp := s.maxPerPrincipal
	if mp <= 0 {
		mp = getMaxPerTokenActiveVerificationRequests()
	}
	return mg, mp
}

func (s *VerificationService) checkAdmission(ctx context.Context, principalKind, principalID, leaseID string) error {
	if s.store == nil {
		return &BackendError{Status: http.StatusServiceUnavailable, Code: "SERVICE_UNAVAILABLE", Message: "存储层未就绪"}
	}
	mg, mp := s.limits()
	err := s.store.CheckVerificationRequestAdmission(ctx, principalKind, principalID, leaseID, mg, mp)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if errors.Is(err, store.ErrConflictActiveRequest) {
			return ErrConflictActiveRequest
		}
		if errors.Is(err, store.ErrServerBusy) {
			return ErrServerBusy
		}
		if errors.Is(err, store.ErrTooManyRequests) {
			return ErrTooManyRequests
		}
		return &BackendError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "准入预检失败: " + err.Error()}
	}
	return nil
}

// CreateVerificationRequest 创建持久化取码任务并采集初始基线 (PR-05 F09/F10 严格准入与边界取消)
func (s *VerificationService) CreateVerificationRequest(ctx context.Context, p auth.Principal, leaseID string) (*store.VerificationRequest, error) {
	return s.CreateVerificationRequestWithIdempotency(ctx, p, leaseID, "", "")
}

// CreateVerificationRequestWithIdempotency 创建持久化取码任务，支持可选 Idempotency-Key 重放已恢复任务 (PR-CONCURRENCY T11)。
func (s *VerificationService) CreateVerificationRequestWithIdempotency(ctx context.Context, p auth.Principal, leaseID, idempotencyKey, idempotencyHash string) (*store.VerificationRequest, error) {
	// 1. CanVerify
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

	// 方案第 12 节固定锁顺序: 1. 主体+创建键锁 -> 2. lease 锁
	// 针对带 Idempotency-Key 请求，先获取排他键锁并在锁内查重，同键并发请求不重复打基线，异参请求明确 409
	if idempotencyKey != "" {
		idempLockKey := fmt.Sprintf("%s:%s:%s", p.Kind, p.ID, idempotencyKey)
		lockCtx, cancelLock := withShortPhaseTimeout(ctx, 5*time.Second)
		unlockKey, err := s.idempotencyLocks.Lock(lockCtx, idempLockKey)
		cancelLock()
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			return nil, &BackendError{Status: http.StatusServiceUnavailable, Code: "SERVER_BUSY", Message: "获取幂等锁超时"}
		}
		defer unlockKey()

		// 锁内权威查重：同主体同键若已存在，直接校验指纹并恢复，无需打 IMAP 或新建基线
		dbCtx, cancelDB := withShortPhaseTimeout(ctx, 5*time.Second)
		existing, err := s.store.GetVerificationRequestByIdempotencyKey(dbCtx, string(p.Kind), p.ID, idempotencyKey)
		cancelDB()
		if err != nil && !errors.Is(err, store.ErrVerificationRequestNotFound) {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			return nil, &BackendError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "幂等查询失败: " + err.Error()}
		}
		if existing != nil {
			if existing.IdempotencyHash != idempotencyHash {
				return nil, ErrIdempotencyConflict
			}
			return existing, nil
		}
	}

	// 2. lease ownership validation (优先按 AllocationID 查询，兼容直接传入 email)
	allocCtx, cancelAlloc := withShortPhaseTimeout(ctx, 5*time.Second)
	var alloc *store.AliasAllocation
	var allocErr error
	if strings.Contains(leaseID, "@") {
		alloc, allocErr = s.store.GetPrincipalAllocation(allocCtx, leaseID, string(p.Kind), p.ID)
	} else {
		alloc, allocErr = s.store.GetPrincipalAllocationByID(allocCtx, leaseID, string(p.Kind), p.ID)
		if alloc == nil && errors.Is(allocErr, store.ErrAllocationNotFound) {
			alloc, allocErr = s.store.GetPrincipalAllocation(allocCtx, leaseID, string(p.Kind), p.ID)
		}
	}
	cancelAlloc()

	// 区分真实数据库故障、取消与资源不存在 (S06)
	if allocErr != nil && !errors.Is(allocErr, store.ErrAllocationNotFound) {
		if errors.Is(allocErr, context.Canceled) || errors.Is(allocErr, context.DeadlineExceeded) {
			return nil, allocErr
		}
		return nil, &BackendError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "查询租约分配失败: " + allocErr.Error()}
	}

	// 仅在明确无记录且属于管理员请求带有 @ 时尝试路由回退
	if alloc == nil && p.IsAdmin() && strings.Contains(leaseID, "@") {
		normEmail := strings.ToLower(strings.TrimSpace(leaseID))
		accID := ""
		if s.syncWorker != nil {
			accID = s.syncWorker.GetAliasAccount(normEmail)
		}
		if accID == "" && s.store != nil {
			var rErr error
			routeCtx, cancelRoute := withShortPhaseTimeout(ctx, 5*time.Second)
			accID, rErr = s.store.FindAliasRouteContext(routeCtx, normEmail)
			cancelRoute()
			if rErr != nil && !errors.Is(rErr, sql.ErrNoRows) {
				if errors.Is(rErr, context.Canceled) || errors.Is(rErr, context.DeadlineExceeded) {
					return nil, rErr
				}
				return nil, &BackendError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "查询别名路由失败: " + rErr.Error()}
			}
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
		}
	}
	if alloc == nil {
		return nil, ErrVReqNotFound
	}

	// 3. cheap DB admission preflight (碰任何网络前快速拒绝超限请求)
	preflightCtx, cancelPreflight := withShortPhaseTimeout(ctx, 5*time.Second)
	err := s.checkAdmission(preflightCtx, string(p.Kind), p.ID, alloc.AllocationID)
	cancelPreflight()
	if err != nil {
		return nil, err
	}

	// 4. acquire per-lease preparation serialization (同 lease 串行化，防止并发重复打 IMAP)
	leaseLockCtx, cancelLeaseLock := withShortPhaseTimeout(ctx, 5*time.Second)
	unlockLease, err := s.leaseLocks.Lock(leaseLockCtx, alloc.AllocationID)
	cancelLeaseLock()
	if err != nil {
		return nil, err
	}
	defer unlockLease()

	// 锁内再次检查幂等，防止并发创建
	if idempotencyKey != "" {
		chkCtx, cancelChk := withShortPhaseTimeout(ctx, 5*time.Second)
		existing, err := s.store.GetVerificationRequestByIdempotencyKey(chkCtx, string(p.Kind), p.ID, idempotencyKey)
		cancelChk()
		if err != nil && !errors.Is(err, store.ErrVerificationRequestNotFound) {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			return nil, &BackendError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "幂等查询失败: " + err.Error()}
		}
		if existing != nil {
			if existing.IdempotencyHash != idempotencyHash {
				return nil, ErrIdempotencyConflict
			}
			return existing, nil
		}
	}

	// 5. acquire bounded baseline slot (有界 baseline 并发槽位，防 IMAP stampede)
	// 设置基线阶段独立有限预算 (10秒)，覆盖槽位等待与网络读取，保留父请求取消 (S05)
	baselineCtx, cancelBaseline := withShortPhaseTimeout(ctx, 10*time.Second)
	defer cancelBaseline()

	select {
	case s.baselineSlots <- struct{}{}:
		defer func() { <-s.baselineSlots }()
	case <-baselineCtx.Done():
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &BackendError{Status: http.StatusServiceUnavailable, Code: "BASELINE_UNAVAILABLE", Message: "等待基线槽位超时"}
	}

	// 6. 再次 cheap DB admission preflight (获取锁和槽位后二次校验，避免 TOCTOU)
	if err := s.checkAdmission(baselineCtx, string(p.Kind), p.ID, alloc.AllocationID); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}

	// 7. ctx 检查
	if err := baselineCtx.Err(); err != nil {
		return nil, err
	}

	// 8. 采集基线游标 (GetMailboxBoundaryContext: 严格响应 HTTP disconnect / cancel 以及基线阶段超时)
	mailboxCtx, source, _, captureErr := s.be.CaptureMailboxContext(baselineCtx, alloc.AccountID)
	if captureErr != nil {
		return nil, captureErr
	}
	provider, uidValidity, uidNext, bErr := s.be.GetMailboxBoundaryContext(mailboxCtx, alloc.AccountID, "INBOX")
	if bErr != nil {
		if errors.Is(bErr, context.Canceled) || errors.Is(bErr, context.DeadlineExceeded) {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &BackendError{Status: http.StatusServiceUnavailable, Code: "BASELINE_UNAVAILABLE", Message: "采集邮件基线游标超时"}
		}
		var be *BackendError
		if errors.As(bErr, &be) {
			return nil, be
		}
		return nil, &BackendError{Status: http.StatusServiceUnavailable, Code: "BASELINE_UNAVAILABLE", Message: "无法获取邮件基线游标: " + bErr.Error()}
	}

	// 9. 构造 ready request
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
		BaselineSource:      source,
		BaselineAccountID:   alloc.AccountID,
		IdempotencyKey:      idempotencyKey,
		IdempotencyHash:     idempotencyHash,
	}

	// 10. final authoritative atomic insert (设置独立 5 秒提交预算，S05)
	commitCtx, cancelCommit := withShortPhaseTimeout(ctx, 5*time.Second)
	defer cancelCommit()
	mg, mp := s.limits()
	if err := s.store.CreateVerificationRequestAtomic(commitCtx, vreq, mg, mp); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		if errors.Is(err, store.ErrIdempotencyConflict) {
			existing, qerr := s.store.GetVerificationRequestByIdempotencyKey(commitCtx, string(p.Kind), p.ID, idempotencyKey)
			if qerr == nil && existing != nil {
				if existing.IdempotencyHash != idempotencyHash {
					return nil, ErrIdempotencyConflict
				}
				return existing, nil
			}
			return nil, ErrIdempotencyConflict
		}
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

// / GetVerificationResult 读取验证码（支持超时长轮询、独立短阶段期限与 Waiter 槽位管控）
func (s *VerificationService) GetVerificationResult(ctx context.Context, p auth.Principal, requestID string, timeoutSec int, credential string, onWaitStart ...func()) (result *VerificationResult, retErr error) {
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

	// 轮换保留主体 ID；复查原请求凭据，覆盖事件消费和全部结果交付路径。
	validateCredential := func(checkCtx ...context.Context) (bool, error) {
		var cc context.Context
		if len(checkCtx) > 0 && checkCtx[0] != nil {
			cc = checkCtx[0]
		} else {
			var cancel context.CancelFunc
			cc, cancel = context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
		}
		id, _, _, valid, err := s.store.ValidateTokenPrincipalContext(cc, credential)
		if err != nil {
			return false, err
		}
		return valid && id == p.ID, nil
	}
	if p.Kind == auth.PrincipalToken {
		shortCtx, shortCancel := context.WithTimeout(ctx, 5*time.Second)
		valid, cErr := validateCredential(shortCtx)
		shortCancel()
		if cErr != nil {
			if errors.Is(cErr, context.Canceled) || errors.Is(cErr, context.DeadlineExceeded) {
				return nil, cErr
			}
			return nil, &BackendError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "凭据校验失败: " + cErr.Error()}
		}
		if !valid {
			return nil, ErrTokenRevoked
		}
		defer func() {
			if retErr != nil || result == nil {
				return
			}
			deliverCtx, deliverCancel := context.WithTimeout(context.Background(), 5*time.Second)
			valid, dErr := validateCredential(deliverCtx)
			deliverCancel()
			if dErr != nil {
				result, retErr = nil, &BackendError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "结果交付凭据复查失败: " + dErr.Error()}
			} else if !valid {
				result, retErr = nil, ErrTokenRevoked
			}
		}()
	}

	readRequest := func(readCtx ...context.Context) (*store.VerificationRequest, error) {
		var rc context.Context
		if len(readCtx) > 0 && readCtx[0] != nil {
			rc = readCtx[0]
		} else {
			var cancel context.CancelFunc
			rc, cancel = context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
		}
		req, err := s.store.GetVerificationRequest(rc, requestID, string(p.Kind), p.ID)
		if err != nil {
			if errors.Is(err, store.ErrVerificationRequestNotFound) {
				return nil, ErrVReqNotFound
			}
			if rc.Err() != nil {
				return nil, rc.Err()
			}
			return nil, &BackendError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "读取取码任务状态失败: " + err.Error()}
		}
		if req == nil {
			return nil, ErrVReqNotFound
		}
		return req, nil
	}

	// 初始读取阶段使用独立 5 秒短超时预算，避免长轮询等待期限掩盖存储死锁
	initCtx, initCancel := context.WithTimeout(ctx, 5*time.Second)
	vreq, err := readRequest(initCtx)
	initCancel()
	if err != nil {
		return nil, err
	}
	checkSource := func() error {
		if vreq.BaselineSource == "" || (vreq.Status != "ready" && vreq.Status != "pending") {
			return nil
		}
		_, current, _, err := s.be.CaptureMailboxContext(ctx, vreq.BaselineAccountID)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// 基线母号已删除时来源不可能恢复，持久化失效而不是让每次轮询都返回 500。
			if mapAccountErr(err).Code != "ACCOUNT_NOT_FOUND" {
				return err
			}
			current = ""
		}
		if current != vreq.BaselineSource {
			phaseCtx, cancel := withShortPhaseTimeout(ctx, 5*time.Second)
			defer cancel()
			latest, _, err := s.store.InvalidateVerificationRequest(phaseCtx, vreq.RequestID)
			if err != nil {
				return err
			}
			if latest != nil {
				vreq = latest
			}
		}
		return nil
	}
	if err := checkSource(); err != nil {
		return nil, err
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

	// 2b. 申请长轮询 Waiter 名额（防重复连接与总体过载），并释放短阶段在途名额 (PR-CONCURRENCY T1)
	if waitDuration > 0 && s.limiter != nil {
		relWaiter, werr := s.limiter.AcquireWaiter(p, vreq.RequestID)
		if werr != nil {
			return nil, werr
		}
		defer relWaiter()
	}

	// 挂起前触发回调释放短阶段全局在途名额
	for _, cb := range onWaitStart {
		if cb != nil {
			cb()
		}
	}

	// 3. 基于 INBOX Mailbox、UIDVALIDITY 与 UIDNEXT 边界订阅事件 (PR-06 §9.2, 9.5, Issue 4 & 5)
	mailbox := vreq.BaselineMailbox
	if mailbox == "" {
		mailbox = "INBOX"
	}
	subID, ch := s.eventBus.SubscribeWithBoundary(vreq.AliasEmail, mailbox, uint32(vreq.BaselineUIDValidity), uint32(vreq.BaselineUID), vreq.BaselineSource)
	defer s.eventBus.Unsubscribe(vreq.AliasEmail, subID)

	if s.syncWorker != nil {
		s.syncWorker.Trigger()
	}

	// Blocker 3 修复: Post-subscribe DB recheck (PR-04B)
	// 订阅建立并触发 worker 后立即复查数据库。若任务已被后台 worker 或并发流程落库为终态
	// (succeeded / expired / invalidated)，直接返回权威结果，不依赖 EventBus 内存唤醒，消除 subscribe race。
	if freshReq, ferr := readRequest(); ferr != nil {
		return nil, ferr
	} else if freshReq.Status == "succeeded" || freshReq.Status == "expired" || freshReq.Status == "invalidated" {
		return mapVerificationRecordToResult(freshReq)
	}

	// 缓存或旧扫描发布的事件可能早于本任务基线，代际不一致时以当前邮箱边界为准。
	// 读取失败时不失效任务，后台 Worker 会在确认真实代际后持久化失效。
	generationChanged := func() (bool, error) {
		if vreq.BaselineAccountID == "" {
			return true, nil
		}
		phaseCtx, cancel := withShortPhaseTimeout(ctx, 10*time.Second)
		defer cancel()
		mailboxCtx, source, _, err := s.be.CaptureMailboxContext(phaseCtx, vreq.BaselineAccountID)
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, nil
		}
		if vreq.BaselineSource != "" && source != vreq.BaselineSource {
			return true, nil
		}
		_, uidValidity, _, err := s.be.GetMailboxBoundaryContext(mailboxCtx, vreq.BaselineAccountID, mailbox)
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, nil
		}
		return uidValidity != 0 && uidValidity != uint32(vreq.BaselineUIDValidity), nil
	}

	handleItem := func(item *mail.CachedOTP) (*VerificationResult, error) {
		if err := checkSource(); err != nil {
			return nil, err
		}
		if vreq.Status == "invalidated" || vreq.Status == "expired" || vreq.Status == "succeeded" {
			return mapVerificationRecordToResult(vreq)
		}
		// 消费验证码事件前复查原请求凭据。
		if p.Kind == auth.PrincipalToken {
			valid, err := validateCredential()
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return nil, err
				}
				return nil, &BackendError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "凭据复查失败: " + err.Error()}
			}
			if !valid {
				return nil, ErrTokenRevoked
			}
		}
		// UIDVALIDITY 突变检测 (Issue 5 & 6)
		if item.UIDValidity != 0 && vreq.BaselineUIDValidity != 0 && item.UIDValidity != uint32(vreq.BaselineUIDValidity) {
			changed, err := generationChanged()
			if err != nil {
				return nil, err
			}
			if !changed {
				return nil, errStaleVerificationEvent
			}
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
			Source:          item.Source,
			Code:            code,
			MagicLink:       magicLink,
			MatchedEventRef: item.EventID,
		}, nowUTC)
		if err != nil {
			return nil, &BackendError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "持久化验证码终态失败: " + err.Error()}
		}
		if won {
			// 仅在明确落库成功后才消费事件
			s.eventBus.ConsumeEvent(vreq.AliasEmail, item.EventID, item.Source)
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

	// consumeItem 处理一个事件；旧代际事件被忽略时，回查期间仅留在缓存的有效事件和数据库终态。
	// 订阅通道容量为 1，旧事件占用通道时后到的有效事件不会再次投递。done=false 表示继续等待。
	consumeItem := func(item *mail.CachedOTP) (res *VerificationResult, err error, done bool) {
		for {
			res, err = handleItem(item)
			if !errors.Is(err, errStaleVerificationEvent) {
				return res, err, true
			}
			if next := s.eventBus.CachedMatch(vreq.AliasEmail, mailbox, uint32(vreq.BaselineUIDValidity), uint32(vreq.BaselineUID), vreq.BaselineSource); next != nil {
				item = next
				continue
			}
			freshReq, ferr := readRequest()
			if ferr != nil {
				return nil, ferr, true
			}
			if freshReq.Status == "succeeded" || freshReq.Status == "expired" || freshReq.Status == "invalidated" {
				res, err = mapVerificationRecordToResult(freshReq)
				return res, err, true
			}
			return nil, nil, false
		}
	}

	if waitDuration <= 0 {
		select {
		case item := <-ch:
			if res, err, done := consumeItem(item); done {
				return res, err
			}
		default:
		}
		if freshReq, ferr := readRequest(); ferr != nil {
			return nil, ferr
		} else if freshReq.Status == "succeeded" || freshReq.Status == "expired" || freshReq.Status == "invalidated" {
			return mapVerificationRecordToResult(freshReq)
		}
		return &VerificationResult{
			RequestID:  vreq.RequestID,
			LeaseID:    vreq.LeaseID,
			AliasEmail: vreq.AliasEmail,
			Status:     "pending",
		}, nil
	}

	timer := time.NewTimer(waitDuration)
	defer timer.Stop()

	var srvDone <-chan struct{}
	if srvCtx := s.getServerContext(); srvCtx != nil {
		srvDone = srvCtx.Done()
	}

	for {
		select {
		case item := <-ch:
			if res, err, done := consumeItem(item); done {
				return res, err
			}
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
			if freshReq, ferr := readRequest(); ferr != nil {
				return nil, ferr
			} else if freshReq.Status == "succeeded" || freshReq.Status == "expired" || freshReq.Status == "invalidated" {
				return mapVerificationRecordToResult(freshReq)
			}
			return &VerificationResult{
				RequestID:  vreq.RequestID,
				LeaseID:    vreq.LeaseID,
				AliasEmail: vreq.AliasEmail,
				Status:     "pending",
			}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-srvDone:
			return nil, &BackendError{Status: http.StatusServiceUnavailable, Code: "SERVER_SHUTTING_DOWN", Message: "服务正在优雅停机"}
		}
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

func withShortPhaseTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		d = 5 * time.Second
	}
	return context.WithTimeout(ctx, d)
}
