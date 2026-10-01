package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/store"
)

func TestCamoufoxLoginCancellationCleansTask(t *testing.T) {
	for _, phase := range []string{"task_creation", "polling"} {
		t.Run(phase, func(t *testing.T) {
			t.Setenv("ICLOUD_HME_CAMOUFOX_TOKEN", "fictional-test-token")
			phaseStarted := make(chan struct{})
			releaseCreation := make(chan struct{})
			cancelled := make(chan struct{}, 1)
			agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodDelete:
					cancelled <- struct{}{}
					fmt.Fprint(w, `{"success":true}`)
				case r.URL.Path == "/health":
					fmt.Fprint(w, `{"camoufox_ready":true}`)
				case r.URL.Path == "/login":
					if phase == "task_creation" {
						close(phaseStarted)
						<-releaseCreation
					}
					fmt.Fprint(w, `{"success":true,"task_id":"cancel-test"}`)
				case r.URL.Path == "/tasks/cancel-test":
					close(phaseStarted)
					<-r.Context().Done()
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer agent.Close()
			t.Setenv("ICLOUD_HME_CAMOUFOX_URL", agent.URL)
			mgr, err := account.NewManager(t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer mgr.Close()
			acc, err := mgr.AddAccountWithInput(account.AddAccountInput{Name: "Test", ICloudEmail: "test@icloud.com", Host: "icloud.com"})
			if err != nil {
				t.Fatal(err)
			}
			st, err := store.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			be := &managerBackend{mgr: mgr, store: st}
			s := newWithBackendAndStore(be, Config{APIKey: "test-api-key-strong"}, st)
			defer s.Close()
			serverAborted := make(chan struct{})
			web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				context.AfterFunc(r.Context(), func() { close(serverAborted) })
				s.Handler().ServeHTTP(w, r)
			}))
			defer web.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, web.URL+"/api/accounts/"+acc.ID+"/login", strings.NewReader(`{"password":"fictional-password"}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-API-Key", "test-api-key-strong")
			requestDone := make(chan error, 1)
			go func() {
				resp, err := http.DefaultClient.Do(req)
				if resp != nil {
					resp.Body.Close()
				}
				requestDone <- err
			}()
			select {
			case <-phaseStarted:
			case <-time.After(2 * time.Second):
				close(releaseCreation)
				t.Fatal("login did not reach expected phase")
			}
			cancel()
			if err := <-requestDone; !errors.Is(err, context.Canceled) {
				close(releaseCreation)
				t.Fatalf("client request was not cancelled: %v", err)
			}
			select {
			case <-serverAborted:
			case <-time.After(2 * time.Second):
				close(releaseCreation)
				t.Fatal("HTTP cancellation did not reach login handler")
			}
			close(releaseCreation)
			select {
			case <-cancelled:
			case <-time.After(2 * time.Second):
				t.Fatal("disconnected login left agent task running")
			}
			// DELETE arrival precedes decoding and local persistence cleanup.
			deadline := time.Now().Add(2 * time.Second)
			for {
				_, exists := be.getCamoufoxTask(acc.ID)
				records, err := st.ListCamoufoxTasks()
				if err == nil && !exists && len(records) == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("cancelled task still blocks login: exists=%v records=%+v err=%v", exists, records, err)
				}
				time.Sleep(time.Millisecond)
			}
			if !be.reserveCamoufoxTask(acc.ID, agent.URL) {
				t.Fatal("new login is blocked after cancellation")
			}
			be.clearCamoufoxTask(acc.ID, "")
		})
	}
}
