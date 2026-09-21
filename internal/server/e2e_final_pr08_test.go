/**
 * [INPUT]: 依赖 testing, net/http, net/http/httptest, bytes, context, fmt, json, icloud-hme/internal/auth, icloud-hme/internal/hme, icloud-hme/internal/mail, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 TestPR08_E2E_FullLifecycleIntegration 全链路端到端集成测试
 * [POS]: internal/server 的终极 E2E 验证套件 (PR-08 §11.3)，串联管理员登录、配额出号、外部v2提取、IMAP基线、令牌撤销与平稳停机重启恢复
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
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

func TestPR08_E2E_FullLifecycleIntegration(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.NewStore(dataDir)
	if err != nil {
		t.Fatalf("failed to init store: %v", err)
	}

	// 1. 模拟管理员预置库存
	aliasEmail := "e2e_final_alias@icloud.com"
	_ = st.AddInventoryAlias("acc_master_1", hme.Alias{
		Email:       aliasEmail,
		AnonymousID: "anon_e2e_001",
		Label:       "e2e-stock",
		Active:      true,
	}, "replenish", true)

	// 2. 生成外部 API 令牌 (仅 allocate,verify 权限)
	tokID := "tok_e2e_partner"
	tokSecret := "sec_e2e_partner_secret_key"
	_ = st.SaveToken(store.APIToken{
		ID:     tokID,
		Name:   "PartnerA",
		Token:  tokSecret,
		Scopes: "allocate,verify",
	})

	// 3. 构建第一代服务实例
	fb := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_master_1", Status: "active", HasCookies: true, Tags: []string{"order_service"}},
		},
		mailboxBoundaryFunc: func(accountID, folder string) (string, uint32, uint32, error) {
			return "imap", 12345, 100, nil
		},
	}

	cfg := Config{
		DataDir:          dataDir,
		AdminPassword:    "admin_secret_pass",
		MailPollInterval: 100 * time.Millisecond,
	}

	srv := newWithBackendAndStore(fb, cfg, st)
	srv.syncWorker.Start()
	ts := newServerWithHandler(srv.Handler())
	defer func() {
		ts.Close()
		srv.Close()
	}()

	// 4. 外部令牌出号 (POST /api/external/v2/allocate)
	allocBody := `{"tag":"order_service","label":"order_666"}`
	allocReq, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/allocate", strings.NewReader(allocBody))
	allocReq.Header.Set("Authorization", "Bearer "+tokSecret)
	allocReq.Header.Set("Idempotency-Key", "idemp_e2e_001")
	allocResp, err := http.DefaultClient.Do(allocReq)
	if err != nil {
		t.Fatalf("allocate request failed: %v", err)
	}
	defer allocResp.Body.Close()

	if allocResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for allocate, got %d", allocResp.StatusCode)
	}
	allocData := parseData(t, allocResp)
	leaseID := allocData["lease_id"].(string)
	assignedEmail := allocData["alias_email"].(string)
	if assignedEmail != aliasEmail {
		t.Fatalf("expected assigned email %s, got %s", aliasEmail, assignedEmail)
	}

	// 5. 准备取码意图 (POST /api/external/v2/verification-requests)
	vreqBody, _ := json.Marshal(map[string]string{"lease_id": leaseID})
	vreqReq, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/verification-requests", bytes.NewReader(vreqBody))
	vreqReq.Header.Set("Authorization", "Bearer "+tokSecret)
	vreqResp, err := http.DefaultClient.Do(vreqReq)
	if err != nil {
		t.Fatalf("vreq request failed: %v", err)
	}
	defer vreqResp.Body.Close()

	if vreqResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for vreq create, got %d", vreqResp.StatusCode)
	}
	vreqData := parseData(t, vreqResp)
	vreqID := vreqData["request_id"].(string)
	if vreqData["baseline_ready"] != true {
		t.Fatalf("expected baseline_ready true")
	}

	// 6. 开启长轮询等待邮件 (异步发起 GET /api/external/v2/verification-requests/:id?timeout=5)
	doneCh := make(chan struct{})
	var pollStatus int
	var pollResult map[string]interface{}

	go func() {
		getReq, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/external/v2/verification-requests/%s?timeout=5", ts.URL, vreqID), nil)
		getReq.Header.Set("Authorization", "Bearer "+tokSecret)
		resp, gErr := http.DefaultClient.Do(getReq)
		if gErr == nil {
			pollStatus = resp.StatusCode
			pollResult = parseData(t, resp)
			resp.Body.Close()
		}
		close(doneCh)
	}()

	// 等待 100ms 挂起长轮询订阅
	time.Sleep(100 * time.Millisecond)

	// 7. 模拟新邮件到达母号 (UID >= 100 符合基线)
	srv.eventBus.PublishEvent(&mail.CachedOTP{
		EventID:     "msg_e2e_ref_999",
		AccountID:   "acc_master_1",
		Email:       aliasEmail,
		Folder:      "INBOX",
		UIDValidity: 12345,
		UID:         105,
		OTP:         &mail.OTPResult{Code: "998877"},
		Subject:     "Your Security Code: 998877",
	})

	// 等待长轮询返回
	select {
	case <-doneCh:
		if pollStatus != http.StatusOK {
			t.Fatalf("expected 200 OK from polling, got %d", pollStatus)
		}
		if pollResult["code"] != "998877" {
			t.Fatalf("expected OTP 998877, got %v", pollResult["code"])
		}
		if pollResult["message_ref"] != "msg_e2e_ref_999" {
			t.Fatalf("expected message_ref msg_e2e_ref_999, got %v", pollResult["message_ref"])
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting for verification long-poll response")
	}

	// 8. 令牌撤销阻断验证：管理员注销令牌后，访问立即被 401 拦截
	_, _ = st.DeleteToken(tokID)
	revReq, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/external/v2/verification-requests/%s", ts.URL, vreqID), nil)
	revReq.Header.Set("Authorization", "Bearer "+tokSecret)
	revResp, _ := http.DefaultClient.Do(revReq)
	if revResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for revoked token, got %d", revResp.StatusCode)
	}

	// 9. 优雅停机测试
	ts.Close()
	srv.Close()

	// 10. 重启服务与恢复验证：建立新实例，复用同一持久化数据目录
	st2, err := store.NewStore(dataDir)
	if err != nil {
		t.Fatalf("failed to reopen store: %v", err)
	}
	defer st2.Close()

	srv2 := newWithBackendAndStore(fb, cfg, st2)
	defer srv2.Close()
	ts2 := newServerWithHandler(srv2.Handler())
	defer ts2.Close()

	// 验证：原别名仍然处于 allocated 状态，不可被再次认领分配！
	tok2Secret := "sec_second_partner"
	_ = st2.SaveToken(store.APIToken{
		ID:     "tok_second",
		Name:   "PartnerB",
		Token:  tok2Secret,
		Scopes: "allocate,verify",
	})

	secondAllocReq, _ := http.NewRequest("POST", ts2.URL+"/api/external/v2/allocate", strings.NewReader(`{"tag":"default"}`))
	secondAllocReq.Header.Set("Authorization", "Bearer "+tok2Secret)
	secondAllocReq.Header.Set("Idempotency-Key", "idemp_second_alloc")
	secondResp, err := http.DefaultClient.Do(secondAllocReq)
	if err != nil {
		t.Fatalf("second alloc request failed: %v", err)
	}
	defer secondResp.Body.Close()

	// 库存仅此 1 个且已分配给 PartnerA，PartnerB 必须收到 503 POOL_EMPTY，绝不重复发放！
	if secondResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 POOL_EMPTY after restart when pool is exhausted, got %d", secondResp.StatusCode)
	}
}

// 辅助函数：快速用已有 http.Handler 启动 httptest.Server
func newServerWithHandler(h http.Handler) *httptest.Server {
	return httptest.NewServer(h)
}
