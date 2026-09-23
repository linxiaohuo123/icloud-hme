package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/store"
)

// B04: 数据库故障注入或非空池错误时，管理员建号降级绝不触发，backend.CreateAlias 调用次数=0
func TestB04_DatabaseErrorDoesNotTriggerCreateAlias(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var createCalls int32
	fb := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_b04", Status: "active", HasCookies: true, Tags: []string{"default"}},
		},
		onCreateAlias: func(accountID, label string) (*hme.CreateResult, error) {
			atomic.AddInt32(&createCalls, 1)
			return &hme.CreateResult{Email: "created_b04@icloud.com", Label: label}, nil
		},
	}

	cfg := Config{
		Debug:         false,
		AdminPassword: "admin-pass-2026-strong",
	}
	s := newWithBackendAndStore(fb, cfg, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// 在数据库注入故障：让 operations 表插入触发失败，造成真实的 DB 约束/SQL 错误
	_, err = st.DB().Exec(`CREATE TRIGGER trigger_fail_op_b04 BEFORE INSERT ON operations BEGIN SELECT RAISE(FAIL, 'db locked/io failure'); END;`)
	if err != nil {
		t.Fatalf("create trigger failed: %v", err)
	}

	cookie, csrf := login(t, ts, "admin-pass-2026-strong")
	req, _ := http.NewRequest("POST", ts.URL+"/api/allocate", strings.NewReader(`{"mode":"pool","tag":"default","idempotency_key":"key_b04"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	req.Header.Set("X-CSRF-Token", csrf)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// 必须报错，不能伪造成 200 现场建号成功
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("B04 FAILED: request succeeded despite database failure")
	}

	// 核心断言：backend.CreateAlias 调用次数必须为 0！
	calls := atomic.LoadInt32(&createCalls)
	if calls != 0 {
		t.Fatalf("B04 FAILED: backend.CreateAlias was called %d times on database error, expected 0", calls)
	}
}
