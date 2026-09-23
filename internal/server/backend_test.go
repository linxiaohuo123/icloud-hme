/**
 * [INPUT]: 依赖 testing, sync, net/http, net/http/httptest, icloud-hme/internal/account, icloud-hme/internal/hme, icloud-hme/internal/mail
 * [OUTPUT]: 对外提供 fakeBackend 测试桩与 Backend 相关集成单元测试
 * [POS]: internal/server 的 Backend 接口门面、双模邮件读取、parseMessageID 与 WebMail 删除 400 单元测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

// fakeBackend 是测试用内存 Backend,记录调用,不访问网络。
type fakeBackend struct {
	mu       sync.RWMutex
	accounts []account.Summary
	aliases  []hme.Alias
	inbox    InboxResult
	created  *hme.CreateResult

	addedInput   account.AddAccountInput
	updatedID    string
	updatedInput account.UpdateAccountInput
	proxyID      string
	proxyValue   string
	cookiesID    string
	cookiesValue string
	appPwdID     string
	appPwdEmail  string
	loginID      string
	loginErr     error
	removedID    string
	removedOK    bool

	aliasActID       string
	aliasActActive   bool
	aliasActErr      error
	aliasUpdateID    string
	aliasUpdateLabel string
	aliasUpdateErr   error
	batchUpdateIDs   []string
	batchUpdateLabel string
	batchUpdateRes   BatchUpdateResult
	batchUpdateErr   error
	aliasDeleteID    string
	aliasDeleteErr   error
	listInboxQuery   InboxQuery
	reloadCount      int

	onClose       func()
	onCreateAlias func(accountID, label string) (*hme.CreateResult, error)
	onListAliases      func(accountID string) ([]hme.Alias, error)
	onListInbox        func(q InboxQuery) (InboxResult, error)
	onListInboxContext func(ctx context.Context, q InboxQuery) (InboxResult, error)
	onListMailboxes        func(accountID string) ([]mail.Folder, error)
	onListMailboxesContext func(ctx context.Context, accountID string) ([]mail.Folder, error)

	validateID   string
	validateFunc func(id string) error

	onSetAliasActive    func(accountID, anonymousID string, active bool) (bool, error)
	onDeleteAlias       func(accountID, anonymousID string) error
	getMessageFunc      func(accountID string, id string) (*mail.FullMessage, error)
	getMessagesFunc     func(accountID string, refs []mail.MessageRef) ([]*mail.FullMessage, error)
	mailboxBoundaryFunc func(accountID, folder string) (string, uint32, uint32, error)
	store               *store.Store
}

func (f *fakeBackend) ListAccounts() []account.Summary {
	f.mu.RLock()
	defer f.mu.RUnlock()
	res := make([]account.Summary, len(f.accounts))
	copy(res, f.accounts)
	return res
}

func (f *fakeBackend) GetAccount(id string) (account.Summary, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, a := range f.accounts {
		if a.ID == id {
			return a, nil
		}
	}
	return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
}

func (f *fakeBackend) AddAccount(in account.AddAccountInput) (account.Summary, error) {
	f.addedInput = in
	return account.Summary{ID: "acc_new", Name: in.Name, Status: "pending"}, nil
}

func (f *fakeBackend) UpdateAccount(id string, in account.UpdateAccountInput) (account.Summary, error) {
	f.updatedID, f.updatedInput = id, in
	if len(f.accounts) == 0 {
		return account.Summary{}, fmt.Errorf("fake: 更新失败")
	}
	return f.accounts[0], nil
}

func (f *fakeBackend) UpdateProxy(id, proxy string) (account.Summary, error) {
	f.proxyID, f.proxyValue = id, proxy
	if len(f.accounts) == 0 {
		return account.Summary{}, fmt.Errorf("fake: 代理更新失败")
	}
	return f.accounts[0], nil
}

func (f *fakeBackend) UpdateCookies(id, cookies string) (account.Summary, error) {
	f.cookiesID, f.cookiesValue = id, cookies
	if len(f.accounts) == 0 {
		return account.Summary{}, fmt.Errorf("fake: cookie 更新失败")
	}
	return f.accounts[0], nil
}

func (f *fakeBackend) SetAppPassword(id, email, appPassword string) (account.Summary, error) {
	f.appPwdID, f.appPwdEmail = id, email
	if len(f.accounts) == 0 {
		return account.Summary{}, fmt.Errorf("fake: 密码设置失败")
	}
	return f.accounts[0], nil
}

func (f *fakeBackend) SetMailbox(id string, config account.MailboxConfig) (account.Summary, error) {
	if len(f.accounts) == 0 {
		return account.Summary{}, fmt.Errorf("fake: 收件邮箱设置失败")
	}
	return f.accounts[0], nil
}

func (f *fakeBackend) LoginAccount(id, password, otp string) (account.Summary, error) {
	f.loginID = id
	if f.loginErr != nil {
		return account.Summary{}, f.loginErr
	}
	if len(f.accounts) == 0 {
		return account.Summary{}, fmt.Errorf("fake: 登录失败")
	}
	return f.accounts[0], nil
}

func (f *fakeBackend) RemoveAccount(id string) bool {
	f.removedID = id
	return f.removedOK
}

func (f *fakeBackend) Close() {
	f.mu.RLock()
	onClose := f.onClose
	f.mu.RUnlock()
	if onClose != nil {
		onClose()
	}
}

func (f *fakeBackend) CreateAlias(accountID, label string) (*hme.CreateResult, error) {
	if f.onCreateAlias != nil {
		return f.onCreateAlias(accountID, label)
	}
	return f.created, nil
}

func (f *fakeBackend) ListAliases(accountID string) ([]hme.Alias, error) {
	if f.onListAliases != nil {
		return f.onListAliases(accountID)
	}
	return f.aliases, nil
}

func (f *fakeBackend) RefreshAliases(accountID string) ([]hme.Alias, error) {
	return f.ListAliases(accountID)
}

func (f *fakeBackend) SetAliasActive(accountID, anonymousID string, active bool) (bool, error) {
	f.aliasActID, f.aliasActActive = anonymousID, active
	var (
		ok  = true
		err = f.aliasActErr
	)
	if f.onSetAliasActive != nil {
		ok, err = f.onSetAliasActive(accountID, anonymousID, active)
	}
	if err != nil || !ok {
		return ok, err
	}
	if f.store != nil {
		rState := store.RemoteInactive
		if active {
			rState = store.RemoteActive
		}
		if sErr := f.store.UpdateAliasRemoteState(accountID, anonymousID, "", rState); sErr != nil {
			return false, fmt.Errorf("local store update failed: %w", sErr)
		}
	}
	return true, nil
}

func (f *fakeBackend) UpdateAlias(accountID, anonymousID, label, note string) error {
	f.aliasUpdateID = anonymousID
	f.aliasUpdateLabel = label
	return f.aliasUpdateErr
}

func (f *fakeBackend) BatchUpdateAliases(accountID string, anonymousIDs []string, label, note string) (BatchUpdateResult, error) {
	f.batchUpdateIDs = anonymousIDs
	f.batchUpdateLabel = label
	if f.batchUpdateErr != nil {
		return BatchUpdateResult{}, f.batchUpdateErr
	}
	if f.batchUpdateRes.Total > 0 || len(f.batchUpdateRes.Succeeded) > 0 {
		return f.batchUpdateRes, nil
	}
	return BatchUpdateResult{
		Total:     len(anonymousIDs),
		Succeeded: anonymousIDs,
		Failed:    []string{},
	}, nil
}

func (f *fakeBackend) DeleteAlias(accountID, anonymousID string) error {
	f.aliasDeleteID = anonymousID
	var err = f.aliasDeleteErr
	if f.onDeleteAlias != nil {
		err = f.onDeleteAlias(accountID, anonymousID)
	}
	if err != nil {
		return err
	}
	if f.store != nil {
		if sErr := f.store.UpdateAliasRemoteState(accountID, anonymousID, "", store.RemoteDeleted); sErr != nil {
			return fmt.Errorf("local store delete update failed: %w", sErr)
		}
	}
	return nil
}

func (f *fakeBackend) BatchCreateAlias(accountID string, count int, labelPrefix string) (*BatchCreateResult, error) {
	created := make([]hme.CreateResult, 0, count)
	for i := 0; i < count; i++ {
		created = append(created, hme.CreateResult{
			Email:     fmt.Sprintf("alias%d@icloud.com", i+1),
			Label:     fmt.Sprintf("%s %d", labelPrefix, i+1),
			CreatedAt: "2026-09-20T12:00:00Z",
		})
	}
	return &BatchCreateResult{
		AccountID:    accountID,
		Requested:    count,
		Created:      created,
		CreatedCount: count,
	}, nil
}

func (f *fakeBackend) ListInbox(q InboxQuery) (InboxResult, error) {
	return f.ListInboxContext(context.Background(), q)
}

func (f *fakeBackend) ListInboxContext(ctx context.Context, q InboxQuery) (InboxResult, error) {
	f.mu.Lock()
	f.listInboxQuery = q
	onCtx := f.onListInboxContext
	onList := f.onListInbox
	inbox := f.inbox
	f.mu.Unlock()

	if onCtx != nil {
		return onCtx(ctx, q)
	}
	if onList != nil {
		if err := ctx.Err(); err != nil {
			return InboxResult{}, err
		}
		type fetchRes struct {
			res InboxResult
			err error
		}
		ch := make(chan fetchRes, 1)
		go func() {
			res, err := onList(q)
			ch <- fetchRes{res, err}
		}()
		select {
		case <-ctx.Done():
			return InboxResult{}, ctx.Err()
		case r := <-ch:
			return r.res, r.err
		}
	}
	return inbox, nil
}

func (f *fakeBackend) ListMailboxes(accountID string) ([]mail.Folder, error) {
	if f.onListMailboxes != nil {
		return f.onListMailboxes(accountID)
	}
	return []mail.Folder{
		{Name: "INBOX", Role: "inbox"},
		{Name: "Junk", Role: "junk"},
	}, nil
}

func (f *fakeBackend) ListMailboxesContext(ctx context.Context, accountID string) ([]mail.Folder, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.onListMailboxesContext != nil {
		return f.onListMailboxesContext(ctx, accountID)
	}
	return f.ListMailboxes(accountID)
}

func (f *fakeBackend) GetMessage(accountID string, id string) (*mail.FullMessage, error) {
	if f.getMessageFunc != nil {
		return f.getMessageFunc(accountID, id)
	}
	_, idPart, _, _ := parseMessageID(id)
	if idPart == "" {
		idPart = id
	}
	return &mail.FullMessage{Message: mail.Message{ID: idPart}}, nil
}

func (f *fakeBackend) GetMessageContext(ctx context.Context, accountID string, id string) (*mail.FullMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f.GetMessage(accountID, id)
}

func (f *fakeBackend) GetMessages(accountID string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
	if f.getMessagesFunc != nil {
		return f.getMessagesFunc(accountID, refs)
	}
	var out []*mail.FullMessage
	for _, r := range refs {
		mailbox := r.Mailbox
		if mailbox == "" {
			mailbox = "INBOX"
		}
		out = append(out, &mail.FullMessage{
			Message: mail.Message{
				ID:          r.Encode(),
				MessageRef:  r.Encode(),
				Folder:      mailbox,
				UIDValidity: r.UIDValidity,
				UID:         r.UID,
				Provider:    "imap",
			},
			BodyComplete: true,
			Provider:     "imap",
			Method:       "imap",
		})
	}
	return out, nil
}

func (f *fakeBackend) GetMessagesContext(ctx context.Context, accountID string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f.GetMessages(accountID, refs)
}

func (f *fakeBackend) GetMailboxBoundary(accountID, folder string) (string, uint32, uint32, error) {
	if f.mailboxBoundaryFunc != nil {
		return f.mailboxBoundaryFunc(accountID, folder)
	}
	return "imap", 1, 100, nil
}

func (f *fakeBackend) DeleteMessage(accountID string, uid uint32) error { return nil }

// ValidateAccount 记录被校验的账号;validateFunc 非空时委托其决定返回结果。
func (f *fakeBackend) ValidateAccount(id string) error {
	f.validateID = id
	if f.validateFunc != nil {
		return f.validateFunc(id)
	}
	return nil
}

func (f *fakeBackend) CheckProxy(proxyURL string) (bool, int64, string, error) {
	if strings.Contains(proxyURL, "invalid") {
		return false, 0, "", fmt.Errorf("fake: 代理连接失败")
	}
	return true, 120, "连接成功", nil
}

func (f *fakeBackend) Reload() error {
	f.reloadCount++
	return nil
}

// newTestServer 构造带固定密码与 fake backend 的测试 Server。
func newTestServer(f *fakeBackend) (*Server, *httptest.Server) {
	cfg := Config{
		Debug:         false,
		AdminPassword: "admin-pass-2026-strong",
		SessionTTL:    12 * time.Hour,
	}
	s := newWithBackend(f, cfg)
	ts := httptest.NewServer(s.Handler())
	return s, ts
}

// newTestServerWithStore 构造注入特定 store 的测试 Server。
func newTestServerWithStore(f *fakeBackend, st *store.Store) (*Server, *httptest.Server) {
	cfg := Config{
		Debug:         false,
		AdminPassword: "admin-pass-2026-strong",
		SessionTTL:    12 * time.Hour,
	}
	s := newWithBackendAndStore(f, cfg, st)
	ts := httptest.NewServer(s.Handler())
	return s, ts
}

// login 登录测试服务并返回 session Cookie 与 CSRF。
func login(t *testing.T, ts *httptest.Server, password string) (sessionCookie, csrf string) {
	t.Helper()
	body := fmt.Sprintf(`{"password":%q}`, password)
	req, _ := http.NewRequest("POST", ts.URL+"/api/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Success bool `json:"success"`
		Data    struct {
			CSRFToken string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "hme_session" {
			return c.Value, out.Data.CSRFToken
		}
	}
	t.Fatalf("响应未设置 hme_session Cookie (status=%d)", resp.StatusCode)
	return "", ""
}

// authedReq 构造带会话 Cookie 与 CSRF 头的请求。
func authedReq(t *testing.T, ts *httptest.Server, method, path, body string) *http.Request {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, ts.URL+path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func do(t *testing.T, req *http.Request) (int, string, []*http.Cookie) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw), resp.Cookies()
}

func TestGetMessagesBatchHandler(t *testing.T) {
	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Name: "测试号"}},
	}
	_, ts := newTestServer(fb)
	defer ts.Close()

	cookie, csrf := login(t, ts, "admin-pass-2026-strong")

	// 批量请求 2 封邮件
	body := `{"account_id":"acc_1","messages":[{"folder":"INBOX","uid":"101"},{"folder":"INBOX","uid":"102"}]}`
	req := authedReq(t, ts, "POST", "/api/messages", body)
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	req.Header.Set("X-CSRF-Token", csrf)

	status, respBody, _ := do(t, req)
	if status != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", status, respBody)
	}

	var res struct {
		Success bool `json:"success"`
		Data    struct {
			AccountID string              `json:"account_id"`
			Count     int                 `json:"count"`
			Messages  []*mail.FullMessage `json:"messages"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(respBody), &res); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if !res.Success || res.Data.Count != 2 {
		t.Fatalf("expected count 2, got %d", res.Data.Count)
	}

	// id 作为 uid 别名（前端预取曾误发 id）
	bodyID := `{"account_id":"acc_1","messages":[{"folder":"INBOX","id":"201"}]}`
	reqID := authedReq(t, ts, "POST", "/api/messages", bodyID)
	reqID.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	reqID.Header.Set("X-CSRF-Token", csrf)
	statusID, respID, _ := do(t, reqID)
	if statusID != http.StatusOK {
		t.Fatalf("id alias expected 200, got %d: %s", statusID, respID)
	}
	if !strings.Contains(respID, `"count":1`) {
		t.Fatalf("id alias expected count 1, got: %s", respID)
	}
}

func TestCheckProxyHandler(t *testing.T) {
	fb := &fakeBackend{}
	_, ts := newTestServer(fb)
	defer ts.Close()

	cookie, csrf := login(t, ts, "admin-pass-2026-strong")

	// 1. 正常代理
	body := `{"proxy":"socks5://127.0.0.1:1080"}`
	req := authedReq(t, ts, "POST", "/api/proxy/check", body)
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	req.Header.Set("X-CSRF-Token", csrf)

	status, respBody, _ := do(t, req)
	if status != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", status, respBody)
	}
	if !strings.Contains(respBody, `"ok":true`) {
		t.Fatalf("expected ok:true, got: %s", respBody)
	}

	// 2. 异常代理
	bodyFail := `{"proxy":"socks5://invalid.proxy:1080"}`
	reqFail := authedReq(t, ts, "POST", "/api/proxy/check", bodyFail)
	reqFail.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	reqFail.Header.Set("X-CSRF-Token", csrf)

	statusFail, respBodyFail, _ := do(t, reqFail)
	if statusFail != http.StatusBadGateway {
		t.Fatalf("expected 502 Bad Gateway, got %d: %s", statusFail, respBodyFail)
	}
}

func TestExportAliasesHandler(t *testing.T) {
	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Name: "测试号"}},
		aliases: []hme.Alias{
			{Email: "test1@icloud.com", Label: "标签1", Active: true, CreatedAt: "2026-09-20"},
			{Email: "test2@icloud.com", Label: "标签2", Active: false, CreatedAt: "2026-09-20"},
		},
	}
	_, ts := newTestServer(fb)
	defer ts.Close()

	cookie, _ := login(t, ts, "admin-pass-2026-strong")

	// 1. 导出 CSV
	reqCSV := authedReq(t, ts, "GET", "/api/aliases/export?account_id=acc_1&format=csv", "")
	reqCSV.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	respCSV, err := http.DefaultClient.Do(reqCSV)
	if err != nil {
		t.Fatal(err)
	}
	defer respCSV.Body.Close()
	rawCSV, _ := io.ReadAll(respCSV.Body)
	respBody := string(rawCSV)
	if respCSV.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", respCSV.StatusCode)
	}
	if !strings.Contains(respCSV.Header.Get("Content-Type"), "text/csv") {
		t.Fatalf("expected text/csv, got %s", respCSV.Header.Get("Content-Type"))
	}
	if !strings.Contains(respBody, "test1@icloud.com") || !strings.Contains(respBody, "已启用") {
		t.Fatalf("CSV content invalid: %s", respBody)
	}

	// 2. 导出 JSON
	reqJSON := authedReq(t, ts, "GET", "/api/aliases/export?account_id=acc_1&format=json", "")
	reqJSON.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	statusJ, respBodyJ, _ := do(t, reqJSON)
	if statusJ != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", statusJ)
	}
	if !strings.Contains(respBodyJ, "test1@icloud.com") {
		t.Fatalf("JSON content invalid: %s", respBodyJ)
	}
}

func TestGetMessagePrimeHandler(t *testing.T) {
	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Name: "测试号"}},
	}
	_, ts := newTestServer(fb)
	defer ts.Close()

	cookie, _ := login(t, ts, "admin-pass-2026-strong")

	req := authedReq(t, ts, "GET", "/api/messages/INBOX:42?account_id=acc_1", "")
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})

	status, respBody, _ := do(t, req)
	if status != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", status, respBody)
	}

	var res struct {
		Success bool `json:"success"`
		Data    struct {
			AccountID string `json:"account_id"`
			Message   struct {
				ID string `json:"id"`
			} `json:"message"`
			Method string `json:"method"`
			Cached bool   `json:"cached"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(respBody), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if res.Data.AccountID != "acc_1" || res.Data.Message.ID != "42" {
		t.Fatalf("unexpected prime message response: %+v", res.Data)
	}
}

func TestGetMessageWebMailStringIDHandler(t *testing.T) {
	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Name: "测试号"}},
	}
	_, ts := newTestServer(fb)
	defer ts.Close()

	cookie, _ := login(t, ts, "admin-pass-2026-strong")

	req := authedReq(t, ts, "GET", "/api/inbox/thread_abcdef123?account_id=acc_1", "")
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})

	status, respBody, _ := do(t, req)
	if status != http.StatusOK {
		t.Fatalf("expected 200 OK for WebMail string ThreadID, got %d: %s", status, respBody)
	}

	var res struct {
		Success bool `json:"success"`
		Data    struct {
			Message struct {
				ID string `json:"id"`
			} `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(respBody), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if res.Data.Message.ID != "thread_abcdef123" {
		t.Fatalf("expected thread_abcdef123, got: %+v", res.Data.Message)
	}
}

func TestDeleteMessageWebMailStringIDHandler(t *testing.T) {
	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Name: "测试号"}},
	}
	_, ts := newTestServer(fb)
	defer ts.Close()

	cookie, csrf := login(t, ts, "admin-pass-2026-strong")

	req := authedReq(t, ts, "DELETE", "/api/inbox/thread_abcdef123?account_id=acc_1", "")
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	req.Header.Set("X-CSRF-Token", csrf)

	status, respBody, _ := do(t, req)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for WebMail string ThreadID delete, got %d: %s", status, respBody)
	}

	var res struct {
		Success bool   `json:"success"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(respBody), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if res.Success || res.Code != "MAIL_DELETE_UNSUPPORTED" {
		t.Fatalf("unexpected delete response: %+v body=%s", res, respBody)
	}
}

func TestParseMessageID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw    string
		folder string
		idPart string
		uid    uint32
		hasUID bool
	}{
		{"1042", "", "1042", 1042, true},
		{"INBOX:1042", "INBOX", "1042", 1042, true},
		{"Junk:7", "Junk", "7", 7, true},
		{"thread_abcdef123", "", "thread_abcdef123", 0, false},
		{"thread:abc", "", "thread:abc", 0, false},
		{"INBOX:", "", "INBOX:", 0, false},
		{":42", "", ":42", 0, false},
		{"", "", "", 0, false},
		{"  99  ", "", "99", 99, true},
	}
	for _, tc := range cases {
		folder, idPart, uid, hasUID := parseMessageID(tc.raw)
		if folder != tc.folder || idPart != tc.idPart || uid != tc.uid || hasUID != tc.hasUID {
			t.Fatalf("parseMessageID(%q) = (%q, %q, %d, %v), want (%q, %q, %d, %v)",
				tc.raw, folder, idPart, uid, hasUID, tc.folder, tc.idPart, tc.uid, tc.hasUID)
		}
	}
}

func TestListInboxWithBodyQuery(t *testing.T) {
	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Name: "测试号"}},
	}
	_, ts := newTestServer(fb)
	defer ts.Close()

	cookie, _ := login(t, ts, "admin-pass-2026-strong")

	req := authedReq(t, ts, "GET", "/api/inbox?account_id=acc_1&body=1", "")
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})

	status, _, _ := do(t, req)
	if status != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", status)
	}
	fb.mu.RLock()
	withBody := fb.listInboxQuery.WithBody
	fb.mu.RUnlock()
	if !withBody {
		t.Fatalf("expected WithBody=true in InboxQuery")
	}
}

func TestManagerBackendRemoveAccountInvalidatesCache(t *testing.T) {
	tempDir := t.TempDir()
	mgr, err := account.NewManager(tempDir, nil)
	if err != nil {
		t.Fatalf("创建 manager 失败: %v", err)
	}
	acc, err := mgr.AddAccount("test_acc", "", "", "")
	if err != nil {
		t.Fatalf("添加账号失败: %v", err)
	}

	be := &managerBackend{
		mgr:        mgr,
		aliasCache: make(map[string]*aliasCacheItem),
	}
	be.setCachedAliases(acc.ID, []hme.Alias{
		{Email: "cached@icloud.com", AnonymousID: "anon_1", Active: true},
	})

	// 确认缓存已写入
	if _, ok := be.getCachedAliases(acc.ID); !ok {
		t.Fatalf("预期别名缓存存在")
	}

	// 删除账号
	if ok := be.RemoveAccount(acc.ID); !ok {
		t.Fatalf("删除账号失败")
	}

	// 验证缓存已被彻底驱逐
	if _, ok := be.getCachedAliases(acc.ID); ok {
		t.Fatalf("删除账号后别名缓存必须被立即销毁")
	}
}
