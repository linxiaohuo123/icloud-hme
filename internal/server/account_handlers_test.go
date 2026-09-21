/**
 * [INPUT]: 依赖 encoding/json, io, net/http, net/http/httptest, os, path/filepath, strings, testing, icloud-hme/internal/account, icloud-hme/internal/hme, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 TestAccountResponseNoSecrets, TestAddAndUpdateAccountTags, TestRemoveAccountCascadeDeleteSchedule 等测试套件
 * [POS]: internal/server 的账号接口与安全边界单元测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/store"
)

// writeSecretAccounts 把含秘密的账号写入测试数据目录。
func writeSecretAccounts(t *testing.T, dir string) {
	t.Helper()
	data := `{
  "accounts": {
    "acc_secret": {
      "id": "acc_secret",
      "name": "秘密账号",
      "real_email": "owner@example.com",
      "icloud_email": "owner@icloud.com",
      "cookies": {"X-APPLE-WEBAUTH-USER": "cookie-secret", "dsid": "dsid-secret"},
      "host": "icloud.com",
      "proxy": "http://user:proxy-secret@example.com:8080",
      "app_password": "app-secret",
      "status": "active",
      "alias_total": 3,
      "alias_active": 2,
      "last_validated": "2026-08-04T09:00:00+08:00",
      "created_at": "2026-08-01T09:00:00+08:00"
    }
  },
  "updated_at": "2026-08-04T09:00:00+08:00"
}`
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

// TestAccountResponseNoSecrets 验证账号列表响应不包含任何秘密。
func TestAccountResponseNoSecrets(t *testing.T) {
	dir := t.TempDir()
	writeSecretAccounts(t, dir)
	mgr, err := account.NewManager(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := newWithBackend(&managerBackend{mgr: mgr}, Config{
		Debug:         false,
		AdminPassword: "admin-pass-2026-strong",
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	sess, csrf := login(t, ts, "admin-pass-2026-strong")
	req := authedReq(t, ts, "GET", "/api/accounts", "")
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	status, body, _ := do(t, req)
	if status != http.StatusOK {
		t.Fatalf("期望 200,得到 %d: %s", status, body)
	}

	// 字节级断言:不含秘密子串
	for _, secret := range []string{"cookie-secret", "app-secret", "proxy-secret", "dsid-secret"} {
		if strings.Contains(body, secret) {
			t.Fatalf("响应泄露秘密 %q: %s", secret, body)
		}
	}

	// 精确键断言:不存在 cookies / app_password / proxy 键
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 1 {
		t.Fatalf("期望 1 个账号,得到 %d", len(out.Data))
	}
	acc := out.Data[0]
	for _, forbidden := range []string{"cookies", "app_password", "proxy"} {
		if _, exists := acc[forbidden]; exists {
			t.Fatalf("响应包含禁止键 %q", forbidden)
		}
	}
	// 合法键存在
	for _, required := range []string{"has_cookies", "has_app_password", "has_proxy", "id", "name"} {
		if _, exists := acc[required]; !exists {
			t.Fatalf("响应缺少键 %q", required)
		}
	}
	if acc["has_cookies"] != true || acc["has_app_password"] != true || acc["has_proxy"] != true {
		t.Fatalf("凭据状态错误: %v", acc)
	}
	_ = csrf
}

// TestAccountLoginResponseNoSecrets 验证 iCloud 登录成功响应只含 Summary 字段。
func TestAccountLoginResponseNoSecrets(t *testing.T) {
	f := &fakeBackend{
		accounts: []account.Summary{{
			ID: "acc_secret", Name: "秘密账号", Status: "active",
			HasCookies: true, HasAppPassword: true, HasProxy: true,
		}},
	}
	s := newWithBackend(f, Config{
		Debug:         false,
		AdminPassword: "admin-pass-2026-strong",
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	sess, csrf := login(t, ts, "admin-pass-2026-strong")
	req := authedReq(t, ts, "POST", "/api/accounts/acc_secret/login", `{"password":"p@ssw0rd-2026"}`)
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	req.Header.Set("X-CSRF-Token", csrf)
	status, body, _ := do(t, req)
	if status != http.StatusOK {
		t.Fatalf("期望 200,得到 %d: %s", status, body)
	}
	var out struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if _, exists := out.Data["cookies"]; exists {
		t.Fatalf("登录响应不应包含 cookies 键")
	}
	// 只允许 Summary 字段
	for key := range out.Data {
		switch key {
		case "id", "name", "real_email", "icloud_email", "host", "status", "alias_total",
			"alias_active", "has_cookies", "has_app_password", "has_proxy",
			"last_validated", "status_message", "created_at":
		default:
			t.Fatalf("登录响应包含意外字段 %q", key)
		}
	}
}

// TestAccountHandlerValidation 验证账号端点的参数校验(使用真实 manager 适配器)。
func TestAccountHandlerValidation(t *testing.T) {
	dir := t.TempDir()
	mgr, err := account.NewManager(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := newWithBackend(&managerBackend{mgr: mgr}, Config{
		Debug:         false,
		AdminPassword: "admin-pass-2026-strong",
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	sess, csrf := login(t, ts, "admin-pass-2026-strong")

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		status int
		code   string
	}{
		{"空名称", "POST", "/api/accounts", `{"name":"","icloud_email":"a@icloud.com"}`, 400, "VALIDATION_ERROR"},
		{"空邮箱", "POST", "/api/accounts", `{"name":"主号","icloud_email":""}`, 400, "VALIDATION_ERROR"},
		{"非法主机", "POST", "/api/accounts", `{"name":"主号","icloud_email":"a@icloud.com","host":"evil.com"}`, 400, "VALIDATION_ERROR"},
		{"非法代理", "POST", "/api/accounts", `{"name":"主号","icloud_email":"a@icloud.com","proxy":"ftp://x"}`, 400, "VALIDATION_ERROR"},
		{"PATCH 空更新", "PATCH", "/api/accounts/acc_1", `{}`, 400, "VALIDATION_ERROR"},
		{"PUT 非法代理", "PUT", "/api/accounts/acc_1/proxy", `{"proxy":"ftp://x"}`, 400, "VALIDATION_ERROR"},
	}
	for _, tc := range cases {
		req := authedReq(t, ts, tc.method, tc.path, tc.body)
		req.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
		req.Header.Set("X-CSRF-Token", csrf)
		status, body, _ := do(t, req)
		if status != tc.status {
			t.Fatalf("%s: 期望 %d,得到 %d: %s", tc.name, tc.status, status, body)
		}
		var out struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal([]byte(body), &out)
		if out.Code != tc.code {
			t.Fatalf("%s: 期望 code=%s,得到 %q", tc.name, tc.code, out.Code)
		}
	}
}

// TestAccountUpdateCookiesAcceptString 验证 PUT cookies 同时接受字符串和对象。
func TestAccountUpdateCookiesAcceptString(t *testing.T) {
	f := &fakeBackend{accounts: []account.Summary{{ID: "acc_1", Name: "主号"}}}
	s := newWithBackend(f, Config{
		Debug:         false,
		AdminPassword: "admin-pass-2026-strong",
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	sess, csrf := login(t, ts, "admin-pass-2026-strong")

	// 字符串输入
	req := authedReq(t, ts, "PUT", "/api/accounts/acc_1/cookies", `{"cookies":"a=1; b=2"}`)
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	req.Header.Set("X-CSRF-Token", csrf)
	status, _, _ := do(t, req)
	if status != http.StatusOK {
		t.Fatalf("字符串 cookies 期望 200,得到 %d", status)
	}

	// 对象输入
	req = authedReq(t, ts, "PUT", "/api/accounts/acc_1/cookies", `{"cookies":{"a":"1","b":"2"}}`)
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	req.Header.Set("X-CSRF-Token", csrf)
	status, _, _ = do(t, req)
	if status != http.StatusOK {
		t.Fatalf("对象 cookies 期望 200,得到 %d", status)
	}
}

// TestAccountDeleteNotFound 验证删除不存在的账号返回 404/ACCOUNT_NOT_FOUND。
func TestAccountDeleteNotFound(t *testing.T) {
	f := &fakeBackend{removedOK: false}
	s := newWithBackend(f, Config{
		Debug:         false,
		AdminPassword: "admin-pass-2026-strong",
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	sess, csrf := login(t, ts, "admin-pass-2026-strong")
	req := authedReq(t, ts, "DELETE", "/api/accounts/acc_missing", "")
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	req.Header.Set("X-CSRF-Token", csrf)
	status, body, _ := do(t, req)
	if status != http.StatusNotFound {
		t.Fatalf("期望 404,得到 %d", status)
	}
	var out struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	if out.Code != "ACCOUNT_NOT_FOUND" {
		t.Fatalf("期望 code=ACCOUNT_NOT_FOUND,得到 %q", out.Code)
	}
}

// TestUpdateAliasHandler 验证修改别名备注接口。
func TestUpdateAliasHandler(t *testing.T) {
	f := &fakeBackend{}
	s := newWithBackend(f, Config{
		Debug:         false,
		AdminPassword: "admin-pass-2026-strong",
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	sess, csrf := login(t, ts, "admin-pass-2026-strong")
	req := authedReq(t, ts, "PATCH", "/api/aliases/anon_123", `{"account_id":"acc_1","label":"Twitter注册","note":"测试备注"}`)
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	req.Header.Set("X-CSRF-Token", csrf)
	status, body, _ := do(t, req)
	if status != http.StatusOK {
		t.Fatalf("期望 200,得到 %d: %s", status, body)
	}
	if f.aliasUpdateID != "anon_123" {
		t.Fatalf("期望 anonymousID=anon_123, 得到 %q", f.aliasUpdateID)
	}
	if f.aliasUpdateLabel != "Twitter注册" {
		t.Fatalf("期望 label=Twitter注册, 得到 %q", f.aliasUpdateLabel)
	}
}

// TestBatchUpdateAliasHandler 验证批量修改别名备注接口。
func TestBatchUpdateAliasHandler(t *testing.T) {
	f := &fakeBackend{}
	s := newWithBackend(f, Config{
		Debug:         false,
		AdminPassword: "admin-pass-2026-strong",
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	sess, csrf := login(t, ts, "admin-pass-2026-strong")

	// 1. 正常批量更新
	req := authedReq(t, ts, "POST", "/api/aliases/batch-update", `{"account_id":"acc_1","anonymous_ids":["anon_1","anon_2"],"label":"批量标记","note":"说明"}`)
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	req.Header.Set("X-CSRF-Token", csrf)
	status, body, _ := do(t, req)
	if status != http.StatusOK {
		t.Fatalf("期望 200, 得到 %d: %s", status, body)
	}
	if len(f.batchUpdateIDs) != 2 || f.batchUpdateLabel != "批量标记" {
		t.Fatalf("fakeBackend 数据未匹配: ids=%v label=%q", f.batchUpdateIDs, f.batchUpdateLabel)
	}

	// 2. 空 anonymous_ids 校验
	reqEmpty := authedReq(t, ts, "POST", "/api/aliases/batch-update", `{"account_id":"acc_1","anonymous_ids":[],"label":"批量标记"}`)
	reqEmpty.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	reqEmpty.Header.Set("X-CSRF-Token", csrf)
	status, _, _ = do(t, reqEmpty)
	if status != http.StatusBadRequest {
		t.Fatalf("期望 400, 得到 %d", status)
	}

	// 3. 缺少 account_id 校验
	reqNoAcc := authedReq(t, ts, "POST", "/api/aliases/batch-update", `{"anonymous_ids":["anon_1"],"label":"批量标记"}`)
	reqNoAcc.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	reqNoAcc.Header.Set("X-CSRF-Token", csrf)
	status, _, _ = do(t, reqNoAcc)
	if status != http.StatusBadRequest {
		t.Fatalf("期望 400, 得到 %d", status)
	}
}

// TestListAliasesHandler 验证别名查询接口，包括单账号与全局聚合模式。
func TestListAliasesHandler(t *testing.T) {
	f := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_1", Name: "小号1", Status: "active", HasCookies: true},
			{ID: "acc_2", Name: "备用号", Status: "active", HasCookies: true},
		},
		aliases: []hme.Alias{
			{Email: "a1@icloud.com", AnonymousID: "anon_1", Label: "test"},
		},
	}
	s := newWithBackend(f, Config{
		Debug:         false,
		AdminPassword: "admin-pass-2026-strong",
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	sess, csrf := login(t, ts, "admin-pass-2026-strong")

	// 1. 全局聚合模式 (account_id=all)
	reqAll := authedReq(t, ts, "GET", "/api/aliases?account_id=all", "")
	reqAll.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	reqAll.Header.Set("X-CSRF-Token", csrf)
	status, body, _ := do(t, reqAll)
	if status != http.StatusOK {
		t.Fatalf("期望 200, 得到 %d: %s", status, body)
	}
	var resAll struct {
		Success bool `json:"success"`
		Data    struct {
			AccountID string      `json:"account_id"`
			Count     int         `json:"count"`
			Aliases   []hme.Alias `json:"aliases"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &resAll); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resAll.Data.AccountID != "all" || resAll.Data.Count != 2 {
		t.Fatalf("期望聚合 2 个别名, 得到 %d (account_id=%s)", resAll.Data.Count, resAll.Data.AccountID)
	}
	if resAll.Data.Aliases[0].AccountID == "" || resAll.Data.Aliases[0].AccountName == "" {
		t.Fatalf("期望别名附带 account_id 与 account_name, 得到 %+v", resAll.Data.Aliases[0])
	}

	// 2. 单账号模式 (account_id=acc_1)
	reqSingle := authedReq(t, ts, "GET", "/api/aliases?account_id=acc_1", "")
	reqSingle.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	reqSingle.Header.Set("X-CSRF-Token", csrf)
	status, bodySingle, _ := do(t, reqSingle)
	if status != http.StatusOK {
		t.Fatalf("期望 200, 得到 %d: %s", status, bodySingle)
	}
	var resSingle struct {
		Data struct {
			AccountID string      `json:"account_id"`
			Count     int         `json:"count"`
			Aliases   []hme.Alias `json:"aliases"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(bodySingle), &resSingle)
	if resSingle.Data.AccountID != "acc_1" || resSingle.Data.Count != 1 {
		t.Fatalf("期望单账号 1 个别名, 得到 %d", resSingle.Data.Count)
	}
	if resSingle.Data.Aliases[0].AccountName != "小号1" {
		t.Fatalf("期望单账号别名附带母账号名称 小号1, 得到 %q", resSingle.Data.Aliases[0].AccountName)
	}
}

// TestAddAndUpdateAccountTags 验证在添加和修改账号时正确持久化和更新 Tags。
func TestAddAndUpdateAccountTags(t *testing.T) {
	dir := t.TempDir()
	mgr, err := account.NewManager(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := newWithBackend(&managerBackend{mgr: mgr}, Config{
		Debug:         false,
		AdminPassword: "admin-pass-2026-strong",
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	sess, csrf := login(t, ts, "admin-pass-2026-strong")

	// 1. POST /api/accounts 添加带 Tags 的账号
	addBody := `{"name": "标签测试号", "icloud_email": "tagtest@icloud.com", "host": "icloud.com", "tags": ["prod", "us_east"]}`
	addReq := authedReq(t, ts, "POST", "/api/accounts", addBody)
	addReq.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	addReq.Header.Set("X-CSRF-Token", csrf)
	status, body, _ := do(t, addReq)
	if status != http.StatusCreated {
		t.Fatalf("添加账号期望 201, 得到 %d: %s", status, body)
	}

	var addResp struct {
		Success bool            `json:"success"`
		Data    account.Summary `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &addResp); err != nil {
		t.Fatal(err)
	}
	if len(addResp.Data.Tags) != 2 || addResp.Data.Tags[0] != "prod" || addResp.Data.Tags[1] != "us_east" {
		t.Fatalf("添加后 Tags 不符, 得到: %v", addResp.Data.Tags)
	}
	accID := addResp.Data.ID

	// 2. PATCH /api/accounts/:id 更新 Tags
	updateBody := `{"tags": ["vip", "asia_hk"]}`
	updateReq := authedReq(t, ts, "PATCH", "/api/accounts/"+accID, updateBody)
	updateReq.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	updateReq.Header.Set("X-CSRF-Token", csrf)
	status, body, _ = do(t, updateReq)
	if status != http.StatusOK {
		t.Fatalf("更新账号期望 200, 得到 %d: %s", status, body)
	}

	var updateResp struct {
		Success bool            `json:"success"`
		Data    account.Summary `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &updateResp); err != nil {
		t.Fatal(err)
	}
	if len(updateResp.Data.Tags) != 2 || updateResp.Data.Tags[0] != "vip" || updateResp.Data.Tags[1] != "asia_hk" {
		t.Fatalf("更新后 Tags 不符, 得到: %v", updateResp.Data.Tags)
	}
}

// TestRemoveAccountCascadeDeleteSchedule 验证删除账号时物理级联清除其调度配置
func TestRemoveAccountCascadeDeleteSchedule(t *testing.T) {
	dir := t.TempDir()
	st, err := store.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	mgr, err := account.NewManager(dir, st)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := mgr.AddAccountWithInput(account.AddAccountInput{
		Name:        "待删除母号",
		ICloudEmail: "delete_me@icloud.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	accID := sum.ID

	// 在 store 中为其创建一条调度配置
	err = st.SaveScheduleConfig(store.ScheduleConfig{
		AccountID:   accID,
		Enabled:     true,
		HourlyQuota: 5,
		AliasLabel:  "cascade_test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.ListScheduleConfigs()) != 1 {
		t.Fatal("保存后 schedules 应有 1 条记录")
	}

	be := &managerBackend{mgr: mgr, store: st}
	cfg := Config{APIKey: "test-api-key"}
	s := newWithBackendAndStore(be, cfg, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// DELETE /api/accounts/:id
	req := authedReq(t, ts, "DELETE", "/api/accounts/"+accID, "")
	req.Header.Set("X-API-Key", "test-api-key")
	status, _, _ := do(t, req)
	if status != http.StatusOK {
		t.Fatalf("删除账号期望 200, 得到 %d", status)
	}

	// 验证账号已被删除
	if _, ok := mgr.GetAccount(accID); ok {
		t.Fatal("账号应已从管理器删除")
	}

	// 关键验证：SQLite schedules 表中该账号记录已被物理级联删除，零幽灵残留
	if len(st.ListScheduleConfigs()) != 0 {
		t.Fatal("删除账号后，其调度配置未被级联物理删除，存在幽灵残留")
	}
}

// TestGetSingleAccountHandler 验证 GET /api/accounts/:id 成功与不存在返回 404
func TestGetSingleAccountHandler(t *testing.T) {
	f := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_alpha", Name: "Alpha账号", Status: "active"},
			{ID: "acc_beta", Name: "Beta账号", Status: "pending"},
		},
	}
	s := newWithBackend(f, Config{Debug: false, AdminPassword: "admin-pass-2026-strong"})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	sess, _ := login(t, ts, "admin-pass-2026-strong")

	// 1. 查询存在的账号
	req := authedReq(t, ts, "GET", "/api/accounts/acc_alpha", "")
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	status, body, _ := do(t, req)
	if status != http.StatusOK {
		t.Fatalf("期望 200 OK, 得到 %d: %s", status, body)
	}
	var resp struct {
		Success bool            `json:"success"`
		Data    account.Summary `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Data.ID != "acc_alpha" || resp.Data.Name != "Alpha账号" {
		t.Fatalf("获取单账号数据不符: %+v", resp.Data)
	}

	// 2. 查询不存在的账号返回 404
	req404 := authedReq(t, ts, "GET", "/api/accounts/acc_nonexistent", "")
	req404.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	status404, _, _ := do(t, req404)
	if status404 != http.StatusNotFound {
		t.Fatalf("不存在账号期望 404, 得到 %d", status404)
	}
}

// TestListAccountsPagination 验证 GET /api/accounts 支持 limit & offset 分页
func TestListAccountsPagination(t *testing.T) {
	accs := make([]account.Summary, 10)
	for i := 0; i < 10; i++ {
		accs[i] = account.Summary{ID: fmt.Sprintf("acc_%d", i), Name: fmt.Sprintf("账号%d", i)}
	}
	f := &fakeBackend{accounts: accs}
	s := newWithBackend(f, Config{Debug: false, AdminPassword: "admin-pass-2026-strong"})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	sess, _ := login(t, ts, "admin-pass-2026-strong")

	// 1. 不传 limit: 全量返回数组（向后兼容）
	reqAll := authedReq(t, ts, "GET", "/api/accounts", "")
	reqAll.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	status, body, _ := do(t, reqAll)
	if status != http.StatusOK {
		t.Fatalf("全量期望 200, 得到 %d", status)
	}
	var allResp struct {
		Success bool              `json:"success"`
		Data    []account.Summary `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &allResp); err != nil {
		t.Fatal(err)
	}
	if len(allResp.Data) != 10 {
		t.Fatalf("全量数量应为 10, 得到 %d", len(allResp.Data))
	}

	// 2. 传 limit=3&offset=2: 返回分页包
	reqPaged := authedReq(t, ts, "GET", "/api/accounts?limit=3&offset=2", "")
	reqPaged.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	status, body, _ = do(t, reqPaged)
	if status != http.StatusOK {
		t.Fatalf("分页期望 200, 得到 %d", status)
	}
	var pagedResp struct {
		Success bool `json:"success"`
		Data    struct {
			Items  []account.Summary `json:"items"`
			Total  int               `json:"total"`
			Limit  int               `json:"limit"`
			Offset int               `json:"offset"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &pagedResp); err != nil {
		t.Fatal(err)
	}
	if pagedResp.Data.Total != 10 || pagedResp.Data.Limit != 3 || pagedResp.Data.Offset != 2 {
		t.Fatalf("分页元数据不符: %+v", pagedResp.Data)
	}
	if len(pagedResp.Data.Items) != 3 || pagedResp.Data.Items[0].ID != "acc_2" {
		t.Fatalf("分页项不符: %+v", pagedResp.Data.Items)
	}
}

var _ = io.Discard
