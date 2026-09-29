package hme

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
)

func browserFixture() *BrowserSession {
	return &BrowserSession{Version: 1, Host: "icloud.com", DSID: "123", Trusted: true, CapturedAt: time.Now().Unix(), Auth: SessionAuth{SessionToken: "auth-secret", TrustToken: "trust-secret", AccountCountry: "USA"}, Cookies: []SessionCookie{
		{Name: "X-APPLE-WEBAUTH-TOKEN", Value: "old", Domain: ".icloud.com", Path: "/", Expires: -1, Secure: true},
	}}
}

func TestSessionJarScopesExpiryAndDeletion(t *testing.T) {
	j := newSessionJar([]SessionCookie{
		{Name: "same", Value: "root", Domain: ".icloud.com", Path: "/", Expires: -1, Secure: true},
		{Name: "same", Value: "path", Domain: ".icloud.com", Path: "/setup", Expires: -1, Secure: true},
		{Name: "same", Value: "china", Domain: ".icloud.com.cn", Path: "/", Expires: -1, Secure: true},
		{Name: "host-only", Value: "setup", Domain: "setup.icloud.com", Path: "/", Expires: -1, Secure: true},
		{Name: "expired", Value: "gone", Domain: ".icloud.com", Path: "/", Expires: float64(time.Now().Add(-time.Hour).Unix())},
	})
	u, _ := url.Parse("https://setup.icloud.com/setup/ws/1/validate")
	cookies := j.Cookies(u)
	if len(cookies) != 3 || cookies[0].Value != "path" {
		t.Fatalf("scope/path selection failed: %v", cookies)
	}
	other, _ := url.Parse("https://p01-maildomainws.icloud.com/v2/hme/list")
	if got := j.Cookies(other); len(got) != 1 || got[0].Value != "root" {
		t.Fatalf("host-only leaked: %v", got)
	}
	plain, _ := url.Parse("http://setup.icloud.com/setup")
	if len(j.Cookies(plain)) != 0 {
		t.Fatal("secure cookie sent over HTTP")
	}
	j.SetCookies(u, []*http.Cookie{{Name: "same", Domain: ".icloud.com", Path: "/setup", MaxAge: -1}})
	if got := j.Cookies(u); len(got) != 2 {
		t.Fatalf("path deletion removed wrong cookies: %v", got)
	}
	// Max-Age overrides an expired Expires value, and becomes an absolute deadline.
	j.SetCookies(u, []*http.Cookie{{Name: "rotated", Value: "new", Domain: ".icloud.com", Path: "/", MaxAge: 120, Expires: time.Unix(1, 0)}})
	j.SetCookies(u, []*http.Cookie{{Name: "invalid", Value: "bad", Domain: ".com", Path: "/"}, {Name: "cross", Value: "bad", Domain: ".icloud.com.cn", Path: "/"}})
	snapshot := j.snapshot()
	for _, c := range snapshot {
		if c.Name == "invalid" || c.Name == "cross" || c.Name == "expired" {
			t.Fatal("invalid cookie retained")
		}
		if c.Name == "rotated" && c.Expires < float64(time.Now().Unix()+100) {
			t.Fatal("max-age lost")
		}
	}
	if !reflect.DeepEqual(j.Cookies(u), newSessionJar(snapshot).Cookies(u)) {
		t.Fatal("restart changed cookie selection")
	}
}

type sessionTransport func(*http.Request) (*http.Response, error)

