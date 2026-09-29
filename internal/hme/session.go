/**
 * [OUTPUT]: 浏览器会话结构、Cookie 作用域及有界认证恢复
 * [POS]: internal/hme 的会话管理层
 */

package hme

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/fhttp/cookiejar"
	"github.com/tidwall/gjson"
	"golang.org/x/net/publicsuffix"
)

// BrowserSession is persisted inside the existing encrypted cookies field.
// Cookies is authoritative; legacy maps are only projections for old readers.
type BrowserSession struct {
	Version         int             `json:"version"`
	Host            string          `json:"host"`
	DSID            string          `json:"dsid"`
	Trusted         bool            `json:"trusted"`
	CapturedAt      int64           `json:"captured_at"`
	Cookies         []SessionCookie `json:"cookies"`
	Auth            SessionAuth     `json:"auth"`
	RecoveryAfter   int64           `json:"recovery_after,omitempty"`
	RecoveryBlocked bool            `json:"recovery_blocked,omitempty"`
}

type SessionAuth struct {
	SessionToken   string `json:"session_token,omitempty"`
	TrustToken     string `json:"trust_token,omitempty"`
	AccountCountry string `json:"account_country,omitempty"`
}

type SessionCookie struct {
	Name         string  `json:"name"`
	Value        string  `json:"value"`
	Domain       string  `json:"domain"`
	Path         string  `json:"path"`
	Expires      float64 `json:"expires"` // -1 means session cookie, never "permanent".
	Secure       bool    `json:"secure"`
	HTTPOnly     bool    `json:"httpOnly"`
	SameSite     string  `json:"sameSite,omitempty"`
	PartitionKey string  `json:"partitionKey,omitempty"`
}

func (s *BrowserSession) Clone() *BrowserSession {
	if s == nil {
		return nil
	}
	cp := *s
	cp.Cookies = append([]SessionCookie(nil), s.Cookies...)
	return &cp
}

func (s *BrowserSession) Validate(host string) error {
	if s == nil {
		return nil
	}
	if s.Version != 1 || s.Host != host || (host != "icloud.com" && host != "icloud.com.cn") || s.DSID == "" || !s.Trusted {
		return fmt.Errorf("invalid or untrusted browser session")
	}
	for _, c := range s.Cookies {
		domain := strings.TrimPrefix(c.Domain, ".")
		if c.Name == "" || c.Path == "" || c.Path[0] != '/' || !appleCookieDomain(domain) || c.PartitionKey != "" {
			return fmt.Errorf("unsupported browser cookie scope")
		}
	}
	return nil
}

func appleCookieDomain(d string) bool {
	return d == "icloud.com" || strings.HasSuffix(d, ".icloud.com") || d == "icloud.com.cn" || strings.HasSuffix(d, ".icloud.com.cn") || d == "idmsa.apple.com" || d == "appleid.apple.com"
}

func (s *BrowserSession) CookieMap() map[string]string {
	if s == nil {
		return nil
	}
	j := newSessionJar(s.Cookies)
	u, _ := url.Parse("https://setup." + s.Host + "/setup/ws/1/validate")
	out := map[string]string{}
	for _, c := range j.Cookies(u) {
		if _, exists := out[c.Name]; !exists {
			out[c.Name] = c.Value
		}
	}
	return out
}

// TokenExpiresAt reports the earliest known expiry among core cookies actually
// eligible for setup requests. Unknown/session lifetimes remain unknown.
func (s *BrowserSession) TokenExpiresAt() *float64 {
	if s == nil {
		return nil
	}
	u, _ := url.Parse("https://setup." + s.Host + "/setup/ws/1/validate")
	var expiry *float64
	for _, cookie := range newSessionJar(s.Cookies).snapshot() {
		if cookie.Name != "X-APPLE-WEBAUTH-TOKEN" || len(newSessionJar([]SessionCookie{cookie}).Cookies(u)) == 0 {
			continue
		}
		if cookie.Expires < 0 {
			return nil
		}
		if expiry == nil || cookie.Expires < *expiry {
			value := cookie.Expires
			expiry = &value
		}
	}
	return expiry
}

