// [INPUT]: Production error mapping and HTTP shutdown, temporary SQLite and local TCP.
// [OUTPUT]: UUID/status classification, cancellation and complete shutdown regressions.
// [POS]: internal/server fourth-review repair tests.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/auth"
	"icloud-hme/internal/hme"

	"github.com/gin-gonic/gin"
)

func TestTransportIdentifiersDoNotBecomeAuthStatus(t *testing.T) {
	for _, digits := range []string{"401", "403", "421"} {
		for _, err := range []error{
			&url.Error{Op: "Post", URL: "https://example.invalid/list?clientId=00000000-0000-0000-0000-000000000" + digits, Err: errors.New("proxy Bad Gateway")},
			fmt.Errorf("proxy 127.0.0.1:%s refused; request_id=%s", digits, digits),
			fmt.Errorf("cookie refresh transport failed; clientId=%s", digits),
			fmt.Errorf("auth complete: %w", &url.Error{Op: "Post", URL: "https://example.invalid/auth?session=" + digits, Err: errors.New("Bad Gateway")}),
			fmt.Errorf("validate 失败: HTTP 500 - request_id=%s unauthorized", digits),
		} {
			for name, classify := range map[string]func(error) *BackendError{
				"upstream": func(e error) *BackendError { return classifyUpstreamErr("upstream failed", e) },
				"login":    classifyLoginErr, "inbox": classifyInboxErr,
			} {
				got := classify(err)
				if got.Status != http.StatusBadGateway || got.Code != "UPSTREAM_FAILURE" {
					t.Fatalf("%s classified transport error as credentials: err=%v result=%+v", name, err, got)
				}
			}
		}
	}
}

func TestExplicitAuthStatusAndSentinelsStillWork(t *testing.T) {
	for _, err := range []error{
		hme.ErrAuthFailed, fmt.Errorf("wrapped: %w", account.ErrCookieExpired),
		errors.New("auth complete 失败: HTTP 401"), errors.New("validate 失败: HTTP 403 - rejected"),
		errors.New("获取邮件失败: HTTP 421 - expired"),
	} {
		for _, classify := range []func(error) *BackendError{
			func(e error) *BackendError { return classifyUpstreamErr("upstream failed", e) }, classifyLoginErr, classifyInboxErr,
		} {
			if got := classify(err); got.Status != http.StatusUnauthorized {
				t.Fatalf("real auth rejection lost: %v -> %+v", err, got)
			}
		}
	}
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{context.Canceled, 499, "REQUEST_CANCELED"},
		{context.DeadlineExceeded, 504, "REQUEST_TIMEOUT"},
		{hme.ErrAccessDenied, 502, "UPSTREAM_ACCESS_DENIED"},
		{hme.ErrSessionIdentity, 409, "ACCOUNT_IDENTITY_MISMATCH"},
	} {
		got := classifyUpstreamErr("failed", tc.err)
		if got.Status != tc.status || got.Code != tc.code {
			t.Fatalf("error contract changed: %+v", got)
		}
	}
}

func TestHTTPCreateCanceledIsNot500(t *testing.T) {
	s, _, _, ts, token, lease := setupV2TestEnv(t)
	defer s.Close()
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("POST", "/api/external/v2/verification-requests", strings.NewReader(fmt.Sprintf(`{"lease_id":%q}`, lease))).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	// Isolate the create handler from auth's independently canceled DB lookup.
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("principal", auth.Principal{Kind: auth.PrincipalToken, ID: "canceled", Scopes: []string{"verify"}})
		c.Next()
	})
	r.POST("/api/external/v2/verification-requests", s.externalV2CreateVerificationRequestHandler)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 499 {
		t.Fatalf("canceled create returned %d: %s", w.Code, w.Body.String())
	}
}

func TestShutdownInterruptsBlockedRequestBody(t *testing.T) {
	s, st, _, ts, token, lease := setupV2TestEnv(t)
	defer s.Close()
	defer ts.Close()
	u, _ := url.Parse(ts.URL)
	conn, err := net.DialTimeout("tcp", u.Host, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body := fmt.Sprintf(`{"lease_id":%q}`, lease)
	fmt.Fprintf(conn, "POST /api/external/v2/verification-requests HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n{", u.Host, token, len(body))
	until := time.Now().Add(time.Second)
	for s.requestLimiter.Stats().ActiveInflight != 1 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if s.requestLimiter.Stats().ActiveInflight != 1 {
		t.Fatal("slow POST was not accepted")
	}
	start := time.Now()
	if err := s.shutdownHTTP(ts.Config); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("shutdown waited for the uncanceled 15s body deadline: %v", time.Since(start))
	}
	stats := s.requestLimiter.Stats()
	if stats.ActiveInflight != 0 || stats.ActiveWaiters != 0 {
		t.Fatalf("handlers did not drain: %+v", stats)
	}
	if st.DB().Ping() == nil {
		t.Fatal("Store remained open after successful shutdown")
	}
	conn.SetReadDeadline(time.Now().Add(time.Second))
	io.Copy(io.Discard, conn)
}
