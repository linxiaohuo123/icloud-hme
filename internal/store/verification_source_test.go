// [INPUT]: Temporary SQLite, source-aware completion CAS and migration hooks.
// [OUTPUT]: Source isolation and v12 transactional migration regression.
// [POS]: internal/store persistent mailbox identity.
// [PROTOCOL]: Update this header and CLAUDE.md when changing this file.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestVerificationSourceCompletionCAS(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	req := &VerificationRequest{RequestID: "source-cas", PrincipalKind: "admin", PrincipalID: "admin", LeaseID: "source-lease", AliasEmail: "target@icloud.com", Status: "ready", CreatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339), BaselineProvider: "imap", BaselineMailbox: "INBOX", BaselineUIDValidity: 1, BaselineUID: 100, BaselineSource: "new", BaselineAccountID: "acc"}
	if err := st.CreateVerificationRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	result, won, err := st.CompleteVerificationRequestResult(ctx, req.RequestID, VerificationCompletion{Source: "old", Code: "111111"}, now)
	if err != nil || won || result.Status != "ready" {
		t.Fatalf("single CAS accepted old source: %+v %v %v", result, won, err)
	}
	ev := VerificationEventInput{Source: "old", AliasEmail: req.AliasEmail, Provider: "imap", Mailbox: "INBOX", UIDValidity: 1, UID: 150, Code: "111111"}
	matched, err := st.CompleteMatchingVerificationRequests(ctx, ev)
	if err != nil || len(matched) != 0 {
		t.Fatalf("batch CAS accepted old source: %+v %v", matched, err)
	}
	if _, err := st.InvalidateVerificationRequestsForGenerationMismatchBatch(ctx, []string{req.AliasEmail}, "INBOX", 2, "old"); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetVerificationRequest(ctx, req.RequestID, "admin", "admin")
	if err != nil || got.Status != "ready" {
		t.Fatalf("old source invalidated new generation: %+v %v", got, err)
	}
	ev.Source = "new"
	ev.Code = "222222"
	matched, err = st.CompleteMatchingVerificationRequests(ctx, ev)
	if err != nil || len(matched) != 1 || matched[0].Code != "222222" || matched[0].BaselineSource != "new" {
		t.Fatalf("new source not completed: %+v %v", matched, err)
	}
}

func TestV11VerificationSourceUpgrade(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("rollback=%v", fail), func(t *testing.T) {
			dir := t.TempDir()
			st, err := NewStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, status := range []string{"pending", "ready", "succeeded", "expired", "invalidated"} {
				now := time.Now().UTC()
				req := &VerificationRequest{RequestID: status, PrincipalKind: "admin", PrincipalID: "admin", LeaseID: status, AliasEmail: status + "@icloud.com", Status: status, CreatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339), Code: "123456", MatchedEventRef: "saved-result"}
				if err := st.CreateVerificationRequest(context.Background(), req); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := st.DB().Exec(`ALTER TABLE verification_requests DROP COLUMN baseline_source; ALTER TABLE verification_requests DROP COLUMN baseline_account_id; PRAGMA user_version=11`); err != nil {
				t.Fatal(err)
			}
			if err := validateSchemaVersion(st.DB(), 11); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			if fail {
				SetBeforeMigrationStepHookForTest(func(step string) error {
					if step == "v12_after_verification_sources" {
						return errors.New("injected source migration failure")
					}
					return nil
				})
				unexpected, err := NewStore(dir)
				SetBeforeMigrationStepHookForTest(nil)
				if err == nil {
					unexpected.Close()
					t.Fatal("expected failure")
				}
				db, err := sql.Open("sqlite", filepath.Join(dir, "icloud_hme.db"))
				if err != nil {
					t.Fatal(err)
				}
				if err := validateSchemaVersion(db, 11); err != nil {
					t.Fatal(err)
				}
				var count int
				if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('verification_requests') WHERE name IN ('baseline_source','baseline_account_id')`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("columns survived rollback: %d %v", count, err)
				}
				var status string
				if err := db.QueryRow(`SELECT status FROM verification_requests WHERE request_id='ready'`).Scan(&status); err != nil || status != "ready" {
					t.Fatalf("rollback changed task: %s %v", status, err)
				}
				db.Close()
			}
			for range 2 {
				upgraded, err := NewStore(dir)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { upgraded.Close() })
				for _, status := range []string{"pending", "ready", "succeeded", "expired", "invalidated"} {
					got, err := upgraded.GetVerificationRequest(context.Background(), status, "admin", "admin")
					expected := status
					if status == "pending" || status == "ready" {
						expected = "invalidated"
					}
					if err != nil || got.Status != expected || got.Code != "123456" || got.MatchedEventRef != "saved-result" {
						t.Fatalf("migration damaged %s: %+v %v", status, got, err)
					}
				}
				upgraded.Close()
			}
			backups, err := filepath.Glob(filepath.Join(dir, "backups", fmt.Sprintf("pre-migrate-v11-to-v%d-*.db", CurrentSchemaVersion)))
			if err != nil || len(backups) == 0 {
				t.Fatalf("missing backup: %v %v", backups, err)
			}
		})
	}
}
