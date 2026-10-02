// [POS]: 临时 SQLite 配额按原预留窗口释放回归
// [PROTOCOL]: 变更时检查 CLAUDE.md
package store

import "testing"

func TestQuotaReleaseIsBoundToReservationWindow(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	allowed, _, window, err := s.TryReserveQuota("acc", 3)
	if err != nil || !allowed {
		t.Fatalf("reserve: %v %v", allowed, err)
	}
	// Simulate an earlier reservation still running across the hour boundary.
	if err := s.ReleaseQuota("acc", 1, window-1); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.GetScheduleConfig("acc")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentHourCount != 3 {
		t.Fatalf("old reservation released new window: %+v", cfg)
	}
	if err := s.ReleaseQuota("acc", 2, window); err != nil {
		t.Fatal(err)
	}
	cfg, err = s.GetScheduleConfig("acc")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentHourCount != 1 {
		t.Fatalf("partial batch release: %+v", cfg)
	}
	if err := s.ReleaseQuota("acc", 1, window); err != nil {
		t.Fatal(err)
	}
	cfg, err = s.GetScheduleConfig("acc")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentHourCount != 0 {
		t.Fatalf("single release: %+v", cfg)
	}
	if err := s.ReleaseQuota("missing", 1, window); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schedules WHERE account_id = 'missing'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("release created a schedule")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseQuota("acc", 1, window); err == nil {
		t.Fatal("closed DB release silently succeeded")
	}
}