func (f sessionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type sessionTestHTTP struct {
	tls_client.HttpClient
	inner *http.Client
}

func (h *sessionTestHTTP) Do(r *http.Request) (*http.Response, error) { return h.inner.Do(r) }
func (h *sessionTestHTTP) SetCookieJar(j http.CookieJar) {
	h.inner.Jar = j
	h.HttpClient.SetCookieJar(j)
}
func responseFor(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}
func sessionClient(t *testing.T, fn sessionTransport) *Client {
	t.Helper()
	c, err := NewClientWithSession(nil, browserFixture(), "icloud.com", "", false)
	if err != nil {
		t.Fatal(err)
	}
	c.httpc = &sessionTestHTTP{HttpClient: c.httpc, inner: &http.Client{Jar: c.scopedJar, Transport: fn}}
	t.Cleanup(c.Close)
	return c
}

func TestSessionRecoveryRefreshesCookiesAndChecksIdentity(t *testing.T) {
	attempts := 0
	validations := 0
	c := sessionClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/setup/ws/1/accountLogin" {
			attempts++
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["dsWebAuthToken"] != "auth-secret" || body["trustToken"] != "trust-secret" || body["extended_login"] != true {
				t.Fatal("authentication materials missing")
			}
			resp := responseFor(r, 200, `{"dsInfo":{"dsid":"123"},"hsaTrustedBrowser":true}`)
			resp.Header.Add("Set-Cookie", "X-APPLE-WEBAUTH-TOKEN=new; Domain=.icloud.com; Path=/; Max-Age=7200; Secure")
			resp.Header.Set("X-Apple-Session-Token", "rotated-auth")
			resp.Header.Set("X-Apple-TwoSV-Trust-Token", "rotated-trust")
			resp.Header.Set("X-Apple-ID-Account-Country", "CHN")
			return resp, nil
		}
		if r.URL.Path != "/setup/ws/1/validate" {
			t.Fatalf("unexpected operation: %s", r.URL.Path)
		}
		validations++
		cookie, _ := r.Cookie("X-APPLE-WEBAUTH-TOKEN")
		if cookie == nil || cookie.Value != "new" {
			return responseFor(r, 401, "expired"), nil
		}
		return responseFor(r, 200, `{"dsInfo":{"dsid":"123"},"hsaTrustedBrowser":true,"webservices":{"premiummailsettings":{"url":"https://p01-maildomainws.icloud.com"}}}`), nil
	})
	checkpoints := 0
	c.SessionValidated = func(s *BrowserSession, info *AccountInfo, endpoint string) error {
		checkpoints++
		if s.Auth.SessionToken != "rotated-auth" || info.DSID != "123" || endpoint == "" || s.RecoveryAfter == 0 {
			t.Fatal("recovery checkpoint incomplete")
		}
		return nil
	}
	if err := c.ValidateSessionWithRecovery(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || validations != 2 || checkpoints != 1 {
		t.Fatalf("unexpected calls: login=%d validate=%d", attempts, validations)
	}
	s := c.SessionSnapshot()
	if s.Auth.SessionToken != "rotated-auth" || s.Auth.TrustToken != "rotated-trust" || s.Auth.AccountCountry != "CHN" || s.CookieMap()["X-APPLE-WEBAUTH-TOKEN"] != "new" {
		t.Fatal("refreshed session lost")
	}
	if err := c.ValidateSessionWithRecovery(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatal("valid session unnecessarily reauthenticated")
	}
}

func TestMalformedSessionResponseDoesNotBlockRecovery(t *testing.T) {
	for _, body := range []string{`{`, `{}`, `[]`, `{"hsaTrustedBrowser":null}`, `{"hsaTrustedBrowser":"false"}`, `{"hsaTrustedBrowser":true}`, `{"hsaTrustedBrowser":true,"dsInfo":{"dsid":{}}}`, `{"hsaTrustedBrowser":true,"hsaChallengeRequired":"true"}`} {
		for _, recovery := range []bool{false, true} {
			c := sessionClient(t, func(r *http.Request) (*http.Response, error) { return responseFor(r, 200, body), nil })
			c.SessionValidated = func(*BrowserSession, *AccountInfo, string) error { t.Fatal("invalid session checkpointed"); return nil }
			var err error
			if recovery {
				err = c.RecoverSession(context.Background())
			} else {
				err = c.ValidateSessionWithRecovery(context.Background())
			}
			if !errors.Is(err, ErrInvalidResponseSchema) || c.SessionSnapshot().RecoveryBlocked {
				t.Fatalf("body=%s recovery=%v err=%v blocked=%v", body, recovery, err, c.SessionSnapshot().RecoveryBlocked)
			}
		}
	}
}

