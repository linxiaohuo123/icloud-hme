package server

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/scheduler"
)

// TestCookieMonitorIntervalGuard 校验周期参数的默认值与下限钳制。
func TestCookieMonitorIntervalGuard(t *testing.T) {
	if got := NewCookieMonitor(nil, 0, nil).interval; got != 30*time.Minute {
		t.Fatalf("零值应取默认 30m, 实际 %v", got)
	}
	if got := NewCookieMonitor(nil, time.Second, nil).interval; got != 5*time.Minute {
		t.Fatalf("过小周期应钳制到 5m, 实际 %v", got)
	}
	if got := NewCookieMonitor(nil, 45*time.Minute, nil).interval; got != 45*time.Minute {
		t.Fatalf("合法周期应原样保留, 实际 %v", got)
	}
}

// TestCookieMonitorValidateOnce 验证单轮校验:
// 只校验带 Cookie 的账号、失效与瞬时错误分别记录不同日志。
func TestCookieMonitorValidateOnce(t *testing.T) {
	expiredErr := fmt.Errorf("%w: HTTP 401: unauthorized", account.ErrCookieExpired)
	fake := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_ok", Name: "健康号", HasCookies: true},
			{ID: "acc_dead", Name: "过期号", HasCookies: true},
			{ID: "acc_pend", Name: "待配置", HasCookies: false},
		},
	}
	fake.validateFunc = func(id string) error {
		if id == "acc_dead" {
			return expiredErr
		}
		return nil
	}

	mon := NewCookieMonitor(fake, 30*time.Minute, nil)
	mon.throttle = 0 // 测试免节流
	mon.validateOnce()

	if fake.validateID != "acc_dead" {
		t.Fatalf("最后一个被校验的账号应为 acc_dead, 实际 %s", fake.validateID)
	}
	logs := mon.Logs()
	joined := joinLogs(logs)
	for _, want := range []string{
		"健康号 Cookie 校验通过",
		"过期号 Cookie 已失效",
		"本轮 Cookie 校验完成 accounts=2",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("监控日志缺少 %q, 实际:\n%s", want, joined)
		}
	}
	// 未配置 Cookie 的账号不应触发校验(只校验了 2 个)
	if got := strings.Count(joined, "Cookie 校验通过"); got != 1 {
		t.Fatalf("应只有 1 条校验通过记录, 实际 %d:\n%s", got, joined)
	}
}

// TestCookieMonitorTransientErrorKeepsStatus 瞬时错误只记录，不产生失效标记。
func TestCookieMonitorTransientErrorKeepsStatus(t *testing.T) {
	fake := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Name: "抖动号", HasCookies: true}},
	}
	fake.validateFunc = func(id string) error {
		return errors.New("连接失败: dial tcp: i/o timeout")
	}

	mon := NewCookieMonitor(fake, 30*time.Minute, nil)
	mon.throttle = 0
	mon.validateOnce()

	joined := joinLogs(mon.Logs())
	if strings.Contains(joined, "已失效") {
		t.Fatalf("瞬时错误不应记录失效标记:\n%s", joined)
	}
	if !strings.Contains(joined, "保留原状态") {
		t.Fatalf("瞬时错误应记录保留原状态:\n%s", joined)
	}
}

// TestCookieMonitorStopsMidRound 停止信号应在下一个账号前生效，不再继续校验。
func TestCookieMonitorStopsMidRound(t *testing.T) {
	fake := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_1", Name: "一号", HasCookies: true},
			{ID: "acc_2", Name: "二号", HasCookies: true},
		},
	}
	started := make(chan string)
	release := make(chan struct{})
	fake.validateFunc = func(id string) error {
		if id == "acc_1" {
			started <- id
			<-release // 阻塞到测试确认 Stop 已发出
		}
		return nil
	}

	mon := NewCookieMonitor(fake, 30*time.Minute, nil)
	mon.throttle = 0
	done := make(chan struct{})
	go func() {
		mon.validateOnce()
		close(done)
	}()

	<-started      // 一号正在校验
	mon.Stop()     // 此刻发出停止信号
	close(release) // 放行一号收尾
	select {
	case <-done: // 单轮应在二号之前退出
	case <-time.After(3 * time.Second):
		t.Fatal("停止后 validateOnce 未及时退出")
	}
	if fake.validateID == "acc_2" {
		t.Fatal("停止后不应继续校验第二个账号")
	}
}

// TestCookieMonitorNoValidateAfterStop 已停止的监控器不再执行任何校验。
func TestCookieMonitorNoValidateAfterStop(t *testing.T) {
	fake := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Name: "一号", HasCookies: true}},
	}
	mon := NewCookieMonitor(fake, 30*time.Minute, nil)
	mon.Stop()
	mon.validateOnce()
	if fake.validateID != "" {
		t.Fatal("停止后不应校验任何账号")
	}
	if len(mon.Logs()) != 0 {
		t.Fatal("停止后不应产生校验日志")
	}
}

// TestCookieMonitorLogRingBounds 日志环形缓冲不超过容量。
func TestCookieMonitorLogRingBounds(t *testing.T) {
	fake := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Name: "号", HasCookies: true}},
	}
	mon := NewCookieMonitor(fake, 30*time.Minute, nil)
	mon.throttle = 0
	for i := 0; i < 200; i++ {
		mon.validateOnce()
	}
	if len(mon.Logs()) > 100 {
		t.Fatalf("环形日志应不超过 100 条, 实际 %d", len(mon.Logs()))
	}
}

// TestCookieMonitorConcurrentStop 并发 Start/Stop 不应 panic。
func TestCookieMonitorConcurrentStop(t *testing.T) {
	fake := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Name: "一号", HasCookies: true}},
	}
	fake.validateFunc = func(id string) error { return nil }
	mon := NewCookieMonitor(fake, 5*time.Minute, nil)
	mon.Start()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mon.Stop()
		}()
	}
	wg.Wait()
}

func joinLogs(entries []scheduler.LogEntry) string {
	var sb strings.Builder
	for _, e := range entries {
		sb.WriteString(e.Time)
		sb.WriteByte(' ')
		sb.WriteString(e.Message)
		sb.WriteByte('\n')
	}
	return sb.String()
}
