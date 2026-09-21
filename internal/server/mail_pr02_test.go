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
		getMessagesFunc: func(accountID string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
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

// MAIL-ID-01: 请求 UID41 + UID42，上游仅返回 UID42 -> UID41 not found，UID42 正确返回，绝不能把 UID42 放进 UID41
func TestPR02_MAIL_ID_01_PartialBatchNotFound(t *testing.T) {
	ref41 := mail.MessageRef{Provider: "imap", AccountID: "acc_1", Mailbox: "INBOX", UIDValidity: 1, UID: 41}
	ref42 := mail.MessageRef{Provider: "imap", AccountID: "acc_1", Mailbox: "INBOX", UIDValidity: 1, UID: 42}

	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Name: "测试号"}},
		getMessagesFunc: func(accountID string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
			// 上游仅返回 UID42
			return []*mail.FullMessage{
				{
					Message: mail.Message{
						ID:          "42",
						Folder:      "INBOX",
						UIDValidity: 1,
						UID:         42,
						Provider:    "imap",
						MessageRef:  ref42.Encode(),
						Subject:     "Subject 42",
					},
					Provider: "imap",
					Method:   "imap",
				},
			}, nil
		},
	}
	_, ts := newTestServer(fb)
	defer ts.Close()

	cookie, csrf := login(t, ts, "admin-pass-2026-strong")

	payload := `{"account_id":"acc_1","messages":[{"message_ref":"` + ref41.Encode() + `"},{"message_ref":"` + ref42.Encode() + `"}]}`
	req := authedReq(t, ts, "POST", "/api/messages", payload)
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	req.Header.Set("X-CSRF-Token", csrf)

	status, body, _ := do(t, req)
	if status != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", status, body)
	}

	var res struct {
		Data struct {
			Items []BatchItemResult `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if len(res.Data.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(res.Data.Items))
	}

	// 第一项: UID41 -> 必须 not found
	item1 := res.Data.Items[0]
	if item1.Message != nil {
		t.Fatalf("UID41 item must NOT have message, but got UID=%d", item1.Message.UID)
	}
	if item1.Error != "message not found" {
		t.Fatalf("expected 'message not found' for UID41, got '%s'", item1.Error)
	}

	// 第二项: UID42 -> 必须正确匹配 UID42
	item2 := res.Data.Items[1]
	if item2.Message == nil || item2.Message.UID != 42 {
		t.Fatalf("expected UID42 message, got %+v", item2.Message)
	}
}

// MAIL-ID-02: 上游乱序返回 UID42、UID41 -> 最终仍与请求正确对应
func TestPR02_MAIL_ID_02_OutOfOrderBatch(t *testing.T) {
	ref41 := mail.MessageRef{Provider: "imap", AccountID: "acc_1", Mailbox: "INBOX", UIDValidity: 1, UID: 41}
	ref42 := mail.MessageRef{Provider: "imap", AccountID: "acc_1", Mailbox: "INBOX", UIDValidity: 1, UID: 42}

	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Name: "测试号"}},
		getMessagesFunc: func(accountID string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
			// 上游故意乱序: 先返回 UID42，再返回 UID41
			return []*mail.FullMessage{
				{
					Message: mail.Message{
						ID:          "42",
						Folder:      "INBOX",
						UIDValidity: 1,
						UID:         42,
						Provider:    "imap",
						MessageRef:  ref42.Encode(),
						Subject:     "Subject 42",
					},
					Provider: "imap",
				},
				{
					Message: mail.Message{
						ID:          "41",
						Folder:      "INBOX",
						UIDValidity: 1,
						UID:         41,
						Provider:    "imap",
						MessageRef:  ref41.Encode(),
						Subject:     "Subject 41",
					},
					Provider: "imap",
				},
			}, nil
		},
	}
	_, ts := newTestServer(fb)
	defer ts.Close()

	cookie, csrf := login(t, ts, "admin-pass-2026-strong")

	payload := `{"account_id":"acc_1","messages":[{"message_ref":"` + ref41.Encode() + `"},{"message_ref":"` + ref42.Encode() + `"}]}`
	req := authedReq(t, ts, "POST", "/api/messages", payload)
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	req.Header.Set("X-CSRF-Token", csrf)

	status, body, _ := do(t, req)
	if status != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", status, body)
	}

	var res struct {
		Data struct {
			Items []BatchItemResult `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if len(res.Data.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(res.Data.Items))
	}
	if res.Data.Items[0].Message == nil || res.Data.Items[0].Message.UID != 41 {
		t.Fatalf("item 0 must be UID41, got %+v", res.Data.Items[0].Message)
	}
	if res.Data.Items[1].Message == nil || res.Data.Items[1].Message.UID != 42 {
		t.Fatalf("item 1 must be UID42, got %+v", res.Data.Items[1].Message)
	}
}

