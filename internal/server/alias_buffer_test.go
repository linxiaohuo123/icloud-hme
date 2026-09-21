package server

import (
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
)

func TestAliasBufferAcquire(t *testing.T) {
	fake := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_1", Status: "active", HasCookies: true},
		},
		created: &hme.CreateResult{
			Email: "prewarmed@icloud.com",
			Label: "test",
		},
	}

	buf := NewAliasBuffer(fake, 2, 50*time.Millisecond)

	// 1. 预先向通道塞入一个
	buf.queue <- &PrewarmedAlias{
		Result: &hme.CreateResult{
			Email: "instant@icloud.com",
			Label: "instant",
		},
		AccountID: "acc_1",
		CreatedAt: time.Now(),
	}

	// 2. 提取 (内存队列出队)
	start := time.Now()
	res, accID, err := buf.Acquire("test")
	duration := time.Since(start)

	if err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}
	if res.Email != "instant@icloud.com" {
		t.Fatalf("期望 instant@icloud.com, 得到: %s", res.Email)
	}
	if accID != "acc_1" {
		t.Fatalf("期望 acc_1, 得到: %s", accID)
	}
	if duration > 10*time.Millisecond {
		t.Fatalf("预热池弹出耗时应小于 10ms, 实际耗时: %v", duration)
	}

	// 3. 再次提取 (通道已空，走同步降级)
	res2, _, err2 := buf.Acquire("fallback")
	if err2 != nil {
		t.Fatalf("同步降级失败: %v", err2)
	}
	if res2.Email != "prewarmed@icloud.com" {
		t.Fatalf("期望 prewarmed@icloud.com, 得到: %s", res2.Email)
	}
}
