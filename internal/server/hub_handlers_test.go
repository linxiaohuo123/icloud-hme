/**
 * [INPUT]: 依赖 testing, net/http, net/http/httptest, icloud-hme/internal/account, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 TestHubHandlers 及其子测试集
 * [POS]: internal/server 的中台功能 (Tags, Tokens, Leases, Schedules) 集成与安全校验单元测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/store"
)

func TestHubHandlers(t *testing.T) {
	tempDir := t.TempDir()

	cfg := Config{
		DataDir:       tempDir,
		AdminPassword: "test-password",
		SessionTTL:    1 * time.Hour,
	}
	s := newWithBackend(&fakeBackend{accounts: []account.Summary{
		{ID: "acc_1", Name: "主账号", Status: "active"},
	}}, cfg)
	defer s.Close()

	// 登录获取 cookie 和 csrf_token
	loginBody, _ := json.Marshal(map[string]string{"password": "test-password"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/auth/login", bytes.NewReader(loginBody))
	s.Handler().ServeHTTP(w, req)
	cookie := w.Header().Get("Set-Cookie")

	var loginResp struct {
		Data struct {
			CSRFToken string `json:"csrf_token"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &loginResp)
	csrf := loginResp.Data.CSRFToken

	// 1. Tags
	tagBody, _ := json.Marshal(store.BusinessTag{Name: "测试业务", Tag: "test-tag"})
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/api/tags", bytes.NewReader(tagBody))
	req.Header.Set("Cookie", cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/tags failed: %d, body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api/tags", nil)
	req.Header.Set("Cookie", cookie)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/tags failed: %d", w.Code)
	}

	// 2. Tokens
	tokBody, _ := json.Marshal(map[string]string{"name": "外部测试脚本"})
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/api/tokens", bytes.NewReader(tokBody))
	req.Header.Set("Cookie", cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/tokens failed: %d", w.Code)
	}

	// 3. Leases
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api/leases", nil)
	req.Header.Set("Cookie", cookie)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/leases failed: %d", w.Code)
	}

	// 4. Schedules
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api/schedule/logs", nil)
	req.Header.Set("Cookie", cookie)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/schedule/logs failed: %d", w.Code)
	}

	// 5. Update Schedule Config 存在性校验
	// 5.1 不存在的账号更新调度应返回 404 NOT_FOUND
	schedBody, _ := json.Marshal(store.ScheduleConfig{Enabled: true, HourlyQuota: 5, AliasLabel: "auto-test"})
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("PUT", "/api/schedule/configs/non_existent_acc", bytes.NewReader(schedBody))
	req.Header.Set("Cookie", cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("PUT /api/schedule/configs/non_existent_acc 期望 404, 实际得到: %d", w.Code)
	}

	// 5.2 存在的账号更新调度应返回 200 OK
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("PUT", "/api/schedule/configs/acc_1", bytes.NewReader(schedBody))
	req.Header.Set("Cookie", cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT /api/schedule/configs/acc_1 期望 200, 实际得到: %d", w.Code)
	}

	// 6. scheduledTag 继承母号首个业务标签，无标签回退 scheduled
	if got := s.scheduledTag("acc_1"); got != "scheduled" {
		t.Fatalf("acc_1 无标签时期望 scheduled, 实际: %s", got)
	}
	s.be.(*fakeBackend).accounts = append(s.be.(*fakeBackend).accounts, account.Summary{
		ID:   "acc_vip",
		Name: "VIP号",
		Tags: []string{"vip", "asia"},
	})
	if got := s.scheduledTag("acc_vip"); got != "vip" {
		t.Fatalf("acc_vip 期望继承首个标签 vip, 实际: %s", got)
	}
}
