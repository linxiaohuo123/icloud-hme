// [INPUT]: Temporary SQLite with historical non-normalized aliases.
// [OUTPUT]: UIDVALIDITY batch invalidation regression.
// [POS]: internal/store normalized verification boundaries.
// [PROTOCOL]: Update this header and CLAUDE.md when changing this file.
package store

import (
	"context"
	"testing"
	"time"
)

func TestGenerationInvalidationNormalizesStoredAlias(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	err = st.CreateVerificationRequest(ctx, &VerificationRequest{RequestID: "mixed-alias", PrincipalKind: "token", PrincipalID: "owner", LeaseID: "lease", AliasEmail: "  Target@iCloud.com  ", Status: "ready", CreatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339), BaselineProvider: "imap", BaselineMailbox: "INBOX", BaselineUIDValidity: 7, BaselineUID: 100})
	if err != nil {
		t.Fatal(err)
	}
	n, err := st.InvalidateVerificationRequestsForGenerationMismatchBatch(ctx, []string{"target@icloud.com"}, "INBOX", 8)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetVerificationRequest(ctx, "mixed-alias", "token", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || got.Status != "invalidated" {
		t.Fatalf("UIDVALIDITY 7 -> 8 was ignored: affected=%d status=%s", n, got.Status)
	}
}
