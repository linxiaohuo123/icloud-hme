/**
 * [INPUT]: 依赖 testing, net, net/http, net/url, io, time, bufio, internal/mail
 * [OUTPUT]: 对外提供 HTTP CONNECT 连通性、鉴权、预读首包保留与文件夹解析测试
 * [POS]: internal/mail 的 HTTP CONNECT 代理隧道回归，真实 TCP 验证合并响应首包完整与双向通信
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package mail

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/utf7"
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

				// Join both directions; propagate upstream EOF through a TCP half-close
				// so buffered tunnel bytes arrive before the client sees EOF.
				done := make(chan struct{}, 2)
				go func() {
					_, _ = io.Copy(targetConn, c)
					done <- struct{}{}
				}()
				go func() {
					_, _ = io.Copy(c, targetConn)
					_ = c.(*net.TCPConn).CloseWrite()
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
	if err := tunnelConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}

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

func TestDialHTTPConnectPreservesBufferedTunnelBytes(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan error, 1)
	finished := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-finished:
		case <-time.After(4 * time.Second):
			t.Error("CONNECT fixture did not stop")
		}
	})
	go func() {
		defer close(finished)
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
			done <- err
			return
		}
		// One write coalesces the HTTP header and the first IMAP tunnel bytes.
		if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n* OK local IMAP ready\r\n"); err != nil {
			done <- err
			return
		}
		var ack [4]byte
		_, err = io.ReadFull(conn, ack[:])
		if err == nil && string(ack[:]) != "PING" {
			err = fmt.Errorf("unexpected tunnel write %q", ack[:])
		}
		done <- err
	}()
	proxyURL, _ := url.Parse("http://" + ln.Addr().String())
	conn, err := dialHTTPConnect(proxyURL, "imap.invalid.example:993", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || line != "* OK local IMAP ready\r\n" {
		t.Fatalf("first tunnel bytes were lost: line=%q err=%v", line, err)
	}
	if _, err := io.WriteString(conn, "PING"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
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

func TestQQMailboxFolderRole(t *testing.T) {
	names := map[string]string{
		"垃圾箱":  "junk",
		"垃圾邮件": "junk",
		"广告邮件": "junk",
		"广告箱":  "junk",
		"已发送":  "sent",
		"草稿箱":  "drafts",
		"已删除":  "trash",
		"废纸篓":  "trash",
	}

	for name, expected := range names {
		encoded, err := utf7.Encoding.NewEncoder().String(name)
		if err != nil {
			t.Fatalf("编码失败: %v", err)
		}
		roleEncoded := folderRole(encoded, nil)
		if roleEncoded != expected {
			t.Fatalf("编码后文件夹 %s (%s) 识别失败: 得到 %s, 期望 %s", encoded, name, roleEncoded, expected)
		}
		roleRaw := folderRole(name, nil)
		if roleRaw != expected {
			t.Fatalf("原始文件夹 %s 识别失败: 得到 %s, 期望 %s", name, roleRaw, expected)
		}
	}
}
