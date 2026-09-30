package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/store"
)

func TestInvalidAliasJSONDoesNotMutate(t *testing.T) {
	for _, body := range []string{`{"account_id":"account_a","label":7}`, `{"account_id":"account_a","note":false}`, `{"account_id":"account_a","label":"ok"`, ""} {
		fb := &fakeBackend{}
		router := gin.New()
		router.PATCH("/aliases/:id", (&Server{be: fb}).updateAliasHandler)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("PATCH", "/aliases/anon_a?account_id=account_a", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || fb.aliasUpdateID != "" {
			t.Fatalf("invalid JSON mutated alias: body=%q status=%d id=%q", body, rec.Code, fb.aliasUpdateID)
		}
	}
}

func TestAliasRefreshSerializesSnapshotWithDeactivation(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mgr, err := account.NewManager(t.TempDir(), st)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/hme/list" {
			fmt.Fprint(w, `{"success":true,"result":{"hmeEmails":[{"hme":"race@icloud.com","anonymousId":"race_id","active":true}]}}`)
			return
		}
		fmt.Fprint(w, `{"success":true}`)
	}))
	defer upstream.Close()
	acc, err := mgr.AddAccountWithInput(account.AddAccountInput{Name: "race", ICloudEmail: "race@icloud.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.SaveSession(acc.ID, map[string]string{"X-APPLE-WEBAUTH-TOKEN": "test-token"}, upstream.URL); err != nil {
		t.Fatal(err)
	}
	if err := st.AddInventoryAlias(acc.ID, hme.Alias{Email: "race@icloud.com", AnonymousID: "race_id", Active: true}, "replenish", true); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	be := &managerBackend{mgr: mgr, store: st, onAliasesFetched: func(string, []hme.Alias) { close(entered); <-release }}
	done := make(chan error, 1)
	go func() { _, err := be.RefreshAliasesContext(context.Background(), acc.ID); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not reach barrier")
	}
	// 当快照尚未落库时，同账号修改必须等待，并响应请求取消。
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	ok, mutationErr := be.SetAliasActiveContext(ctx, acc.ID, "race_id", false)
	cancel()
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if ok || mutationErr == nil {
		t.Fatal("deactivation crossed uncommitted snapshot")
	}
	if ok, err := be.SetAliasActiveContext(context.Background(), acc.ID, "race_id", false); err != nil || !ok {
		t.Fatalf("deactivate failed: ok=%v err=%v", ok, err)
	}
	inv, err := st.GetInventoryAlias("race@icloud.com")
	if err != nil {
		t.Fatal(err)
	}
	if inv.RemoteState != store.RemoteInactive {
		t.Fatalf("snapshot overwrote deactivation: %+v", inv)
	}
}

func TestAliasRefreshReportsInventoryPersistenceFailure(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mgr, err := account.NewManager(t.TempDir(), st)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"success":true,"result":{"hmeEmails":[]}}`)
	}))
	defer upstream.Close()
	acc, err := mgr.AddAccountWithInput(account.AddAccountInput{Name: "refresh", ICloudEmail: "refresh@icloud.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.SaveSession(acc.ID, map[string]string{"X-APPLE-WEBAUTH-TOKEN": "test-token"}, upstream.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`DROP TABLE alias_inventory`); err != nil {
		t.Fatal(err)
	}
	be := &managerBackend{mgr: mgr, store: st}
	_, err = be.RefreshAliasesContext(context.Background(), acc.ID)
	var backendErr *BackendError
	if !errors.As(err, &backendErr) || backendErr.Code != "PERSISTENCE_ERROR" {
		t.Fatalf("inventory failure reported success: %v", err)
	}
	if _, cached := be.getCachedAliases(acc.ID); cached {
		t.Fatal("failed snapshot was cached")
	}
}

func TestIncompleteAliasSnapshotDoesNotQuarantineInventory(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mgr, err := account.NewManager(t.TempDir(), st)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"success":true,"result":{"hmeEmails":[{"hme":"known@icloud.com","active":true},{}]}}`)
	}))
	defer upstream.Close()
	acc, err := mgr.AddAccountWithInput(account.AddAccountInput{Name: "snapshot", ICloudEmail: "snapshot@icloud.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.SaveSession(acc.ID, map[string]string{"X-APPLE-WEBAUTH-TOKEN": "test-token"}, upstream.URL); err != nil {
		t.Fatal(err)
	}
	if err := st.AddInventoryAlias(acc.ID, hme.Alias{Email: "unparsed@icloud.com", Active: true}, "replenish", true); err != nil {
		t.Fatal(err)
	}
	be := &managerBackend{mgr: mgr, store: st}
	if _, err := be.RefreshAliasesContext(context.Background(), acc.ID); err == nil {
		t.Fatal("incomplete snapshot accepted")
	}
	inv, err := st.GetInventoryAlias("unparsed@icloud.com")
	if err != nil {
		t.Fatal(err)
	}
	if inv.RemoteState != store.RemoteActive || inv.AllocationState != store.AllocationAvailable {
		t.Fatalf("incomplete response changed inventory: %+v", inv)
	}
}
