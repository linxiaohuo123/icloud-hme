/**
 * [INPUT]: 依赖 testing, net/http/httptest, internal/hme, internal/store
 * [OUTPUT]: 验证账号身份、DSID 持久化、凭据代际与 Cookie 校验状态
 * [POS]: internal/account 的会话与身份回归测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package account

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"icloud-hme/internal/hme"
	"icloud-hme/internal/store"
)

func TestVerifyAppleIdentity(t *testing.T) {
	tests := []struct {
		name         string
		acc          Account
		info         *hme.AccountInfo
		wantMismatch bool
	}{
		{"same DSID", Account{AppleDSID: "123"}, &hme.AccountInfo{DSID: "123", AppleID: "renamed@example.com"}, false},
		{"different DSID", Account{AppleDSID: "123"}, &hme.AccountInfo{DSID: "456", AppleID: "owner@example.com"}, true},
		{"missing DSID", Account{AppleDSID: "123"}, &hme.AccountInfo{AppleID: "owner@example.com"}, true},
		{"legacy same email", Account{RealEmail: "Owner@Example.com", LastValidated: "2026-01-01"}, &hme.AccountInfo{AppleID: "owner@example.com"}, false},
		{"legacy different email", Account{RealEmail: "owner@example.com", AliasTotal: 1}, &hme.AccountInfo{AppleID: "other@example.com"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyAppleIdentity(&tt.acc, tt.info)
			if got := errors.Is(err, ErrAccountIdentityMismatch); got != tt.wantMismatch {
				t.Fatalf("mismatch=%v, want %v: %v", got, tt.wantMismatch, err)
			}
		})
	}
}

func TestAppleDSIDPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	st, err := store.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(dir, st)
	if err != nil {
		t.Fatal(err)
	}
	acc, err := m.AddAccount("owner", "", "icloud.com", "")
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.accounts[acc.ID].AppleDSID = "123456"
	err = m.saveAccount(m.accounts[acc.ID])
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m, err = NewManager(dir, st)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	loaded, ok := m.GetAccount(acc.ID)
	if !ok || loaded.AppleDSID != "123456" {
		t.Fatalf("Apple DSID lost after restart: %+v", loaded)
	}
	if err := verifyAppleIdentity(loaded, &hme.AccountInfo{DSID: "654321"}); !errors.Is(err, ErrAccountIdentityMismatch) {
		t.Fatalf("different Apple identity accepted after restart: %v", err)
	}
}

func TestOldCredentialEpochCannotOverwriteAliasCounts(t *testing.T) {
	m, err := NewManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	acc, err := m.AddAccount("owner", "", "icloud.com", "")
	if err != nil {
		t.Fatal(err)
	}
	epoch, _ := m.CredentialEpoch(acc.ID)
	if err := m.SaveSession(acc.ID, map[string]string{"session": "new"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateAliasCounts(acc.ID, 8, 7); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateAliasCountsIfEpoch(acc.ID, epoch, 1, 1); !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("late count update must be rejected: %v", err)
	}
	loaded, _ := m.GetAccount(acc.ID)
	if loaded.AliasTotal != 8 || loaded.AliasActive != 7 {
		t.Fatalf("late update changed new counts: total=%d active=%d", loaded.AliasTotal, loaded.AliasActive)
	}
}

func TestUnverifiedCookieCannotRunHMEOperationForBoundAccount(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer proxy.Close()
	m, err := NewManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	acc, err := m.AddAccount("owner", "", "icloud.com", proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	bound := m.accounts[acc.ID]
	bound.AppleDSID = "123456"
	bound.Cookies = map[string]string{"session": "unverified"}
	bound.Status = "error"
	err = m.saveAccount(bound)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	err = m.WithHMEClient(acc.ID, func(*hme.Client) error { ran = true; return nil })
	if err == nil || ran {
		t.Fatalf("unverified cookie reached HME operation: ran=%v err=%v", ran, err)
	}
}

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
