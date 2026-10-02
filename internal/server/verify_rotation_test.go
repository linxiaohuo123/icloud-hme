package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

func TestRotatedTokenCannotReceivePendingCode(t *testing.T) {
	for _, queryCredential := range []bool{false, true} {
		t.Run(map[bool]string{false: "bearer", true: "query"}[queryCredential], func(t *testing.T) {
			st, err := store.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			tok, err := st.CreateToken("worker", store.ScopeVerify, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(`INSERT INTO alias_allocations(allocation_id,alias_email,account_id,owner_kind,owner_id,business_tag,allocated_at,status) VALUES('rotation_alloc','rotation@icloud.com','acc','token',?,'default',datetime('now'),'allocated')`, tok.ID); err != nil {
				t.Fatal(err)
			}
			bus := mail.NewEventBus(time.Minute)
			srv := &Server{store: st, eventBus: bus}
			router := gin.New()
			router.GET("/mail/code", requireSession(nil, "", st), srv.verifyCodeHandler)
			makeRequest := func(credential string) *http.Request {
				url := "/mail/code?email=rotation@icloud.com"
				if queryCredential {
					url += "&token=" + credential
				}
				req := httptest.NewRequest("GET", url, nil)
				if !queryCredential {
					req.Header.Set("Authorization", "Bearer "+credential)
				}
				return req
			}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			rec := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { router.ServeHTTP(rec, makeRequest(tok.Token).WithContext(ctx)); close(done) }()
			for bus.SubscriberCount("rotation@icloud.com") == 0 {
				select {
				case <-ctx.Done():
					t.Fatal("request did not subscribe")
				case <-time.After(time.Millisecond):
				}
			}
			rotated, err := st.RotateToken(tok.ID)
			if err != nil {
				t.Fatal(err)
			}
			if st.ValidateToken(tok.Token) {
				t.Fatal("old credential remained valid")
			}
			bus.Publish("rotation@icloud.com", "acc", "Verification code", "Service", time.Now().Format(time.RFC3339), &mail.OTPResult{Code: "123456"})
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("pending request did not finish")
			}
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("rotated credential received code: status=%d body=%s", rec.Code, rec.Body)
			}
			// 拒绝旧请求不能消费事件；新凭据必须仍可收到该验证码。
			fresh := httptest.NewRecorder()
			router.ServeHTTP(fresh, makeRequest(rotated.Token).WithContext(ctx))
			if fresh.Code != http.StatusOK || !strings.Contains(fresh.Body.String(), `"code":"123456"`) {
				t.Fatalf("new credential lost event: status=%d body=%s", fresh.Code, fresh.Body)
			}
		})
	}
}

func TestV2RotationBlocksEveryWaitingResultPath(t *testing.T) {
	for _, delivery := range []string{"event", "database", "pending"} {
		t.Run(delivery, func(t *testing.T) {
			st, task := newVerificationRegressionStore(t)
			bus := mail.NewEventBus(time.Minute)
			srv := &Server{store: st, verifyService: NewVerificationService(nil, st, bus, nil)}
			router := gin.New()
			router.GET("/api/external/v2/verification-requests/:request_id", requireExternalV2Auth("", st), srv.externalV2GetVerificationRequestHandler)
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			request := func(credential string, timeout string) *http.Request {
				req := httptest.NewRequest("GET", "/api/external/v2/verification-requests/"+task.RequestID+"?timeout="+timeout, nil).WithContext(ctx)
				req.Header.Set("Authorization", "Bearer "+credential)
				return req
			}
			rec := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); router.ServeHTTP(rec, request("review-test-token", "1")) }()
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for bus.SubscriberCount(task.AliasEmail) == 0 {
				select {
				case <-ticker.C:
				case <-done:
					t.Fatalf("request returned before subscription: %d", rec.Code)
				case <-ctx.Done():
					t.Fatal("subscription timed out")
				}
			}
			rotated, err := st.RotateToken(task.PrincipalID)
			if err != nil {
				t.Fatal(err)
			}
			if st.ValidateToken("review-test-token") {
				t.Fatal("old credential remained valid")
			}
			switch delivery {
			case "event":
				bus.PublishEvent(&mail.CachedOTP{EventID: "rotation-event", Email: task.AliasEmail, Folder: "INBOX", UIDValidity: 1, UID: 105, OTP: &mail.OTPResult{Code: "654321"}})
			case "database":
				if _, won, err := st.CompleteVerificationRequestResult(ctx, task.RequestID, store.VerificationCompletion{Code: "654321", MatchedEventRef: "rotation-event"}, time.Now().UTC()); err != nil || !won {
					t.Fatalf("completion failed: won=%v err=%v", won, err)
				}
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("result delivery timed out")
			}
			if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"TOKEN_REVOKED"`) {
				t.Fatalf("rotated credential received result: %d %s", rec.Code, rec.Body)
			}
			fresh := httptest.NewRecorder()
			router.ServeHTTP(fresh, request(rotated.Token, "0"))
			if fresh.Code != http.StatusOK {
				t.Fatalf("new credential rejected: %d %s", fresh.Code, fresh.Body)
			}
			if delivery != "pending" && !strings.Contains(fresh.Body.String(), `"code":"654321"`) {
				t.Fatalf("new credential lost result: %s", fresh.Body)
			}
		})
	}
}

func TestV2RotationBetweenAuthenticationAndServiceIsRejected(t *testing.T) {
	st, task := newVerificationRegressionStore(t)
	srv := &Server{store: st, verifyService: NewVerificationService(nil, st, mail.NewEventBus(time.Minute), nil)}
	router := gin.New()
	router.GET("/api/external/v2/verification-requests/:request_id", requireExternalV2Auth("", st), func(c *gin.Context) {
		if _, err := st.RotateToken(task.PrincipalID); err != nil {
			t.Fatal(err)
		}
		c.Next()
	}, srv.externalV2GetVerificationRequestHandler)
	req := httptest.NewRequest("GET", "/api/external/v2/verification-requests/"+task.RequestID, nil)
	req.Header.Set("Authorization", "Bearer review-test-token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("rotation after authentication bypassed recheck: %d %s", rec.Code, rec.Body)
	}
}

// /mail/code?timeout=0 只查缓存：命中立即交付，未命中立即 408，均不进入长轮询等待
func TestMailCodeTimeoutZeroIsInstant(t *testing.T) {
	s := newWithBackend(&fakeBackend{}, Config{AdminPassword: "admin-pass-2026-strong", APIKey: "instant-key"})
	defer s.Close()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	get := func(email string) (int, string, time.Duration) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/mail/code?timeout=0&email="+email, nil)
		req.Header.Set("X-API-Key", "instant-key")
		start := time.Now()
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		buf := new(strings.Builder)
		_, _ = io.Copy(buf, resp.Body)
		return resp.StatusCode, buf.String(), time.Since(start)
	}

	status, body, took := get("missing@icloud.com")
	if status != http.StatusRequestTimeout || !strings.Contains(body, "VERIFY_TIMEOUT") || took > 2*time.Second {
		t.Fatalf("cache miss must return 408 immediately, got %d in %v: %s", status, took, body)
	}

	s.eventBus.Publish("instant@icloud.com", "acc_1", "Your code", "sender@example.com", "2026-10-02 12:00:00", &mail.OTPResult{Code: "246810"})
	status, body, took = get("instant@icloud.com")
	if status != http.StatusOK || !strings.Contains(body, "246810") || took > 2*time.Second {
		t.Fatalf("cache hit must be delivered immediately, got %d in %v: %s", status, took, body)
	}
}
