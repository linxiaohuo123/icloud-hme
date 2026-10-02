/**
 * [INPUT]: 依赖 crypto/sha256, encoding/hex, sort, sync, sync/atomic, time, icloud-hme/internal/hme
 * [OUTPUT]: 对外提供 hmeClientPool, hmeFingerprint、ErrSessionChanged 与 Manager.WithHMEClient
 * [POS]: internal/account 的 HME 客户端按账号复用池，通过复用 transport 的空闲连接跳过每次请求的完整 TLS 握手
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
var (
	ErrHMEClientUnavailable = errors.New("HME 客户端不可用")
	ErrSessionChanged       = errors.New("账号会话已更新，请重试操作")
	ErrHMEPoolBusy          = errors.New("HME 客户端池已满且无可安全驱逐条目")
	ErrHMEOpBusy            = errors.New("上游 HME 操作繁忙，已达并发上限")
)

const (
	// defaultHMEClientIdleTTL 是空闲客户端被回收前的保留时长。
	defaultHMEClientIdleTTL = 5 * time.Minute
	// defaultMaxHMEClients 是同时缓存的客户端上限（LRU 淘汰最久未用且当前空闲者）。
	//
	// 每个缓存的客户端自带一条到 Apple 的连接池，必须设上限，
	// 否则几千账号会把空闲 socket 一直攥在手里。
	defaultMaxHMEClients   = 200
	defaultMaxActiveHMEOps = 16
	hmeClientReapInterval  = time.Minute
)

// hmeClientEntry 是单个账号的客户端缓存条目。
//
// mu 同时承担两件事：保护 client/fingerprint，以及把「同一账号的 HME 操作」串行化。
// 串行化是必要的 —— hme.Client 内部虽有 stateMu 保护端点解析，但 ListAliases 的
// 「置空端点 → 重新校验」自愈流程与其它并发操作交错时仍会读到空端点。
type hmeClientEntry struct {
	mu              sync.Mutex
	pinCount        int // 引用计数，>0 时不可被 LRU 驱逐
	closing         atomic.Bool
	closedDone      chan struct{}
	fingerprint     string
	credentialEpoch uint64
	client          *hme.Client
	needsValidation bool
	lastUsed        atomic.Int64 // UnixNano
}

// hmeClientPool 按账号 ID 缓存并复用 HME 客户端。
type hmeClientPool struct {
	mu             sync.Mutex
	entries        map[string]*hmeClientEntry
	closingEntries map[string]*hmeClientEntry
	max            int
	maxActive      int
	activeOps      int
	idleTTL        time.Duration
	closed         bool
	closeDone      chan struct{} // 支持并发重复 Close 安全等待首次完成
	stopCh         chan struct{}
	stopOnce       sync.Once
}

func newHMEClientPool() *hmeClientPool {
	return newHMEClientPoolWithLimits(defaultMaxHMEClients, defaultMaxActiveHMEOps)
}

func newHMEClientPoolWithLimits(max, maxActive int) *hmeClientPool {
	if max <= 0 {
		max = defaultMaxHMEClients
	}
	if maxActive <= 0 {
		maxActive = defaultMaxActiveHMEOps
	}
	p := &hmeClientPool{
		entries:        make(map[string]*hmeClientEntry),
		closingEntries: make(map[string]*hmeClientEntry),
		max:            max,
		maxActive:      maxActive,
		idleTTL:        defaultHMEClientIdleTTL,
		stopCh:         make(chan struct{}),
	}
	go p.reapLoop()
	return p
}

func (p *hmeClientPool) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// Stats 返回当前缓存条目数与活跃操作数。
func (p *hmeClientPool) Stats() (conns, active int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries), p.activeOps
}

// acquireActiveOp 申请全局活跃 HME 操作槽位。
func (p *hmeClientPool) acquireActiveOp(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, errors.New("HME 客户端池已关闭")
	}

	maxActive := p.maxActive
	if maxActive <= 0 {
		maxActive = defaultMaxActiveHMEOps
	}
	if p.activeOps >= maxActive {
		return nil, ErrHMEOpBusy
	}
	p.activeOps++

	var once sync.Once
	release := func() {
		once.Do(func() {
			p.mu.Lock()
			p.activeOps--
			if p.activeOps < 0 {
				p.activeOps = 0
			}
			p.mu.Unlock()
		})
	}
	return release, nil
}

// acquire 取得（必要时创建）某账号的条目并增加 Pin 保护 (兼容无 Context 调用)。
func (p *hmeClientPool) acquire(id string) (*hmeClientEntry, func(), error) {
	return p.acquireContext(context.Background(), id)
}

// acquireContext 取得（必要时创建）某账号的条目并增加 Pin 保护，等待清理时支持 Context 取消。
func (p *hmeClientPool) acquireContext(ctx context.Context, id string) (*hmeClientEntry, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, nil, errors.New("HME 客户端池已关闭")
		}

		// 若存在同账号旧条目正在退出，必须等待其彻底收尾退出，且等待支持 Context 取消 (S03)
		if old, ok := p.closingEntries[id]; ok {
			done := old.closedDone
			p.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}

		if e, ok := p.entries[id]; ok {
			e.pinCount++
			p.mu.Unlock()
			return e, p.makeUnpin(e), nil
		}
		// 必须在插入之前腾出位置:否则新建条目的 lastUsed 还是零值，
		// 会被紧跟其后的淘汰逻辑判定为"最久未用"而立刻踢掉
		if len(p.entries) >= p.max {
			p.evictLocked(id)
		}
		// 若驱逐后仍达上限，说明全量在用，严格拒绝无界扩容
		if len(p.entries) >= p.max {
			p.mu.Unlock()
			return nil, nil, ErrHMEPoolBusy
		}
		e := &hmeClientEntry{
			pinCount:   1,
			closedDone: make(chan struct{}),
		}
		e.lastUsed.Store(time.Now().UnixNano())
		p.entries[id] = e
		p.mu.Unlock()
		return e, p.makeUnpin(e), nil
	}
}

func (p *hmeClientPool) makeUnpin(e *hmeClientEntry) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			e.pinCount--
			if e.pinCount < 0 {
				e.pinCount = 0
			}
			p.mu.Unlock()
		})
	}
}

// drop 丢弃某账号的缓存（账号被删除或凭据整体失效时调用）。
func (p *hmeClientPool) drop(id string) {
	p.mu.Lock()
	e, ok := p.entries[id]
	if !ok {
		p.mu.Unlock()
		return
	}
	e.closing.Store(true)
	delete(p.entries, id)
	p.closingEntries[id] = e
	p.mu.Unlock()

	e.mu.Lock()
	if e.client != nil {
		e.client.Close()
		e.client = nil
	}
	select {
	case <-e.closedDone:
	default:
		close(e.closedDone)
	}
	e.mu.Unlock()

	p.mu.Lock()
	delete(p.closingEntries, id)
	p.mu.Unlock()
}

// Close 关闭全部缓存客户端并停止回收协程（幂等且并发安全等待首次完成）。
func (p *hmeClientPool) Close() {
	p.stopOnce.Do(func() { close(p.stopCh) })

	p.mu.Lock()
	if p.closed {
		done := p.closeDone
		p.mu.Unlock()
		if done != nil {
			<-done
		}
		return
	}
	p.closed = true
	p.closeDone = make(chan struct{})

	allEntries := make([]*hmeClientEntry, 0, len(p.entries)+len(p.closingEntries))
	for _, e := range p.entries {
		e.closing.Store(true)
		allEntries = append(allEntries, e)
	}
	for _, e := range p.closingEntries {
		e.closing.Store(true)
		allEntries = append(allEntries, e)
	}
	p.entries = make(map[string]*hmeClientEntry)
	p.closingEntries = make(map[string]*hmeClientEntry)
	p.mu.Unlock()

	for _, e := range allEntries {
		e.mu.Lock()
		if e.client != nil {
			e.client.Close()
			e.client = nil
		}
		select {
		case <-e.closedDone:
		default:
			close(e.closedDone)
		}
		e.mu.Unlock()
	}

	close(p.closeDone)
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
		// 只回收当前未被 Pin 且空闲的条目，绝不动正在执行操作的连接
		if e.pinCount == 0 && e.mu.TryLock() {
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
// 且只淘汰 pinCount == 0 且 tryLock 成功的空闲条目 —— 正在执行操作的客户端绝不能被回收。
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
		if c.e.pinCount == 0 && c.e.mu.TryLock() {
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
	if acc.Session != nil {
		raw, _ := json.Marshal(acc.Session)
		h.Write(raw)
	}
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
func (m *Manager) WithHMEClient(id string, fn func(*hme.Client) error) error {
	return m.WithHMEClientContext(context.Background(), id, fn)
}

// WithHMEClientContext 借出账号级 HME 客户端执行 fn，借锁阶段响应 Context 取消 (PR-05 F10)。
// 调用返回后客户端留在池中复用。
func (m *Manager) WithHMEClientContext(ctx context.Context, id string, fn func(*hme.Client) error) error {
	return m.WithHMEClientContextSession(ctx, id, func(client *hme.Client, _ uint64) error {
		return fn(client)
	})
}

// HMEPoolStats 返回当前 HME 客户端池的缓存条目数与活跃操作数。
func (m *Manager) HMEPoolStats() (conns, active int) {
	if m.hmePool == nil {
		return 0, 0
	}
	return m.hmePool.Stats()
}

// WithHMEClientContextSession 将借出时的凭据代际交给需要提交本地状态的调用方。
func (m *Manager) WithHMEClientContextSession(ctx context.Context, id string, fn func(*hme.Client, uint64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// 1. 从池中借出条目并增加 Pin 保护（安全借用/Pin，排队等待时不占用全局网络操作配额，S02）
	entry, unpin, err := m.hmePool.acquireContext(ctx, id)
	if err != nil {
		return err
	}
	defer unpin()

	// 2. 单账号可取消串行锁等待
	if !entry.mu.TryLock() {
		ticker := time.NewTicker(5 * time.Millisecond)
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

	// 3. 拿到单账号锁后，检查池状态和条目 closing 状态（S03: 严防 Close 后重建客户端，T07: 原子同步保护）
	if m.hmePool.isClosed() || entry.closing.Load() {
		return errors.New("HME 客户端池已关闭")
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// 4. 拿到单账号锁后，申请全局活跃操作槽位（网络操作准入，S02）
	releaseOp, err := m.hmePool.acquireActiveOp(ctx)
	if err != nil {
		return err
	}
	defer releaseOp()

	if err := ctx.Err(); err != nil {
		return err
	}
	entry.lastUsed.Store(time.Now().UnixNano())

	snap, err := m.accountSnapshot(id)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrHMEClientUnavailable, err)
	}
	if len(snap.Cookies) == 0 && snap.Session == nil {
		return fmt.Errorf("%w: 账号未配置 Cookie，无法使用 HME 功能", ErrHMEClientUnavailable)
	}

	fp := hmeFingerprint(snap)
	if entry.client == nil || entry.fingerprint != fp || entry.credentialEpoch != snap.credentialEpoch {
		if entry.client != nil {
			entry.client.Close()
			entry.client = nil
		}
		client, cerr := hme.NewClientWithSession(snap.Cookies, snap.Session, snap.Host, snap.Proxy, false)
		if cerr != nil {
			entry.fingerprint = ""
			return fmt.Errorf("%w: %v", ErrHMEClientUnavailable, cerr)
		}
		if snap.ServiceURL != "" {
			client.SetServiceURL(snap.ServiceURL)
		}
		entry.client = client
		entry.fingerprint = fp
		entry.credentialEpoch = snap.credentialEpoch
	}
	// Commit verified authentication separately from business success. A later
	// failed response must not erase this checkpoint or resurrect older tokens.
	client := entry.client
	client.SessionValidated = func(session *hme.BrowserSession, info *hme.AccountInfo, serviceURL string) error {
		if err := verifyAppleIdentity(snap, info); err != nil {
			return err
		}
		cookies := session.CookieMap()
		saved, err := m.saveSessionIfCurrent(id, snap.credentialEpoch, snap.Host, snap.Proxy, cookies, serviceURL, false, session)
		if err != nil {
			return err
		}
		if !saved {
			return ErrSessionChanged
		}
		snap.Session, snap.Cookies, snap.ServiceURL = session.Clone(), cookies, serviceURL
		return nil
	}
	defer func() { client.SessionValidated = nil }()
	if entry.needsValidation || entry.client.SessionNeedsValidation() || (snap.Status == "error" && (snap.AppleDSID != "" || snap.LastValidated != "" || snap.AliasTotal > 0)) {
		if err := entry.client.ValidateSessionWithRecovery(ctx); err != nil {
			saveErr := m.saveRecoveryProgress(id, snap, entry.client.SessionSnapshot())
			entry.client.Close()
			entry.client = nil
			return errors.Join(err, saveErr)
		}
		entry.needsValidation = false
		if err := verifyAppleIdentity(snap, entry.client.AccountInfo()); err != nil {
			return err
		}
	}

	runErr := fn(entry.client, snap.credentialEpoch)
	if errors.Is(runErr, hme.ErrAuthFailed) || errors.Is(runErr, hme.ErrOTPRequired) || errors.Is(runErr, hme.ErrSessionIdentity) {
		entry.needsValidation = true
	}
	if runErr != nil {
		saveErr := m.saveRecoveryProgress(id, snap, entry.client.SessionSnapshot())
		if snap.Session != nil {
			entry.client.Close()
			entry.client = nil
		}
		return errors.Join(runErr, saveErr)
	}

	// 仅当业务调用成功时才回写刷新后的会话（防止业务报错时由于上游 Set-Cookie: Max-Age=0 将残缺 Cookie 脏写回库）
	newCookies := entry.client.CookieSnapshot()
	newServiceURL := entry.client.ServiceURL()
	saved, saveErr := m.saveSessionIfCurrent(id, snap.credentialEpoch, snap.Host, snap.Proxy, newCookies, newServiceURL, false, entry.client.SessionSnapshot())
	if saveErr != nil {
		entry.client.Close()
		entry.client = nil
		return saveErr
	}
	if !saved {
		entry.fingerprint = ""
		return ErrSessionChanged
	}
	// 同步条目指纹，避免下次借出时因正常会话刷新被误判为凭据变更而摧毁长连接
	snap.Cookies = newCookies
	snap.Session = entry.client.SessionSnapshot()
	if newServiceURL != "" {
		snap.ServiceURL = newServiceURL
	}
	entry.fingerprint = hmeFingerprint(snap)

	return nil
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
