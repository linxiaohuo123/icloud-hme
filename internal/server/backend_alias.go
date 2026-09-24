/**
 * [INPUT]: 依赖 internal/account, internal/hme, internal/store
 * [OUTPUT]: 对外提供 managerBackend 的别名相关方法 (CreateAlias, BatchCreateAlias, ListAliases, RefreshAliases, SetAliasActive, UpdateAlias, BatchUpdateAliases, DeleteAlias) 与 BatchCreateResult, BatchUpdateResult 类型
 * [POS]: internal/server 的别名业务门面实现
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
}

// BatchUpdateResult 批量修改别名备注结果。
type BatchUpdateResult struct {
	Total     int      `json:"total"`
	Succeeded []string `json:"succeeded"`
	Failed    []string `json:"failed"`
}

// aliasCacheTTL 是内存别名缓存生命周期(15分钟)。
// 别名是准静态数据，新增/删除/修改已具备精准缓存驱逐钩子，长效缓存消除日常跨洋阻塞。
const aliasCacheTTL = 15 * time.Minute

type aliasCacheItem struct {
	aliases []hme.Alias
	// total/active 在写入缓存时算一次并固化。
	// 若改为每次 ListAccounts 现场求和，2000 账号 × 200 别名就是每次调用 40 万次迭代。
	total     int
	active    int
	fetchedAt time.Time
}

// CreateAlias 创建 HME 别名。
func (b *managerBackend) CreateAlias(accountID, label string) (*hme.CreateResult, error) {
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
	err := b.mgr.WithHMEClient(accountID, func(client *hme.Client) error {
		res, cerr := b.durableCreateAlias(context.Background(), client, accountID, label, 5)
		if cerr != nil {
			return cerr
		}
		result = res
		return nil
	})
	if err != nil {
		if !errors.Is(err, hme.ErrOutcomeUnknown) && b.store != nil {
			b.store.ReleaseQuota(accountID, 1)
		}
		if errors.Is(err, account.ErrHMEClientUnavailable) {
			return nil, mapAccountErr(err)
		}
		return nil, classifyUpstreamErr("创建邮箱失败", err)
	}
	_ = b.mgr.AdjustAliasCounts(accountID, 1, 1)
	b.invalidateAliasCache(accountID)
	return result, nil
}

// durableCreateAlias 执行符合 F03 铁律的持久化创建状态机：
// 1. Generate candidate A
// 2. 持久化 intent(A, prepared) 并 Commit SQLite
// 3. 标记状态为 reserve_sent 并 Commit SQLite
// 4. 才向网络发送 Reserve(A)
// 5. 成功 -> succeeded; 明确失败 -> confirmed_failed 并允许重试下一候选; 未知异常 -> outcome_unknown 并坚决阻断重试
func (b *managerBackend) durableCreateAlias(ctx context.Context, client *hme.Client, accountID, label string, maxRetries int) (*hme.CreateResult, error) {
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
			_ = b.store.UpdateReserveIntentState(ctx, intentID, store.IntentStateSucceeded, anonID, "", "")
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

	for i := range intents {
		it := intents[i]
		_ = b.mgr.WithHMEClient(it.AccountID, func(client *hme.Client) error {
			aliases, listErr := client.ListAliasesWithContext(ctx)
			if listErr != nil {
				_ = b.store.UpdateReserveIntentState(ctx, it.IntentID, store.IntentStateOutcomeUnknown, "", "", fmt.Sprintf("reconciliation list failed: %v", listErr))
				intents[i].State = store.IntentStateOutcomeUnknown
				return listErr
			}

			var found *hme.Alias
			for j := range aliases {
				if strings.EqualFold(aliases[j].Email, it.CandidateEmail) {
					found = &aliases[j]
					break
				}
			}

			if found != nil {
				// FOUND: 证实已在上游成功落盘
				_ = b.store.UpdateReserveIntentState(ctx, it.IntentID, store.IntentStateSucceeded, found.AnonymousID, "", "")
				_ = b.store.AddInventoryAlias(it.AccountID, *found, "created", true)
				_ = b.store.UpsertAliasRoutes(it.AccountID, []string{found.Email})
				intents[i].State = store.IntentStateSucceeded
				intents[i].AnonymousID = found.AnonymousID
			} else {
				// INCONCLUSIVE_NOT_FOUND: 坚决保持 outcome_unknown，严禁标记失败，严禁产生第二候选！
				_ = b.store.UpdateReserveIntentState(ctx, it.IntentID, store.IntentStateOutcomeUnknown, "", "", "reconciliation inconclusive: candidate not found in upstream list")
				intents[i].State = store.IntentStateOutcomeUnknown
			}
			return nil
		})
	}
	return intents, nil
}

// BatchCreateAlias 批量创建 HME 别名 (1-5个)。
func (b *managerBackend) BatchCreateAlias(accountID string, count int, labelPrefix string) (*BatchCreateResult, error) {
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

	batchErr := b.mgr.WithHMEClient(accountID, func(client *hme.Client) error {
		for i := 0; i < count; i++ {
			lbl := labelPrefix
			if lbl != "" && count > 1 {
				lbl = fmt.Sprintf("%s %d", labelPrefix, i+1)
			}
			res, createErr := b.durableCreateAlias(context.Background(), client, accountID, lbl, 3)
			if createErr != nil {
				return createErr
			}
			resp.Created = append(resp.Created, *res)
			resp.CreatedCount++
			_ = b.mgr.AdjustAliasCounts(accountID, 1, 1)
		}
		return nil
	})
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
	b.aliasMu.RLock()
	defer b.aliasMu.RUnlock()
	if b.aliasCache == nil {
		return nil, false
	}
	item, ok := b.aliasCache[accountID]
	if !ok || time.Since(item.fetchedAt) >= aliasCacheTTL {
		return nil, false
	}
	cached := make([]hme.Alias, len(item.aliases))
	copy(cached, item.aliases)
	return cached, true
}

func (b *managerBackend) setCachedAliases(accountID string, aliases []hme.Alias) {
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
		aliases:   cached,
		total:     len(cached),
		active:    active,
		fetchedAt: time.Now(),
	}
}

func (b *managerBackend) invalidateAliasCache(accountID string) {
	b.aliasMu.Lock()
	defer b.aliasMu.Unlock()
	if b.aliasCache != nil {
		delete(b.aliasCache, accountID)
	}
}

// ListAliases 列出账号的 HME 别名(带 15 分钟内存 TTL 缓存, 避免每次刷新页面阻塞跨洋请求 Apple)。
func (b *managerBackend) ListAliases(accountID string) ([]hme.Alias, error) {
	if cached, ok := b.getCachedAliases(accountID); ok {
		return cached, nil
	}
	return b.RefreshAliases(accountID)
}

// RefreshAliases 强制穿透缓存，从 Apple 官方拉取最新别名列表并回填缓存。
func (b *managerBackend) RefreshAliases(accountID string) ([]hme.Alias, error) {
	var aliases []hme.Alias
	err := b.mgr.WithHMEClient(accountID, func(client *hme.Client) error {
		var listErr error
		aliases, listErr = client.ListAliases()
		return listErr
	})
	if err != nil {
		if errors.Is(err, account.ErrHMEClientUnavailable) {
			return nil, mapAccountErr(err)
		}
		return nil, classifyUpstreamErr("获取别名列表失败", err)
	}
	activeCount := 0
	for _, al := range aliases {
		if al.Active {
			activeCount++
		}
	}
	_ = b.mgr.UpdateAliasCounts(accountID, len(aliases), activeCount)
	b.setCachedAliases(accountID, aliases)

	// 返回防御性独立拷贝，避免并发调用者直接修改缓存底切片
	res := make([]hme.Alias, len(aliases))
	copy(res, aliases)
	// 自愈「别名 → 母号」路由：本次拉取已确知归属，直接登记，免去后续盲扫
	if b.onAliasesFetched != nil {
		b.onAliasesFetched(accountID, res)
	}
	return res, nil
}

func (b *managerBackend) resolveAliasIdentifiers(accountID, identifier string) (anonymousID string, email string, err error) {
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
			aliases, lerr := b.ListAliases(accountID)
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
		aliases, lerr := b.ListAliases(accountID)
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

// SetAliasActive 停用或激活别名。
func (b *managerBackend) SetAliasActive(accountID, anonymousID string, active bool) (bool, error) {
	accountID = strings.TrimSpace(accountID)
	anonymousID = strings.TrimSpace(anonymousID)
	if accountID == "" || anonymousID == "" {
		return false, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: "accountID 和 anonymousID 不能为空"}
	}

	// 远端修改前可靠解析 account_id、anonymousID、email；不能用 "_" 忽略错误，无法确认目标时上游调用次数必须为 0
	resolvedAnonID, resolvedEmail, err := b.resolveAliasIdentifiers(accountID, anonymousID)
	if err != nil {
		return false, &BackendError{Status: http.StatusBadRequest, Code: "ALIAS_NOT_FOUND", Message: fmt.Sprintf("解析别名标识失败: %v", err)}
	}
	if resolvedAnonID == "" || resolvedEmail == "" {
		return false, &BackendError{Status: http.StatusBadRequest, Code: "ALIAS_NOT_FOUND", Message: fmt.Sprintf("无法完整解析别名标识 (account=%s, identifier=%s)", accountID, anonymousID)}
	}
	targetAnonID := resolvedAnonID

	var success bool
	err = b.mgr.WithHMEClient(accountID, func(client *hme.Client) error {
		var opErr error
		if active {
			success, opErr = client.ReactivateHME(targetAnonID)
		} else {
			success, opErr = client.DeactivateHME(targetAnonID)
		}
		return opErr
	})
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

	if active {
		_ = b.mgr.AdjustAliasCounts(accountID, 0, 1)
	} else {
		_ = b.mgr.AdjustAliasCounts(accountID, 0, -1)
	}

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

	b.invalidateAliasCache(accountID)
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
	if borrowErr != nil && errors.Is(borrowErr, account.ErrHMEClientUnavailable) {
		return result, mapAccountErr(borrowErr)
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
	err = b.mgr.WithHMEClient(accountID, func(client *hme.Client) error {
		return client.Delete(targetAnonID)
	})
	if err != nil {
		if errors.Is(err, account.ErrHMEClientUnavailable) {
			return mapAccountErr(err)
		}
		return classifyUpstreamErr("删除失败", err)
	}
	if b.store != nil {
		if stErr := b.store.UpdateAliasRemoteState(accountID, targetAnonID, resolvedEmail, store.RemoteDeleted); stErr != nil {
			return &BackendError{
				Status:  http.StatusInternalServerError,
				Code:    "STORE_SYNC_PENDING_RECONCILIATION",
				Message: fmt.Sprintf("上游删除成功但本地库存状态同步失败 (待核对): %v", stErr),
			}
		}
	}

	_ = b.mgr.AdjustAliasCounts(accountID, -1, deltaActive)
	b.invalidateAliasCache(accountID)
	return nil
}
