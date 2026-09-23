package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/auth"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

// C01 available 邮箱停用/删除成功后不能分配；激活已 allocated 邮箱不会重新 available。
func TestC01_LifecycleEligibilityTransition(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fb := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_c01", Status: "active", HasCookies: true, Tags: []string{"default"}},
		},
		store: st,
	}
	cfg := Config{
		AdminPassword: "admin-pass-strong-2026",
	}
	s := newWithBackendAndStore(fb, cfg, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	cookie, csrf := login(t, ts, "admin-pass-strong-2026")

	// 1. 存入一个 available 别名
	err = st.AddInventoryAlias("acc_c01", hme.Alias{Email: "avail@example.com", AnonymousID: "ano_c01", Active: true}, "replenish", true)
	if err != nil {
		t.Fatalf("AddInventoryAlias failed: %v", err)
	}
	if cnt := st.CountAuthoritativeAvailableAliases(); cnt != 1 {
		t.Fatalf("expected 1 available alias, got %d", cnt)
	}

	// 2. 调用停用接口 POST /api/aliases/:id/deactivate
	reqDeact, _ := http.NewRequest("POST", ts.URL+"/api/aliases/ano_c01/deactivate", strings.NewReader(`{"account_id":"acc_c01"}`))
	reqDeact.Header.Set("Content-Type", "application/json")
	reqDeact.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	reqDeact.Header.Set("X-CSRF-Token", csrf)
	respDeact, err := http.DefaultClient.Do(reqDeact)
	if err != nil {
		t.Fatal(err)
	}
	respDeact.Body.Close()
	if respDeact.StatusCode != http.StatusOK {
		t.Fatalf("deactivate failed: %d", respDeact.StatusCode)
	}

	// 验证底层 alias_inventory 的 remote_state 变为 inactive
	inv, err := st.GetInventoryAlias("avail@example.com")
	if err != nil || inv == nil {
		t.Fatalf("GetInventoryAlias failed: %v", err)
	}
	if inv.RemoteState != store.RemoteInactive {
		t.Fatalf("C01 FAILED: expected remote_state inactive, got %s", inv.RemoteState)
	}
	if cnt := st.CountAuthoritativeAvailableAliases(); cnt != 0 {
		t.Fatalf("C01 FAILED: available count should be 0 after deactivation, got %d", cnt)
	}

	// 尝试认领：此时已停用，绝不能被分配
	alloc, _, err := st.ClaimInventoryAlias(context.Background(), "admin", "admin", "allocate", "key_c01", "hash", "default", []string{"acc_c01"})
	if !errors.Is(err, store.ErrNoAvailableInventory) || alloc != nil {
		t.Fatalf("C01 FAILED: deactivated alias was claimed: alloc=%v, err=%v", alloc, err)
	}

	// 3. 激活已 allocated 邮箱：绝不会重新变成 available
	_ = st.AddInventoryAlias("acc_c01", hme.Alias{Email: "alloc@example.com", AnonymousID: "ano_alloc", Active: true}, "replenish", true)
	allocRecord := &store.AliasAllocation{
		AllocationID: "alloc_rec_c01",
		AliasEmail:   "alloc@example.com",
		AccountID:    "acc_c01",
		OwnerKind:    "token",
		OwnerID:      "tok_user1",
		BusinessTag:  "default",
		AllocatedAt:  "2026-09-20T00:00:00Z",
		Status:       "allocated",
	}
	_, err = st.RecordAllocation(allocRecord, "bot")
	if err != nil {
		t.Fatalf("RecordAllocation failed: %v", err)
	}

	// 先将已分配别名在远端和本地停用为 inactive
	_ = st.UpdateAliasRemoteState("acc_c01", "ano_alloc", "alloc@example.com", store.RemoteInactive)

	// 调用激活接口 POST /api/aliases/:id/reactivate
	reqReact, _ := http.NewRequest("POST", ts.URL+"/api/aliases/ano_alloc/reactivate", strings.NewReader(`{"account_id":"acc_c01"}`))
	reqReact.Header.Set("Content-Type", "application/json")
	reqReact.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	reqReact.Header.Set("X-CSRF-Token", csrf)
	respReact, err := http.DefaultClient.Do(reqReact)
	if err != nil {
		t.Fatal(err)
	}
	respReact.Body.Close()
	if respReact.StatusCode != http.StatusOK {
		t.Fatalf("reactivate failed: %d", respReact.StatusCode)
	}

	invAlloc, err := st.GetInventoryAlias("alloc@example.com")
	if err != nil || invAlloc == nil {
		t.Fatalf("GetInventoryAlias failed: %v", err)
	}
	if invAlloc.RemoteState != store.RemoteActive {
		t.Fatalf("C01 FAILED: expected remote_state active, got %s", invAlloc.RemoteState)
	}
	if invAlloc.AllocationState != store.AllocationAllocated {
		t.Fatalf("C01 FAILED: allocation_state must remain allocated, got %s", invAlloc.AllocationState)
	}
	// 尝试认领：绝不能被任何人重新作为可用库存分配
	allocSecond, _, err := st.ClaimInventoryAlias(context.Background(), "admin", "admin", "allocate", "key_c01_2", "hash", "default", []string{"acc_c01"})
	if !errors.Is(err, store.ErrNoAvailableInventory) || allocSecond != nil {
		t.Fatalf("C01 FAILED: reactivated allocated alias was stolen/re-allocated: alloc=%v", allocSecond)
	}

	// 4. 删除 available 别名：状态变为 deleted，allocation_state 变为 quarantined
	_ = st.AddInventoryAlias("acc_c01", hme.Alias{Email: "del@example.com", AnonymousID: "ano_del", Active: true}, "replenish", true)
	reqDel, _ := http.NewRequest("DELETE", ts.URL+"/api/aliases/ano_del", strings.NewReader(`{"account_id":"acc_c01"}`))
	reqDel.Header.Set("Content-Type", "application/json")
	reqDel.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	reqDel.Header.Set("X-CSRF-Token", csrf)
	respDel, err := http.DefaultClient.Do(reqDel)
	if err != nil {
		t.Fatal(err)
	}
	respDel.Body.Close()
	if respDel.StatusCode != http.StatusOK {
		t.Fatalf("delete failed: %d", respDel.StatusCode)
	}

	invDel, err := st.GetInventoryAlias("del@example.com")
	if err != nil || invDel == nil {
		t.Fatalf("GetInventoryAlias failed: %v", err)
	}
	if invDel.RemoteState != store.RemoteDeleted {
		t.Fatalf("C01 FAILED: expected remote_state deleted, got %s", invDel.RemoteState)
	}
	if invDel.AllocationState != store.AllocationQuarantined {
		t.Fatalf("C01 FAILED: expected allocation_state quarantined, got %s", invDel.AllocationState)
	}
}

