package mail

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/client"
)

func TestPoolGetOrCreateAndClose(t *testing.T) {
	p := NewPool()
	defer p.Close()

	// 1. 验证空凭据报错
	err := p.Do("", "", "", func(c *Client) error {
		return nil
	})
	if err == nil {
		t.Fatalf("空凭据应当报错")
	}

	// 2. 验证并发获取同一账号的连接对象安全且归一
	var wg sync.WaitGroup
	var pcs [10]*pooledConn
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			pcs[idx] = p.getOrCreate("User@iCloud.com")
		}(i)
	}
	wg.Wait()

	target := pcs[0]
	for i := 1; i < 10; i++ {
		if pcs[i] != target {
			t.Fatalf("相同账号获取的 pooledConn 指针不一致")
		}
	}

	// 3. 验证 Close 后清空
	p.Close()
	if len(p.items) != 0 {
		t.Fatalf("Close 后 items 应当为空，实际 len=%d", len(p.items))
	}
}

func TestPoolLRUEviction(t *testing.T) {
	p := NewPool()
	defer p.Close()

	p.SetMaxConns(3)

	p.getOrCreate("user1@icloud.com")
	p.getOrCreate("user2@icloud.com")
	p.getOrCreate("user3@icloud.com")

	if len(p.items) != 3 {
		t.Fatalf("期望 3 个连接，实际 %d", len(p.items))
	}

	// 访问 user1 使其变为最近使用
	p.getOrCreate("user1@icloud.com")

	// 插入第 4 个连接，应当淘汰最久未使用的 user2
	p.getOrCreate("user4@icloud.com")

	if len(p.items) != 3 {
		t.Fatalf("超出上限后期望 3 个连接，实际 %d", len(p.items))
	}

	p.mu.Lock()
	_, hasUser1 := p.items["user1@icloud.com"]
	_, hasUser2 := p.items["user2@icloud.com"]
	_, hasUser3 := p.items["user3@icloud.com"]
	_, hasUser4 := p.items["user4@icloud.com"]
	p.mu.Unlock()

	if !hasUser1 || !hasUser3 || !hasUser4 {
		t.Fatalf("保留的应为 user1, user3, user4")
	}
	if hasUser2 {
		t.Fatalf("user2 应当被 LRU 驱逐")
	}
}

func TestPoolDoContext_CancellationSafe(t *testing.T) {
	p := NewPool()
	defer p.Close()

	pc := p.getOrCreate("cancel_test@icloud.com")
	pc.appPassword = "dummy"
	pc.client = &Client{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预先取消

	err := p.DoContext(ctx, "cancel_test@icloud.com", "dummy", "", func(c *Client) error {
		return nil
	})
	if err == nil {
		t.Fatalf("预先取消的 context 应当返回错误")
	}
}

func TestPoolDoContext_CancellationDuringExecution(t *testing.T) {
	// 1. 启动本地 Mock IMAP 服务端，完全本地闭环，严禁连接真实 Apple 资产与外网
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer ln.Close()

	serverClosedCh := make(chan struct{})
	serverReceivedCmds := make(chan string, 10)

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		// 发送合法 IMAP 欢迎语
		_, _ = conn.Write([]byte("* OK [CAPABILITY IMAP4rev1] Mock IMAP Server Ready\r\n"))

		reader := bufio.NewReader(conn)
		for {
			line, rerr := reader.ReadString('\n')
			if rerr != nil {
				close(serverClosedCh)
				return
			}
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				tag := fields[0]
				cmd := strings.ToUpper(fields[1])
				serverReceivedCmds <- cmd
				if cmd == "NOOP" {
					// 响应第一个 NOOP (来自 ensure -> Ping)
					_, _ = conn.Write([]byte(tag + " OK NOOP completed\r\n"))
					break
				}
			}
		}

		// 保持连接挂起，读取下一条命令后不再应答，使客户端在业务回调中阻塞在实际网络读取上
		for {
			line, rerr := reader.ReadString('\n')
			if rerr != nil {
				close(serverClosedCh)
				return
			}
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				serverReceivedCmds <- strings.ToUpper(fields[1])
			}
			// 挂起故意不回写任何应答
		}
	}()

	clientConn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("dial mock IMAP server failed: %v", err)
	}

	imapCli, err := client.New(clientConn)
	if err != nil {
		t.Fatalf("imap client.New failed: %v", err)
	}

	mockClient := NewClientForTesting("cancel_run@icloud.com", "dummy_pass", clientConn, imapCli)

	p := NewPool()
	defer p.Close()

	p.SetClientForTesting("cancel_run@icloud.com", "dummy_pass", mockClient)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	callbackEntered := make(chan struct{})
	var doErr error
	var cancelDuration time.Duration

	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		doErr = p.DoContext(ctx, "cancel_run@icloud.com", "dummy_pass", "", func(c *Client) error {
			close(callbackEntered)
			// 阻塞在实际网络读取：发送 NOOP 并等待服务端响应 (服务端挂起不应答)
			return c.Ping()
		})
	}()

	// 确认业务回调已经进入
	select {
	case <-callbackEntered:
	case <-time.After(2 * time.Second):
		t.Fatalf("业务回调未能在限时内进入")
	}

	// 确认服务端已收到 ensure NOOP
	select {
	case cmd := <-serverReceivedCmds:
		if cmd != "NOOP" {
			t.Fatalf("服务端收到未知前置命令: %s", cmd)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("服务端未收到 initial ensure NOOP")
	}

	// 确认业务回调内部发起的第二条 NOOP 已到达服务端并阻塞在网络读取
	select {
	case cmd := <-serverReceivedCmds:
		if cmd != "NOOP" {
			t.Fatalf("服务端收到非预期命令: %s", cmd)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("业务回调内部发起的 NOOP 命令未到达服务端")
	}

	// 断言：取消前连接保持打开
	select {
	case <-serverClosedCh:
		t.Fatalf("连接在取消前已被提前关闭")
	default:
	}

	// 由测试取消请求 Context，开始高精度计时
	cancelStart := time.Now()
	cancel()

	select {
	case <-doneCh:
		cancelDuration = time.Since(cancelStart)
		t.Logf("[OPTIMIZED] Context cancel 到读取退出耗时: %v", cancelDuration)
	case <-time.After(2 * time.Second):
		t.Fatalf("DoContext 未能在 Context 取消后及时退出")
	}

	// 断言 1: 取消错误类型必须为 context.Canceled，拒绝任意拨号/认证错误
	if !errors.Is(doErr, context.Canceled) {
		t.Fatalf("期望错误为 context.Canceled，实际得到: %v", doErr)
	}

	// 断言 2: 取消后底层套接字被强制掐断，服务端感知到连接断开
	select {
	case <-serverClosedCh:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("服务端未能在 500ms 内检测到套接字关闭，连接发生泄漏")
	}

	// 断言 3: 取消后已中断的 client 必须从连接池中丢弃
	if cli := p.ClientForTesting("cancel_run@icloud.com"); cli != nil {
		t.Fatalf("取消后 client 应当从连接池中丢弃，但依然存在")
	}

	// 断言 4: 账号槽位信号量已释放，下一次请求可正常执行
	if !p.TryLockForTesting("cancel_run@icloud.com") {
		t.Fatalf("取消后单账号槽位信号量未释放，槽位发生泄漏死锁")
	}
	p.UnlockForTesting("cancel_run@icloud.com")
}
