/**
 * [INPUT]: 依赖 testing, fmt, sync, time, icloud-hme/internal/account, hme, mail, notify, store
 * [OUTPUT]: 对外提供千账号量级的基准压测与扇出探测（ScaleBench 系列）
 * [POS]: internal/server 的规模基线压测套件，用于量化热路径随账号数增长的开销
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/notify"
	"icloud-hme/internal/store"
)

// 压测规模:2000 个母号、每账号 200 个别名(合计 40 万别名)，贴近"几千账号"的真实目标。
const (
	scaleAccounts      = 2000
	scaleAliasesPerAcc = 200
)

// buildScaleBackend 构造生产级 managerBackend(N 个账号 + 已填充的别名缓存)。
func buildScaleBackend(tb testing.TB, n, aliasPerAccount int) *managerBackend {
	tb.Helper()
	dir := tb.TempDir()
	st, err := store.NewStore(dir)
	if err != nil {
		tb.Fatalf("NewStore: %v", err)
	}
	tb.Cleanup(func() { _ = st.Close() })

	now := time.Now().Format(time.RFC3339)
	recs := make([]*store.AccountRecord, 0, n)
	for i := 0; i < n; i++ {
		recs = append(recs, &store.AccountRecord{
			ID:            fmt.Sprintf("acc_%04d", i),
			Name:          fmt.Sprintf("母号%04d", i),
			RealEmail:     fmt.Sprintf("user%04d@icloud.com", i),
			ICloudEmail:   fmt.Sprintf("user%04d@icloud.com", i),
			CookiesJSON:   `{"X-APPLE-WEBAUTH-TOKEN":"stub"}`,
			Host:          "icloud.com",
			Status:        "active",
			AliasTotal:    aliasPerAccount,
			AliasActive:   aliasPerAccount,
			CreatedAt:     now,
			UpdatedAt:     now,
			TagsJSON:      "[]",
			LastValidated: now,
		})
	}
	if err := st.SaveAccountsBatch(recs); err != nil {
		tb.Fatalf("SaveAccountsBatch: %v", err)
	}
	mgr, err := account.NewManager(dir, st)
	if err != nil {
		tb.Fatalf("NewManager: %v", err)
	}

	mb := &managerBackend{mgr: mgr, store: st}
	aliases := make([]hme.Alias, aliasPerAccount)
	for i := range aliases {
		aliases[i] = hme.Alias{
			Email:       fmt.Sprintf("alias_%04d@icloud.com", i),
			AnonymousID: fmt.Sprintf("anon_%04d", i),
			Label:       fmt.Sprintf("label-%04d", i),
			Active:      i%4 != 0, // 75% 活跃
		}
	}
	for _, sum := range mgr.ListSummaries() {
		mb.setCachedAliases(sum.ID, aliases)
	}
	return mb
}

// BenchmarkScaleListAccounts 量测 ListAccounts 在 2000x200 下的单次开销。
func BenchmarkScaleListAccounts(b *testing.B) {
	mb := buildScaleBackend(b, scaleAccounts, scaleAliasesPerAcc)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = mb.ListAccounts()
	}
}

// BenchmarkScaleListAccountsUncached 每次强制作废快照，量测未命中缓存的真实成本。
// 与 BenchmarkScaleListAccounts 的差值即短 TTL 快照带来的收益。
func BenchmarkScaleListAccountsUncached(b *testing.B) {
	mb := buildScaleBackend(b, scaleAccounts, scaleAliasesPerAcc)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mb.invalidateSummaryCache()
		_ = mb.ListAccounts()
	}
}

// ListAccounts 在 TTL 内必须复用同一份快照，避免突发请求把 N 条 Summary 反复构造一遍。
func TestListAccountsUsesSummaryCache(t *testing.T) {
	mb := buildScaleBackend(t, 50, 3)

	first := mb.ListAccounts()
	second := mb.ListAccounts()
	if len(first) == 0 || len(second) == 0 {
		t.Fatal("账号列表不应为空")
	}
	if &first[0] != &second[0] {
		t.Fatal("TTL 内应复用同一份快照，而不是每次重新构造")
	}

	// 作废后必须重新计算
	mb.invalidateSummaryCache()
	third := mb.ListAccounts()
	if &third[0] == &first[0] {
		t.Fatal("作废后应重新构造快照")
	}

	// 账号删除必须立即反映到列表(不能等 TTL 过期)
	target := third[0].ID
	before := len(third)
	if !mb.RemoveAccount(target) {
		t.Fatalf("RemoveAccount(%s) 失败", target)
	}
	if after := len(mb.ListAccounts()); after != before-1 {
		t.Fatalf("删除账号后列表应立即少一个: 期望 %d, 实际 %d", before-1, after)
	}
}

// BenchmarkScaleHasAccount 量测 hasAccount 查单个账号的开销(应为 O(1))。
func BenchmarkScaleHasAccount(b *testing.B) {
	mb := buildScaleBackend(b, scaleAccounts, scaleAliasesPerAcc)
	s := &Server{be: mb}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.hasAccount("acc_1999")
	}
}

// BenchmarkScaleCheckQuota 量测 Cookie 监控器单账号配额检查的开销。
func BenchmarkScaleCheckQuota(b *testing.B) {
	mb := buildScaleBackend(b, scaleAccounts, scaleAliasesPerAcc)
	notifier := notify.NewSender()
	notifier.UpdateSettings(notify.Settings{
		EventKinds:     notify.DefaultEventKinds(),
		QuotaThreshold: 100,
	})
	mon := NewCookieMonitor(mb, 0, notifier)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mon.checkQuota("acc_1999", "母号1999")
	}
}

// TestScaleCookieRoundCost 单次量测「一整轮 Cookie 校验」中统计部分的纯 CPU 成本。
// 该路径是 O(N × (N + A_total))，会随账号数二次方增长。
func TestScaleCookieRoundCost(t *testing.T) {
	if testing.Short() {
		t.Skip("规模探测，-short 下跳过")
	}
	mb := buildScaleBackend(t, scaleAccounts, scaleAliasesPerAcc)
	notifier := notify.NewSender()
	notifier.UpdateSettings(notify.Settings{
		EventKinds:     notify.DefaultEventKinds(),
		QuotaThreshold: 100,
	})
	mon := NewCookieMonitor(mb, 0, notifier)

	start := time.Now()
	for i := 0; i < scaleAccounts; i++ {
		mon.checkQuota(fmt.Sprintf("acc_%04d", i), "母号")
	}
	elapsed := time.Since(start)

	t.Logf("规模 N=%d A=%d: 一轮 Cookie 校验的配额统计耗时 %v (即 O(N²) 的那部分)",
		scaleAccounts, scaleAliasesPerAcc, elapsed)
}

// TestAliasRouteSelfHealing 验证「别名 → 母号」归属自愈：
// 只要某账号的别名列表被拉取过一次(启动预热 / GUI 浏览 / 手动刷新)，
// 它的别名就必须立刻可路由，从而不再依赖 mail_sync 里有上限的盲扫兜底。
func TestAliasRouteSelfHealing(t *testing.T) {
	mb := buildScaleBackend(t, 8, 2)

	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer st.Close()

	s := newWithBackendAndStore(mb, Config{AdminPassword: "admin-pass-2026-strong"}, st)
	if mb.onAliasesFetched == nil {
		t.Fatal("生产后端必须挂上别名归属自愈钩子，否则存量别名只能靠盲扫兜底")
	}
	if s.syncWorker == nil {
		t.Fatal("syncWorker 未装配")
	}

	// 模拟一次别名列表拉取完成后的回调
	mb.onAliasesFetched("acc_0007", []hme.Alias{
		{Email: "Legacy.Alias@icloud.com", Active: true},
		{Email: "", Active: true}, // 空值必须被忽略而不是登记成空键
	})
	if got := s.syncWorker.GetAliasAccount("legacy.alias@icloud.com"); got != "acc_0007" {
		t.Fatalf("自愈登记失败: 期望 acc_0007, 实际 %q", got)
	}
	if got := s.syncWorker.GetAliasAccount(""); got != "" {
		t.Fatalf("空别名不应被登记, 实际 %q", got)
	}
}

// countingInboxBackend 统计 IMAP 拉取次数，用于探测 mail_sync 的扇出放大。
type countingInboxBackend struct {
	*fakeBackend
	mu    sync.Mutex
	calls int
}

func (c *countingInboxBackend) ListInbox(InboxQuery) (InboxResult, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return InboxResult{}, nil
}

func (c *countingInboxBackend) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// TestScaleMailSyncFanout 探测取码时的别名归属解析成本：
//   - 路由表命中(出号写穿 / 落库自愈) → 一次主键点查 + 一次定向拉取
//   - 彻底未知(纯 Apple 侧手工创建的别名) → 盲扫必须被硬上限 + 负缓存约束
//   - 进程重启后内存表为空 → 仍必须靠持久化路由表定向解析，绝不能退化成盲扫
func TestScaleMailSyncFanout(t *testing.T) {
	if testing.Short() {
		t.Skip("规模探测，-short 下跳过")
	}
	const n = scaleAccounts
	newAccounts := func() []account.Summary {
		accounts := make([]account.Summary, 0, n)
		for i := 0; i < n; i++ {
			accounts = append(accounts, account.Summary{
				ID:             fmt.Sprintf("acc_%04d", i),
				Name:           fmt.Sprintf("母号%04d", i),
				Status:         "active",
				HasCookies:     true,
				HasAppPassword: true,
			})
		}
		return accounts
	}

	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer st.Close()

	// 出号写穿路由表(RecordLease 内部同步 upsert alias_routes)
	if err := st.RecordLease(store.LeaseRecord{
		Email:       "Legacy-Alias@iCloud.com", // 故意混合大小写
		AccountID:   "acc_1999",
		Tag:         "t",
		Status:      "completed",
		AllocatedAt: time.Now().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("RecordLease: %v", err)
	}

	// 场景 A: 路由表命中
	beA := &countingInboxBackend{fakeBackend: &fakeBackend{accounts: newAccounts()}}
	busA := mail.NewEventBus(5 * time.Minute)
	wA := NewMailSyncWorker(beA, st, busA, 2*time.Second)
	busA.Subscribe("legacy-alias@icloud.com")
	wA.syncOnce()
	t.Logf("规模 N=%d: 路由表命中时, 单轮 IMAP 拉取 %d 次", n, beA.callCount())
	if got := beA.callCount(); got > 1 {
		t.Fatalf("路由表命中时不应盲扫，实际拉取 %d 次", got)
	}

	// 场景 B(关键): 模拟进程重启 —— 全新 worker、内存表为空、别名不在流水表里，
	// 但此前已通过列表自愈把归属持久化到 alias_routes。
	_ = st.UpsertAliasRoutes("acc_1500", []string{"self-healed@icloud.com"})
	beB := &countingInboxBackend{fakeBackend: &fakeBackend{accounts: newAccounts()}}
	busB := mail.NewEventBus(5 * time.Minute)
	wB := NewMailSyncWorker(beB, st, busB, 2*time.Second)
	if wB.GetAliasAccount("self-healed@icloud.com") != "" {
		t.Fatal("新 worker 的内存表应为空，才能验证持久化路径")
	}
	busB.Subscribe("self-healed@icloud.com")
	wB.syncOnce()
	t.Logf("规模 N=%d: 重启后靠持久化路由表, 单轮 IMAP 拉取 %d 次", n, beB.callCount())
	if got := beB.callCount(); got != 1 {
		t.Fatalf("重启后应靠持久化路由定向拉取 1 次，实际 %d 次(退化成盲扫)", got)
	}

	// 场景 C: 彻底未知 → 盲扫必须被硬上限约束
	beC := &countingInboxBackend{fakeBackend: &fakeBackend{accounts: newAccounts()}}
	busC := mail.NewEventBus(5 * time.Minute)
	wC := NewMailSyncWorker(beC, st, busC, 2*time.Second)
	busC.Subscribe("never-seen-alias@icloud.com")
	wC.syncOnce()
	first := beC.callCount()
	t.Logf("规模 N=%d: 彻底未知时, 单轮 IMAP 拉取 %d 次 (硬上限 %d)", n, first, maxUnknownAliasProbeAccounts)
	if first > maxUnknownAliasProbeAccounts {
		t.Fatalf("盲扫必须被硬上限约束在 %d 次以内，实际 %d 次", maxUnknownAliasProbeAccounts, first)
	}

	// 负缓存: 紧接着再跑一轮不得重复盲扫
	wC.syncOnce()
	if second := beC.callCount(); second != first {
		t.Fatalf("负缓存失效: 第二轮又发起了 %d 次拉取", second-first)
	}
	t.Logf("负缓存生效: 第二轮新增 IMAP 拉取 0 次")
}
