/**
 * [INPUT]: context、net、net/http、共享内网放行环境变量
 * [OUTPUT]: ValidateOutboundPublicURL、NewOutboundHTTPClient、ErrPrivateOutbound
 * [POS]: 通知配置、HTTP 重定向及实际 IP 拨号共用的出站安全边界
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */
package security

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const AllowPrivateOutboundEnv = "ICLOUD_HME_ALLOW_PRIVATE_WEBHOOK"

var ErrPrivateOutbound = errors.New("指向内网/环回地址，已被安全策略拒绝(如需放行请设置 " + AllowPrivateOutboundEnv + "=true)")

func privateOutboundAllowed() bool { return os.Getenv(AllowPrivateOutboundEnv) == "true" }

func validateOutboundURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, errors.New("不是合法的 URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("必须以 http:// 或 https:// 开头")
	}
	if u.Hostname() == "" {
		return nil, errors.New("缺少主机名")
	}
	if !privateOutboundAllowed() && internalHostName(u.Hostname()) {
		return nil, ErrPrivateOutbound
	}
	return u, nil
}

func internalHostName(host string) bool {
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") ||
		strings.HasSuffix(lower, ".local") || strings.HasSuffix(lower, ".internal") {
		return true
	}
	if strings.Contains(host, "%") {
		return true
	}
	return internalIP(net.ParseIP(host))
}

func internalIP(ip net.IP) bool {
	return ip != nil && (!ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast())
}

// ValidateOutboundPublicURL checks saved configuration. Unresolved DNS remains
// configurable; sending must resolve and validate again before connecting.
func ValidateOutboundPublicURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	u, err := validateOutboundURL(raw)
	if err != nil || privateOutboundAllowed() {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
	if err != nil {
		return nil
	}
	for _, ip := range ips {
		if internalIP(ip.IP) {
			return ErrPrivateOutbound
		}
	}
	return nil
}

type outboundDialer struct {
	lookup func(context.Context, string) ([]net.IPAddr, error)
	dial   func(context.Context, string, string) (net.Conn, error)
}

func (d outboundDialer) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if privateOutboundAllowed() {
		return d.dial(ctx, network, address)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if internalHostName(host) {
		return nil, ErrPrivateOutbound
	}
	ips, err := d.lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, errors.New("通知目标未解析到地址")
	}
	for _, ip := range ips {
		if internalIP(ip.IP) || ip.IP == nil || ip.Zone != "" {
			return nil, ErrPrivateOutbound
		}
	}
	// Dial only these validated IPs: never resolve the hostname a second time.
	var dialErr error
	for _, ip := range ips {
		conn, err := d.dial(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		dialErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, dialErr
}

type outboundTransport struct{ transport *http.Transport }

func (t outboundTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if _, err := validateOutboundURL(req.URL.String()); err != nil {
		return nil, err
	}
	return t.transport.RoundTrip(req)
}

func (t outboundTransport) CloseIdleConnections() { t.transport.CloseIdleConnections() }

// NewOutboundHTTPClient checks each request, redirect and connected IP. It uses
// direct connections, retaining the URL hostname for TLS certificate validation.
func NewOutboundHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	d := outboundDialer{lookup: net.DefaultResolver.LookupIPAddr, dial: dialer.DialContext}
	transport := &http.Transport{
		DialContext: d.dialContext, ForceAttemptHTTP2: true,
		MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second, ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Timeout: timeout, Transport: outboundTransport{transport},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("通知重定向次数超过限制")
			}
			_, err := validateOutboundURL(req.URL.String())
			return err
		},
	}
}
