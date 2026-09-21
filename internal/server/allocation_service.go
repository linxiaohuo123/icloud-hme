/**
 * [INPUT]: 依赖 context, errors, fmt, strings, time, icloud-hme/internal/auth, icloud-hme/internal/hme, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 AliasAllocationService, AllocationRequest, AllocationResult, ErrPoolEmpty, ErrAllocationNotReady, ErrForbiddenAccountID, ErrForbiddenRemoteCreation, ErrTagNotAllowed
 * [POS]: internal/server 的领域出号应用服务 (Section I & II)，统一所有出号入口（/quick-create, /alias/lease, /allocate, /external/v1/allocate, /external/v2/allocate）至单一库存真相源，并阻断外部令牌远程建号
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"icloud-hme/internal/auth"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/store"
)

var (
	// ErrPoolEmpty 库存为空 / 池空
	ErrPoolEmpty = errors.New("pool empty")
	// ErrAllocationNotReady 存储层未就绪
	ErrAllocationNotReady = errors.New("allocation not ready")
	// ErrForbiddenAccountID 外部令牌禁止指定母号
	ErrForbiddenAccountID = errors.New("specifying account_id is forbidden for external tokens")
	// ErrForbiddenRemoteCreation 外部令牌禁止现场按需创号
	ErrForbiddenRemoteCreation = errors.New("remote on-demand creation is forbidden for external tokens")
	// ErrTagNotAllowed 业务标签越权
	ErrTagNotAllowed = errors.New("specified tag is not allowed for this principal")
)

// AllocationRequest 统一出号请求
type AllocationRequest struct {
	Tag            string
	Label          string
	AccountID      string
	Mode           string // "pool", "pool_only", "create"
	IdempotencyKey string
	RequestHash    string
}

// AllocationResult 统一出号结果
type AllocationResult struct {
	Allocation *store.AliasAllocation
	Operation  *store.Operation
	Source     string // "pool" or "created"
}

// AliasAllocationService 统管所有出号路径的领域/应用服务
type AliasAllocationService struct {
	store      *store.Store
	be         Backend
	syncWorker *MailSyncWorker
}

// NewAliasAllocationService 创建统一出号服务实例
func NewAliasAllocationService(st *store.Store, be Backend, sw *MailSyncWorker) *AliasAllocationService {
	return &AliasAllocationService{
		store:      st,
		be:         be,
		syncWorker: sw,
	}
}

// Allocate 执行统一出号逻辑
func (s *AliasAllocationService) Allocate(ctx context.Context, p auth.Principal, req AllocationRequest) (*AllocationResult, error) {
	if s.store == nil {
		return nil, ErrAllocationNotReady
	}

	tag := strings.TrimSpace(req.Tag)
	if tag == "" {
		tag = "default"
	}
	req.Tag = tag

	// 1. 业务标签权限范围核验
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

	// 2. 外部令牌安全防护：绝不允许指定 account_id 或远程创号
	if p.Kind == auth.PrincipalToken && !p.IsAdmin() {
		if req.AccountID != "" {
			return nil, ErrForbiddenAccountID
		}
		if req.Mode == "create" {
			return nil, ErrForbiddenRemoteCreation
		}
	}

	// 若指定了 account_id，前置核验该账号是否存在
	if req.AccountID != "" && s.be != nil {
		if _, err := s.be.GetAccount(req.AccountID); err != nil {
			return nil, err
		}
	}

	mode := req.Mode
	if mode == "" {
		mode = "pool"
	}

	tokenName := p.TokenName
	if tokenName == "" {
		tokenName = p.ID
	}

	// 3. 收集并交错候选别名，保证跨母号负载均衡与预存池更新
	var poolAccountIDs []string
	if req.AccountID != "" {
		poolAccountIDs = []string{req.AccountID}
	} else if s.be != nil {
		poolAccountIDs = selectPoolAccounts(s.be.ListAccounts(), req.Tag)
	}

	var accountAliases [][]store.PoolCandidate
	maxCount := 0
	if s.be != nil {
		for _, accID := range poolAccountIDs {
			aliases, _ := s.be.ListAliases(accID)
			var list []store.PoolCandidate
			for _, a := range aliases {
				if a.Active {
					list = append(list, store.PoolCandidate{
						AccountID: accID,
						Email:     a.Email,
					})
				}
			}
			if len(list) > 0 {
				accountAliases = append(accountAliases, list)
				if len(list) > maxCount {
					maxCount = len(list)
				}
			}
		}
	}

	var candidates []store.PoolCandidate
	for i := 0; i < maxCount; i++ {
		for _, list := range accountAliases {
			if i < len(list) {
				candidates = append(candidates, list[i])
			}
		}
	}

	// 4. 首选从交织候选池认领 (驱动底层 alias_inventory 与 alias_allocations 单一真相源)
	if len(candidates) > 0 {
		rec, err := s.store.ClaimPoolAlias(candidates, req.Tag, tokenName)
		if err != nil {
			return nil, err
		}
		if rec != nil {
			if s.syncWorker != nil {
				s.syncWorker.RegisterAliasAccount(rec.Email, rec.AccountID)
			}
			alloc := &store.AliasAllocation{
				AllocationID: rec.ID,
				AliasEmail:   rec.Email,
				AccountID:    rec.AccountID,
				OwnerKind:    string(p.Kind),
				OwnerID:      p.ID,
				BusinessTag:  rec.Tag,
				AllocatedAt:  rec.AllocatedAt,
				Status:       rec.Status,
			}
			return &AllocationResult{
				Allocation: alloc,
				Source:     "pool",
			}, nil
		}
	} else {
		// 无外挂账号别名列表时，直接从底层 alias_inventory 认领
		alloc, op, err := s.store.ClaimInventoryAlias(
			ctx,
			string(p.Kind),
			p.ID,
			"allocate",
			req.IdempotencyKey,
			req.RequestHash,
			req.Tag,
			req.AccountID,
		)
		if err == nil && alloc != nil {
			if s.syncWorker != nil {
				s.syncWorker.RegisterAliasAccount(alloc.AliasEmail, alloc.AccountID)
			}
			return &AllocationResult{
				Allocation: alloc,
				Operation:  op,
				Source:     "pool",
			}, nil
		}
	}

	// 5. 若库存池为空 (POOL_EMPTY)
	// 【PR-05-1 Section II 核心铁律】外部令牌在库存为空时严禁 fallback 远程创号，必须立即返回 503 POOL_EMPTY
	if p.Kind == auth.PrincipalToken && !p.IsAdmin() {
		return nil, ErrPoolEmpty
	}
	if mode == "pool_only" {
		return nil, ErrPoolEmpty
	}

	// 6. 仅管理员且允许新建 (mode="pool" 或 mode="create") 时，降级现场新建
	if p.IsAdmin() && s.be != nil {
		var res *hme.CreateResult
		var accountID string
		var err error

		if req.AccountID != "" {
			res, err = s.be.CreateAlias(req.AccountID, req.Label)
			accountID = req.AccountID
		} else {
			cands := selectAccountCandidates(s.be.ListAccounts(), req.Tag, s.store)
			for _, candID := range cands {
				res, err = s.be.CreateAlias(candID, req.Label)
				if err == nil && res != nil {
					accountID = candID
					break
				}
			}
		}

		if err != nil {
			return nil, err
		}
		if res == nil {
			return nil, ErrPoolEmpty
		}

		now := time.Now().Format(time.RFC3339)
		allocID := fmt.Sprintf("alloc_%d", time.Now().UnixNano())

		// 同步记入 alias_inventory 与 alias_allocations
		_ = s.store.AddInventoryAlias(accountID, hme.Alias{Email: res.Email, Active: true}, "created", false)
		alloc := &store.AliasAllocation{
			AllocationID: allocID,
			AliasEmail:   res.Email,
			AccountID:    accountID,
			OwnerKind:    string(p.Kind),
			OwnerID:      p.ID,
			BusinessTag:  req.Tag,
			AllocatedAt:  now,
			Status:       "allocated",
		}
		_ = s.store.RecordAllocation(alloc, tokenName)
		_ = s.store.UpsertAliasRoutes(accountID, []string{res.Email})
		if s.syncWorker != nil {
			s.syncWorker.RegisterAliasAccount(res.Email, accountID)
		}

		return &AllocationResult{
			Allocation: alloc,
			Source:     "created",
		}, nil
	}

	return nil, ErrPoolEmpty
}
