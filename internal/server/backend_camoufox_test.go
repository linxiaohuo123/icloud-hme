package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/account"
	"icloud-hme/internal/store"
)

func TestCamoufoxOTPErrorIncludesTaskID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	backendFail(c, &BackendError{
		Status: http.StatusConflict, Code: "OTP_REQUIRED", Message: "请输入验证码",
		Data: map[string]string{"task_id": "task-1"},
	})
	var response struct {
		Data struct {
			TaskID string `json:"task_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusConflict || response.Data.TaskID != "task-1" {
		t.Fatalf("OTP response lost task ID: status=%d data=%+v", w.Code, response.Data)
	}
}

func TestCamoufoxHealthRequiresTokenAndReady(t *testing.T) {
	t.Setenv("ICLOUD_HME_CAMOUFOX_TOKEN", "test-token")
	var ready atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Camoufox-Token") != "test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if ready.Load() {
			w.Write([]byte(`{"camoufox_ready":true}`))
		} else {
			w.Write([]byte(`{"camoufox_ready":false}`))
		}
	}))
	defer server.Close()

	if ok, _, _ := camoufoxHealth(server.URL, time.Second); ok {
		t.Fatal("browser not ready should be unavailable")
	}
	ready.Store(true)
	if ok, _, err := camoufoxHealth(server.URL, time.Second); err != nil || !ok {
		t.Fatalf("settings health failed: ready=%v err=%v", ok, err)
	}
}

func TestCamoufoxTaskReservationPreservesAccountTask(t *testing.T) {
	be := &managerBackend{}
	if !be.reserveCamoufoxTask("account-1", "http://agent") {
		t.Fatal("first reservation failed")
	}
	if be.reserveCamoufoxTask("account-1", "http://agent") {
		t.Fatal("duplicate reservation should fail")
	}
	be.setCamoufoxTask("account-1", "task-1")
	be.clearCamoufoxTask("account-1", "other-task")
	if task, ok := be.getCamoufoxTask("account-1"); !ok || task.taskID != "task-1" {
		t.Fatal("old task cleanup removed current task")
	}
	be.clearCamoufoxTask("account-1", "task-1")
	if _, ok := be.getCamoufoxTask("account-1"); ok {
		t.Fatal("completed task retained")
	}
}

func TestCancelCamoufoxLoginReleasesPendingTask(t *testing.T) {
	t.Setenv("ICLOUD_HME_CAMOUFOX_TOKEN", "test-token")
	var cancelled atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/tasks/task-1" && r.Header.Get("X-Camoufox-Token") == "test-token" {
			cancelled.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true}`))
		}
	}))
	defer server.Close()

	be := &managerBackend{}
	be.reserveCamoufoxTask("account-1", server.URL)
	be.setCamoufoxTask("account-1", "task-1")
	if cancelled, err := be.CancelCamoufoxLogin("account-1", "other-task"); err != nil || cancelled {
		t.Fatalf("stale cancellation affected current task: cancelled=%v err=%v", cancelled, err)
	}
	if cancelled, err := be.CancelCamoufoxLogin("account-1", "task-1"); err != nil || !cancelled {
		t.Fatal("pending task was not cancelled")
	}
	if !cancelled.Load() {
		t.Fatal("agent did not receive authenticated cancellation")
	}
	if _, ok := be.getCamoufoxTask("account-1"); ok {
		t.Fatal("cancelled task retained in backend")
	}
	if cancelled, err := be.CancelCamoufoxLogin("account-1", "task-1"); err != nil || cancelled {
		t.Fatal("missing task reported as cancelled")
	}
}

