/**
 * [INPUT]: 依赖 crypto/sha256, encoding/hex, sort, sync, sync/atomic, time, icloud-hme/internal/hme
 * [OUTPUT]: 对外提供 hmeClientPool, hmeFingerprint 与 Manager.WithHMEClient
 * [POS]: internal/account 的 HME 客户端按账号复用池，通过复用 transport 的空闲连接跳过每次请求的完整 TLS 握手
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"icloud-hme/internal/hme"
)

// ErrHMEClientUnavailable 表示未能借出 HME 客户端（账号不存在 / 未配置 Cookie / 客户端构造失败）。
//
// 调用方据此把错误映射为「账号类」错误，而不是上游故障，保持与改造前的错误语义一致。
var ErrHMEClientUnavailable = errors.New("HME 客户端不可用")

const (
	// defaultHMEClientIdleTTL 是空闲客户端被回收前的保留时长。
	defaultHMEClientIdleTTL = 5 * time.Minute
	// defaultMaxHMEClients 是同时缓存的客户端上限（LRU 淘汰最久未用且当前空闲者）。
	//
	// 每个缓存的客户端自带一条到 Apple 的连接池，必须设上限，
	// 否则几千账号会把空闲 socket 一直攥在手里。
	defaultMaxHMEClients  = 200
	hmeClientReapInterval = time.Minute
)

// hmeClientEntry 是单个账号的客户端缓存条目。
//
// mu 同时承担两件事：保护 client/fingerprint，以及把「同一账号的 HME 操作」串行化。
// 串行化是必要的 —— hme.Client 内部虽有 stateMu 保护端点解析，但 ListAliases 的
// 「置空端点 → 重新校验」自愈流程与其它并发操作交错时仍会读到空端点。
type hmeClientEntry struct {
	mu          sync.Mutex
	fingerprint string
	client      *hme.Client
	lastUsed    atomic.Int64 // UnixNano
}

// hmeClientPool 按账号 ID 缓存并复用 HME 客户端。
type hmeClientPool struct {
	mu       sync.Mutex
	entries  map[string]*hmeClientEntry
	max      int
	idleTTL  time.Duration
	stopCh   chan struct{}
	stopOnce sync.Once
}

func newHMEClientPool() *hmeClientPool {
	p := &hmeClientPool{
		entries: make(map[string]*hmeClientEntry),
		max:     defaultMaxHMEClients,
		idleTTL: defaultHMEClientIdleTTL,
		stopCh:  make(chan struct{}),
	}
	go p.reapLoop()
	return p
}

// acquire 取得（必要时创建）某账号的条目。注意只取条目，不加条目锁。
func (p *hmeClientPool) acquire(id string) *hmeClientEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[id]; ok {
		return e
	}
	// 必须在插入之前腾出位置:否则新建条目的 lastUsed 还是零值，
	// 会被紧跟其后的淘汰逻辑判定为"最久未用"而立刻踢掉，
	// 结果是池永远长不大、而调用方还握着一个已脱离池的孤儿条目。
	if len(p.entries) >= p.max {
		p.evictLocked(id)
	}
	e := &hmeClientEntry{}
	e.lastUsed.Store(time.Now().UnixNano())
	p.entries[id] = e
	return e
}

// drop 丢弃某账号的缓存（账号被删除或凭据整体失效时调用）。
func (p *hmeClientPool) drop(id string) {
	p.mu.Lock()
	e, ok := p.entries[id]
	if ok {
		delete(p.entries, id)
	}
	p.mu.Unlock()
	if !ok {
		return
	}
	e.mu.Lock()
	if e.client != nil {
		e.client.Close()
		e.client = nil
	}
	e.mu.Unlock()
}

// Close 关闭全部缓存客户端并停止回收协程（幂等）。
func (p *hmeClientPool) Close() {
	p.stopOnce.Do(func() { close(p.stopCh) })

	p.mu.Lock()
	entries := p.entries
	p.entries = make(map[string]*hmeClientEntry)
	p.mu.Unlock()

	for _, e := range entries {
		if e.mu.TryLock() {
			if e.client != nil {
				e.client.Close()
				e.client = nil
			}
			e.mu.Unlock()
		}
	}
}

func (p *hmeClientPool) reapLoop() {
	ticker := time.NewTicker(hmeClientReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.reapIdle()
		}
	}
}

// reapIdle 回收空闲过久的客户端，防止长期占用 socket。
func (p *hmeClientPool) reapIdle() {
	cutoff := time.Now().Add(-p.idleTTL).UnixNano()
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, e := range p.entries {
		if e.lastUsed.Load() > cutoff {
			continue
		}
		// 只回收当前空闲的条目，绝不动正在执行操作的连接
		if e.mu.TryLock() {
			if e.client != nil {
				e.client.Close()
				e.client = nil
				e.fingerprint = ""
			}
			e.mu.Unlock()
			delete(p.entries, id)
		}
	}
}

// evictLocked 按最久未用淘汰空闲条目，为即将插入的新条目腾出位置。调用方须持有 p.mu。
//
// 淘汰目标是让 len 严格小于 max（因为调用方随后还要插入一条），跳过 keepID，
// 且只淘汰 tryLock 成功的空闲条目 —— 正在执行操作的客户端绝不能被回收。
// 若所有条目都在使用中，池会临时超出上限，这是安全的降级
// （宁可多占一点内存，也不能把在用客户端抽走）。
func (p *hmeClientPool) evictLocked(keepID string) {
	if len(p.entries) < p.max {
		return
	}
	type cand struct {
		id string
		e  *hmeClientEntry
	}
	list := make([]cand, 0, len(p.entries))
	for id, e := range p.entries {
		if id == keepID {
			continue
		}
		list = append(list, cand{id, e})
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].e.lastUsed.Load() < list[j].e.lastUsed.Load()
	})
	for _, c := range list {
		if len(p.entries) < p.max {
			return
		}
		if c.e.mu.TryLock() {
			if c.e.client != nil {
				c.e.client.Close()
				c.e.client = nil
			}
			c.e.mu.Unlock()
			delete(p.entries, c.id)
		}
	}
}

// hmeFingerprint 计算账号的凭据指纹（主机 + 代理 + Cookie 全集）。
// 指纹变化说明凭据已更新，必须重建客户端，否则会一直用旧会话。
func hmeFingerprint(acc *Account) string {
	keys := make([]string, 0, len(acc.Cookies))
	for k := range acc.Cookies {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	h.Write([]byte(acc.Host))
	h.Write([]byte{0})
	h.Write([]byte(acc.Proxy))
	h.Write([]byte{0})
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{'='})
		h.Write([]byte(acc.Cookies[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// WithHMEClient 借出账号级 HME 客户端执行 fn，调用返回后客户端留在池中复用。
//
// 【为什么必须复用】hme.NewClient 内部会构造一个**独立的 http.Transport**。
// 每次操作都新建客户端，就等于每次都从一个没有任何空闲连接的 transport 出发，
// 于是**每一个 HME 请求都要走一遍完整 TLS 握手**。实测(本地 TLS 服务)：
//
//	每次新建客户端: 5 次请求 → 5 个 TCP 连接(5 次握手)
//	复用同一客户端: 5 次请求 → 1 个 TCP 连接
//
// 注意别被构造开销误导 —— hme.NewClient 本身只要约 9.5µs，真正的代价是那次跨洋握手
// (新建连接 = TCP 三次握手 + TLS 1.3 握手 ≈ 2 RTT，国内到 Apple 约 300–500ms)。
// 因此这个池省下的是 RTT 量级，不是微秒量级。
//
// 另外，同一账号的调用在此串行执行：hme.Client 的 ListAliases 在失败时会
// 「置空端点 → 重新校验」，若与并发操作交错，对方会读到空端点。
//
// WithHMEClientContext 借出账号级 HME 客户端执行 fn，借锁阶段响应 Context 取消 (PR-05 F10)。
// 调用返回后客户端留在池中复用。
func (m *Manager) WithHMEClientContext(ctx context.Context, id string, fn func(*hme.Client) error) error {
	entry := m.hmePool.acquire(id)

	if !entry.mu.TryLock() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		locked := false
		for !locked {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				if entry.mu.TryLock() {
					locked = true
				}
			}
		}
	}
	defer entry.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	entry.lastUsed.Store(time.Now().UnixNano())

	snap, err := m.accountSnapshot(id)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrHMEClientUnavailable, err)
	}
	if len(snap.Cookies) == 0 {
		return fmt.Errorf("%w: 账号未配置 Cookie，无法使用 HME 功能", ErrHMEClientUnavailable)
	}

	fp := hmeFingerprint(snap)
	if entry.client == nil || entry.fingerprint != fp {
		if entry.client != nil {
			entry.client.Close()
			entry.client = nil
		}
		client, cerr := hme.NewClient(snap.Cookies, snap.Host, snap.Proxy, false)
		if cerr != nil {
			entry.fingerprint = ""
			return fmt.Errorf("%w: %v", ErrHMEClientUnavailable, cerr)
		}
		if snap.ServiceURL != "" {
			client.SetServiceURL(snap.ServiceURL)
		}
		entry.client = client
		entry.fingerprint = fp
	}

	runErr := fn(entry.client)

	// 回写刷新后的会话；仅当业务本身成功时才把回写失败上抛
	newCookies := entry.client.CookieSnapshot()
	newServiceURL := entry.client.ServiceURL()
	if saveErr := m.SaveSession(id, newCookies, newServiceURL); saveErr != nil && runErr == nil {
		return saveErr
	}

	// 同步条目指纹，避免下次借出时因正常会话刷新被误判为凭据变更而摧毁长连接
	snap.Cookies = newCookies
	if newServiceURL != "" {
		snap.ServiceURL = newServiceURL
	}
	entry.fingerprint = hmeFingerprint(snap)

	return runErr
}

// WithHMEClient 借出账号级 HME 客户端执行 fn (兼容保留包装)。
func (m *Manager) WithHMEClient(id string, fn func(*hme.Client) error) error {
	return m.WithHMEClientContext(context.Background(), id, fn)
}

// accountSnapshot 返回账号的深拷贝（含 Cookies）。
func (m *Manager) accountSnapshot(id string) (*Account, error) {
	m.mu.RLock()
	acc, ok := m.accounts[id]
	var snap *Account
	if ok {
		snap = copyAccount(acc)
	}
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("账号不存在: %s", id)
	}
	return snap, nil
}
