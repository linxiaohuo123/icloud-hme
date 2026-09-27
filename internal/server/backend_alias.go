/**
 * [INPUT]: 依赖 internal/account, internal/hme, internal/store
 * [OUTPUT]: 对外提供 managerBackend 的别名相关方法 (CreateAlias, BatchCreateAlias, ListAliases, RefreshAliases, SetAliasActive, UpdateAlias, BatchUpdateAliases, DeleteAlias) 与 BatchCreateResult, BatchUpdateResult 类型
 * [POS]: internal/server 的别名业务门面实现，按账号锁内仲裁数量上限并保留上游成功结果
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/store"
)

// BatchCreateResult 批量创建别名结果。
type BatchCreateResult struct {
	AccountID         string             `json:"account_id"`
	Requested         int                `json:"requested"`
	Created           []hme.CreateResult `json:"created"`
	CreatedCount      int                `json:"created_count"`
	SkippedCount      int                `json:"skipped_count"`
	RemainingThisHour int                `json:"remaining_this_hour"`
	Message           string             `json:"message,omitempty"`
	LastError         string             `json:"last_error,omitempty"`
	AuditFailed       []string           `json:"audit_failed,omitempty"`
}

// BatchUpdateResult 批量修改别名备注结果。
type BatchUpdateResult struct {
	Total     int      `json:"total"`
	Succeeded []string `json:"succeeded"`
	Failed    []string `json:"failed"`
	LastError string   `json:"last_error,omitempty"`
}

// aliasCacheTTL 是内存别名缓存生命周期(15分钟)。
// 别名是准静态数据，新增/删除/修改已具备精准缓存驱逐钩子，长效缓存消除日常跨洋阻塞。
const aliasCacheTTL = 15 * time.Minute

type aliasCacheItem struct {
	aliases         []hme.Alias
	credentialEpoch uint64
	// total/active 在写入缓存时算一次并固化。
	// 若改为每次 ListAccounts 现场求和，2000 账号 × 200 别名就是每次调用 40 万次迭代。
	total     int
	active    int
	fetchedAt time.Time
}

// CreateAlias 创建 HME 别名 (兼容保留包装)。
func (b *managerBackend) CreateAlias(accountID, label string) (*hme.CreateResult, error) {
	return b.CreateAliasContext(context.Background(), accountID, label)
}

// CreateAliasContext 支持 Context 贯穿的 HME 别名创建 (PR-05 F10)。
func (b *managerBackend) CreateAliasContext(ctx context.Context, accountID, label string) (*hme.CreateResult, error) {
	// 1. 检查别名上限熔断 (总数或活跃数达到上限)
	if acc, ok := b.mgr.GetAccount(accountID); ok && (acc.AliasTotal >= account.MaxAliasesPerAccount || acc.AliasActive >= account.MaxAliasesPerAccount) {
		return nil, &BackendError{
			Status:  http.StatusBadRequest,
			Code:    "ALIAS_LIMIT_REACHED",
			Message: fmt.Sprintf("账号 %s 别名数量已达 Apple 物理上限 (总数: %d, 活跃: %d/%d)，本地熔断阻断", accountID, acc.AliasTotal, acc.AliasActive, account.MaxAliasesPerAccount),
		}
	}

	// 2. 扣减本地小时配额
	if b.store != nil {
		allowed, rem := b.store.TryReserveQuota(accountID, 1)
		if !allowed {
			return nil, &BackendError{
				Status:  http.StatusTooManyRequests,
				Code:    "RATE_LIMITED",
				Message: fmt.Sprintf("账号 %s 当前小时创建配额已用完 (剩余 %d 个)", accountID, rem),
			}
		}
	}

	var result *hme.CreateResult
	err := b.mgr.WithHMEClientContext(ctx, accountID, func(client *hme.Client) error {
		res, cerr := b.durableCreateAlias(ctx, client, accountID, label, 5)
		if cerr != nil {
			return cerr
		}
		result = res
		return nil
	})
	if result != nil {
		if err != nil {
			log.Printf("[HME] 账号 %s 别名已创建，但会话回写失败: %v", accountID, err)
		}
		b.invalidateAliasCache(accountID)
		return result, nil
	}
	if err != nil {
		if !errors.Is(err, hme.ErrOutcomeUnknown) && b.store != nil {
			b.store.ReleaseQuota(accountID, 1)
		}
		if errors.Is(err, account.ErrHMEClientUnavailable) {
			return nil, mapAccountErr(err)
		}
		var backendErr *BackendError
		if errors.As(err, &backendErr) {
			return nil, backendErr
		}
		return nil, classifyUpstreamErr("创建邮箱失败", err)
	}
	return nil, fmt.Errorf("创建邮箱未返回结果")
}

func (b *managerBackend) getAccountMutationLock(accountID string) *sync.Mutex {
	v, _ := b.accountMutations.LoadOrStore(accountID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// reconcileIntentsWithClient 使用给定的 client 对一组未决意图执行核对
func (b *managerBackend) reconcileIntentsWithClient(ctx context.Context, client *hme.Client, intents []store.HmeReserveIntent) error {
	if len(intents) == 0 || b.store == nil {
		return nil
	}
	aliases, listErr := client.ListAliasesWithContext(ctx)
	if listErr != nil {
		for i := range intents {
			if err := b.store.UpdateReserveIntentState(ctx, intents[i].IntentID, store.IntentStateOutcomeUnknown, "", "", fmt.Sprintf("reconciliation list failed: %v", listErr)); err != nil {
				return fmt.Errorf("保存别名核对失败状态 %s: %w", intents[i].IntentID, err)
			}
			intents[i].State = store.IntentStateOutcomeUnknown
		}
		return listErr
	}

	for i := range intents {
		it := intents[i]
		var found *hme.Alias
		for j := range aliases {
			if strings.EqualFold(aliases[j].Email, it.CandidateEmail) {
				found = &aliases[j]
				break
			}
		}

		if found != nil {
			// 原请求的用途未知，恢复的别名必须隔离，避免被公共或定向分配。
			if err := b.store.AddRecoveredInventoryAlias(it.AccountID, *found); err != nil {
				return fmt.Errorf("保存恢复别名 %s: %w", found.Email, err)
			}
			if err := b.store.UpsertAliasRoutes(it.AccountID, []string{found.Email}); err != nil {
				return fmt.Errorf("保存恢复别名路由 %s: %w", found.Email, err)
			}
			if err := b.store.UpdateReserveIntentState(ctx, it.IntentID, store.IntentStateSucceeded, found.AnonymousID, "", ""); err != nil {
				return fmt.Errorf("保存别名核对结果 %s: %w", it.IntentID, err)
			}
			intents[i].State = store.IntentStateSucceeded
			intents[i].AnonymousID = found.AnonymousID
		} else {
			// INCONCLUSIVE_NOT_FOUND: 坚决保持 outcome_unknown，严禁标记失败，严禁产生第二候选！
			if err := b.store.UpdateReserveIntentState(ctx, it.IntentID, store.IntentStateOutcomeUnknown, "", "", "reconciliation inconclusive: candidate not found in upstream list"); err != nil {
				return fmt.Errorf("保存别名核对未决状态 %s: %w", it.IntentID, err)
			}
			intents[i].State = store.IntentStateOutcomeUnknown
		}
	}
	return nil
}

// durableCreateAlias 执行符合 F03 铁律的持久化创建状态机：
//  0. Account-level unresolved gate: 在调用任何 Generate 之前，检查是否存在 prepared, reserve_sent, outcome_unknown 的意图；
//     若存在先核对，核对后若仍未解决，坚决阻断新创建并返回 hme.ErrOutcomeUnknown，严禁 Generate，严禁 Reserve，严禁生成候选 B！
//  1. Generate candidate A
//  2. 持久化 intent(A, prepared) 并 Commit SQLite
//  3. 标记状态为 reserve_sent 并 Commit SQLite
//  4. 才向网络发送 Reserve(A)
//  5. 成功 -> succeeded; 明确失败 -> confirmed_failed 并允许重试下一候选; 未知异常 -> outcome_unknown 并坚决阻断重试
func (b *managerBackend) durableCreateAlias(ctx context.Context, client *hme.Client, accountID, label string, maxRetries int) (*hme.CreateResult, error) {
	// 针对单账号串行化写操作与未决门禁检查，避免并发 check empty -> Generate 穿透窗口
	lock := b.getAccountMutationLock(accountID)
	lock.Lock()
	defer lock.Unlock()
	if acc, ok := b.mgr.GetAccount(accountID); ok && (acc.AliasTotal >= account.MaxAliasesPerAccount || acc.AliasActive >= account.MaxAliasesPerAccount) {
		return nil, &BackendError{
			Status:  http.StatusBadRequest,
			Code:    "ALIAS_LIMIT_REACHED",
			Message: fmt.Sprintf("账号 %s 别名数量已达本地上限 (%d)", accountID, account.MaxAliasesPerAccount),
		}
	}

	// 0. Account-level unresolved gate
	if b.store != nil {
		unresolved, err := b.store.ListUnresolvedReserveIntents(ctx, accountID)
		if err != nil {
			return nil, fmt.Errorf("查询未决 reserve intent 失败: %w", err)
		}
		if len(unresolved) > 0 {
			// 存在未决 intent，先执行该账号的 reconciliation
			if rErr := b.reconcileIntentsWithClient(ctx, client, unresolved); rErr != nil {
				log.Printf("[HME] 账号 %s 核对未决意图遭遇错误: %v", accountID, rErr)
			}
			// reconciliation 后再次查询
			unresolvedAfter, err := b.store.ListUnresolvedReserveIntents(ctx, accountID)
			if err != nil {
				return nil, fmt.Errorf("核对后再次查询未决 reserve intent 失败: %w", err)
			}
			if len(unresolvedAfter) > 0 {
				// 仍存在任何 unresolved intent：立即阻断，严禁 Generate，严禁 Reserve！
				return nil, hme.ErrOutcomeUnknown
			}
		}
	}

	if maxRetries <= 0 {
		maxRetries = 5
	}
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if attempt > 0 {
			client.ResetServiceEndpoint()
		}

		// 1. Generate candidate A
		cand, gErr := client.GenerateWithContext(ctx)
		if gErr != nil {
			lastErr = fmt.Errorf("generate 失败: %w", gErr)
			if errors.Is(gErr, hme.ErrAuthFailed) {
				return nil, gErr
			}
			if attempt < maxRetries-1 {
				continue
			}
			break
		}

		// 2. 持久化 intent(A, prepared) 并 Commit SQLite (必须在 Reserve 发送之前完成)
		var intentID string
		if b.store != nil {
			intent, iErr := b.store.CreateReserveIntent(ctx, accountID, cand, label)
			if iErr != nil {
				return nil, fmt.Errorf("持久化 reserve intent 失败: %w", iErr)
			}
			intentID = intent.IntentID
		}

		// 3. 标记状态为 reserve_sent (即将向网络发出写请求)
		if b.store != nil && intentID != "" {
			_ = b.store.UpdateReserveIntentState(ctx, intentID, store.IntentStateReserveSent, "", "", "")
		}

		// 4. 发送写请求 Reserve(A)
		email, anonID, rErr := client.ReserveDetailedWithContext(ctx, cand, label)
		if rErr != nil {
			lastErr = rErr
			if errors.Is(rErr, hme.ErrAuthFailed) {
				if b.store != nil && intentID != "" {
					_ = b.store.UpdateReserveIntentState(ctx, intentID, store.IntentStateConfirmedFailed, "", "", rErr.Error())
				}
				return nil, rErr
			}

			// 5. 结果未知: 标记 outcome_unknown，铁律阻断重试生成候选 B！
			if errors.Is(rErr, hme.ErrOutcomeUnknown) {
				if b.store != nil && intentID != "" {
					_ = b.store.UpdateReserveIntentState(ctx, intentID, store.IntentStateOutcomeUnknown, "", "", rErr.Error())
				}
				return nil, rErr
			}

			// 明确失败 (confirmed_failed, 如 Apple 显式业务拒绝)
			if b.store != nil && intentID != "" {
				_ = b.store.UpdateReserveIntentState(ctx, intentID, store.IntentStateConfirmedFailed, "", "", rErr.Error())
			}

			// 只有确知明确拒绝才允许重试下一候选
			if attempt < maxRetries-1 {
				continue
			}
			break
		}

		// 6. 成功
		if b.store != nil && intentID != "" {
			if err := b.store.UpdateReserveIntentState(ctx, intentID, store.IntentStateSucceeded, anonID, "", ""); err != nil {
				log.Printf("[HME] 账号 %s 别名已创建，但意图状态回写失败: %v", accountID, err)
			}
		}
		if err := b.mgr.AdjustAliasCounts(accountID, 1, 1); err != nil {
			log.Printf("[HME] 账号 %s 别名已创建，但数量回写失败: %v", accountID, err)
		}

		return &hme.CreateResult{
			Email:       email,
			AnonymousID: anonID,
			Label:       label,
			CreatedAt:   time.Now().Format(time.RFC3339),
		}, nil
	}

	if lastErr != nil {
		return nil, fmt.Errorf("创建别名失败: %w", lastErr)
	}
	return nil, fmt.Errorf("创建别名失败,已重试 %d 次", maxRetries)
}

// ReconcileUnresolvedIntents 扫描所有未决 intent 并向 Apple 核对原候选 A (F03)
func (b *managerBackend) ReconcileUnresolvedIntents(ctx context.Context) ([]store.HmeReserveIntent, error) {
	if b.store == nil {
		return nil, nil
	}
	intents, err := b.store.ListUnresolvedReserveIntents(ctx, "")
	if err != nil {
		return nil, err
	}
	if len(intents) == 0 {
		return nil, nil
	}

	var reconcileErrors []error
	for i := range intents {
		if err := b.mgr.WithHMEClientContext(ctx, intents[i].AccountID, func(client *hme.Client) error {
			return b.reconcileIntentsWithClient(ctx, client, intents[i:i+1])
		}); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("核对未决别名 %s: %w", intents[i].IntentID, err))
		}
	}
	return intents, errors.Join(reconcileErrors...)
}

// BatchCreateAlias 批量创建 HME 别名 (1-5个)。
// BatchCreateAlias 批量创建 HME 别名 (兼容保留包装)。
func (b *managerBackend) BatchCreateAlias(accountID string, count int, labelPrefix string) (*BatchCreateResult, error) {
	return b.BatchCreateAliasContext(context.Background(), accountID, count, labelPrefix)
}

// BatchCreateAliasContext 支持 Context 贯穿的批量创建 (PR-05 F10)。
func (b *managerBackend) BatchCreateAliasContext(ctx context.Context, accountID string, count int, labelPrefix string) (*BatchCreateResult, error) {
	accountID = strings.TrimSpace(accountID)
	labelPrefix = strings.TrimSpace(labelPrefix)
	if count < 1 || count > 5 {
		return nil, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: "count 必须在 1-5 之间"}
	}

	// 1. 检查别名上限熔断 (总数或活跃数达到上限)
	if acc, ok := b.mgr.GetAccount(accountID); ok && (acc.AliasTotal+count > account.MaxAliasesPerAccount || acc.AliasActive+count > account.MaxAliasesPerAccount) {
		return nil, &BackendError{
			Status:  http.StatusBadRequest,
			Code:    "ALIAS_LIMIT_REACHED",
			Message: fmt.Sprintf("账号 %s 别名数量已达或将超过 Apple 物理上限 (当前总数: %d, 活跃: %d/%d)，本地熔断阻断", accountID, acc.AliasTotal, acc.AliasActive, account.MaxAliasesPerAccount),
		}
	}

	// 2. 扣减本地小时配额
	if b.store != nil {
		allowed, rem := b.store.TryReserveQuota(accountID, count)
		if !allowed {
			return nil, &BackendError{
				Status:  http.StatusTooManyRequests,
				Code:    "RATE_LIMITED",
				Message: fmt.Sprintf("账号 %s 当前小时创建配额不足以满足 %d 个别名 (剩余 %d 个)", accountID, count, rem),
			}
		}
	}

	resp := &BatchCreateResult{
		AccountID:    accountID,
		Requested:    count,
		Created:      make([]hme.CreateResult, 0, count),
		SkippedCount: 0,
	}

	batchCompleted := false
	batchErr := b.mgr.WithHMEClientContext(ctx, accountID, func(client *hme.Client) error {
		for i := 0; i < count; i++ {
			lbl := labelPrefix
			if lbl != "" && count > 1 {
				lbl = fmt.Sprintf("%s %d", labelPrefix, i+1)
			}
			res, createErr := b.durableCreateAlias(ctx, client, accountID, lbl, 3)
			if createErr != nil {
				return createErr
			}
			resp.Created = append(resp.Created, *res)
			resp.CreatedCount++
		}
		batchCompleted = true
		return nil
	})
	if batchCompleted && batchErr != nil {
		log.Printf("[HME] 账号 %s 批量别名已创建，但会话回写失败: %v", accountID, batchErr)
		batchErr = nil
	}
	if batchErr != nil {
		resp.SkippedCount = count - resp.CreatedCount
		resp.LastError = batchErr.Error()
		if b.store != nil && resp.SkippedCount > 0 {
			toRelease := resp.SkippedCount
			if errors.Is(batchErr, hme.ErrOutcomeUnknown) {
				toRelease--
			}
			if toRelease > 0 {
				b.store.ReleaseQuota(accountID, toRelease)
			}
		}
		if resp.CreatedCount == 0 {
			if errors.Is(batchErr, account.ErrHMEClientUnavailable) {
				return nil, mapAccountErr(batchErr)
			}
			var backendErr *BackendError
			if errors.As(batchErr, &backendErr) {
				return nil, backendErr
			}
			return nil, classifyUpstreamErr("批量创建失败", batchErr)
		}
	}

	b.invalidateAliasCache(accountID)
	if b.store != nil {
		resp.RemainingThisHour = b.store.RemainingQuota(accountID)
	}
	return resp, nil
}

func (b *managerBackend) getCachedAliases(accountID string) ([]hme.Alias, bool) {
	var epoch uint64
	if b.mgr != nil {
		var exists bool
		epoch, exists = b.mgr.CredentialEpoch(accountID)
		if !exists {
			return nil, false
		}
	}
	b.aliasMu.RLock()
	defer b.aliasMu.RUnlock()
	if b.aliasCache == nil {
		return nil, false
	}
	item, ok := b.aliasCache[accountID]
	if !ok || item.credentialEpoch != epoch || time.Since(item.fetchedAt) >= aliasCacheTTL {
		return nil, false
	}
	if b.mgr != nil {
		if current, ok := b.mgr.CredentialEpoch(accountID); !ok || current != epoch {
			return nil, false
		}
	}
	cached := make([]hme.Alias, len(item.aliases))
	copy(cached, item.aliases)
	return cached, true
}

func (b *managerBackend) setCachedAliases(accountID string, aliases []hme.Alias) {
	var epoch uint64
	if b.mgr != nil {
		var ok bool
		epoch, ok = b.mgr.CredentialEpoch(accountID)
		if !ok {
			return
		}
	}
	b.setCachedAliasesAtEpoch(accountID, epoch, aliases)
}

func (b *managerBackend) setCachedAliasesAtEpoch(accountID string, epoch uint64, aliases []hme.Alias) {
	// 计数在写入侧一次算清，读取侧只做 O(1) 取值
	active := 0
	for i := range aliases {
		if aliases[i].Active {
			active++
		}
	}
	b.aliasMu.Lock()
	defer b.aliasMu.Unlock()
	if b.aliasCache == nil {
		b.aliasCache = make(map[string]*aliasCacheItem)
	}
	// 防御性拷贝，隔绝外部切片修改污染内部缓存
	cached := make([]hme.Alias, len(aliases))
	copy(cached, aliases)
	b.aliasCache[accountID] = &aliasCacheItem{
		aliases:         cached,
		credentialEpoch: epoch,
		total:           len(cached),
		active:          active,
		fetchedAt:       time.Now(),
	}
}

func (b *managerBackend) invalidateAliasCache(accountID string) {
	b.aliasMu.Lock()
	defer b.aliasMu.Unlock()
	if b.aliasCache != nil {
		delete(b.aliasCache, accountID)
	}
}

// ListAliases 列出账号的 HME 别名 (兼容保留包装)。
func (b *managerBackend) ListAliases(accountID string) ([]hme.Alias, error) {
	return b.ListAliasesContext(context.Background(), accountID)
}

// ListAliasesContext 支持 Context 贯穿的别名列表查询 (PR-05 F10)。
func (b *managerBackend) ListAliasesContext(ctx context.Context, accountID string) ([]hme.Alias, error) {
	if cached, ok := b.getCachedAliases(accountID); ok {
		return cached, nil
	}
	return b.RefreshAliasesContext(ctx, accountID)
}

// RefreshAliases 强制穿透缓存 (兼容保留包装)。
func (b *managerBackend) RefreshAliases(accountID string) ([]hme.Alias, error) {
	return b.RefreshAliasesContext(context.Background(), accountID)
}

// RefreshAliasesContext 支持 Context 贯穿的强制穿透拉取 (PR-05 F10)。
func (b *managerBackend) RefreshAliasesContext(ctx context.Context, accountID string) ([]hme.Alias, error) {
	var aliases []hme.Alias
	err := b.mgr.WithHMEClientContextSession(ctx, accountID, func(client *hme.Client, epoch uint64) error {
		var listErr error
		aliases, listErr = client.ListAliasesWithContext(ctx)
		if listErr != nil {
			return listErr
		}
		activeCount := 0
		for _, al := range aliases {
			if al.Active {
				activeCount++
			}
		}
		// 与同账号 HME 创建共用客户端锁，避免旧快照在创建成功后回写计数或缓存。
		if err := b.mgr.UpdateAliasCountsIfEpoch(accountID, epoch, len(aliases), activeCount); err != nil {
			if errors.Is(err, account.ErrSessionChanged) {
				return err
			}
			return &BackendError{Status: http.StatusInternalServerError, Code: "PERSISTENCE_ERROR", Message: "别名数量保存失败"}
		}
		b.setCachedAliasesAtEpoch(accountID, epoch, aliases)
		return nil
	})
	if err != nil {
		if errors.Is(err, account.ErrHMEClientUnavailable) {
			return nil, mapAccountErr(err)
		}
		var backendErr *BackendError
		if errors.As(err, &backendErr) {
			return nil, backendErr
		}
		return nil, classifyUpstreamErr("获取别名列表失败", err)
	}
	// 返回防御性独立拷贝，避免并发调用者直接修改缓存底切片
	res := make([]hme.Alias, len(aliases))
	copy(res, aliases)
	// 自愈「别名 → 母号」路由：本次拉取已确知归属，直接登记，免去后续盲扫
	if b.onAliasesFetched != nil {
		b.onAliasesFetched(accountID, res)
	}
	// 将远端真实状态 (active/inactive) 与 provider_alias_id 同步持久化至库存表 (PR-09)
	if b.store != nil {
		_ = b.store.SyncAliasInventory(accountID, res)
	}
	return res, nil
}

func (b *managerBackend) resolveAliasIdentifiersContext(ctx context.Context, accountID, identifier string) (anonymousID string, email string, err error) {
	accountID = strings.TrimSpace(accountID)
	identifier = strings.TrimSpace(identifier)
	if accountID == "" || identifier == "" {
		return "", "", errors.New("empty accountID or identifier")
	}

	if strings.Contains(identifier, "@") {
		email = strings.ToLower(identifier)
		// 1. 优先从内存缓存查找
		if cached, ok := b.getCachedAliases(accountID); ok {
			for _, al := range cached {
				if strings.EqualFold(al.Email, email) && al.AnonymousID != "" {
					anonymousID = al.AnonymousID
					break
				}
			}
		}
		// 2. 若内存未命中，从本地数据库查找
		if anonymousID == "" && b.store != nil {
			inv, errQ := b.store.GetInventoryAlias(email)
			if errQ != nil && !errors.Is(errQ, sql.ErrNoRows) {
				return "", "", fmt.Errorf("query inventory alias for email %s failed: %w", email, errQ)
			}
			if inv != nil {
				// 关键校验：按邮箱从本地查询后，必须验证 inventory.AccountID
				if inv.AccountID != "" && inv.AccountID != accountID {
					return "", "", fmt.Errorf("alias %s belongs to account %s, not %s: %w", email, inv.AccountID, accountID, store.ErrAllocationConflict)
				}
				if inv.ProviderAliasID != "" {
					anonymousID = inv.ProviderAliasID
				}
			}
		}
		// 3. 若仍未命中，从上游远端列表拉取并解析（同时刷新缓存）
		if anonymousID == "" {
			aliases, lerr := b.ListAliasesContext(ctx, accountID)
			if lerr != nil {
				return "", "", fmt.Errorf("list aliases from upstream for account %s failed: %w", accountID, lerr)
			}
			for _, al := range aliases {
				if strings.EqualFold(al.Email, email) && al.AnonymousID != "" {
					anonymousID = al.AnonymousID
					break
				}
			}
		}
		if anonymousID == "" {
			return "", "", fmt.Errorf("unable to resolve providerAliasID for email %s on account %s", email, accountID)
		}
		return anonymousID, email, nil
	}

	// 标识符为 anonymousID
	anonymousID = identifier
	// 1. 优先从内存缓存查找
	if cached, ok := b.getCachedAliases(accountID); ok {
		for _, al := range cached {
			if al.AnonymousID == anonymousID && al.Email != "" {
				email = strings.ToLower(al.Email)
				break
			}
		}
	}
	// 2. 若内存未命中，从本地数据库通过 provider_alias_id 查找
	if email == "" && b.store != nil {
		var dbEmail, dbAccID string
		errQ := b.store.DB().QueryRow(`
			SELECT email, account_id FROM alias_inventory
			WHERE provider_alias_id = ?
		`, anonymousID).Scan(&dbEmail, &dbAccID)
		if errQ != nil && !errors.Is(errQ, sql.ErrNoRows) {
			return "", "", fmt.Errorf("query inventory alias for provider_alias_id %s failed: %w", anonymousID, errQ)
		}
		if errQ == nil {
			if dbAccID != accountID {
				return "", "", fmt.Errorf("providerAliasID %s belongs to account %s, not %s: %w", anonymousID, dbAccID, accountID, store.ErrAllocationConflict)
			}
			if dbEmail != "" {
				email = strings.ToLower(dbEmail)
			}
		}
	}
	// 3. 若仍未命中，从上游远端列表拉取并解析（必须在远端操作前完成解析）
	if email == "" {
		aliases, lerr := b.ListAliasesContext(ctx, accountID)
		if lerr != nil {
			return "", "", fmt.Errorf("list aliases from upstream for account %s failed: %w", accountID, lerr)
		}
		for _, al := range aliases {
			if al.AnonymousID == anonymousID && al.Email != "" {
				email = strings.ToLower(al.Email)
				break
			}
		}
	}
	if email == "" {
		return "", "", fmt.Errorf("unable to resolve email for providerAliasID %s on account %s", anonymousID, accountID)
	}

	return anonymousID, email, nil
}

func (b *managerBackend) resolveAliasIdentifiers(accountID, identifier string) (anonymousID string, email string, err error) {
	return b.resolveAliasIdentifiersContext(context.Background(), accountID, identifier)
}

// SetAliasActive 停用或激活别名 (兼容保留包装)。
func (b *managerBackend) SetAliasActive(accountID, anonymousID string, active bool) (bool, error) {
	return b.SetAliasActiveContext(context.Background(), accountID, anonymousID, active)
}

// SetAliasActiveContext 支持 Context 贯穿的停用或激活别名 (PR-05 F10)。
func (b *managerBackend) SetAliasActiveContext(ctx context.Context, accountID, anonymousID string, active bool) (bool, error) {
	accountID = strings.TrimSpace(accountID)
	anonymousID = strings.TrimSpace(anonymousID)
	if accountID == "" || anonymousID == "" {
		return false, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: "accountID 和 anonymousID 不能为空"}
	}

	// 远端修改前可靠解析 account_id、anonymousID、email；不能用 "_" 忽略错误，无法确认目标时上游调用次数必须为 0
	resolvedAnonID, resolvedEmail, err := b.resolveAliasIdentifiersContext(ctx, accountID, anonymousID)
	if err != nil {
		return false, &BackendError{Status: http.StatusBadRequest, Code: "ALIAS_NOT_FOUND", Message: fmt.Sprintf("解析别名标识失败: %v", err)}
	}
	if resolvedAnonID == "" || resolvedEmail == "" {
		return false, &BackendError{Status: http.StatusBadRequest, Code: "ALIAS_NOT_FOUND", Message: fmt.Sprintf("无法完整解析别名标识 (account=%s, identifier=%s)", accountID, anonymousID)}
	}
	targetAnonID := resolvedAnonID

	var success bool
	var remoteCompleted bool
	err = b.mgr.WithHMEClientContext(ctx, accountID, func(client *hme.Client) error {
		var opErr error
		if active {
			success, opErr = client.ReactivateHMEWithContext(ctx, targetAnonID)
		} else {
			success, opErr = client.DeactivateHMEWithContext(ctx, targetAnonID)
		}
		if opErr == nil && success {
			remoteCompleted = true
			delta := -1
			if active {
				delta = 1
			}
			if countErr := b.mgr.AdjustAliasCounts(accountID, 0, delta); countErr != nil {
				log.Printf("[HME] 账号 %s 别名状态已改变，但数量回写失败: %v", accountID, countErr)
			}
		}
		return opErr
	})
	if remoteCompleted && err != nil {
		log.Printf("[HME] 账号 %s 别名状态已改变，但会话回写失败: %v", accountID, err)
		err = nil
	}
	if err != nil {
		if errors.Is(err, account.ErrHMEClientUnavailable) {
			return false, mapAccountErr(err)
		}
		msg := "操作失败"
		if !active {
			msg = "停用失败"
		} else {
			msg = "激活失败"
		}
		return false, classifyUpstreamErr(msg, err)
	}
	if !success {
		msg := "停用操作未成功"
		if active {
			msg = "激活操作未成功"
		}
		return false, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILED", Message: msg}
	}

	b.invalidateAliasCache(accountID)
	if b.store != nil {
		rState := store.RemoteInactive
		if active {
			rState = store.RemoteActive
		}
		if stErr := b.store.UpdateAliasRemoteState(accountID, targetAnonID, resolvedEmail, rState); stErr != nil {
			opName := "停用"
			if active {
				opName = "激活"
			}
			return false, &BackendError{
				Status:  http.StatusInternalServerError,
				Code:    "STORE_SYNC_PENDING_RECONCILIATION",
				Message: fmt.Sprintf("上游%s成功但本地库存状态同步失败 (待核对): %v", opName, stErr),
			}
		}
	}

	return success, nil
}

// UpdateAlias 更新别名备注 (label) 与说明 (note)。
func (b *managerBackend) UpdateAlias(accountID, anonymousID, label, note string) error {
	err := b.mgr.WithHMEClient(accountID, func(client *hme.Client) error {
		return client.UpdateMetaData(anonymousID, label, note)
	})
	if err != nil {
		if errors.Is(err, account.ErrHMEClientUnavailable) {
			return mapAccountErr(err)
		}
		return classifyUpstreamErr("更新备注失败", err)
	}
	b.updateCachedAliasLabel(accountID, anonymousID, label)
	return nil
}

func (b *managerBackend) updateCachedAliasLabel(accountID, anonymousID, label string) {
	b.aliasMu.Lock()
	defer b.aliasMu.Unlock()
	if b.aliasCache != nil {
		if item, ok := b.aliasCache[accountID]; ok && item != nil {
			for i := range item.aliases {
				if item.aliases[i].AnonymousID == anonymousID {
					item.aliases[i].Label = label
					break
				}
			}
		}
	}
}

// BatchUpdateAliases 批量更新别名备注 (label) 与说明 (note)。
func (b *managerBackend) BatchUpdateAliases(accountID string, anonymousIDs []string, label, note string) (BatchUpdateResult, error) {
	result := BatchUpdateResult{
		Total:     len(anonymousIDs),
		Succeeded: make([]string, 0, len(anonymousIDs)),
		Failed:    make([]string, 0),
	}
	if len(anonymousIDs) == 0 {
		return result, nil
	}

	borrowErr := b.mgr.WithHMEClient(accountID, func(client *hme.Client) error {
		// 并发扇出前先串行解析一次服务端点:否则 4 个 worker 会同时触发
		// ValidateSession 并交错写入 serviceURL/dsid(数据竞争 + 重复 Apple validate 风控暴露)。
		if err := client.EnsureService(); err != nil {
			return err
		}
		// 限制受控并发为 4，兼顾批处理吞吐量与 Apple 限流风控
		concurrency := 4
		if len(anonymousIDs) < concurrency {
			concurrency = len(anonymousIDs)
		}

		type task struct {
			id string
		}
		taskCh := make(chan task, len(anonymousIDs))
		for _, id := range anonymousIDs {
			taskCh <- task{id: id}
		}
		close(taskCh)

		var (
			mu sync.Mutex
			wg sync.WaitGroup
		)
		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// 派生 goroutine 的 panic 不受 gin.Recovery 保护，必须就地兜住
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[PANIC RECOVER] BatchUpdateAliases.worker: %v", r)
					}
				}()
				for t := range taskCh {
					updateErr := client.UpdateMetaData(t.id, label, note)
					mu.Lock()
					if updateErr == nil {
						result.Succeeded = append(result.Succeeded, t.id)
					} else {
						result.Failed = append(result.Failed, t.id)
					}
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		return nil
	})
	if borrowErr != nil {
		if errors.Is(borrowErr, account.ErrHMEClientUnavailable) {
			return result, mapAccountErr(borrowErr)
		}
		if len(result.Succeeded)+len(result.Failed) == 0 {
			return result, classifyUpstreamErr("批量更新备注失败", borrowErr)
		}
		log.Printf("[HME] 账号 %s 批量备注已提交，但会话未同步: %v", accountID, borrowErr)
		result.LastError = "批量修改已提交，但账号会话未同步，请刷新列表核对"
	}

	if len(result.Succeeded) > 0 {
		b.updateCachedAliasLabels(accountID, result.Succeeded, label)
	}

	return result, nil
}

func (b *managerBackend) updateCachedAliasLabels(accountID string, anonymousIDs []string, label string) {
	if len(anonymousIDs) == 0 {
		return
	}
	idMap := make(map[string]struct{}, len(anonymousIDs))
	for _, id := range anonymousIDs {
		idMap[id] = struct{}{}
	}

	b.aliasMu.Lock()
	defer b.aliasMu.Unlock()
	if b.aliasCache != nil {
		if item, ok := b.aliasCache[accountID]; ok && item != nil {
			for i := range item.aliases {
				if _, hit := idMap[item.aliases[i].AnonymousID]; hit {
					item.aliases[i].Label = label
				}
			}
		}
	}
}

// DeleteAlias 删除别名。
func (b *managerBackend) DeleteAlias(accountID, anonymousID string) error {
	accountID = strings.TrimSpace(accountID)
	anonymousID = strings.TrimSpace(anonymousID)
	if accountID == "" || anonymousID == "" {
		return &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: "accountID 和 anonymousID 不能为空"}
	}

	// 删除前必须完成解析，不能删除后再依赖远端查找；不能用 "_" 忽略错误，无法确认目标时上游调用次数必须为 0
	resolvedAnonID, resolvedEmail, err := b.resolveAliasIdentifiers(accountID, anonymousID)
	if err != nil {
		return &BackendError{Status: http.StatusBadRequest, Code: "ALIAS_NOT_FOUND", Message: fmt.Sprintf("解析别名标识失败: %v", err)}
	}
	if resolvedAnonID == "" || resolvedEmail == "" {
		return &BackendError{Status: http.StatusBadRequest, Code: "ALIAS_NOT_FOUND", Message: fmt.Sprintf("无法完整解析别名标识 (account=%s, identifier=%s)", accountID, anonymousID)}
	}
	targetAnonID := resolvedAnonID

	deltaActive := -1
	if cached, ok := b.getCachedAliases(accountID); ok {
		for _, al := range cached {
			if al.AnonymousID == targetAnonID || (resolvedEmail != "" && strings.EqualFold(al.Email, resolvedEmail)) {
				if !al.Active {
					deltaActive = 0
				}
				break
			}
		}
	}
	deleted := false
	err = b.mgr.WithHMEClient(accountID, func(client *hme.Client) error {
		if err := client.Delete(targetAnonID); err != nil {
			return err
		}
		deleted = true
		if countErr := b.mgr.AdjustAliasCounts(accountID, -1, deltaActive); countErr != nil {
			log.Printf("[HME] 账号 %s 别名已删除，但数量回写失败: %v", accountID, countErr)
		}
		return nil
	})
	if deleted && err != nil {
		log.Printf("[HME] 账号 %s 别名已删除，但会话回写失败: %v", accountID, err)
		err = nil
	}
	if err != nil {
		if errors.Is(err, account.ErrHMEClientUnavailable) {
			return mapAccountErr(err)
		}
		return classifyUpstreamErr("删除失败", err)
	}
	b.invalidateAliasCache(accountID)
	if b.store != nil {
		if stErr := b.store.UpdateAliasRemoteState(accountID, targetAnonID, resolvedEmail, store.RemoteDeleted); stErr != nil {
			return &BackendError{
				Status:  http.StatusInternalServerError,
				Code:    "STORE_SYNC_PENDING_RECONCILIATION",
				Message: fmt.Sprintf("上游删除成功但本地库存状态同步失败 (待核对): %v", stErr),
			}
		}
	}

	return nil
}
