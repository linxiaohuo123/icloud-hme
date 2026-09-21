/**
 * [INPUT]: 依赖 internal/account, internal/hme, internal/store
 * [OUTPUT]: 对外提供 managerBackend 的别名相关方法 (CreateAlias, BatchCreateAlias, ListAliases, RefreshAliases, SetAliasActive, UpdateAlias, BatchUpdateAliases, DeleteAlias) 与 BatchCreateResult, BatchUpdateResult 类型
 * [POS]: internal/server 的别名业务门面实现
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
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
	// 1. 检查 500 别名上限熔断 (总数或活跃数达到 500)
	if acc, ok := b.mgr.GetAccount(accountID); ok && (acc.AliasTotal >= 500 || acc.AliasActive >= 500) {
		return nil, &BackendError{
			Status:  http.StatusBadRequest,
			Code:    "ALIAS_LIMIT_REACHED",
			Message: fmt.Sprintf("账号 %s 别名数量已达 Apple 物理上限 (总数: %d, 活跃: %d/500)，本地熔断阻断", accountID, acc.AliasTotal, acc.AliasActive),
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
		res, cerr := client.CreateAlias(label, 5)
		if cerr != nil {
			return cerr
		}
		result = res
		return nil
	})
	if err != nil {
		if b.store != nil {
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

// BatchCreateAlias 批量创建 HME 别名 (1-5个)。
func (b *managerBackend) BatchCreateAlias(accountID string, count int, labelPrefix string) (*BatchCreateResult, error) {
	accountID = strings.TrimSpace(accountID)
	labelPrefix = strings.TrimSpace(labelPrefix)
	if count < 1 || count > 5 {
		return nil, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: "count 必须在 1-5 之间"}
	}

	// 1. 检查 500 别名上限熔断 (总数或活跃数达到 500)
	if acc, ok := b.mgr.GetAccount(accountID); ok && (acc.AliasTotal+count > 500 || acc.AliasActive+count > 500) {
		return nil, &BackendError{
			Status:  http.StatusBadRequest,
			Code:    "ALIAS_LIMIT_REACHED",
			Message: fmt.Sprintf("账号 %s 别名数量已达或将超过 Apple 物理上限 (当前总数: %d, 活跃: %d/500)，本地熔断阻断", accountID, acc.AliasTotal, acc.AliasActive),
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
			res, createErr := client.CreateAlias(lbl, 3)
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
			b.store.ReleaseQuota(accountID, resp.SkippedCount)
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

// SetAliasActive 停用或激活别名。
func (b *managerBackend) SetAliasActive(accountID, anonymousID string, active bool) (bool, error) {
	var success bool
	err := b.mgr.WithHMEClient(accountID, func(client *hme.Client) error {
		var opErr error
		if active {
			success, opErr = client.ReactivateHME(anonymousID)
		} else {
			success, opErr = client.DeactivateHME(anonymousID)
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
	if active {
		_ = b.mgr.AdjustAliasCounts(accountID, 0, 1)
	} else {
		_ = b.mgr.AdjustAliasCounts(accountID, 0, -1)
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
	deltaActive := -1
	if cached, ok := b.getCachedAliases(accountID); ok {
		for _, al := range cached {
			if al.AnonymousID == anonymousID {
				if !al.Active {
					deltaActive = 0
				}
				break
			}
		}
	}
	err := b.mgr.WithHMEClient(accountID, func(client *hme.Client) error {
		return client.Delete(anonymousID)
	})
	if err != nil {
		if errors.Is(err, account.ErrHMEClientUnavailable) {
			return mapAccountErr(err)
		}
		return classifyUpstreamErr("删除失败", err)
	}
	_ = b.mgr.AdjustAliasCounts(accountID, -1, deltaActive)
	b.invalidateAliasCache(accountID)
	return nil
}
