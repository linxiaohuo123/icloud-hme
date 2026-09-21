/**
 * [INPUT]: 依赖 testing, time
 * [OUTPUT]: 对外提供 Cookie 校验与启动预热的账号间节流自动摊平回归测试
 * [POS]: internal/server 的节流摊平策略单元测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"strings"
	"testing"
	"time"

	"icloud-hme/internal/store"
)

// 流水清理:未配置保留期时不得启动，配置后按保留期删除。
func TestLeasePrunerRespectsRetention(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer st.Close()

	oldTS := time.Now().Add(-400 * 24 * time.Hour).Format(time.RFC3339)
	recentTS := time.Now().Add(-time.Hour).Format(time.RFC3339)
	for _, rec := range []store.LeaseRecord{
		{ID: "old1", Email: "old1@icloud.com", AccountID: "acc_1", AllocatedAt: oldTS},
		{ID: "new1", Email: "new1@icloud.com", AccountID: "acc_1", AllocatedAt: recentTS},
	} {
		if err := st.RecordLease(rec); err != nil {
			t.Fatalf("RecordLease: %v", err)
		}
	}

	// 未配置保留期: 不启用、不删除
	disabled := NewLeasePruner(st, 0)
	if disabled.Enabled() {
		t.Fatal("保留期为 0 时不应启用清理")
	}
	if n := disabled.PruneOnce(); n != 0 {
		t.Fatalf("未启用时不应删除任何数据, 实际 %d", n)
	}
	if got := st.CountLeases(); got != 2 {
		t.Fatalf("未启用时流水应保持 2 条, 实际 %d", got)
	}

	// 配置 180 天保留期: 保留配置且启用，但 PruneOnce 安全暂停(不删除历史流水以防重号)
	p := NewLeasePruner(st, 180*24*time.Hour)
	if !p.Enabled() {
		t.Fatal("配置保留期后应启用")
	}
	if n := p.PruneOnce(); n != 0 {
		t.Fatalf("PR-01 阶段安全暂停物理清理, 删除条数应为 0, 实际 %d", n)
	}
	if got := st.CountLeases(); got != 2 {
		t.Fatalf("安全暂停期间流水应完整保留 2 条, 实际 %d", got)
	}
	// 路由同样必须保留
	if got := st.CountAliasRoutes(); got != 2 {
		t.Fatalf("路由表不得受影响, 期望 2, 实际 %d", got)
	}
	if len(p.Logs()) == 0 || !strings.Contains(p.Logs()[0], "安全暂停") {
		t.Fatal("清理应留下安全暂停的可观测告警日志")
	}
}

// 可观测性端点: 管理员可读且包含关键水位；受限令牌必须被拒。
func TestSystemStatsEndpoint(t *testing.T) {
	ts, _ := newScopeTestServer(t)

	resp, body := doGet(t, ts, "/api/system/stats", "admin-token-bbbb")
	if resp.StatusCode != 200 {
		t.Fatalf("admin 应能读取系统水位: %d %s", resp.StatusCode, body)
	}
	for _, key := range []string{"uptime_seconds", "goroutines", "store", "engines", "alias_routes", "leases", "alias_pool", "available_aliases"} {
		if !strings.Contains(body, key) {
			t.Fatalf("系统水位应包含 %q: %s", key, body)
		}
	}

	// 受限令牌(仅 allocate,verify)不得读取运维信息
	resp, _ = doGet(t, ts, "/api/system/stats", "scoped-token-aaaa")
	if resp.StatusCode != 403 {
		t.Fatalf("受限令牌读取系统水位应 403, 实际 %d", resp.StatusCode)
	}
}

// Cookie 校验的账号间节流必须按账号数摊平，使一轮校验落在监控周期之内。
//
// 回归防线:此前硬编码 2 秒 —— 2000 账号一轮要 66 分钟，远超 30 分钟周期，
// 实际效果是 7×24 不停地向 Apple 发请求。
func TestCookieMonitorPerAccountGap(t *testing.T) {
	const interval = 30 * time.Minute

	cases := []struct {
		accounts int
		explicit time.Duration
		want     time.Duration
		why      string
	}{
		{accounts: 1, want: 0, why: "单账号无需节流"},
		{accounts: 10, want: defaultCookieThrottle, why: "账号少时用上限 2s"},
		{accounts: 2000, want: interval / 2000, why: "账号多时摊平到周期内(900ms)"},
		{accounts: 2000, explicit: 5 * time.Second, want: 5 * time.Second, why: "显式配置优先"},
		{accounts: 2000, explicit: time.Millisecond, want: time.Millisecond, why: "显式配置即使很小也应生效(由配置校验兜底)"},
	}
	for _, tc := range cases {
		m := NewCookieMonitor(nil, interval, nil)
		m.SetThrottle(tc.explicit)
		if got := m.perAccountGap(tc.accounts); got != tc.want {
			t.Fatalf("accounts=%d explicit=%v: 期望 %v (%s), 实际 %v",
				tc.accounts, tc.explicit, tc.want, tc.why, got)
		}
	}

	// 下限保护:周期很短而账号极多时不得退化成无节流
	m := NewCookieMonitor(nil, 5*time.Minute, nil)
	if got := m.perAccountGap(100000); got != minCookieThrottle {
		t.Fatalf("应受下限 %v 保护, 实际 %v", minCookieThrottle, got)
	}
}

// 一轮校验的总时长必须落在监控周期之内(否则会退化成 7×24 连续探测)。
func TestCookieRoundFitsWithinInterval(t *testing.T) {
	const interval = 30 * time.Minute
	const accounts = 2000

	m := NewCookieMonitor(nil, interval, nil)
	gap := m.perAccountGap(accounts)
	roundCost := time.Duration(accounts-1) * gap
	if roundCost > interval {
		t.Fatalf("2000 账号一轮耗时 %v 超过监控周期 %v, 会退化成连续探测", roundCost, interval)
	}
	t.Logf("2000 账号: 账号间隔=%v, 一轮节流总耗时=%v (周期 %v)", gap, roundCost, interval)
}

// 启动预热的账号间间隔必须摊平，避免 2000 账号预热耗时 66 分钟。
func TestStartupSyncGap(t *testing.T) {
	const accounts = 2000

	s := &Server{cfg: Config{}}
	gap := s.startupSyncGap(accounts)
	if gap <= 0 {
		t.Fatalf("多账号时预热间隔应为正, 实际 %v", gap)
	}
	if total := time.Duration(accounts) * gap; total > startupSyncBudget+gap {
		t.Fatalf("2000 账号预热摊平后应约等于预算 %v, 实际 %v", startupSyncBudget, total)
	}
	t.Logf("2000 账号: 预热提交间隔=%v, 摊平总时长≈%v", gap, time.Duration(accounts)*gap)

	if got := s.startupSyncGap(1); got != 0 {
		t.Fatalf("单账号无需等待, 实际 %v", got)
	}
	if got := s.startupSyncGap(10); got != defaultStartupSyncGap {
		t.Fatalf("账号少时应取上限 %v, 实际 %v", defaultStartupSyncGap, got)
	}

	// 显式配置优先
	s2 := &Server{cfg: Config{StartupSyncInterval: 3 * time.Second}}
	if got := s2.startupSyncGap(accounts); got != 3*time.Second {
		t.Fatalf("显式配置应优先, 实际 %v", got)
	}

	// 下限保护
	s3 := &Server{cfg: Config{}}
	if got := s3.startupSyncGap(1000000); got != minStartupSyncGap {
		t.Fatalf("应受下限 %v 保护, 实际 %v", minStartupSyncGap, got)
	}
}