// Missing or malformed fields are not evidence of a revoked trust grant.
func checkSessionIdentity(raw, expected string) error {
	if !gjson.Valid(raw) || !gjson.Parse(raw).IsObject() {
		return ErrInvalidResponseSchema
	}
	challenge := gjson.Get(raw, "hsaChallengeRequired")
	trusted := gjson.Get(raw, "hsaTrustedBrowser")
	if challenge.Exists() && challenge.Type != gjson.True && challenge.Type != gjson.False {
		return ErrInvalidResponseSchema
	}
	if challenge.Type == gjson.True {
		return ErrOTPRequired
	}
	if trusted.Type != gjson.True && trusted.Type != gjson.False {
		return ErrInvalidResponseSchema
	}
	if trusted.Type == gjson.False {
		return ErrOTPRequired
	}
	dsid := gjson.Get(raw, "dsInfo.dsid")
	if (dsid.Type != gjson.String && dsid.Type != gjson.Number) || dsid.String() == "" {
		return ErrInvalidResponseSchema
	}
	if dsid.String() != expected {
		return ErrSessionIdentity
	}
	return nil
}

// sessionJar keeps one authoritative record set. A standard jar applies RFC
// domain/path/expiry selection, rather than tls-client's name-only cookie jar.
type sessionJar struct {
	mu      sync.Mutex
	records map[string]SessionCookie
}

func cookieKey(c SessionCookie) string {
	return strings.TrimPrefix(c.Domain, ".") + "\x00" + c.Path + "\x00" + c.Name
}
func newSessionJar(cookies []SessionCookie) *sessionJar {
	j := &sessionJar{records: map[string]SessionCookie{}}
	for _, c := range cookies {
		j.records[cookieKey(c)] = c
	}
	return j
}
func nativeCookie(c SessionCookie) *http.Cookie {
	out := &http.Cookie{Name: c.Name, Value: c.Value, Path: c.Path, Secure: c.Secure, HttpOnly: c.HTTPOnly}
	if strings.HasPrefix(c.Domain, ".") {
		out.Domain = c.Domain
	}
	if c.Expires >= 0 {
		out.Expires = time.Unix(int64(c.Expires), 0)
	}
	switch c.SameSite {
	case "Strict":
		out.SameSite = http.SameSiteStrictMode
	case "Lax":
		out.SameSite = http.SameSiteLaxMode
	case "None":
		out.SameSite = http.SameSiteNoneMode
	}
	return out
}
func (j *sessionJar) snapshot() []SessionCookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := float64(time.Now().Unix())
	out := []SessionCookie{}
	for key, c := range j.records {
		if c.Expires >= 0 && c.Expires <= now {
			delete(j.records, key)
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(a, b int) bool { return cookieKey(out[a]) < cookieKey(out[b]) })
	return out
}
func (j *sessionJar) Cookies(u *url.URL) []*http.Cookie {
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	for _, c := range j.snapshot() {
		origin := &url.URL{Scheme: "https", Host: strings.TrimPrefix(c.Domain, "."), Path: c.Path}
		jar.SetCookies(origin, []*http.Cookie{nativeCookie(c)})
	}
	return jar.Cookies(u)
}
func (j *sessionJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	for _, c := range cookies {
		// Let the standard jar reject invalid domains and names, including suffixes.
		probe, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
		check := *c
		check.MaxAge = 0
		check.Expires = time.Time{}
		target := *u
		target.Scheme = "https"
		if strings.HasPrefix(c.Path, "/") {
			target.Path = c.Path
		}
		probe.SetCookies(u, []*http.Cookie{&check})
		if len(probe.Cookies(&target)) == 0 {
			continue
		}
		domain := strings.ToLower(u.Hostname())
		if c.Domain != "" {
			domain = "." + strings.TrimPrefix(strings.ToLower(c.Domain), ".")
		}
		path := c.Path
		if !strings.HasPrefix(path, "/") {
			path = "/"
			if n := strings.LastIndex(u.Path, "/"); n > 0 {
				path = u.Path[:n]
			}
		}
		record := SessionCookie{Name: c.Name, Value: c.Value, Domain: domain, Path: path, Expires: -1, Secure: c.Secure, HTTPOnly: c.HttpOnly}
		switch c.SameSite {
		case http.SameSiteStrictMode:
			record.SameSite = "Strict"
		case http.SameSiteLaxMode:
			record.SameSite = "Lax"
		case http.SameSiteNoneMode:
			record.SameSite = "None"
		}
		if c.MaxAge > 0 {
			record.Expires = float64(now.Add(time.Duration(c.MaxAge) * time.Second).Unix())
		} else if !c.Expires.IsZero() {
			record.Expires = float64(c.Expires.Unix())
		}
		key := cookieKey(record)
		if c.MaxAge < 0 || (record.Expires >= 0 && record.Expires <= float64(now.Unix())) {
			delete(j.records, key)
		} else {
			j.records[key] = record
		}
	}
}

