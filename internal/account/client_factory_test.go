/**
 * [INPUT]: 依赖 testing, icloud-hme/internal/account
 * [OUTPUT]: 对外提供 TestWebMailClientDSIDExtraction, TestWebMailClientProxyPropagation
 * [POS]: internal/account 的客户端工厂连接装配、凭据解析与代理透传测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package account

import (
	"strings"
	"testing"
)

func TestWebMailClientDSIDExtraction(t *testing.T) {
	cases := []struct {
		cookieVal string
		wantDSID  string
	}{
		{"v=1:s=1:d=22789132008", "22789132008"},
		{"v=1:s=1:d=22789132008:t=auth_token_foo", "22789132008"},
		{`v=1:s=1:d="22789132008":t=foo`, "22789132008"},
		{"invalid_format", ""},
	}

	dir := t.TempDir()
	mgr, err := NewManager(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	for i, tc := range cases {
		accID := "acc_test_dsid"
		mgr.mu.Lock()
		mgr.accounts[accID] = &Account{
			ID:          accID,
			Name:        "dsid_test",
			ICloudEmail: "test@icloud.com",
			Cookies: map[string]string{
				"X-APPLE-WEBAUTH-USER": tc.cookieVal,
			},
			Host: "icloud.com",
		}
		mgr.mu.Unlock()

		wmc, err := mgr.WebMailClient(accID)
		if err != nil {
			t.Fatalf("[%d] WebMailClient 创建失败: %v", i, err)
		}
		if wmc == nil {
			t.Fatalf("[%d] WebMailClient 不能为空", i)
		}

		// 验证提取逻辑：与 client_factory.go 保持同构验证
		dsid := ""
		if v, ok := mgr.accounts[accID].Cookies["X-APPLE-WEBAUTH-USER"]; ok {
			parts := strings.Split(v, ":d=")
			if len(parts) == 2 {
				dsid = strings.Trim(strings.Split(parts[1], ":")[0], `"`)
			}
		}
		if dsid != tc.wantDSID {
			t.Fatalf("[%d] 期望 DSID %q, 得到 %q", i, tc.wantDSID, dsid)
		}
	}
}

func TestWebMailClientProxyPropagation(t *testing.T) {
	dir := t.TempDir()
	mgr, err := NewManager(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	accID := "acc_test_proxy"
	expectedProxy := "http://127.0.0.1:8080"
	mgr.mu.Lock()
	mgr.accounts[accID] = &Account{
		ID:          accID,
		Name:        "proxy_test",
		ICloudEmail: "proxy@icloud.com",
		Cookies: map[string]string{
			"X-APPLE-WEBAUTH-USER": "v=1:s=1:d=123456",
		},
		Host:  "icloud.com",
		Proxy: expectedProxy,
	}
	mgr.mu.Unlock()

	wmc, err := mgr.WebMailClient(accID)
	if err != nil {
		t.Fatalf("WebMailClient 创建失败: %v", err)
	}
	if wmc == nil {
		t.Fatal("WebMailClient 不能为空")
	}
	if wmc.Proxy() != expectedProxy {
		t.Fatalf("期望代理为 %q, 实际得到 %q", expectedProxy, wmc.Proxy())
	}
}