// C02 上游 false/timeout/本地写入失败有明确失败或待核对状态，不返回完整成功。
func TestC02_UpstreamFailureAndLocalStoreFailure(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fb := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_c02", Status: "active", HasCookies: true, Tags: []string{"default"}},
		},
		store: st,
	}
	cfg := Config{AdminPassword: "admin-pass-strong-2026"}
	s := newWithBackendAndStore(fb, cfg, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	cookie, csrf := login(t, ts, "admin-pass-strong-2026")
	_ = st.AddInventoryAlias("acc_c02", hme.Alias{Email: "test_c02@example.com", AnonymousID: "ano_c02", Active: true}, "replenish", true)

	// 1. 上游返回 success=false (err=nil)
	fb.onSetAliasActive = func(accountID, anonymousID string, active bool) (bool, error) {
		return false, nil
	}

	req, _ := http.NewRequest("POST", ts.URL+"/api/aliases/ano_c02/deactivate", strings.NewReader(`{"account_id":"acc_c02"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Fatalf("C02 FAILED: upstream success=false returned 200 OK")
	}

	// 验证本地库存没有错误地被改为 inactive
	inv, _ := st.GetInventoryAlias("test_c02@example.com")
	if inv.RemoteState != store.RemoteActive {
		t.Fatalf("C02 FAILED: local inventory was updated on upstream failure")
	}

	// 2. 上游超时或报错
	fb.onSetAliasActive = func(accountID, anonymousID string, active bool) (bool, error) {
		return false, errors.New("upstream timeout")
	}
	req2, _ := http.NewRequest("POST", ts.URL+"/api/aliases/ano_c02/deactivate", strings.NewReader(`{"account_id":"acc_c02"}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	req2.Header.Set("X-CSRF-Token", csrf)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode == http.StatusOK {
		t.Fatalf("C02 FAILED: upstream error returned 200 OK")
	}

	// 3. 上游成功，但本地 SQLite 写入失败 (故障注入)
	fb.onSetAliasActive = func(accountID, anonymousID string, active bool) (bool, error) {
		return true, nil
	}
	_, err = st.DB().Exec(`CREATE TRIGGER trigger_fail_inv_c02 BEFORE UPDATE ON alias_inventory BEGIN SELECT RAISE(FAIL, 'db write failure'); END;`)
	if err != nil {
		t.Fatal(err)
	}

	req3, _ := http.NewRequest("POST", ts.URL+"/api/aliases/ano_c02/deactivate", strings.NewReader(`{"account_id":"acc_c02"}`))
	req3.Header.Set("Content-Type", "application/json")
	req3.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
	req3.Header.Set("X-CSRF-Token", csrf)
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode == http.StatusOK {
		t.Fatalf("C02 FAILED: local store error returned 200 OK instead of failure")
	}
}

// C03 succeeded 和 expiry/invalidation 交错：数据库 winner 与所有最终响应一致。
func TestC03_InterleavingWinnerAuthoritative(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fb := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_c03", Status: "active", HasCookies: true, Tags: []string{"default"}},
		},
	}
	eb := mail.NewEventBus(10 * time.Minute)
	vService := NewVerificationService(fb, st, eb, nil)

	ctx := context.Background()
	p := auth.Principal{
		Kind:   auth.PrincipalToken,
		ID:     "tok_c03",
		Scopes: []string{"verify"},
	}
	_ = st.SaveToken(store.APIToken{ID: "tok_c03", Name: "tok_c03", Token: "sec_c03", Scopes: "verify"})

	// 1. 在 DB 中已记录为 succeeded，但 expires_at 已经过去
	pastTime := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	vreqSucc := &store.VerificationRequest{
		RequestID:           "vreq_succ",
		PrincipalKind:       "token",
		PrincipalID:         "tok_c03",
		LeaseID:             "lease_c03_1",
		AliasEmail:          "succ@example.com",
		Status:              "succeeded",
		Code:                "987654",
		CreatedAt:           pastTime,
		ExpiresAt:           pastTime,
		BaselineProvider:    "icloud",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1,
		BaselineUID:         100,
	}
	_ = st.CreateVerificationRequestAtomic(ctx, vreqSucc, 100, 100)
	_ = st.UpdateVerificationRequestResult(ctx, "vreq_succ", "succeeded", "987654", "ev_succ")

	// 读取取码结果：尽管过期时间已过，数据库 winner 为 succeeded，绝不能返回 expired！
	res1, err := vService.GetVerificationResult(ctx, p, "vreq_succ", 0)
	if err != nil {
		t.Fatalf("GetVerificationResult failed: %v", err)
	}
	if res1.Status != "succeeded" || res1.Code != "987654" {
		t.Fatalf("C03 FAILED: expected succeeded with code 987654, got status=%s code=%s", res1.Status, res1.Code)
	}

	// 2. 在 DB 中已记录为 invalidated，但 expires_at 已经过去
	vreqInv := &store.VerificationRequest{
		RequestID:           "vreq_inv",
		PrincipalKind:       "token",
		PrincipalID:         "tok_c03",
		LeaseID:             "lease_c03_2",
		AliasEmail:          "inv@example.com",
		Status:              "invalidated",
		CreatedAt:           pastTime,
		ExpiresAt:           pastTime,
		BaselineProvider:    "icloud",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1,
		BaselineUID:         100,
	}
	_ = st.CreateVerificationRequestAtomic(ctx, vreqInv, 100, 100)
	_, _, _ = st.InvalidateVerificationRequest(ctx, "vreq_inv")

	// 读取取码结果：绝不能因为时间过去把 invalidated 篡改成 expired，必须返回 ErrUIDValidityChanged！
	res2, err := vService.GetVerificationResult(ctx, p, "vreq_inv", 0)
	if !errors.Is(err, ErrUIDValidityChanged) {
		t.Fatalf("C03 FAILED: expected ErrUIDValidityChanged, got res=%v, err=%v", res2, err)
	}

	// 3. 校验数据库中的状态没有被篡改成 expired
	checkReq, _ := st.GetVerificationRequest(ctx, "vreq_inv", "token", "tok_c03")
	if checkReq.Status != "invalidated" {
		t.Fatalf("C03 FAILED: database status was mutated to %s", checkReq.Status)
	}
}

