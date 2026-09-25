/**
 * [INPUT]: 依赖 encoding/json, net/http, net/http/httptest, strings, testing, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 PR-07 通知安全测试套件：GET 脱敏掩码、PUT 脱敏值 Preserve 契约、PATCH 部分更新与显式置空
 * [POS]: internal/server 的 PR-07 通知凭据加密与脱敏回归测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"icloud-hme/internal/store"
)

func newPR07NotifyServer(t *testing.T) (*httptest.Server, *Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	fb := &fakeBackend{}
	s := newWithBackendAndStore(fb, Config{AdminPassword: "admin-pass-2026-strong"}, st)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = st.Close()
	})
	return ts, s, st
}

// TestPR07_NotifySecretsMaskedOnGet 验证敏感通知凭据在 GET 时必须返回掩码脱敏，绝不泄露明文。
func TestPR07_NotifySecretsMaskedOnGet(t *testing.T) {
	ts, _, _ := newPR07NotifyServer(t)
	sess, csrf := login(t, ts, "admin-pass-2026-strong")

	feishuSecret := "my-feishu-webhook-secret-token-abcdef"
	telegramSecret := "987654321:AAE_secret_telegram_bot_token_pr07"
	barkSecret := "my-bark-device-secret-key-12345"

	// 1. PUT 写入明文 Secret
	payload := map[string]interface{}{
		"feishu_webhook":  "https://open.feishu.cn/open-apis/bot/v2/hook/" + feishuSecret,
		"telegram_token":  telegramSecret,
		"telegram_chat":   "10086",
		"bark_url":        "https://api.day.app/" + barkSecret + "/",
		"quota_threshold": 500,
	}
	bodyBytes, _ := json.Marshal(payload)
	reqPut := authedReq(t, ts, "PUT", "/api/settings/notify", string(bodyBytes))
	reqPut.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	reqPut.Header.Set("X-CSRF-Token", csrf)

	status, body, _ := do(t, reqPut)
	if status != http.StatusOK {
		t.Fatalf("PUT /api/settings/notify 失败: %d %s", status, body)
	}

	// 2. GET 读取配置
	reqGet := authedReq(t, ts, "GET", "/api/settings/notify", "")
	reqGet.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	statusGet, bodyGet, _ := do(t, reqGet)
	if statusGet != http.StatusOK {
		t.Fatalf("GET /api/settings/notify 失败: %d %s", statusGet, bodyGet)
	}

	// 检查是否泄露真实 Secret
	if strings.Contains(bodyGet, feishuSecret) {
		t.Fatalf("【安全红线踩雷】GET /api/settings/notify 泄露了飞书 Secret: %s", bodyGet)
	}
	if strings.Contains(bodyGet, telegramSecret) {
		t.Fatalf("【安全红线踩雷】GET /api/settings/notify 泄露了 Telegram Token: %s", bodyGet)
	}
	if strings.Contains(bodyGet, barkSecret) {
		t.Fatalf("【安全红线踩雷】GET /api/settings/notify 泄露了 Bark Key: %s", bodyGet)
	}

	// 检查必须包含脱敏掩码
	if !strings.Contains(bodyGet, "********") {
		t.Fatalf("GET /api/settings/notify 响应中未包含脱敏掩码: %s", bodyGet)
	}
}

// TestPR07_NotifySecretsPreserveContract 验证前端将脱敏掩码传回时，
// 服务端 Preserve 合约生效，绝对不得将脱敏掩码当作新密钥覆写数据库。
func TestPR07_NotifySecretsPreserveContract(t *testing.T) {
	ts, s, _ := newPR07NotifyServer(t)
	sess, csrf := login(t, ts, "admin-pass-2026-strong")

	feishuSecret := "feishu_real_secret_token_112233"
	telegramSecret := "123456789:telegram_real_secret_token_445566"

	// 1. 初始化设置
	payloadInit := map[string]interface{}{
		"feishu_webhook":  "https://open.feishu.cn/open-apis/bot/v2/hook/" + feishuSecret,
		"telegram_token":  telegramSecret,
		"telegram_chat":   "12345",
		"quota_threshold": 500,
	}
	bodyInit, _ := json.Marshal(payloadInit)
	reqPut := authedReq(t, ts, "PUT", "/api/settings/notify", string(bodyInit))
	reqPut.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	reqPut.Header.Set("X-CSRF-Token", csrf)
	_, _, _ = do(t, reqPut)

	// 2. GET 获取脱敏配置
	reqGet := authedReq(t, ts, "GET", "/api/settings/notify", "")
	reqGet.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	_, bodyGet, _ := do(t, reqGet)

	var getRes struct {
		Data struct {
			FeishuWebhook  string `json:"feishu_webhook"`
			TelegramToken  string `json:"telegram_token"`
			TelegramChat   string `json:"telegram_chat"`
			QuotaThreshold int    `json:"quota_threshold"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(bodyGet), &getRes)

	// 3. 模拟前端修改了 quota_threshold，把含脱敏值的表单原样 PUT 提交
	getRes.Data.QuotaThreshold = 888
	bodyUpdate, _ := json.Marshal(getRes.Data)

	reqUpdate := authedReq(t, ts, "PUT", "/api/settings/notify", string(bodyUpdate))
	reqUpdate.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	reqUpdate.Header.Set("X-CSRF-Token", csrf)
	statusUp, bodyUp, _ := do(t, reqUpdate)
	if statusUp != http.StatusOK {
		t.Fatalf("PUT 回写失败: %d %s", statusUp, bodyUp)
	}

	// 4. 从底层服务读取持久化的真实配置，断言真实密钥被安全 Preserve，未被星号破坏
	loaded, err := s.loadNotifySettings()
	if err != nil {
		t.Fatalf("loadNotifySettings failed: %v", err)
	}

	expectedHook := "https://open.feishu.cn/open-apis/bot/v2/hook/" + feishuSecret
	if loaded.FeishuWebhook != expectedHook {
		t.Fatalf("【严重缺陷】Preserve 合约失效，飞书 Webhook 被破坏: 期望 %q, 实际 %q", expectedHook, loaded.FeishuWebhook)
	}
	if loaded.TelegramToken != telegramSecret {
		t.Fatalf("【严重缺陷】Preserve 合约失效，Telegram Token 被破坏: 期望 %q, 实际 %q", telegramSecret, loaded.TelegramToken)
	}
	if loaded.QuotaThreshold != 888 {
		t.Fatalf("QuotaThreshold 未更新成功: %d", loaded.QuotaThreshold)
	}
}

// TestPR07_NotifySecretsPatchPreserveAndExplicitClear 验证 PATCH 部分更新与显式清空凭据。
func TestPR07_NotifySecretsPatchPreserveAndExplicitClear(t *testing.T) {
	ts, s, _ := newPR07NotifyServer(t)
	sess, csrf := login(t, ts, "admin-pass-2026-strong")

	// 1. 初始化设置含有飞书与 Bark
	payloadInit := map[string]interface{}{
		"feishu_webhook": "https://open.feishu.cn/open-apis/bot/v2/hook/feishu_keep_123",
		"bark_url":       "https://api.day.app/bark_keep_456/",
	}
	bodyInit, _ := json.Marshal(payloadInit)
	reqInit := authedReq(t, ts, "PUT", "/api/settings/notify", string(bodyInit))
	reqInit.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	reqInit.Header.Set("X-CSRF-Token", csrf)
	_, _, _ = do(t, reqInit)

	// 2. PATCH 只更新 quota_threshold，省略凭据字段
	reqPatch1 := authedReq(t, ts, "PATCH", "/api/settings/notify", `{"quota_threshold": 999}`)
	reqPatch1.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	reqPatch1.Header.Set("X-CSRF-Token", csrf)
	statusP1, bodyP1, _ := do(t, reqPatch1)
	if statusP1 != http.StatusOK {
		t.Fatalf("PATCH 1 失败: %d %s", statusP1, bodyP1)
	}

	loaded1, err := s.loadNotifySettings()
	if err != nil {
		t.Fatalf("loadNotifySettings 1 failed: %v", err)
	}
	if loaded1.FeishuWebhook != "https://open.feishu.cn/open-apis/bot/v2/hook/feishu_keep_123" {
		t.Fatalf("PATCH 省略凭据字段时不应清空飞书凭据: %s", loaded1.FeishuWebhook)
	}
	if loaded1.BarkURL != "https://api.day.app/bark_keep_456/" {
		t.Fatalf("PATCH 省略凭据字段时不应清空 Bark 凭据: %s", loaded1.BarkURL)
	}
	if loaded1.QuotaThreshold != 999 {
		t.Fatalf("PATCH 更新 quota_threshold 失败: %d", loaded1.QuotaThreshold)
	}

	// 3. 显式清空飞书 Webhook: 传空字符串
	reqClear := authedReq(t, ts, "PATCH", "/api/settings/notify", `{"feishu_webhook": ""}`)
	reqClear.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	reqClear.Header.Set("X-CSRF-Token", csrf)
	statusClr, bodyClr, _ := do(t, reqClear)
	if statusClr != http.StatusOK {
		t.Fatalf("PATCH clear 失败: %d %s", statusClr, bodyClr)
	}

	loaded2, err := s.loadNotifySettings()
	if err != nil {
		t.Fatalf("loadNotifySettings 2 failed: %v", err)
	}
	if loaded2.FeishuWebhook != "" {
		t.Fatalf("显式清空飞书后应为空，实际为 %q", loaded2.FeishuWebhook)
	}
	// BarkURL 未被显式清空，应继续保持
	if loaded2.BarkURL != "https://api.day.app/bark_keep_456/" {
		t.Fatalf("显式清空飞书不应波及 BarkURL: %s", loaded2.BarkURL)
	}
}
