package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"icloud-hme/internal/store"
)

func TestReplenishmentRecoversAfterInventoryFailureAndRestart(t *testing.T) {
	const accountID, email = "replenish-test", "replenish-recovery@icloud.com"
	var reserves atomic.Int32
	var st *store.Store
	upstream, be, initialStore, dir := setupFaultTestEnvironment(t, accountID, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/hme/generate":
			fmt.Fprintf(w, `{"success":true,"result":{"hme":%q}}`, email)
		case "/v1/hme/reserve":
			reserves.Add(1)
			intent, err := st.FindLatestIntentForCandidate(context.Background(), email)
			if err != nil || intent.Purpose != store.IntentPurposeReplenishment || intent.State != store.IntentStateReserveSent {
				t.Errorf("replenishment purpose was not durable before Reserve: intent=%+v err=%v", intent, err)
			}
			fmt.Fprintf(w, `{"success":true,"result":{"hme":{"hme":%q,"anonymousId":"replenish-anon"}}}`, email)
		case "/v2/hme/list":
			fmt.Fprintf(w, `{"success":true,"result":{"hmeEmails":[{"hme":%q,"anonymousId":"replenish-anon","isActive":true}]}}`, email)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	st = initialStore
	defer upstream.Close()
	defer be.mgr.Close()
	defer func() { st.Close() }()
	now := time.Now().UTC().Format(time.RFC3339)
	if err := st.SaveAccount(&store.AccountRecord{ID: accountID, Name: "Test", Status: "active", TagsJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	s := newWithBackendAndStore(be, Config{DataDir: dir, AdminPassword: "replenish-test-password"}, st)
	defer s.Close()
	if _, err := st.DB().Exec(`CREATE TRIGGER fail_replenish BEFORE INSERT ON alias_inventory BEGIN SELECT RAISE(ABORT, 'inventory failure'); END`); err != nil {
		t.Fatal(err)
	}
	if created, failed := s.scheduler.RunAllNow(1); created != 0 || failed != 1 {
		t.Fatalf("failed commit was counted successful: created=%d failed=%d", created, failed)
	}
	if _, err := st.DB().Exec(`DROP TRIGGER fail_replenish`); err != nil {
		t.Fatal(err)
	}
	// A refresh before recovery must not lose the pending replenishment proof.
	if _, err := be.RefreshAliases(accountID); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	st, err = store.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	restarted := &managerBackend{mgr: be.mgr, store: st}
	if recovered, err := restarted.ReconcileUnresolvedIntents(context.Background()); err != nil || len(recovered) != 1 {
		t.Fatalf("restart failed to recover replenishment: recovered=%+v err=%v", recovered, err)
	}
	inv, err := st.GetInventoryAlias(email)
	if err != nil || inv.AllocationState != store.AllocationAvailable || inv.SourceType != "replenish" {
		t.Fatalf("replenishment did not recover as available: inventory=%+v err=%v", inv, err)
	}
	intent, err := st.FindLatestIntentForCandidate(context.Background(), email)
	if err != nil || !strings.HasPrefix(intent.ResultRef, "inventory:") {
		t.Fatalf("inventory commit was not recorded: intent=%+v err=%v", intent, err)
	}
	if _, _, err := st.ClaimInventoryAlias(context.Background(), "admin", "admin", "allocate", "recovery-key", "recovery-hash", "default", []string{accountID}); err != nil {
		t.Fatalf("recovered replenishment cannot be allocated: %v", err)
	}
	if recovered, err := restarted.ReconcileUnresolvedIntents(context.Background()); err != nil || len(recovered) != 0 || reserves.Load() != 1 {
		t.Fatalf("recovery repeated Reserve or completion: recovered=%+v reserves=%d err=%v", recovered, reserves.Load(), err)
	}
}
