// [INPUT]: Real SQLite status CAS and failure-injecting SQLite triggers.
// [OUTPUT]: Expiry failures remain errors; a concurrent terminal winner stays authoritative.
// [POS]: internal/store verification completion expiry regression.
// [PROTOCOL]: Update this header and CLAUDE.md when changing this test.
package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func expiredCompletionRequest(t *testing.T, st *Store) (*VerificationRequest, time.Time) {
	t.Helper()
	now := time.Now().UTC()
	req := &VerificationRequest{
		RequestID: "expired-completion", PrincipalKind: "token", PrincipalID: "expiry-owner",
		LeaseID: "expiry-lease", AliasEmail: "expiry@invalid.example", Status: "ready",
		CreatedAt: now.Add(-time.Minute).Format(time.RFC3339),
		ExpiresAt: now.Add(-time.Second).Format(time.RFC3339),
	}
	if err := st.CreateVerificationRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	return req, now
}

func TestCompletion_ExpiredCASFailureIsReturned(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	req, now := expiredCompletionRequest(t, st)
	if _, err := st.DB().Exec(`CREATE TRIGGER reject_completion_expiry
		BEFORE UPDATE OF status ON verification_requests WHEN NEW.status = 'expired'
		BEGIN SELECT RAISE(ABORT, 'forced expiry failure'); END`); err != nil {
		t.Fatal(err)
	}

	result, won, err := st.CompleteVerificationRequestResult(context.Background(), req.RequestID,
		VerificationCompletion{Code: "123456", MatchedEventRef: "late-event"}, now)
	if err == nil || !strings.Contains(err.Error(), "forced expiry failure") || won || result != nil {
		t.Fatalf("failed expiry must be explicit: result=%+v won=%v err=%v", result, won, err)
	}
	durable, err := st.GetVerificationRequest(context.Background(), req.RequestID, req.PrincipalKind, req.PrincipalID)
	if err != nil || durable.Status != "ready" || durable.Code != "" {
		t.Fatalf("failed expiry changed durable state: result=%+v err=%v", durable, err)
	}
	if _, err := st.DB().Exec(`DROP TRIGGER reject_completion_expiry`); err != nil {
		t.Fatal(err)
	}
	result, won, err = st.CompleteVerificationRequestResult(context.Background(), req.RequestID,
		VerificationCompletion{Code: "123456", MatchedEventRef: "late-event"}, now)
	if err != nil || won || result == nil || result.Status != "expired" || result.Code != "" {
		t.Fatalf("expiry did not recover: result=%+v won=%v err=%v", result, won, err)
	}
}

func TestCompletion_ExpiredCASReturnsTerminalWinner(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	req, now := expiredCompletionRequest(t, st)
	// Schedule a committed completion winner during the expiry CAS itself.
	if _, err := st.DB().Exec(`CREATE TRIGGER completion_wins_expiry
		BEFORE UPDATE OF status ON verification_requests WHEN NEW.status = 'expired'
		BEGIN
			UPDATE verification_requests SET status = 'succeeded', code = '654321',
				magic_link = 'https://example.invalid/winner', matched_event_ref = 'winner-event'
			WHERE request_id = OLD.request_id;
			SELECT RAISE(IGNORE);
		END`); err != nil {
		t.Fatal(err)
	}
	result, won, err := st.CompleteVerificationRequestResult(context.Background(), req.RequestID,
		VerificationCompletion{Code: "123456", MatchedEventRef: "late-event"}, now)
	if err != nil || won || result == nil || result.Status != "succeeded" ||
		result.Code != "654321" || result.MatchedEventRef != "winner-event" || result.MagicLink != "https://example.invalid/winner" {
		t.Fatalf("expiry CAS lost its authoritative winner: result=%+v won=%v err=%v", result, won, err)
	}
}
