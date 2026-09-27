package store

import (
	"sync"
	"testing"
)

func TestStoreReadFailuresAreNotEmptyResults(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	checks := map[string]func() error{
		"schedule":        func() error { _, err := st.GetScheduleConfig("acc_1"); return err },
		"schedules":       func() error { _, err := st.ListScheduleConfigs(); return err },
		"remaining quota": func() error { _, err := st.RemainingQuota("acc_1"); return err },
		"reserve quota":   func() error { _, _, err := st.TryReserveQuota("acc_1", 1); return err },
		"setting":         func() error { _, err := st.GetSetting("key"); return err },
		"tags":            func() error { _, err := st.ListTags(); return err },
		"tokens":          func() error { _, err := st.ListTokens(); return err },
	}
	for name, check := range checks {
		if err := check(); err == nil {
			t.Errorf("%s read silently succeeded after database close", name)
		}
	}
}

func TestConcurrentScheduleUpdatesPreserveIndependentFields(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveScheduleConfig(ScheduleConfig{AccountID: "acc_1", HourlyQuota: 5}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := st.UpdateScheduleConfig("acc_1", func(cfg *ScheduleConfig) { cfg.HourlyQuota = 50 }); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := st.UpdateScheduleConfig("acc_1", func(cfg *ScheduleConfig) { cfg.AliasLabel = "parallel" }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	cfg, err := st.GetScheduleConfig("acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HourlyQuota != 50 || cfg.AliasLabel != "parallel" {
		t.Fatalf("concurrent updates lost a field: %+v", cfg)
	}
}
