/**
 * [INPUT]: 依赖 bufio, context, crypto/tls, encoding/base64, fmt, net, net/http, net/url, strings, time
 * [OUTPUT]: 对外提供 dialHTTPConnect、dialHTTPConnectContext
 * [POS]: internal/mail 的 HTTP CONNECT 隧道拨号器，支持请求取消与正向代理连接远程 IMAP 端口
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package mail

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// dialHTTPConnect 通过 HTTP/HTTPS 代理发起 CONNECT 请求建立到达目标 targetAddr 的裸 TCP 隧道。
func dialHTTPConnect(proxyURL *url.URL, targetAddr string, timeout time.Duration) (net.Conn, error) {
	return dialHTTPConnectContext(context.Background(), proxyURL, targetAddr, timeout)
}

func dialHTTPConnectContext(ctx context.Context, proxyURL *url.URL, targetAddr string, timeout time.Duration) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	proxyAddr := proxyURL.Host
	if !strings.Contains(proxyAddr, ":") {
		if strings.EqualFold(proxyURL.Scheme, "https") {
			proxyAddr = net.JoinHostPort(proxyAddr, "443")
		} else {
			proxyAddr = net.JoinHostPort(proxyAddr, "80")
		}
	}

	dialer := &net.Dialer{Timeout: timeout}
	var conn net.Conn
	var err error

	if strings.EqualFold(proxyURL.Scheme, "https") {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: proxyURL.Hostname()}}).DialContext(ctx, "tcp", proxyAddr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", proxyAddr)
	}
	if err != nil {
		return nil, err
	}
	stopCancel := watchContextClose(ctx, conn)
	defer stopCancel()

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: targetAddr},
		Host:   targetAddr,
		Header: make(http.Header),
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	if proxyURL.User != nil {
		user := proxyURL.User.Username()
		pass, _ := proxyURL.User.Password()
		auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		req.Header.Set("Proxy-Authorization", "Basic "+auth)
	}

	_ = conn.SetDeadline(time.Now().Add(timeout))
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("HTTP CONNECT 握手失败: HTTP %d %s", resp.StatusCode, resp.Status)
	}
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	stopCancel()
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})

	return conn, nil
}
