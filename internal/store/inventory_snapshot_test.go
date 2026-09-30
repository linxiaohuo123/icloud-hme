package store

import (
	"context"
	"errors"
	"testing"

	"icloud-hme/internal/hme"
)

func TestInventorySnapshotRemovesMissingAliasesWithoutReleasingOwnership(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	insertTestAccount(t, st, "account_a")
	insertTestAccount(t, st, "account_b")
	for _, email := range []string{"gone@icloud.com", "allocated@icloud.com", "present@icloud.com", "inactive@icloud.com"} {
		if err := st.AddInventoryAlias("account_a", hme.Alias{Email: email, Active: true}, "replenish", true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB().Exec(`UPDATE alias_inventory SET allocation_state = 'allocated' WHERE email = 'allocated@icloud.com'`); err != nil {
		t.Fatal(err)
	}
	if err := st.AddInventoryAlias("account_b", hme.Alias{Email: "other@icloud.com", Active: true}, "replenish", true); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncAliasInventory("account_a", []hme.Alias{
		{Email: " PRESENT@icloud.com ", AnonymousID: "present_id", Active: true},
		{Email: "inactive@icloud.com", Active: false},
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		email      string
		remote     RemoteState
		allocation AllocationState
	}{
		{"gone@icloud.com", RemoteDeleted, AllocationQuarantined},
		{"allocated@icloud.com", RemoteDeleted, AllocationAllocated},
		{"present@icloud.com", RemoteActive, AllocationAvailable},
		{"inactive@icloud.com", RemoteInactive, AllocationAvailable},
		{"other@icloud.com", RemoteActive, AllocationAvailable},
	} {
		inv, err := st.GetInventoryAlias(want.email)
		if err != nil {
			t.Fatal(err)
		}
		if inv.RemoteState != want.remote || inv.AllocationState != want.allocation {
			t.Fatalf("unexpected state: %+v", inv)
		}
	}
	if err := st.SyncAliasInventory("account_a", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileAvailableInventory(); err != nil {
		t.Fatal(err)
	}
	_, _, err = st.ClaimInventoryAlias(context.Background(), "token", "worker", "allocate", "missing", "v2:missing", "default", []string{"account_a"})
	if !errors.Is(err, ErrNoAvailableInventory) {
		t.Fatalf("empty snapshot remained claimable: %v", err)
	}
}

func TestInventorySnapshotConflictRollsBackMissingMarkers(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, accountID := range []string{"account_a", "account_b"} {
		if err := st.AddInventoryAlias(accountID, hme.Alias{Email: accountID + "@icloud.com", Active: true}, "replenish", true); err != nil {
			t.Fatal(err)
		}
	}
	err = st.SyncAliasInventory("account_a", []hme.Alias{{Email: "account_b@icloud.com", Active: true}})
	if !errors.Is(err, ErrAllocationConflict) {
		t.Fatalf("cross-account snapshot accepted: %v", err)
	}
	for _, accountID := range []string{"account_a", "account_b"} {
		inv, err := st.GetInventoryAlias(accountID + "@icloud.com")
		if err != nil {
			t.Fatal(err)
		}
		if inv.AccountID != accountID || inv.RemoteState != RemoteActive || inv.AllocationState != AllocationAvailable {
			t.Fatalf("failed snapshot changed inventory: %+v", inv)
		}
	}
}
