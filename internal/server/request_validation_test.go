package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/store"
)

func TestAliasActionsRejectMalformedJSONBeforeMutation(t *testing.T) {
	for _, action := range []string{"delete", "deactivate", "reactivate"} {
		for _, body := range []string{"{", `{"account_id":7}`, `{"account_id":"account_a",`, `[]`, "", "  ", `{}`, `{"account_id":"account_a"}`} {
			t.Run(action+"/"+body, func(t *testing.T) {
				fb := &fakeBackend{}
				srv := &Server{be: fb}
				router := gin.New()
				method := http.MethodPost
				handler := srv.deactivateAliasHandler
				if action == "delete" {
					method, handler = http.MethodDelete, srv.deleteAliasHandler
				} else if action == "reactivate" {
					handler = srv.reactivateAliasHandler
				}
				router.Handle(method, "/aliases/:id", handler)
				req := httptest.NewRequest(method, "/aliases/anon_a?account_id=account_a", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				valid := strings.TrimSpace(body) == "" || body == `{}` || body == `{"account_id":"account_a"}`
				if !valid {
					if rec.Code != http.StatusBadRequest || fb.aliasDeleteID != "" || fb.aliasActID != "" {
						t.Fatalf("invalid request mutated alias: status=%d delete=%q active=%q", rec.Code, fb.aliasDeleteID, fb.aliasActID)
					}
					return
				}
				if rec.Code != http.StatusOK {
					t.Fatalf("valid query/body request rejected: %d %s", rec.Code, rec.Body)
				}
				if action == "delete" && fb.aliasDeleteID != "anon_a" || action != "delete" && (fb.aliasActID != "anon_a" || fb.aliasActActive != (action == "reactivate")) {
					t.Fatal("valid request did not perform expected action")
				}
			})
		}
	}
}

func TestCreateTokenRejectsInvalidJSONAndPreservesExpiry(t *testing.T) {
	for _, tc := range []struct {
		name, body      string
		valid, expiring bool
	}{
		{"syntax", "{", false, false},
		{"partial", `{"name":"probe",`, false, false},
		{"days type", `{"expires_in_days":"7"}`, false, false},
		{"date type", `{"expires_at":7}`, false, false},
		{"scopes type", `{"scopes":[]}`, false, false},
		{"empty", "", true, false},
		{"object", `{}`, true, false},
		{"days", `{"expires_in_days":7}`, true, true},
		{"date", `{"expires_at":"2099-01-01T00:00:00Z"}`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			srv := &Server{store: st}
			router := gin.New()
			router.POST("/tokens", srv.createTokenHandler)
			req := httptest.NewRequest("POST", "/tokens", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			tokens, err := st.ListTokens()
			if err != nil {
				t.Fatal(err)
			}
			if !tc.valid {
				if rec.Code != http.StatusBadRequest || len(tokens) != 0 {
					t.Fatalf("invalid request persisted token: status=%d count=%d", rec.Code, len(tokens))
				}
				return
			}
			if rec.Code != http.StatusOK || len(tokens) != 1 {
				t.Fatalf("valid request failed: status=%d count=%d", rec.Code, len(tokens))
			}
			if tc.expiring {
				expires, err := time.Parse(time.RFC3339, tokens[0].ExpiresAt)
				if err != nil || !expires.After(time.Now()) {
					t.Fatalf("expiry lost: %q", tokens[0].ExpiresAt)
				}
			} else if tokens[0].ExpiresAt != "" {
				t.Fatal("optional expiry changed")
			}
		})
	}
}
