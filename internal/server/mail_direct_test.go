/**
 * [INPUT]: 依赖 testing, net/http, net/http/httptest, strings, icloud-hme/internal/mail, icloud-hme/internal/account
 * [OUTPUT]: 对外提供 TestMailDirectLinks 测试套件
 * [POS]: internal/server 的对外直出链接集成测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"icloud-hme/internal/account"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

func TestMailDirectLinks(t *testing.T) {
	dir := t.TempDir()
	st, err := store.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer st.Close()

	// 1. 创建具备 verify 作用域的 API 令牌
	tok, err := st.CreateToken("test_worker", store.ScopeVerify, "")
	if err != nil {
		t.Fatalf("CreateToken failed: %v", err)
	}
	secretToken := tok.Token

	// 2. 准备 fakeBackend
	testEmail := "direct_test_alias@icloud.com"
	testAccountID := "acc_main_1"

	fb := &fakeBackend{
		accounts: []account.Summary{
			{ID: testAccountID, RealEmail: "owner@icloud.com", Status: "active", HasCookies: true},
			{ID: "acc_other_2", RealEmail: "other@icloud.com", Status: "active", HasCookies: true},
		},
		inbox: InboxResult{
			AccountID: testAccountID,
			Alias:     testEmail,
			Folder:    "INBOX",
			Count:     1,
			Messages: []mail.Message{
				{
					ID:      "msg_direct_1",
					From:    "Apple <appleid@id.apple.com>",
					Subject: "Your verification code is 654321",
					Date:    "2026-09-28T12:00:00Z",
					Preview: "Your Apple verification code is 654321. Do not share it.",
				},
			},
		},
		getMessageFunc: func(accountID, id string) (*mail.FullMessage, error) {
			return &mail.FullMessage{
				Message: mail.Message{
					ID:      id,
					From:    "Apple <appleid@id.apple.com>",
					Subject: "Your verification code is 654321",
					Date:    "2026-09-28T12:00:00Z",
				},
				Body:        "Your Apple verification code is 654321. Do not share it.",
				ContentType: "text/plain",
			}, nil
		},
	}

	srv := newWithBackendAndStore(fb, Config{
		AdminPassword: "admin-pass-2026-strong",
		DataDir:       dir,
	}, st)

	// 登记别名到该账号并关联到该 Token 所有权 (这样普通 verify token 能过权限校验)
	_ = st.UpsertAliasRoutes(testAccountID, []string{testEmail})
	_, err = st.DB().Exec(`
		INSERT INTO alias_allocations (allocation_id, alias_email, account_id, owner_kind, owner_id, business_tag, allocated_at, status)
		VALUES ('alloc_test_1', ?, ?, 'token', ?, 'test-tag', datetime('now'), 'allocated')
	`, testEmail, testAccountID, tok.ID)
	if err != nil {
		t.Fatalf("insert alias_allocations failed: %v", err)
	}

	// ==========================================
	// 场景 1: /mail/code?email=...&token=...
	// ==========================================
	t.Run("API VerifyCode Query Token", func(t *testing.T) {
		srv.eventBus.PublishEvent(&mail.CachedOTP{
			EventID:   "ev_1",
			Email:     testEmail,
			AccountID: testAccountID,
			Folder:    "INBOX",
			Subject:   "Your verification code is 654321",
			From:      "Apple <appleid@id.apple.com>",
			Date:      "2026-09-28T12:00:00Z",
			OTP: &mail.OTPResult{
				Code: "654321",
			},
			ExpiresAt: time.Now().Add(time.Minute),
		})

		req := httptest.NewRequest("GET", "/mail/code?email="+testEmail+"&token="+secretToken, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("期望 200, 实际 %d: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"code":"654321"`) {
			t.Fatalf("期望包含验证码 654321, 实际: %s", rec.Body.String())
		}
	})

	// ==========================================
	// 场景 2: /mail/code?email=...&token=... (别名直链)
	// ==========================================
	t.Run("Mail Code Direct Link", func(t *testing.T) {
		srv.eventBus.PublishEvent(&mail.CachedOTP{
			EventID:   "ev_2",
			Email:     testEmail,
			AccountID: testAccountID,
			Folder:    "INBOX",
			Subject:   "Your code is 987654",
			From:      "Verify Service",
			Date:      "2026-09-28T12:01:00Z",
			OTP: &mail.OTPResult{
				Code: "987654",
			},
			ExpiresAt: time.Now().Add(time.Minute),
		})

		req := httptest.NewRequest("GET", "/mail/code?email="+testEmail+"&token="+secretToken, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("期望 200, 实际 %d: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"code":"987654"`) {
			t.Fatalf("期望包含验证码 987654, 实际: %s", rec.Body.String())
		}

		// 测试 raw=1 纯字符串直出
		srv.eventBus.PublishEvent(&mail.CachedOTP{
			EventID:   "ev_2_raw",
			Email:     testEmail,
			AccountID: testAccountID,
			Folder:    "INBOX",
			Subject:   "Your code is 987654",
			From:      "Verify Service",
			Date:      "2026-09-28T12:01:00Z",
			OTP: &mail.OTPResult{
				Code: "987654",
			},
			ExpiresAt: time.Now().Add(time.Minute),
		})
		reqRaw := httptest.NewRequest("GET", "/mail/code?email="+testEmail+"&token="+secretToken+"&raw=1", nil)
		recRaw := httptest.NewRecorder()
		srv.Handler().ServeHTTP(recRaw, reqRaw)
		if recRaw.Code != http.StatusOK || recRaw.Body.String() != "987654" {
			t.Fatalf("期望纯文本 987654, 实际 %d: %q", recRaw.Code, recRaw.Body.String())
		}
	})

	// ==========================================
	// 场景 3: /mail/view?email=...&token=... (可视化网页查信)
	// ==========================================
	t.Run("Mail View HTML Direct Link", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/mail/view?email="+testEmail+"&token="+secretToken, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("期望 200, 实际 %d: %s", rec.Code, rec.Body.String())
		}
		ct := rec.Header().Get("Content-Type")
		if !strings.Contains(ct, "text/html") {
			t.Fatalf("期望 text/html, 实际 %s", ct)
		}
		body := rec.Body.String()
		if !strings.Contains(body, testEmail) {
			t.Fatalf("HTML 应包含邮箱地址 %s", testEmail)
		}
		if !strings.Contains(body, "654321") {
			t.Fatalf("HTML 应渲染出提取到的验证码 654321, 实际: %s", body)
		}
		if strings.Contains(body, "<style") || strings.Contains(body, "onclick=") || !strings.Contains(body, "/mail/view-assets.js") {
			t.Fatalf("邮件页应使用 CSP 兼容的外部资源和事件委托")
		}

		assetReq := httptest.NewRequest("GET", "/mail/view-assets.js", nil)
		assetRec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(assetRec, assetReq)
		if assetRec.Code != http.StatusOK || !strings.Contains(assetRec.Header().Get("Content-Type"), "javascript") || !strings.Contains(assetRec.Body.String(), "data-action") {
			t.Fatalf("邮件页脚本资源应公开可加载，实际 %d %s", assetRec.Code, assetRec.Header().Get("Content-Type"))
		}
	})

	t.Run("Mail View Cannot Override Token Account", func(t *testing.T) {
		for _, path := range []string{"/mail/view", "/mail/raw", "/mail/view", "/mail/raw"} {
			for _, parameter := range []string{"account_id", "account"} {
				req := httptest.NewRequest("GET", path+"?email="+testEmail+"&"+parameter+"=acc_other_2&token="+secretToken, nil)
				rec := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rec, req)
				if rec.Code != http.StatusNotFound {
					t.Fatalf("%s 普通令牌指定其他 %s 应返回 404, 实际 %d: %s", path, parameter, rec.Code, rec.Body.String())
				}
			}
		}
	})

	t.Run("Raw HTML Has IsolatedPolicy", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/mail/raw?email="+testEmail+"&format=html&token="+secretToken, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		policy := rec.Header().Get("Content-Security-Policy")
		if rec.Code != http.StatusOK || !strings.Contains(policy, "sandbox allow-popups;") ||
			!strings.Contains(policy, "script-src 'none'") || !strings.Contains(policy, "form-action 'none'") || strings.Contains(policy, "allow-same-origin") {
			t.Fatalf("raw HTML must be isolated and deny active content: status=%d policy=%q", rec.Code, policy)
		}
	})

	// ==========================================
	// 场景 4: /mail/raw?email=...&token=... (纯正文提取)
	// ==========================================
	t.Run("Mail Raw Content Direct Link", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/mail/raw?email="+testEmail+"&token="+secretToken, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("期望 200, 实际 %d: %s", rec.Code, rec.Body.String())
		}
		ct := rec.Header().Get("Content-Type")
		if !strings.Contains(ct, "text/plain") {
			t.Fatalf("期望 text/plain, 实际 %s", ct)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "654321") {
			t.Fatalf("纯文本应包含正文内容 654321, 实际: %s", body)
		}
	})

	// ==========================================
	// 场景 5: 无 Token 或 Token 无效时鉴权拦截
	// ==========================================
	t.Run("Direct Link Rejects Missing Or Invalid Token", func(t *testing.T) {
		// 无 Token
		req1 := httptest.NewRequest("GET", "/mail/view?email="+testEmail, nil)
		rec1 := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec1, req1)
		if rec1.Code != http.StatusUnauthorized {
			t.Fatalf("未提供 Token 应返回 401, 实际: %d", rec1.Code)
		}

		// 无效 Token
		req2 := httptest.NewRequest("GET", "/mail/view?email="+testEmail+"&token=invalid_token_123", nil)
		rec2 := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec2, req2)
		if rec2.Code != http.StatusUnauthorized {
			t.Fatalf("无效 Token 应返回 401, 实际: %d", rec2.Code)
		}
	})

	// ==========================================
	// 场景 6: 优雅短路径路由 /mail/view/:email
	// ==========================================
	t.Run("Mail View Clean Path Route", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/mail/view/"+testEmail+"?token="+secretToken, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("期望 200, 实际 %d: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), testEmail) {
			t.Fatalf("响应中应包含 %s", testEmail)
		}
		if !strings.Contains(rec.Body.String(), "pageFavicon") {
			t.Fatalf("页面应包含自定义矢量 SVG Favicon")
		}
	})
}

func TestMailViewDataAttributePreservesUntrustedBody(t *testing.T) {
	items := []mailViewItem{{TextBody: `"'><script>alert(1)</script><img src=x onerror=alert(1)> & \`, Subject: `</div><script>bad()</script>`}}
	encoded, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	var page bytes.Buffer
	if err := mailViewTemplate.Execute(&page, mailViewData{ItemsJSON: string(encoded), Items: items}); err != nil {
		t.Fatal(err)
	}
	tokens := html.NewTokenizer(&page)
	foundData, scripts := false, 0
	for {
		switch tokens.Next() {
		case html.ErrorToken:
			if tokens.Err() != io.EOF {
				t.Fatal(tokens.Err())
			}
			if !foundData || scripts != 1 {
				t.Fatalf("missing mailbox data or injected script: found=%v scripts=%d", foundData, scripts)
			}
			return
		case html.StartTagToken:
			token := tokens.Token()
			if token.Data == "script" {
				scripts++
			}
			attrs := map[string]string{}
			for _, attr := range token.Attr {
				attrs[attr.Key] = attr.Val
			}
			if attrs["id"] == "mailViewData" {
				var decoded []mailViewItem
				if err := json.Unmarshal([]byte(attrs["data-items"]), &decoded); err != nil {
					t.Fatal(err)
				}
				if len(decoded) != 1 || decoded[0] != items[0] {
					t.Fatal("HTML escaping changed the mailbox payload")
				}
				foundData = true
			}
		}
	}
}
