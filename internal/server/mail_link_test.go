package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

// TestSignedMailLink 单别名签名直链：只读本别名、不可改道、不可触达管理面、过期与一键作废生效。
func TestSignedMailLink(t *testing.T) {
	dir := t.TempDir()
	st, err := store.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer st.Close()
	if st.Cipher() == nil {
		t.Fatal("测试环境应配置 Master Key")
	}

	const aliasA, aliasB = "link_a@icloud.com", "link_b@icloud.com"
	fb := &fakeBackend{accounts: []account.Summary{
		{ID: "acc_1", RealEmail: "one@icloud.com", Status: "active", HasCookies: true},
		{ID: "acc_2", RealEmail: "two@icloud.com", Status: "active", HasCookies: true},
	}}
	srv := newWithBackendAndStore(fb, Config{AdminPassword: "admin-pass-2026-strong", APIKey: "admin-key-for-test", DataDir: dir}, st)
	_ = st.UpsertAliasRoutes("acc_1", []string{aliasA})
	_ = st.UpsertAliasRoutes("acc_2", []string{aliasB})

	serve := func(method, target, body string, admin bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if admin {
			req.Header.Set("X-API-Key", "admin-key-for-test")
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	issue := func(email string) url.Values {
		t.Helper()
		rec := serve("POST", "/api/mail-links", `{"email":"`+email+`","days":7}`, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("签发失败 %d: %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Data struct{ Query string } `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		q, err := url.ParseQuery(out.Data.Query)
		if err != nil || q.Get("sig") == "" {
			t.Fatalf("签发结果缺少签名: %s", rec.Body.String())
		}
		return q
	}
	publish := func(email, code string) {
		srv.eventBus.PublishEvent(&mail.CachedOTP{
			EventID: "ev_" + code, Email: email, AccountID: "acc_1", Folder: "INBOX",
			OTP: &mail.OTPResult{Code: code}, ExpiresAt: time.Now().Add(time.Minute),
		})
	}
	expectCode := func(name string, rec *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		if rec.Code != status || !strings.Contains(rec.Body.String(), code) {
			t.Fatalf("%s: 期望 %d/%s, 实际 %d: %s", name, status, code, rec.Code, rec.Body.String())
		}
	}

	q := issue(aliasA)
	publish(aliasA, "112233")
	expectCode("本别名取码", serve("GET", "/mail/code?timeout=0&"+q.Encode(), "", false), http.StatusOK, "112233")

	tampered := url.Values{"email": {aliasB}, "exp": {q.Get("exp")}, "sig": {q.Get("sig")}}
	expectCode("改邮箱", serve("GET", "/mail/code?timeout=0&"+tampered.Encode(), "", false), http.StatusUnauthorized, "INVALID_LINK")
	expectCode("路径改道", serve("GET", "/mail/code/"+aliasB+"?timeout=0&"+q.Encode(), "", false), http.StatusUnauthorized, "INVALID_LINK")
	expectCode("账号改道", serve("GET", "/mail/raw?account_id=acc_2&"+q.Encode(), "", false), http.StatusNotFound, "RESOURCE_NOT_FOUND")
	expectCode("管理面", serve("GET", "/api/aliases?"+q.Encode(), "", false), http.StatusUnauthorized, "AUTH_REQUIRED")

	key, _ := srv.mailLinkKey()
	past := time.Now().Add(-time.Minute).Unix()
	expired := url.Values{"email": {aliasA}, "exp": {strconv.FormatInt(past, 10)}, "sig": {mailLinkSig(key, aliasA, past)}}
	expectCode("已过期", serve("GET", "/mail/code?timeout=0&"+expired.Encode(), "", false), http.StatusUnauthorized, "INVALID_LINK")

	expectCode("天数越界", serve("POST", "/api/mail-links", `{"email":"`+aliasA+`","days":0}`, true), http.StatusBadRequest, "VALIDATION_ERROR")

	expectCode("作废", serve("POST", "/api/mail-links/revoke", "", true), http.StatusOK, `"revoked":true`)
	expectCode("作废后旧链", serve("GET", "/mail/code?timeout=0&"+q.Encode(), "", false), http.StatusUnauthorized, "INVALID_LINK")
	publish(aliasA, "445566")
	expectCode("作废后新链", serve("GET", "/mail/code?timeout=0&"+issue(aliasA).Encode(), "", false), http.StatusOK, "445566")
}
