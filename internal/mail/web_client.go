/**
 * [INPUT]: 依赖 bogdanfinn/tls-client, bogdanfinn/fhttp 进行 TLS 指纹伪造，依赖 Google UUID
 * [OUTPUT]: 对外提供 WebClient、NewWebClient、ListInbox、SearchMails、FindByAlias
 * [POS]: internal/mail 的 Web 邮件读取客户端，当账号未配置 App 专用密码时作为回退通道
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

// Package mail - iCloud Web 邮件客户端
//
// 使用 Cookie 认证通过 iCloud Web API 读取邮件，
// 无需 App Password。基于 mccgateway 服务。
package mail

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/google/uuid"
)

// WebClientBuildNumber 是与浏览器一致的 mccgateway 邮件接口构建号。
const WebClientBuildNumber = "2626Build21"

// WebClient 是 iCloud Web 邮件客户端。
type WebClient struct {
	cookies       map[string]string
	dsid          string
	clientID      string
	mccGatewayURL string
	host          string // "icloud.com" 或 "icloud.com.cn"
	proxy         string
	httpc         tls_client.HttpClient
}

// Proxy 返回客户端当前配置的代理地址。
func (c *WebClient) Proxy() string {
	return c.proxy
}

// NewWebClient 创建一个 Web 邮件客户端，支持独立代理隧道。
//
// 返回 error:代理地址非法(如缺少 scheme / 不支持的协议)时 tls-client 会返回 nil 客户端，
// 若沿用旧签名吞掉错误，后续 c.httpc.Do 会直接 nil 解引用 panic。
func NewWebClient(cookies map[string]string, dsid, host, proxy string) (*WebClient, error) {
	jar := tls_client.NewCookieJar()
	options := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(30),
		tls_client.WithClientProfile(profiles.Chrome_146),
		tls_client.WithCookieJar(jar),
		tls_client.WithNotFollowRedirects(),
	}
	if proxy != "" {
		options = append(options, tls_client.WithProxyUrl(proxy))
	}

	httpc, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
	if err != nil {
		return nil, fmt.Errorf("创建 Web 邮件客户端失败: %w", err)
	}
	if httpc == nil {
		return nil, fmt.Errorf("创建 Web 邮件客户端失败: 客户端为空(通常是代理地址不合法)")
	}

	if host == "" {
		host = "icloud.com"
	}

	c := &WebClient{
		cookies:  cookies,
		dsid:     dsid,
		clientID: uuid.New().String(),
		host:     host,
		proxy:    proxy,
		httpc:    httpc,
	}

	// 设置 Cookie 到基础域名
	if len(cookies) > 0 {
		suffix := "icloud.com"
		if host == "icloud.com.cn" {
			suffix = "icloud.com.cn"
		}
		domains := []string{
			"https://setup." + suffix,
			"https://www." + suffix,
			"https://p217-mccgateway." + suffix,
			"https://p217-maildomainws." + suffix,
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

// maxWebResponseBytes 是 Web 邮件接口响应体读取上限。
//
// 该响应来自 iCloud 网关(且可能经由用户配置的第三方代理)，
// 不加限制地 io.ReadAll 会让不可信上游用超大响应把进程内存打满。
const maxWebResponseBytes = 10 << 20 // 10 MiB

// readAllLimited 读取响应体但设硬上限，超限即报错而不是无限膨胀。
func readAllLimited(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxWebResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxWebResponseBytes {
		return nil, fmt.Errorf("响应体超过 %d 字节上限，已中止读取", maxWebResponseBytes)
	}
	return body, nil
}

func (c *WebClient) updateCookies(resp *http.Response) {
	if resp == nil {
		return
	}
	if c.cookies == nil {
		c.cookies = make(map[string]string)
	}
	for _, cookie := range resp.Cookies() {
		if cookie.MaxAge < 0 {
			delete(c.cookies, cookie.Name)
		} else if cookie.Value != "" {
			c.cookies[cookie.Name] = cookie.Value
		}
	}
}

// origin 返回当前账号对应的 Web Origin。
func (c *WebClient) origin() string {
	return "https://www." + c.host
}

// setCommonHeaders 设置与浏览器一致的通用请求头。
func (c *WebClient) setCommonHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", c.origin())
	req.Header.Set("Referer", c.origin()+"/")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-site")

	// 手动添加 Cookie 头，确保即使跨分区或非标准域名也能稳定携带凭据
	if len(c.cookies) > 0 {
		cookieParts := make([]string, 0, len(c.cookies))
		for k, v := range c.cookies {
			if strings.HasPrefix(v, `"`) {
				cookieParts = append(cookieParts, k+"="+v)
			} else {
				cookieParts = append(cookieParts, k+`="`+v+`"`)
			}
		}
		req.Header.Set("Cookie", strings.Join(cookieParts, "; "))
	}
}

// withParams 给 URL 追加 clientBuildNumber / clientId / dsid 查询参数。
func (c *WebClient) withParams(rawURL string) string {
	sep := "?"
	if strings.Contains(rawURL, "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s%sclientBuildNumber=%s&clientMasteringNumber=%s&clientId=%s&dsid=%s",
		rawURL, sep, WebClientBuildNumber, WebClientBuildNumber, c.clientID, c.dsid)
}

// resolveMccGateway 从 validate 响应中获取 mccgateway URL。
func (c *WebClient) resolveMccGateway() error {
	if c.mccGatewayURL != "" {
		return nil
	}

	setupURL := "https://setup." + c.host + "/setup/ws/1/validate"
	req, err := http.NewRequest("POST", c.withParams(setupURL), nil)
	if err != nil {
		return err
	}
	c.setCommonHeaders(req)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	c.updateCookies(resp)

	body, err := readAllLimited(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("validate 失败: HTTP %d - %s", resp.StatusCode, truncate(string(body), 200))
	}

	var parsed struct {
		Webservices struct {
			Mccgateway struct {
				URL string `json:"url"`
			} `json:"mccgateway"`
		} `json:"webservices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("解析 validate 响应失败: %w", err)
	}

	mccURL := parsed.Webservices.Mccgateway.URL
	if mccURL == "" {
		return fmt.Errorf("未找到 mccgateway URL,响应: %s", truncate(string(body), 200))
	}
	if !strings.HasPrefix(mccURL, "https://") {
		mccURL = "https://" + mccURL
	}
	// 去掉端口号(如 :443)——tls-client 的 cookie jar 按不带端口的 host 存储 Cookie,
	// 带端口的 URL 会导致 Cookie 无法附加,返回 403。
	if u, err := url.Parse(mccURL); err == nil && u.Host != "" {
		u.Host = u.Hostname()
		mccURL = u.String()
	}
	c.mccGatewayURL = strings.TrimRight(mccURL, "/")

	// 动态解析出 mccGatewayURL 后，设置 Cookie 到该 URL
	if u, err := url.Parse(c.mccGatewayURL); err == nil && len(c.cookies) > 0 {
		httpCookies := make([]*http.Cookie, 0, len(c.cookies))
		for k, v := range c.cookies {
			httpCookies = append(httpCookies, &http.Cookie{
				Name:  k,
				Value: v,
				Path:  "/",
			})
		}
		c.httpc.GetCookieJar().SetCookies(u, httpCookies)
	}
	return nil
}

// threadSearchResp 是 thread/search 接口的响应结构。
type threadSearchResp struct {
	TotalThreadsReturned int `json:"totalThreadsReturned"`
	ThreadList           []struct {
		ThreadID     string          `json:"threadId"`
		Subject      string          `json:"subject"`
		Senders      []string        `json:"senders"`
		To           json.RawMessage `json:"to"`
		ToRecipients json.RawMessage `json:"toRecipients"`
		Recipients   json.RawMessage `json:"recipients"`
		Preview      string          `json:"preview"`
		Timestamp    int64           `json:"timestamp"`
	} `json:"threadList"`
}

// search 执行 thread/search 请求,返回解析后的邮件列表。
func (c *WebClient) search(payload string) ([]Message, error) {
	if err := c.resolveMccGateway(); err != nil {
		return nil, err
	}

	searchURL := c.withParams(c.mccGatewayURL + "/mailws2/v1/thread/search")
	req, err := http.NewRequest("POST", searchURL, strings.NewReader(payload))
	if err != nil {
		return nil, err
	}
	c.setCommonHeaders(req)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	c.updateCookies(resp)

	body, err := readAllLimited(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		snippet := string(body)
		if strings.Contains(snippet, "Non iCloud Mail user") || strings.Contains(snippet, "Empty Mail ID") {
			return nil, fmt.Errorf("账号未开通 iCloud 邮件功能 (Non iCloud Mail user)")
		}
		return nil, fmt.Errorf("获取邮件失败: HTTP %d - %s", resp.StatusCode, truncate(snippet, 300))
	}
	if strings.Contains(string(body), `"success":false`) {
		return nil, fmt.Errorf("获取邮件失败: %s", truncate(string(body), 300))
	}

	var result threadSearchResp
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("解析邮件响应失败: %w", err)
	}

	messages := make([]Message, 0, len(result.ThreadList))
	for _, t := range result.ThreadList {
		from := ""
		if len(t.Senders) > 0 {
			from = t.Senders[0]
		}
		to := parseWebRecipients(t.To, t.ToRecipients, t.Recipients)
		date := ""
		if t.Timestamp > 0 {
			date = time.UnixMilli(t.Timestamp).UTC().Format(time.RFC3339)
		}
		messages = append(messages, Message{
			ID:      t.ThreadID,
			From:    from,
			To:      to,
			Subject: t.Subject,
			Preview: sanitizePreview(t.Preview),
			Date:    date,
		})
	}
	return messages, nil
}

func parseWebRecipients(toRaw, toRecipientsRaw, recipientsRaw json.RawMessage) string {
	for _, raw := range []json.RawMessage{toRecipientsRaw, recipientsRaw, toRaw} {
		if len(raw) == 0 {
			continue
		}
		var strList []string
		if json.Unmarshal(raw, &strList) == nil && len(strList) > 0 {
			return strings.Join(strList, ", ")
		}
		var objList []struct {
			Email   string `json:"email"`
			Address string `json:"address"`
		}
		if json.Unmarshal(raw, &objList) == nil && len(objList) > 0 {
			var emails []string
			for _, item := range objList {
				if item.Email != "" {
					emails = append(emails, item.Email)
				} else if item.Address != "" {
					emails = append(emails, item.Address)
				}
			}
			if len(emails) > 0 {
				return strings.Join(emails, ", ")
			}
		}
		var singleStr string
		if json.Unmarshal(raw, &singleStr) == nil && singleStr != "" {
			return singleStr
		}
	}
	return ""
}

// ListInbox 列出收件箱邮件。
func (c *WebClient) ListInbox(limit int) ([]Message, error) {
	payload := fmt.Sprintf(`{"responseType":"THREAD_DIGEST","includeFolderStatus":true,"maxResults":%d,"sessionHeaders":{"folder":"INBOX","modseq":null,"threadmodseq":null,"condstore":1,"qresync":1,"threadmode":1}}`, limit)
	return c.search(payload)
}

// SearchMails 搜索邮件。query 为空时等价于 ListInbox。
func (c *WebClient) SearchMails(query string, limit int) ([]Message, error) {
	if query == "" {
		return c.ListInbox(limit)
	}
	payload := fmt.Sprintf(`{"responseType":"THREAD_DIGEST","includeFolderStatus":false,"maxResults":%d,"query":%q,"sessionHeaders":{"folder":"INBOX","condstore":1,"qresync":1,"threadmode":1}}`, limit, query)
	return c.search(payload)
}

// FindByAlias 查找发给指定别名的邮件——优先使用服务端搜索,并回退本地过滤。
func (c *WebClient) FindByAlias(alias string, limit int) ([]Message, error) {
	// 优先使用服务端检索
	messages, err := c.SearchMails(alias, limit)
	if err == nil && len(messages) > 0 {
		for i := range messages {
			if messages[i].To == "" {
				messages[i].To = alias
			}
		}
		return messages, nil
	}

	// 回退到拉取收件箱并在本地过滤
	batchSize := limit * 2
	if batchSize < 50 {
		batchSize = 50
	}
	raw, err := c.ListInbox(batchSize)
	if err != nil {
		return nil, err
	}

	// 本地过滤: To/CC/BCC 或主题中包含 alias
	filtered := make([]Message, 0, limit)
	for _, m := range raw {
		if strings.Contains(strings.ToLower(m.Subject), strings.ToLower(alias)) ||
			strings.Contains(strings.ToLower(m.From), strings.ToLower(alias)) ||
			strings.Contains(strings.ToLower(m.To), strings.ToLower(alias)) {
			if m.To == "" {
				m.To = alias
			}
			filtered = append(filtered, m)
			if len(filtered) >= limit {
				break
			}
		}
	}
	return filtered, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// 回退到 UTF-8 字符边界，避免把多字节字符截成乱码
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
