package server

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/account"
	"icloud-hme/internal/store"
)

func TestTagDescriptionPatchPreservesOmittedAndClearsExplicitEmpty(t *testing.T) {
	for name, testCase := range map[string]struct{ body, description string }{
		"omitted":     {`{"name":"renamed"}`, "old description"},
		"null":        {`{"description":null}`, "old description"},
		"empty":       {`{"description":""}`, ""},
		"replacement": {`{"description":"new description"}`, "new description"},
	} {
		t.Run(name, func(t *testing.T) {
			st, err := store.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			original := store.BusinessTag{ID: "tag_probe", Tag: "test", Name: "test", Description: "old description", Status: "disabled", CreatedAt: "2026-01-01T00:00:00Z", LastAssignedAt: "2026-09-30T01:00:00Z"}
			if err := st.CreateTag(original); err != nil {
				t.Fatal(err)
			}
			router := gin.New()
			router.PATCH("/tags/:id", (&Server{store: st}).updateTagHandler)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPatch, "/tags/tag_probe", bytes.NewBufferString(testCase.body))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status %d: %s", recorder.Code, recorder.Body)
			}
			tags, err := st.ListTags()
			if err != nil {
				t.Fatal(err)
			}
			if len(tags) != 1 || tags[0].Description != testCase.description {
				t.Fatalf("unexpected persisted tags: %+v", tags)
			}
			if tags[0].Tag != original.Tag || tags[0].Status != original.Status || tags[0].CreatedAt != original.CreatedAt || tags[0].LastAssignedAt != original.LastAssignedAt {
				t.Fatalf("unsubmitted fields changed: %+v", tags[0])
			}
		})
	}
}

func TestScheduleValidationRejectsInvalidMergedConfigWithoutWriting(t *testing.T) {
	for name, body := range map[string]string{
		"zero duration":     `{"enabled":true,"mode":"duration","duration_hours":0}`,
		"negative duration": `{"enabled":true,"mode":"duration","duration_hours":-1}`,
		"overflow duration": fmt.Sprintf(`{"enabled":true,"mode":"duration","duration_hours":%d}`, maxScheduleDurationHours+1),
		"invalid timestamp": `{"enabled":true,"mode":"duration","duration_hours":1,"started_at":"invalid"}`,
		"invalid start":     `{"enabled":true,"mode":"daily_window","start_time":"invalid","end_time":"18:00"}`,
		"invalid end":       `{"enabled":true,"mode":"daily_window","start_time":"09:00","end_time":"24:00"}`,
		"missing window":    `{"enabled":true,"mode":"daily_window"}`,
		"invalid merge":     `{"mode":"duration"}`,
	} {
		t.Run(name, func(t *testing.T) {
			st, err := store.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if err := st.SaveScheduleConfig(store.ScheduleConfig{AccountID: "probe", Enabled: true, Mode: "always", HourlyQuota: 5, AliasLabel: "original"}); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := st.TryReserveQuota("probe", 2); err != nil {
				t.Fatal(err)
			}
			before, err := st.GetScheduleConfig("probe")
			if err != nil {
				t.Fatal(err)
			}
			srv := &Server{store: st, be: &fakeBackend{accounts: []account.Summary{{ID: "probe"}}}}
			router := gin.New()
			router.PUT("/configs/:account_id", srv.updateScheduleConfigHandler)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPut, "/configs/probe", bytes.NewBufferString(body))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest || !bytes.Contains(recorder.Body.Bytes(), []byte("VALIDATION_ERROR")) {
				t.Fatalf("status %d: %s", recorder.Code, recorder.Body)
			}
			after, err := st.GetScheduleConfig("probe")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("invalid update persisted: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestScheduleValidatedPatchPreservesOtherFieldsAndAllowsStoppingLegacyInvalidConfig(t *testing.T) {
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := &Server{store: st, be: &fakeBackend{accounts: []account.Summary{{ID: "probe"}}}}
	router := gin.New()
	router.PUT("/configs/:account_id", srv.updateScheduleConfigHandler)
	update := func(body string) {
		t.Helper()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPut, "/configs/probe", bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status %d: %s", recorder.Code, recorder.Body)
		}
	}
	update(`{"enabled":true,"mode":"daily_window","start_time":"22:00","end_time":"06:00","hourly_quota":5}`)
	update(`{"hourly_quota":7}`)
	cfg, err := st.GetScheduleConfig("probe")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || cfg.HourlyQuota != 7 || cfg.Mode != "daily_window" || cfg.StartTime != "22:00" || cfg.EndTime != "06:00" {
		t.Fatalf("partial update lost fields: %+v", cfg)
	}
	update(`{"mode":"duration","duration_hours":2,"started_at":""}`)
	cfg, err = st.GetScheduleConfig("probe")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StartedAt == "" {
		t.Fatal("duration start was not initialized")
	}
	cfg.StartedAt = "legacy-invalid"
	if err := st.SaveScheduleConfig(cfg); err != nil {
		t.Fatal(err)
	}
	update(`{"enabled":false}`)
	cfg, err = st.GetScheduleConfig("probe")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled {
		t.Fatal("legacy invalid schedule could not be stopped")
	}
}

func TestJobResumeCannotEnableInvalidScheduleDraft(t *testing.T) {
	for name, draft := range map[string]store.ScheduleConfig{
		"duration": {AccountID: "probe", Mode: "duration", HourlyQuota: 5},
		"window":   {AccountID: "probe", Mode: "daily_window", HourlyQuota: 5, StartTime: "09:00"},
	} {
		t.Run(name, func(t *testing.T) {
			st, err := store.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if err := st.SaveScheduleConfig(draft); err != nil {
				t.Fatal(err)
			}
			srv := &Server{store: st, be: &fakeBackend{accounts: []account.Summary{{ID: "probe"}}}}
			router := gin.New()
			router.POST("/jobs/:id/resume", srv.resumeCreateJobHandler)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/jobs/job_probe/resume", nil))
			if recorder.Code != http.StatusBadRequest || !bytes.Contains(recorder.Body.Bytes(), []byte("VALIDATION_ERROR")) {
				t.Fatalf("status %d: %s", recorder.Code, recorder.Body)
			}
			cfg, err := st.GetScheduleConfig("probe")
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Enabled || cfg.StartedAt != "" {
				t.Fatalf("invalid draft was activated: %+v", cfg)
			}
		})
	}
}
