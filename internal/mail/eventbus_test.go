/**
 * [INPUT]: 依赖 testing, time, internal/mail
 * [OUTPUT]: 对外提供 TestEventBusPublishAndSubscribe, TestEventBusUnsubscribe, TestEventBusFreshAndConsume
 * [POS]: internal/mail 的内存事件总线订阅、发布、缓存消费与 fresh 模式单元测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package mail

import (
	"testing"
	"time"
)

func TestEventBusPublishAndSubscribe(t *testing.T) {
	bus := NewEventBus(50 * time.Millisecond)

	email := "test@icloud.com"
	otp := &OTPResult{Code: "123456"}

	// 1. 先订阅
	subID, ch := bus.Subscribe(email)
	defer bus.Unsubscribe(email, subID)

	// 2. 发布事件
	go bus.Publish(email, "acc_1", "Subject", "from@test.com", "2026-09-19T10:00:00Z", otp)

	// 3. 接收通知
	select {
	case item := <-ch:
		if item.OTP.Code != "123456" {
			t.Fatalf("期望 Code 123456, 得到: %s", item.OTP.Code)
		}
		if item.AccountID != "acc_1" {
			t.Fatalf("期望 AccountID acc_1, 得到: %s", item.AccountID)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("等待订阅通道超时")
	}

	// 4. 再次获取缓存 (应命中)
	cached := bus.GetCached(email)
	if cached == nil || cached.OTP.Code != "123456" {
		t.Fatal("期望缓存命中")
	}

	// 5. 等待缓存过期
	time.Sleep(60 * time.Millisecond)
	if bus.GetCached(email) != nil {
		t.Fatal("期望缓存已过期返回 nil")
	}

	bus.CleanupCache()
	if len(bus.cache) != 0 {
		t.Fatalf("期望 cache 已清空, 实际长度: %d", len(bus.cache))
	}
}

func TestEventBusUnsubscribe(t *testing.T) {
	bus := NewEventBus(1 * time.Minute)
	email := "test2@icloud.com"

	subID, _ := bus.Subscribe(email)
	bus.Unsubscribe(email, subID)

	bus.mu.RLock()
	defer bus.mu.RUnlock()
	if _, ok := bus.subscribers[email]; ok {
		t.Fatal("注销后 subscribers[email] 应被清除")
	}
}

func TestEventBusFreshAndConsume(t *testing.T) {
	bus := NewEventBus(5 * time.Minute)
	email := "fresh@icloud.com"
	oldOTP := &OTPResult{Code: "111111"}

	// 1. 预先发布一条旧验证码到缓存中
	bus.Publish(email, "acc_1", "Old Subject", "from@test.com", "2026-09-20T10:00:00Z", oldOTP)

	// 2. 使用 fresh = true 订阅，断言跳过缓存
	subID, ch := bus.SubscribeWithFresh(email, true)
	defer bus.Unsubscribe(email, subID)

	select {
	case <-ch:
		t.Fatal("fresh 模式不应读取历史缓存")
	default:
		// 符合预期，通道为空
	}

	// 3. 发布一条新验证码
	newOTP := &OTPResult{Code: "999999"}
	bus.Publish(email, "acc_1", "New Subject", "from@test.com", "2026-09-20T10:05:00Z", newOTP)

	select {
	case item := <-ch:
		if item.OTP.Code != "999999" {
			t.Fatalf("期望新验证码 999999, 实际: %s", item.OTP.Code)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("等待新验证码超时")
	}

	// 4. 测试 ConsumeCache
	if bus.GetCached(email) == nil {
		t.Fatal("清除前缓存应存在")
	}
	bus.ConsumeCache(email)
	if bus.GetCached(email) != nil {
		t.Fatal("调用 ConsumeCache 后缓存应被清空")
	}
}
