package store

import "testing"

func TestV9Upgrade(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"DROP INDEX uidx_vreq_idempotency",
		"DROP INDEX idx_vreq_status_exp",
		"ALTER TABLE verification_requests DROP COLUMN idempotency_key",
		"ALTER TABLE verification_requests DROP COLUMN idempotency_hash",
		"PRAGMA user_version = 9",
	} {
		if _, err := s.DB().Exec(q); err != nil {
			s.Close()
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := NewStore(dir)
	if upgraded != nil {
		defer upgraded.Close()
	}
	if err != nil {
		t.Fatalf("historical v9 database cannot start after upgrade: %v", err)
	}
}

func TestV10SelfRepairMissingIndex(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟升级到 10 的异常遗留状态：version=10 但被删除了索引
	for _, q := range []string{
		"DROP INDEX IF EXISTS uidx_vreq_idempotency",
		"DROP INDEX IF EXISTS idx_vreq_status_exp",
		"PRAGMA user_version = 10",
	} {
		if _, err := s.DB().Exec(q); err != nil {
			s.Close()
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	repaired, err := NewStore(dir)
	if err != nil {
		t.Fatalf("version 10 database missing index failed to self-repair on startup: %v", err)
	}
	defer repaired.Close()

	// 验证索引已正确自愈补齐
	var count int
	if err := repaired.DB().QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_vreq_status_exp'").Scan(&count); err != nil || count == 0 {
		t.Fatal("idx_vreq_status_exp was not self-repaired")
	}
	if err := repaired.DB().QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='uidx_vreq_idempotency'").Scan(&count); err != nil || count == 0 {
		t.Fatal("uidx_vreq_idempotency was not self-repaired")
	}
}

func TestRepeatedStartup(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	// 连续启动两次，确保启动自愈与校验幂等稳定
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("second startup failed: %v", err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}

	s3, err := NewStore(dir)
	if err != nil {
		t.Fatalf("third startup failed: %v", err)
	}
	s3.Close()
}
