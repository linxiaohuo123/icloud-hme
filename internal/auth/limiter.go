/**
 * [INPUT]: 依赖 sync, time
 * [OUTPUT]: 对外提供 Limiter, NewLimiter 等客户端 IP 限流能力
 * [POS]: internal/auth 的登录防暴力破解与内存防 DoS 限流组件
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package auth

import (
	"sync"
	"time"
)

// limiterEntry 记录一个 key 的失败次数与窗口起点。
type limiterEntry struct {
	firstFailure time.Time
	failures     int
}

// Limiter 按客户端 IP 的登录失败限流(固定窗口),线程安全。
//
// 每次访问时删除已过窗口的 key,key 数达到上限时淘汰最早窗口,
// 避免伪造来源地址造成内存 DoS。
type Limiter struct {
	mu          sync.Mutex
	now         func() time.Time
	window      time.Duration
	maxFailures int
	maxKeys     int
	failures    map[string]limiterEntry
}

// NewLimiter 创建限流器。now 为可注入时钟,window 为固定窗口,
// maxFailures 为窗口内允许的最大失败次数,maxKeys 为 key 数上限。
func NewLimiter(now func() time.Time, window time.Duration, maxFailures, maxKeys int) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{
		now:         now,
		window:      window,
		maxFailures: maxFailures,
		maxKeys:     maxKeys,
		failures:    make(map[string]limiterEntry),
	}
}

// Allow 记录一次失败并判断是否允许继续尝试。
//
// 返回 allowed=false 时,retryAfter 为建议等待时间(不超过窗口)。
func (l *Limiter) Allow(key string) (allowed bool, retryAfter time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, exists := l.failures[key]
	if !exists || now.Sub(entry.firstFailure) >= l.window {
		// 新窗口:重置计数
		l.failures[key] = limiterEntry{firstFailure: now, failures: 1}
		l.gcLocked(now)
		return true, 0
	}
	entry.failures++
	if entry.failures > l.maxFailures {
		l.failures[key] = entry
		retryAfter = l.window - now.Sub(entry.firstFailure)
		if retryAfter < 0 {
			retryAfter = 0
		}
		return false, retryAfter
	}
	l.failures[key] = entry
	return true, 0
}

// Success 标记登录成功,清除该 key 的失败记录。
func (l *Limiter) Success(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, key)
}

// gcLocked 清理过窗 key 并淘汰最早窗口,须持锁调用。
func (l *Limiter) gcLocked(now time.Time) {
	for k, e := range l.failures {
		if now.Sub(e.firstFailure) >= l.window {
			delete(l.failures, k)
		}
	}
	if len(l.failures) > l.maxKeys {
		oldest := time.Time{}
		var oldestKey string
		for k, e := range l.failures {
			if oldest.IsZero() || e.firstFailure.Before(oldest) {
				oldest = e.firstFailure
				oldestKey = k
			}
		}
		if oldestKey != "" {
			delete(l.failures, oldestKey)
		}
	}
}
