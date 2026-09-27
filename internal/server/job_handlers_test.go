/**
 * [INPUT]: 依赖 encoding/json, net/http, net/http/httptest, path/filepath, strings, testing, time, icloud-hme/internal/account, icloud-hme/internal/store
 * [OUTPUT]: 对外提供作业生命周期、保存失败响应及 next_run_at 契约测试
 * [POS]: internal/server 的自动化创建作业标准 RESTful 门面集成测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/store"
)

func TestCreateJobHandlers(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := store.NewStore(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	f := &fakeBackend{
		accounts: []account.Summary{
			{ID: "acc_1", Name: "主号", Status: "active", HasCookies: true},
			{ID: "acc_2", Name: "备号", Status: "active", HasCookies: true},
		},
	}
	cfg := Config{
		APIKey:     "test-key",
		SessionTTL: 1 * time.Hour,
	}
	srv := newWithBackendAndStore(f, cfg, st)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := ts.Client()

	// 1. GET /api/create/jobs (无任务时)
	req, _ := http.NewRequest("GET", ts.URL+"/api/create/jobs?account_id=acc_1", nil)
	req.Header.Set("X-API-Key", "test-key")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/create/jobs 期望 200, 得到 %d", resp.StatusCode)
	}
	var getRes struct {
		Success bool `json:"success"`
		Data    struct {
			RemainingThisHour int             `json:"remaining_this_hour"`
			Jobs              []createJobResp `json:"jobs"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&getRes)
	if getRes.Data.RemainingThisHour != 5 || len(getRes.Data.Jobs) != 1 {
		t.Fatalf("预期剩余额度 5，任务数 1，实际: %+v", getRes.Data)
	}

	// 2. POST /api/create/jobs (创建 duration 任务)
	postBody := `{"account_id":"acc_1","label_prefix":"AutoBot","mode":"duration","duration_hours":10}`
	req2, _ := http.NewRequest("POST", ts.URL+"/api/create/jobs", strings.NewReader(postBody))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-API-Key", "test-key")
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/create/jobs 期望 200, 得到 %d", resp2.StatusCode)
	}
	var jobRes struct {
		Success bool          `json:"success"`
		Data    createJobResp `json:"data"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&jobRes)
	if jobRes.Data.Status != "running" || jobRes.Data.DurationHours != 10 || jobRes.Data.LabelPrefix != "AutoBot" {
		t.Fatalf("创建的任务字段不符合预期: %+v", jobRes.Data)
	}

	jobID := jobRes.Data.ID

	// 3. POST /api/create/jobs/:id/pause
	reqPause, _ := http.NewRequest("POST", ts.URL+"/api/create/jobs/"+jobID+"/pause", nil)
	reqPause.Header.Set("X-API-Key", "test-key")
	respPause, err := client.Do(reqPause)
	if err != nil {
		t.Fatal(err)
	}
	defer respPause.Body.Close()
	if respPause.StatusCode != http.StatusOK {
		t.Fatalf("POST pause 期望 200, 得到 %d", respPause.StatusCode)
	}
	var pauseRes struct {
		Data createJobResp `json:"data"`
	}
	_ = json.NewDecoder(respPause.Body).Decode(&pauseRes)
	if pauseRes.Data.Status != "paused" {
		t.Fatalf("暂停后状态应为 paused, 实际为: %s", pauseRes.Data.Status)
	}

	// 4. POST /api/create/jobs/:id/resume
	reqResume, _ := http.NewRequest("POST", ts.URL+"/api/create/jobs/"+jobID+"/resume", nil)
	reqResume.Header.Set("X-API-Key", "test-key")
	respResume, err := client.Do(reqResume)
	if err != nil {
		t.Fatal(err)
	}
	defer respResume.Body.Close()
	if respResume.StatusCode != http.StatusOK {
		t.Fatalf("POST resume 期望 200, 得到 %d", respResume.StatusCode)
	}
	var resumeRes struct {
		Data createJobResp `json:"data"`
	}
	_ = json.NewDecoder(respResume.Body).Decode(&resumeRes)
	if resumeRes.Data.Status != "running" {
		t.Fatalf("恢复后状态应为 running, 实际为: %s", resumeRes.Data.Status)
	}

	// 5. DELETE /api/create/jobs/:id
	reqDel, _ := http.NewRequest("DELETE", ts.URL+"/api/create/jobs/"+jobID, nil)
	reqDel.Header.Set("X-API-Key", "test-key")
	respDel, err := client.Do(reqDel)
	if err != nil {
		t.Fatal(err)
	}
	defer respDel.Body.Close()
	if respDel.StatusCode != http.StatusOK {
		t.Fatalf("DELETE job 期望 200, 得到 %d", respDel.StatusCode)
	}
	cfgAfterDel := st.GetScheduleConfig("acc_1")
	if cfgAfterDel.StartedAt != "" {
		t.Fatalf("DELETE 后 StartedAt 应清空，实际得到: %s", cfgAfterDel.StartedAt)
	}

	// 6. 参数校验错误测试 (非法 mode)
	reqErr, _ := http.NewRequest("POST", ts.URL+"/api/create/jobs", strings.NewReader(`{"account_id":"acc_1","mode":"invalid"}`))
	reqErr.Header.Set("Content-Type", "application/json")
	reqErr.Header.Set("X-API-Key", "test-key")
	respErr, err := client.Do(reqErr)
	if err != nil {
		t.Fatal(err)
	}
	defer respErr.Body.Close()
	if respErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 mode 期望 400, 得到 %d", respErr.StatusCode)
	}
}

func TestCreateJobHandlersReportPersistenceErrors(t *testing.T) {
	st, err := store.NewStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := newWithBackendAndStore(&fakeBackend{accounts: []account.Summary{{ID: "acc_1", Name: "test"}}}, Config{APIKey: "test-key", SessionTTL: time.Hour}, st)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/api/create/jobs", `{"account_id":"acc_1","mode":"always"}`},
		{"POST", "/api/create/jobs/job_acc_1/pause", ""},
		{"POST", "/api/create/jobs/job_acc_1/resume", ""},
		{"DELETE", "/api/create/jobs/job_acc_1", ""},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("X-API-Key", "test-key")
		if tc.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp := httptest.NewRecorder()
		srv.Handler().ServeHTTP(resp, req)
		if resp.Code != http.StatusInternalServerError || !strings.Contains(resp.Body.String(), "PERSISTENCE_ERROR") {
			t.Errorf("%s %s: got %d %s", tc.method, tc.path, resp.Code, resp.Body.String())
		}
	}
}

func TestCreateJobResponseDoesNotGuessNextRun(t *testing.T) {
	resp := toJobResp(store.ScheduleConfig{AccountID: "acc_1", Enabled: true, Mode: "always"}, time.Now())
	if resp.NextRunAt != "" {
		t.Fatalf("无法准确预测执行时刻时应省略 next_run_at: %q", resp.NextRunAt)
	}
}
