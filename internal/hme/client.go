/**
 * [INPUT]: 依赖 bogdanfinn/tls-client, tidwall/gjson, google/uuid, fhttp
 * [OUTPUT]: 对外提供 Client, NewClient, AccountInfo 等 HME 传输会话能力
 * [POS]: internal/hme 的底层 HTTP/TLS 传输与会话端点协商层
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

// Package hme 实现了 iCloud Hide My Email 协议客户端。
//
// 基于 Cookie 会话,通过 tls-client 伪装 Chrome TLS 指纹规避 iCloud 风控。
// 对应原 Python 项目 icloud_hme.py 的 ICloudHME 类。
package hme

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const (
	// ClientBuildNumber 是 iCloud Web 客户端构建号,从浏览器抓包获取。
	// maildomainws (HME 别名管理) 专用。
	ClientBuildNumber = "2624Build22"
	// ClientMasteringNumber 是 iCloud Web 客户端主版本号。
	ClientMasteringNumber = "2624Build22"
	// DefaultBuildNumber 用于 validate 和 mccgateway (邮件) 等非 HME 端点。
	DefaultBuildNumber = "2624Build13"
	// RequestTimeout 单次请求超时。
	RequestTimeout = 15 * time.Second
	// MaxRetries 最大重试次数。
	MaxRetries = 3
)

var retryDelays = []time.Duration{
	1 * time.Second,
	2500 * time.Millisecond,
	5 * time.Second,
}

// AccountInfo 是从 /validate 响应中提取的账号身份信息。
type AccountInfo struct {
	DSID             string `json:"dsid"`
	AppleID          string `json:"appleId"`
	PrimaryEmail     string `json:"primaryEmail"`
	FullName         string `json:"fullName"`
	IsManagedAppleID bool   `json:"isManagedAppleId"`
}

// Client 是 iCloud Hide My Email 客户端。
//
// 一个 Client 对应一个 iCloud 账号。通过传入的 Cookie 维持会话,
// 首次调用业务方法时会自动触发 ValidateSession 解析 HME 服务端点。
type Client struct {
	cookieMu sync.RWMutex
	// stateMu 串行化「服务端点解析」并保护 setupURL/serviceURL/dsid/accountInfo。
	//
	// 一个 Client 会被 BatchUpdateAliases 的多个 worker 共享；若不串行化，
	// 并发首次调用会同时触发多次 ValidateSession 并交错写入这三个字段
	// (数据竞争 + 重复的 Apple validate 风控暴露)。
	stateMu     sync.Mutex
	Cookies     map[string]string
	Host        string // "icloud.com" or "icloud.com.cn"
	Proxy       string // HTTP/SOCKS5 代理
	Username    string // iCloud 账号 (用于登录)
	Password    string // iCloud 密码 (用于登录)
	Verbose     bool
	httpc       tls_client.HttpClient
	setupURL    string
	serviceURL  string
	dsid        string // 从 validate 响应提取
	clientID    string // UUID,每次会话生成
	accountInfo *AccountInfo
}

// NewClient 创建一个新的 HME 客户端,底层使用 Chrome TLS 指纹。
//
// proxy 支持格式:
//   - HTTP:  "http://user:pass@host:port"
//   - SOCKS5: "socks5://user:pass@host:port"
func NewClient(cookies map[string]string, host, proxy string, verbose bool) (*Client, error) {
	if host == "" {
		host = "icloud.com"
	}
	if cookies == nil {
		cookies = make(map[string]string)
	}
	jar := tls_client.NewCookieJar()
	options := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(30),
		tls_client.WithClientProfile(profiles.Chrome_146),
		tls_client.WithCookieJar(jar),
		tls_client.WithNotFollowRedirects(),
	}

	// 添加代理支持
	if proxy != "" {
		options = append(options, tls_client.WithProxyUrl(proxy))
	}

	httpc, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
	if err != nil {
		return nil, err
	}

	c := &Client{
		Cookies:  cookies,
		Host:     normalizeHost(host),
		Proxy:    proxy,
		Verbose:  verbose,
		httpc:    httpc,
		clientID: uuid.New().String(),
	}

	// 把传入的 Cookie 灌入 jar,后续请求自动携带。
	if len(cookies) > 0 {
		// 设置 Cookie 到所有可能的域名
		domains := []string{
			"https://www.icloud.com",
			"https://www.icloud.com.cn",
			"https://setup.icloud.com",
			"https://setup.icloud.com.cn",
			"https://" + c.Host,
		}

		// 添加 serviceURL 的域名（如果已知）
		if c.serviceURL != "" {
			if u, err := url.Parse(c.serviceURL); err == nil {
				domains = append(domains, u.Scheme+"://"+u.Host)
			}
		}

		for _, domain := range domains {
			u, _ := url.Parse(domain)
			httpCookies := make([]*http.Cookie, 0, len(cookies))
			for k, v := range cookies {
				httpCookies = append(httpCookies, &http.Cookie{
					Name:  k,
					Value: v,
					Path:  "/",
				})
			}
			jar.SetCookies(u, httpCookies)
		}
	}
	return c, nil
}

func normalizeHost(host string) string {
	h := strings.TrimSpace(strings.ToLower(host))
	if u, err := url.Parse(h); err == nil && u.Hostname() != "" {
		h = u.Hostname()
	} else if !strings.Contains(h, "://") {
		if u, err := url.Parse("https://" + h); err == nil && u.Hostname() != "" {
			h = u.Hostname()
		}
	}
	if strings.HasSuffix(h, ".icloud.com.cn") || h == "icloud.com.cn" {
		return "icloud.com.cn"
	}
	return "icloud.com"
}

// SetupURL 返回 iCloud setup 端点。
func (c *Client) SetupURL() string {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.setupURLLocked()
}

// setupURLLocked 是 SetupURL 的无锁内核，调用方须持有 stateMu（或确认独占）。
func (c *Client) setupURLLocked() string {
	if c.setupURL == "" {
		suffix := "setup.icloud.com"
		if c.Host == "icloud.com.cn" {
			suffix = "setup.icloud.com.cn"
		}
		c.setupURL = "https://" + suffix + "/setup/ws/1"
	}
	return c.setupURL
}

// Origin 返回 Web Origin。
func (c *Client) Origin() string {
	return "https://www." + c.Host
}

func (c *Client) log(format string, args ...any) {
	if c.Verbose {
		fmt.Printf("  [iCloud] %s\n", fmt.Sprintf(format, args...))
	}
}

// isSensitiveHeader 判断请求头是否携带凭据。
func isSensitiveHeader(name string) bool {
	switch strings.ToLower(name) {
	case "cookie", "set-cookie", "authorization", "x-apple-webauth-token", "proxy-authorization":
		return true
	}
	return false
}

// redactSecret 只保留长度信息，绝不回显凭据内容。
func redactSecret(v string) string {
	return fmt.Sprintf("<redacted %d bytes>", len(v))
}

// buildURL 给 URL 追加 clientBuildNumber / clientMasteringNumber / clientId / dsid 查询参数,
// 这是 iCloud Web API 的强制要求。
// buildURL 给 URL 追加 clientBuildNumber / clientMasteringNumber / clientId / dsid 查询参数,
// 这是 iCloud Web API 的强制要求。
//
// 调用约定:dsid/clientID 只在 ValidateSession 内被写入，而 ValidateSession 由 stateMu
// 串行化；并发批处理前必须先 EnsureService()，之后这些字段即为只读，故此处无需再加锁
// (若在此处取 stateMu，会与 validateSessionLocked 内的调用形成自死锁)。
func (c *Client) buildURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := parsed.Query()
	// setup.icloud.com (validate) 和 mccgateway 用 DefaultBuildNumber,maildomainws 用 ClientBuildNumber
	host := parsed.Hostname()
	if strings.Contains(host, "maildomainws") {
		q.Set("clientBuildNumber", ClientBuildNumber)
		q.Set("clientMasteringNumber", ClientMasteringNumber)
	} else {
		q.Set("clientBuildNumber", DefaultBuildNumber)
		q.Set("clientMasteringNumber", DefaultBuildNumber)
	}
	if c.clientID != "" {
		q.Set("clientId", c.clientID)
	}
	if c.dsid != "" {
		q.Set("dsid", c.dsid)
	}
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

// requestOrigin 根据实际请求端点选择 Origin。
// Apple 可能把国区账号路由到全球服务，Origin 必须跟随目标域名而不是账号配置。
func requestOrigin(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err == nil {
		host := strings.ToLower(u.Hostname())
		if host == "icloud.com.cn" || strings.HasSuffix(host, ".icloud.com.cn") {
			return "https://www.icloud.com.cn"
		}
		if host == "icloud.com" || strings.HasSuffix(host, ".icloud.com") {
			return "https://www.icloud.com"
		}
	}
	return "https://www.icloud.com"
}

// request 执行带重试的 HTTP 请求,返回响应体字符串。
func (c *Client) request(method, rawURL string, body any, timeout time.Duration, maxAttempts int) (string, error) {
	return c.RequestWithContext(context.Background(), method, rawURL, body, timeout, maxAttempts)
}

// RequestWithContext 执行带 context 贯穿与重试预算的 HTTP 请求 (PR-05)。
func (c *Client) RequestWithContext(ctx context.Context, method, rawURL string, body any, timeout time.Duration, maxAttempts int) (string, error) {
	if timeout == 0 {
		timeout = RequestTimeout
	}
	if maxAttempts == 0 {
		maxAttempts = MaxRetries
	}
	fullURL := c.buildURL(rawURL)

	hostName := ""
	if u, err := url.Parse(rawURL); err == nil {
		hostName = u.Hostname()
	}
	contentType := "application/json"
	acceptType := "application/json, text/plain, */*"
	if strings.Contains(hostName, "maildomainws") {
		contentType = "text/plain"
		acceptType = "*/*"
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		var reqBody io.Reader
		if body != nil {
			buf, err := json.Marshal(body)
			if err != nil {
				return "", err
			}
			reqBody = bytes.NewReader(buf)
		}

		req, err := http.NewRequestWithContext(ctx, method, fullURL, reqBody)
		if err != nil {
			return "", err
		}
		origin := requestOrigin(rawURL)
		req.Header.Set("Origin", origin)
		req.Header.Set("Referer", origin+"/")
		req.Header.Set("Accept", acceptType)
		req.Header.Set("Accept-Language", "en-US,en;q=0.9,zh-CN;q=0.8,zh;q=0.7")
		req.Header.Set("Connection", "keep-alive")
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Sec-Fetch-Dest", "empty")
		req.Header.Set("Sec-Fetch-Mode", "cors")
		req.Header.Set("Sec-Fetch-Site", "same-site")
		req.Header.Set("sec-ch-ua", `"Google Chrome";v="147", "Not.A/Brand";v="8", "Chromium";v="147"`)
		req.Header.Set("sec-ch-ua-mobile", "?0")
		req.Header.Set("sec-ch-ua-platform", `"Windows"`)
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36")

		// 手动添加 Cookie 头（确保跨域也能传递）
		var cookieHeader string
		c.cookieMu.RLock()
		if len(c.Cookies) > 0 {
			cookieParts := make([]string, 0, len(c.Cookies))
			for k, v := range c.Cookies {
				if strings.HasPrefix(v, `"`) {
					cookieParts = append(cookieParts, k+"="+v)
				} else {
					cookieParts = append(cookieParts, k+`="`+v+`"`)
				}
			}
			cookieHeader = strings.Join(cookieParts, "; ")
		}
		c.cookieMu.RUnlock()

		if cookieHeader != "" {
			req.Header.Set("Cookie", cookieHeader)
			if c.Verbose {
				c.log(">>> URL: %s", fullURL)
				c.log(">>> Cookie: %s", redactSecret(cookieHeader))
				for k, vv := range req.Header {
					for _, v := range vv {
						if isSensitiveHeader(k) {
							c.log(">>> %s: %s", k, redactSecret(v))
							continue
						}
						c.log(">>> %s: %s", k, v[:min(100, len(v))])
					}
				}
			}
		}

		resp, err := c.httpc.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("连接失败: %w", err)
			if attempt < maxAttempts {
				if sleepErr := c.sleepRetry(ctx, attempt); sleepErr != nil {
					return "", sleepErr
				}
				continue
			}
			return "", lastErr
		}

		// 读取上限防御
		text, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
		_ = resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("读取响应失败: %w", err)
			if attempt < maxAttempts {
				if sleepErr := c.sleepRetry(ctx, attempt); sleepErr != nil {
					return "", sleepErr
				}
				continue
			}
			return "", lastErr
		}

		respCookies := resp.Cookies()
		if len(respCookies) > 0 {
			now := time.Now()
			c.cookieMu.Lock()
			if c.Cookies == nil {
				c.Cookies = make(map[string]string)
			}
			for _, sc := range respCookies {
				if sc.MaxAge < 0 || (sc.MaxAge == 0 && !sc.Expires.IsZero() && !sc.Expires.After(now)) {
					delete(c.Cookies, sc.Name)
				} else if sc.Name != "" && sc.Value != "" {
					c.Cookies[sc.Name] = sc.Value
				}
			}
			c.cookieMu.Unlock()
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			snippet := string(text)
			if len(snippet) > 200 {
				snippet = snippet[:200]
			}
			// 401/403 说明 Cookie 失效,不重试直接返回。
			if resp.StatusCode == 401 || resp.StatusCode == 403 {
				return "", fmt.Errorf("%w: HTTP %d: %s", ErrAuthFailed, resp.StatusCode, snippet)
			}
			if resp.StatusCode == 429 {
				lastErr = fmt.Errorf("%w: HTTP 429: %s", ErrRateLimited, snippet)
			} else {
				lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet)
			}
			if attempt < maxAttempts {
				if sleepErr := c.sleepRetry(ctx, attempt); sleepErr != nil {
					return "", sleepErr
				}
				continue
			}
			return "", lastErr
		}

		return string(text), nil
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("未知错误")
}

