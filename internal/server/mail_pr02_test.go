/**
 * [INPUT]: 依赖 testing, net/http, net/http/httptest, encoding/json, strings, icloud-hme/internal/account, icloud-hme/internal/mail
 * [OUTPUT]: 对外提供 PR-02 邮件详情契约一致性、身份隔离与安全防伪单元测试
 * [POS]: internal/server 的 PR-02 邮件回归测试集
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"icloud-hme/internal/account"
	"icloud-hme/internal/mail"
)

func TestPR02_CacheHitMissSymmetryAndProviderHonesty(t *testing.T) {
	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Name: "测试号"}},
		getMessageFunc: func(accountID string, id string) (*mail.FullMessage, error) {
			return &mail.FullMessage{
				Message: mail.Message{
					ID:       "42",
					Folder:   "INBOX",
					Provider: "imap",
				},
				BodyComplete: true,
				Method:       "imap",
			}, nil
		},
	}
	_, ts := newTestServer(fb)
	defer ts.Close()

	cookie, _ := login(t, ts, "admin-pass-2026-strong")

	ref := mail.MessageRef{
		Provider:    "imap",
		AccountID:   "acc_1",
		Mailbox:     "INBOX",
		UIDValidity: 1234,
		UID:         42,
	}
	encodedRef := ref.Encode()

	// 1. 首次请求 (回源未命中缓存)
	req1 := authedReq(t, ts, "GET", "/api/inbox/"+encodedRef+"?account_id=acc_1", "")
	req1.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})

	status1, body1, _ := do(t, req1)
	if status1 != http.StatusOK {
		t.Fatalf("first request expected 200, got %d: %s", status1, body1)
	}

	type detailResp struct {
		Success bool `json:"success"`
		Data    struct {
			AccountID string            `json:"account_id"`
			Message   *mail.FullMessage `json:"message"`
			Provider  string            `json:"provider"`
			Method    string            `json:"method"`
			Cached    bool              `json:"cached"`
		} `json:"data"`
	}

	var res1 detailResp
	if err := json.Unmarshal([]byte(body1), &res1); err != nil {
		t.Fatalf("unmarshal body1 failed: %v", err)
	}

	if res1.Data.Cached != false {
		t.Fatalf("first request expected cached=false, got true")
	}
	if res1.Data.Provider != "imap" || res1.Data.Method != "imap" {
		t.Fatalf("expected provider=imap method=imap, got provider=%s method=%s", res1.Data.Provider, res1.Data.Method)
	}
	if res1.Data.Message.ID != "42" {
		t.Fatalf("expected message.id=42, got %s", res1.Data.Message.ID)
	}

	// 2. 二次请求 (命中缓存)
	req2 := authedReq(t, ts, "GET", "/api/inbox/"+encodedRef+"?account_id=acc_1", "")
	req2.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})

	status2, body2, _ := do(t, req2)
	if status2 != http.StatusOK {
		t.Fatalf("second request expected 200, got %d: %s", status2, body2)
	}

	var res2 detailResp
	if err := json.Unmarshal([]byte(body2), &res2); err != nil {
		t.Fatalf("unmarshal body2 failed: %v", err)
	}

	// 验证缓存命中与回源数据结构完全对称
	if res2.Data.Cached != true {
		t.Fatalf("second request expected cached=true, got false")
	}
	// 验证 provider 与 method 必须忠实保留真实信道，严禁被篡改为 "cache"
	if res2.Data.Provider != "imap" || res2.Data.Method != "imap" {
		t.Fatalf("cached response must retain provider=imap method=imap, got provider=%s method=%s", res2.Data.Provider, res2.Data.Method)
	}
	if res2.Data.AccountID != res1.Data.AccountID || res2.Data.Message.ID != res1.Data.Message.ID {
		t.Fatalf("cached payload asymmetric: miss=%+v, hit=%+v", res1.Data, res2.Data)
	}
}

func TestPR02_DetailRoutesConsistency(t *testing.T) {
	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Name: "测试号"}},
		getMessageFunc: func(accountID string, id string) (*mail.FullMessage, error) {
			return &mail.FullMessage{
				Message: mail.Message{
					ID:       "99",
					Folder:   "INBOX",
					Provider: "imap",
				},
				BodyComplete: true,
				Method:       "imap",
			}, nil
		},
	}
	_, ts := newTestServer(fb)
	defer ts.Close()

	cookie, _ := login(t, ts, "admin-pass-2026-strong")

	// 验证 /api/inbox/:id 与 /api/messages/:id 两条路由行为和结构完全一致
	reqInbox := authedReq(t, ts, "GET", "/api/inbox/99?account_id=acc_1", "")
	reqInbox.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	status1, body1, _ := do(t, reqInbox)

	reqMessages := authedReq(t, ts, "GET", "/api/messages/99?account_id=acc_1", "")
	reqMessages.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	status2, body2, _ := do(t, reqMessages)

	if status1 != http.StatusOK || status2 != http.StatusOK {
		t.Fatalf("routes status mismatch: inbox=%d, messages=%d", status1, status2)
	}

	var r1, r2 struct {
		Success bool `json:"success"`
		Data    struct {
			AccountID string `json:"account_id"`
			Message   struct {
				ID string `json:"id"`
			} `json:"message"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(body1), &r1)
	_ = json.Unmarshal([]byte(body2), &r2)

	if r1.Data.AccountID != r2.Data.AccountID || r1.Data.Message.ID != r2.Data.Message.ID {
		t.Fatalf("route outputs inconsistent: r1=%+v, r2=%+v", r1, r2)
	}
}

func TestPR02_CrossAccountAccessForbidden(t *testing.T) {
	fb := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_attacker", Name: "攻击者账号"},
			{ID: "acc_victim", Name: "受害者账号"},
		},
	}
	_, ts := newTestServer(fb)
	defer ts.Close()

	cookie, _ := login(t, ts, "admin-pass-2026-strong")

	// 构造明确属于受害者的 MessageRef
	victimRef := mail.MessageRef{
		Provider:    "imap",
		AccountID:   "acc_victim",
		Mailbox:     "INBOX",
		UIDValidity: 1234,
		UID:         100,
	}.Encode()

	// 攻击者试图以 acc_attacker 身份读取受害者引用的邮件
	req := authedReq(t, ts, "GET", "/api/inbox/"+victimRef+"?account_id=acc_attacker", "")
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})

	status, body, _ := do(t, req)
	if status != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden, got %d: %s", status, body)
	}
}

func TestPR02_UIDValidityMismatch(t *testing.T) {
	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Name: "测试号"}},
		getMessageFunc: func(accountID string, id string) (*mail.FullMessage, error) {
			return nil, &BackendError{
				Status:  http.StatusNotFound,
				Code:    "UIDVALIDITY_MISMATCH",
				Message: "邮箱 UIDVALIDITY 已变更，原邮件引用失效",
			}
		},
	}
	_, ts := newTestServer(fb)
	defer ts.Close()

	cookie, _ := login(t, ts, "admin-pass-2026-strong")

	req := authedReq(t, ts, "GET", "/api/inbox/100?account_id=acc_1", "")
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})

	status, body, _ := do(t, req)
	if status != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", status, body)
	}

	var res struct {
		Success bool   `json:"success"`
		Code    string `json:"code"`
	}
	_ = json.Unmarshal([]byte(body), &res)
	if res.Code != "UIDVALIDITY_MISMATCH" {
		t.Fatalf("expected code UIDVALIDITY_MISMATCH, got %s", res.Code)
	}
}

func TestPR02_WebMailBodyCompleteFalsePreview(t *testing.T) {
	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Name: "测试号"}},
		getMessageFunc: func(accountID string, id string) (*mail.FullMessage, error) {
			return &mail.FullMessage{
				Message: mail.Message{
					ID:       "thread_preview_only",
					Provider: "webmail",
					ThreadID: "thread_preview_only",
				},
				BodyComplete: false,
				Method:       "web_api",
			}, nil
		},
	}
	_, ts := newTestServer(fb)
	defer ts.Close()

	cookie, _ := login(t, ts, "admin-pass-2026-strong")

	req := authedReq(t, ts, "GET", "/api/inbox/thread_preview_only?account_id=acc_1", "")
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})

	status, body, _ := do(t, req)
	if status != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", status, body)
	}

	var res struct {
		Success bool `json:"success"`
		Data    struct {
			Provider string `json:"provider"`
			Method   string `json:"method"`
			Message  struct {
				ID           string `json:"id"`
				BodyComplete bool   `json:"body_complete"`
			} `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if res.Data.Provider != "webmail" || res.Data.Method != "web_api" {
		t.Fatalf("expected webmail/web_api, got %s/%s", res.Data.Provider, res.Data.Method)
	}
	if res.Data.Message.BodyComplete != false {
		t.Fatalf("expected body_complete=false for webmail preview, got true")
	}
}

func TestPR02_GetMessagesNoFakeSubstitution(t *testing.T) {
	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Name: "测试号"}},
		getMessagesFunc: func(accountID string, refs []MessageRef) ([]*mail.FullMessage, error) {
			return nil, errors.New("upstream imap failed")
		},
	}
	_, ts := newTestServer(fb)
	defer ts.Close()

	cookie, csrf := login(t, ts, "admin-pass-2026-strong")

	payload := `{"account_id":"acc_1","messages":[{"folder":"INBOX","uid":"1001"}]}`
	req := authedReq(t, ts, "POST", "/api/messages", payload)
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	req.Header.Set("X-CSRF-Token", csrf)

	status, body, _ := do(t, req)
	// 当 IMAP 读取失败时，必须返回错误，严禁用无关邮件冒充成功返回 200
	if status == http.StatusOK {
		t.Fatalf("expected failure status on batch get error, got 200: %s", body)
	}
}
