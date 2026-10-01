package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icloud-hme/internal/hme"
	"icloud-hme/internal/store"
)

func TestValidatedSessionSurvivesBusinessFailureAndRestart(t *testing.T) {
	for _, failure := range []error{context.Canceled, hme.ErrAuthFailed, errors.New("list timeout")} {
		m := newPoolTestManager(t, 1)
		id := firstAccountIDs(m, 1)[0]
		s := accountSessionFixture()
		m.accounts[id].Session, m.accounts[id].Cookies = s, s.CookieMap()
		calls := 0
		err := m.WithHMEClient(id, func(c *hme.Client) error {
			calls++
			rotated := s.Clone()
			rotated.Auth.SessionToken = "ROTATED"
			rotated.Cookies[0].Value = "ROTATED"
			if err := c.SessionValidated(rotated, &hme.AccountInfo{DSID: s.DSID}, "https://p01-maildomainws.icloud.com"); err != nil {
				return err
			}
			return failure
		})
		if !errors.Is(err, failure) || calls != 1 {
			t.Fatalf("err=%v calls=%d", err, calls)
		}
		reloaded, err := NewManager(m.dataDir, m.store)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := reloaded.GetAccount(id)
		reloaded.Close()
		if got.Session.Auth.SessionToken != "ROTATED" || got.Cookies["X-APPLE-WEBAUTH-TOKEN"] != "ROTATED" {
			t.Fatal("verified checkpoint lost on business failure")
		}
	}
}

func TestSessionCheckpointRejectsManualReplacementAndIdentityMismatch(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		m := newPoolTestManager(t, 1)
		id := firstAccountIDs(m, 1)[0]
		s := accountSessionFixture()
		m.accounts[id].Session, m.accounts[id].Cookies, m.accounts[id].AppleDSID = s, s.CookieMap(), s.DSID
		err := m.WithHMEClient(id, func(c *hme.Client) error {
			info := &hme.AccountInfo{DSID: "wrong"}
			if replacement {
				if err := m.SaveSession(id, map[string]string{"manual": "new"}, ""); err != nil {
					return err
				}
				info.DSID = s.DSID
			}
			return c.SessionValidated(s, info, "https://p01-maildomainws.icloud.com")
		})
		want := ErrAccountIdentityMismatch
		if replacement {
			want = ErrSessionChanged
		}
		if !errors.Is(err, want) {
			t.Fatalf("replacement=%v err=%v", replacement, err)
		}
		if replacement {
			a, _ := m.GetAccount(id)
			if a.Session != nil || a.Cookies["manual"] != "new" {
				t.Fatal("manual replacement overwritten")
			}
		}
	}
}

func TestPublicSessionExpiryUsesSetupScope(t *testing.T) {
	s := accountSessionFixture()
	expected := s.Cookies[0].Expires
	for _, cookie := range []hme.SessionCookie{
		{Name: "X-APPLE-WEBAUTH-TOKEN", Domain: ".icloud.com.cn", Path: "/", Expires: expected - 100},
		{Name: "X-APPLE-WEBAUTH-TOKEN", Domain: "www.icloud.com", Path: "/", Expires: expected - 100},
		{Name: "X-APPLE-WEBAUTH-TOKEN", Domain: ".icloud.com", Path: "/other", Expires: expected - 100},
	} {
		s.Cookies = append(s.Cookies, cookie)
	}
	a := Account{Session: s}
	if got := a.Summary().Session.TokenExpiresAt; got == nil || *got != expected {
		t.Fatal("unrelated scope used")
	}
	s.Cookies = append(s.Cookies, hme.SessionCookie{Name: "X-APPLE-WEBAUTH-TOKEN", Domain: "setup.icloud.com", Path: "/setup", Expires: -1})
	if a.Summary().Session.TokenExpiresAt != nil {
		t.Fatal("unknown applicable lifetime reported as known")
	}
}

func TestSessionCheckpointPersistenceFailurePreservesPreviousState(t *testing.T) {
	m := newPoolTestManager(t, 1)
	id := firstAccountIDs(m, 1)[0]
	s := accountSessionFixture()
	m.accounts[id].Session, m.accounts[id].Cookies = s, s.CookieMap()
	err := m.WithHMEClient(id, func(c *hme.Client) error {
		if err := m.store.DB().Close(); err != nil {
			t.Fatal(err)
		}
		rotated := s.Clone()
		rotated.Auth.SessionToken = "ROTATED"
		return c.SessionValidated(rotated, &hme.AccountInfo{DSID: s.DSID}, "https://p01-maildomainws.icloud.com")
	})
	if err == nil {
		t.Fatal("failed persistence reported success")
	}
	got, _ := m.GetAccount(id)
	if got.Session.Auth.SessionToken != s.Auth.SessionToken || m.hmePool.entries[id].client != nil {
		t.Fatal("failed checkpoint retained")
	}
}