func TestSessionCheckpointFailureIsNotReportedAsAuthenticationSuccess(t *testing.T) {
	c := sessionClient(t, func(r *http.Request) (*http.Response, error) {
		return responseFor(r, 200, `{"dsInfo":{"dsid":"123"},"hsaTrustedBrowser":true,"webservices":{"premiummailsettings":{"url":"https://p01-maildomainws.icloud.com"}}}`), nil
	})
	persistErr := errors.New("checkpoint persistence failed")
	c.SessionValidated = func(*BrowserSession, *AccountInfo, string) error { return persistErr }
	if err := c.ValidateSessionWithRecovery(context.Background()); !errors.Is(err, persistErr) {
		t.Fatal(err)
	}
	if c.SessionSnapshot().RecoveryBlocked || c.SessionSnapshot().RecoveryAfter != 0 {
		t.Fatal("persistence failure triggered authentication")
	}
}

func TestEndpointRediscoveryPropagatesAuthenticationFailure(t *testing.T) {
	calls := 0
	c := sessionClient(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path == "/setup/ws/1/validate" {
			return responseFor(r, 401, "expired"), nil
		}
		return responseFor(r, 403, "Forbidden"), nil
	})
	c.SetServiceURL("https://p01-maildomainws.icloud.com")
	_, err := c.ListAliasesWithContext(context.Background())
	if !errors.Is(err, ErrAuthFailed) || calls != 2 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestValidatedCheckpointPrecedesFailedResponseCookieDeletion(t *testing.T) {
	var checkpoint *BrowserSession
	c := sessionClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/setup/ws/1/validate" {
			resp := responseFor(r, 200, `{"dsInfo":{"dsid":"123"},"hsaTrustedBrowser":true,"webservices":{"premiummailsettings":{"url":"https://p01-maildomainws.icloud.com"}}}`)
			resp.Header.Set("Set-Cookie", "X-APPLE-WEBAUTH-TOKEN=new; Domain=.icloud.com; Path=/; Secure")
			resp.Header.Set("X-Apple-Session-Token", "rotated")
			return resp, nil
		}
		resp := responseFor(r, 401, "expired")
		resp.Header.Set("Set-Cookie", "X-APPLE-WEBAUTH-TOKEN=; Domain=.icloud.com; Path=/; Max-Age=0")
		return resp, nil
	})
	c.SessionValidated = func(s *BrowserSession, info *AccountInfo, endpoint string) error {
		if info.DSID != "123" || endpoint == "" {
			t.Fatal("checkpoint before identity/endpoint validation")
		}
		checkpoint = s
		return nil
	}
	if err := c.ValidateSessionWithRecovery(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListAliasesWithContext(context.Background()); !errors.Is(err, ErrAuthFailed) {
		t.Fatal(err)
	}
	if c.CookieSnapshot()["X-APPLE-WEBAUTH-TOKEN"] != "" {
		t.Fatal("test deletion was not applied")
	}
	if checkpoint == nil || checkpoint.Auth.SessionToken != "rotated" || checkpoint.CookieMap()["X-APPLE-WEBAUTH-TOKEN"] != "new" {
		t.Fatal("validated checkpoint corrupted")
	}
}

func TestSessionRecoveryStopsOnChallengeMismatchAndCooldown(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		want    error
		blocked bool
	}{
		{"challenge", 200, `{"dsInfo":{"dsid":"123"},"hsaTrustedBrowser":false}`, ErrOTPRequired, true},
		{"identity", 200, `{"dsInfo":{"dsid":"456"},"hsaTrustedBrowser":true}`, ErrSessionIdentity, true},
		{"rejected", 401, "auth-secret echoed", ErrAuthFailed, true},
		{"limited", 429, "try later", ErrRateLimited, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts := 0
			c := sessionClient(t, func(r *http.Request) (*http.Response, error) {
				attempts++
				return responseFor(r, tc.status, tc.body), nil
			})
			err := c.RecoverSession(context.Background())
			if !errors.Is(err, tc.want) {
				t.Fatalf("wrong error: %v", err)
			}
			if strings.Contains(err.Error(), "auth-secret") {
				t.Fatal("response leaked authentication material")
			}
			s := c.SessionSnapshot()
			if s.RecoveryBlocked != tc.blocked || s.RecoveryAfter <= time.Now().Unix() {
				t.Fatal("recovery budget not retained")
			}
			_ = c.RecoverSession(context.Background())
			if attempts != 1 {
				t.Fatal("repeated recovery despite cooldown/block")
			}
		})
	}
}

