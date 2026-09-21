/**
 * [INPUT]: 依赖 testing, os, path/filepath, time, fmt
 * [OUTPUT]: 对外提供 TestStorePersistence, TestStoreMigration, TestStoreLeasePagination, TestScheduleConfigHourWindowReset, TestDeleteScheduleConfig, TestStoreConnectionPragmas
 * [POS]: internal/store 的单元测试套件，验证 SQLite 持久化、自动迁移、分页检索、物理删除、时钟窗口配额重置与 DSN PRAGMA 连接继承正确性
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// 并发创建业务标识/令牌不得互相覆盖。
//
// 回归防线:主键曾用 time.Now().UnixNano() 生成，Windows 时钟粒度约 15ms，
// 同一刻度内的并发创建会得到相同 ID，再叠加 ON CONFLICT(id) DO UPDATE
// 就会静默覆盖掉刚写入的记录(接口照常返回 200，但记录消失、旧令牌失效)。
func TestStoreConcurrentCreateNoOverwrite(t *testing.T) {
	tempDir := filepath.Join(os.TempDir(), "test_store_concurrent_create")
	_ = os.RemoveAll(tempDir)
	defer os.RemoveAll(tempDir)

	s, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer s.Close()

	const n = 64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.SaveToken(APIToken{
				Name:  fmt.Sprintf("tok-%d", i),
				Token: fmt.Sprintf("am_concurrent_%d", i),
			}); err != nil {
				t.Errorf("SaveToken(%d) failed: %v", i, err)
			}
			if err := s.SaveTag(BusinessTag{
				Tag:  fmt.Sprintf("biz-%d", i),
				Name: fmt.Sprintf("Biz%d", i),
			}); err != nil {
				t.Errorf("SaveTag(%d) failed: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if got := len(s.ListTokens()); got != n {
		t.Fatalf("并发创建的令牌被覆盖: 期望 %d 行, 实际 %d 行", n, got)
	}
	if got := len(s.ListTags()); got != n {
		t.Fatalf("并发创建的业务标识被覆盖: 期望 %d 行, 实际 %d 行", n, got)
	}
	// 每个令牌都必须仍然可用(被覆盖会让旧令牌静默失效)
	for i := 0; i < n; i++ {
		if !s.ValidateToken(fmt.Sprintf("am_concurrent_%d", i)) {
			t.Fatalf("令牌 am_concurrent_%d 已失效(被同 ID 记录覆盖)", i)
		}
	}
}

// 主键生成器必须每次返回不同值，且不能依赖时钟精度。
func TestNewOpaqueIDIsUnique(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := NewBusinessTagID()
		if seen[id] {
			t.Fatalf("主键重复: %s", id)
		}
		seen[id] = true
	}
}

// 别名邮箱大小写必须归一：mail_sync 依靠 lease 反查「别名→母号」路由，
// 而代码库其它位置(verify_handler 等)用 EqualFold 比较，说明别名邮箱并不保证全小写。
func TestFindLeaseAccountIsCaseInsensitive(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer s.Close()

	// 混合大小写写入(模拟 Apple/历史数据)
	if err := s.RecordLease(LeaseRecord{
		Email:       "Mixed.Case@iCloud.com",
		AccountID:   "acc_9",
		Tag:         "t",
		Status:      "completed",
		AllocatedAt: time.Now().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("RecordLease failed: %v", err)
	}

	for _, q := range []string{"mixed.case@icloud.com", "MIXED.CASE@ICLOUD.COM", " Mixed.Case@iCloud.com "} {
		id, ok := s.FindLeaseAccount(q)
		if !ok || id != "acc_9" {
			t.Fatalf("FindLeaseAccount(%q) 应命中 acc_9 且大小写不敏感, 实际 ok=%v id=%q", q, ok, id)
		}
	}
}

// 别名路由表: 出号即写穿，取码链路可直接主键点查归属。
func TestAliasRouteWriteThroughAndLookup(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer s.Close()

	if err := s.RecordLease(LeaseRecord{
		Email:       "Fresh.Alias@iCloud.com",
		AccountID:   "acc_1",
		Status:      "completed",
		AllocatedAt: time.Now().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("RecordLease failed: %v", err)
	}

	// 写入即归一 + 立刻可点查
	if id, ok := s.FindAliasRoute("fresh.alias@icloud.com"); !ok || id != "acc_1" {
		t.Fatalf("RecordLease 应写穿路由表, 实际 ok=%v id=%q", ok, id)
	}

	// 幂等 + 改派 + 忽略空值
	if err := s.UpsertAliasRoutes("acc_2", []string{"fresh.alias@icloud.com", "   "}); err != nil {
		t.Fatalf("UpsertAliasRoutes failed: %v", err)
	}
	if id, _ := s.FindAliasRoute("fresh.alias@icloud.com"); id != "acc_2" {
		t.Fatalf("路由应可改派到 acc_2, 实际 %q", id)
	}
	if got := s.CountAliasRoutes(); got != 1 {
		t.Fatalf("空别名应被忽略, 期望 1 条路由, 实际 %d", got)
	}
}

// 用出号流水回填路由表: 覆盖本功能上线前已分配的别名，且兼容历史混合大小写。
func TestAliasRoutesBackfillFromLegacyLeases(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	legacy := []LeaseRecord{
		{ID: "l1", Email: "Legacy.One@iCloud.com", AccountID: "acc_1", Status: "completed", AllocatedAt: "2026-01-01T00:00:00Z"},
		{ID: "l2", Email: "legacy.two@icloud.com", AccountID: "acc_2", Status: "completed", AllocatedAt: "2026-01-02T00:00:00Z"},
	}
	for _, rec := range legacy {
		if err := s.RecordLease(rec); err != nil {
			t.Fatalf("RecordLease failed: %v", err)
		}
	}
	// 清空路由表，模拟"升级前的老库"
	if _, err := s.db.Exec(`DELETE FROM alias_routes`); err != nil {
		t.Fatalf("清空路由表失败: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// 重新打开 → 触发出号流水回填
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("重新打开 Store 失败: %v", err)
	}
	defer s2.Close()

	if got := s2.CountAliasRoutes(); got != 2 {
		t.Fatalf("回填后应有 2 条路由, 实际 %d", got)
	}
	for email, want := range map[string]string{
		"legacy.one@icloud.com": "acc_1",
		"LEGACY.TWO@ICLOUD.COM": "acc_2",
	} {
		if id, ok := s2.FindAliasRoute(email); !ok || id != want {
			t.Fatalf("回填后 FindAliasRoute(%q) 应为 %s, 实际 ok=%v id=%q", email, want, ok, id)
		}
	}

	// 回填只应执行一次: 标记落库后再打开不得重复全表扫
	if err := s2.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	s3, err := NewStore(dir)
	if err != nil {
		t.Fatalf("第三次打开 Store 失败: %v", err)
	}
	defer s3.Close()
	if got := s3.GetSetting(settingKeyAliasRoutesBackfilled); got != "1" {
		t.Fatalf("回填标记应已落库, 实际 %q", got)
	}
	if got := s3.CountAliasRoutes(); got != 2 {
		t.Fatalf("重复打开不应改变路由条数, 实际 %d", got)
	}
}

// 回填标记落库后，即使流水里新增了没有路由的记录，也应由按需自愈补上。
func TestAliasRouteOnDemandHealFallback(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer s.Close()

	// 模拟标记已置位、但流水里存在一条缺路由的历史记录
	if err := s.SaveSetting(settingKeyAliasRoutesBackfilled, "1"); err != nil {
		t.Fatalf("SaveSetting failed: %v", err)
	}
	if err := s.RecordLease(LeaseRecord{
		Email:       "orphan@icloud.com",
		AccountID:   "acc_orphan",
		Status:      "completed",
		AllocatedAt: time.Now().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("RecordLease failed: %v", err)
	}
	// 出号写穿已建立路由；这里直接删掉以模拟"写穿失败"的残留状态
	if _, err := s.db.Exec(`DELETE FROM alias_routes WHERE email = 'orphan@icloud.com'`); err != nil {
		t.Fatalf("构造残留状态失败: %v", err)
	}
	if _, ok := s.FindAliasRoute("orphan@icloud.com"); ok {
		t.Fatal("前置条件: 路由应已被删除")
	}
	// 流水兜底仍应能解析出归属(mail_sync 的 resolveAccountFromStore 依赖它)
	if id, ok := s.FindLeaseAccount("orphan@icloud.com"); !ok || id != "acc_orphan" {
		t.Fatalf("流水兜底应能解析归属, 实际 ok=%v id=%q", ok, id)
	}
}

// 流水清理只删过期流水，绝不能影响别名路由表(取码链路依赖它长期保留)。
func TestPruneLeasesKeepsAliasRoutes(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer s.Close()

	oldTS := time.Now().Add(-400 * 24 * time.Hour).Format(time.RFC3339)
	recentTS := time.Now().Add(-time.Hour).Format(time.RFC3339)
	seed := []LeaseRecord{
		{ID: "old1", Email: "old1@icloud.com", AccountID: "acc_1", AllocatedAt: oldTS},
		{ID: "old2", Email: "old2@icloud.com", AccountID: "acc_1", AllocatedAt: oldTS},
		{ID: "new1", Email: "new1@icloud.com", AccountID: "acc_1", AllocatedAt: recentTS},
	}
	for _, rec := range seed {
		if err := s.RecordLease(rec); err != nil {
			t.Fatalf("RecordLease(%s): %v", rec.ID, err)
		}
	}
	if routes := s.CountAliasRoutes(); routes != 3 {
		t.Fatalf("前置条件: 应有 3 条路由, 实际 %d", routes)
	}

	cutoff := time.Now().Add(-180 * 24 * time.Hour)
	n, err := s.PruneLeases(cutoff, 100)
	if err != nil {
		t.Fatalf("PruneLeases failed: %v", err)
	}
	if n != 2 {
		t.Fatalf("应删除 2 条过期流水, 实际 %d", n)
	}
	if got := s.CountLeases(); got != 1 {
		t.Fatalf("清理后应剩 1 条流水, 实际 %d", got)
	}
	// 关键:过期流水对应的别名仍然必须可路由
	if routes := s.CountAliasRoutes(); routes != 3 {
		t.Fatalf("路由表不得随流水清理而减少: 期望 3, 实际 %d", routes)
	}
	if id, ok := s.FindAliasRoute("old1@icloud.com"); !ok || id != "acc_1" {
		t.Fatalf("过期流水的别名仍应可路由, 实际 ok=%v id=%q", ok, id)
	}
	// 幂等:再清理一次不再删除
	if n2, _ := s.PruneLeases(cutoff, 100); n2 != 0 {
		t.Fatalf("重复清理不应再删数据, 实际 %d", n2)
	}
}

// 账号注销必须级联清理路由，否则取码会指向已不存在的账号。
func TestDeleteAccountCascadesAliasRoutes(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer s.Close()

	now := time.Now().Format(time.RFC3339)
	if err := s.SaveAccount(&AccountRecord{
		ID: "acc_gone", Name: "x", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("SaveAccount failed: %v", err)
	}
	if err := s.UpsertAliasRoutes("acc_gone", []string{"a@icloud.com", "b@icloud.com"}); err != nil {
		t.Fatalf("UpsertAliasRoutes failed: %v", err)
	}
	if got := s.CountAliasRoutes(); got != 2 {
		t.Fatalf("期望 2 条路由, 实际 %d", got)
	}

	if err := s.DeleteAccount("acc_gone"); err != nil {
		t.Fatalf("DeleteAccount failed: %v", err)
	}
	if got := s.CountAliasRoutes(); got != 0 {
		t.Fatalf("账号注销后路由应被级联清理, 实际残留 %d 条", got)
	}
}

func TestStorePersistence(t *testing.T) {
	tempDir := filepath.Join(os.TempDir(), "test_store_persistence")
	_ = os.RemoveAll(tempDir)
	defer os.RemoveAll(tempDir)

	s, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer s.Close()

	// 1. Tags
	tag := BusinessTag{Name: "GPT 注册", Tag: "gpt-register", Description: "用于注册"}
	if err := s.SaveTag(tag); err != nil {
		t.Fatalf("SaveTag failed: %v", err)
	}
	tags := s.ListTags()
	if len(tags) != 1 || tags[0].Tag != "gpt-register" {
		t.Fatalf("Tags mismatch: %+v", tags)
	}
	s.UpdateTagLastAssigned("gpt-register")
	tags = s.ListTags()
	if tags[0].LastAssignedAt == "" {
		t.Fatal("UpdateTagLastAssigned failed to update timestamp")
	}

	// 2. Tokens
	tok := APIToken{Name: "领号脚本", Token: "tok_secret_123"}
	if err := s.SaveToken(tok); err != nil {
		t.Fatalf("SaveToken failed: %v", err)
	}
	if !s.ValidateToken("tok_secret_123") {
		t.Fatal("ValidateToken failed")
	}
	if s.ValidateToken("wrong_token") {
		t.Fatal("ValidateToken should fail for invalid token")
	}

	// 3. Leases
	lease := LeaseRecord{Email: "test1@icloud.com", AccountID: "acc_1", Tag: "gpt-register"}
	if err := s.RecordLease(lease); err != nil {
		t.Fatalf("RecordLease failed: %v", err)
	}
	records, total := s.ListLeases("", "gpt-register", "all", 10, 0)
	if total != 1 || len(records) != 1 {
		t.Fatalf("ListLeases failed: count=%d, total=%d", len(records), total)
	}

	// 4. Schedules & Quota
	allowed, count := s.IncrementHourlyQuota("acc_1")
	if !allowed || count != 1 {
		t.Fatalf("IncrementHourlyQuota failed: allowed=%v, count=%d", allowed, count)
	}

	// 5. Reload test
	s2, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore reload failed: %v", err)
	}
	defer s2.Close()
	if len(s2.ListTags()) != 1 || len(s2.ListTokens()) != 1 {
		t.Fatal("Reload data missing")
	}
}

func TestStoreMigration(t *testing.T) {
	tempDir := filepath.Join(os.TempDir(), "test_store_migration")
	_ = os.RemoveAll(tempDir)
	defer os.RemoveAll(tempDir)
	_ = os.MkdirAll(tempDir, 0755)

	// 写入模拟遗留 JSON 文件
	legacyTags := `{"tag_1":{"id":"tag_1","name":"旧标签","tag":"legacy-tag","status":"active","created_at":"2026-01-01T00:00:00Z"}}`
	_ = os.WriteFile(filepath.Join(tempDir, "tags.json"), []byte(legacyTags), 0644)

	legacyTokens := `{"tok_1":{"id":"tok_1","name":"旧令牌","token":"legacy_tok","created_at":"2026-01-01T00:00:00Z"}}`
	_ = os.WriteFile(filepath.Join(tempDir, "tokens.json"), []byte(legacyTokens), 0644)

	legacyLeases := `[{"id":"lease_1","email":"legacy@icloud.com","account_id":"acc_old","tag":"legacy-tag","status":"completed","allocated_at":"2026-01-01T00:00:00Z","token_name":"prod-token"}]`
	_ = os.WriteFile(filepath.Join(tempDir, "leases.json"), []byte(legacyLeases), 0644)

	legacySchedules := `{"acc_old":{"account_id":"acc_old","enabled":true,"hourly_quota":10,"alias_label":"gpt","mode":"daily_window","start_time":"09:00","end_time":"18:00","duration_hours":3,"started_at":"2026-01-01T00:00:00Z","current_hour_count":2,"last_hour_window":12345,"last_run_at":"2026-01-01T00:00:00Z"}}`
	_ = os.WriteFile(filepath.Join(tempDir, "schedules.json"), []byte(legacySchedules), 0644)

	// 初始化 Store 触发自动迁移
	s, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore with migration failed: %v", err)
	}
	defer s.Close()

	// 验证数据已迁移到 SQLite
	tags := s.ListTags()
	if len(tags) != 1 || tags[0].Tag != "legacy-tag" {
		t.Fatalf("Migrated tags mismatch: %+v", tags)
	}

	tokens := s.ListTokens()
	if len(tokens) != 1 || tokens[0].Token != "legacy_tok" {
		t.Fatalf("Migrated tokens mismatch: %+v", tokens)
	}

	leases, total := s.ListLeases("", "", "all", 10, 0)
	if total != 1 || len(leases) != 1 || leases[0].Email != "legacy@icloud.com" {
		t.Fatalf("Migrated leases mismatch: total=%d, leases=%+v", total, leases)
	}
	// 流水的 Token 归属必须一并迁移，否则审计链断裂
	if leases[0].TokenName != "prod-token" {
		t.Fatalf("lease token_name was dropped during migration: %+v", leases[0])
	}

	sched := s.GetScheduleConfig("acc_old")
	if !sched.Enabled || sched.HourlyQuota != 10 {
		t.Fatalf("Migrated schedule mismatch: %+v", sched)
	}
	// 运行模式与时间窗口必须一并迁移:退化成 always 会把"每天9-18点"变成 7x24 全天发号
	if sched.Mode != "daily_window" || sched.AliasLabel != "gpt" ||
		sched.StartTime != "09:00" || sched.EndTime != "18:00" || sched.DurationHours != 3 {
		t.Fatalf("schedule window/mode was dropped during migration: %+v", sched)
	}

	// 验证旧文件已归档(带时间戳，避免覆盖人工备份)
	matches, _ := filepath.Glob(filepath.Join(tempDir, "tags.json*migrated"))
	if len(matches) == 0 {
		t.Fatal("tags.json was not archived to a .migrated file")
	}
	if _, err := os.Stat(filepath.Join(tempDir, "tags.json")); !os.IsNotExist(err) {
		t.Fatal("tags.json should have been moved away after a successful migration")
	}
}

// 解析失败的遗留 JSON 绝不能被归档，必须保留现场以便重试或人工修复。
func TestStoreMigrationKeepsCorruptSource(t *testing.T) {
	tempDir := filepath.Join(os.TempDir(), "test_store_migration_corrupt")
	_ = os.RemoveAll(tempDir)
	defer os.RemoveAll(tempDir)
	_ = os.MkdirAll(tempDir, 0755)

	corruptPath := filepath.Join(tempDir, "tags.json")
	_ = os.WriteFile(corruptPath, []byte(`{"tag_1": {`), 0644)

	s, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore with corrupt legacy file should not fail: %v", err)
	}
	defer s.Close()

	if _, err := os.Stat(corruptPath); err != nil {
		t.Fatalf("corrupt tags.json must be preserved for manual repair, stat err=%v", err)
	}
	if len(s.ListTags()) != 0 {
		t.Fatalf("corrupt file should migrate nothing")
	}
}

func TestStoreLeasePagination(t *testing.T) {
	tempDir := filepath.Join(os.TempDir(), "test_store_lease_paged")
	_ = os.RemoveAll(tempDir)
	defer os.RemoveAll(tempDir)

	s, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer s.Close()

	// 写入 150 条记录
	for i := 1; i <= 150; i++ {
		rec := LeaseRecord{
			ID:          fmt.Sprintf("lease_%03d", i),
			Email:       fmt.Sprintf("user_%03d@apple.com", i),
			AccountID:   "acc_1",
			Tag:         "batch-test",
			Status:      "completed",
			AllocatedAt: fmt.Sprintf("2026-03-%02dT10:00:00Z", (i%28)+1),
		}
		if err := s.RecordLease(rec); err != nil {
			t.Fatalf("RecordLease %d failed: %v", i, err)
		}
	}

	// 分页查询第一页 (50 条)
	p1, total := s.ListLeases("", "batch-test", "completed", 50, 0)
	if total != 150 || len(p1) != 50 {
		t.Fatalf("Page 1 mismatch: total=%d, len=%d", total, len(p1))
	}

	// 分页查询最后一页 (50 条)
	p3, total3 := s.ListLeases("", "batch-test", "completed", 50, 100)
	if total3 != 150 || len(p3) != 50 {
		t.Fatalf("Page 3 mismatch: total=%d, len=%d", total3, len(p3))
	}

	// 越界查询
	empty, totalEmpty := s.ListLeases("", "batch-test", "completed", 50, 200)
	if totalEmpty != 150 || len(empty) != 0 {
		t.Fatalf("Out of bounds mismatch: total=%d, len=%d", totalEmpty, len(empty))
	}
}

func BenchmarkRecordLease(b *testing.B) {
	tempDir := filepath.Join(os.TempDir(), fmt.Sprintf("bench_store_%d", os.Getpid()))
	_ = os.RemoveAll(tempDir)
	defer os.RemoveAll(tempDir)

	s, err := NewStore(tempDir)
	if err != nil {
		b.Fatalf("NewStore failed: %v", err)
	}
	defer s.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.RecordLease(LeaseRecord{
			ID:          fmt.Sprintf("lease_%d", i),
			Email:       fmt.Sprintf("user_%d@icloud.com", i),
			AccountID:   "acc_1",
			Tag:         "bench",
			Status:      "completed",
			AllocatedAt: "2026-09-20T12:00:00Z",
		})
	}
}

func BenchmarkListLeases(b *testing.B) {
	tempDir := filepath.Join(os.TempDir(), fmt.Sprintf("bench_store_list_%d", os.Getpid()))
	_ = os.RemoveAll(tempDir)
	defer os.RemoveAll(tempDir)

	s, err := NewStore(tempDir)
	if err != nil {
		b.Fatalf("NewStore failed: %v", err)
	}
	defer s.Close()

	for i := 0; i < 500; i++ {
		_ = s.RecordLease(LeaseRecord{
			ID:          fmt.Sprintf("lease_%d", i),
			Email:       fmt.Sprintf("user_%d@icloud.com", i),
			AccountID:   "acc_1",
			Tag:         "bench",
			Status:      "completed",
			AllocatedAt: "2026-09-20T12:00:00Z",
		})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.ListLeases("", "bench", "completed", 50, 0)
	}
}

func TestScheduleConfigHourWindowReset(t *testing.T) {
	tempDir := filepath.Join(os.TempDir(), fmt.Sprintf("test_store_hour_reset_%d", os.Getpid()))
	_ = os.RemoveAll(tempDir)
	defer os.RemoveAll(tempDir)

	s, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer s.Close()

	// 保存一条属于过去时间窗口 (例如 window=100) 且已消耗 5 个配额的调度配置
	pastCfg := ScheduleConfig{
		AccountID:        "acc_past",
		Enabled:          true,
		HourlyQuota:      10,
		CurrentHourCount: 5,
		LastHourWindow:   100, // 远早于当前时钟窗口
	}
	if err := s.SaveScheduleConfig(pastCfg); err != nil {
		t.Fatalf("SaveScheduleConfig failed: %v", err)
	}

	// 1. GetScheduleConfig 读取时应当天衣无缝地重置 CurrentHourCount 为 0
	readCfg := s.GetScheduleConfig("acc_past")
	if readCfg.CurrentHourCount != 0 {
		t.Fatalf("跨小时后 CurrentHourCount 应自动重置为 0, 实际得到: %d", readCfg.CurrentHourCount)
	}

	// 2. ListScheduleConfigs 读取时也应当一致性返回 0
	listConfigs := s.ListScheduleConfigs()
	if len(listConfigs) == 0 || listConfigs[0].CurrentHourCount != 0 {
		t.Fatalf("ListScheduleConfigs 中 CurrentHourCount 应自动重置为 0, 实际得到: %d", listConfigs[0].CurrentHourCount)
	}

	// 3. RemainingQuota 应当正确呈现为完整的 10
	rem := s.RemainingQuota("acc_past")
	if rem != 10 {
		t.Fatalf("剩余配额应为完整的 10, 实际得到: %d", rem)
	}
}

func TestDeleteScheduleConfig(t *testing.T) {
	tempDir := filepath.Join(os.TempDir(), "test_delete_schedule_config")
	_ = os.RemoveAll(tempDir)
	defer os.RemoveAll(tempDir)

	s, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer s.Close()

	cfg := ScheduleConfig{
		AccountID:   "acc_to_delete",
		Enabled:     true,
		HourlyQuota: 8,
		AliasLabel:  "delete_test",
	}
	if err := s.SaveScheduleConfig(cfg); err != nil {
		t.Fatalf("SaveScheduleConfig failed: %v", err)
	}

	if len(s.ListScheduleConfigs()) != 1 {
		t.Fatal("保存后配置列表应有 1 条记录")
	}

	if err := s.DeleteScheduleConfig("acc_to_delete"); err != nil {
		t.Fatalf("DeleteScheduleConfig 失败: %v", err)
	}

	if len(s.ListScheduleConfigs()) != 0 {
		t.Fatal("物理删除后配置列表应为空，杜绝幽灵记录")
	}
}

func TestStoreConnectionPragmas(t *testing.T) {
	tempDir := filepath.Join(os.TempDir(), "test_store_pragmas")
	_ = os.RemoveAll(tempDir)
	defer os.RemoveAll(tempDir)

	s, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer s.Close()

	// 验证新检出的连接上 busy_timeout 和 foreign_keys 是否继承了 DSN 配置
	var busyTimeout int
	if err := s.db.QueryRow("PRAGMA busy_timeout;").Scan(&busyTimeout); err != nil {
		t.Fatalf("查询 busy_timeout 失败: %v", err)
	}
	if busyTimeout < 5000 {
		t.Fatalf("期望 busy_timeout >= 5000, 实际: %d", busyTimeout)
	}

	var foreignKeys int
	if err := s.db.QueryRow("PRAGMA foreign_keys;").Scan(&foreignKeys); err != nil {
		t.Fatalf("查询 foreign_keys 失败: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("期望 foreign_keys == 1, 实际: %d", foreignKeys)
	}
}

func TestStoreUpdateLeaseStatusCaseInsensitive(t *testing.T) {
	tempDir := filepath.Join(os.TempDir(), "test_store_lease_status_case")
	_ = os.RemoveAll(tempDir)
	defer os.RemoveAll(tempDir)

	s, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer s.Close()

	rec := LeaseRecord{
		ID:          "lease_case_1",
		Email:       "MixedCase.User@example.com",
		AccountID:   "acc_1",
		Tag:         "test",
		Status:      "leased",
		AllocatedAt: time.Now().Format(time.RFC3339),
	}
	if err := s.RecordLease(rec); err != nil {
		t.Fatalf("RecordLease failed: %v", err)
	}

	// 传入全大写邮箱更新状态，应利用 LOWER(email) 成功更新
	if err := s.UpdateLeaseStatus("MIXEDCASE.USER@EXAMPLE.COM", "completed"); err != nil {
		t.Fatalf("UpdateLeaseStatus with uppercase email failed: %v", err)
	}

	leases, _ := s.ListLeases("", "test", "completed", 10, 0)
	if len(leases) != 1 || leases[0].Email != "mixedcase.user@example.com" {
		t.Fatalf("期望查到状态已更新为 completed 的流水, 实际: %+v", leases)
	}
}

func TestClaimPoolAlias(t *testing.T) {
	tempDir := filepath.Join(os.TempDir(), "test_claim_pool_alias")
	_ = os.RemoveAll(tempDir)
	defer os.RemoveAll(tempDir)

	s, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()

	// 1. 模拟 scheduler 预先补货了 2 个别名
	_ = s.RecordLease(LeaseRecord{
		Email:     "pool1@icloud.com",
		AccountID: "acc_1",
		Tag:       "t1",
		TokenName: "scheduler",
	})
	_ = s.RecordLease(LeaseRecord{
		Email:     "pool2@icloud.com",
		AccountID: "acc_1",
		Tag:       "t1",
		TokenName: "scheduler",
	})

	candidates := []PoolCandidate{
		{AccountID: "acc_1", Email: "pool1@icloud.com"},
		{AccountID: "acc_1", Email: "pool2@icloud.com"},
		{AccountID: "acc_1", Email: "fresh_apple_web@icloud.com"},
	}

	// 2. 统计初始可用数 (3 个)
	avail, err := s.CountAvailablePoolAliases(candidates)
	if err != nil || avail != 3 {
		t.Fatalf("CountAvailablePoolAliases 期望 3, 实际: %d (err=%v)", avail, err)
	}

	// 3. 第一次认领：应拿到 pool1
	rec1, err := s.ClaimPoolAlias(candidates, "t1", "bot_alpha")
	if err != nil || rec1 == nil || rec1.Email != "pool1@icloud.com" {
		t.Fatalf("ClaimPoolAlias 1 期望 pool1, 实际: %+v, err: %v", rec1, err)
	}

	// 4. 第二次认领：应拿到 pool2
	rec2, err := s.ClaimPoolAlias(candidates, "t1", "bot_beta")
	if err != nil || rec2 == nil || rec2.Email != "pool2@icloud.com" {
		t.Fatalf("ClaimPoolAlias 2 期望 pool2, 实际: %+v, err: %v", rec2, err)
	}

	// 5. 第三次认领：应拿到 fresh_apple_web
	rec3, err := s.ClaimPoolAlias(candidates, "t1", "bot_gamma")
	if err != nil || rec3 == nil || rec3.Email != "fresh_apple_web@icloud.com" {
		t.Fatalf("ClaimPoolAlias 3 期望 fresh_apple_web, 实际: %+v, err: %v", rec3, err)
	}

	// 6. 第四次认领：池已空，应返回 nil
	rec4, err := s.ClaimPoolAlias(candidates, "t1", "bot_delta")
	if err != nil || rec4 != nil {
		t.Fatalf("ClaimPoolAlias 4 期望 nil, 实际: %+v, err: %v", rec4, err)
	}

	// 7. 再次统计可用数 (0 个)
	availAfter, _ := s.CountAvailablePoolAliases(candidates)
	if availAfter != 0 {
		t.Fatalf("CountAvailablePoolAliases 期望 0, 实际: %d", availAfter)
	}
}

func TestClaimPoolAliasConcurrency(t *testing.T) {
	tempDir := filepath.Join(os.TempDir(), "test_claim_pool_concurrency")
	_ = os.RemoveAll(tempDir)
	defer os.RemoveAll(tempDir)

	s, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()

	const totalAliases = 30
	candidates := make([]PoolCandidate, totalAliases)
	for i := 0; i < totalAliases; i++ {
		candidates[i] = PoolCandidate{
			AccountID: "acc_1",
			Email:     fmt.Sprintf("alias_%d@icloud.com", i),
		}
	}

	// 30 个并发协程抢领 30 个别名，必须恰好人手一个，绝无重复
	const numGoroutines = 30
	claimedMap := sync.Map{}
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(workerID int) {
			defer wg.Done()
			rec, err := s.ClaimPoolAlias(candidates, "test_tag", fmt.Sprintf("bot_%d", workerID))
			if err != nil {
				t.Errorf("worker %d 报错: %v", workerID, err)
				return
			}
			if rec != nil {
				if _, loaded := claimedMap.LoadOrStore(rec.Email, workerID); loaded {
					t.Errorf("别名 %s 被重复认领！", rec.Email)
				}
			}
		}(i)
	}
	wg.Wait()

	count := 0
	claimedMap.Range(func(key, value any) bool {
		count++
		return true
	})
	if count != totalAliases {
		t.Fatalf("并发认领期望领取 %d 个唯一别名，实际仅领取 %d 个", totalAliases, count)
	}

	consumed := s.CountConsumedPoolAliases()
	if consumed != totalAliases {
		t.Fatalf("CountConsumedPoolAliases 期望 %d, 实际: %d", totalAliases, consumed)
	}
}

