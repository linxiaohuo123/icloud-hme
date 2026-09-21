/**
 * [INPUT]: 依赖 net, net/url, os, strings
 * [OUTPUT]: 对外提供 validateOutboundPublicURL
 * [POS]: internal/server 的出站地址安全守卫，阻断通知 Webhook 等可配置外呼打向内网/云元数据
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"net"
	"net/url"
	"os"
	"strings"
)

// allowPrivateOutboundEnv 是内网出站的显式逃生开关。
// 默认拒绝内网目标；确有自建内网 Webhook 需求时由运维显式开启。
const allowPrivateOutboundEnv = "ICLOUD_HME_ALLOW_PRIVATE_WEBHOOK"

// validateOutboundPublicURL 校验可配置的出站 URL:
//   - 必须是 http/https
//   - 不允许指向环回 / 私有网段 / 链路本地(含 169.254.169.254 云元数据) / 未指定地址
//
// 这类地址是典型的盲 SSRF 入口:服务端会带着自己的网络位置去请求，
// 攻击者仅凭状态码或耗时差异即可探测内网拓扑、甚至读取云实例元数据。
func validateOutboundPublicURL(raw, label string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errNotify(label + " 不是合法的 URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errNotify(label + " 必须以 http:// 或 https:// 开头")
	}
	host := u.Hostname()
	if host == "" {
		return errNotify(label + " 缺少主机名")
	}
	if os.Getenv(allowPrivateOutboundEnv) == "true" {
		return nil
	}
	if isInternalHost(host) {
		return errNotify(label + " 指向内网/环回地址，已被安全策略拒绝(如需放行请设置 " +
			allowPrivateOutboundEnv + "=true)")
	}
	return nil
}

// isInternalHost 判断主机名是否解析到内网、环回或链路本地地址。
func isInternalHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if host == "" {
		return true
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") ||
		strings.HasSuffix(lower, ".local") || strings.HasSuffix(lower, ".internal") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return isInternalIP(ip)
	}
	// 域名:解析后逐一判断，避免 https://metadata.example.com 指向 169.254.169.254
	ips, err := net.LookupIP(host)
	if err != nil {
		// DNS 解析失败不阻断配置(可能是运维尚未配置内网 DNS)，出站时会自然失败
		return false
	}
	for _, ip := range ips {
		if isInternalIP(ip) {
			return true
		}
	}
	return false
}

// isInternalIP 判断 IP 是否属于不可外呼的内部地址段。
func isInternalIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || // 含 IPv4 169.254.0.0/16(云元数据 169.254.169.254)
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast() ||
		ip.IsInterfaceLocalMulticast()
}
