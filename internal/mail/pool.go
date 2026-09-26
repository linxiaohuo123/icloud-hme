/**
 * [INPUT]: 依赖 sync, time, fmt, strings
 * [OUTPUT]: 对外提供 Pool, NewPool 等按账号复用的 IMAP 长连接池管理能力
 * [POS]: internal/mail 的连接复用与生命周期管控层，接入 MailPerf 观测 (pool_wait/ensure/connect/ping/op 耗时)
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

// IMAP 连接池: 按 Apple ID 复用长连接, 避免每次读信都 TLS+Login。
package mail

import (
	"container/list"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// DefaultMaxConns 默认最大同时持有的 IMAP 长连接数。
const DefaultMaxConns = 50

// pooledConnHealthCheckInterval 最近活跃连接免 Ping 快速复用窗口 (PR-MAIL-05)。
// 在此窗口期内最近使用过的长连接直接复用，消除多余的 NOOP 网络 RTT。
const pooledConnHealthCheckInterval = 30 * time.Second

// Pool 管理按账号复用的 IMAP 长连接。同一账号串行使用(go-imap 非并发安全)。
// 支持 LRU 驱逐与后台空闲连接主动回收，杜绝千号场景下的 socket 泄漏。
type Pool struct {
	mu        sync.Mutex
	items     map[string]*list.Element
	lruList   *list.List
	maxConns  int
	idleClose time.Duration
	stopCh    chan struct{}
	closed    bool
}

type pooledConn struct {
	sem         chan struct{} // 单账号并发信号量 (cap=1)，支持真正的无泄露 Context 超时
	appleID     string
	appPassword string
	server      string
	port        int
	proxyURL    string
	client      *Client
	lastUsed    time.Time
}

func (pc *pooledConn) lock() {
	pc.sem <- struct{}{}
}

func (pc *pooledConn) unlock() {
	select {
	case <-pc.sem:
	default:
	}
}

func (pc *pooledConn) tryLock() bool {
	select {
	case pc.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

// NewPool 创建连接池。
func NewPool() *Pool {
	p := &Pool{
		items:     make(map[string]*list.Element),
		lruList:   list.New(),
		maxConns:  DefaultMaxConns,
		idleClose: 10 * time.Minute,
		stopCh:    make(chan struct{}),
	}
	go p.reaperLoop()
	return p
}

// SetMaxConns 设置连接池上限。
func (p *Pool) SetMaxConns(max int) {
	if max <= 0 {
		max = DefaultMaxConns
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.maxConns = max
	p.evictOldestLocked()
}

// DoContext 借出已连接的 Client 执行 fn，支持真实 Context 超时与取消 (Issue 13)。
func (p *Pool) DoContext(ctx context.Context, appleID, appPassword, proxyURL string, fn func(*Client) error) error {
	return p.DoContextWithServer(ctx, appleID, appPassword, IMAPServer, IMAPPort, proxyURL, fn)
}

// DoContextWithServer 借出指定服务器与端口的已连接 Client 执行 fn，支持真实 Context 超时与取消。
func (p *Pool) DoContextWithServer(ctx context.Context, email, password, server string, port int, proxyURL string, fn func(*Client) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if email == "" || password == "" {
		return fmt.Errorf("IMAP 凭据为空")
	}
	server = strings.TrimSpace(server)
	if server == "" {
		server = IMAPServer
	}
	if port <= 0 {
		port = IMAPPort
	}
	pc := p.getOrCreateWithServer(email, server, port)
	if pc == nil {
		return fmt.Errorf("连接池已关闭")
	}

	poolWaitStart := time.Now()
	select {
	case pc.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	poolWaitMS := time.Since(poolWaitStart).Milliseconds()

	// perfRecord 只收集纯数据；最终 defer 在释放 pc.sem 之后才输出日志 (FIX-1)。
	// 标准库 log.Print 是同步输出，若在持有单账号 IMAP slot 期间执行会人为延长 slot 占用并污染 pool_wait_ms 观测。
	// 本 defer 注册最早 → LIFO 最后执行，天然保证时序:
	//   连接状态收尾 (函数体内) → deadline 清理 → stopWatch 关闭 → pc.unlock → LogMailPerf
	var perf *poolPerfRecord
	defer func() {
		pc.unlock()
		if perf != nil {
			logPoolPerfRecord(perf)
		}
	}()

	// 密码、代理或目标服务器变更则换新 (仅在单账号自身锁 pc.mu 内执行, 杜绝占死全局池锁 p.mu)
	if pc.appPassword != password || pc.proxyURL != proxyURL || pc.server != server || pc.port != port {
		if pc.client != nil {
			pc.client.forceClose()
			pc.client = nil
		}
		pc.appleID = email
		pc.appPassword = password
		pc.server = server
		pc.port = port
		pc.proxyURL = proxyURL
	}

	perf = &poolPerfRecord{
		account:    MaskEmailForLog(email),
		server:     server,
		poolWaitMS: poolWaitMS,
	}
	ensureStart := time.Now()
	ensureStats, ensureErr := pc.ensure(p.idleClose)
	perf.ensureMS = time.Since(ensureStart).Milliseconds()
	perf.stats = ensureStats
	if ensureErr != nil {
		perf.ensureErr = true
		perf.err = true
		// FIX-9: 按错误类型判别连接类失败 (超时/reset/refused 等)；
		// 认证失败等业务错误保持 conn_err=false，严禁把所有 IMAP 错误都算作连接错误
		perf.connErr = isLikelyConnErr(ensureErr)
		return ensureErr
	}

	cli := pc.client
	if cli == nil {
		perf.err = true
		return fmt.Errorf("IMAP 客户端未就绪")
	}

	// 监听 Context 取消：真正打断底层 TCP/TLS 连接网络 I/O
	stopWatch := make(chan struct{})
	defer close(stopWatch)

	go func() {
		defer func() {
			_ = recover()
		}()
		select {
		case <-ctx.Done():
			cli.SetDeadline(time.Now())
			cli.forceClose()
		case <-stopWatch:
		}
	}()

	cli.SetDeadline(time.Now().Add(IMAPCommandTimeout))
	defer func() {
		cli.SetDeadline(time.Time{})
	}()

	opStart := time.Now()
	err := fn(cli)
	perf.opMS = time.Since(opStart).Milliseconds()
	pc.lastUsed = time.Now()

	// 若在执行期间 context 已触发取消，连接已被打断，必须从连接池丢弃，严禁复用 (Issue 13)
	if ctxErr := ctx.Err(); ctxErr != nil {
		cli.forceClose()
		pc.client = nil
		perf.err = true
		return ctxErr
	}

	if err != nil && isLikelyConnErr(err) {
		// 连接坏了, 丢掉, 下次重建
		cli.forceClose()
		pc.client = nil
	}
	perf.err = err != nil
	perf.connErr = isLikelyConnErr(err)
	return err
}

// Do 借出已连接的 Client 执行 fn; 用完不 Logout, 连接留在池中。
func (p *Pool) Do(appleID, appPassword, proxyURL string, fn func(*Client) error) error {
	return p.DoContext(context.Background(), appleID, appPassword, proxyURL, fn)
}

// Close 关闭池内全部连接并停止后台回收协程。
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.stopCh)

	for k, elem := range p.items {
		pc := elem.Value.(*pooledConn)
		pc.lock()
		if pc.client != nil {
			pc.client.Disconnect()
			pc.client = nil
		}
		pc.unlock()
		delete(p.items, k)
	}
	p.lruList.Init()
	p.mu.Unlock()
}

func poolKey(email, server string, port int) string {
	lowerEmail := strings.ToLower(strings.TrimSpace(email))
	if (server == "" || strings.EqualFold(server, IMAPServer)) && (port <= 0 || port == IMAPPort) {
		return lowerEmail
	}
	return fmt.Sprintf("%s|%s:%d", lowerEmail, strings.ToLower(strings.TrimSpace(server)), port)
}

func (p *Pool) getOrCreate(appleID string) *pooledConn {
	return p.getOrCreateWithServer(appleID, IMAPServer, IMAPPort)
}

func (p *Pool) getOrCreateWithServer(email, server string, port int) *pooledConn {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	server = strings.TrimSpace(server)
	if server == "" {
		server = IMAPServer
	}
	if port <= 0 {
		port = IMAPPort
	}
	key := poolKey(email, server, port)
	if elem, ok := p.items[key]; ok {
		p.lruList.MoveToFront(elem)
		return elem.Value.(*pooledConn)
	}

	// 超出容量上限时，驱逐最久未使用的空闲连接
	p.evictOldestLocked()

	pc := &pooledConn{
		appleID:  email,
		server:   server,
		port:     port,
		sem:      make(chan struct{}, 1),
	}
	elem := p.lruList.PushFront(pc)
	p.items[key] = elem
	return pc
}

// evictOldestLocked 淘汰超出 maxConns 的旧连接。调用方必须持有 p.mu。
func (p *Pool) evictOldestLocked() {
	elem := p.lruList.Back()
	for p.lruList.Len() >= p.maxConns && elem != nil {
		prev := elem.Prev()
		pc := elem.Value.(*pooledConn)
		// 仅淘汰当前空闲且非使用中的连接，杜绝将正在并发执行操作的活跃连接析构或漏泄
		if pc.tryLock() {
			if pc.client != nil {
				pc.client.forceClose()
				pc.client = nil
			}
			pc.unlock()
			p.lruList.Remove(elem)
			key := poolKey(pc.appleID, pc.server, pc.port)
			delete(p.items, key)
		}
		elem = prev
	}
}

// reaperLoop 定时扫描空闲过久的连接并断开，防止无期限占用系统句柄。
func (p *Pool) reaperLoop() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.reapIdleConns()
		}
	}
}

func (p *Pool) reapIdleConns() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	now := time.Now()
	for _, elem := range p.items {
		pc := elem.Value.(*pooledConn)
		if pc.tryLock() {
			if pc.client != nil && p.idleClose > 0 && !pc.lastUsed.IsZero() && now.Sub(pc.lastUsed) > p.idleClose {
				pc.client.forceClose()
				pc.client = nil
			}
			pc.unlock()
		}
	}
	p.mu.Unlock()
}

// poolEnsureStats 记录单次 ensure 的连接复用与耗时情况，供 MailPerf 观测 (PR-MAIL-00 / FIX-2)。
// ConnectMS 恒为"从首次 Connect 开始到最终连接成功/失败"的总耗时 (含 proxy→direct fallback 全程)，
// 保证任何路径下 conn_ms 都反映真实的建连成本，不低估。
type poolEnsureStats struct {
	Reused          bool  // 复用既有连接 (Ping 通过)
	PingMS          int64 // 复用路径 NOOP 耗时
	ConnectMS       int64 // 首次 Connect 起点到最终结果的总耗时 (含 fallback)
	ProxyFallback   bool  // 是否发生 proxy 连接失败 → direct 直连降级
	ProxyConnectMS  int64 // 走 proxy 的尝试耗时 (未走 proxy 时为 0)
	DirectConnectMS int64 // 直连尝试耗时 (无 proxy 或 fallback direct 时)
}

// 四种建连路径的指标语义 (FIX-2):
//   无 proxy 直连成功:            ProxyFallback=false, ProxyConnectMS=0,      DirectConnectMS=T,   ConnectMS=T
//   proxy 成功:                   ProxyFallback=false, ProxyConnectMS=T,      DirectConnectMS=0,   ConnectMS=T
//   proxy 失败 → direct 成功:      ProxyFallback=true,  ProxyConnectMS=T1,     DirectConnectMS=T2,  ConnectMS=T1+T2
//   proxy 失败 → direct 失败:      ProxyFallback=true,  ProxyConnectMS=T1,     DirectConnectMS=T2,  ConnectMS=T1+T2, err=true

// poolPerfRecord 是 pool_op 的纯数据观测记录 (FIX-1)。
// 在持有 pc.sem 期间只填充字段，释放 semaphore 后由 logPoolPerfRecord 输出，
// 保证同步日志输出不延长单账号 IMAP slot 占用时间。
type poolPerfRecord struct {
	account    string
	server     string
	poolWaitMS int64
	ensureMS   int64
	opMS       int64
	stats      poolEnsureStats
	ensureErr  bool
	err        bool
	connErr    bool
}

func logPoolPerfRecord(r *poolPerfRecord) {
	LogMailPerf("pool_op",
		"account", r.account,
		"server", r.server,
		"pool_wait_ms", r.poolWaitMS,
		"ensure_ms", r.ensureMS,
		"conn_ms", r.stats.ConnectMS,
		"proxy_connect_ms", r.stats.ProxyConnectMS,
		"direct_connect_ms", r.stats.DirectConnectMS,
		"proxy_fallback", r.stats.ProxyFallback,
		"ping_ms", r.stats.PingMS,
		"reused", r.stats.Reused,
		"op_ms", r.opMS,
		"ensure_err", r.ensureErr,
		"err", r.err,
		"conn_err", r.connErr,
	)
}

func (pc *pooledConn) ensure(idleClose time.Duration) (poolEnsureStats, error) {
	var stats poolEnsureStats
	if pc.client != nil {
		// 空闲太久主动重建, 避免服务端静默断连
		if idleClose > 0 && !pc.lastUsed.IsZero() && time.Since(pc.lastUsed) > idleClose {
			pc.client.forceClose()
			pc.client = nil
		}
	}
	if pc.client != nil {
		// 最近活跃连接免 Ping 直接复用，消除每次请求的 NOOP 网络 RTT (PR-MAIL-05)
		if !pc.lastUsed.IsZero() && time.Since(pc.lastUsed) <= pooledConnHealthCheckInterval {
			stats.Reused = true
			return stats, nil
		}

		pingStart := time.Now()
		err := pc.client.Ping()
		stats.PingMS = time.Since(pingStart).Milliseconds()
		if err == nil {
			stats.Reused = true
			return stats, nil
		}
		pc.client.forceClose()
		pc.client = nil
	}
	server := pc.server
	if server == "" {
		server = IMAPServer
	}
	port := pc.port
	if port <= 0 {
		port = IMAPPort
	}
	c := NewClientWithServer(pc.appleID, pc.appPassword, server, port)
	if pc.proxyURL != "" {
		c.SetProxy(pc.proxyURL)
	}
	connectStart := time.Now()
	connectErr := c.Connect()
	if pc.proxyURL != "" {
		stats.ProxyConnectMS = time.Since(connectStart).Milliseconds()
	} else {
		stats.DirectConnectMS = time.Since(connectStart).Milliseconds()
	}
	if connectErr == nil {
		stats.ConnectMS = time.Since(connectStart).Milliseconds()
		pc.client = c
		pc.lastUsed = time.Now()
		return stats, nil
	}
	if pc.proxyURL == "" {
		// FIX-7: 直连失败同样必须记录总建连耗时，禁止 conn_ms=0 + err=true 的错误指标
		stats.ConnectMS = time.Since(connectStart).Milliseconds()
		return stats, connectErr
	}
	// 慢代理超时/坏节点时，自动降级为直连尝试，保障 IMAP 取信不断供 (仅修统计，不改此业务行为)
	stats.ProxyFallback = true
	direct := NewClientWithServer(pc.appleID, pc.appPassword, server, port)
	directStart := time.Now()
	directErr := direct.Connect()
	stats.DirectConnectMS = time.Since(directStart).Milliseconds()
	stats.ConnectMS = time.Since(connectStart).Milliseconds()
	if directErr != nil {
		return stats, connectErr
	}
	pc.client = direct
	pc.lastUsed = time.Now()
	return stats, nil
}

func isLikelyConnErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	// 常见断连/IO/建连失败错误关键字 (FIX-9: 仅显式连接类, 不把认证等业务错误归入 conn_err)
	for _, k := range []string{
		"connection reset", "broken pipe", "eof", "i/o timeout",
		"use of closed", "not connected", "connection refused",
		"actively refused", "connectex", "no such host",
		"network is unreachable", "handshake failure", "握手失败",
		"imap 连接", "wsarecv", "wsasend",
	} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// SetClientForTesting 供测试在无需真实 Apple IMAP 认证时注入已初始化的 Client 验证生产连接池生命周期
func (p *Pool) SetClientForTesting(appleID, appPassword string, cli *Client) {
	pc := p.getOrCreate(appleID)
	pc.appPassword = appPassword
	pc.lastUsed = time.Now()
	pc.client = cli
}

// ClientForTesting 供测试检查连接池中账号关联的 Client 是否已被置空丢弃
func (p *Pool) ClientForTesting(appleID string) *Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	elem, ok := p.items[appleID]
	if !ok || elem == nil {
		return nil
	}
	return elem.Value.(*pooledConn).client
}

// TryLockForTesting 供测试检查单账号并发槽位信号量是否已释放
func (p *Pool) TryLockForTesting(appleID string) bool {
	pc := p.getOrCreate(appleID)
	return pc.tryLock()
}

// UnlockForTesting 供测试释放单账号并发槽位信号量
func (p *Pool) UnlockForTesting(appleID string) {
	pc := p.getOrCreate(appleID)
	pc.unlock()
}

// SetLastUsedForTesting 供测试调整连接最后使用时间
func (p *Pool) SetLastUsedForTesting(appleID string, t time.Time) {
	pc := p.getOrCreate(appleID)
	pc.lastUsed = t
}