func (c *Client) sleepRetry(ctx context.Context, attempt int) error {
	idx := attempt - 1
	if idx >= len(retryDelays) {
		idx = len(retryDelays) - 1
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(retryDelays[idx]):
		return nil
	}
}

// validationURLs 返回会话校验端点。
// HME (Hide My Email) 服务端点托管在 Apple 全球基础设施上，
// 优先使用全球端点可避免国区 setup.icloud.com.cn 缺少 premiummailsettings 导致的重试与回退耗时。
//
// 调用方须持有 stateMu（或确认独占），内部走无锁内核避免自死锁。
func (c *Client) validationURLs() []string {
	primary := c.setupURLLocked() + "/validate"
	if c.Host != "icloud.com.cn" {
		return []string{primary}
	}
	global := "https://setup.icloud.com/setup/ws/1/validate"
	if primary == global {
		return []string{primary}
	}
	return []string{primary, global}
}

// ServiceURL 返回当前已解析的 HME 接口端点(线程安全)。
func (c *Client) ServiceURL() string {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.serviceURL
}

// SetServiceURL 设置已记忆的 HME 服务端点，避免重复进行耗时的 ValidateSession 请求。
func (c *Client) SetServiceURL(u string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.serviceURLLocked(u)
}

// serviceURLLocked 归一化并写入服务端点，调用方须持有 stateMu。
func (c *Client) serviceURLLocked(u string) {
	u = strings.TrimRight(u, "/")
	// 剥离 :443 端口——tls-client cookie jar 按无端口 host 存储 cookie,带端口会丢失 cookie → 401
	if strings.HasSuffix(u, ":443") {
		u = strings.TrimSuffix(u, ":443")
	}
	c.serviceURL = u
}

