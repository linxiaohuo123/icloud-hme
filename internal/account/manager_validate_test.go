package account

import (
	"errors"
	"strings"
	"testing"
)

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
