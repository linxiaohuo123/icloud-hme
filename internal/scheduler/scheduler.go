/**
 * [INPUT]: 依赖 icloud-hme/internal/account, icloud-hme/internal/hme, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 Scheduler, NewScheduler, LogEntry, Status, isInDailyWindow, isTransientCreateError, isDurationExpired
 * [POS]: internal/scheduler 的定时别名补货引擎与环形日志中心，负责平滑号池供给与风控防封
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package scheduler

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/store"
)

// LogEntry 任务日志条目
type LogEntry struct {
	Time    string `json:"time"`
	Message string `json:"message"`
}

// RingBuffer 环形内存日志缓冲区 (固定容量循环队列，零重分配)
type RingBuffer struct {
	mu       sync.RWMutex
	capacity int
	entries  []LogEntry
	head     int
	count    int
}

func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = 200
	}
	return &RingBuffer{
		capacity: capacity,
		entries:  make([]LogEntry, capacity),
	}
}

func (r *RingBuffer) Add(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := LogEntry{
		Time:    time.Now().Format("15:04:05"),
		Message: msg,
	}
	if r.count < r.capacity {
		r.entries[(r.head+r.count)%r.capacity] = entry
		r.count++
	} else {
		r.entries[r.head] = entry
		r.head = (r.head + 1) % r.capacity
	}
}

func (r *RingBuffer) Get() []LogEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	res := make([]LogEntry, r.count)
	for i := 0; i < r.count; i++ {
		res[i] = r.entries[(r.head+i)%r.capacity]
	}
	return res
}

// AliasCreator 抽象别名创建函数
type AliasCreator func(accountID, label string) (*hme.CreateResult, error)

// AccountsProvider 抽象账号列表提供者
type AccountsProvider func() []account.Summary

// Scheduler 定时调度引擎
type Scheduler struct {
	store        *store.Store
	creator      AliasCreator
	accounts     AccountsProvider
	logs         *RingBuffer
	interval     time.Duration
	stopCh       chan struct{}
	running      bool
	runMu        sync.Mutex
	mu           sync.Mutex
	roundRunning atomic.Bool  // 当前是否有一轮补货正在执行
	lastRunAt    atomic.Int64 // 最近一轮完成时间(UnixNano, 0=从未执行)
	paceMu       sync.Mutex
	lastPacedRun map[string]time.Time // 记录每个账号最近一次发号时间(用于平滑平摊)
	// wg 跟踪在途的补货轮次，使 Stop 能等到本轮收敛后再放行停机。
	wg sync.WaitGroup
}

// schedulerStopGrace 是停机时等待在途补货轮次收敛的上限。
const schedulerStopGrace = 10 * time.Second

func NewScheduler(s *store.Store, creator AliasCreator, accounts AccountsProvider) *Scheduler {
	return &Scheduler{
		store:        s,
		creator:      creator,
		accounts:     accounts,
		logs:         NewRingBuffer(200),
		interval:     5 * time.Minute,
		stopCh:       make(chan struct{}),
		lastPacedRun: make(map[string]time.Time),
	}
}

func (s *Scheduler) Logs() []LogEntry {
	return s.logs.Get()
}

// Status 调度器实时状态快照, 供管理台展示执行进度
type Status struct {
	Running     bool   `json:"running"`
	LastRunAt   string `json:"last_run_at,omitempty"` // RFC3339, 空=从未执行
	IntervalSec int    `json:"interval_seconds"`
}

func (s *Scheduler) Status() Status {
	st := Status{
		Running:     s.roundRunning.Load(),
		IntervalSec: int(s.interval / time.Second),
	}
	if last := s.lastRunAt.Load(); last != 0 {
		st.LastRunAt = time.Unix(0, last).Format(time.RFC3339)
	}
	return st
}

func (s *Scheduler) getStopCh() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopCh
}

func (s *Scheduler) Start() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.stopCh = make(chan struct{})
	stopCh := s.stopCh
	s.mu.Unlock()

	go func() {
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()

		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				s.RunOnce(false, 1)
			}
		}
	}()
}

func (s *Scheduler) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	close(s.stopCh)
	s.mu.Unlock()

	// 等待在途轮次收敛。若不等，Server.Close() 会紧接着关掉 SQLite，
	// 而此时 Apple 侧可能已经建号成功 —— 配额计数与流水写入会静默失败，
	// 造成「配额被消耗但本地无记录」的漂移。
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(schedulerStopGrace):
		log.Printf("[Scheduler] 等待在途补货轮次收敛超时(%s)，继续停机", schedulerStopGrace)
	}
}

// RunOnce 执行一次调度流程 (防重入)。
//
// manual=true 表示"立即执行":仅绕过日间窗口、持续时长与拟人化节奏等时间闸门，
// 仍然只处理【已启用】定时任务的账号，避免一次点击对全部母号突发建号触发风控。
// 需要连未启用账号一起强推时请显式调用 RunAllNow。
func (s *Scheduler) RunOnce(manual bool, countPerAccount int) (int, int) {
	return s.run(manual, false, countPerAccount)
}

// RunAllNow 立即对所有账号(含未启用定时任务的账号)执行一轮补货，供运维强推场景显式调用。
func (s *Scheduler) RunAllNow(countPerAccount int) (int, int) {
	return s.run(true, true, countPerAccount)
}

func (s *Scheduler) run(manual, includeDisabled bool, countPerAccount int) (int, int) {
	if !s.runMu.TryLock() {
		s.logs.Add("已有调度任务正在执行中，本次触发跳过")
		return 0, 0
	}
	defer s.runMu.Unlock()
	s.wg.Add(1)
	defer s.wg.Done()
	s.roundRunning.Store(true)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[PANIC RECOVER] scheduler.RunOnce: %v", r)
		}
		s.roundRunning.Store(false)
		s.lastRunAt.Store(time.Now().UnixNano())
	}()

	if s.store == nil {
		s.logs.Add("存储未就绪，无法执行补货")
		return 0, 0
	}

	stopCh := s.getStopCh()

	if countPerAccount <= 0 {
		countPerAccount = 1
	}
	startTime := time.Now()
	accs := s.accounts()

	// 统计需要处理的账号
	var targetAccs []account.Summary
	now := time.Now()
	for _, a := range accs {
		cfg := s.store.GetScheduleConfig(a.ID)
		if !cfg.Enabled && !includeDisabled {
			continue
		}
		if !manual {
			if cfg.Mode == "duration" && isDurationExpired(cfg, now) {
				continue
			}
			if cfg.Mode == "daily_window" && !isInDailyWindow(now, cfg.StartTime, cfg.EndTime) {
				continue
			}
			if !s.shouldPacedRun(a.ID, now) {
				continue
			}
		}
		targetAccs = append(targetAccs, a)
	}

	// 没有可执行目标:计划触发保持静默(避免空转心跳刷满日志缓冲);手动触发给出指引
	if len(targetAccs) == 0 {
		if manual {
			if includeDisabled {
				s.logs.Add("暂无账号可执行补货，请先在「账号管理」页添加账号")
			} else {
				s.logs.Add("没有已启用的定时任务，本轮未执行任何补货（如需对全部账号强推，请使用「强制全部补货」）")
			}
		}
		return 0, 0
	}

	startMsg := fmt.Sprintf("开始补货 · 共 %d 个账号 · 每账号 %d 个", len(targetAccs), countPerAccount)
	s.logs.Add(startMsg)

	var createdTotal, errorTotal atomic.Int64

	// 有界并发工作池: 最大 10 个 worker 并发处理不同账号，大幅压缩千号场景补货耗时
	workers := 10
	if len(targetAccs) < workers {
		workers = len(targetAccs)
	}

	taskCh := make(chan account.Summary, len(targetAccs))
	for _, a := range targetAccs {
		taskCh <- a
	}
	close(taskCh)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 派生 goroutine 的 panic 不受 gin.Recovery 保护，必须就地兜住，
			// 否则单个账号的异常会直接终止整个进程。
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[PANIC RECOVER] scheduler.worker: %v", r)
				}
			}()
			for a := range taskCh {
				select {
				case <-stopCh:
					return
				default:
				}
				c, e := s.processAccount(a, countPerAccount, stopCh)
				createdTotal.Add(int64(c))
				errorTotal.Add(int64(e))
			}
		}()
	}
	wg.Wait()

	totalCreated := int(createdTotal.Load())
	totalErrors := int(errorTotal.Load())

	duration := time.Since(startTime)
	durationStr := fmt.Sprintf("%dms", duration.Milliseconds())
	if duration >= time.Second {
		durationStr = fmt.Sprintf("%.1fs", duration.Seconds())
	}
	var endMsg string
	if totalErrors > 0 {
		endMsg = fmt.Sprintf("本轮补货结束 · 产出 %d 个 · 失败 %d 个 · 耗时 %s", totalCreated, totalErrors, durationStr)
	} else {
		endMsg = fmt.Sprintf("本轮补货结束 · 产出 %d 个 · 耗时 %s", totalCreated, durationStr)
	}
	s.logs.Add(endMsg)

	return totalCreated, totalErrors
}

// processAccount 负责单个账号的配额校验与别名生成。
func (s *Scheduler) processAccount(a account.Summary, countPerAccount int, stopCh <-chan struct{}) (int, int) {
	cfg := s.store.GetScheduleConfig(a.ID)
	accName := a.Name
	if accName == "" {
		accName = a.RealEmail
	}
	if accName == "" {
		accName = a.ID
	}

	if a.Status != "active" {
		s.logs.Add(fmt.Sprintf("[%s] 账号状态异常（%s），已跳过", accName, a.Status))
		return 0, 0
	}
	if a.AliasTotal >= 500 || a.AliasActive >= 500 {
		s.logs.Add(fmt.Sprintf("[%s] 别名已达 500 上限，自动熔断跳过", accName))
		return 0, 0
	}

	created := 0
	errTotal := 0

	for i := 0; i < countPerAccount; i++ {
		select {
		case <-stopCh:
			s.logs.Add(fmt.Sprintf("[%s] 补货任务被中断", accName))
			return created, errTotal
		default:
		}

		// 配额前置快速守卫（不提前扣减，统一交由 creator/be.CreateAlias 原子仲裁）
		if s.store != nil {
			rem := s.store.RemainingQuota(a.ID)
			if rem <= 0 {
				s.logs.Add(fmt.Sprintf("[%s] 本小时额度已用满 (%d/%d)", accName, cfg.HourlyQuota, cfg.HourlyQuota))
				break
			}
		}

		label := formatScheduleLabel(cfg.AliasLabel, accName, i+1)
		res, err := s.creator(a.ID, label)
		if err != nil {
			errTotal++
			errStr := err.Error()
			if strings.Contains(errStr, "RATE_LIMITED") || strings.Contains(errStr, "配额") {
				s.logs.Add(fmt.Sprintf("[%s] 本小时额度已用满 (%d/%d)", accName, cfg.HourlyQuota, cfg.HourlyQuota))
				break
			}
			if isTransientCreateError(err) {
				s.logs.Add(fmt.Sprintf("[%s] 遭遇瞬态故障 (%v)，已释放配额并退避重试", accName, err))
				break
			}
			if strings.Contains(errStr, "401") || strings.Contains(errStr, "Cookie") || strings.Contains(errStr, "403") {
				s.logs.Add(fmt.Sprintf("[%s] 凭据已失效/被封 (%v)，自动熔断", accName, err))
				break
			}
			s.logs.Add(fmt.Sprintf("[%s] 创建失败: %v", accName, err))
		} else if res == nil {
			// creator 返回 (nil, nil) 时必须短路:否则下面读 res.Email 会 nil 解引用，
			// 把整个进程带走(recover 只能兜住 worker，不能保证业务正确)。
			errTotal++
			s.logs.Add(fmt.Sprintf("[%s] 创建返回空结果，已跳过本轮该账号", accName))
			break
		} else {
			created++
			a.AliasActive++
			a.AliasTotal++
			s.recordPacedRun(a.ID, time.Now())
			s.logs.Add(fmt.Sprintf("[%s] 创建成功: %s (备注: %s)", accName, res.Email, label))
			if a.AliasTotal >= 500 || a.AliasActive >= 500 {
				s.logs.Add(fmt.Sprintf("[%s] 别名已达 500 上限，已停止后续创建", accName))
				break
			}
		}

		// 平滑节流：每个账号之间等待 2 秒，防 Apple IP 标记 (支持中断唤醒)
		select {
		case <-stopCh:
			s.logs.Add(fmt.Sprintf("[%s] 补货任务被中断", accName))
			return created, errTotal
		case <-time.After(2 * time.Second):
		}
	}
	return created, errTotal
}

// formatScheduleLabel 将调度配置的备注模板格式化为具体标签。
// 支持动态宏变量：
//
//	{date}    -> 8位年月日 (如 20260920)
//	{time}    -> 4位时分 (如 1400)
//	{seq}     -> 本轮该账号生成的序号 (如 1, 2...)
//	{account} -> 账号名称
func formatScheduleLabel(template, accountName string, seq int) string {
	tmpl := strings.TrimSpace(template)
	if tmpl == "" {
		tmpl = "scheduled"
	}
	now := time.Now()
	r := strings.NewReplacer(
		"{date}", now.Format("20060102"),
		"{time}", now.Format("1504"),
		"{seq}", fmt.Sprintf("%d", seq),
		"{account}", accountName,
	)
	out := r.Replace(tmpl)
	if len([]rune(out)) > 100 {
		out = string([]rune(out)[:100])
	}
	return out
}

// isInDailyWindow 检查当前时间是否处于设定的日间窗口 (HH:MM 格式)
// 支持常规窗口 (如 09:00 - 18:00) 及跨午夜窗口 (如 22:00 - 06:00)
func isInDailyWindow(now time.Time, startStr, endStr string) bool {
	startStr = strings.TrimSpace(startStr)
	endStr = strings.TrimSpace(endStr)
	if startStr == "" || endStr == "" {
		return true
	}

	parseHM := func(s string) (int, bool) {
		var h, m int
		n, err := fmt.Sscanf(s, "%d:%d", &h, &m)
		if err != nil || n != 2 || h < 0 || h > 23 || m < 0 || m > 59 {
			return 0, false
		}
		return h*60 + m, true
	}

	startM, ok1 := parseHM(startStr)
	endM, ok2 := parseHM(endStr)
	if !ok1 || !ok2 {
		return true
	}

	nowM := now.Hour()*60 + now.Minute()
	if startM == endM {
		return true
	}
	if startM < endM {
		return nowM >= startM && nowM < endM
	}
	// 跨午夜: 如 22:00 (1320) 到 06:00 (360)
	return nowM >= startM || nowM < endM
}

// shouldPacedRun 检查在拟人化平滑模式下当前时间是否允许该账号创建
func (s *Scheduler) shouldPacedRun(accountID string, now time.Time) bool {
	s.paceMu.Lock()
	defer s.paceMu.Unlock()

	last, exists := s.lastPacedRun[accountID]
	if !exists {
		return true
	}
	if s.store == nil {
		return true
	}

	remQuota := s.store.RemainingQuota(accountID)
	if remQuota <= 0 {
		return false
	}

	hourEnd := now.Truncate(time.Hour).Add(time.Hour)
	remTime := hourEnd.Sub(now)
	if remTime <= time.Minute {
		return true
	}

	spacing := remTime / time.Duration(remQuota+1)
	if spacing < 2*time.Minute {
		spacing = 2 * time.Minute
	}

	return now.Sub(last) >= spacing
}

func (s *Scheduler) recordPacedRun(accountID string, now time.Time) {
	s.paceMu.Lock()
	s.lastPacedRun[accountID] = now
	s.paceMu.Unlock()
}

// isTransientCreateError 检查是否属于上游瞬态故障 (网络抖动/边缘网关切换/临时限流)
func isTransientCreateError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	markers := []string{
		"http 421", "http 429", "http 500", "http 502", "http 503", "http 504",
		"too many requests", "timeout", "deadline exceeded", "connection reset",
		"temporary", "temporarily", "连接失败",
	}
	for _, m := range markers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// isDurationExpired 检查 duration 模式下任务是否已达到设定的持续时长
func isDurationExpired(cfg store.ScheduleConfig, now time.Time) bool {
	if cfg.Mode != "duration" || cfg.DurationHours <= 0 {
		return false
	}
	if strings.TrimSpace(cfg.StartedAt) == "" {
		return false
	}
	started, err := time.Parse(time.RFC3339, cfg.StartedAt)
	if err != nil {
		return false
	}
	return now.After(started.Add(time.Duration(cfg.DurationHours) * time.Hour))
}
