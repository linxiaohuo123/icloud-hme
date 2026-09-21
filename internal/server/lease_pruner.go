/**
 * [INPUT]: 依赖 log, sync, time, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 LeasePruner, NewLeasePruner
 * [POS]: internal/server 的已用别名流水保留期清理引擎，防止 lease_records 无限增长
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"fmt"
	"log"
	"sync"
	"time"

	"icloud-hme/internal/store"
)

const (
	// leasePruneInterval 是清理任务的执行周期(每天一次)
	leasePruneInterval = 24 * time.Hour
	// leasePruneBatch 是单批删除条数
	leasePruneBatch = 5000
	// leasePruneMaxBatches 限制单次运行的最大批数，避免一次清理长时间占用写锁。
	// 每批 5000 条 × 100 批 = 单次最多 50 万条，剩余留到下次运行继续。
	leasePruneMaxBatches = 100
	// leasePruneInitialDelay 首次执行前的等待，错开启动期
	leasePruneInitialDelay = 5 * time.Minute
)

// LeasePruner 按保留期清理已用别名流水。
//
// 背景:几千账号规模下流水可达每天数万条(数千万条/年)，
// 而历史流水只用于审计展示，不影响取码(路由表独立保留)。
type LeasePruner struct {
	store     *store.Store
	retention time.Duration
	interval  time.Duration
	logs      *logRing
	stopCh    chan struct{}
	once      sync.Once
	stopOnce  sync.Once
}

// logRing 是最小环形日志，避免为一个后台任务引入额外依赖。
type logRing struct {
	mu      sync.Mutex
	entries []string
	max     int
}

func newLogRing(max int) *logRing { return &logRing{max: max} }

func (r *logRing) Add(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := time.Now().Format("15:04:05") + " " + msg
	if len(r.entries) >= r.max {
		r.entries = r.entries[1:]
	}
	r.entries = append(r.entries, entry)
}

func (r *logRing) Get() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.entries))
	copy(out, r.entries)
	return out
}

// NewLeasePruner 创建清理引擎。retention <= 0 表示保留全部(不清理)。
func NewLeasePruner(st *store.Store, retention time.Duration) *LeasePruner {
	return &LeasePruner{
		store:     st,
		retention: retention,
		interval:  leasePruneInterval,
		logs:      newLogRing(50),
		stopCh:    make(chan struct{}),
	}
}

// Enabled 返回是否配置了保留期。
func (p *LeasePruner) Enabled() bool {
	return p.store != nil && p.retention > 0
}

// Logs 返回最近的清理日志。
func (p *LeasePruner) Logs() []string { return p.logs.Get() }

// Start 启动后台清理协程(未配置保留期时为空操作)。
func (p *LeasePruner) Start() {
	if !p.Enabled() {
		return
	}
	p.once.Do(func() { go p.loop() })
}

// Stop 停止清理协程(幂等)。
func (p *LeasePruner) Stop() {
	p.stopOnce.Do(func() { close(p.stopCh) })
}

func (p *LeasePruner) loop() {
	select {
	case <-p.stopCh:
		return
	case <-time.After(leasePruneInitialDelay):
	}
	p.PruneOnce()

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.PruneOnce()
		}
	}
}

// PruneOnce 执行一轮清理，返回删除总条数。
func (p *LeasePruner) PruneOnce() int {
	if !p.Enabled() {
		return 0
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[PANIC RECOVER] lease_pruner.PruneOnce: %v", r)
		}
	}()

	cutoff := time.Now().Add(-p.retention)
	total := 0
	for batch := 0; batch < leasePruneMaxBatches; batch++ {
		n, err := p.store.PruneLeases(cutoff, leasePruneBatch)
		if err != nil {
			log.Printf("[LeasePruner] 清理失败: %v", err)
			break
		}
		total += n
		if n < leasePruneBatch {
			break
		}
	}
	if total > 0 {
		p.logs.Add(fmt.Sprintf("已清理 %d 条早于 %s 的流水，剩余 %d 条",
			total, cutoff.Format(time.RFC3339), p.store.CountLeases()))
		log.Printf("[LeasePruner] 已清理 %d 条早于 %s 的流水", total, cutoff.Format(time.RFC3339))
	} else {
		p.logs.Add("本轮无需清理")
	}
	return total
}
