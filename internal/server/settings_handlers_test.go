package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newSettingsTestServer 构造带临时数据目录的测试服务。
func newSettingsTestServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	s := newWithBackend(&fakeBackend{}, Config{
		DataDir:       t.TempDir(),
		AdminPassword: "admin-pass-2026-strong",
	})
	t.Cleanup(s.Close) // 释放 SQLite 连接, 否则 TempDir 清理失败
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, s
}

// TestNotifySettingsRoundtrip 校验通知配置的保存与回读。
func TestNotifySettingsRoundtrip(t *testing.T) {
	ts, s := newSettingsTestServer(t)
	sess, csrf := login(t, ts, "admin-pass-2026-strong")

	// 初始为默认配置
	req := authedReq(t, ts, "GET", "/api/settings/notify", "")
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	status, body, _ := do(t, req)
	if status != http.StatusOK {
		t.Fatalf("读取配置失败: %d %s", status, body)
	}
	if !containsString(body, `"event_kinds"`) {
		t.Fatalf("默认配置应含事件开关: %s", body)
	}

	// 保存配置
	payload := `{
		"feishu_webhook": "https://open.feishu.cn/open-apis/bot/v2/hook/xxx",
		"bark_url": "",
		"telegram_token": "tok",
		"telegram_chat": "42",
		"event_kinds": {"cookie_expired": true, "cookie_recovered": false, "quota_low": true},
		"quota_threshold": 700
	}`
	req = authedReq(t, ts, "PUT", "/api/settings/notify", payload)
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	req.Header.Set("X-CSRF-Token", csrf)
	status, body, _ = do(t, req)
	if status != http.StatusOK {
		t.Fatalf("保存配置失败: %d %s", status, body)
	}

	// 回读并断言
	req = authedReq(t, ts, "GET", "/api/settings/notify", "")
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	status, body, _ = do(t, req)
	if status != http.StatusOK {
		t.Fatalf("回读配置失败: %d %s", status, body)
	}
	var out struct {
		Data struct {
			FeishuWebhook  string          `json:"feishu_webhook"`
			TelegramChat   string          `json:"telegram_chat"`
			QuotaThreshold int             `json:"quota_threshold"`
			EventKinds     map[string]bool `json:"event_kinds"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("解析回读响应失败: %v", err)
	}
	if !strings.Contains(out.Data.FeishuWebhook, "********") || strings.Contains(out.Data.FeishuWebhook, "xxx") ||
		out.Data.QuotaThreshold != 700 || out.Data.TelegramChat != "42" {
		t.Fatalf("回读配置不符(应返回脱敏值): %+v", out.Data)
	}
	if out.Data.EventKinds["cookie_recovered"] {
		t.Fatal("cookie_recovered 应为关闭")
	}

	// 重启后配置仍在(从 store 加载)
	loaded, err := s.loadNotifySettings()
	if err != nil {
		t.Fatalf("loadNotifySettings 失败: %v", err)
	}
	if got := loaded.FeishuWebhook; got != "https://open.feishu.cn/open-apis/bot/v2/hook/xxx" {
		t.Fatalf("store 持久化配置不符: %q", got)
	}
}

// TestNotifySettingsValidation 校验非法配置被拒绝。
func TestNotifySettingsValidation(t *testing.T) {
	ts, _ := newSettingsTestServer(t)
	sess, csrf := login(t, ts, "admin-pass-2026-strong")

	cases := []struct {
		name   string
		body   string
		reason string
	}{
		{"飞书URL缺协议", `{"feishu_webhook":"open.feishu.cn/hook"}`, "http"},
		{"BarkURL缺协议", `{"bark_url":"api.day.app/key"}`, "http"},
		{"Telegram缺ChatID", `{"telegram_token":"tok"}`, "同时填写"},
		{"阈值越界", `{"quota_threshold":9999}`, "0-2000"},
		{"未知事件类型", `{"event_kinds":{"nope":true}}`, "未知的事件类型"},
	}
	for _, tc := range cases {
		req := authedReq(t, ts, "PUT", "/api/settings/notify", tc.body)
		req.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
		req.Header.Set("X-CSRF-Token", csrf)
		status, body, _ := do(t, req)
		if status != http.StatusBadRequest {
			t.Fatalf("%s: 期望 400, 得到 %d: %s", tc.name, status, body)
		}
		if !containsString(body, tc.reason) {
			t.Fatalf("%s: 错误信息应含 %q: %s", tc.name, tc.reason, body)
		}
	}
}

// TestNotifyTestEndpoint 校验测试推送端点返回逐渠道结果。
func TestNotifyTestEndpoint(t *testing.T) {
	ts, _ := newSettingsTestServer(t)
	sess, csrf := login(t, ts, "admin-pass-2026-strong")
	req := authedReq(t, ts, "POST", "/api/settings/notify/test", "")
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	req.Header.Set("X-CSRF-Token", csrf)
	status, body, _ := do(t, req)
	if status != http.StatusOK {
		t.Fatalf("测试推送失败: %d %s", status, body)
	}
	if !containsString(body, "results") {
		t.Fatalf("响应应含 results: %s", body)
	}
}

// TestRequireSessionGuardsSettings 校验设置接口受会话保护。
func TestRequireSessionGuardsSettings(t *testing.T) {
	ts, _ := newSettingsTestServer(t)
	status, _, _ := do(t, authedReq(t, ts, "GET", "/api/settings/notify", ""))
	if status != http.StatusUnauthorized {
		t.Fatalf("未登录读取设置应 401, 得到 %d", status)
	}
	status, _, _ = do(t, authedReq(t, ts, "PUT", "/api/settings/notify", "{}"))
	if status != http.StatusUnauthorized {
		t.Fatalf("未登录保存设置应 401, 得到 %d", status)
	}
}

func containsString(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