// 并发交错测试：一个请求在长轮询等待中，并发请求或后台任务写入 expired 或 succeeded，
// 等待唤醒/超时后准确返回数据库真实终态，不得误报 pending 或覆盖。
func TestC03_ConcurrentInterleaving_ExpiredAndSucceededDuringWait(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fb := &fakeBackend{}
	eb := mail.NewEventBus(10 * time.Minute)
	vService := NewVerificationService(fb, st, eb, nil)

	ctx := context.Background()
	p := auth.Principal{
		Kind:   auth.PrincipalToken,
		ID:     "tok_interleave",
		Scopes: []string{"verify"},
	}
	_ = st.SaveToken(store.APIToken{ID: "tok_interleave", Name: "tok_interleave", Token: "sec_interleave", Scopes: "verify"})

	// 1. 测试等待期间并发写入 expired
	now := time.Now().UTC()
	vreqExp := &store.VerificationRequest{
		RequestID:           "vreq_wait_exp",
		PrincipalKind:       "token",
		PrincipalID:         "tok_interleave",
		LeaseID:             "lease_exp",
		AliasEmail:          "wait_exp@example.com",
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "icloud",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1,
		BaselineUID:         100,
	}
	_ = st.CreateVerificationRequestAtomic(ctx, vreqExp, 100, 100)

	doneExp := make(chan *VerificationResult, 1)
	errExp := make(chan error, 1)
	go func() {
		res, err := vService.GetVerificationResult(ctx, p, "vreq_wait_exp", 1)
		errExp <- err
		doneExp <- res
	}()

	// 稍等以确认已进入订阅与定时器等待
	time.Sleep(30 * time.Millisecond)

	// 并发协程将 DB 中该记录标记为 expired
	_, won, _ := st.ExpireVerificationRequest(ctx, "vreq_wait_exp")
	if !won {
		t.Fatal("expected ExpireVerificationRequest to win CAS")
	}

	select {
	case err := <-errExp:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		res := <-doneExp
		if res == nil || res.Status != "expired" {
			t.Fatalf("expected real DB final status expired, got: %v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for GetVerificationResult")
	}

	// 2. 测试等待期间并发写入 succeeded
	vreqSucc := &store.VerificationRequest{
		RequestID:           "vreq_wait_succ",
		PrincipalKind:       "token",
		PrincipalID:         "tok_interleave",
		LeaseID:             "lease_succ",
		AliasEmail:          "wait_succ@example.com",
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "icloud",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 1,
		BaselineUID:         100,
	}
	_ = st.CreateVerificationRequestAtomic(ctx, vreqSucc, 100, 100)

	doneSucc := make(chan *VerificationResult, 1)
	errSucc := make(chan error, 1)
	go func() {
		res, err := vService.GetVerificationResult(ctx, p, "vreq_wait_succ", 1)
		errSucc <- err
		doneSucc <- res
	}()

	time.Sleep(30 * time.Millisecond)

	// 并发协程将 DB 中该记录标记为 succeeded
	_, won, _ = st.CompleteVerificationRequest(ctx, "vreq_wait_succ", "123456", "ev_succ_interleave")
	if !won {
		t.Fatal("expected CompleteVerificationRequest to win CAS")
	}

	select {
	case err := <-errSucc:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		res := <-doneSucc
		if res == nil || res.Status != "succeeded" || res.Code != "123456" {
			t.Fatalf("expected real DB final status succeeded with code 123456, got: %v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for GetVerificationResult")
	}
}

// 测试 UIDVALIDITY 突变发生时，CAS 如果输给并发完成的 expired 或 succeeded，严格返回数据库真实胜出终态
func TestC03_UIDValidityMutation_LosesToExpiredOrSucceeded(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fb := &fakeBackend{}
	eb := mail.NewEventBus(10 * time.Minute)
	vService := NewVerificationService(fb, st, eb, nil)

	ctx := context.Background()
	p := auth.Principal{
		Kind:   auth.PrincipalToken,
		ID:     "tok_cas_loser",
		Scopes: []string{"verify"},
	}
	_ = st.SaveToken(store.APIToken{ID: "tok_cas_loser", Name: "tok_cas_loser", Token: "sec_cas", Scopes: "verify"})

	now := time.Now().UTC()

	// 1. UIDVALIDITY 突变输给 expired：严格返回 expired
	vreqExp := &store.VerificationRequest{
		RequestID:           "vreq_uid_exp",
		PrincipalKind:       "token",
		PrincipalID:         "tok_cas_loser",
		LeaseID:             "lease_cas_1",
		AliasEmail:          "uid_exp@example.com",
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "icloud",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 10,
		BaselineUID:         100,
	}
	_ = st.CreateVerificationRequestAtomic(ctx, vreqExp, 100, 100)

	// 先将其置为 expired
	_, won, _ := st.ExpireVerificationRequest(ctx, "vreq_uid_exp")
	if !won {
		t.Fatal("ExpireVerificationRequest failed")
	}

	// 此时推送一个带有不同 UIDValidity (20 != 10) 的事件
	eb.PublishEvent(&mail.CachedOTP{
		EventID:     "ev_uid_1",
		Email:       "uid_exp@example.com",
		UIDValidity: 20,
		UID:         101,
		OTP:         &mail.OTPResult{Code: "000000"},
	})

	// 获取结果：此时执行 InvalidateVerificationRequest 会输给 expired，必须准确返回 expired！
	resExp, err := vService.GetVerificationResult(ctx, p, "vreq_uid_exp", 0)
	if err != nil {
		t.Fatalf("expected nil err for expired status, got: %v", err)
	}
	if resExp == nil || resExp.Status != "expired" {
		t.Fatalf("expected status expired when losing CAS to expired, got: %v", resExp)
	}

	// 2. UIDVALIDITY 突变输给 succeeded：严格返回 succeeded
	vreqSucc := &store.VerificationRequest{
		RequestID:           "vreq_uid_succ",
		PrincipalKind:       "token",
		PrincipalID:         "tok_cas_loser",
		LeaseID:             "lease_cas_2",
		AliasEmail:          "uid_succ@example.com",
		Status:              "ready",
		CreatedAt:           now.Format(time.RFC3339),
		ExpiresAt:           now.Add(10 * time.Minute).Format(time.RFC3339),
		BaselineProvider:    "icloud",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 10,
		BaselineUID:         100,
	}
	_ = st.CreateVerificationRequestAtomic(ctx, vreqSucc, 100, 100)

	// 先将其置为 succeeded
	_, won, _ = st.CompleteVerificationRequest(ctx, "vreq_uid_succ", "777888", "ev_prior_succ")
	if !won {
		t.Fatal("CompleteVerificationRequest failed")
	}

	// 推送带有不同 UIDValidity 的事件
	eb.PublishEvent(&mail.CachedOTP{
		EventID:     "ev_uid_2",
		Email:       "uid_succ@example.com",
		UIDValidity: 20,
		UID:         102,
		OTP:         &mail.OTPResult{Code: "000000"},
	})

	// 获取结果：此时执行 InvalidateVerificationRequest 会输给 succeeded，必须准确返回 succeeded！
	resSucc, err := vService.GetVerificationResult(ctx, p, "vreq_uid_succ", 0)
	if err != nil {
		t.Fatalf("expected nil err for succeeded status, got: %v", err)
	}
	if resSucc == nil || resSucc.Status != "succeeded" || resSucc.Code != "777888" {
		t.Fatalf("expected status succeeded with code 777888 when losing CAS to succeeded, got: %v", resSucc)
	}
}

// C04 消费事件 A 时 B 后到，B 不被旧版接口清除。
func TestC04_SingleEventConsumptionPreservesConcurrentEvents(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fb := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_c04", Status: "active", HasCookies: true, Tags: []string{"default"}},
		},
	}
	cfg := Config{AdminPassword: "admin-pass-strong-2026"}
	s := newWithBackendAndStore(fb, cfg, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	ctx := context.Background()
	_ = st.SaveToken(store.APIToken{ID: "tok_c04", Name: "tok_c04", Token: "sec_c04", Scopes: "verify"})
	_ = st.AddInventoryAlias("acc_c04", hme.Alias{Email: "multi@example.com", Active: true}, "replenish", true)
	alloc := &store.AliasAllocation{
		AllocationID: "alloc_c04",
		AliasEmail:   "multi@example.com",
		AccountID:    "acc_c04",
		OwnerKind:    "token",
		OwnerID:      "tok_c04",
		BusinessTag:  "default",
		AllocatedAt:  "2026-09-20T00:00:00Z",
		Status:       "allocated",
	}
	_, _ = st.RecordAllocation(alloc, "tok_c04")

	// 注入事件 A 和事件 B
	s.eventBus.PublishEvent(&mail.CachedOTP{
		EventID: "ev_A",
		Email:   "multi@example.com",
		OTP:     &mail.OTPResult{Code: "111111"},
		Date:    time.Now().Format(time.RFC3339),
	})
	s.eventBus.PublishEvent(&mail.CachedOTP{
		EventID: "ev_B",
		Email:   "multi@example.com",
		OTP:     &mail.OTPResult{Code: "222222"},
		Date:    time.Now().Format(time.RFC3339),
	})

	// 调用 GET /api/verify-code 取码
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/verify-code?email=multi@example.com&timeout=1", nil)
	req.Header.Set("Authorization", "Bearer sec_c04")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got: %d", resp.StatusCode)
	}

	var body map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	data, _ := body["data"].(map[string]interface{})
	if data == nil {
		data = body
	}
	if data["code"] != "111111" {
		t.Fatalf("expected code 111111 for ev_A, got: %v", data["code"])
	}

	// 核心断言：事件 B 绝不能被 ConsumeCache 整桶删掉！事件 B 必须依然保留在 cache 中
	cachedB := s.eventBus.GetCached("multi@example.com")
	if cachedB == nil || cachedB.EventID != "ev_B" || cachedB.OTP.Code != "222222" {
		t.Fatalf("C04 FAILED: event B was erroneously wiped out by ConsumeCache: cached=%v", cachedB)
	}
}