func TestBrowserSessionFormatsAndUnsupportedScope(t *testing.T) {
	cookies, s, err := DecodeSession(`{"version":"legacy-cookie","token":"old"}`, "icloud.com")
	if err != nil || s != nil || cookies["version"] != "legacy-cookie" {
		t.Fatal("legacy map compatibility broken")
	}
	source := browserFixture()
	raw := EncodeSession(nil, source)
	_, s, err = DecodeSession(raw, "icloud.com")
	if err != nil || !reflect.DeepEqual(source, s) {
		t.Fatalf("session roundtrip failed: %v", err)
	}
	if _, _, err := DecodeSession(raw, "icloud.com.cn"); err == nil {
		t.Fatal("cross-region import accepted")
	}
	source.Cookies[0].PartitionKey = "https://example.test"
	if source.Validate("icloud.com") == nil {
		t.Fatal("partitioned cookie silently flattened")
	}
	if _, _, err := DecodeSession(`{"version":9}`, "icloud.com"); err == nil {
		t.Fatal("unknown format silently dropped")
	}
}

func TestSessionChallengeWithoutServiceEndpointRequiresReauthentication(t *testing.T) {
	c := sessionClient(t, func(r *http.Request) (*http.Response, error) {
		return responseFor(r, 200, `{"dsInfo":{"dsid":"123"},"hsaChallengeRequired":true,"hsaTrustedBrowser":false}`), nil
	})
	if err := c.ValidateSessionWithRecovery(context.Background()); !errors.Is(err, ErrOTPRequired) {
		t.Fatalf("challenge hidden by missing service: %v", err)
	}
	if !c.SessionSnapshot().RecoveryBlocked {
		t.Fatal("MFA requirement not retained")
	}
}

func TestExpiredCoreCookieRequiresPreflightAfterRestart(t *testing.T) {
	s := browserFixture()
	s.Cookies[0].Expires = float64(time.Now().Add(-time.Second).Unix())
	c, err := NewClientWithSession(nil, s, "icloud.com", "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !c.SessionNeedsValidation() {
		t.Fatal("expired cookie bypassed preflight")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.RecoverSession(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
	if c.SessionSnapshot().RecoveryAfter != 0 {
		t.Fatal("cancelled request consumed recovery budget")
	}
}

func TestSessionWAFDeniedDoesNotTriggerAuthentication(t *testing.T) {
	calls := 0
	c := sessionClient(t, func(r *http.Request) (*http.Response, error) {
		calls++
		return responseFor(r, 403, "<html>Forbidden</html>"), nil
	})
	if err := c.ValidateSessionWithRecovery(context.Background()); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("access denial misclassified: %v", err)
	}
	if calls != 1 || c.SessionSnapshot().RecoveryAfter != 0 || c.SessionSnapshot().RecoveryBlocked {
		t.Fatal("WAF response triggered authentication recovery")
	}
}

func TestSessionHTTP200AuthenticationErrorUsesRecovery(t *testing.T) {
	logins := 0
	c := sessionClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/setup/ws/1/accountLogin" {
			logins++
			return responseFor(r, 200, `{"dsInfo":{"dsid":"123"},"hsaTrustedBrowser":true}`), nil
		}
		if logins == 0 {
			return responseFor(r, 200, `{"success":false,"error":{"errorCode":"AUTHENTICATION_FAILED"}}`), nil
		}
		return responseFor(r, 200, `{"dsInfo":{"dsid":"123"},"hsaTrustedBrowser":true,"webservices":{"premiummailsettings":{"url":"https://p01-maildomainws.icloud.com"}}}`), nil
	})
	if err := c.ValidateSessionWithRecovery(context.Background()); err != nil {
		t.Fatal(err)
	}
	if logins != 1 {
		t.Fatalf("logins=%d", logins)
	}
}

