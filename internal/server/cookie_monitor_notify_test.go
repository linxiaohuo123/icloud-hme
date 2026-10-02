package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/notify"
)

// recordingSink 记录监控器发出的事件, 阈值可配置。
type recordingSink struct {
	events    []notify.Event
	threshold int
	pool      int
}

func (r *recordingSink) Emit(ev notify.Event) { r.events = append(r.events, ev) }
func (r *recordingSink) QuotaThreshold() int  { return r.threshold }
func (r *recordingSink) PoolThreshold() int   { return r.pool }

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

func TestCookieMonitorIdentityMismatchNotifiesOnce(t *testing.T) {
	sink := &recordingSink{}
	fake := &fakeBackend{accounts: []account.Summary{{ID: "acc_1", Name: "一号", HasCookies: true, Status: "active"}}}
	fake.validateFunc = func(string) error { return account.ErrAccountIdentityMismatch }
	mon := NewCookieMonitor(fake, 30*time.Minute, sink)
	mon.validateAccount("acc_1", "一号")
	mon.validateAccount("acc_1", "一号")
	if got := countKinds(sink.events, notify.KindCookieExpired); got != 1 {
		t.Fatalf("identity mismatch must notify once, got %d", got)
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

// TestCookieMonitorMailAuthEdge 收信故障/恢复只在跳变沿各推一次，故障账号退出号池。
func TestCookieMonitorMailAuthEdge(t *testing.T) {
	sink := &recordingSink{}
	fake := &fakeBackend{accounts: []account.Summary{{ID: "acc_1", Name: "一号", HasCookies: true, HasAppPassword: true, Status: "active"}}}
	fake.validateFunc = func(string) error { return nil }
	probes := 0
	fake.onGetMailboxBoundaryContext = func(context.Context, string, string) (string, uint32, uint32, error) {
		probes++
		return "imap", 1, 1, nil
	}
	mon := NewCookieMonitor(fake, 30*time.Minute, sink)

	fake.accounts[0].MailAuthFailed = true
	mon.validateAccount("acc_1", "一号")
	mon.validateAccount("acc_1", "一号")
	if got := countKinds(sink.events, notify.KindMailFailed); got != 1 || probes != 2 {
		t.Fatalf("故障应只推 1 次且每轮探测, 实际 events=%d probes=%d", got, probes)
	}
	if ids := selectPoolAccounts(fake.accounts, ""); len(ids) != 0 {
		t.Fatalf("收信故障账号不应参与出号, 实际 %v", ids)
	}

	fake.accounts[0].MailAuthFailed = false
	mon.validateAccount("acc_1", "一号")
	mon.validateAccount("acc_1", "一号")
	if got := countKinds(sink.events, notify.KindMailRecovered); got != 1 {
		t.Fatalf("恢复应只推 1 次, 实际 %d", got)
	}
	if ids := selectPoolAccounts(fake.accounts, ""); len(ids) != 1 {
		t.Fatalf("恢复后应重新参与出号, 实际 %v", ids)
	}
}

// TestCookieMonitorPoolLowEdge 号源余量跌破阈值只推一次、回升后复位；停用别名同样占用 750 名额，故障/保护账号不计入。
func TestCookieMonitorPoolLowEdge(t *testing.T) {
	sink := &recordingSink{pool: 100}
	fake := &fakeBackend{accounts: []account.Summary{
		{ID: "acc_1", Name: "一号", Status: "active", AliasTotal: 740, AliasActive: 100}, // 可建 10
		{ID: "acc_bad", Name: "坏邮箱", Status: "active", MailAuthFailed: true},           // 不计入
		{ID: "acc_err", Name: "失效", Status: "error"},                                   // 不计入
	}}
	available := 50
	mon := NewCookieMonitor(fake, 30*time.Minute, sink)
	mon.poolAvailable = func() int { return available }

	mon.checkPool() // 50 + 10 = 60 < 100
	mon.checkPool()
	if got := countKinds(sink.events, notify.KindPoolLow); got != 1 {
		t.Fatalf("跌破阈值应只推 1 次, 实际 %d", got)
	}
	available = 200 // 回升复位
	mon.checkPool()
	available = 50 // 再次跌破
	mon.checkPool()
	if got := countKinds(sink.events, notify.KindPoolLow); got != 2 {
		t.Fatalf("回升后再次跌破应重新告警, 实际 %d", got)
	}
	if !strings.Contains(sink.events[len(sink.events)-1].Message, "合计 60") {
		t.Fatalf("余量应为 50 库存 + 10 可建: %s", sink.events[len(sink.events)-1].Message)
	}
}
