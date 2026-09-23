/**
 * [INPUT]: 依赖 sync, time, icloud-hme/internal/account, icloud-hme/internal/hme
 * [OUTPUT]: 对外提供 PrewarmedAlias, AliasBuffer, NewAliasBuffer
 * [POS]: server 的别名预热缓冲池与令牌桶补货器，实现内存缓冲出号与防 429 频控
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
)

// PrewarmedAlias 封装预热就绪的别名信息。
type PrewarmedAlias struct {
	Result    *hme.CreateResult
	AccountID string
	CreatedAt time.Time
}

// AliasBuffer 别名预热池与平滑补货引擎。
type AliasBuffer struct {
	be             Backend
	queue          chan *PrewarmedAlias
	capacity       int
	refillInterval time.Duration
	stopCh         chan struct{}
	once           sync.Once
	stopOnce       sync.Once
}

// NewAliasBuffer 创建预热池实例。
func NewAliasBuffer(be Backend, capacity int, refillInterval time.Duration) *AliasBuffer {
	if capacity <= 0 {
		capacity = 3
	}
	if refillInterval <= 0 {
		refillInterval = 20 * time.Second
	}
	return &AliasBuffer{
		be:             be,
		queue:          make(chan *PrewarmedAlias, capacity),
		capacity:       capacity,
		refillInterval: refillInterval,
		stopCh:         make(chan struct{}),
	}
}

// Start 启动后台平滑补货 Worker。
// 【安全铁律】：大号安全第一，绝对禁止后台私自向 Apple 发起预热建号！
// 任何自动向 Apple API 刷请求建号的行为都会极大增加主账号被风控封禁的风险。
// 预热后台协程彻底停用，出号一律按需现场创建。
func (b *AliasBuffer) Start() {
	// no-op: 彻底关闭后台自动预热创建
}

// Stop 停止补货协程 (线程安全且幂等)。
func (b *AliasBuffer) Stop() {
	b.stopOnce.Do(func() {
		close(b.stopCh)
	})
}

// Acquire 提取一个可用别名。
// 优先从预存通道弹出；通道为空时走现场按需创建。绝不自动后台预热补货。
func (b *AliasBuffer) Acquire(label string) (*hme.CreateResult, string, error) {
	// 1. 尝试从缓冲队列秒级弹出 (如有预存)
	select {
	case item := <-b.queue:
		return item.Result, item.AccountID, nil
	default:
	}

	// 2. 缓冲池为空，走同步现场创建
	return b.syncCreate(label)
}

// syncCreate 现场同步创建降级逻辑。
func (b *AliasBuffer) syncCreate(label string) (*hme.CreateResult, string, error) {
	accounts := b.be.ListAccounts()
	var candidates []account.Summary
	for _, acc := range accounts {
		if acc.Status != "error" && acc.HasCookies && acc.AliasTotal < account.MaxAliasesPerAccount && acc.AliasActive < account.MaxAliasesPerAccount {
			candidates = append(candidates, acc)
		}
	}
	if len(candidates) == 0 {
		return nil, "", fmt.Errorf("没有可用的健康 iCloud 账号")
	}

	// 优雅自适应负载均衡与配额避障：优先选用当前活跃别名最少、离配额上限最远的健康账号
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].AliasActive < candidates[j].AliasActive
	})

	for _, target := range candidates {
		res, err := b.be.CreateAlias(target.ID, label)
		if err == nil && res != nil {
			return res, target.ID, nil
		}
	}
	return nil, "", fmt.Errorf("所有可用账号创建别名均失败")
}