// ResetServiceEndpoint 清空已缓存的端点，强制下次调用重新解析会话(线程安全)。
func (c *Client) ResetServiceEndpoint() {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.serviceURL = ""
	c.setupURL = ""
}

// EnsureService 显式确保服务端点已解析(线程安全)。
//
// 并发批处理(如 BatchUpdateAliases)前必须串行调用一次，使后续并发方法只读取
// 已就绪的端点，而不是同时触发多次 ValidateSession。
func (c *Client) EnsureService() error { return c.resolveService() }

// Close 释放底层客户端的空闲连接。
//
// 供账号级客户端池在条目被淘汰/账号删除时调用；Client 本身不可再用于后续请求。
func (c *Client) Close() {
	if c.httpc != nil {
		c.httpc.CloseIdleConnections()
	}
}

// CookieSnapshot 返回当前 Cookies 的并发安全快照副本。
func (c *Client) CookieSnapshot() map[string]string {
	c.cookieMu.RLock()
	defer c.cookieMu.RUnlock()
	snap := make(map[string]string, len(c.Cookies))
	for k, v := range c.Cookies {
		snap[k] = v
	}
	return snap
}

// ValidateSession 校验 iCloud 会话,解析 HME 服务端点和账号身份。
//
// 必须在调用 ListAliases / Generate / Reserve / Delete 之前完成。
// 失败通常意味着 Cookie 过期或未订阅 iCloud+。
// 线程安全:内部串行化，同一 Client 的并发校验只会真正执行一次。
func (c *Client) ValidateSession() error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.validateSessionLocked()
}