func accountSessionFixture() *hme.BrowserSession {
	return &hme.BrowserSession{Version: 1, Host: "icloud.com", DSID: "123", Trusted: true, CapturedAt: time.Now().Unix(), Auth: hme.SessionAuth{SessionToken: "SESSION_SECRET_SENTINEL", TrustToken: "TRUST_SECRET_SENTINEL"}, Cookies: []hme.SessionCookie{{Name: "X-APPLE-WEBAUTH-TOKEN", Value: "COOKIE_SECRET_SENTINEL", Domain: ".icloud.com", Path: "/", Expires: float64(time.Now().Add(time.Hour).Unix()), Secure: true}}}
}
func TestBrowserSessionEncryptedRestartAndPublicRedaction(t *testing.T) {
	m := newPoolTestManager(t, 1)
	id := firstAccountIDs(m, 1)[0]
	s := accountSessionFixture()
	m.mu.Lock()
	a := m.accounts[id]
	a.Session = s
	a.Cookies = s.CookieMap()
	a.AppleDSID = s.DSID
	err := m.saveAccount(a)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var encrypted string
	if err := m.store.DB().QueryRow("SELECT cookies FROM accounts WHERE id=?", id).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encrypted, "enc:v1:") || strings.Contains(encrypted, "SENTINEL") {
		t.Fatal("session not encrypted")
	}
	raw, _ := json.Marshal(m.ListAccounts())
	summary, _ := json.Marshal(a.Summary())
	if strings.Contains(string(raw), "SENTINEL") || strings.Contains(string(summary), "SENTINEL") {
		t.Fatal("session leaked via public representation")
	}
	if a.Summary().Session == nil || !a.Summary().Session.RecoveryAvailable || a.Summary().Session.TokenExpiresAt == nil {
		t.Fatal("safe diagnostics missing")
	}
	restored, err := NewManager(m.dataDir, m.store)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	got, _ := restored.GetAccount(id)
	if got.Session == nil || got.Session.Auth.SessionToken != s.Auth.SessionToken || got.Cookies["X-APPLE-WEBAUTH-TOKEN"] != "COOKIE_SECRET_SENTINEL" {
		t.Fatal("session did not survive restart")
	}
	got.Session.Auth.SessionToken = "changed"
	actual, _ := restored.GetAccount(id)
	if actual.Session.Auth.SessionToken == "changed" {
		t.Fatal("snapshot aliases internal session")
	}
}

func TestRecoveryProgressPreservesCookiesAndManualReplacement(t *testing.T) {
	m := newPoolTestManager(t, 1)
	id := firstAccountIDs(m, 1)[0]
	s := accountSessionFixture()
	m.mu.Lock()
	m.accounts[id].Session = s
	m.accounts[id].Cookies = s.CookieMap()
	m.mu.Unlock()
	before, _ := m.GetAccount(id)
	failed := s.Clone()
	failed.Cookies = nil
	failed.RecoveryBlocked = true
	failed.RecoveryAfter = time.Now().Add(time.Minute).Unix()
	if err := m.saveRecoveryProgress(id, before, failed); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetAccount(id)
	if !got.Session.RecoveryBlocked || got.Cookies["X-APPLE-WEBAUTH-TOKEN"] != "COOKIE_SECRET_SENTINEL" || len(got.Session.Cookies) != 1 {
		t.Fatal("failed recovery poisoned usable cookie snapshot")
	}
	restored, err := NewManager(m.dataDir, m.store)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	durable, _ := restored.GetAccount(id)
	if !durable.Session.RecoveryBlocked || durable.Session.RecoveryAfter != failed.RecoveryAfter {
		t.Fatal("restart lost recovery budget")
	}
	if err := m.SaveSession(id, map[string]string{"new": "manual"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.saveRecoveryProgress(id, before, failed); !errors.Is(err, ErrSessionChanged) {
		t.Fatal("late recovery overwrote manual login")
	}
	got, _ = m.GetAccount(id)
	if got.Session != nil || got.Cookies["new"] != "manual" {
		t.Fatal("manual cookie import retained old auth materials")
	}
}

func TestBrowserSessionOldPoolOperationCannotOverwriteNewLogin(t *testing.T) {
	m := newPoolTestManager(t, 1)
	id := firstAccountIDs(m, 1)[0]
	s := accountSessionFixture()
	m.mu.Lock()
	m.accounts[id].Session = s
	m.accounts[id].Cookies = s.CookieMap()
	m.mu.Unlock()
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- m.WithHMEClient(id, func(*hme.Client) error { close(entered); <-release; return nil }) }()
	<-entered
	err := m.SaveSession(id, map[string]string{"new": "manual"}, "")
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("expected late snapshot rejection: %v", err)
	}
	got, _ := m.GetAccount(id)
	if got.Session != nil || got.Cookies["new"] != "manual" {
		t.Fatal("new credentials overwritten")
	}
}

