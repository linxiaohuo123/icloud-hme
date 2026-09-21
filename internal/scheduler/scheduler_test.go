/**
 * [INPUT]: 依赖 testing, os, path/filepath, time, fmt, icloud-hme/internal/account, icloud-hme/internal/hme, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 TestSchedulerRunOnce, TestSchedulerDoubleQuotaPrevention 等单元测试套件
 * [POS]: internal/scheduler 的调度生命周期与配额防重扣单元测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package scheduler

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/store"
)

func TestSchedulerRunOnce(t *testing.T) {
	tempDir := filepath.Join(os.TempDir(), "test_scheduler")
	_ = os.RemoveAll(tempDir)
	defer os.RemoveAll(tempDir)

	st, err := store.NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	accID := "acc_test_1"
	// 保存开启配置，额度 2
	_ = st.SaveScheduleConfig(store.ScheduleConfig{
		AccountID:   accID,
		Enabled:     true,
		HourlyQuota: 2,
	})

	mockAccounts := func() []account.Summary {
		return []account.Summary{
			{ID: accID, Name: "测试号", Status: "active"},
		}
	}

	createdCount := 0
	mockCreator := func(id, label string) (*hme.CreateResult, error) {
		createdCount++
		return &hme.CreateResult{Email: "created@icloud.com"}, nil
	}

	sched := NewScheduler(st, mockCreator, mockAccounts)

	// 触发一次调度
	created, errs := sched.RunOnce(false, 1)
	if created != 1 || errs != 0 {
		t.Fatalf("RunOnce failed: created=%d, errs=%d", created, errs)
	}

	logs := sched.Logs()
	if len(logs) < 3 {
		t.Fatalf("Logs too few: %d", len(logs))
	}
}

// 计划触发且 0 个启用账号时空转静默:不产生日志也不创建;手动触发正常执行
func TestSchedulerSilentIdleRound(t *testing.T) {
	tempDir := filepath.Join(os.TempDir(), "test_scheduler_idle")
	_ = os.RemoveAll(tempDir)
	defer os.RemoveAll(tempDir)

	st, err := store.NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	// 账号存在但未启用调度
	mockAccounts := func() []account.Summary {
		return []account.Summary{
			{ID: "acc_idle", Name: "闲置号", Status: "active"},
		}
	}
	creations := 0
	mockCreator := func(id, label string) (*hme.CreateResult, error) {
		creations++
		return &hme.CreateResult{Email: "x@icloud.com"}, nil
	}
	sched := NewScheduler(st, mockCreator, mockAccounts)

	// 计划触发:无启用账号 → 静默空转,零日志零创建
	if _, _ = sched.RunOnce(false, 1); len(sched.Logs()) != 0 || creations != 0 {
		t.Fatalf("idle scheduled round should be silent, logs=%v creations=%d", sched.Logs(), creations)
	}

	// 立即执行:未启用定时任务的账号不得被顺手建号(否则一次点击会对全部母号突发请求)
	if _, _ = sched.RunOnce(true, 1); creations != 0 || len(sched.Logs()) != 1 {
		t.Fatalf("manual round must skip disabled accounts, creations=%d logs=%v", creations, sched.Logs())
	}

	// 强制全部补货:显式 API 才对未启用账号建号(开始+成功+结束)
	if _, _ = sched.RunAllNow(1); creations != 1 || len(sched.Logs()) != 4 {
		t.Fatalf("force-all round should run once, creations=%d logs=%v", creations, sched.Logs())
	}
}

// RunOnce 前后 Status 的 running/last_run_at 如实翻转
func TestSchedulerStatus(t *testing.T) {
	tempDir := filepath.Join(os.TempDir(), "test_scheduler_status")
	_ = os.RemoveAll(tempDir)
	defer os.RemoveAll(tempDir)

	st, err := store.NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	accID := "acc_status"
	_ = st.SaveScheduleConfig(store.ScheduleConfig{
		AccountID:   accID,
		Enabled:     true,
		HourlyQuota: 1,
	})
	mockAccounts := func() []account.Summary {
		return []account.Summary{{ID: accID, Name: "状态号", Status: "active"}}
	}
	mockCreator := func(id, label string) (*hme.CreateResult, error) {
		return &hme.CreateResult{Email: "created@icloud.com"}, nil
	}
	sched := NewScheduler(st, mockCreator, mockAccounts)

	before := sched.Status()
	if before.Running || before.LastRunAt != "" {
		t.Fatalf("unexpected initial status: %+v", before)
	}

	_, _ = sched.RunOnce(true, 1)

	after := sched.Status()
	if after.Running {
		t.Fatalf("running should be false after RunOnce returns")
	}
	if after.LastRunAt == "" {
		t.Fatalf("last_run_at should be set after a completed round")
	}
	if after.IntervalSec != 300 {
		t.Fatalf("interval_seconds = %d, want 300", after.IntervalSec)
	}
}

func TestSchedulerCustomLabel(t *testing.T) {
	tempDir := filepath.Join(os.TempDir(), "test_scheduler_custom_label")
	_ = os.RemoveAll(tempDir)
	defer os.RemoveAll(tempDir)

	st, err := store.NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	accID := "acc_test_custom"
	_ = st.SaveScheduleConfig(store.ScheduleConfig{
		AccountID:   accID,
		Enabled:     true,
		HourlyQuota: 5,
		AliasLabel:  "gpt-{seq}",
	})

	mockAccounts := func() []account.Summary {
		return []account.Summary{
			{ID: accID, Name: "ChatGPT账号", Status: "active"},
		}
	}

	var capturedLabel string
	mockCreator := func(id, label string) (*hme.CreateResult, error) {
		capturedLabel = label
		return &hme.CreateResult{Email: "gpt@icloud.com"}, nil
	}

	sched := NewScheduler(st, mockCreator, mockAccounts)
	created, errs := sched.RunOnce(false, 1)
	if created != 1 || errs != 0 {
		t.Fatalf("RunOnce failed: created=%d, errs=%d", created, errs)
	}

	if capturedLabel != "gpt-1" {
		t.Fatalf("expected label 'gpt-1', got '%s'", capturedLabel)
	}
}

func TestIsInDailyWindow(t *testing.T) {
	// 2026-09-20 14:30:00
	baseTime := time.Date(2026, 9, 20, 14, 30, 0, 0, time.Local)

	// 1. 常规时间窗口: 09:00 - 18:00 (14:30 在窗口内)
	if !isInDailyWindow(baseTime, "09:00", "18:00") {
		t.Fatalf("14:30 should be in 09:00-18:00")
	}
	// 2. 常规时间窗口: 15:00 - 18:00 (14:30 在窗口外)
	if isInDailyWindow(baseTime, "15:00", "18:00") {
		t.Fatalf("14:30 should NOT be in 15:00-18:00")
	}
	// 3. 跨午夜时间窗口: 22:00 - 06:00 (14:30 在窗口外)
	if isInDailyWindow(baseTime, "22:00", "06:00") {
		t.Fatalf("14:30 should NOT be in 22:00-06:00")
	}
	// 4. 跨午夜时间窗口: 12:00 - 04:00 (14:30 在窗口内)
	if !isInDailyWindow(baseTime, "12:00", "04:00") {
		t.Fatalf("14:30 should be in 12:00-04:00")
	}
	// 5. 空或非法输入默认放行
	if !isInDailyWindow(baseTime, "", "") {
		t.Fatalf("empty should return true")
	}
	if !isInDailyWindow(baseTime, "invalid", "18:00") {
		t.Fatalf("invalid should return true")
	}
}

func TestSchedulerAliasLimitCircuitBreak(t *testing.T) {
	tempDir := filepath.Join(os.TempDir(), "test_scheduler_limit")
	_ = os.RemoveAll(tempDir)
	defer os.RemoveAll(tempDir)

	st, err := store.NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	accID := "acc_full"
	_ = st.SaveScheduleConfig(store.ScheduleConfig{
		AccountID:   accID,
		Enabled:     true,
		HourlyQuota: 5,
	})

	accID2 := "acc_full_total"
	st.SaveScheduleConfig(store.ScheduleConfig{
		AccountID:   accID2,
		Enabled:     true,
		HourlyQuota: 5,
	})

	mockAccounts := func() []account.Summary {
		return []account.Summary{
			{ID: accID, Name: "活跃满额号", Status: "active", AliasActive: 500},
			{ID: accID2, Name: "总数满额号", Status: "active", AliasTotal: 500, AliasActive: 100},
		}
	}

	creations := 0
	mockCreator := func(id, label string) (*hme.CreateResult, error) {
		creations++
		return &hme.CreateResult{Email: "full@icloud.com"}, nil
	}

	sched := NewScheduler(st, mockCreator, mockAccounts)
	created, errs := sched.RunOnce(false, 1)
	if created != 0 || errs != 0 || creations != 0 {
		t.Fatalf("account with 500 aliases should be skipped, created=%d creations=%d", created, creations)
	}
}

func TestIsTransientCreateError(t *testing.T) {
	transientErrs := []error{
		fmt.Errorf("HTTP 421: Misdirected Request"),
		fmt.Errorf("HTTP 429: Too Many Requests"),
		fmt.Errorf("HTTP 502: Bad Gateway"),
		fmt.Errorf("dial tcp: i/o timeout"),
		fmt.Errorf("connection reset by peer"),
		fmt.Errorf("service temporarily unavailable"),
	}
	for _, err := range transientErrs {
		if !isTransientCreateError(err) {
			t.Fatalf("expected transient error for: %v", err)
		}
	}

	terminalErrs := []error{
		fmt.Errorf("HTTP 401: Unauthorized"),
		fmt.Errorf("HTTP 403: Forbidden"),
		fmt.Errorf("Cookie expired"),
		fmt.Errorf("invalid label"),
	}
	for _, err := range terminalErrs {
		if isTransientCreateError(err) {
			t.Fatalf("expected non-transient error for: %v", err)
		}
	}
}

func TestDurationExpired(t *testing.T) {
	now := time.Now()
	// 1. 未启用 duration 模式
	cfg1 := store.ScheduleConfig{Mode: "daily_window", DurationHours: 12}
	if isDurationExpired(cfg1, now) {
		t.Fatalf("mode != duration should not expire")
	}

	// 2. 启动于 2 小时前，持续 12 小时 (未过期)
	cfg2 := store.ScheduleConfig{
		Mode:          "duration",
		DurationHours: 12,
		StartedAt:     now.Add(-2 * time.Hour).Format(time.RFC3339),
	}
	if isDurationExpired(cfg2, now) {
		t.Fatalf("2h into 12h should not expire")
	}

	// 3. 启动于 13 小时前，持续 12 小时 (已过期)
	cfg3 := store.ScheduleConfig{
		Mode:          "duration",
		DurationHours: 12,
		StartedAt:     now.Add(-13 * time.Hour).Format(time.RFC3339),
	}
	if !isDurationExpired(cfg3, now) {
		t.Fatalf("13h into 12h should expire")
	}
}

// TestSchedulerDoubleQuotaPrevention 验证调度器调用原子出号门面时绝不发生配额双重扣减。
func TestSchedulerDoubleQuotaPrevention(t *testing.T) {
	tempDir := filepath.Join(os.TempDir(), fmt.Sprintf("test_sched_quota_%d", os.Getpid()))
	_ = os.RemoveAll(tempDir)
	defer os.RemoveAll(tempDir)

	st, err := store.NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer st.Close()

	accID := "acc_quota_test"
	// 设置小时配额为 2
	_ = st.SaveScheduleConfig(store.ScheduleConfig{
		AccountID:   accID,
		Enabled:     true,
		HourlyQuota: 2,
	})

	mockAccounts := func() []account.Summary {
		return []account.Summary{
			{ID: accID, Name: "配额账号", Status: "active"},
		}
	}

	// 模拟 production server.go 中的 creator (它会调用 TryReserveQuota)
	createdCount := 0
	mockCreator := func(id, label string) (*hme.CreateResult, error) {
		allowed, _ := st.TryReserveQuota(id, 1)
		if !allowed {
			return nil, fmt.Errorf("RATE_LIMITED: 配额已用完")
		}
		createdCount++
		return &hme.CreateResult{Email: fmt.Sprintf("alias_%d@icloud.com", createdCount)}, nil
	}

	sched := NewScheduler(st, mockCreator, mockAccounts)

	// 要求创建 2 个
	created, errs := sched.RunOnce(false, 2)
	if created != 2 || errs != 0 {
		t.Fatalf("期望成功创建满配额 2 个，实际 created=%d errs=%d", created, errs)
	}

	cfg := st.GetScheduleConfig(accID)
	if cfg.CurrentHourCount != 2 {
		t.Fatalf("配额计数应精确为 2 (无双重扣减)，实际为 %d", cfg.CurrentHourCount)
	}

	// 再次尝试创建，应被前置配额守卫拦截，created=0, errs=0
	created2, _ := sched.RunOnce(false, 1)
	if created2 != 0 {
		t.Fatalf("配额耗尽后应被前置守卫拦截，实际 created=%d", created2)
	}
}