// validateSessionLocked 是 ValidateSession 的无锁内核，调用方必须持有 stateMu。
func (c *Client) validateSessionLocked() error {
	c.log("校验 iCloud 会话...")
	c.cookieMu.RLock()
	cookieLen := len(c.Cookies)
	c.log("使用的 Cookie 数量: %d", cookieLen)
	if cookieLen > 0 {
		for k := range c.Cookies {
			c.log("Cookie: %s", k)
		}
	}
	c.cookieMu.RUnlock()

	var body string
	var err error
	validationURLs := c.validationURLs()
	for i, validationURL := range validationURLs {
		var candidate string
		candidate, err = c.request("POST", validationURL, nil, 15*time.Second, 1)
		if err == nil && !gjson.Valid(candidate) {
			err = fmt.Errorf("invalid JSON response")
		}
		if err == nil && gjson.Get(candidate, "webservices.premiummailsettings.url").String() == "" {
			err = fmt.Errorf("validate 响应缺少 Hide My Email 服务端点")
		}
		if err == nil {
			body = candidate
			break
		}
		if i < len(validationURLs)-1 {
			c.log("首选端点未能解析 HME 服务，尝试备用端点: %v", err)
		}
	}
	if err != nil {
		c.log("校验失败: %v", err)
		return err
	}
	data := gjson.Parse(body)
	serviceURL := data.Get("webservices.premiummailsettings.url").String()
	c.serviceURLLocked(serviceURL)

	// 获取 serviceURL 后，再次设置 Cookie 到该域名
	c.cookieMu.RLock()
	cookieCount := len(c.Cookies)
	var httpCookies []*http.Cookie
	if cookieCount > 0 {
		httpCookies = make([]*http.Cookie, 0, cookieCount)
		for k, v := range c.Cookies {
			httpCookies = append(httpCookies, &http.Cookie{
				Name:  k,
				Value: v,
				Path:  "/",
			})
		}
	}
	c.cookieMu.RUnlock()
	if len(httpCookies) > 0 && c.serviceURL != "" {
		if u, err := url.Parse(c.serviceURL); err == nil && u.Host != "" {
			c.httpc.SetCookies(u, httpCookies)
		}
	}

	dsInfo := data.Get("dsInfo")
	c.dsid = dsInfo.Get("dsid").String()
	info := &AccountInfo{
		DSID:             c.dsid,
		AppleID:          firstNonEmpty(dsInfo.Get("appleId").String(), dsInfo.Get("primaryEmail").String(), dsInfo.Get("appleIdEmail").String()),
		PrimaryEmail:     firstNonEmpty(dsInfo.Get("primaryEmail").String(), dsInfo.Get("appleId").String()),
		FullName:         firstNonEmpty(dsInfo.Get("fullName").String(), dsInfo.Get("name").String()),
		IsManagedAppleID: dsInfo.Get("isManagedAppleId").Bool(),
	}
	if info.AppleID == "" {
		c.cookieMu.RLock()
		for _, name := range []string{"aosappleid", "appleId", "dsid"} {
			if v, ok := c.Cookies[name]; ok && v != "" {
				info.AppleID = v
				break
			}
		}
		c.cookieMu.RUnlock()
	}
	c.accountInfo = info
	c.log("会话有效 → %s", nonEmpty(info.AppleID, "未知账号"))
	return nil
}

// AccountInfo 返回已校验的账号身份(校验前为 nil)。
func (c *Client) AccountInfo() *AccountInfo {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.accountInfo
}

// resolveService 确保服务端点已就绪。
//
// 双重检查 + stateMu 串行化:并发调用中只有第一个真正执行 ValidateSession，
// 其余等待并复用结果，既消除数据竞争也避免重复 validate 触发 Apple 风控。
func (c *Client) resolveService() error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.serviceURL != "" {
		return nil
	}
	return c.validateSessionLocked()
}

// ---- 小工具 ----

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func nonEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