func TestSessionRecoveryHonorsLongerRetryAfter(t *testing.T) {
	c := sessionClient(t, func(r *http.Request) (*http.Response, error) {
		resp := responseFor(r, 429, "limited")
		resp.Header.Set("Retry-After", "3600")
		return resp, nil
	})
	start := time.Now().Unix()
	if err := c.RecoverSession(context.Background()); !errors.Is(err, ErrRateLimited) {
		t.Fatal(err)
	}
	if c.SessionSnapshot().RecoveryAfter < start+3599 {
		t.Fatal("server retry deadline shortened")
	}
}

func TestLegacyCookieHeaderHasOneSourceAndHonorsDeletion(t *testing.T) {
	c, err := NewClient(map[string]string{"X-APPLE-WEBAUTH-TOKEN": `v=2:t=AQ==fixture+/~`, "X-APPLE-WEBAUTH-HSA-TRUST": `"fixture==SRVX:other==SRVX"`}, "icloud.com", "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	calls := 0
	inner := &http.Client{Jar: c.httpc.GetCookieJar(), Transport: sessionTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		counts := map[string]int{}
		for _, cookie := range r.Cookies() {
			counts[cookie.Name]++
		}
		want := 1
		if calls > 1 {
			want = 0
		}
		if counts["X-APPLE-WEBAUTH-TOKEN"] != want || counts["X-APPLE-WEBAUTH-HSA-TRUST"] != 1 {
			t.Errorf("duplicate/stale credentials: counts=%v", counts)
		}
		if calls == 1 {
			token, _ := r.Cookie("X-APPLE-WEBAUTH-TOKEN")
			if token.Value != `v=2:t=AQ==fixture+/~` {
				t.Error("opaque token damaged")
			}
		}
		resp := responseFor(r, 200, `{}`)
		resp.Header.Set("Set-Cookie", "X-APPLE-WEBAUTH-TOKEN=; Path=/; Max-Age=0")
		return resp, nil
	})}
	c.httpc = &sessionTestHTTP{HttpClient: c.httpc, inner: inner}
	for i := 0; i < 2; i++ {
		if _, err := c.RequestWithContext(context.Background(), "POST", "https://setup.icloud.com/setup/ws/1/validate", nil, 0, 1); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSRPJarHandoffKeepsProtocolCookieSourceUnique(t *testing.T) {
	c, err := NewClient(nil, "icloud.com", "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.httpc = &sessionTestHTTP{HttpClient: c.httpc, inner: &http.Client{Transport: sessionTransport(func(r *http.Request) (*http.Response, error) {
		resp := responseFor(r, 200, `{"dsInfo":{"dsid":"123"}}`)
		if r.URL.String() == authTrust {
			resp.StatusCode = 204
		}
		resp.Header.Set("Set-Cookie", "X-APPLE-WEBAUTH-TOKEN=fixture; Path=/; Secure")
		return resp, nil
	})}}
	c.httpc.SetCookieJar(tls_client.NewCookieJar())
	if err := c.finishAuth(&authState{}); err != nil {
		t.Fatal(err)
	}
	if c.CookieSnapshot()["X-APPLE-WEBAUTH-TOKEN"] != "fixture" || c.httpc.GetCookieJar() != nil {
		t.Fatal("authentication cookies not handed to protocol map")
	}
}

func TestExplicitJSONAuthFailureNeverReplaysMutation(t *testing.T) {
	calls := 0
	c := sessionClient(t, func(r *http.Request) (*http.Response, error) {
		calls++
		return responseFor(r, 200, `{"success":false,"error":{"errorCode":"AUTHENTICATION_FAILED"}}`), nil
	})
	c.SetServiceURL("https://p01-maildomainws.icloud.com")
	if _, err := c.ReserveWithContext(context.Background(), "fixture@icloud.com", "test"); !errors.Is(err, ErrAuthFailed) {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("mutation replayed: %d", calls)
	}
}