func TestV7MigrationKeepsLegacyCookiesAndCreatesBackup(t *testing.T) {
	dir := t.TempDir()
	st, err := store.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAccount(&store.AccountRecord{ID: "legacy", Name: "legacy", Host: "icloud.com", CookiesJSON: `{"token":"legacy-value"}`}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec("PRAGMA user_version = 7"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	upgraded, err := store.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var version int
	if err := upgraded.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != store.CurrentSchemaVersion {
		t.Fatalf("migration failed: %d %v", version, err)
	}
	backups, err := filepath.Glob(filepath.Join(dir, "backups", fmt.Sprintf("pre-migrate-v7-to-v%d-*.db", store.CurrentSchemaVersion)))
	if err != nil || len(backups) != 1 {
		t.Fatal("migration backup missing")
	}
	m, err := NewManager(dir, upgraded)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	acc, _ := m.GetAccount("legacy")
	if acc == nil || acc.Cookies["token"] != "legacy-value" || acc.Session != nil {
		t.Fatal("migration changed legacy credentials")
	}
}

func TestInvalidBrowserSessionFailsClosedOnLoad(t *testing.T) {
	m := newPoolTestManager(t, 1)
	id := firstAccountIDs(m, 1)[0]
	if err := m.saveAccount(m.accounts[id]); err != nil {
		t.Fatal(err)
	}
	if err := m.store.UpdateAccountFields(id, map[string]interface{}{"cookies": `{"version":99}`}); err != nil {
		t.Fatal(err)
	}
	if loaded, err := NewManager(m.dataDir, m.store); err == nil {
		loaded.Close()
		t.Fatal("invalid format silently accepted")
	}
}

func TestBrowserSessionWriteFailureIsNeverReplayed(t *testing.T) {
	m := newPoolTestManager(t, 1)
	id := firstAccountIDs(m, 1)[0]
	s := accountSessionFixture()
	m.mu.Lock()
	m.accounts[id].Session = s
	m.accounts[id].Cookies = s.CookieMap()
	m.mu.Unlock()
	calls := 0
	err := m.WithHMEClient(id, func(*hme.Client) error { calls++; return hme.ErrAuthFailed })
	if !errors.Is(err, hme.ErrAuthFailed) || calls != 1 {
		t.Fatalf("write callback replayed: calls=%d err=%v", calls, err)
	}
	acc, _ := m.GetAccount(id)
	if acc.Session.Auth.SessionToken != s.Auth.SessionToken || acc.Session.RecoveryAfter != 0 {
		t.Fatal("business failure invoked authentication in-place")
	}
}

func TestSessionListRecoveryNeverSwallowsAuthenticationFailure(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		browser                 bool
		recoveryErr, errorAfter error
		wantLists, wantRecover  int
	}{
		{"legacy", false, nil, nil, 1, 0},
		{"challenge", true, hme.ErrOTPRequired, nil, 1, 1},
		{"cooldown", true, hme.ErrRecoveryDeferred, nil, 1, 1},
		{"still rejected", true, nil, hme.ErrAuthFailed, 2, 1},
		{"restored", true, nil, nil, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lists, recoveries := 0, 0
			_, err := listAliasesWithRecovery(func() ([]hme.Alias, error) {
				lists++
				if lists == 1 {
					return nil, hme.ErrAuthFailed
				}
				return []hme.Alias{}, tc.errorAfter
			}, func() error { recoveries++; return tc.recoveryErr }, tc.browser)
			if lists != tc.wantLists || recoveries != tc.wantRecover {
				t.Fatalf("wrong retry counts: %d %d", lists, recoveries)
			}
			if tc.name == "restored" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("authentication failure reported success")
			}
		})
	}
}
