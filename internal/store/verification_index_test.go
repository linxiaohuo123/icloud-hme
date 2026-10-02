// [INPUT]: Real SQLite query plans, historical v10 fixtures and transactional migration hooks.
// [OUTPUT]: Indexed verification hot paths, mixed-case baseline correctness and upgrade/rollback regression.
// [POS]: internal/store verification dispatch and admission index validation.
// [PROTOCOL]: Update this header and CLAUDE.md when changing this file.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVerificationRequestHotQueryPlans(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, tc := range []struct{ name, query, index string }{
		{"completion", `UPDATE verification_requests SET status='succeeded',code='123456'
			WHERE status IN ('ready','pending') AND (expires_at IS NULL OR expires_at > '2026-10-02T00:00:00Z')
			AND LOWER(TRIM(alias_email))='target@icloud.com' AND baseline_provider='imap'
			AND (baseline_mailbox='INBOX' OR baseline_mailbox='') AND baseline_uidvalidity=7 AND baseline_uid<=100`, "idx_vreq_email_normalized"},
		{"principal", `SELECT count(*) FROM verification_requests WHERE principal_kind='token' AND principal_id='owner'
			AND status IN ('ready','pending') AND (expires_at IS NULL OR expires_at > '2026-10-02T00:00:00Z')`, "COVERING INDEX idx_vreq_principal"},
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
			if !strings.Contains(got, tc.index) || strings.Contains(got, "SCAN verification_requests") {
				t.Fatalf("hot path needs indexed lookup via %s:\n%s", tc.index, got)
			}
			t.Log(got)
		})
	}
}

func TestVerificationBatchBaselinePreservesNormalizedAliases(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	if err := st.CreateVerificationRequest(context.Background(), &VerificationRequest{
		RequestID: "normalized-baseline", PrincipalKind: "token", PrincipalID: "owner", LeaseID: "baseline-lease",
		AliasEmail: "  Target@iCloud.com  ", Status: "ready", CreatedAt: now.Format(time.RFC3339),
		ExpiresAt: now.Add(time.Minute).Format(time.RFC3339), BaselineProvider: "imap",
		BaselineMailbox: "INBOX", BaselineUIDValidity: 7, BaselineUID: 100,
	}); err != nil {
		t.Fatal(err)
	}
	baselines, err := st.GetMinBaselineUIDsByEmails(context.Background(), []string{"target@icloud.com"})
	if err != nil || baselines["target@icloud.com"] != 100 {
		t.Fatalf("batch lookup lost the persisted baseline: baselines=%v err=%v", baselines, err)
	}
}

func TestV10VerificationIndexUpgrade(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("rollback=%v", fail), func(t *testing.T) {
			dir := t.TempDir()
			st, err := NewStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			req, _ := expiredCompletionRequest(t, st)
			// This fixture downgrades current schema solely to exercise index migration.
			if _, err := st.DB().Exec(`UPDATE verification_requests SET baseline_source='known-inbox', baseline_account_id='acc' WHERE request_id=?`, req.RequestID); err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(`DROP INDEX IF EXISTS idx_vreq_email_normalized;
				DROP INDEX idx_vreq_principal;
				CREATE INDEX idx_vreq_principal ON verification_requests(principal_kind,principal_id);
				PRAGMA user_version=10;`); err != nil {
				t.Fatal(err)
			}
			if err := validateSchemaVersion(st.DB(), 10); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			if fail {
				SetBeforeMigrationStepHookForTest(func(step string) error {
					if step == "v11_after_verification_indexes" {
						return errors.New("injected index migration failure")
					}
					return nil
				})
				t.Cleanup(func() { SetBeforeMigrationStepHookForTest(nil) })
				unexpected, err := NewStore(dir)
				SetBeforeMigrationStepHookForTest(nil)
				if err == nil {
					unexpected.Close()
					t.Fatal("expected migration failure")
				}
				db, err := sql.Open("sqlite", filepath.Join(dir, "icloud_hme.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if v, err := getUserVersion(db); err != nil || v != 10 {
					t.Fatalf("failed migration advanced version: version=%d err=%v", v, err)
				}
				var count int
				if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='idx_vreq_email_normalized'`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("new index survived rollback: count=%d err=%v", count, err)
				}
				var oldSQL string
				if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='idx_vreq_principal'`).Scan(&oldSQL); err != nil || strings.Contains(oldSQL, "expires_at") {
					t.Fatalf("old principal index was not restored: sql=%s err=%v", oldSQL, err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				upgraded, err := NewStore(dir)
				if err != nil {
					t.Fatal(err)
				}
				got, err := upgraded.GetVerificationRequest(context.Background(), req.RequestID, req.PrincipalKind, req.PrincipalID)
				if err != nil || got.Status != req.Status || got.AliasEmail != req.AliasEmail || got.ExpiresAt != req.ExpiresAt {
					t.Fatalf("index upgrade changed verification data: request=%+v err=%v", got, err)
				}
				if err := upgraded.Close(); err != nil {
					t.Fatal(err)
				}
			}
			backups, err := filepath.Glob(filepath.Join(dir, "backups", fmt.Sprintf("pre-migrate-v10-to-v%d-*.db", CurrentSchemaVersion)))
			if err != nil || len(backups) == 0 {
				t.Fatalf("upgrade did not preserve a backup: files=%v err=%v", backups, err)
			}
		})
	}
}
