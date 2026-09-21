package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

// TestAPIKeyBypassesSessionAndCSRF 验证 API Key 可以完全绕过 Session 和 CSRF 检查
func TestAPIKeyBypassesSessionAndCSRF(t *testing.T) {
	f := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_1", Status: "active", HasCookies: true},
		},
		created: &hme.CreateResult{
			Email: "test-alias@icloud.com",
			Label: "bot",
		},
	}
	cfg := Config{
		Debug:         false,
		AdminPassword: "admin-pass-2026-strong",
		APIKey:        "my-secret-api-key",
		SessionTTL:    12 * time.Hour,
	}
	s := newWithBackend(f, cfg)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// 1. 使用 X-API-Key 调用 POST /api/quick-create (无 Session, 无 CSRF)
	req, _ := http.NewRequest("POST", ts.URL+"/api/quick-create", strings.NewReader(`{"label":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "my-secret-api-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("API Key 请求应返回 200, 实际得到: %d", resp.StatusCode)
	}

	var out struct {
		Success bool `json:"success"`
		Data    struct {
			Email string `json:"email"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Data.Email != "test-alias@icloud.com" {
		t.Fatalf("期望 email=test-alias@icloud.com, 得到 %s", out.Data.Email)
	}

	// 2. 使用 Bearer Token 调用 GET /api/accounts
	req2, _ := http.NewRequest("GET", ts.URL+"/api/accounts", nil)
	req2.Header.Set("Authorization", "Bearer my-secret-api-key")

	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("Bearer Token 请求应返回 200, 实际得到: %d", resp2.StatusCode)
	}

	// 3. 使用错误 API Key 且无 Session -> 401
	req3, _ := http.NewRequest("GET", ts.URL+"/api/accounts", nil)
	req3.Header.Set("X-API-Key", "wrong-key")

	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()

	if resp3.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误 API Key 期望 401, 实际得到: %d", resp3.StatusCode)
	}
}

// TestVerifyCodeHandler 验证验证码长轮询接口 (EventBus 缓存命中与通道唤醒)
func TestVerifyCodeHandler(t *testing.T) {
	f := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_1", Status: "active", HasCookies: true},
		},
		inbox: InboxResult{
			AccountID: "acc_1",
			Alias:     "target@icloud.com",
			Count:     1,
			Messages: []mail.Message{
				{
					ID:      "1",
					To:      "target@icloud.com",
					Subject: "Your verification code",
					Preview: "Your code is: 582910. Do not share it.",
					From:    "auth@example.com",
				},
			},
		},
	}
	cfg := Config{
		Debug:         false,
		AdminPassword: "admin-pass-2026-strong",
		APIKey:        "test-key",
	}
	s := newWithBackend(f, cfg)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// 启动后台拉信协程
	s.syncWorker.Start()
	defer s.syncWorker.Stop()

	// 发起验证码长轮询
	req, _ := http.NewRequest("GET", ts.URL+"/api/verify-code?email=target@icloud.com&timeout=3", nil)
	req.Header.Set("X-API-Key", "test-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("verify-code 期望 200, 实际得到: %d", resp.StatusCode)
	}

	var out struct {
		Success bool `json:"success"`
		Data    struct {
			Code  string `json:"code"`
			Email string `json:"email"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}

	if out.Data.Code != "582910" {
		t.Fatalf("期望提取验证码 582910, 得到: %s", out.Data.Code)
	}
}

// TestExternalAllocateAndVerifyRoutes 验证标准外部 Headless API 门面路由、标签号池亲和性与 Token 审计
func TestExternalAllocateAndVerifyRoutes(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 保存一个外部 Token
	err = st.SaveToken(store.APIToken{
		Name:  "faka_bot_01",
		Token: "faka-token-secret-12345",
	})
	if err != nil {
		t.Fatal(err)
	}

	f := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_steam", Status: "active", HasCookies: true, Tags: []string{"steam"}},
			{ID: "acc_chatgpt", Status: "active", HasCookies: true, Tags: []string{"chatgpt"}},
			{ID: "acc_general", Status: "active", HasCookies: true},
		},
		created: &hme.CreateResult{
			Email: "chatgpt-alias@icloud.com",
			Label: "chatgpt-reg",
		},
	}
	cfg := Config{
		Debug:         false,
		AdminPassword: "admin-pass-2026-strong",
	}
	s := newWithBackendAndStore(f, cfg, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// 1. 测试 POST /api/allocate 携 Bearer Token 与 tag=chatgpt
	req, _ := http.NewRequest("POST", ts.URL+"/api/allocate", strings.NewReader(`{"tag":"chatgpt","label":"chatgpt-reg"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer faka-token-secret-12345")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/allocate 期望 200, 实际得到: %d", resp.StatusCode)
	}

	var out struct {
		Success bool `json:"success"`
		Data    struct {
			Email     string `json:"email"`
			AccountID string `json:"account_id"`
			Tag       string `json:"tag"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	// 验证标签亲和性：必须分配到 acc_chatgpt，绝不可分配到 acc_steam
	if out.Data.AccountID != "acc_chatgpt" {
		t.Fatalf("标签亲和性失败: 期望分配 acc_chatgpt, 实际分配: %s", out.Data.AccountID)
	}

	// 2. 验证流水中正确记录了 TokenName
	leases, total := st.ListLeases("", "chatgpt", "", 10, 0)
	if total == 0 || len(leases) == 0 {
		t.Fatalf("期望记录出号流水，实际未找到")
	}
	if leases[0].TokenName != "faka_bot_01" {
		t.Fatalf("期望 TokenName=faka_bot_01, 实际得到: %s", leases[0].TokenName)
	}

	// 3. 测试 POST /api/external/v1/allocate
	req2, _ := http.NewRequest("POST", ts.URL+"/api/external/v1/allocate", strings.NewReader(`{"tag":"chatgpt","label":"bot2"}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer faka-token-secret-12345")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/external/v1/allocate 期望 200, 实际得到: %d", resp2.StatusCode)
	}

	// 4. 测试 GET /api/external/v1/verify-code (使用 API Key)
	req3, _ := http.NewRequest("GET", ts.URL+"/api/external/v1/verify-code?email=target@icloud.com&timeout=1", nil)
	req3.Header.Set("Authorization", "Bearer faka-token-secret-12345")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	// 超时期望 408，说明路由正确挂载且鉴权通过进入等待
	if resp3.StatusCode != http.StatusRequestTimeout && resp3.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/external/v1/verify-code 期望 408 或 200, 实际得到: %d", resp3.StatusCode)
	}
}