func TestCancelCamoufoxLoginRejectsSuccessFalse(t *testing.T) {
	t.Setenv("ICLOUD_HME_CAMOUFOX_TOKEN", "test-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":false}`))
	}))
	defer server.Close()

	be := &managerBackend{}
	be.reserveCamoufoxTask("account-1", server.URL)
	be.setCamoufoxTask("account-1", "task-1")
	if cancelled, err := be.CancelCamoufoxLogin("account-1", "task-1"); err != nil || cancelled {
		t.Fatalf("success:false must report a missing task: cancelled=%v err=%v", cancelled, err)
	}
	if _, ok := be.getCamoufoxTask("account-1"); ok {
		t.Fatal("missing task must be cleared")
	}
}

func TestRecoverCamoufoxTaskRetriesAndExpiresOTP(t *testing.T) {
	t.Setenv("ICLOUD_HME_CAMOUFOX_TOKEN", "test-token")
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var reject atomic.Bool
	reject.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reject.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer server.Close()
	if err := st.SaveCamoufoxTask(store.CamoufoxTask{
		AccountID: "account-1", TaskID: "task-1", BaseURL: server.URL, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	be := &managerBackend{store: st}
	be.recoverCamoufoxTasks()
	if task, ok := be.getCamoufoxTask("account-1"); !ok || !task.recovered {
		t.Fatal("failed startup cancellation must retain a recovered task")
	}
	if tasks, err := st.ListCamoufoxTasks(); err != nil || len(tasks) != 1 {
		t.Fatalf("failed startup cancellation lost durable task: tasks=%v err=%v", tasks, err)
	}
	reject.Store(false)
	_, err = be.LoginAccount("account-1", "", "123456")
	if backendErr, ok := err.(*BackendError); !ok || backendErr.Code != "OTP_EXPIRED" {
		t.Fatalf("old OTP must expire after recovery: %v", err)
	}
	if _, ok := be.getCamoufoxTask("account-1"); ok {
		t.Fatal("recovered task retained after cancellation")
	}
	if tasks, err := st.ListCamoufoxTasks(); err != nil || len(tasks) != 0 {
		t.Fatalf("recovered task retained in store: tasks=%v err=%v", tasks, err)
	}
}

func TestCamoufoxShutdownRetainsFailedCancellation(t *testing.T) {
	t.Setenv("ICLOUD_HME_CAMOUFOX_TOKEN", "test-token")
	st, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	be := &managerBackend{store: st}
	be.reserveCamoufoxTask("account-1", server.URL)
	be.setCamoufoxTask("account-1", "task-1")
	task, _ := be.getCamoufoxTask("account-1")
	if err := be.persistCamoufoxTask(task, "account-1"); err != nil {
		t.Fatal(err)
	}
	be.cancelAllCamoufoxTasks()
	if tasks, err := st.ListCamoufoxTasks(); err != nil || len(tasks) != 1 {
		t.Fatalf("failed shutdown cancellation lost durable task: tasks=%v err=%v", tasks, err)
	}
}

func TestConfiguredCamoufoxFailureDoesNotFallBack(t *testing.T) {
	t.Setenv("ICLOUD_HME_CAMOUFOX_TOKEN", "test-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	t.Setenv("ICLOUD_HME_CAMOUFOX_URL", server.URL)
	_, err := (&managerBackend{}).LoginAccount("account-1", "secret", "")
	if backendErr, ok := err.(*BackendError); !ok || backendErr.Code != "CAMOUFOX_UNAVAILABLE" {
		t.Fatalf("configured agent failure must be explicit: %v", err)
	}
}

func TestCamoufoxIncompleteSessionReportsActionableError(t *testing.T) {
	t.Setenv("ICLOUD_HME_CAMOUFOX_TOKEN", "test-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete {
			_, _ = w.Write([]byte(`{"success":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"task_id":"task-1","status":"failed","error_message":"未捕获完整的保持登录会话"}`))
	}))
	defer server.Close()
	be := &managerBackend{}
	be.reserveCamoufoxTask("account-1", server.URL)
	be.setCamoufoxTask("account-1", "task-1")
	_, err := be.pollCamoufoxTask("account-1", "task-1", server.URL, time.Second, false)
	backendErr, ok := err.(*BackendError)
	if !ok || backendErr.Code != "APPLE_AUTH_REJECTED" || backendErr.Message != "未捕获完整的保持登录会话，请重新登录并确认保持登录和信任浏览器" {
		t.Fatalf("incomplete session must have an actionable error: %v", err)
	}
	if _, ok := be.getCamoufoxTask("account-1"); ok {
		t.Fatal("failed task retained in backend")
	}
}

func TestCamoufoxClientDoesNotForwardTokenOnRedirect(t *testing.T) {
	t.Setenv("ICLOUD_HME_CAMOUFOX_TOKEN", "test-token")
	var forwarded atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Store(true)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer server.Close()
	if ok, _, err := camoufoxHealth(server.URL, time.Second); err == nil || ok {
		t.Fatalf("redirected health response must fail: ready=%v err=%v", ok, err)
	}
	if forwarded.Load() {
		t.Fatal("Camoufox request followed redirect")
	}
}

func TestPollCamoufoxTaskFailsFastOnHTTPError(t *testing.T) {
	t.Setenv("ICLOUD_HME_CAMOUFOX_TOKEN", "test-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	be := &managerBackend{}
	started := time.Now()
	_, err := be.pollCamoufoxTask("account-1", "task-1", server.URL, 30*time.Second, false)
	if time.Since(started) > 2*time.Second {
		t.Fatalf("HTTP 503 was not handled promptly: elapsed=%v", time.Since(started))
	}
	backendErr, ok := err.(*BackendError)
	if !ok || backendErr.Code != "UPSTREAM_FAILURE" {
		t.Fatalf("unexpected polling error: %v", err)
	}
}

func TestPollCamoufoxTaskReportsOTPInputFailure(t *testing.T) {
	t.Setenv("ICLOUD_HME_CAMOUFOX_TOKEN", "test-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete {
			_, _ = w.Write([]byte(`{"success":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"task_id":"task-1","status":"failed","error_message":"Apple ID 双重认证验证码输入失败"}`))
	}))
	defer server.Close()

	be := &managerBackend{}
	_, err := be.pollCamoufoxTask("account-1", "task-1", server.URL, time.Second, true)
	backendErr, ok := err.(*BackendError)
	if !ok || backendErr.Code != "OTP_INPUT_FAILED" || backendErr.Status != http.StatusBadGateway {
		t.Fatalf("OTP automation failure was reported as invalid code: %v", err)
	}
}

func TestCamoufoxOTPSubmissionExpiredTask(t *testing.T) {
	t.Setenv("ICLOUD_HME_CAMOUFOX_TOKEN", "test-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer server.Close()
	mgr, err := account.NewManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	acc, err := mgr.AddAccountWithInput(account.AddAccountInput{
		Name: "test", ICloudEmail: "test@example.com", Host: "icloud.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	be := &managerBackend{mgr: mgr}
	be.reserveCamoufoxTask(acc.ID, server.URL)
	be.setCamoufoxTask(acc.ID, "task-1")
	_, err = be.loginWithCamoufox(acc.ID, "", "123456", server.URL)
	backendErr, ok := err.(*BackendError)
	if !ok || backendErr.Code != "OTP_EXPIRED" {
		t.Fatalf("expired OTP task was not reported as expired: %v", err)
	}
	if _, ok := be.getCamoufoxTask(acc.ID); ok {
		t.Fatal("expired OTP task retained after proxy rejected submission")
	}
}

func TestCamoufoxURLRejectsRemoteHTTP(t *testing.T) {
	t.Setenv("ICLOUD_HME_CAMOUFOX_ALLOW_INSECURE", "")
	if err := validateCamoufoxURL("http://10.0.0.5:8089"); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("remote HTTP must require HTTPS, err=%v", err)
	}
	if err := validateCamoufoxURL("http://127.0.0.1:8089"); err != nil {
		t.Fatalf("loopback HTTP should remain valid: %v", err)
	}
}

func TestCancelCamoufoxLoginRetainsTaskOnAgentFailure(t *testing.T) {
	t.Setenv("ICLOUD_HME_CAMOUFOX_TOKEN", "test-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	be := &managerBackend{}
	be.reserveCamoufoxTask("account-1", server.URL)
	be.setCamoufoxTask("account-1", "task-1")
	if cancelled, err := be.CancelCamoufoxLogin("account-1", "task-1"); err == nil || cancelled {
		t.Fatalf("agent failure was reported as success: cancelled=%v err=%v", cancelled, err)
	}
	if task, ok := be.getCamoufoxTask("account-1"); !ok || task.taskID != "task-1" {
		t.Fatal("failed cancellation did not retain task for retry")
	}
}
