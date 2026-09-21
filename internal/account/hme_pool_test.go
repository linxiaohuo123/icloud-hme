/**
 * [INPUT]: 依赖 testing, errors, fmt, time, icloud-hme/internal/hme, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 HME 客户端按账号复用、凭据变更重建、池容量回收与借出失败语义的回归测试
 * [POS]: internal/account 的 HME 客户端池单元测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package account

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"icloud-hme/internal/hme"
	"icloud-hme/internal/store"
)

// newPoolTestManager 构造一个带 n 个账号(已配置 Cookie)的管理器。
//
// 直接注入账号 map 而不用 AddAccount:后者会立刻联网校验 Cookie，
// 让单元测试依赖真实网络且耗时数秒。
func newPoolTestManager(tb testing.TB, n int) *Manager {
	tb.Helper()
	dir := tb.TempDir()
	st, err := store.NewStore(dir)
	if err != nil {
		tb.Fatalf("NewStore: %v", err)
	}
	tb.Cleanup(func() { _ = st.Close() })

	mgr, err := NewManager(dir, st)
	if err != nil {
		tb.Fatalf("NewManager: %v", err)
	}
	tb.Cleanup(mgr.Close)

	now := time.Now().Format(time.RFC3339)
	mgr.mu.Lock()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("acc_%02d", i)
		mgr.accounts[id] = &Account{
			ID:          id,
			Name:        fmt.Sprintf("母号%02d", i),
			RealEmail:   fmt.Sprintf("user%02d@icloud.com", i),
			ICloudEmail: fmt.Sprintf("user%02d@icloud.com", i),
			Cookies:     map[string]string{"X-APPLE-WEBAUTH-TOKEN": fmt.Sprintf("token-%02d", i)},
			Host:        "icloud.com",
			Status:      "active",
			CreatedAt:   now,
		}
	}
	mgr.mu.Unlock()
	return mgr
}

func firstAccountIDs(m *Manager, n int) []string {
	out := make([]string, 0, n)
	for _, s := range m.ListSummaries() {
		out = append(out, s.ID)
		if len(out) == n {
			break
		}
	}
	return out
}

// 核心断言: 同一账号连续借出必须复用同一个客户端实例。
//
// 回归防线:改造前每次操作都会 hme.NewClient —— 而它内部会构造一个**独立的
// http.Transport**，等于从一个没有任何空闲连接的 transport 出发，
// 于是每个 HME 请求都要走一遍完整 TLS 握手。
//
// 本地 TLS 服务实测(相同请求序列):
//
//	每次新建客户端 → 5 次请求用掉 5 个 TCP 连接
//	复用同一客户端 → 5 次请求只用 1 个 TCP 连接
//
// 所以"同一个 *hme.Client 实例"是连接复用的必要条件，也就是本测试要守住的性质。
// 生产侧还需配合 hme/client.go 中 io.ReadAll 把响应体读干(否则连接不会回池)。
func TestWithHMEClientReusesClientPerAccount(t *testing.T) {
	mgr := newPoolTestManager(t, 2)
	ids := firstAccountIDs(mgr, 2)

	var first, second *hme.Client
	for i := 0; i < 5; i++ {
		var got *hme.Client
		if err := mgr.WithHMEClient(ids[0], func(c *hme.Client) error {
			got = c
			return nil
		}); err != nil {
			t.Fatalf("WithHMEClient: %v", err)
		}
		if i == 0 {
			first = got
		}
		second = got
	}
	if first == nil || first != second {
		t.Fatal("同一账号连续借出应复用同一个客户端实例")
	}

	// 不同账号必须是不同实例(凭据隔离)
	var other *hme.Client
	if err := mgr.WithHMEClient(ids[1], func(c *hme.Client) error {
		other = c
		return nil
	}); err != nil {
		t.Fatalf("WithHMEClient(other): %v", err)
	}
	if other == first {
		t.Fatal("不同账号必须使用各自独立的客户端实例")
	}
}

// 会话刷新 (Set-Cookie / ServiceURL) 回写账号后，池条目指纹应同步更新，
// 下次借出必须继续复用该实例，不得误判为凭据外生变更而销毁长连接。
func TestWithHMEClientReusesClientAcrossSessionUpdates(t *testing.T) {
	mgr := newPoolTestManager(t, 1)
	id := firstAccountIDs(mgr, 1)[0]

	var first, second *hme.Client
	if err := mgr.WithHMEClient(id, func(c *hme.Client) error {
		first = c
		c.SetServiceURL("https://setup.icloud.com/setup/ws/v1")
		return nil
	}); err != nil {
		t.Fatalf("WithHMEClient(1): %v", err)
	}

	if err := mgr.WithHMEClient(id, func(c *hme.Client) error {
		second = c
		return nil
	}); err != nil {
		t.Fatalf("WithHMEClient(2): %v", err)
	}

	if first == nil || first != second {
		t.Fatal("会话刷新后再次借出应继续复用同一个客户端实例，避免长连接失效")
	}
}

// 凭据变更必须重建客户端，否则会一直用旧会话。
func TestWithHMEClientRebuildsOnCredentialChange(t *testing.T) {
	mgr := newPoolTestManager(t, 1)
	id := firstAccountIDs(mgr, 1)[0]

	var before *hme.Client
	if err := mgr.WithHMEClient(id, func(c *hme.Client) error { before = c; return nil }); err != nil {
		t.Fatalf("WithHMEClient: %v", err)
	}

	// 更新 Cookie(指纹变化)
	mgr.mu.Lock()
	mgr.accounts[id].Cookies = map[string]string{"X-APPLE-WEBAUTH-TOKEN": "brand-new-token"}
	mgr.mu.Unlock()

	var after *hme.Client
	if err := mgr.WithHMEClient(id, func(c *hme.Client) error { after = c; return nil }); err != nil {
		t.Fatalf("WithHMEClient(after): %v", err)
	}
	if before == after {
		t.Fatal("凭据变化后必须重建客户端")
	}
}

// 账号不存在 / 未配置 Cookie 时返回可识别的借出失败错误，
// 使调用方能映射为账号类错误而不是上游故障。
func TestWithHMEClientUnavailableSemantics(t *testing.T) {
	mgr := newPoolTestManager(t, 1)
	id := firstAccountIDs(mgr, 1)[0]

	err := mgr.WithHMEClient("acc_does_not_exist", func(*hme.Client) error { return nil })
	if !errors.Is(err, ErrHMEClientUnavailable) {
		t.Fatalf("账号不存在应返回 ErrHMEClientUnavailable, 实际 %v", err)
	}

	// 清空 Cookie
	mgr.mu.Lock()
	mgr.accounts[id].Cookies = map[string]string{}
	mgr.mu.Unlock()
	err = mgr.WithHMEClient(id, func(*hme.Client) error { return nil })
	if !errors.Is(err, ErrHMEClientUnavailable) {
		t.Fatalf("未配置 Cookie 应返回 ErrHMEClientUnavailable, 实际 %v", err)
	}

	// 借出失败时 fn 不得被执行
	ran := false
	_ = mgr.WithHMEClient("acc_missing", func(*hme.Client) error { ran = true; return nil })
	if ran {
		t.Fatal("借出失败时不得执行回调")
	}
}

// 业务回调的错误必须原样上抛（供上层做上游错误分类）。
func TestWithHMEClientPropagatesCallbackError(t *testing.T) {
	mgr := newPoolTestManager(t, 1)
	id := firstAccountIDs(mgr, 1)[0]

	sentinel := errors.New("upstream boom")
	err := mgr.WithHMEClient(id, func(*hme.Client) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("回调错误应原样上抛, 实际 %v", err)
	}
}

// 账号删除必须丢弃其缓存客户端。
func TestRemoveAccountDropsPooledClient(t *testing.T) {
	mgr := newPoolTestManager(t, 1)
	id := firstAccountIDs(mgr, 1)[0]

	var before *hme.Client
	if err := mgr.WithHMEClient(id, func(c *hme.Client) error { before = c; return nil }); err != nil {
		t.Fatalf("WithHMEClient: %v", err)
	}
	if before == nil {
		t.Fatal("首次借出应返回客户端")
	}
	if !mgr.RemoveAccount(id) {
		t.Fatal("RemoveAccount 应成功")
	}
	mgr.hmePool.mu.Lock()
	_, still := mgr.hmePool.entries[id]
	mgr.hmePool.mu.Unlock()
	if still {
		t.Fatal("账号删除后其池条目应被丢弃，否则会残留到空闲回收窗口")
	}
}

// 空闲回收:超过 idleTTL 的客户端必须被释放，避免几千账号把 socket 一直攥着。
func TestHMEPoolReapsIdleClients(t *testing.T) {
	mgr := newPoolTestManager(t, 1)
	id := firstAccountIDs(mgr, 1)[0]
	if err := mgr.WithHMEClient(id, func(*hme.Client) error { return nil }); err != nil {
		t.Fatalf("WithHMEClient: %v", err)
	}
	if len(mgr.hmePool.entries) != 1 {
		t.Fatalf("期望 1 个池条目, 实际 %d", len(mgr.hmePool.entries))
	}

	// 把最后使用时间拨到很久以前，再触发一次回收
	mgr.hmePool.mu.Lock()
	for _, e := range mgr.hmePool.entries {
		e.lastUsed.Store(time.Now().Add(-2 * mgr.hmePool.idleTTL).UnixNano())
	}
	mgr.hmePool.mu.Unlock()
	mgr.hmePool.reapIdle()

	if len(mgr.hmePool.entries) != 0 {
		t.Fatalf("空闲超时的客户端应被回收, 实际残留 %d 个", len(mgr.hmePool.entries))
	}
}

// 池上限:超出后按最久未用淘汰，且绝不动正在使用中的条目。
func TestHMEPoolEvictsBeyondCapacity(t *testing.T) {
	p := newHMEClientPool()
	defer p.Close()
	p.max = 3

	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("acc_%02d", i)
		e := p.acquire(id)
		e.lastUsed.Store(int64(i + 1))
	}
	if len(p.entries) > p.max {
		t.Fatalf("池条目应被淘汰到上限 %d 以内, 实际 %d", p.max, len(p.entries))
	}
	// 最近使用的应保留
	if _, ok := p.entries["acc_09"]; !ok {
		t.Fatal("最近使用的条目不应被淘汰")
	}
}

// Close 必须幂等且清空池。
func TestHMEPoolCloseIsIdempotent(t *testing.T) {
	p := newHMEClientPool()
	_ = p.acquire("acc_1")
	p.Close()
	p.Close()
	if len(p.entries) != 0 {
		t.Fatalf("Close 后池应为空, 实际 %d", len(p.entries))
	}
}

// BenchmarkHMEClientConstructUnpooled 量测"每次操作都新建客户端"的**构造**开销。
//
// 请注意不要据此判断池化的价值:构造只要约 9.5µs，**不是**池化的收益来源。
// 真实收益是复用客户端带来的 transport 空闲连接复用 —— 跳过每次请求的完整 TLS 握手
// (新建连接 = TCP + TLS1.3 ≈ 2 RTT，国内到 Apple 约 300–500ms)，这属于网络开销，
// 本地微基准无法体现。连接复用的实证见 TestWithHMEClientReusesClientPerAccount
// 的注释与 WithHMEClient 的文档。
func BenchmarkHMEClientConstructUnpooled(b *testing.B) {
	cookies := map[string]string{"X-APPLE-WEBAUTH-TOKEN": "bench-token"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c, err := hme.NewClient(cookies, "icloud.com", "", false)
		if err != nil {
			b.Fatalf("NewClient: %v", err)
		}
		c.Close()
	}
}

// BenchmarkHMEClientBorrowPooled 量测池化借出同账号客户端的成本。
//
// 该值包含每次借出后的会话回写(SQLite)，因此衡量的是"借出+回写"的完整成本。
// 与上面那个基准的差值只能说明构造开销已被消除，**不代表池化的全部收益**。
func BenchmarkHMEClientBorrowPooled(b *testing.B) {
	mgr := newPoolTestManager(b, 1)
	id := firstAccountIDs(mgr, 1)[0]
	noop := func(*hme.Client) error { return nil }
	// 预热池
	if err := mgr.WithHMEClient(id, noop); err != nil {
		b.Fatalf("warmup: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := mgr.WithHMEClient(id, noop); err != nil {
			b.Fatalf("WithHMEClient: %v", err)
		}
	}
}
