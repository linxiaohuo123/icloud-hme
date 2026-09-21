/**
 * [INPUT]: 依赖 sync, time, fmt, strings
 * [OUTPUT]: 对外提供 Pool, NewPool 等按账号复用的 IMAP 长连接池管理能力
 * [POS]: internal/mail 的连接复用与生命周期管控层
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

// IMAP 连接池: 按 Apple ID 复用长连接, 避免每次读信都 TLS+Login。
package mail

import (
	"container/list"
	"fmt"
	"strings"
	"sync"
	"time"
)

// DefaultMaxConns 默认最大同时持有的 IMAP 长连接数。
const DefaultMaxConns = 50

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
	mu          sync.Mutex
	appleID     string
	appPassword string
	proxyURL    string
	client      *Client
	lastUsed    time.Time
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

// Do 借出已连接的 Client 执行 fn; 用完不 Logout, 连接留在池中。
func (p *Pool) Do(appleID, appPassword, proxyURL string, fn func(*Client) error) error {
	if appleID == "" || appPassword == "" {
		return fmt.Errorf("IMAP 凭据为空")
	}
	pc := p.getOrCreate(appleID)
	if pc == nil {
		return fmt.Errorf("连接池已关闭")
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()

	// 密码或代理变更则换新 (仅在单账号自身锁 pc.mu 内执行, 杜绝占死全局池锁 p.mu)
	if pc.appPassword != appPassword || pc.proxyURL != proxyURL {
		if pc.client != nil {
			pc.client.forceClose()
			pc.client = nil
		}
		pc.appleID = appleID
		pc.appPassword = appPassword
		pc.proxyURL = proxyURL
	}

	if err := pc.ensure(p.idleClose); err != nil {
		return err
	}
	// IMAP 命令无内建超时: 设置绝对截止时间, 挂起时以 i/o timeout 断开,
	// 池据此丢弃坏连接重建, 避免一次网络黑洞永久占死该账号
	pc.client.SetDeadline(time.Now().Add(IMAPCommandTimeout))
	defer pc.client.SetDeadline(time.Time{})

	err := fn(pc.client)
	pc.lastUsed = time.Now()
	if err != nil && isLikelyConnErr(err) {
		// 连接坏了, 丢掉, 下次重建
		pc.client.forceClose()
		pc.client = nil
	}
	return err
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
		pc.mu.Lock()
		if pc.client != nil {
			pc.client.Disconnect()
			pc.client = nil
		}
		pc.mu.Unlock()
		delete(p.items, k)
	}
	p.lruList.Init()
	p.mu.Unlock()
}

func (p *Pool) getOrCreate(appleID string) *pooledConn {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	key := strings.ToLower(strings.TrimSpace(appleID))
	if elem, ok := p.items[key]; ok {
		p.lruList.MoveToFront(elem)
		return elem.Value.(*pooledConn)
	}

	// 超出容量上限时，驱逐最久未使用的空闲连接
	p.evictOldestLocked()

	pc := &pooledConn{appleID: appleID}
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
		if pc.mu.TryLock() {
			if pc.client != nil {
				pc.client.forceClose()
				pc.client = nil
			}
			pc.mu.Unlock()
			p.lruList.Remove(elem)
			delete(p.items, strings.ToLower(strings.TrimSpace(pc.appleID)))
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
		if pc.mu.TryLock() {
			if pc.client != nil && p.idleClose > 0 && !pc.lastUsed.IsZero() && now.Sub(pc.lastUsed) > p.idleClose {
				pc.client.forceClose()
				pc.client = nil
			}
			pc.mu.Unlock()
		}
	}
	p.mu.Unlock()
}

func (pc *pooledConn) ensure(idleClose time.Duration) error {
	if pc.client != nil {
		// 空闲太久主动重建, 避免服务端静默断连
		if idleClose > 0 && !pc.lastUsed.IsZero() && time.Since(pc.lastUsed) > idleClose {
			pc.client.forceClose()
			pc.client = nil
		}
	}
	if pc.client != nil {
		if err := pc.client.Ping(); err == nil {
			return nil
		}
		pc.client.forceClose()
		pc.client = nil
	}
	c := NewClientWithProxy(pc.appleID, pc.appPassword, pc.proxyURL)
	if err := c.Connect(); err != nil {
		return err
	}
	pc.client = c
	pc.lastUsed = time.Now()
	return nil
}

func isLikelyConnErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	// 常见断连/IO 错误关键字
	for _, k := range []string{
		"connection reset", "broken pipe", "eof", "i/o timeout",
		"use of closed", "not connected", "connection refused",
		"imap 连接", "wsarecv", "wsasend",
	} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}
