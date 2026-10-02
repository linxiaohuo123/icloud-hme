/**
 * [INPUT]: 依赖 sync, sync/atomic, net/http, icloud-hme/internal/auth
 * [OUTPUT]: 对外提供 RequestLimiter, WaiterToken, InflightToken 等请求与等待者资源准入守卫
 * [POS]: server 的 API 并发防护层，统一约束短阶段在途请求与长轮询等待者配额；管理员按凭据来源分桶，避免全局 API Key 挤占 Web 会话
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"icloud-hme/internal/auth"
)

const InflightTokenContextKey = "inflight_token"

// 准入拒绝错误定义
var (
	ErrWaiterKeyLimit         = &BackendError{Status: http.StatusTooManyRequests, Code: "VERIFY_WAITER_LIMIT", Message: "当前任务/邮箱等待者过多，请稍后重试"}
	ErrWaiterPrincipalLimit   = &BackendError{Status: http.StatusTooManyRequests, Code: "TOKEN_CONCURRENCY_LIMIT", Message: "主体并发等待连接数超限，请稍后重试"}
	ErrWaiterGlobalLimit      = &BackendError{Status: http.StatusServiceUnavailable, Code: "SERVER_BUSY", Message: "服务等待队列已满，请稍后重试"}
	ErrInflightPrincipalLimit = &BackendError{Status: http.StatusTooManyRequests, Code: "TOKEN_CONCURRENCY_LIMIT", Message: "主体在途请求过多，请稍后重试"}
	ErrInflightGlobalLimit    = &BackendError{Status: http.StatusServiceUnavailable, Code: "SERVER_BUSY", Message: "服务繁忙，请稍后重试"}
)

// RequestLimiterConfig 准入器配置参数
type RequestLimiterConfig struct {
	MaxWaitersGlobal        int
	MaxWaitersPerPrincipal  int
	MaxWaitersPerKey        int
	MaxInflightGlobal       int
	MaxInflightPerPrincipal int
}

// DefaultRequestLimiterConfig 缺省保护参数
func DefaultRequestLimiterConfig() RequestLimiterConfig {
	return RequestLimiterConfig{
		MaxWaitersGlobal:        1024,
		MaxWaitersPerPrincipal:  256,
		MaxWaitersPerKey:        8,
		MaxInflightGlobal:       128,
		MaxInflightPerPrincipal: 64,
	}
}

// RequestLimiter 统管 API 入口在途与长轮询等待者的并发准入
type RequestLimiter struct {
	cfg RequestLimiterConfig

	mu               sync.Mutex
	globalWaiters    int
	principalWaiters map[string]int
	keyWaiters       map[string]int

	globalInflight    int
	principalInflight map[string]int

	// 峰值统计
	peakWaiters  atomic.Int64
	peakInflight atomic.Int64

	// 拒绝计数
	waiterKeyRejections         atomic.Uint64
	waiterPrincipalRejections   atomic.Uint64
	waiterGlobalRejections      atomic.Uint64
	inflightPrincipalRejections atomic.Uint64
	inflightGlobalRejections    atomic.Uint64
}

// NewRequestLimiter 创建请求准入控制器
func NewRequestLimiter(cfg RequestLimiterConfig) *RequestLimiter {
	if cfg.MaxWaitersGlobal <= 0 {
		cfg.MaxWaitersGlobal = 1024
	}
	if cfg.MaxWaitersPerPrincipal <= 0 {
		cfg.MaxWaitersPerPrincipal = 256
	}
	if cfg.MaxWaitersPerKey <= 0 {
		cfg.MaxWaitersPerKey = 8
	}
	if cfg.MaxInflightGlobal <= 0 {
		cfg.MaxInflightGlobal = 128
	}
	if cfg.MaxInflightPerPrincipal <= 0 {
		cfg.MaxInflightPerPrincipal = 64
	}

	return &RequestLimiter{
		cfg:               cfg,
		principalWaiters:  make(map[string]int),
		keyWaiters:        make(map[string]int),
		principalInflight: make(map[string]int),
	}
}

// principalKey 生成限流分桶键。管理员会话与全局 API Key 共用主体 ID "admin" 以保持资源归属，
// 但限流必须按凭据来源分桶，避免自动化脚本占满配额后挤掉 Web 管理面板。
func principalKey(p auth.Principal) string {
	kind := strings.TrimSpace(string(p.Kind))
	if kind == "" {
		kind = "unknown"
	}
	id := strings.TrimSpace(p.ID)
	if id == "" {
		id = "anonymous"
	}
	key := kind + ":" + id
	if p.Kind == auth.PrincipalAdmin {
		if name := strings.TrimSpace(p.TokenName); name != "" {
			key += "#" + name
		}
	}
	return key
}

// AcquireWaiter 尝试获取长轮询等待者名额 (在单个临界区内原子判定全局、主体与单键容量)
func (l *RequestLimiter) AcquireWaiter(p auth.Principal, resourceKey string) (release func(), err error) {
	resourceKey = strings.TrimSpace(resourceKey)
	if resourceKey == "" {
		return func() {}, nil
	}
	pKey := principalKey(p)

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.globalWaiters >= l.cfg.MaxWaitersGlobal {
		l.waiterGlobalRejections.Add(1)
		return nil, ErrWaiterGlobalLimit
	}
	if l.principalWaiters[pKey] >= l.cfg.MaxWaitersPerPrincipal {
		l.waiterPrincipalRejections.Add(1)
		return nil, ErrWaiterPrincipalLimit
	}
	if l.keyWaiters[resourceKey] >= l.cfg.MaxWaitersPerKey {
		l.waiterKeyRejections.Add(1)
		return nil, ErrWaiterKeyLimit
	}

	l.globalWaiters++
	l.principalWaiters[pKey]++
	l.keyWaiters[resourceKey]++

	cur := int64(l.globalWaiters)
	for {
		peak := l.peakWaiters.Load()
		if cur <= peak || l.peakWaiters.CompareAndSwap(peak, cur) {
			break
		}
	}

	var once sync.Once
	release = func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()

			l.globalWaiters--
			if l.globalWaiters < 0 {
				l.globalWaiters = 0
			}

			if l.principalWaiters[pKey] > 1 {
				l.principalWaiters[pKey]--
			} else {
				delete(l.principalWaiters, pKey)
			}

			if l.keyWaiters[resourceKey] > 1 {
				l.keyWaiters[resourceKey]--
			} else {
				delete(l.keyWaiters, resourceKey)
			}
		})
	}
	return release, nil
}

// InflightToken 管理短阶段在途请求的生命周期
type InflightToken struct {
	limiter     *RequestLimiter
	hasGlobal   bool
	pKey        string
	releaseOnce sync.Once
}

// Release 释放短阶段在途名额 (幂等)
func (t *InflightToken) Release() {
	if t == nil {
		return
	}
	t.releaseOnce.Do(func() {
		if t.limiter == nil {
			return
		}
		t.limiter.mu.Lock()
		defer t.limiter.mu.Unlock()

		if t.hasGlobal {
			t.limiter.globalInflight--
			if t.limiter.globalInflight < 0 {
				t.limiter.globalInflight = 0
			}
			t.hasGlobal = false
		}
		if t.pKey != "" {
			if t.limiter.principalInflight[t.pKey] > 1 {
				t.limiter.principalInflight[t.pKey]--
			} else {
				delete(t.limiter.principalInflight, t.pKey)
			}
			t.pKey = ""
		}
	})
}

// AcquireGlobalInflight 获取全局短请求在途名额 (在鉴权前调用)
func (l *RequestLimiter) AcquireGlobalInflight() (*InflightToken, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.globalInflight >= l.cfg.MaxInflightGlobal {
		l.inflightGlobalRejections.Add(1)
		return nil, ErrInflightGlobalLimit
	}
	l.globalInflight++

	cur := int64(l.globalInflight)
	for {
		peak := l.peakInflight.Load()
		if cur <= peak || l.peakInflight.CompareAndSwap(peak, cur) {
			break
		}
	}

	return &InflightToken{
		limiter:   l,
		hasGlobal: true,
	}, nil
}

// BindPrincipalInflight 在鉴权通过后绑定主体并扣减单主体短请求名额
func (t *InflightToken) BindPrincipalInflight(p auth.Principal) error {
	if t == nil || t.limiter == nil {
		return nil
	}
	pKey := principalKey(p)

	t.limiter.mu.Lock()
	defer t.limiter.mu.Unlock()

	if t.limiter.principalInflight[pKey] >= t.limiter.cfg.MaxInflightPerPrincipal {
		t.limiter.inflightPrincipalRejections.Add(1)
		return ErrInflightPrincipalLimit
	}
	t.limiter.principalInflight[pKey]++
	t.pKey = pKey
	return nil
}

// StatsSnapshot 统计快照
type RequestLimiterStats struct {
	Config                      RequestLimiterConfig `json:"config"`
	ActiveWaiters               int                  `json:"active_waiters"`
	PeakWaiters                 int64                `json:"peak_waiters"`
	ActiveInflight              int                  `json:"active_inflight"`
	PeakInflight                int64                `json:"peak_inflight"`
	WaiterKeyRejections         uint64               `json:"waiter_key_rejections"`
	WaiterPrincipalRejections   uint64               `json:"waiter_principal_rejections"`
	WaiterGlobalRejections      uint64               `json:"waiter_global_rejections"`
	InflightPrincipalRejections uint64               `json:"inflight_principal_rejections"`
	InflightGlobalRejections    uint64               `json:"inflight_global_rejections"`
}

// Stats 获取当前准入快照
func (l *RequestLimiter) Stats() RequestLimiterStats {
	l.mu.Lock()
	activeWaiters := l.globalWaiters
	activeInflight := l.globalInflight
	l.mu.Unlock()

	return RequestLimiterStats{
		Config:                      l.cfg,
		ActiveWaiters:               activeWaiters,
		PeakWaiters:                 l.peakWaiters.Load(),
		ActiveInflight:              activeInflight,
		PeakInflight:                l.peakInflight.Load(),
		WaiterKeyRejections:         l.waiterKeyRejections.Load(),
		WaiterPrincipalRejections:   l.waiterPrincipalRejections.Load(),
		WaiterGlobalRejections:      l.waiterGlobalRejections.Load(),
		InflightPrincipalRejections: l.inflightPrincipalRejections.Load(),
		InflightGlobalRejections:    l.inflightGlobalRejections.Load(),
	}
}
