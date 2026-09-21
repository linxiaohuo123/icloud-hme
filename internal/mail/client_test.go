/**
 * [INPUT]: 依赖 testing, net, net/http, net/url, io, time, bufio, internal/mail
 * [OUTPUT]: 对外提供 TestDialHTTPConnectSuccess, TestDialHTTPConnectAuthFailure, TestResolveFoldersFallback
 * [POS]: internal/mail 的 HTTP CONNECT 代理隧道拨号、连通性与文件夹解析单元测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package mail

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestDialHTTPConnectSuccess(t *testing.T) {
	// 1. 模拟目标 IMAP 裸 TCP 服务
	targetListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动目标服务失败: %v", err)
	}
	defer targetListener.Close()

	targetAddr := targetListener.Addr().String()
	go func() {
		for {
			conn, err := targetListener.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("HELLO_FROM_TARGET\n"))
			_ = conn.Close()
		}
	}()

	// 2. 模拟 HTTP CONNECT 代理服务
	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动代理服务失败: %v", err)
	}
	defer proxyListener.Close()

	proxyAddr := proxyListener.Addr().String()
	go func() {
		for {
			clientConn, err := proxyListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil || req.Method != http.MethodConnect {
					return
				}
				// 校验鉴权头
				if req.Header.Get("Proxy-Authorization") == "" {
					_, _ = c.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\n\r\n"))
					return
				}

				targetConn, err := net.DialTimeout("tcp", req.Host, 2*time.Second)
				if err != nil {
					_, _ = c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
					return
				}
				defer targetConn.Close()

				_, _ = c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

				// 必须等两个方向都收尾再返回，否则 defer 强关会踩中经典 RST 竞态:
				// 只等一个方向就返回时，另一个方向仍阻塞在读上，带未读数据关闭连接
				// 会让内核发 RST，把已经转发给客户端的问候语一并丢掉，
				// 表现为客户端 ReadString 拿到 EOF。真实代理会保持隧道，
				// 所以这里也应当等到客户端主动断开。(-race 下调度变慢，该竞态几乎必现)
				done := make(chan struct{}, 2)
				go func() {
					_, _ = io.Copy(targetConn, c)
					done <- struct{}{}
				}()
				go func() {
					_, _ = io.Copy(c, targetConn)
					done <- struct{}{}
				}()
				<-done
				<-done
			}(clientConn)
		}
	}()

	// 3. 测试通过 HTTP 代理打通隧道
	proxyURL, _ := url.Parse("http://user:pass@" + proxyAddr)
	tunnelConn, err := dialHTTPConnect(proxyURL, targetAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dialHTTPConnect 失败: %v", err)
	}
	defer tunnelConn.Close()

	reader := bufio.NewReader(tunnelConn)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("读取目标响应失败: %v", err)
	}
	if strings.TrimSpace(line) != "HELLO_FROM_TARGET" {
		t.Fatalf("期望目标服务问候语 HELLO_FROM_TARGET, 实际得到: %s", line)
	}
}

func TestDialHTTPConnectAuthFailure(t *testing.T) {
	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动代理服务失败: %v", err)
	}
	defer proxyListener.Close()

	proxyAddr := proxyListener.Addr().String()
	go func() {
		for {
			clientConn, err := proxyListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = http.ReadRequest(bufio.NewReader(c))
				_, _ = c.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\n\r\n"))
			}(clientConn)
		}
	}()

	proxyURL, _ := url.Parse("http://" + proxyAddr)
	_, err = dialHTTPConnect(proxyURL, "127.0.0.1:993", 2*time.Second)
	if err == nil {
		t.Fatal("期望鉴权失败报错，但返回成功")
	}
	if !strings.Contains(err.Error(), "407") {
		t.Fatalf("期望错误信息包含 407, 实际得到: %v", err)
	}
}

func TestResolveFoldersFallback(t *testing.T) {
	c := NewClient("test@icloud.com", "pass")

	// 1. role == "inbox" 或空
	folders, err := c.resolveFolders("")
	if err != nil || len(folders) != 1 || folders[0] != "INBOX" {
		t.Fatalf("空 folder 解析失败: %v, %v", folders, err)
	}

	folders, err = c.resolveFolders("inbox")
	if err != nil || len(folders) != 1 || folders[0] != "INBOX" {
		t.Fatalf("inbox 解析失败: %v, %v", folders, err)
	}

	// 2. role == "all" 在未连接(ListMailboxes 失败)时应安全降级到 INBOX 和 Junk
	folders, err = c.resolveFolders("all")
	if err != nil || len(folders) != 2 || folders[0] != "INBOX" || folders[1] != "Junk" {
		t.Fatalf("all 降级解析失败: %v, %v", folders, err)
	}

	// 3. role == "junk" 在未连接时应安全降级到 Junk
	folders, err = c.resolveFolders("junk")
	if err != nil || len(folders) != 1 || folders[0] != "Junk" {
		t.Fatalf("junk 降级解析失败: %v, %v", folders, err)
	}

	// 4. 自定义文件夹
	folders, err = c.resolveFolders("Archive")
	if err != nil || len(folders) != 1 || folders[0] != "Archive" {
		t.Fatalf("自定义文件夹解析失败: %v, %v", folders, err)
	}
}
