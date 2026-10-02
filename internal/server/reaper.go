/**
 * [INPUT]: 依赖 sync, time, server.Backend
 * [OUTPUT]: 对外提供 AliasReaper, NewAliasReaper
 * [POS]: server 的别名回收器占位，遵从"不清理"原则已彻底停用：Start 为空操作，保留生命周期接口供装配调用
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"sync"
	"time"
)

// AliasReaper 僵尸别名垃圾回收器。
type AliasReaper struct {
	be       Backend
	interval time.Duration
	maxAge   time.Duration
	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewAliasReaper 创建收割机实例。
// interval: 巡检频率 (默认 1 小时)
// maxAge: 别名闲置超过该时间则自动停用 (默认 2 小时)
func NewAliasReaper(be Backend, interval, maxAge time.Duration) *AliasReaper {
	if interval <= 0 {
		interval = 1 * time.Hour
	}
	if maxAge <= 0 {
		maxAge = 2 * time.Hour
	}
	return &AliasReaper{
		be:       be,
		interval: interval,
		maxAge:   maxAge,
		stopCh:   make(chan struct{}),
	}
}

// Start 启动后台垃圾回收协程。
// 响应用户“不清理”指令，彻底停用后台静默清理/停用，保障任何既有别名绝不被自动处置。
func (r *AliasReaper) Start() {
	// no-op: 彻底停用后台自动清理与停用
}

// Stop 停止收割机 (线程安全且幂等)。
func (r *AliasReaper) Stop() {
	r.stopOnce.Do(func() {
		close(r.stopCh)
	})
}