func NewClientWithSession(cookies map[string]string, session *BrowserSession, host, proxy string, verbose bool) (*Client, error) {
	if session == nil {
		return NewClient(cookies, host, proxy, verbose)
	}
	if err := session.Validate(host); err != nil {
		return nil, err
	}
	c, err := NewClient(nil, host, proxy, verbose)
	if err != nil {
		return nil, err
	}
	c.session = session.Clone()
	c.scopedJar = newSessionJar(session.Cookies)
	c.httpc.SetCookieJar(c.scopedJar)
	return c, nil
}

func (c *Client) SessionSnapshot() *BrowserSession {
	c.cookieMu.RLock()
	defer c.cookieMu.RUnlock()
	s := c.session.Clone()
	if s != nil {
		s.Cookies = c.scopedJar.snapshot()
	}
	return s
}

// RecoverSession performs one authentication exchange, never retries a business
// mutation. The account pool serializes and persists the recovery budget.
func (c *Client) RecoverSession(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.cookieMu.Lock()
	s := c.session
	if s == nil || s.Auth.SessionToken == "" {
		c.cookieMu.Unlock()
		return ErrAuthFailed
	}
	if s.RecoveryBlocked {
		c.cookieMu.Unlock()
		return ErrOTPRequired
	}
	if time.Now().Unix() < s.RecoveryAfter {
		c.cookieMu.Unlock()
		return ErrRecoveryDeferred
	}
	s.RecoveryAfter = time.Now().Add(5 * time.Minute).Unix()
	auth := s.Auth
	expected := s.DSID
	c.cookieMu.Unlock()
	body := map[string]any{"dsWebAuthToken": auth.SessionToken, "trustToken": auth.TrustToken, "accountCountryCode": auth.AccountCountry, "extended_login": true}
	raw, err := c.RequestWithContext(ctx, "POST", "https://setup."+c.Host+"/setup/ws/1/accountLogin", body, RequestTimeout, 1)
	if err == nil {
		err = checkSessionIdentity(raw, expected)
	}
	if err == nil {
		c.serviceURL = ""
		err = c.validateSessionLocked(ctx)
	}
	c.cookieMu.Lock()
	defer c.cookieMu.Unlock()
	if err == nil {
		s.Trusted = true
	} else if errors.Is(err, ErrAuthFailed) || errors.Is(err, ErrOTPRequired) || errors.Is(err, ErrSessionIdentity) {
		s.RecoveryBlocked = true
	}
	return err
}

var ErrRecoveryDeferred = errors.New("session recovery is cooling down")
var ErrAccessDenied = errors.New("upstream access denied without authentication evidence")

var ErrSessionIdentity = errors.New("browser session identity changed")

func (c *Client) ValidateSessionWithRecovery(ctx context.Context) error {
	err := c.ValidateSessionWithContext(ctx)
	if c.scopedJar != nil && (errors.Is(err, ErrOTPRequired) || errors.Is(err, ErrSessionIdentity)) {
		c.cookieMu.Lock()
		c.session.RecoveryBlocked = true
		c.cookieMu.Unlock()
	}
	if errors.Is(err, ErrAuthFailed) && c.scopedJar != nil {
		return c.RecoverSession(ctx)
	}
	return err
}

func (c *Client) SessionNeedsValidation() bool {
	return c.scopedJar != nil && c.CookieSnapshot()["X-APPLE-WEBAUTH-TOKEN"] == ""
}

// EncodeSession preserves the legacy format until an account has browser data.
func EncodeSession(cookies map[string]string, session *BrowserSession) string {
	var value any = cookies
	if session != nil {
		value = session
	}
	raw, _ := json.Marshal(value)
	return string(raw)
}
func DecodeSession(raw, host string) (map[string]string, *BrowserSession, error) {
	if raw == "" {
		return nil, nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return nil, nil, fmt.Errorf("invalid stored cookie data")
	}
	if version := fields["version"]; len(version) > 0 && string(version) != "null" && version[0] != '"' {
		var s BrowserSession
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			return nil, nil, fmt.Errorf("invalid stored browser session")
		}
		if err := s.Validate(host); err != nil {
			return nil, nil, err
		}
		return s.CookieMap(), &s, nil
	}
	var cookies map[string]string
	if err := json.Unmarshal([]byte(raw), &cookies); err != nil {
		return nil, nil, fmt.Errorf("invalid legacy cookie data")
	}
	return cookies, nil, nil
}
