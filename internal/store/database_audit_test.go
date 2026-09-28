// Database audit regressions cover WAL restore, mixed timestamp offsets,
// and the distinction between partial and global uniqueness constraints.
package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRestoreIncludesCommittedSourceWAL(t *testing.T) {
	sourceDir := t.TempDir()
	source, err := NewStore(sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	// Flush the schema, then keep the newest committed value only in WAL.
	if _, err := source.DB().Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if err := source.SaveSetting("wal_restore_probe", "committed"); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(sourceDir, "icloud_hme.db")
	if info, err := os.Stat(sourcePath + "-wal"); err != nil || info.Size() <= 32 {
		t.Fatalf("expected committed WAL frames: info=%v err=%v", info, err)
	}
	destination := t.TempDir()
	if err := RestoreDatabase(context.Background(), destination, sourcePath); err != nil {
		t.Fatal(err)
	}
	restored, err := NewStore(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if got, err := restored.GetSetting("wal_restore_probe"); err != nil || got != "committed" {
		t.Fatalf("restore lost committed source WAL data: value=%q err=%v", got, err)
	}
}

func TestLeaseTimesWithMixedOffsets(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, rec := range []LeaseRecord{
		{ID: "older", Email: "same@example.com", AccountID: "old_account", AllocatedAt: "2026-09-28T09:59:59+08:00"},
		{ID: "boundary", Email: "boundary@example.com", AccountID: "boundary_account", AllocatedAt: "2026-09-28T02:00:00Z"},
		{ID: "newer", Email: "same@example.com", AccountID: "new_account", AllocatedAt: "2026-09-28T02:00:01Z"},
	} {
		if err := st.RecordLease(rec); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("list", func(t *testing.T) {
		rows, _, err := st.ListLeases("", "", "", 10, 0)
		if err != nil || len(rows) != 3 || rows[0].ID != "newer" || rows[2].ID != "older" {
			t.Fatalf("leases not ordered by actual time: rows=%+v err=%v", rows, err)
		}
	})
	t.Run("latest account", func(t *testing.T) {
		if id, ok := st.FindLeaseAccount("same@example.com"); !ok || id != "new_account" {
			t.Fatalf("latest lease account=%q found=%v", id, ok)
		}
	})
	t.Run("prune", func(t *testing.T) {
		cutoff, err := time.Parse(time.RFC3339, "2026-09-28T10:00:00+08:00")
		if err != nil {
			t.Fatal(err)
		}
		if n, err := st.PruneLeases(cutoff, 100); err != nil || n != 1 {
			t.Fatalf("prune must delete only the older record: deleted=%d err=%v", n, err)
		}
		var retained int
		if err := st.DB().QueryRow(`SELECT COUNT(*) FROM lease_records WHERE id IN ('boundary', 'newer')`).Scan(&retained); err != nil || retained != 2 {
			t.Fatalf("unexpired records lost: retained=%d err=%v", retained, err)
		}
	})
}

func TestPartialUniqueIndexDoesNotSatisfySchemaContract(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "schema.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE operations (principal_kind TEXT, principal_id TEXT, operation_kind TEXT, idempotency_key TEXT, state TEXT);
		CREATE UNIQUE INDEX partial_idempotency ON operations (principal_kind, principal_id, operation_kind, idempotency_key) WHERE state = 'succeeded';
		INSERT INTO operations VALUES ('token', 'one', 'allocate', 'same', 'pending');
		INSERT INTO operations VALUES ('token', 'one', 'allocate', 'same', 'pending');
	`); err != nil {
		t.Fatal(err)
	}
	columns := []string{"principal_kind", "principal_id", "operation_kind", "idempotency_key"}
	if has, err := hasUniqueConstraint(db, "operations", columns); err != nil || has {
		t.Fatalf("partial index accepted as global uniqueness: has=%v err=%v", has, err)
	}
}
