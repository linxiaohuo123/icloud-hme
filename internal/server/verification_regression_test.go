package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"icloud-hme/internal/auth"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

func TestVerificationResultReadErrors(t *testing.T) {
	for _, failure := range []string{"database", "missing", "cancelled"} {
		for _, duringWait := range []bool{false, true} {
			name := failure + "/initial"
			if duringWait {
				name = failure + "/waiting"
			}
			t.Run(name, func(t *testing.T) {
				st, req := newVerificationRegressionStore(t)
				bus := mail.NewEventBus(time.Minute)
				svc := NewVerificationService(nil, st, bus, nil)
				p := auth.Principal{Kind: auth.PrincipalToken, ID: req.PrincipalID, Scopes: []string{"verify"}}
				if failure == "cancelled" {
					// Isolate request cancellation from the initial token check.
					p = auth.Principal{Kind: auth.PrincipalAdmin, ID: "review_admin"}
					if _, err := st.DB().Exec(`UPDATE verification_requests SET principal_kind = ?, principal_id = ?`, string(p.Kind), p.ID); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				defer cancel()
				injectFailure := func() {
					var err error
					switch failure {
					case "database":
						_, err = st.DB().Exec(`DROP TABLE verification_requests`)
					case "missing":
						_, err = st.DB().Exec(`DELETE FROM verification_requests`)
					case "cancelled":
						cancel()
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				type outcome struct {
					result *VerificationResult
					err    error
				}
				done := make(chan outcome, 1)
				if !duringWait {
					injectFailure()
				}
				go func() {
					r, err := svc.GetVerificationResult(ctx, p, req.RequestID, 1)
					done <- outcome{r, err}
				}()
				if duringWait {
					ticker := time.NewTicker(time.Millisecond)
					defer ticker.Stop()
					for bus.SubscriberCount(req.AliasEmail) == 0 {
						select {
						case <-ticker.C:
						case got := <-done:
							t.Fatalf("request returned before subscribing: %+v", got)
						case <-ctx.Done():
							t.Fatal("subscription not ready")
						}
					}
					injectFailure()
				}
				got := <-done
				if got.result != nil || got.err == nil {
					t.Fatalf("failed read returned success: result=%+v err=%v", got.result, got.err)
				}
				switch failure {
				case "database":
					var be *BackendError
					if !errors.As(got.err, &be) || be.Status != http.StatusInternalServerError || be.Code != "INTERNAL_ERROR" {
						t.Fatalf("database error misclassified: %v", got.err)
					}
				case "missing":
					if !errors.Is(got.err, ErrVReqNotFound) {
						t.Fatalf("missing request misclassified: %v", got.err)
					}
				case "cancelled":
					if !errors.Is(got.err, context.Canceled) {
						t.Fatalf("cancellation lost: %v", got.err)
					}
				}
				if bus.SubscriberCount(req.AliasEmail) != 0 {
					t.Fatal("failed request leaked subscription")
				}
			})
		}
	}
}

func TestVerificationScanRetriesFailedBody(t *testing.T) {
	st, req := newVerificationRegressionStore(t)
	msg := mail.Message{ID: "105", UID: 105, UIDValidity: 1, AccountID: "review_account", Provider: "imap", Folder: "INBOX", To: req.AliasEmail, Subject: "Login", Preview: "Your verification code is 654321"}
	fb := newScanTestBackend("review_account", []mail.Message{msg}, 106)
	getFull := fb.onGetMessagesContext
	wantErr := errors.New("decode message body failed")
	calls := 0
	fb.onGetMessagesContext = func(ctx context.Context, accountID string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
		calls++
		if calls == 1 {
			return nil, wantErr
		}
		return getFull(ctx, accountID, refs)
	}
	worker := NewMailSyncWorker(fb, st, mail.NewEventBus(time.Minute), time.Second)
	if _, err := worker.fetchAndPublishBatchResult(context.Background(), "review_account", []string{req.AliasEmail}); !errors.Is(err, wantErr) {
		t.Fatalf("body fetch error lost: %v", err)
	}
	key := checkpointKey{accountID: "review_account", mailbox: "INBOX", uidValidity: 1}
	if cp := worker.checkpoints[key]; cp != nil && cp.NextUID > 105 {
		t.Fatalf("failed body skipped: nextUID=%d", cp.NextUID)
	}
	if _, err := worker.fetchAndPublishBatchResult(context.Background(), "review_account", []string{req.AliasEmail}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetVerificationRequest(context.Background(), req.RequestID, req.PrincipalKind, req.PrincipalID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" || got.Code != "654321" || calls != 2 {
		t.Fatalf("retry lost verification code: status=%s code=%s calls=%d", got.Status, got.Code, calls)
	}
}

func newVerificationRegressionStore(t *testing.T) (*store.Store, *store.VerificationRequest) {
	t.Helper()
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now().UTC()
	if err := st.SaveToken(store.APIToken{ID: "review_token", Name: "Review", Token: "review-test-token", Scopes: "verify", CreatedAt: now.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	req := &store.VerificationRequest{RequestID: "review_request", PrincipalKind: "token", PrincipalID: "review_token", LeaseID: "review_lease", AliasEmail: "review@icloud.com", Status: "ready", CreatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(10 * time.Minute).Format(time.RFC3339), BaselineProvider: "imap", BaselineMailbox: "INBOX", BaselineUIDValidity: 1, BaselineUID: 100}
	if err := st.CreateVerificationRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	return st, req
}

func TestVerificationResultRechecksTokenAfterWait(t *testing.T) {
	for _, delivery := range []string{"database", "event", "pending"} {
		for _, tokenState := range []string{"revoked", "expired", "active"} {
			t.Run(delivery+"/"+tokenState, func(t *testing.T) {
				st, req := newVerificationRegressionStore(t)
				bus := mail.NewEventBus(time.Minute)
				svc := NewVerificationService(nil, st, bus, nil)
				p := auth.Principal{Kind: auth.PrincipalToken, ID: req.PrincipalID, Scopes: []string{"verify"}}
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				defer cancel()
				type outcome struct {
					result *VerificationResult
					err    error
				}
				done := make(chan outcome, 1)
				go func() {
					result, err := svc.GetVerificationResult(ctx, p, req.RequestID, 1)
					done <- outcome{result, err}
				}()
				ticker := time.NewTicker(time.Millisecond)
				defer ticker.Stop()
				for bus.SubscriberCount(req.AliasEmail) == 0 {
					select {
					case <-ticker.C:
					case <-ctx.Done():
						t.Fatal("subscription not ready")
					}
				}
				switch tokenState {
				case "revoked":
					if ok, err := st.DeleteToken(p.ID); err != nil || !ok {
						t.Fatalf("revoke: ok=%v err=%v", ok, err)
					}
				case "expired":
					if _, err := st.DB().Exec(`UPDATE api_tokens SET expires_at = ? WHERE id = ?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), p.ID); err != nil {
						t.Fatal(err)
					}
				}
				switch delivery {
				case "database":
					// Durable completion may occur without a bus event.
					if _, won, err := st.CompleteVerificationRequestResult(ctx, req.RequestID, store.VerificationCompletion{Code: "654321", MatchedEventRef: "review-event"}, time.Now().UTC()); err != nil || !won {
						t.Fatalf("complete: won=%v err=%v", won, err)
					}
				case "event":
					bus.PublishEvent(&mail.CachedOTP{EventID: "review-event", Email: req.AliasEmail, Folder: "INBOX", UIDValidity: 1, UID: 105, OTP: &mail.OTPResult{Code: "654321"}})
				}
				got := <-done
				if tokenState != "active" {
					if !errors.Is(got.err, ErrTokenRevoked) || got.result != nil {
						t.Fatalf("invalid token received result=%+v err=%v", got.result, got.err)
					}
				} else {
					if got.err != nil || got.result == nil {
						t.Fatalf("active token rejected: %v", got.err)
					}
					if delivery == "pending" {
						if got.result.Status != "pending" || got.result.Code != "" {
							t.Fatalf("unexpected pending result: %+v", got.result)
						}
					} else if got.result.Status != "succeeded" || got.result.Code != "654321" {
						t.Fatalf("active token lost result: %+v", got.result)
					}
				}
			})
		}
	}
}

func TestEmptyMailboxGenerationHandling(t *testing.T) {
	for _, tc := range []struct {
		name       string
		generation uint32
		status     string
	}{
		{"unchanged", 1, "ready"},
		{"recreated", 2, "invalidated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, req := newVerificationRegressionStore(t)
			fb := newScanTestBackend("review_account", nil, 1)
			fb.mailboxBoundaryFunc = func(string, string) (string, uint32, uint32, error) { return "imap", tc.generation, 1, nil }
			fb.onScanMailboxUIDPage = func(context.Context, ScanPageQuery) (ScanPageResult, error) {
				t.Error("empty mailbox should not issue a UID scan")
				return ScanPageResult{}, nil
			}
			worker := NewMailSyncWorker(fb, st, mail.NewEventBus(time.Minute), time.Second)
			oldKey := checkpointKey{accountID: "review_account", mailbox: "INBOX", uidValidity: 1}
			worker.checkpoints[oldKey] = &checkpointState{NextUID: 110, AliasBaselines: map[string]uint32{req.AliasEmail: 100}}
			if _, err := worker.fetchAndPublishBatchResult(context.Background(), "review_account", []string{req.AliasEmail}); err != nil {
				t.Fatal(err)
			}
			got, err := st.GetVerificationRequest(context.Background(), req.RequestID, req.PrincipalKind, req.PrincipalID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tc.status {
				t.Fatalf("generation=%d: status=%s, want %s", tc.generation, got.Status, tc.status)
			}
			if _, exists := worker.checkpoints[oldKey]; exists && tc.generation != 1 {
				t.Fatal("recreated mailbox retained an old-generation checkpoint")
			}
		})
	}
}

func TestVerificationScanOTPSelection(t *testing.T) {
	for _, tc := range []struct{ name, subject, body, wantCode string }{
		{"ambiguous", "Verification code", "Your code is 123456. Your code is 654321.", ""},
		{"order", "Order [839201] shipped", "Your parcel is on its way.", ""},
		{"explicit over order", "Order [839201] confirmation", "Your verification code is 492019", "492019"},
		{"grouped code", "Login", "Your verification code is 1234-5678", "12345678"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, req := newVerificationRegressionStore(t)
			msg := mail.Message{ID: "105", UID: 105, UIDValidity: 1, AccountID: "review_account", Provider: "imap", Folder: "INBOX", To: req.AliasEmail, Subject: tc.subject, Preview: tc.body}
			fb := newScanTestBackend("review_account", []mail.Message{msg}, 106)
			worker := NewMailSyncWorker(fb, st, mail.NewEventBus(time.Minute), time.Second)
			if _, err := worker.fetchAndPublishBatchResult(context.Background(), "review_account", []string{req.AliasEmail}); err != nil {
				t.Fatal(err)
			}
			got, err := st.GetVerificationRequest(context.Background(), req.RequestID, req.PrincipalKind, req.PrincipalID)
			if err != nil {
				t.Fatal(err)
			}
			wantStatus := "ready"
			if tc.wantCode != "" {
				wantStatus = "succeeded"
			}
			if got.Status != wantStatus || got.Code != tc.wantCode {
				t.Fatalf("status=%s code=%q want status=%s code=%q", got.Status, got.Code, wantStatus, tc.wantCode)
			}
		})
	}
}
