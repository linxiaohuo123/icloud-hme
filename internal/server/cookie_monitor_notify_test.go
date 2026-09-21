package server

import (
	"fmt"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/notify"
)

// recordingSink 记录监控器发出的事件, 阈值可配置。
type recordingSink struct {
	events    []notify.Event
	threshold int
}

func (r *recordingSink) Emit(ev notify.Event) { r.events = append(r.events, ev) }
func (r *recordingSink) QuotaThreshold() int  { return r.threshold }

// TestCookieMonitorEmitsExpiredAndRecovered 校验失效与恢复仅在跳变沿各推送一次。
func TestCookieMonitorEmitsExpiredAndRecovered(t *testing.T) {
	expiredErr := fmt.Errorf("%w: HTTP 401: unauthorized", account.ErrCookieExpired)
	sink := &recordingSink{}
	fake := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Name: "一号", HasCookies: true, Status: "active"}},
	}
	fake.validateFunc = func(id string) error {
		return expiredErr
	}
	mon := NewCookieMonitor(fake, 30*time.Minute, sink)
	mon.throttle = 0

	mon.validateAccount("acc_1", "一号")
	mon.validateAccount("acc_1", "一号") // 持续失效, 不应重复推送
	if got := countKinds(sink.events, notify.KindCookieExpired); got != 1 {
		t.Fatalf("失效事件应只推 1 次, 实际 %d", got)
	}

	fake.validateFunc = func(id string) error { return nil }
	mon.validateAccount("acc_1", "一号")
	if got := countKinds(sink.events, notify.KindCookieRecovered); got != 1 {
		t.Fatalf("恢复事件应只推 1 次, 实际 %d", got)
	}
	mon.validateAccount("acc_1", "一号") // 持续健康, 不再推送
	if got := countKinds(sink.events, notify.KindCookieRecovered); got != 1 {
		t.Fatalf("持续健康不应重复推送恢复事件, 实际 %d", got)
	}
}

// TestCookieMonitorQuotaEdge 校验配额水位只在首次越过阈值时告警。
func TestCookieMonitorQuotaEdge(t *testing.T) {
	sink := &recordingSink{threshold: 90}
	fake := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_1", Name: "一号", HasCookies: true, Status: "active", AliasActive: 10},
		},
	}
	fake.validateFunc = func(id string) error { return nil }
	mon := NewCookieMonitor(fake, 30*time.Minute, sink)
	mon.throttle = 0

	mon.validateAccount("acc_1", "一号") // 10 → 未达阈值
	if got := countKinds(sink.events, notify.KindQuotaLow); got != 0 {
		t.Fatalf("未达阈值不应告警, 实际 %d", got)
	}

	fake.accounts[0].AliasActive = 95
	mon.validateAccount("acc_1", "一号") // 95 → 越过阈值, 告警一次
	if got := countKinds(sink.events, notify.KindQuotaLow); got != 1 {
		t.Fatalf("越过阈值应告警 1 次, 实际 %d", got)
	}

	mon.validateAccount("acc_1", "一号") // 持续高于阈值, 不重复
	if got := countKinds(sink.events, notify.KindQuotaLow); got != 1 {
		t.Fatalf("持续越线不应重复告警, 实际 %d", got)
	}
}

// TestCookieMonitorNilNotifier 校验未配置通知时监控器照常工作。
func TestCookieMonitorNilNotifier(t *testing.T) {
	fake := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Name: "一号", HasCookies: true}},
	}
	fake.validateFunc = func(id string) error { return nil }
	mon := NewCookieMonitor(fake, 30*time.Minute, nil)
	mon.throttle = 0
	mon.validateOnce()
	if len(mon.Logs()) == 0 {
		t.Fatal("无通知器时应有校验日志")
	}
}

func countKinds(events []notify.Event, kind string) int {
	n := 0
	for _, ev := range events {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}