// MAIL-ID-03: INBOX UID42 与 Junk UID42 -> 不串
func TestPR02_MAIL_ID_03_FolderIsolation(t *testing.T) {
	refInbox := mail.MessageRef{Provider: "imap", AccountID: "acc_1", Mailbox: "INBOX", UIDValidity: 1, UID: 42}
	refJunk := mail.MessageRef{Provider: "imap", AccountID: "acc_1", Mailbox: "Junk", UIDValidity: 1, UID: 42}

	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Name: "测试号"}},
		getMessagesFunc: func(accountID string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
			return []*mail.FullMessage{
				{
					Message: mail.Message{
						ID:          "42",
						Folder:      "Junk",
						UIDValidity: 1,
						UID:         42,
						Provider:    "imap",
						MessageRef:  refJunk.Encode(),
						Subject:     "Junk 42",
					},
					Provider: "imap",
				},
				{
					Message: mail.Message{
						ID:          "42",
						Folder:      "INBOX",
						UIDValidity: 1,
						UID:         42,
						Provider:    "imap",
						MessageRef:  refInbox.Encode(),
						Subject:     "Inbox 42",
					},
					Provider: "imap",
				},
			}, nil
		},
	}
	_, ts := newTestServer(fb)
	defer ts.Close()

	cookie, csrf := login(t, ts, "admin-pass-2026-strong")

	payload := `{"account_id":"acc_1","messages":[{"message_ref":"` + refInbox.Encode() + `"},{"message_ref":"` + refJunk.Encode() + `"}]}`
	req := authedReq(t, ts, "POST", "/api/messages", payload)
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	req.Header.Set("X-CSRF-Token", csrf)

	status, body, _ := do(t, req)
	if status != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", status, body)
	}

	var res struct {
		Data struct {
			Items []BatchItemResult `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if len(res.Data.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(res.Data.Items))
	}
	if res.Data.Items[0].Message.Folder != "INBOX" || res.Data.Items[0].Message.Subject != "Inbox 42" {
		t.Fatalf("item 0 must be INBOX 42, got folder=%s subject=%s", res.Data.Items[0].Message.Folder, res.Data.Items[0].Message.Subject)
	}
	if res.Data.Items[1].Message.Folder != "Junk" || res.Data.Items[1].Message.Subject != "Junk 42" {
		t.Fatalf("item 1 must be Junk 42, got folder=%s subject=%s", res.Data.Items[1].Message.Folder, res.Data.Items[1].Message.Subject)
	}
}

// MAIL-ID-04: 同文件夹 UID42，但 UIDVALIDITY old/new -> 不串
func TestPR02_MAIL_ID_04_UIDValidityOldNew(t *testing.T) {
	refOld := mail.MessageRef{Provider: "imap", AccountID: "acc_1", Mailbox: "INBOX", UIDValidity: 100, UID: 42}
	refNew := mail.MessageRef{Provider: "imap", AccountID: "acc_1", Mailbox: "INBOX", UIDValidity: 200, UID: 42}

	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Name: "测试号"}},
		getMessagesFunc: func(accountID string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
			// 上游仅返回新代际 UIDVALIDITY=200 的邮件
			return []*mail.FullMessage{
				{
					Message: mail.Message{
						ID:          "42",
						Folder:      "INBOX",
						UIDValidity: 200,
						UID:         42,
						Provider:    "imap",
						MessageRef:  refNew.Encode(),
						Subject:     "New Generation 42",
					},
					Provider: "imap",
				},
			}, nil
		},
	}
	_, ts := newTestServer(fb)
	defer ts.Close()

	cookie, csrf := login(t, ts, "admin-pass-2026-strong")

	payload := `{"account_id":"acc_1","messages":[{"message_ref":"` + refOld.Encode() + `"},{"message_ref":"` + refNew.Encode() + `"}]}`
	req := authedReq(t, ts, "POST", "/api/messages", payload)
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	req.Header.Set("X-CSRF-Token", csrf)

	status, body, _ := do(t, req)
	if status != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", status, body)
	}

	var res struct {
		Data struct {
			Items []BatchItemResult `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if len(res.Data.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(res.Data.Items))
	}
	// 老代际必须 not found，绝不能把新代际的 42 错配给老代际
	if res.Data.Items[0].Message != nil {
		t.Fatalf("item 0 (old validity) must be not found, but got message")
	}
	if res.Data.Items[0].Error != "message not found" {
		t.Fatalf("expected 'message not found', got %s", res.Data.Items[0].Error)
	}
	// 新代际正确返回
	if res.Data.Items[1].Message == nil || res.Data.Items[1].Message.UIDValidity != 200 {
		t.Fatalf("item 1 (new validity) must be matched, got %+v", res.Data.Items[1].Message)
	}
}

