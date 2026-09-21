/**
 * [INPUT]: 依赖 testing, net/http, net/http/httptest, strings, encoding/json, time, icloud-hme/internal/store, icloud-hme/internal/mail, icloud-hme/internal/account
 * [OUTPUT]: 提供 V01~V10 验证码时效、事件身份与持久恢复 PR-06 验收单测
 * [POS]: internal/server 的 PR-06 验证码边界与状态机安全保证验证套件
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

func setupV2TestEnv(t *testing.T) (*Server, *store.Store, *fakeBackend, *httptest.Server, string, string) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	fb := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_imap", Status: "active", HasAppPassword: true, Tags: []string{"default"}},
		},
	}
	cfg := Config{
		Debug:         false,
		AdminPassword: "admin-pass-strong",
	}
	s := newWithBackendAndStore(fb, cfg, st)
	ts := httptest.NewServer(s.Handler())

	// 预先注册一个外部 Token
	tokenID := "tok_v06_test"
	tokenSecret := "secret_v06_test"
	_ = st.SaveToken(store.APIToken{
		ID:        tokenID,
		Name:      "v06_bot",
		Token:     tokenSecret,
		Scopes:    "allocate,verify",
		CreatedAt: time.Now().Format(time.RFC3339),
	})

	// 预先给该 token 分配一个别名
	email := "target_alias@icloud.com"
	_ = st.AddInventoryAlias("acc_imap", hme.Alias{Email: email, Active: true}, "replenish", true)
	alloc := &store.AliasAllocation{
		AllocationID: "lease_v06_1",
		AliasEmail:   email,
		AccountID:    "acc_imap",
		OwnerKind:    "token",
		OwnerID:      tokenID,
		BusinessTag:  "default",
		Status:       "allocated",
		AllocatedAt:  time.Now().Format(time.RFC3339),
	}
	if err := st.RecordAllocation(alloc, "v06_bot"); err != nil {
		t.Fatalf("RecordAllocation failed: %v", err)
	}

	return s, st, fb, ts, tokenSecret, alloc.AllocationID
}

func parseData(t *testing.T, resp *http.Response) map[string]interface{} {
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("json decode failed: %v", err)
	}
	if data, ok := body["data"].(map[string]interface{}); ok {
		return data
	}
	return body
}

// V01: 准备前已存在的历史 OTP 不会作为本次新信返回
func TestPR06_V01_HistoricalOTPFilteredOut(t *testing.T) {
	s, st, fb, ts, tok, leaseID := setupV2TestEnv(t)
	defer st.Close()
	defer ts.Close()

	fb.mailboxBoundaryFunc = func(accountID, folder string) (string, uint32, uint32, error) {
		return "imap", 1, 100, nil // 当前基线 UID 为 100
	}

	// 1. 外部创建 verification request
	body, _ := json.Marshal(map[string]string{"lease_id": leaseID})
	req, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/verification-requests", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("create vreq failed: code=%d, err=%v", resp.StatusCode, err)
	}
	res := parseData(t, resp)
	vreqID := res["request_id"].(string)

	// 2. 注入一封历史邮件 (UID=95 < 100)
	s.eventBus.PublishEvent(&mail.CachedOTP{
		EventID:     "evt_hist_95",
		Email:       "target_alias@icloud.com",
		UIDValidity: 1,
		UID:         95,
		OTP:         &mail.OTPResult{Code: "999999"},
	})

	// 3. 查询结果，验证历史 OTP 不被接收，依然处于 pending
	qReq, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/external/v2/verification-requests/%s?timeout=0", ts.URL, vreqID), nil)
	qReq.Header.Set("Authorization", "Bearer "+tok)
	qResp, err := http.DefaultClient.Do(qReq)
	if err != nil {
		t.Fatal(err)
	}
	qRes := parseData(t, qResp)
	if status := qRes["status"]; status != "pending" {
		t.Fatalf("V01 failed: expected status 'pending', got: %v", status)
	}
	if qRes["code"] != nil && qRes["code"] != "" {
		t.Fatalf("V01 failed: historical code leaked: %v", qRes["code"])
	}
}

// V02: 准备后新信正常返回
func TestPR06_V02_FreshOTPDelivered(t *testing.T) {
	s, st, fb, ts, tok, leaseID := setupV2TestEnv(t)
	defer st.Close()
	defer ts.Close()

	fb.mailboxBoundaryFunc = func(accountID, folder string) (string, uint32, uint32, error) {
		return "imap", 1, 100, nil
	}

	body, _ := json.Marshal(map[string]string{"lease_id": leaseID})
	req, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/verification-requests", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	res := parseData(t, resp)
	vreqID := res["request_id"].(string)

	// 注入新到达邮件 (UID=100 >= 100)
	s.eventBus.PublishEvent(&mail.CachedOTP{
		EventID:     "evt_fresh_100",
		Email:       "target_alias@icloud.com",
		UIDValidity: 1,
		UID:         100,
		OTP:         &mail.OTPResult{Code: "123456"},
	})

	qReq, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/external/v2/verification-requests/%s?timeout=0", ts.URL, vreqID), nil)
	qReq.Header.Set("Authorization", "Bearer "+tok)
	qResp, err := http.DefaultClient.Do(qReq)
	if err != nil {
		t.Fatal(err)
	}
	qRes := parseData(t, qResp)
	if qRes["status"] != "succeeded" {
		t.Fatalf("V02 failed: expected succeeded, got %v", qRes["status"])
	}
	if qRes["code"] != "123456" {
		t.Fatalf("V02 failed: expected code 123456, got %v", qRes["code"])
	}
}

// V03: 重启恢复保留原边界，不回放旧码
func TestPR06_V03_RestartRecoveryPreservesBoundary(t *testing.T) {
	tempDir := t.TempDir()
	st, _ := store.NewStore(tempDir)
	defer st.Close()

	// 写入持久化任务，基线 UID=500
	now := time.Now().UTC()
	vreq := &store.VerificationRequest{
		RequestID:           "vreq_restart_1",
		PrincipalKind:       "token",
		PrincipalID:         "tok_v03",
		LeaseID:             "lease_v03",
		AliasEmail:          "restart@icloud.com",
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "imap",
		BaselineUIDValidity: 1,
		BaselineUID:         500,
	}
	_ = st.CreateVerificationRequest(context.Background(), vreq)
	_ = st.SaveToken(store.APIToken{ID: "tok_v03", Token: "tok_v03_sec", Scopes: "verify"})

	// 模拟重启：创建新 Server
	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", HasAppPassword: true}},
	}
	sNew := newWithBackendAndStore(fb, Config{AdminPassword: "admin"}, st)
	tsNew := httptest.NewServer(sNew.Handler())
	defer tsNew.Close()

	// 1. 注入旧信 (UID 499)，必须被过滤
	sNew.eventBus.PublishEvent(&mail.CachedOTP{
		EventID:     "evt_old_499",
		Email:       "restart@icloud.com",
		UIDValidity: 1,
		UID:         499,
		OTP:         &mail.OTPResult{Code: "000499"},
	})

	qReq, _ := http.NewRequest("GET", tsNew.URL+"/api/external/v2/verification-requests/vreq_restart_1?timeout=0", nil)
	qReq.Header.Set("Authorization", "Bearer tok_v03_sec")
	resp, _ := http.DefaultClient.Do(qReq)
	qRes := parseData(t, resp)
	if qRes["status"] != "pending" {
		t.Fatalf("V03 failed: expected pending for UID 499, got %v", qRes["status"])
	}

	// 2. 注入符合边界的新信 (UID 501)
	sNew.eventBus.PublishEvent(&mail.CachedOTP{
		EventID:     "evt_new_501",
		Email:       "restart@icloud.com",
		UIDValidity: 1,
		UID:         501,
		OTP:         &mail.OTPResult{Code: "501501"},
	})
	resp2, _ := http.DefaultClient.Do(qReq)
	qRes2 := parseData(t, resp2)
	if qRes2["status"] != "succeeded" || qRes2["code"] != "501501" {
		t.Fatalf("V03 failed: expected succeeded 501501, got %v (%v)", qRes2["status"], qRes2["code"])
	}
}

// V04: 消费事件 A 不删除更晚到达事件 B
func TestPR06_V04_ConsumeDoesNotDeleteLaterEvents(t *testing.T) {
	bus := mail.NewEventBus(5 * time.Minute)
	email := "multi_event@icloud.com"

	// 发布事件 A (UID=100) 与事件 B (UID=101)
	bus.PublishEvent(&mail.CachedOTP{
		EventID:     "evt_A",
		Email:       email,
		UIDValidity: 1,
		UID:         100,
		OTP:         &mail.OTPResult{Code: "111111"},
	})
	bus.PublishEvent(&mail.CachedOTP{
		EventID:     "evt_B",
		Email:       email,
		UIDValidity: 1,
		UID:         101,
		OTP:         &mail.OTPResult{Code: "222222"},
	})

	// 消费事件 A
	bus.ConsumeEvent(email, "evt_A")

	// 订阅基线 101 的事件 (代表更晚的任务)，事件 B 必须依然存在
	_, ch := bus.SubscribeWithBoundary(email, "INBOX", 1, 101)
	select {
	case ev := <-ch:
		if ev.EventID != "evt_B" || ev.OTP.Code != "222222" {
			t.Fatalf("V04 failed: expected evt_B, got: %v", ev)
		}
	default:
		t.Fatalf("V04 failed: evt_B was mistakenly deleted when consuming evt_A")
	}
}

// V05: 同一 request 多次查询返回同一结果；不同请求不抢占同一共享缓存
func TestPR06_V05_IdempotentResultsAndCacheIsolation(t *testing.T) {
	s, st, fb, ts, tok, leaseID := setupV2TestEnv(t)
	defer st.Close()
	defer ts.Close()

	fb.mailboxBoundaryFunc = func(accountID, folder string) (string, uint32, uint32, error) {
		return "imap", 1, 100, nil
	}

	body, _ := json.Marshal(map[string]string{"lease_id": leaseID})
	req, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/verification-requests", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	res := parseData(t, resp)
	vreqID := res["request_id"].(string)

	// 注入 OTP
	s.eventBus.PublishEvent(&mail.CachedOTP{
		EventID:     "evt_100",
		Email:       "target_alias@icloud.com",
		UIDValidity: 1,
		UID:         100,
		OTP:         &mail.OTPResult{Code: "777888"},
	})

	// 第一次查询 -> succeeded 777888
	qReq, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/external/v2/verification-requests/%s", ts.URL, vreqID), nil)
	qReq.Header.Set("Authorization", "Bearer "+tok)
	resp1, _ := http.DefaultClient.Do(qReq)
	res1 := parseData(t, resp1)
	if res1["status"] != "succeeded" || res1["code"] != "777888" {
		t.Fatalf("V05 first query failed: %v", res1)
	}

	// 紧接着注入一封新信 999999
	s.eventBus.PublishEvent(&mail.CachedOTP{
		EventID:     "evt_101",
		Email:       "target_alias@icloud.com",
		UIDValidity: 1,
		UID:         101,
		OTP:         &mail.OTPResult{Code: "999999"},
	})

	// 第二次幂等查询同一个 request -> 必须依然返回原结果 777888，绝不改变为 999999
	resp2, _ := http.DefaultClient.Do(qReq)
	res2 := parseData(t, resp2)
	if res2["status"] != "succeeded" || res2["code"] != "777888" {
		t.Fatalf("V05 idempotent query failed: expected 777888, got: %v", res2["code"])
	}
}

// V06: 正文提到另一 alias 的邮件不会跨租约发布
func TestPR06_V06_BodyMentionDoesNotCrossLease(t *testing.T) {
	// 邮件收件人为 attacker@icloud.com，正文提及 target@icloud.com 的验证码
	msg := mail.Message{
		From:    "service@bank.com",
		To:      "attacker@icloud.com",
		Subject: "Your Login Verification Code",
		Preview: "The verification code for target@icloud.com is 654321.",
	}

	// 针对 target@icloud.com 进行匹配测试
	if msgMatchesRecipient(msg, "target@icloud.com") {
		t.Fatalf("V06 failed: msgMatchesRecipient falsely matched email mentioned only in body")
	}

	// 结构化匹配核查
	if len(msg.RecipientAddresses()) != 1 || msg.RecipientAddresses()[0] != "attacker@icloud.com" {
		t.Fatalf("V06 failed: RecipientAddresses should only contain attacker@icloud.com, got: %v", msg.RecipientAddresses())
	}
}

// V07: 原始 To 缺失不自动补成查询 alias
func TestPR06_V07_MissingToNotAutoFilled(t *testing.T) {
	msg := mail.Message{
		From:    "no-to@domain.com",
		To:      "", // 缺失 To
		Subject: "OTP notification",
		Preview: "Code is 112233",
	}

	if msgMatchesRecipient(msg, "target@icloud.com") {
		t.Fatalf("V07 failed: missing To was falsely matched against target query alias")
	}
	if len(msg.RecipientAddresses()) != 0 {
		t.Fatalf("V07 failed: RecipientAddresses should be empty when To is empty, got: %v", msg.RecipientAddresses())
	}
}

// V08: WebMail 无法证明严格时效时明确能力受限
func TestPR06_V08_WebMailCapabilityUnsupported(t *testing.T) {
	_, st, fb, ts, tok, leaseID := setupV2TestEnv(t)
	defer st.Close()
	defer ts.Close()

	// 模拟 WebMail 模式：返回 CAPABILITY_UNSUPPORTED
	fb.mailboxBoundaryFunc = func(accountID, folder string) (string, uint32, uint32, error) {
		return "", 0, 0, &BackendError{
			Status:  http.StatusBadRequest,
			Code:    "CAPABILITY_UNSUPPORTED",
			Message: "WebMail 不支持严格时效验证码基线",
		}
	}

	body, _ := json.Marshal(map[string]string{"lease_id": leaseID})
	req, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/verification-requests", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("V08 failed: expected 400 Bad Request, got: %d", resp.StatusCode)
	}
	var res map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	if errObj, ok := res["error"].(map[string]interface{}); ok {
		if errObj["code"] != "CAPABILITY_UNSUPPORTED" {
			t.Fatalf("V08 failed: expected error code CAPABILITY_UNSUPPORTED, got: %v", errObj["code"])
		}
	}
}

// V09: UIDVALIDITY 改变、过期、撤销 token 都阻止交付
func TestPR06_V09_RevocationExpiryAndUIDValidityChange(t *testing.T) {
	s, st, fb, ts, tok, leaseID := setupV2TestEnv(t)
	defer st.Close()
	defer ts.Close()

	fb.mailboxBoundaryFunc = func(accountID, folder string) (string, uint32, uint32, error) {
		return "imap", 1, 100, nil
	}

	// 1. 测试 UIDVALIDITY 突变
	body, _ := json.Marshal(map[string]string{"lease_id": leaseID})
	req, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/verification-requests", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	res := parseData(t, resp)
	vreqID := res["request_id"].(string)

	// 注入 UIDVALIDITY 突变的事件 (例如重建了 mailbox，validity 变为 999)
	s.eventBus.PublishEvent(&mail.CachedOTP{
		EventID:     "evt_invalidated",
		Email:       "target_alias@icloud.com",
		UIDValidity: 999, // 突变！
		UID:         100,
		OTP:         &mail.OTPResult{Code: "000111"},
	})

	qReq, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/external/v2/verification-requests/%s?timeout=0", ts.URL, vreqID), nil)
	qReq.Header.Set("Authorization", "Bearer "+tok)
	qResp, _ := http.DefaultClient.Do(qReq)
	// 期望 409 UIDVALIDITY_CHANGED
	if qResp.StatusCode != http.StatusConflict {
		t.Fatalf("V09 UIDValidityChange failed: expected 409 Conflict, got %d", qResp.StatusCode)
	}

	// 2. 测试 Token 被撤销后无法取码 (401 Unauthorized)
	// 删除该 token
	_, _ = st.DeleteToken("tok_v06_test")
	qResp2, _ := http.DefaultClient.Do(qReq)
	if qResp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("V09 TokenRevocation failed: expected 401 Unauthorized, got %d", qResp2.StatusCode)
	}

	// 3. 测试过期任务
	now := time.Now().UTC()
	vreqExpired := &store.VerificationRequest{
		RequestID:           "vreq_expired_test",
		PrincipalKind:       "token",
		PrincipalID:         "tok_alive",
		LeaseID:             leaseID,
		AliasEmail:          "target_alias@icloud.com",
		Status:              "ready",
		CreatedAt:           now.Add(-20 * time.Minute).Format(time.RFC3339),
		ExpiresAt:           now.Add(-10 * time.Minute).Format(time.RFC3339), // 10分钟前已过期
		BaselineProvider:    "imap",
		BaselineUIDValidity: 1,
		BaselineUID:         100,
	}
	_ = st.SaveToken(store.APIToken{ID: "tok_alive", Token: "alive_sec", Scopes: "verify"})
	_ = st.CreateVerificationRequest(context.Background(), vreqExpired)

	qExpReq, _ := http.NewRequest("GET", ts.URL+"/api/external/v2/verification-requests/vreq_expired_test", nil)
	qExpReq.Header.Set("Authorization", "Bearer alive_sec")
	expResp, _ := http.DefaultClient.Do(qExpReq)
	expRes := parseData(t, expResp)
	if expRes["status"] != "expired" {
		t.Fatalf("V09 Expiration failed: expected status 'expired', got: %v", expRes["status"])
	}
}

// V10: 准备时接收新邮件的竞态不丢失符合边界的事件
func TestPR06_V10_RaceBetweenBaselineAndArrival(t *testing.T) {
	s, st, fb, ts, tok, leaseID := setupV2TestEnv(t)
	defer st.Close()
	defer ts.Close()

	fb.mailboxBoundaryFunc = func(accountID, folder string) (string, uint32, uint32, error) {
		return "imap", 1, 200, nil // 边界为 200
	}

	// 1. POST 请求完成采集基线边界
	body, _ := json.Marshal(map[string]string{"lease_id": leaseID})
	req, _ := http.NewRequest("POST", ts.URL+"/api/external/v2/verification-requests", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	res := parseData(t, resp)
	vreqID := res["request_id"].(string)

	// 2. 竞态事件：在外部客户端调用 GET 开始轮询之前，新邮件就已经推入 EventBus 缓存
	s.eventBus.PublishEvent(&mail.CachedOTP{
		EventID:     "evt_race_200",
		Email:       "target_alias@icloud.com",
		UIDValidity: 1,
		UID:         200,
		OTP:         &mail.OTPResult{Code: "888200"},
	})

	// 3. 稍后客户端发起 GET 请求
	qReq, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/external/v2/verification-requests/%s?timeout=1", ts.URL, vreqID), nil)
	qReq.Header.Set("Authorization", "Bearer "+tok)
	qResp, _ := http.DefaultClient.Do(qReq)
	qRes := parseData(t, qResp)

	if qRes["status"] != "succeeded" || qRes["code"] != "888200" {
		t.Fatalf("V10 race test failed: expected immediate delivery of pre-buffered event 888200, got: %v (%v)", qRes["status"], qRes["code"])
	}
}
