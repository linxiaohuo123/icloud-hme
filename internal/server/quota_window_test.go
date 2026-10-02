// [POS]: 真实账号锁与取消路径的单条/批量跨窗口配额回归
// [PROTOCOL]: 变更时检查 CLAUDE.md
package server

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/store"
)

func TestBackendFailedCreatesKeepNewWindowQuota(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "batch"}[batch], func(t *testing.T) {
			dir := t.TempDir()
			st, err := store.NewStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			mgr, err := account.NewManager(t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer mgr.Close()
			sum, err := mgr.AddAccountWithInput(account.AddAccountInput{Name: "quota", ICloudEmail: "quota@icloud.com"})
			if err != nil {
				t.Fatal(err)
			}
			if err := mgr.SaveSession(sum.ID, map[string]string{"X-APPLE-WEBAUTH-USER": "fixture-cookie"}, "http://127.0.0.1:1"); err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{})
			release := make(chan struct{})
			heldDone := make(chan error, 1)
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			go func() {
				heldDone <- mgr.WithHMEClient(sum.ID, func(c *hme.Client) error {
					c.SetFixedServiceURL("http://127.0.0.1:1")
					close(entered)
					<-release
					return nil
				})
			}()
			defer func() { unblock(); <-heldDone }()
			select {
			case <-entered:
			case err := <-heldDone:
				heldDone <- err
				t.Fatalf("fixture client: %v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("fixture lock not acquired")
			}
			db, err := sql.Open("sqlite", filepath.ToSlash(filepath.Join(dir, "icloud_hme.db")))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			be := &managerBackend{mgr: mgr, store: st}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			requested := 1
			if batch {
				requested = 3
			}
			go func() {
				var err error
				if batch {
					_, err = be.BatchCreateAliasContext(ctx, sum.ID, requested, "test")
				} else {
					_, err = be.CreateAliasContext(ctx, sum.ID, "test")
				}
				done <- err
			}()
			var originalWindow int64
			for {
				cfg, err := st.GetScheduleConfig(sum.ID)
				if err != nil {
					t.Fatal(err)
				}
				if cfg.CurrentHourCount == requested {
					originalWindow = cfg.LastHourWindow
					break
				}
				select {
				case err := <-done:
					t.Fatalf("create failed before quota reserved: %v", err)
				case <-ctx.Done():
					t.Fatal("quota not reserved")
				case <-time.After(time.Millisecond):
				}
			}
			// Equivalent to a second request installing next-hour reservations
			// while the first request waits on the real account mutex.
			newWindow := originalWindow + 1
			if _, err := db.Exec(`UPDATE schedules SET last_hour_window = ?, current_hour_count = 2 WHERE account_id = ?`, newWindow, sum.ID); err != nil {
				t.Fatal(err)
			}
			cancel()
			if err := <-done; err == nil {
				t.Fatal("canceled create succeeded")
			}
			var count int
			var window int64
			if err := db.QueryRow(`SELECT current_hour_count, last_hour_window FROM schedules WHERE account_id = ?`, sum.ID).Scan(&count, &window); err != nil {
				t.Fatal(err)
			}
			if window != newWindow || count != 2 {
				t.Fatalf("old request released new quota: window=%d count=%d", window, count)
			}
		})
	}
}
