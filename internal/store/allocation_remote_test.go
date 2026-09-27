package store

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestRemoteAllocationCommitFailureRemainsRecoverable(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	_, op, err := st.BeginRemoteAllocation(ctx, "admin", "admin", "v2_allocate", "fault-key", "v2:fault", "default", "admin_console")
	if err != nil {
		t.Fatal(err)
	}
	intent, err := st.CreateReserveIntentForOperation(ctx, op.OperationID, "acc_1", "fault@icloud.com", "label")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateReserveIntentState(ctx, intent.IntentID, IntentStateSucceeded, "anon_fault", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`CREATE TRIGGER fail_remote_allocation BEFORE INSERT ON alias_allocations BEGIN SELECT RAISE(FAIL, 'injected allocation failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecoverRemoteAllocationOperation(ctx, op.OperationID); err == nil {
		t.Fatal("allocation write failure was not reported")
	}
	if _, _, err := st.BeginRemoteAllocation(ctx, "admin", "admin", "v2_allocate", "fault-key", "v2:fault", "default", "admin_console"); !errors.Is(err, ErrOperationPending) {
		t.Fatalf("same key retried after failed local transaction: %v", err)
	}
	var inventory, allocations, leases int
	if err := st.DB().QueryRow(`SELECT COUNT(1) FROM alias_inventory WHERE email = 'fault@icloud.com'`).Scan(&inventory); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(1) FROM alias_allocations WHERE alias_email = 'fault@icloud.com'`).Scan(&allocations); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(1) FROM lease_records WHERE email = 'fault@icloud.com'`).Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if inventory != 0 || allocations != 0 || leases != 0 {
		t.Fatalf("partial allocation escaped rollback: inventory=%d allocations=%d leases=%d", inventory, allocations, leases)
	}
	if _, err := st.DB().Exec(`DROP TRIGGER fail_remote_allocation`); err != nil {
		t.Fatal(err)
	}
	alloc, err := st.RecoverRemoteAllocationOperation(ctx, op.OperationID)
	if err != nil || alloc == nil {
		t.Fatalf("original intent was not recoverable: allocation=%+v err=%v", alloc, err)
	}
}

func TestConcurrentRemoteAllocationSameKeyCreatesOneOperation(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const requests = 30
	var wg sync.WaitGroup
	var created, pending int
	var mu sync.Mutex
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, op, err := st.BeginRemoteAllocation(context.Background(), "admin", "admin", "v2_allocate", "parallel-key", "v2:parallel", "default", "admin_console")
			mu.Lock()
			defer mu.Unlock()
			if err == nil && op != nil {
				created++
			} else if errors.Is(err, ErrOperationPending) {
				pending++
			} else {
				t.Errorf("unexpected concurrent result: operation=%+v err=%v", op, err)
			}
		}()
	}
	wg.Wait()
	if created != 1 || pending != requests-1 {
		t.Fatalf("same key claimed %d times, pending %d times", created, pending)
	}
}

func TestRemoteAllocationRecoveryAfterRestart(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	st, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, op, err := st.BeginRemoteAllocation(ctx, "admin", "admin", "v2_allocate", "recover-key", "v2:recover", "default", "admin_console")
	if err != nil {
		t.Fatal(err)
	}
	intent, err := st.CreateReserveIntentForOperation(ctx, op.OperationID, "acc_1", "recovered@icloud.com", "label")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateReserveIntentState(ctx, intent.IntentID, IntentStateSucceeded, "anon_recovered", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	recovered, err := st.RecoverRemoteAllocationOperations(ctx)
	if err != nil || recovered != 1 {
		t.Fatalf("restart recovery failed: count=%d err=%v", recovered, err)
	}
	alloc, replay, err := st.BeginRemoteAllocation(ctx, "admin", "admin", "v2_allocate", "recover-key", "v2:recover", "default", "admin_console")
	if err != nil || alloc == nil || alloc.AliasEmail != "recovered@icloud.com" || replay.ResultSource != "created" {
		t.Fatalf("recovered operation did not replay: allocation=%+v operation=%+v err=%v", alloc, replay, err)
	}
	inv, err := st.GetInventoryAlias("recovered@icloud.com")
	if err != nil || inv.AllocationState != AllocationAllocated {
		t.Fatalf("recovered inventory is not allocated: inventory=%+v err=%v", inv, err)
	}
	var leases, routes int
	if err := st.DB().QueryRow(`SELECT COUNT(1) FROM lease_records WHERE email = ?`, alloc.AliasEmail).Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(1) FROM alias_routes WHERE email = ? AND account_id = ?`, alloc.AliasEmail, alloc.AccountID).Scan(&routes); err != nil {
		t.Fatal(err)
	}
	if leases != 1 || routes != 1 {
		t.Fatalf("recovery omitted audit or route: leases=%d routes=%d", leases, routes)
	}
}

func TestUnresolvedRemoteAllocationBlocksSameKeyUntilConfirmed(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	_, op, err := st.BeginRemoteAllocation(ctx, "admin", "admin", "v2_allocate", "unknown-key", "v2:unknown", "default", "admin_console")
	if err != nil {
		t.Fatal(err)
	}
	intent, err := st.CreateReserveIntentForOperation(ctx, op.OperationID, "acc_1", "candidate@icloud.com", "label")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateReserveIntentState(ctx, intent.IntentID, IntentStateReserveSent, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecoverRemoteAllocationOperation(ctx, op.OperationID); err != nil {
		t.Fatal(err)
	}
	_, _, err = st.BeginRemoteAllocation(ctx, "admin", "admin", "v2_allocate", "unknown-key", "v2:unknown", "default", "admin_console")
	if !errors.Is(err, ErrOperationOutcomeUnknown) {
		t.Fatalf("unresolved Reserve allowed the same key to create again: %v", err)
	}
	if err := st.UpdateReserveIntentState(ctx, intent.IntentID, IntentStateSucceeded, "anon_candidate", "", ""); err != nil {
		t.Fatal(err)
	}
	alloc, err := st.RecoverRemoteAllocationOperation(ctx, op.OperationID)
	if err != nil || alloc == nil || alloc.AliasEmail != intent.CandidateEmail {
		t.Fatalf("confirmed candidate did not finish original operation: allocation=%+v err=%v", alloc, err)
	}
}