// C05 Token A/B 隔离与撤销检查不回退。
func TestC05_TokenIsolationAndRevocationDuringWait(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fb := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_c05", Status: "active", HasCookies: true, Tags: []string{"default"}},
		},
	}
	cfg := Config{AdminPassword: "admin-pass-strong-2026"}
	s := newWithBackendAndStore(fb, cfg, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	_ = st.SaveToken(store.APIToken{ID: "tok_a", Name: "tok_a", Token: "sec_a", Scopes: "verify"})
	_ = st.SaveToken(store.APIToken{ID: "tok_b", Name: "tok_b", Token: "sec_b", Scopes: "verify"})

	// 分配别名给 tok_a
	_ = st.AddInventoryAlias("acc_c05", hme.Alias{Email: "alias_a@example.com", Active: true}, "replenish", true)
	allocA := &store.AliasAllocation{
		AllocationID: "alloc_c05_a",
		AliasEmail:   "alias_a@example.com",
		AccountID:    "acc_c05",
		OwnerKind:    "token",
		OwnerID:      "tok_a",
		BusinessTag:  "default",
		AllocatedAt:  "2026-09-20T00:00:00Z",
		Status:       "allocated",
	}
	_, _ = st.RecordAllocation(allocA, "tok_a")

	// 1. tok_b 试图读取 tok_a 的别名验证码 -> 404 RESOURCE_NOT_FOUND
	reqB, _ := http.NewRequest("GET", ts.URL+"/api/verify-code?email=alias_a@example.com&timeout=1", nil)
	reqB.Header.Set("Authorization", "Bearer sec_b")
	respB, err := http.DefaultClient.Do(reqB)
	if err != nil {
		t.Fatal(err)
	}
	respB.Body.Close()
	if respB.StatusCode != http.StatusNotFound {
		t.Fatalf("C05 FAILED: token B was able to access token A alias, status=%d", respB.StatusCode)
	}

	// 2. tok_a 长轮询等待验证码期间，token 被撤销
	doneCh := make(chan int)
	go func() {
		reqA, _ := http.NewRequest("GET", ts.URL+"/api/verify-code?email=alias_a@example.com&timeout=3", nil)
		reqA.Header.Set("Authorization", "Bearer sec_a")
		respA, err := http.DefaultClient.Do(reqA)
		if err != nil {
			doneCh <- 0
			return
		}
		defer respA.Body.Close()
		doneCh <- respA.StatusCode
	}()

	// 确认长轮询已建立
	time.Sleep(50 * time.Millisecond)

	// 撤销 tok_a
	_, _ = st.DeleteToken("tok_a")

	// 此时邮件到达，唤醒长轮询
	s.eventBus.PublishEvent(&mail.CachedOTP{
		EventID: "ev_c05",
		Email:   "alias_a@example.com",
		OTP:     &mail.OTPResult{Code: "654321"},
		Date:    time.Now().Format(time.RFC3339),
	})

	select {
	case code := <-doneCh:
		// 唤醒后复查发现被撤销，必须返回 401 Unauthorized，绝不能泄露验证码！
		if code != http.StatusUnauthorized {
			t.Fatalf("C05 FAILED: revoked token received status %d, expected 401", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("C05 FAILED: timeout waiting for verify-code response")
	}
}

// TestLifecycle_SchedulerReplenishedAlias_DeactivateAndDelete_ResolvesAnonymousID 验证
// Scheduler 补货别名在无 provider_alias_id 时，通过 upstream 远端解析 anonymousID/email
// 可靠停用并把本地库存置为 inactive/quarantined，不再处于 available 状态。
func TestLifecycle_SchedulerReplenishedAlias_DeactivateAndDelete_ResolvesAnonymousID(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = st.DB().Exec(`INSERT INTO accounts (id, name, real_email, status, tags, created_at, updated_at)
		VALUES ('acc_sched', 'Sched Account', 'sched@test.com', 'active', '[]', ?, ?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}

	// 1. 模拟历史/补货入库：provider_alias_id 为空，仅有 email
	_, err = st.DB().Exec(`INSERT INTO alias_inventory (email, account_id, provider_alias_id, allocation_state, remote_state, source_type)
		VALUES ('sched_target@example.com', 'acc_sched', '', 'available', 'active', 'replenish')`)
	if err != nil {
		t.Fatal(err)
	}

	// 验证初始可用库存为 1
	if cnt := st.CountAuthoritativeAvailableAliases(); cnt != 1 {
		t.Fatalf("expected 1 available alias initially, got %d", cnt)
	}

	// 2. 模拟 managerBackend 的 resolveAliasIdentifiers 链路：
	// 当外部仅传入 anonymousID = "anon_from_apple_456"
	// 上游返回包含该 anonymousID 及对应 email
	mb := &managerBackend{
		store: st,
	}
	mb.setCachedAliases("acc_sched", []hme.Alias{
		{
			Email:       "sched_target@example.com",
			AnonymousID: "anon_from_apple_456",
			Active:      true,
		},
	})

	anonID, email, err := mb.resolveAliasIdentifiers("acc_sched", "anon_from_apple_456")
	if err != nil {
		t.Fatalf("resolveAliasIdentifiers failed: %v", err)
	}
	if anonID != "anon_from_apple_456" || email != "sched_target@example.com" {
		t.Fatalf("expected anonID=anon_from_apple_456 and email=sched_target@example.com, got anonID=%s, email=%s", anonID, email)
	}

	// 3. 执行 UpdateAliasRemoteState (模拟停用)
	err = st.UpdateAliasRemoteState("acc_sched", anonID, email, store.RemoteInactive)
	if err != nil {
		t.Fatalf("UpdateAliasRemoteState to inactive failed: %v", err)
	}

	// 验证数据库状态及 provider_alias_id 回填
	inv, err := st.GetInventoryAlias("sched_target@example.com")
	if err != nil || inv == nil {
		t.Fatalf("GetInventoryAlias failed: %v", err)
	}
	if inv.RemoteState != store.RemoteInactive {
		t.Fatalf("expected remote_state inactive, got %s", inv.RemoteState)
	}
	if inv.ProviderAliasID != "anon_from_apple_456" {
		t.Fatalf("expected provider_alias_id backfilled to anon_from_apple_456, got %s", inv.ProviderAliasID)
	}

	// 验证可用库存已变为 0，且认领必定失败
	if cnt := st.CountAuthoritativeAvailableAliases(); cnt != 0 {
		t.Fatalf("expected 0 available aliases after deactivation, got %d", cnt)
	}
	alloc, _, err := st.ClaimInventoryAlias(context.Background(), "admin", "admin", "allocate", "key_test", "h", "default", nil)
	if !errors.Is(err, store.ErrNoAvailableInventory) || alloc != nil {
		t.Fatalf("expected ErrNoAvailableInventory for deactivated alias, got alloc=%v, err=%v", alloc, err)
	}

	// 4. 执行删除操作
	err = st.UpdateAliasRemoteState("acc_sched", anonID, email, store.RemoteDeleted)
	if err != nil {
		t.Fatalf("UpdateAliasRemoteState to deleted failed: %v", err)
	}

	// 验证 allocation_state 变为 quarantined
	invDel, err := st.GetInventoryAlias("sched_target@example.com")
	if err != nil || invDel == nil {
		t.Fatalf("GetInventoryAlias failed: %v", err)
	}
	if invDel.RemoteState != store.RemoteDeleted {
		t.Fatalf("expected remote_state deleted, got %s", invDel.RemoteState)
	}
	if invDel.AllocationState != store.AllocationQuarantined {
		t.Fatalf("expected allocation_state quarantined, got %s", invDel.AllocationState)
	}

	// 5. 验证防护机制：空 providerAliasID 和空 email 严禁执行更新，必须报错
	err = st.UpdateAliasRemoteState("acc_sched", "", "", store.RemoteInactive)
	if err == nil {
		t.Fatalf("expected error when both providerAliasID and email are empty, got nil")
	}

	// 6. 验证防护机制：零行匹配时必须显式返回错误，禁止假成功
	err = st.UpdateAliasRemoteState("acc_sched", "non_existent_id", "non_existent@example.com", store.RemoteInactive)
	if err == nil {
		t.Fatalf("expected error when 0 rows affected, got nil")
	}
}

