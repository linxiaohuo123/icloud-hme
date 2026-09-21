/**
 * [INPUT]: 依赖 errors, fmt, log, sync, time, icloud-hme/internal/account, icloud-hme/internal/notify, icloud-hme/internal/scheduler
 * [OUTPUT]: 对外提供 CookieMonitor, NewCookieMonitor, EventSink
 * [POS]: server 的 Cookie 健康监控器 (PR-07 §10.4)，周期校验账号会话、凭据失效即标记 error 并在跳变沿推送通知，支持平稳停机等待
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

// Package server - Cookie 健康监控器。
//
// iCloud Web Cookie 约 24 小时必然过期，而 IMAP 读信(App Password)不受影响，
// 系统表面存活、出号已死，是最典型的静默降级。本监控器每周期用现有 Cookie
// 轻量调一次 HME 校验接口：
//   - 校验通过     → 状态回 active、刷新 LastValidated 与别名计数(等效会话保活)
//   - 401/403     → 账号标记 error(调度器/预热池自动跳过，管理台可见)
//   - 瞬时网络错误 → 保留原状态，仅记录日志
//
// 状态跳变沿(active→error / error→active)通过 EventSink 推送通知；
// 配额水位在成功校验后检查，越过阈值只告警一次，由 Sender 负责重提醒节流。
package server

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/notify"
	"icloud-hme/internal/scheduler"
)

// EventSink 抽象通知出口，监控器只依赖本接口(生产注入 *notify.Sender)。
type EventSink interface {
	Emit(event notify.Event)
	QuotaThreshold() int
}

// CookieMonitor 定时校验账号 Cookie 健康度的后台引擎。
type CookieMonitor struct {
	be         Backend
	notifier   EventSink
	interval   time.Duration
	throttle   time.Duration // 账号间节流间隔; 0 表示按账号数自动摊平
	logs       *scheduler.RingBuffer
	stopCh     chan struct{}
	once       sync.Once
	stopOnce   sync.Once
	wg         sync.WaitGroup
	mu         sync.Mutex
	prevStatus map[string]string // accountID → 上次已知状态
	prevQuota  map[string]int    // accountID → 上次已知 active 别名数
}

// 账号间节流的自动摊平边界。
const (
	// defaultCookieThrottle 是自动摊平的上限(与历史硬编码值一致)
	defaultCookieThrottle = 2 * time.Second
	// minCookieThrottle 是自动摊平的下限,防止账号极多时对 Apple 形成突发
	minCookieThrottle = 200 * time.Millisecond
)

// NewCookieMonitor 创建监控器。interval 为校验周期，默认 30 分钟，下限 5 分钟；
// notifier 可为 nil(仅记录日志不推送)。账号间节流默认按账号数自动摊平。
func NewCookieMonitor(be Backend, interval time.Duration, notifier EventSink) *CookieMonitor {
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	if interval < 5*time.Minute {
		interval = 5 * time.Minute
	}
	return &CookieMonitor{
		be:         be,
		notifier:   notifier,
		interval:   interval,
		logs:       scheduler.NewRingBuffer(100),
		stopCh:     make(chan struct{}),
		prevStatus: make(map[string]string),
		prevQuota:  make(map[string]int),
	}
}

// SetThrottle 显式设置账号间节流间隔(<=0 表示恢复自动摊平)。
//
// 自动摊平的动机:一轮校验耗时 = 账号数 × 节流间隔。若固定 2 秒，
// 2000 账号一轮要 66 分钟，远超 30 分钟周期，结果是 7×24 不停地探测 Apple。
func (m *CookieMonitor) SetThrottle(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d < 0 {
		d = 0
	}
	m.throttle = d
}

// perAccountGap 计算本轮应使用的账号间间隔。
func (m *CookieMonitor) perAccountGap(n int) time.Duration {
	if n <= 1 {
		return 0
	}
	m.mu.Lock()
	configured := m.throttle
	m.mu.Unlock()
	if configured > 0 {
		return configured
	}
	gap := m.interval / time.Duration(n)
	if gap > defaultCookieThrottle {
		gap = defaultCookieThrottle
	}
	if gap < minCookieThrottle {
		gap = minCookieThrottle
	}
	return gap
}

// Start 启动后台校验协程。首轮延迟 1 分钟，错开启动期 autoSyncAccounts 的请求。
func (m *CookieMonitor) Start() {
	m.once.Do(func() {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.loop()
		}()
	})
}

func (m *CookieMonitor) loop() {
	select {
	case <-m.stopCh:
		return
	case <-time.After(time.Minute):
	}
	m.validateOnce()
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.validateOnce()
		}
	}
}

// Stop 停止监控协程并平稳等待退出(线程安全且幂等，PR-07 §10.4)。
func (m *CookieMonitor) Stop() {
	m.stopOnce.Do(func() {
		close(m.stopCh)
	})
	m.wg.Wait()
}

// Logs 返回监控环形日志快照。
func (m *CookieMonitor) Logs() []scheduler.LogEntry { return m.logs.Get() }

// validateOnce 执行单轮全量账号校验，账号之间节流防 Apple IP 风控。
func (m *CookieMonitor) validateOnce() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[PANIC RECOVER] cookie_monitor.validateOnce: %v", r)
		}
	}()
	accounts := m.be.ListAccounts()

	// 按账号数自动摊平账号间节流，使一轮校验落在 interval 之内
	targets := make([]account.Summary, 0, len(accounts))
	for _, acc := range accounts {
		// 未配置 Cookie 的 pending 账号无从校验，交给人工配置流程
		if acc.HasCookies {
			targets = append(targets, acc)
		}
	}
	gap := m.perAccountGap(len(targets))

	checked := 0
	for _, acc := range targets {
		select {
		case <-m.stopCh:
			return
		default:
		}
		if checked > 0 && gap > 0 {
			select {
			case <-m.stopCh:
				return
			case <-time.After(gap):
			}
		}
		checked++
		m.validateAccount(acc.ID, acc.Name)
	}
	if checked > 0 {
		m.logs.Add(fmt.Sprintf("本轮 Cookie 校验完成 accounts=%d 间隔=%v", checked, gap))
	}
}

// validateAccount 校验单个账号：记录日志、在状态跳变沿推送通知、成功后检查配额水位。
func (m *CookieMonitor) validateAccount(id, name string) {
	m.mu.Lock()
	prev := m.prevStatus[id]
	m.mu.Unlock()

	err := m.be.ValidateAccount(id)
	switch {
	case err == nil:
		m.logs.Add(fmt.Sprintf("账号=%s Cookie 校验通过", name))
		if prev == "error" {
			m.emit(notify.KindCookieRecovered, id, name,
				"iCloud Cookie 已恢复",
				"账号的 Cookie 校验已恢复通过，出号链路已重新可用。")
		}
		m.setPrevStatus(id, "active")
		m.checkQuota(id, name)
	case errors.Is(err, account.ErrCookieExpired):
		m.logs.Add(fmt.Sprintf("账号=%s Cookie 已失效，账号标记为 error，请更新 Cookie", name))
		log.Printf("[CookieMonitor] 账号=%s(%s) Cookie 失效，已标记 error", name, id)
		if prev != "error" {
			m.emit(notify.KindCookieExpired, id, name,
				"iCloud Cookie 已失效",
				"账号的 Cookie 已失效，账号已标记为 error，出号链路已停止使用该账号，请尽快到管理台更新 Cookie。")
		}
		m.setPrevStatus(id, "error")
	default:
		// 瞬时错误(网络/超时):不改状态，避免网络抖动引发大面积误判
		m.logs.Add(fmt.Sprintf("账号=%s 校验暂时失败(网络原因)，保留原状态", name))
		log.Printf("[CookieMonitor] 账号=%s(%s) 瞬时校验失败: %v", name, id, err)
	}
}

// checkQuota 在成功校验后读取最新别名计数，首次越过阈值时推送水位告警。
func (m *CookieMonitor) checkQuota(id, name string) {
	if m.notifier == nil {
		return
	}
	threshold := m.notifier.QuotaThreshold()
	if threshold <= 0 {
		return
	}
	// 必须走 O(1) 的按 ID 直查。此前是遍历 ListAccounts() 找自己，
	// 而本函数在「每账号一次」的循环内被调用，2000 账号会退化成 O(N²)：
	// 实测一轮校验仅统计部分就烧掉 7.8 秒纯 CPU。
	sum, err := m.be.GetAccount(id)
	if err != nil {
		return
	}
	active := sum.AliasActive
	m.mu.Lock()
	prev := m.prevQuota[id]
	m.prevQuota[id] = active
	m.mu.Unlock()
	if active >= threshold && prev < threshold {
		m.emit(notify.KindQuotaLow, id, name,
			"别名配额水位告警",
			fmt.Sprintf("账号的活跃别名已达 %d 个 (阈值 %d)，请补充账号或清理别名。", active, threshold))
	}
}

func (m *CookieMonitor) setPrevStatus(id, status string) {
	m.mu.Lock()
	m.prevStatus[id] = status
	m.mu.Unlock()
}

func (m *CookieMonitor) emit(kind, id, name, title, message string) {
	if m.notifier == nil {
		return
	}
	m.notifier.Emit(notify.Event{
		Kind:        kind,
		AccountID:   id,
		AccountName: name,
		Title:       title,
		Message:     message,
	})
}
