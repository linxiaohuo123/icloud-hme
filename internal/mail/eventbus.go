/**
 * [INPUT]: 依赖 sync, sync/atomic, time, mail.OTPResult
 * [OUTPUT]: 对外提供 EventBus, NewEventBus
 * [POS]: internal/mail 的内存事件分发总线，解耦邮件接收与 HTTP 长轮询，彻底消除 IMAP 锁争用
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package mail

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// CachedOTP 包装带过期时间的验证码缓存。
type CachedOTP struct {
	OTP       *OTPResult
	Subject   string
	From      string
	Date      string
	AccountID string
	ExpiresAt time.Time
}

// EventBus 是收信内存发布/订阅总线。
type EventBus struct {
	mu          sync.RWMutex
	subscribers map[string]map[uint64]chan *CachedOTP // email -> subID -> chan
	cache       map[string]*CachedOTP                 // email -> CachedOTP
	subCounter  uint64
	cacheTTL    time.Duration
}

// NewEventBus 创建事件总线实例。
func NewEventBus(cacheTTL time.Duration) *EventBus {
	if cacheTTL <= 0 {
		cacheTTL = 5 * time.Minute
	}
	return &EventBus{
		subscribers: make(map[string]map[uint64]chan *CachedOTP),
		cache:       make(map[string]*CachedOTP),
		cacheTTL:    cacheTTL,
	}
}

// Subscribe 订阅指定邮箱别名的验证码到达事件。
// 返回唯一的 subID 和带缓冲的通信 channel。
// 内部原子检查近期缓存：若已有未过期验证码，直接预填至 channel，彻底消除 TOCTOU 竞态漏洞。
func (b *EventBus) Subscribe(email string) (uint64, chan *CachedOTP) {
	return b.SubscribeWithFresh(email, false)
}

// SubscribeWithFresh 订阅指定邮箱别名的验证码到达事件。
// 若 fresh 为 true，则跳过历史缓存检查，只等待最新到达的邮件；
// 若 fresh 为 false，若已有未过期缓存，原子预填至 channel。
func (b *EventBus) SubscribeWithFresh(email string, fresh bool) (uint64, chan *CachedOTP) {
	email = strings.ToLower(strings.TrimSpace(email))
	b.mu.Lock()
	defer b.mu.Unlock()

	subID := atomic.AddUint64(&b.subCounter, 1)
	ch := make(chan *CachedOTP, 1)

	// 非 fresh 模式下，原子检查近期缓存：存在且未过期则立即注入 channel
	if !fresh {
		if cached, ok := b.cache[email]; ok {
			if time.Now().Before(cached.ExpiresAt) {
				ch <- cached
			} else {
				delete(b.cache, email)
			}
		}
	}

	subs, ok := b.subscribers[email]
	if !ok {
		subs = make(map[uint64]chan *CachedOTP)
		b.subscribers[email] = subs
	}
	subs[subID] = ch
	return subID, ch
}

// ConsumeCache 显式将指定邮箱的验证码缓存标记为已消费并清除，
// 杜绝后续重发验证码或二次提取时错误获取陈旧历史 OTP。
func (b *EventBus) ConsumeCache(email string) {
	email = strings.ToLower(strings.TrimSpace(email))
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.cache, email)
}

// Unsubscribe 注销订阅，防止 channel 泄漏。
func (b *EventBus) Unsubscribe(email string, subID uint64) {
	email = strings.ToLower(strings.TrimSpace(email))
	b.mu.Lock()
	defer b.mu.Unlock()

	if subs, ok := b.subscribers[email]; ok {
		delete(subs, subID)
		if len(subs) == 0 {
			delete(b.subscribers, email)
		}
	}
}

// HasSubscribers 检查当前是否有活跃的外部等待订阅者。
func (b *EventBus) HasSubscribers() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers) > 0
}

// SubscribedEmails 获取当前所有正在被等待订阅的邮箱别名列表。
func (b *EventBus) SubscribedEmails() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	emails := make([]string, 0, len(b.subscribers))
	for email := range b.subscribers {
		emails = append(emails, email)
	}
	return emails
}

// Publish 广播新到达的验证码。
// 写入内存缓存并通知所有正在挂起的订阅者。
func (b *EventBus) Publish(email, accountID, subject, from, date string, otp *OTPResult) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || otp == nil {
		return
	}

	item := &CachedOTP{
		OTP:       otp,
		Subject:   subject,
		From:      from,
		Date:      date,
		AccountID: accountID,
		ExpiresAt: time.Now().Add(b.cacheTTL),
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// 0. 顺带淘汰已过期缓存，防止长期运行内存无界膨胀
	now := time.Now()
	for k, v := range b.cache {
		if now.After(v.ExpiresAt) {
			delete(b.cache, k)
		}
	}

	// 1. 存入近期缓存 (覆盖旧验证码)
	b.cache[email] = item

	// 2. 唤醒所有挂起的订阅者 (非阻塞投递)
	if subs, ok := b.subscribers[email]; ok {
		for _, ch := range subs {
			select {
			case ch <- item:
			default:
			}
		}
	}
}

// GetCached 获取过去有效期内收到的验证码缓存。
// 若存在且未过期则返回，否则返回 nil。
func (b *EventBus) GetCached(email string) *CachedOTP {
	email = strings.ToLower(strings.TrimSpace(email))
	b.mu.Lock()
	defer b.mu.Unlock()

	item, ok := b.cache[email]
	if !ok {
		return nil
	}
	if time.Now().After(item.ExpiresAt) {
		delete(b.cache, email)
		return nil
	}
	return item
}

// CleanupCache 清理已过期的验证码缓存。
func (b *EventBus) CleanupCache() {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	for email, item := range b.cache {
		if now.After(item.ExpiresAt) {
			delete(b.cache, email)
		}
	}
}
