/**
 * [INPUT]: 依赖 context, errors, fmt, strings, time, icloud-hme/internal/auth, icloud-hme/internal/hme, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 AliasAllocationService, AllocationRequest, AllocationResult, ErrPoolEmpty, ErrAllocationNotReady, ErrForbiddenAccountID, ErrForbiddenRemoteCreation, ErrTagNotAllowed
 * [POS]: internal/server 的领域出号应用服务 (Section I & II)，统一所有出号入口（/quick-create, /alias/lease, /allocate, /external/v1/allocate, /external/v2/allocate）至单一库存真相源，并阻断外部令牌远程建号
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"icloud-hme/internal/auth"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/store"
)

var (
	// ErrPoolEmpty 库存为空 / 池空
	ErrPoolEmpty = errors.New("pool empty")
	// ErrAllocationNotReady 存储层未就绪
	ErrAllocationNotReady = errors.New("allocation store not ready")
	// ErrForbiddenAccountID 外部令牌禁止指定出号母号
	ErrForbiddenAccountID = errors.New("specifying account_id is forbidden for external tokens")
	// ErrForbiddenRemoteCreation 外部令牌禁止现场按需创号
	ErrForbiddenRemoteCreation = errors.New("remote on-demand creation is forbidden for external tokens")
	// ErrTagNotAllowed 业务标签越权
	ErrTagNotAllowed = errors.New("specified tag is not allowed for this principal")
	// ErrIdempotencyKeyRequired 强制要求幂等键
	ErrIdempotencyKeyRequired = errors.New("idempotency key required")
	ErrInvalidAllocationMode  = errors.New("invalid allocation mode")
)

// AllocationRequest 统一出号请求
type AllocationRequest struct {
	Tag                string
	Label              string
	AccountID          string
	Mode               string // "pool", "pool_only", "create"
	IdempotencyKey     string
	RequestHash        string
	RequireIdempotency bool
}

// AllocationFingerprintV2 规范化幂等指纹结构 (F04: 杜绝字符串拼接碰撞，包含 mode 语义)
type AllocationFingerprintV2 struct {
	Version   int    `json:"v"`
	Tag       string `json:"tag"`
	AccountID string `json:"account_id"`
	Label     string `json:"label"`
	Mode      string `json:"mode"`
}

// ComputeAllocationRequestHash 计算规范的 v2 请求指纹
func ComputeAllocationRequestHash(tag, accountID, label, mode string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		tag = "default"
	}
	accountID = strings.TrimSpace(accountID)
	label = strings.TrimSpace(label)
	mode = strings.TrimSpace(mode)
	if mode == "" {
		mode = "pool"
	}
	fp := AllocationFingerprintV2{
		Version:   2,
		Tag:       tag,
		AccountID: accountID,
		Label:     label,
		Mode:      mode,
	}
	data, _ := json.Marshal(fp)
	h := sha256.Sum256(data)
	return fmt.Sprintf("v2:%x", h[:])
}

// ComputeLegacyAllocationRequestHash 计算历史版本请求指纹 (仅用于可证明等价的历史操作记录受控比对)
// 约束 (Fail-Closed):
// 1. mode 必须为默认 "pool"，若非 pool (如 pool_only, create) 无法证明与旧版等价，坚决返回空；
// 2. tag/accountID/label 严禁包含 '&' 或 '='，若包含拼接歧义字符则无法证明原请求参数，坚决返回空拒绝降级。
func ComputeLegacyAllocationRequestHash(tag, accountID, label, mode string) string {
	mode = strings.TrimSpace(mode)
	if mode != "" && mode != "pool" {
		return ""
	}
	tag = strings.TrimSpace(tag)
	if tag == "" {
		tag = "default"
	}
	accountID = strings.TrimSpace(accountID)
	label = strings.TrimSpace(label)
	if strings.ContainsAny(tag, "&=") || strings.ContainsAny(accountID, "&=") || strings.ContainsAny(label, "&=") {
		return ""
	}
	return fmt.Sprintf("tag=%s&account_id=%s&label=%s", tag, accountID, label)
}

// AllocationResult 统一出号结果
type AllocationResult struct {
	Allocation *store.AliasAllocation
	Operation  *store.Operation
	Source     string // "pool" or "created"
}

// AliasAllocationService 统管所有出号路径的领域/应用服务
type AliasAllocationService struct {
	store                      *store.Store
	be                         Backend
	syncWorker                 *MailSyncWorker
	rrIndex                    uint64
	beforeRecordAllocationHook func(email string)
}

// NewAliasAllocationService 创建统一出号服务实例
func NewAliasAllocationService(st *store.Store, be Backend, sw *MailSyncWorker) *AliasAllocationService {
	return &AliasAllocationService{
		store:      st,
		be:         be,
		syncWorker: sw,
	}
}

// Allocate 执行统一出号逻辑 (PR-08 Final Hardening §2: 所有五个出号入口唯一领域应用入口)
func (s *AliasAllocationService) Allocate(ctx context.Context, p auth.Principal, req AllocationRequest) (*AllocationResult, error) {
	if !p.CanAllocate() {
		return nil, ErrScopeDenied
	}
	mode := strings.TrimSpace(req.Mode)
	if mode == "" {
		mode = "pool"
	}
	switch mode {
	case "pool", "pool_only", "create":
	default:
		return nil, ErrInvalidAllocationMode
	}
	req.Mode = mode

	// 1. 外部令牌安全防护：绝不允许指定 account_id 或远程创号 (403 优先级高于参数校验)
	if p.Kind == auth.PrincipalToken && !p.IsAdmin() {
		if req.AccountID != "" {
			return nil, ErrForbiddenAccountID
		}
		if req.Mode == "create" {
			return nil, ErrForbiddenRemoteCreation
		}
	}

	tag := strings.TrimSpace(req.Tag)
	if tag == "" {
		tag = "default"
	}
	req.Tag = tag

	// 2. 业务标签权限范围核验 (403)
	if len(p.AllowedTags) > 0 {
		allowed := false
		for _, t := range p.AllowedTags {
			if strings.EqualFold(t, tag) {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, ErrTagNotAllowed
		}
	}

	// 3. 参数校验与幂等键必须性
	if req.RequireIdempotency && p.Kind == auth.PrincipalToken && strings.TrimSpace(req.IdempotencyKey) == "" {
		return nil, ErrIdempotencyKeyRequired
	}

	if s.store == nil {
		return nil, ErrAllocationNotReady
	}

	// 若指定了 account_id，前置核验该账号是否存在
	if req.AccountID != "" && s.be != nil {
		if _, err := s.be.GetAccount(req.AccountID); err != nil {
			return nil, err
		}
	}

	principalKind := string(p.Kind)
	principalID := p.ID
	tokenDisplayName := p.TokenName

	switch p.Kind {
	case auth.PrincipalToken:
		principalKind = "token"
		principalID = p.ID
		if tokenDisplayName == "" {
			tokenDisplayName = p.ID
		}
	case auth.PrincipalAdmin:
		principalKind = "admin"
		if principalID == "" {
			principalID = "admin"
		}
		if tokenDisplayName == "" || tokenDisplayName == "admin_session" {
			tokenDisplayName = "admin_console"
		}
	case auth.PrincipalSystem:
		principalKind = "system"
		if principalID == "" {
			principalID = "system"
		}
		if tokenDisplayName == "" {
			tokenDisplayName = "scheduler"
		}
	default:
		principalKind = "admin"
		principalID = "admin"
		tokenDisplayName = "admin_console"
	}

	if req.RequestHash == "" {
		v2Hash := ComputeAllocationRequestHash(req.Tag, req.AccountID, req.Label, mode)
		legacyHash := ComputeLegacyAllocationRequestHash(req.Tag, req.AccountID, req.Label, mode)
		if legacyHash != "" {
			req.RequestHash = v2Hash + "|legacy:" + legacyHash
		} else {
			req.RequestHash = v2Hash
		}
	}

	var poolAccountIDs []string
	if req.AccountID != "" {
		poolAccountIDs = []string{req.AccountID}
	} else if s.be != nil {
		poolAccountIDs = selectPoolAccounts(s.be.ListAccounts(), req.Tag)
		if poolAccountIDs == nil {
			poolAccountIDs = []string{}
		}
		if len(poolAccountIDs) > 1 {
			idx := int(atomic.AddUint64(&s.rrIndex, 1)-1) % len(poolAccountIDs)
			rotated := make([]string, len(poolAccountIDs))
			for i := 0; i < len(poolAccountIDs); i++ {
				rotated[i] = poolAccountIDs[(idx+i)%len(poolAccountIDs)]
			}
			poolAccountIDs = rotated
		}
	} else {
		// 未指定母号且无 Backend 可路由，必须固定为空切片，严禁传 nil 退化成无界全局库存
		poolAccountIDs = []string{}
	}

	operationKind := "allocate"
	if req.IdempotencyKey != "" {
		operationKind = "v2_allocate"
	}
	if mode != "create" {
		alloc, op, err := s.store.ClaimInventoryAlias(ctx, principalKind, principalID, operationKind,
			req.IdempotencyKey, req.RequestHash, req.Tag, poolAccountIDs)
		if err == nil && alloc != nil {
			if s.syncWorker != nil {
				s.syncWorker.RegisterAliasAccount(alloc.AliasEmail, alloc.AccountID)
			}
			source := "pool"
			if op != nil && op.ResultSource != "" {
				source = op.ResultSource
			}
			return &AllocationResult{Allocation: alloc, Operation: op, Source: source}, nil
		}
		if !errors.Is(err, store.ErrNoAvailableInventory) {
			return &AllocationResult{Operation: op}, err
		}
		if !p.IsAdmin() || mode == "pool_only" {
			return nil, ErrPoolEmpty
		}
	}

	// A keyed remote creation keeps the same operation kind used by historical pool claims.
	if p.IsAdmin() && s.be != nil {
		replayed, op, beginErr := s.store.BeginRemoteAllocation(ctx, principalKind, principalID,
			operationKind, req.IdempotencyKey, req.RequestHash, req.Tag, tokenDisplayName)
		if beginErr != nil {
			return &AllocationResult{Operation: op}, beginErr
		}
		if replayed != nil {
			if s.syncWorker != nil {
				s.syncWorker.RegisterAliasAccount(replayed.AliasEmail, replayed.AccountID)
			}
			return &AllocationResult{Allocation: replayed, Operation: op, Source: op.ResultSource}, nil
		}
		var res *hme.CreateResult
		var accountID string
		var err error

		if req.AccountID != "" {
			res, err = s.be.CreateAliasForAllocationContext(ctx, req.AccountID, req.Label, op.OperationID)
			accountID = req.AccountID
		} else {
			cands, selectErr := selectAccountCandidates(s.be.ListAccounts(), req.Tag, s.store)
			if selectErr != nil {
				persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				if stateErr := s.store.MarkRemoteAllocationError(persistCtx, op.OperationID); stateErr != nil {
					return nil, errors.Join(fmt.Errorf("查询候选账号配额: %w", selectErr), fmt.Errorf("保存出号失败状态: %w", stateErr))
				}
				return &AllocationResult{Operation: op}, fmt.Errorf("查询候选账号配额: %w", selectErr)
			}
			for _, candID := range cands {
				res, err = s.be.CreateAliasForAllocationContext(ctx, candID, req.Label, op.OperationID)
				if err == nil && res != nil {
					accountID = candID
					break
				}
				// F03 闭环: 若上游写操作结果未知，严禁换账号重试同一业务操作，必须立即阻断并上抛！
				if err != nil {
					var be *BackendError
					if (errors.As(err, &be) && be.Code == "UPSTREAM_OUTCOME_UNKNOWN") || errors.Is(err, hme.ErrOutcomeUnknown) {
						break
					}
				}
			}
		}

		if err != nil {
			persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if stateErr := s.store.MarkRemoteAllocationError(persistCtx, op.OperationID); stateErr != nil {
				return nil, errors.Join(err, fmt.Errorf("保存出号失败状态: %w", stateErr))
			}
			return nil, err
		}
		if res == nil {
			persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if stateErr := s.store.MarkRemoteAllocationError(persistCtx, op.OperationID); stateErr != nil {
				return nil, stateErr
			}
			return nil, ErrPoolEmpty
		}

		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		// 现场创建别名先以不可分配暂存状态 (created + unknown) 记入 alias_inventory，
		// 严禁提前暴露为公共 available 库存，防止被并发的普通 ClaimInventoryAlias 抢先认领
		if invErr := s.store.AddInventoryAlias(accountID, hme.Alias{
			Email:       res.Email,
			AnonymousID: res.AnonymousID,
			Label:       res.Label,
			CreatedAt:   res.CreatedAt,
			Active:      true,
		}, "created", false); invErr != nil {
			_ = s.store.MarkOperationOutcomeUnknown(persistCtx, op.OperationID, res.Email, "LOCAL_PERSISTENCE_FAILED")
			return nil, fmt.Errorf("upstream created alias %s successfully but local inventory persistence failed (pending reconciliation): %w", res.Email, invErr)
		}
		if s.beforeRecordAllocationHook != nil {
			s.beforeRecordAllocationHook(res.Email)
		}
		alloc, recErr := s.store.ReconcileUnknownOperation(persistCtx, op.OperationID, store.ReconciliationFound,
			res.Email, accountID, req.Tag, principalKind, principalID, tokenDisplayName)
		if recErr != nil {
			// 本地入账失败时，主动隔离暂存别名，确保不遗留可被其他消费者领取的中间状态
			_ = s.store.QuarantineInventoryAlias(res.Email)
			_ = s.store.MarkOperationOutcomeUnknown(persistCtx, op.OperationID, res.Email, "LOCAL_PERSISTENCE_FAILED")
			return nil, fmt.Errorf("持久化分配凭据失败: %w", recErr)
		}
		op.State, op.ResultRef, op.CandidateEmail, op.ResultSource = "succeeded", alloc.AllocationID, res.Email, "created"
		if s.syncWorker != nil {
			s.syncWorker.RegisterAliasAccount(res.Email, accountID)
		}

		return &AllocationResult{
			Allocation: alloc,
			Operation:  op,
			Source:     "created",
		}, nil
	}

	return nil, ErrPoolEmpty
}
