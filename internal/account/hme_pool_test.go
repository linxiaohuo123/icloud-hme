/**
 * [INPUT]: 依赖 testing, errors, fmt, time, icloud-hme/internal/hme, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 HME 客户端按账号复用、凭据变更重建、迟到回写拦截、保存失败回滚、池容量回收与借出失败语义的回归测试
 * [POS]: internal/account 的 HME 客户端池单元测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package account

import (
	"context"
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

func TestWithHMEClientRebuildsOnSameCookieReplacement(t *testing.T) {
	mgr := newPoolTestManager(t, 1)
	id := firstAccountIDs(mgr, 1)[0]
	var before, after *hme.Client
	if err := mgr.WithHMEClient(id, func(c *hme.Client) error { before = c; return nil }); err != nil {
		t.Fatal(err)
	}
	acc, _ := mgr.GetAccount(id)
	if err := mgr.SaveSession(id, acc.Cookies, ""); err != nil {
		t.Fatal(err)
	}
	if err := mgr.WithHMEClient(id, func(c *hme.Client) error { after = c; return nil }); err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("显式替换凭据后，即使 Cookie 相同也必须重建客户端")
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

func TestWithHMEClientDoesNotOverwriteNewCredentials(t *testing.T) {
	mgr := newPoolTestManager(t, 1)
	id := firstAccountIDs(mgr, 1)[0]
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- mgr.WithHMEClient(id, func(*hme.Client) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	newCookies := map[string]string{"X-APPLE-WEBAUTH-TOKEN": "new-token"}
	if err := mgr.SaveSession(id, newCookies, ""); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("旧请求应被拒绝回写: %v", err)
	}
	acc, _ := mgr.GetAccount(id)
	if acc.Cookies["X-APPLE-WEBAUTH-TOKEN"] != "new-token" {
		t.Fatalf("新 Cookie 被旧请求覆盖: %+v", acc.Cookies)
	}
	if err := mgr.WithHMEClient(id, func(c *hme.Client) error {
		if c.CookieSnapshot()["X-APPLE-WEBAUTH-TOKEN"] != "new-token" {
			t.Fatal("下次借出必须使用新 Cookie")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWithHMEClientDoesNotWriteAcrossReload(t *testing.T) {
	mgr := newPoolTestManager(t, 1)
	id := firstAccountIDs(mgr, 1)[0]
	mgr.mu.Lock()
	err := mgr.saveAccount(mgr.accounts[id])
	mgr.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- mgr.WithHMEClient(id, func(*hme.Client) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	if err := mgr.Reload(); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("重载前请求不能写入重载后的账号: %v", err)
	}
}

func TestSaveSessionRestoresMemoryOnPersistenceFailure(t *testing.T) {
	mgr := newPoolTestManager(t, 1)
	id := firstAccountIDs(mgr, 1)[0]
	before, _ := mgr.GetAccount(id)
	if err := mgr.Store().Close(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SaveSession(id, map[string]string{"X-APPLE-WEBAUTH-TOKEN": "new-token"}, "https://new.example"); err == nil {
		t.Fatal("数据库关闭后保存应失败")
	}
	after, _ := mgr.GetAccount(id)
	if after.Cookies["X-APPLE-WEBAUTH-TOKEN"] != before.Cookies["X-APPLE-WEBAUTH-TOKEN"] || after.ServiceURL != before.ServiceURL {
		t.Fatalf("保存失败后内存未恢复: before=%+v after=%+v", before, after)
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
		e, unpin, err := p.acquire(id)
		if err != nil {
			t.Fatalf("acquire 失败: %v", err)
		}
		e.lastUsed.Store(int64(i + 1))
		unpin() // 归还 Pin，使其变为可驱逐状态
	}
	if len(p.entries) > p.max {
		t.Fatalf("池条目应被淘汰到上限 %d 以内, 实际 %d", p.max, len(p.entries))
	}
	// 最近使用的应保留
	if _, ok := p.entries["acc_09"]; !ok {
		t.Fatal("最近使用的条目不应被淘汰")
	}
}

// 验证全量 Pin 保护时，达到上限拒绝无界扩容
func TestHMEPoolPinProtectsAndBlocksUnboundedGrowth(t *testing.T) {
	p := newHMEClientPoolWithLimits(2, 5)
	defer p.Close()

	_, unpin1, err := p.acquire("acc_1")
	if err != nil {
		t.Fatalf("借出 acc_1 失败: %v", err)
	}
	defer unpin1()

	_, unpin2, err := p.acquire("acc_2")
	if err != nil {
		t.Fatalf("借出 acc_2 失败: %v", err)
	}
	defer unpin2()

	// 此时两个条目均处于 Pin 状态，无法驱逐
	_, _, err = p.acquire("acc_3")
	if !errors.Is(err, ErrHMEPoolBusy) {
		t.Fatalf("全量 Pin 状态下超出容量上限应返回 ErrHMEPoolBusy，实际为: %v", err)
	}
}

// 验证全局活跃 HME 操作上限与拒绝
func TestHMEPoolActiveOpLimit(t *testing.T) {
	p := newHMEClientPoolWithLimits(10, 2)
	defer p.Close()

	rel1, err := p.acquireActiveOp(context.Background())
	if err != nil {
		t.Fatalf("申请槽位 1 失败: %v", err)
	}
	defer rel1()

	rel2, err := p.acquireActiveOp(context.Background())
	if err != nil {
		t.Fatalf("申请槽位 2 失败: %v", err)
	}
	defer rel2()

	// 达到 maxActive=2，第 3 个申请应当返回 ErrHMEOpBusy
	_, err = p.acquireActiveOp(context.Background())
	if !errors.Is(err, ErrHMEOpBusy) {
		t.Fatalf("超限申请应返回 ErrHMEOpBusy，实际为: %v", err)
	}
}

// Close 必须幂等且清空池。
func TestHMEPoolCloseIsIdempotent(t *testing.T) {
	p := newHMEClientPool()
	_, unpin, _ := p.acquire("acc_1")
	if unpin != nil {
		unpin()
	}
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

// TestWithHMEClientContext_CancelWhileWaitingForBusyAccount 验证同账号借锁竞争时响应 Context 取消 (PR-05 F10)。
// 步骤：
// 1. goroutine A 调用 WithHMEClient 成功拿到 account entry lock 并通过 channel 阻塞；
// 2. 确认 A 已进入后，goroutine B 调用 WithHMEClientContext(ctx, sameAccount) 尝试借出；
// 3. cancel ctx；
// 4. B 必须立即返回 context.Canceled，且 B 的回调函数绝不执行；
// 5. 释放 A，确保无泄漏。
func TestWithHMEClientContext_CancelWhileWaitingForBusyAccount(t *testing.T) {
	mgr := newPoolTestManager(t, 1)
	id := firstAccountIDs(mgr, 1)[0]

	inA := make(chan struct{})
	unblockA := make(chan struct{})
	doneA := make(chan struct{})

	// 1. goroutine A 占住该账号锁
	go func() {
		defer close(doneA)
		_ = mgr.WithHMEClient(id, func(c *hme.Client) error {
			close(inA)
			<-unblockA
			return nil
		})
	}()

	// 2. 等待 A 确定性进入锁保护区
	select {
	case <-inA:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine A failed to enter WithHMEClient in time")
	}

	// 3. goroutine B 尝试借同账号 client，等待期间取消 ctx
	ctx, cancel := context.WithCancel(context.Background())
	bRan := false
	errChB := make(chan error, 1)

	go func() {
		err := mgr.WithHMEClientContext(ctx, id, func(c *hme.Client) error {
			bRan = true
			return nil
		})
		errChB <- err
	}()

	// 稍微休眠让 B 进入 TryLock 失败并 select 等待
	time.Sleep(20 * time.Millisecond)
	cancel() // 触发取消

	// 4. B 必须立即返回 context.Canceled
	select {
	case err := <-errChB:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("goroutine B expected context.Canceled, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine B did not cancel promptly while waiting for busy account")
	}

	// 5. B 的回调绝未执行
	if bRan {
		t.Fatal("goroutine B fn must never execute after context cancellation")
	}

	// 6. 释放 A 并等待其退出
	close(unblockA)
	select {
	case <-doneA:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine A failed to finish cleanly")
	}
}
