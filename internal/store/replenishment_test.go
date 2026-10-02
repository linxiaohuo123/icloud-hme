package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"icloud-hme/internal/hme"
)

func TestReplenishmentCommitIsAtomicAndPreservesAllocation(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	insertTestAccount(t, st, "account")
	ctx := context.Background()
	const email = "atomic-replenish@icloud.com"
	intent, err := st.CreateReplenishmentReserveIntent(ctx, "account", email, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateReserveIntentState(ctx, intent.IntentID, IntentStateSucceeded, "anon", "", ""); err != nil {
		t.Fatal(err)
	}
	alias := hme.Alias{Email: email, AnonymousID: "anon", Active: true}
	if _, err := st.DB().Exec(`CREATE TRIGGER fail_replenish_route BEFORE INSERT ON alias_routes BEGIN SELECT RAISE(ABORT, 'route failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := st.AddReplenishedInventoryAlias(ctx, "account", alias); err == nil {
		t.Fatal("expected route failure")
	}
	if _, err := st.GetInventoryAlias(email); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("partial inventory write survived rollback: %v", err)
	}
	pending, err := st.ListUnresolvedReserveIntents(ctx, "account")
	if err != nil || len(pending) != 1 || pending[0].ResultRef != "" {
		t.Fatalf("failed commit lost recovery state: pending=%+v err=%v", pending, err)
	}
	if _, err := st.DB().Exec(`DROP TRIGGER fail_replenish_route`); err != nil {
		t.Fatal(err)
	}
	if err := st.AddReplenishedInventoryAlias(ctx, "account", alias); err != nil {
		t.Fatal(err)
	}
	allocation, _, err := st.ClaimInventoryAlias(ctx, "admin", "admin", "allocate", "atomic-key", "atomic-hash", "default", []string{"account"})
	if err != nil || allocation.AliasEmail != email {
		t.Fatalf("cannot claim replenishment: allocation=%+v err=%v", allocation, err)
	}
	if err := st.AddReplenishedInventoryAlias(ctx, "account", alias); err != nil {
		t.Fatal(err)
	}
	inv, err := st.GetInventoryAlias(email)
	if err != nil || inv.AllocationState != AllocationAllocated {
		t.Fatalf("repeated completion made allocated inventory available: inventory=%+v err=%v", inv, err)
	}
	if pending, err := st.ListUnresolvedReserveIntents(ctx, "account"); err != nil || len(pending) != 0 {
		t.Fatalf("completed replenishment is still pending: %+v %v", pending, err)
	}
}

func TestMigrationV8ToV9PreservesUnknownIntentPurpose(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := st.CreateReserveIntent(context.Background(), "legacy", "legacy@icloud.com", "original label")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`ALTER TABLE hme_reserve_intents DROP COLUMN purpose; PRAGMA user_version = 8;`); err != nil {
		t.Fatal(err)
	}
	if err := validateSchemaVersion(st.DB(), 8); err != nil {
		t.Fatalf("invalid v8 fixture: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		upgraded, err := NewStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer upgraded.Close()
		preserved, err := upgraded.GetReserveIntent(context.Background(), intent.IntentID)
		if err != nil || preserved.Purpose != "" || preserved.Label != "original label" || preserved.CandidateEmail != "legacy@icloud.com" || preserved.State != IntentStatePrepared {
			t.Fatalf("migration guessed purpose or changed historical intent: %+v %v", preserved, err)
		}
		if err := validateSchema(upgraded.DB()); err != nil {
			t.Fatal(err)
		}
		if err := upgraded.Close(); err != nil {
			t.Fatal(err)
		}
	}
	backups, err := filepath.Glob(filepath.Join(dir, "backups", fmt.Sprintf("pre-migrate-v8-to-v%d-*.db", CurrentSchemaVersion)))
	if err != nil || len(backups) != 1 {
		t.Fatalf("migration backup is missing or repeated: %v %v", backups, err)
	}
}
