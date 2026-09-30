package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

func TestRegressionBlockedCreateMustNotSpendNewQuota(t *testing.T) {
	writes := 0
	upstream, be, st, _ := setupFaultTestEnvironment(t, "audit", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/hme/list" {
			writes++
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"success":true,"result":{"hmeEmails":[]}}`)
	}))
	defer upstream.Close()
	defer st.Close()
	defer be.mgr.Close()
	ctx := context.Background()
	intent, err := st.CreateReserveIntent(ctx, "audit", "original@icloud.com", "original")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateReserveIntentState(ctx, intent.IntentID, store.IntentStateOutcomeUnknown, "", "", "original uncertain write"); err != nil {
		t.Fatal(err)
	}
	before, err := st.RemainingQuota("audit")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := be.CreateAlias("audit", "blocked"); err == nil {
			t.Fatal("expected blocked request")
		}
		if _, err := be.BatchCreateAlias("audit", 3, "blocked"); err == nil {
			t.Fatal("expected blocked batch request")
		}
	}
	after, err := st.RemainingQuota("audit")
	if err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Fatalf("writes=%d", writes)
	}
	if after != before {
		t.Fatalf("blocked requests spent quota without Generate/Reserve: before=%d after=%d", before, after)
	}
}

func TestRegressionDeleteInactiveAliasMustPreserveActiveCount(t *testing.T) {
	upstream, be, st, _ := setupFaultTestEnvironment(t, "audit", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"success":true}`) }))
	defer upstream.Close()
	defer st.Close()
	defer be.mgr.Close()
	if err := be.mgr.UpdateAliasCounts("audit", 2, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.AddInventoryAlias("audit", hme.Alias{Email: "inactive@icloud.com", AnonymousID: "inactive-id", Active: false}, "replenish", false); err != nil {
		t.Fatal(err)
	}
	if err := be.DeleteAlias("audit", "inactive-id"); err != nil {
		t.Fatal(err)
	}
	acc, found := be.mgr.GetAccount("audit")
	if !found {
		t.Fatal("missing account")
	}
	if acc.AliasTotal != 1 || acc.AliasActive != 1 {
		t.Fatalf("deleting inactive alias corrupted counts: total=%d active=%d; expected total=1 active=1", acc.AliasTotal, acc.AliasActive)
	}
}

func TestRegressionRawMailMustReportBodyFetchFailure(t *testing.T) {
	srv := newMailPreviewTestServer(t)
	fb := srv.be.(*fakeBackend)
	fb.inbox.Messages[0].Preview = "partial preview"
	failure := errors.New("upstream body read failed")
	fb.getMessageFunc = func(string, string) (*mail.FullMessage, error) { return nil, failure }
	fb.onGetMessagesContext = func(context.Context, string, []mail.MessageRef) ([]*mail.FullMessage, error) { return nil, failure }
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/mail/raw?email=fixture@icloud.com&api_key="+previewTestAPIKey, nil)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "MAIL_BODY_UNAVAILABLE") || strings.Contains(rec.Body.String(), "partial preview") {
		t.Fatalf("raw mail reported success when every body fetch failed: status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestRegressionUnknownCurrentWriteRetainsExactlyOneQuota(t *testing.T) {
	for _, tc := range []struct{ batch, failPersistence bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprintf("batch=%v/persistenceFailure=%v", tc.batch, tc.failPersistence), func(t *testing.T) {
			writes := 0
			upstream, be, st, _ := setupFaultTestEnvironment(t, "audit", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/hme/generate":
					fmt.Fprint(w, `{"success":true,"result":{"hme":"uncertain@icloud.com"}}`)
				case "/v1/hme/reserve":
					writes++
					fmt.Fprint(w, "malformed response")
				case "/v2/hme/list":
					fmt.Fprint(w, `{"success":true,"result":{"hmeEmails":[]}}`)
				default:
					t.Errorf("unexpected path: %s", r.URL.Path)
				}
			}))
			defer upstream.Close()
			defer st.Close()
			defer be.mgr.Close()
			if tc.failPersistence {
				if _, err := st.DB().Exec(`CREATE TRIGGER reject_unknown_state BEFORE UPDATE OF state ON hme_reserve_intents WHEN NEW.state = 'outcome_unknown' BEGIN SELECT RAISE(ABORT, 'injected state persistence failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			if tc.batch {
				_, err = be.BatchCreateAlias("audit", 3, "uncertain")
			} else {
				_, err = be.CreateAlias("audit", "uncertain")
			}
			var backendErr *BackendError
			if !errors.As(err, &backendErr) || backendErr.Code != "UPSTREAM_OUTCOME_UNKNOWN" {
				t.Fatalf("uncertain write classification lost: %v", err)
			}
			remaining, quotaErr := st.RemainingQuota("audit")
			if quotaErr != nil || remaining != 4 || writes != 1 {
				t.Fatalf("unknown write lost its quota or retained unsent slots: remaining=%d writes=%d err=%v", remaining, writes, quotaErr)
			}
		})
	}
}

func TestRegressionMailBodyFailureIsolatedAndEmptyBodyNotPreview(t *testing.T) {
	srv := newMailPreviewTestServer(t)
	fb := srv.be.(*fakeBackend)
	original := fb.getMessageFunc
	fb.inbox.Messages[0].Preview = "must not become a body"
	fb.getMessageFunc = func(accountID, id string) (*mail.FullMessage, error) {
		ref, err := mail.ParseMessageRef(id, accountID)
		if err != nil {
			return nil, err
		}
		if ref.ThreadID == "older" {
			return nil, errors.New("broken MIME")
		}
		full, err := original(accountID, id)
		if ref.ThreadID == "rich" && full != nil {
			full.Body = ""
			full.ContentType = "text/plain"
		}
		return full, err
	}
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path+"&email=fixture@icloud.com&api_key="+previewTestAPIKey, nil))
		return rec
	}
	view := get("/mail/view?format=json")
	var response struct {
		Success bool         `json:"success"`
		Data    mailViewData `json:"data"`
	}
	if err := json.Unmarshal(view.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if view.Code != 200 || !response.Success || len(response.Data.Items) != 3 || response.Data.Items[1].BodyError == "" || response.Data.Items[2].MagicLink == "" {
		t.Fatalf("per-message failure hid healthy mail: %s", view.Body)
	}
	empty := get("/mail/raw?message_id=rich")
	if empty.Code != 200 || empty.Body.Len() != 0 || empty.Header().Get("X-Mail-Body-Complete") != "true" {
		t.Fatalf("empty complete body replaced with preview: status=%d body=%q", empty.Code, empty.Body.String())
	}
	broken := get("/mail/raw?message_id=older")
	if broken.Code != 502 {
		t.Fatalf("broken selected body accepted: %d %s", broken.Code, broken.Body)
	}
	healthy := get("/mail/raw?message_id=magic")
	if healthy.Code != 200 || !strings.Contains(healthy.Body.String(), "token=sample") {
		t.Fatalf("healthy mail blocked by other body failure: %d %s", healthy.Code, healthy.Body)
	}
}
