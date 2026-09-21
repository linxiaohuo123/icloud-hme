/**
 * [INPUT]: 依赖 testing, context, net, time, io, internal/mail
 * [OUTPUT]: 提供 TestIMAP_TrueCancellationOnContextDone, TestWebMail_TrueCancellationOnContextDone 测试
 * [POS]: internal/mail 的真实网络连接中断与防假取消验证套件 (Issue 13)
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package mail

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestIMAP_TrueCancellationOnContextDone 验证底层 TCP 连接在 Context 取消时被强制掐断 (Issue 13)
func TestIMAP_TrueCancellationOnContextDone(t *testing.T) {
	// 启动一个挂起的本地 Mock TCP 服务端
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer ln.Close()

	serverClosedCh := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		// 挂起服务端，只读不写，等待客户端断开
		buf := make([]byte, 1024)
		for {
			_, rerr := conn.Read(buf)
			if rerr != nil {
				close(serverClosedCh)
				return
			}
		}
	}()

	// 建立到 Mock 服务的原生连接并包入 Client
	conn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}

	cli := &Client{
		conn: conn,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	// 启动监听协程 (与 WithMailClientContext / DoContext 行为一致)
	stopWatch := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			cli.SetDeadline(time.Now())
			cli.ForceClose()
		case <-stopWatch:
		}
	}()

	// 阻塞在网络读取
	readBuf := make([]byte, 10)
	_, readErr := conn.Read(readBuf)
	close(stopWatch)

	duration := time.Since(start)
	if duration > 300*time.Millisecond {
		t.Fatalf("超时未能在合理时间内中断阻塞连接, 耗时: %v", duration)
	}
	if readErr == nil {
		t.Fatalf("期望读操作被中断报错, 实际无错误")
	}

	// 验证服务端确实感知到了连接被掐断
	select {
	case <-serverClosedCh:
		// 服务端感知到连接断开，通过
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("服务端在 500ms 内未收到连接关闭事件，表明存在假取消与连接泄漏")
	}
}

// TestWebMail_TrueCancellationOnContextDone 验证 WebMailClient 在 Context 取消时能真正中断阻塞请求 (Issue 13)
func TestWebMail_TrueCancellationOnContextDone(t *testing.T) {
	// 启动一个挂起的 HTTP 服务
	handlerBlocked := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-handlerBlocked
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"success":true}`)
	}))
	defer func() {
		close(handlerBlocked)
		ts.Close()
	}()

	wmc, err := NewWebClient(nil, "dsid_test", "example.com", "")
	if err != nil {
		t.Fatalf("NewWebClient failed: %v", err)
	}
	wmc.mccGatewayURL = ts.URL

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, searchErr := wmc.ListInboxContext(ctx, 10)
	duration := time.Since(start)

	if duration > 300*time.Millisecond {
		t.Fatalf("WebMail 超时未能在合理时间内中断阻塞连接, 耗时: %v", duration)
	}
	if searchErr == nil {
		t.Fatalf("期望 WebMail 操作被取消报错, 实际无错误")
	}
}
