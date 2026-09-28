package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestLeaseTimeQueryPlans(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, tc := range []struct{ name, query, index string }{
		{"list", `SELECT id, email, account_id, tag, status, allocated_at, COALESCE(completed_at, ''), COALESCE(token_name, '') FROM lease_records WHERE 1=1 ORDER BY julianday(allocated_at) DESC, id DESC LIMIT 50 OFFSET 0`, "idx_leases_allocated_time"},
		{"account", `SELECT account_id FROM lease_records WHERE LOWER(email) = 'same@example.com' AND account_id != '' ORDER BY julianday(allocated_at) DESC, id DESC LIMIT 1`, "idx_leases_email_time"},
		{"prune", `DELETE FROM lease_records WHERE id IN (SELECT id FROM lease_records WHERE julianday(allocated_at) < julianday('2026-09-28T00:00:00Z') ORDER BY julianday(allocated_at) ASC, id ASC LIMIT 100)`, "idx_leases_allocated_time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := st.DB().Query("EXPLAIN QUERY PLAN " + tc.query)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var plan []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plan = append(plan, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			got := strings.Join(plan, "\n")
			if !strings.Contains(got, tc.index) || strings.Contains(got, "USE TEMP B-TREE") {
				t.Fatalf("query should use %s without temporary sorting:\n%s", tc.index, got)
			}
			t.Log(got)
		})
	}
}

func TestV5LeaseIndexUpgrade(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("migration_failure=%v", fail), func(t *testing.T) {
			dir := t.TempDir()
			st, err := NewStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if err := st.RecordLease(LeaseRecord{ID: "preserved", Email: "same@example.com", AccountID: "account", AllocatedAt: "2026-09-28T10:00:00+08:00"}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(`DROP INDEX IF EXISTS idx_leases_allocated_time; DROP INDEX IF EXISTS idx_leases_email_time; PRAGMA user_version = 5;`); err != nil {
				t.Fatal(err)
			}
			if err := validateSchemaVersion(st.DB(), 5); err != nil {
				t.Fatalf("fixture is not a valid v5 database: %v", err)
			}
			if fail {
				// Fail the second CREATE INDEX after the first has succeeded.
				if _, err := st.DB().Exec(`CREATE TABLE idx_leases_email_time (dummy TEXT)`); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			if fail {
				unexpected, err := NewStore(dir)
				if err == nil {
					unexpected.Close()
					t.Fatal("expected index migration to fail")
				}
				db, err := sql.Open("sqlite", filepath.Join(dir, "icloud_hme.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if version, err := getUserVersion(db); err != nil || version != 5 {
					t.Fatalf("failed migration advanced version: version=%d err=%v", version, err)
				}
				var count int
				if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_leases_allocated_time'`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("first index was not rolled back: count=%d err=%v", count, err)
				}
				if _, err := db.Exec(`DROP TABLE idx_leases_email_time`); err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			for attempt := 0; attempt < 2; attempt++ {
				upgraded, err := NewStore(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer upgraded.Close()
				if version, err := getUserVersion(upgraded.DB()); err != nil || version != CurrentSchemaVersion {
					t.Fatalf("version=%d err=%v", version, err)
				}
				records, total, err := upgraded.ListLeases("", "", "", 10, 0)
				if err != nil || total != 1 || len(records) != 1 || records[0].ID != "preserved" || records[0].AllocatedAt != "2026-09-28T10:00:00+08:00" {
					t.Fatalf("migration changed lease: records=%+v total=%d err=%v", records, total, err)
				}
				if err := upgraded.Close(); err != nil {
					t.Fatal(err)
				}
			}
			backups, err := filepath.Glob(filepath.Join(dir, "backups", fmt.Sprintf("pre-migrate-v5-to-v%d-*.db", CurrentSchemaVersion)))
			want := 1
			if fail {
				want = 2
			}
			if err != nil || len(backups) != want {
				t.Fatalf("expected %d migration backups, got %v: %v", want, backups, err)
			}
		})
	}
}
