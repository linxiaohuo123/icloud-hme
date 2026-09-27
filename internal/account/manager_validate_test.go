package account

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"icloud-hme/internal/store"
)

func TestUpdateCookiesReturnsValidationFailure(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "proxy denied", http.StatusForbidden)
	}))
	defer proxy.Close()

	m, err := NewManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	acc, err := m.AddAccount("test", "", "icloud.com", proxy.URL)
	if err != nil {
		t.Fatal(err)
	}

	err = m.UpdateCookies(acc.ID, map[string]string{"session": "invalid"})
	if err == nil {
		t.Fatal("Cookie validation failure must be returned to the API")
	}
	saved, ok := m.GetAccount(acc.ID)
	if !ok || saved.Status != "error" || saved.LastError == "" {
		t.Fatalf("failed account status must be retained: %+v", saved)
	}
}

func TestUpdateCookiesRestoresMemoryOnPersistenceFailure(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "proxy denied", http.StatusForbidden)
	}))
	defer proxy.Close()
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(t.TempDir(), st)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	acc, err := m.AddAccount("test", "", "icloud.com", proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := m.GetAccount(acc.ID)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateCookies(acc.ID, map[string]string{"session": "new"}); err == nil {
		t.Fatal("保存失败应返回错误")
	}
	after, _ := m.GetAccount(acc.ID)
	if after.Status != before.Status || after.LastError != before.LastError || len(after.Cookies) != len(before.Cookies) {
		t.Fatalf("保存失败后应恢复旧账号: before=%+v after=%+v", before, after)
	}
}

// TestValidateAccountGuards 校验 ValidateAccount 的无网络守护路径:
// 账号不存在与未配置 Cookie 都直接报错，不触发任何上游请求。
func TestValidateAccountGuards(t *testing.T) {
	m, err := NewManager(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("创建管理器失败: %v", err)
	}
	defer m.Close()

	if _, err := m.AddAccountWithInput(AddAccountInput{Name: "待配置号", ICloudEmail: "owner@icloud.com"}); err != nil {
		t.Fatalf("添加账号失败: %v", err)
	}

	if err := m.ValidateAccount("acc_missing"); err == nil || !strings.Contains(err.Error(), "账号不存在") {
		t.Fatalf("不存在的账号应报账号不存在, 实际: %v", err)
	}

	sums := m.ListSummaries()
	if len(sums) != 1 {
		t.Fatalf("应有 1 个账号, 实际 %d", len(sums))
	}
	if err := m.ValidateAccount(sums[0].ID); err == nil || !strings.Contains(err.Error(), "未配置 Cookie") {
		t.Fatalf("无 Cookie 账号应报未配置 Cookie, 实际: %v", err)
	}
}

// TestIsAuthFailure 校验凭据级失效的判定只认 HTTP 401/403。
func TestIsAuthFailure(t *testing.T) {
	cases := map[string]bool{
		"HTTP 401: unauthorized":         true,
		"HTTP 403: forbidden":            true,
		"连接失败: dial tcp: i/o timeout":    false,
		"validate 响应缺少 Hide My Email 端点": false,
	}
	for msg, want := range cases {
		if got := isAuthFailure(msg); got != want {
			t.Fatalf("isAuthFailure(%q) = %v, 期望 %v", msg, got, want)
		}
	}
}

// TestErrCookieExpiredSentinel 校验哨兵错误可被 errors.Is 识别。
func TestErrCookieExpiredSentinel(t *testing.T) {
	wrapped := errors.Join(ErrCookieExpired)
	if !errors.Is(wrapped, ErrCookieExpired) {
		t.Fatal("ErrCookieExpired 应可被 errors.Is 识别")
	}
}
