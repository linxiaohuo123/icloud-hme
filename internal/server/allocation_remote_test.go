package server

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"icloud-hme/internal/auth"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/store"
)

func TestAllocateCreateBypassesStockAndReplaysCreatedSource(t *testing.T) {
	st, fb, ts := setupAllocTestServer(t)
	defer st.Close()
	defer ts.Close()
	if err := st.AddInventoryAlias("acc_1", hme.Alias{Email: "pooled@icloud.com", Active: true}, "replenish", true); err != nil {
		t.Fatal(err)
	}
	var creates atomic.Int32
	fb.onCreateAliasContext = func(context.Context, string, string) (*hme.CreateResult, error) {
		creates.Add(1)
		return &hme.CreateResult{Email: "created@icloud.com", AnonymousID: "anon_created"}, nil
	}
	svc := NewAliasAllocationService(st, fb, nil)
	p := auth.Principal{Kind: auth.PrincipalAdmin, ID: "admin"}
	req := AllocationRequest{Mode: "create", Tag: "default", IdempotencyKey: "create-key"}

	first, err := svc.Allocate(context.Background(), p, req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Allocate(context.Background(), p, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Source != "created" || second.Source != "created" || first.Allocation.AllocationID != second.Allocation.AllocationID || creates.Load() != 1 {
		t.Fatalf("create replay changed result or made another upstream call: first=%+v second=%+v calls=%d", first, second, creates.Load())
	}
	stock, err := st.GetInventoryAlias("pooled@icloud.com")
	if err != nil || stock.AllocationState != store.AllocationAvailable {
		t.Fatalf("create mode consumed pool stock: inventory=%+v err=%v", stock, err)
	}
	var leases, routes int
	if err := st.DB().QueryRow(`SELECT COUNT(1) FROM lease_records WHERE email = 'created@icloud.com'`).Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(1) FROM alias_routes WHERE email = 'created@icloud.com' AND account_id = 'acc_1'`).Scan(&routes); err != nil {
		t.Fatal(err)
	}
	if leases != 1 || routes != 1 {
		t.Fatalf("created allocation is missing audit or route: leases=%d routes=%d", leases, routes)
	}
}

func TestAllocateKeyedPoolFallbackReplaysCreatedSource(t *testing.T) {
	st, fb, ts := setupAllocTestServer(t)
	defer st.Close()
	defer ts.Close()
	var creates atomic.Int32
	fb.onCreateAliasContext = func(context.Context, string, string) (*hme.CreateResult, error) {
		creates.Add(1)
		return &hme.CreateResult{Email: "fallback@icloud.com", AnonymousID: "anon_fallback"}, nil
	}
	svc := NewAliasAllocationService(st, fb, nil)
	p := auth.Principal{Kind: auth.PrincipalAdmin, ID: "admin"}
	req := AllocationRequest{Mode: "pool", Tag: "default", IdempotencyKey: "pool-fallback-key"}
	first, err := svc.Allocate(context.Background(), p, req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Allocate(context.Background(), p, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Source != "created" || second.Source != "created" || first.Allocation.AllocationID != second.Allocation.AllocationID || creates.Load() != 1 {
		t.Fatalf("keyed pool fallback did not replay created result: first=%+v second=%+v calls=%d", first, second, creates.Load())
	}
}

func TestAllocateRejectsUnknownModeBeforeSideEffects(t *testing.T) {
	st, fb, ts := setupAllocTestServer(t)
	defer st.Close()
	defer ts.Close()
	if err := st.AddInventoryAlias("acc_1", hme.Alias{Email: "pooled@icloud.com", Active: true}, "replenish", true); err != nil {
		t.Fatal(err)
	}
	svc := NewAliasAllocationService(st, fb, nil)
	_, err := svc.Allocate(context.Background(), auth.Principal{Kind: auth.PrincipalAdmin, ID: "admin"}, AllocationRequest{Mode: "invalid", IdempotencyKey: "bad-mode"})
	if !errors.Is(err, ErrInvalidAllocationMode) {
		t.Fatalf("invalid mode was accepted: %v", err)
	}
	stock, err := st.GetInventoryAlias("pooled@icloud.com")
	if err != nil || stock.AllocationState != store.AllocationAvailable {
		t.Fatalf("invalid mode changed stock: inventory=%+v err=%v", stock, err)
	}
}
