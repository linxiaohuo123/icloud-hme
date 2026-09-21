/**
 * [INPUT]: 依赖 sync, sync/atomic, time, mail.OTPResult
 * [OUTPUT]: 对外提供 EventBus, NewEventBus, CachedOTP
 * [POS]: internal/mail 的内存事件分发总线，解耦邮件接收与 HTTP 长轮询，提供基于 UID/UIDVALIDITY 边界的严格过滤与多事件并发隔离
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package mail

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// CachedOTP 包装带精确来源、UID 边界与过期时间的验证码事件。
type CachedOTP struct {
	EventID     string     `json:"event_id,omitempty"`
	AccountID   string     `json:"account_id,omitempty"`
	Email       string     `json:"email,omitempty"`
	Folder      string     `json:"folder,omitempty"`
	UIDValidity uint32     `json:"uid_validity,omitempty"`
	UID         uint32     `json:"uid,omitempty"`
	OTP         *OTPResult `json:"otp"`
	Subject     string     `json:"subject"`
	From        string     `json:"from"`
	Date        string     `json:"date"`
	ReceivedAt  time.Time  `json:"received_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
}

type subscriberEntry struct {
	ch                  chan *CachedOTP
	baselineUIDValidity uint32
	baselineUID         uint32
	hasBoundary         bool
}

// EventBus 是收信内存发布/订阅总线。
type EventBus struct {
	mu          sync.RWMutex
	subscribers map[string]map[uint64]*subscriberEntry // email -> subID -> subscriberEntry
	cache       map[string][]*CachedOTP                // email -> []*CachedOTP
	subCounter  uint64
	cacheTTL    time.Duration
}

// NewEventBus 创建事件总线实例。
func NewEventBus(cacheTTL time.Duration) *EventBus {
	if cacheTTL <= 0 {
		cacheTTL = 5 * time.Minute
	}
	return &EventBus{
		subscribers: make(map[string]map[uint64]*subscriberEntry),
		cache:       make(map[string][]*CachedOTP),
		cacheTTL:    cacheTTL,
	}
}

// Subscribe 订阅指定邮箱别名的验证码到达事件。
func (b *EventBus) Subscribe(email string) (uint64, chan *CachedOTP) {
	return b.SubscribeWithFresh(email, false)
}

// SubscribeWithFresh 订阅指定邮箱别名的验证码到达事件。
func (b *EventBus) SubscribeWithFresh(email string, fresh bool) (uint64, chan *CachedOTP) {
	email = strings.ToLower(strings.TrimSpace(email))
	b.mu.Lock()
	defer b.mu.Unlock()

	subID := atomic.AddUint64(&b.subCounter, 1)
	ch := make(chan *CachedOTP, 1)

	if !fresh {
		now := time.Now()
		var unexpired []*CachedOTP
		var matched *CachedOTP
		for _, ev := range b.cache[email] {
			if now.Before(ev.ExpiresAt) {
				unexpired = append(unexpired, ev)
				if matched == nil {
					matched = ev
				}
			}
		}
		b.cache[email] = unexpired
		if matched != nil {
			ch <- matched
		}
	}

	subs, ok := b.subscribers[email]
	if !ok {
		subs = make(map[uint64]*subscriberEntry)
		b.subscribers[email] = subs
	}
	subs[subID] = &subscriberEntry{
		ch:          ch,
		hasBoundary: false,
	}
	return subID, ch
}

// SubscribeWithBoundary 订阅满足 UIDVALIDITY 和 UIDNEXT 边界的验证码到达事件 (PR-06 Section 9.2, 9.5)。
// 只有在满足 UIDValidity == baselineUIDValidity 且 UID >= baselineUID 时才匹配交付。
func (b *EventBus) SubscribeWithBoundary(email string, baselineUIDValidity, baselineUID uint32) (uint64, chan *CachedOTP) {
	email = strings.ToLower(strings.TrimSpace(email))
	b.mu.Lock()
	defer b.mu.Unlock()

	subID := atomic.AddUint64(&b.subCounter, 1)
	ch := make(chan *CachedOTP, 1)

	now := time.Now()
	var unexpired []*CachedOTP
	var matched *CachedOTP
	for _, ev := range b.cache[email] {
		if now.Before(ev.ExpiresAt) {
			unexpired = append(unexpired, ev)
			if matched == nil {
				if baselineUIDValidity != 0 && ev.UIDValidity != 0 && ev.UIDValidity != baselineUIDValidity {
					matched = ev
					break
				}
				validMatch := (baselineUIDValidity == 0 || ev.UIDValidity == 0 || ev.UIDValidity == baselineUIDValidity)
				uidMatch := (baselineUID == 0 || ev.UID >= baselineUID)
				if validMatch && uidMatch {
					matched = ev
				}
			}
		}
	}
	b.cache[email] = unexpired

	if matched != nil {
		ch <- matched
	}

	subs, ok := b.subscribers[email]
	if !ok {
		subs = make(map[uint64]*subscriberEntry)
		b.subscribers[email] = subs
	}
	subs[subID] = &subscriberEntry{
		ch:                  ch,
		baselineUIDValidity: baselineUIDValidity,
		baselineUID:         baselineUID,
		hasBoundary:         true,
	}
	return subID, ch
}

// Publish 广播新到达的验证码。
func (b *EventBus) Publish(email, accountID, subject, from, date string, otp *OTPResult) {
	b.PublishEvent(&CachedOTP{
		AccountID: accountID,
		Email:     email,
		Subject:   subject,
		From:      from,
		Date:      date,
		OTP:       otp,
	})
}

// PublishEvent 广播包含精准身份和 UID 边界的新验证码事件。
func (b *EventBus) PublishEvent(ev *CachedOTP) {
	if ev == nil || ev.OTP == nil {
		return
	}
	email := strings.ToLower(strings.TrimSpace(ev.Email))
	if email == "" {
		return
	}
	now := time.Now()
	if ev.ReceivedAt.IsZero() {
		ev.ReceivedAt = now
	}
	if ev.ExpiresAt.IsZero() {
		ev.ExpiresAt = now.Add(b.cacheTTL)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	var unexpired []*CachedOTP
	for _, old := range b.cache[email] {
		if now.Before(old.ExpiresAt) {
			unexpired = append(unexpired, old)
		}
	}
	unexpired = append(unexpired, ev)
	b.cache[email] = unexpired

	if subs, ok := b.subscribers[email]; ok {
		for _, sub := range subs {
			if sub.hasBoundary {
				if sub.baselineUIDValidity != 0 && ev.UIDValidity != 0 && sub.baselineUIDValidity != ev.UIDValidity {
					continue
				}
				if sub.baselineUID != 0 && ev.UID < sub.baselineUID {
					continue
				}
			}
			select {
			case sub.ch <- ev:
			default:
			}
		}
	}
}

// ConsumeEvent 消费特定事件，绝不清除更晚到达的其它新事件 (PR-06 V04)。
func (b *EventBus) ConsumeEvent(email string, eventID string) {
	email = strings.ToLower(strings.TrimSpace(email))
	eventID = strings.TrimSpace(eventID)
	if email == "" || eventID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	var remaining []*CachedOTP
	for _, ev := range b.cache[email] {
		if ev.EventID != eventID {
			remaining = append(remaining, ev)
		}
	}
	b.cache[email] = remaining
}

// ConsumeCache 清除该别名的所有缓存 (兼容旧接口)。
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

// GetCached 获取过去有效期内收到的最新验证码缓存。
func (b *EventBus) GetCached(email string) *CachedOTP {
	email = strings.ToLower(strings.TrimSpace(email))
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	var unexpired []*CachedOTP
	var latest *CachedOTP
	for _, ev := range b.cache[email] {
		if now.Before(ev.ExpiresAt) {
			unexpired = append(unexpired, ev)
			latest = ev
		}
	}
	b.cache[email] = unexpired
	return latest
}

// CleanupCache 清理已过期的验证码缓存。
func (b *EventBus) CleanupCache() {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	for email, list := range b.cache {
		var unexpired []*CachedOTP
		for _, ev := range list {
			if now.Before(ev.ExpiresAt) {
				unexpired = append(unexpired, ev)
			}
		}
		if len(unexpired) == 0 {
			delete(b.cache, email)
		} else {
			b.cache[email] = unexpired
		}
	}
}