// TestPR02_CrossLayerContract_ReactToBackendPayload: 真实 React request payload -> 真实 Go JSON bind -> MessageRef 保持完整
func TestPR02_CrossLayerContract_ReactToBackendPayload(t *testing.T) {
	origRef := mail.MessageRef{
		Provider:    "imap",
		AccountID:   "acc_react_1",
		Mailbox:     "INBOX",
		UIDValidity: 9876,
		UID:         12345,
	}
	encodedRef := origRef.Encode()

	var capturedRefs []mail.MessageRef
	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_react_1", Name: "React测试号"}},
		getMessagesFunc: func(accountID string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
			capturedRefs = refs
			return []*mail.FullMessage{
				{
					Message: mail.Message{
						ID:          "12345",
						Folder:      "INBOX",
						UIDValidity: 9876,
						UID:         12345,
						Provider:    "imap",
						MessageRef:  encodedRef,
						Subject:     "React Contract Subject",
					},
					Provider: "imap",
					Method:   "imap",
				},
			}, nil
		},
	}
	_, ts := newTestServer(fb)
	defer ts.Close()

	cookie, csrf := login(t, ts, "admin-pass-2026-strong")

	// 模拟真实 React 请求体：主路径严格仅发送 message_ref，无旧字段
	reactPayload := map[string]any{
		"account_id": "acc_react_1",
		"messages": []map[string]any{
			{
				"message_ref": encodedRef,
			},
		},
	}
	payloadBytes, _ := json.Marshal(reactPayload)

	req := authedReq(t, ts, "POST", "/api/messages", string(payloadBytes))
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	req.Header.Set("X-CSRF-Token", csrf)

	status, body, _ := do(t, req)
	if status != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", status, body)
	}

	if len(capturedRefs) != 1 {
		t.Fatalf("expected 1 captured ref in backend, got %d", len(capturedRefs))
	}
	cRef := capturedRefs[0]
	if cRef.Provider != "imap" || cRef.AccountID != "acc_react_1" || cRef.Mailbox != "INBOX" || cRef.UIDValidity != 9876 || cRef.UID != 12345 {
		t.Fatalf("MessageRef degraded or mutated: %+v", cRef)
	}

	var resp struct {
		Data struct {
			Items []BatchItemResult `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if len(resp.Data.Items) != 1 || resp.Data.Items[0].Message == nil {
		t.Fatalf("invalid items returned: %+v", resp.Data.Items)
	}
	if resp.Data.Items[0].RequestedRef != encodedRef {
		t.Fatalf("requested ref mismatch: got %s, want %s", resp.Data.Items[0].RequestedRef, encodedRef)
	}
}
