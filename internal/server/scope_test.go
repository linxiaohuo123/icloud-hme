/**
 * [INPUT]: 依赖 testing, net/http, net/http/httptest, encoding/json, icloud-hme/internal/account, icloud-hme/internal/store
 * [OUTPUT]: 对外提供令牌作用域最小权限、令牌不回显本体、分页上限与整数溢出守卫的回归测试
 * [POS]: internal/server 的鉴权作用域与入参边界回归测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"icloud-hme/internal/account"
	"icloud-hme/internal/store"
)

// newScopeTestServer 启动一个带两枚令牌(受限 / 管理员)的测试服务。
func newScopeTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	if err := st.SaveToken(store.APIToken{Name: "partner", Token: "scoped-token-aaaa", Scopes: store.DefaultExternalScopes}); err != nil {
		t.Fatalf("SaveToken(scoped) failed: %v", err)
	}
	if err := st.SaveToken(store.APIToken{Name: "ops", Token: "admin-token-bbbb", Scopes: store.ScopeAdmin}); err != nil {
		t.Fatalf("SaveToken(admin) failed: %v", err)
	}
	f := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Status: "active", HasCookies: true}},
	}
	s := newWithBackendAndStore(f, Config{AdminPassword: "admin-pass-2026-strong"}, st)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = st.Close()
	})
	return ts, st
}

func doGet(t *testing.T, ts *httptest.Server, path, token string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("X-API-Key", token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf strings.Builder
	dec := json.NewDecoder(resp.Body)
	var raw json.RawMessage
	_ = dec.Decode(&raw)
	buf.Write(raw)
	return resp, buf.String()
}

// 对外发放的受限令牌不得触达管理面，管理面只能由管理员会话/管理员作用域令牌访问。
func TestTokenScopesRestrictAdminSurface(t *testing.T) {
	ts, _ := newScopeTestServer(t)

	cases := []struct {
		path  string
		token string
		want  int
	}{
		// 受限令牌: 管理面一律 403
		{"/api/tokens", "scoped-token-aaaa", http.StatusForbidden},
		{"/api/accounts", "scoped-token-aaaa", http.StatusForbidden},
		{"/api/leases", "scoped-token-aaaa", http.StatusForbidden},
		{"/api/settings/notify", "scoped-token-aaaa", http.StatusForbidden},
		{"/api/aliases", "scoped-token-aaaa", http.StatusForbidden},
		// 管理员作用域令牌: 放行
		{"/api/tokens", "admin-token-bbbb", http.StatusOK},
		{"/api/accounts", "admin-token-bbbb", http.StatusOK},
		{"/api/leases", "admin-token-bbbb", http.StatusOK},
		// 无凭据: 401
		{"/api/tokens", "", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		resp, body := doGet(t, ts, tc.path, tc.token)
		if resp.StatusCode != tc.want {
			t.Fatalf("GET %s token=%q 期望 %d, 实际 %d, body=%s", tc.path, tc.token, tc.want, resp.StatusCode, body)
		}
	}
}

// 令牌列表只回显掩码，杜绝「一枚令牌收割全部令牌」的权限永久化链路。
func TestListTokensReturnsMaskedValue(t *testing.T) {
	ts, _ := newScopeTestServer(t)

	_, body := doGet(t, ts, "/api/tokens", "admin-token-bbbb")
	if strings.Contains(body, "scoped-token-aaaa") || strings.Contains(body, "admin-token-bbbb") {
		t.Fatalf("令牌列表不得回显令牌本体: %s", body)
	}
	if !strings.Contains(body, "****") {
		t.Fatalf("令牌列表应回显掩码: %s", body)
	}
}

// limit 超大值不得因 offset+limit 整数溢出触发切片越界 panic。
func TestListAccountsRejectsHugeLimit(t *testing.T) {
	ts, _ := newScopeTestServer(t)

	paths := []string{
		"/api/accounts?limit=9223372036854775807&offset=1",
		"/api/accounts?limit=999999999&offset=1",
		"/api/accounts?limit=-5&offset=-1",
	}
	for _, p := range paths {
		resp, body := doGet(t, ts, p, "admin-token-bbbb")
		if resp.StatusCode == http.StatusInternalServerError {
			t.Fatalf("GET %s 触发服务端错误(整数溢出/越界): %s", p, body)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s 期望 200, 实际 %d, body=%s", p, resp.StatusCode, body)
		}
	}
}

// 出站 Webhook 必须拒绝内网/环回/云元数据地址(盲 SSRF 防护)。
func TestNotifySettingsRejectsInternalWebhook(t *testing.T) {
	blocked := []string{
		"http://127.0.0.1:6379/",
		"http://localhost:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.5/hook",
		"http://192.168.1.1/hook",
		"http://[::1]:9000/hook",
	}
	for _, raw := range blocked {
		if err := validateOutboundPublicURL(raw, "飞书 Webhook"); err == nil {
			t.Fatalf("内网地址应被拒绝: %s", raw)
		}
	}
	allowed := []string{
		"",
		"https://open.feishu.cn/open-apis/bot/v2/hook/xxxx",
		"https://api.day.app/devicekey",
		"https://api.telegram.org",
	}
	for _, raw := range allowed {
		if err := validateOutboundPublicURL(raw, "飞书 Webhook"); err != nil {
			t.Fatalf("公网地址应放行: %s (%v)", raw, err)
		}
	}
}

// CSV 导出必须中和公式注入前缀。
func TestCSVSafeNeutralizesFormula(t *testing.T) {
	cases := map[string]string{
		"=cmd|'/C calc'!A0": "'=cmd|'/C calc'!A0",
		"+1+1":              "'+1+1",
		"-2+3":              "'-2+3",
		"@SUM(A1)":          "'@SUM(A1)",
		"normal-label":      "normal-label",
		"":                  "",
	}
	for in, want := range cases {
		if got := csvSafe(in); got != want {
			t.Fatalf("csvSafe(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

func doJSON(t *testing.T, ts *httptest.Server, method, path, token, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-API-Key", token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw json.RawMessage
	_ = json.NewDecoder(resp.Body).Decode(&raw)
	return resp, string(raw)
}

// 调度配置 PUT 必须是真 PATCH：只覆盖显式提交的字段，
// 且客户端提交的配额仲裁字段(current_hour_count/last_hour_window)必须被忽略。
func TestScheduleConfigPatchSemantics(t *testing.T) {
	ts, st := newScopeTestServer(t)

	// 1) 建立完整配置，并让服务端自行把小时计数推进到 3
	resp, body := doJSON(t, ts, http.MethodPut, "/api/schedule/configs/acc_1", "admin-token-bbbb",
		`{"enabled":true,"hourly_quota":10,"alias_label":"gpt","mode":"daily_window","start_time":"09:00","end_time":"18:00"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("建立调度配置失败: %d %s", resp.StatusCode, body)
	}
	// 客户端即使提交了配额仲裁字段也必须被忽略
	resp, body = doJSON(t, ts, http.MethodPut, "/api/schedule/configs/acc_1", "admin-token-bbbb",
		`{"enabled":true,"hourly_quota":10,"alias_label":"gpt","mode":"daily_window","start_time":"09:00","end_time":"18:00","current_hour_count":9999,"last_hour_window":12345}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("写入完整配置失败: %d %s", resp.StatusCode, body)
	}
	if ok, _ := st.TryReserveQuota("acc_1", 3); !ok {
		t.Fatal("预留 3 个配额应成功")
	}
	if got := st.RemainingQuota("acc_1"); got != 7 {
		t.Fatalf("预留后剩余配额应为 7, 实际 %d", got)
	}

	// 2) 只改配额:其余字段必须原样保留
	resp, body = doJSON(t, ts, http.MethodPut, "/api/schedule/configs/acc_1", "admin-token-bbbb",
		`{"hourly_quota":50}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("部分更新失败: %d %s", resp.StatusCode, body)
	}
	cfg := st.GetScheduleConfig("acc_1")
	if !cfg.Enabled {
		t.Fatal("只改配额不应把定时任务静默关闭")
	}
	if cfg.Mode != "daily_window" || cfg.AliasLabel != "gpt" || cfg.StartTime != "09:00" || cfg.EndTime != "18:00" {
		t.Fatalf("只改配额不应清空运行模式与时间窗口: %+v", cfg)
	}
	if cfg.HourlyQuota != 50 {
		t.Fatalf("配额应更新为 50, 实际 %d", cfg.HourlyQuota)
	}

	// 3) 已完成的小时计数不得被陈旧快照回退(否则会绕过小时限流超额建号)
	if got := st.RemainingQuota("acc_1"); got != 47 {
		t.Fatalf("小时计数被回退: 期望剩余 47 (50-3), 实际 %d", got)
	}
}

// run-now 默认只跑已启用任务；?all=true 才强推全部账号。
func TestRunNowScope(t *testing.T) {
	ts, _ := newScopeTestServer(t)

	resp, body := doJSON(t, ts, http.MethodPost, "/api/schedule/run-now", "admin-token-bbbb", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("run-now 失败: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "enabled_only") {
		t.Fatalf("缺省 run-now 应只覆盖已启用任务: %s", body)
	}

	resp, body = doJSON(t, ts, http.MethodPost, "/api/schedule/run-now?all=true", "admin-token-bbbb", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("run-now?all=true 失败: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "all_accounts") {
		t.Fatalf("all=true 应对全部账号强推: %s", body)
	}
}

// 默认配置(TrustedProxies 为空)下，限流必须基于真实连接地址:
// 轮换 X-Forwarded-For 不得让攻击者绕过 15 分钟 / 5 次的登录失败限流。
// 该行为依赖 SetTrustedProxies(nil)，一旦被改成信任任意代理头即失守。
func TestSpoofedForwardedForCannotBypassLoginLimit(t *testing.T) {
	f := &fakeBackend{}
	s := newWithBackend(f, Config{AdminPassword: "admin-pass-2026-strong"})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	post := func(xff string) int {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/auth/login",
			strings.NewReader(`{"password":"definitely-wrong"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// 前 5 次允许尝试(凭据错误 → 401)，每次都换一个伪造来源
	for i := 0; i < 5; i++ {
		if code := post("10.0.0." + string(rune('1'+i))); code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次错误口令应返回 401, 实际 %d", i+1, code)
		}
	}
	// 第 6 次即使再次更换来源，也必须已被限流
	if code := post("203.0.113.77"); code != http.StatusTooManyRequests {
		t.Fatalf("轮换 X-Forwarded-For 绕过了登录限流: 第 6 次返回 %d, 期望 429", code)
	}
}
