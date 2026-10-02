package store

import (
	"context"
	"testing"
	"time"
)

// TestExpiredReplayReturnsCASWinner 验证到期重放时 CAS 输给并发 succeeded 能够正确返回权威成功终态 (S01)
func TestExpiredReplayReturnsCASWinner(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	req := &VerificationRequest{
		RequestID:       "rereview-cas",
		PrincipalKind:   "token",
		PrincipalID:     "rereview-owner",
		LeaseID:         "rereview-lease",
		AliasEmail:      "rereview@invalid.example",
		Status:          "ready",
		CreatedAt:       time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		ExpiresAt:       time.Now().Add(-time.Second).UTC().Format(time.RFC3339),
		IdempotencyKey:  "rereview-key",
		IdempotencyHash: "rereview-hash",
	}
	if err := st.CreateVerificationRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	// 触发器确定性注入在读取重放快照后由并发完成胜出的 CAS 终态
	if _, err := st.DB().Exec("CREATE TRIGGER rereview_success_wins BEFORE UPDATE OF status ON verification_requests WHEN OLD.request_id='rereview-cas' AND NEW.status='expired' BEGIN UPDATE verification_requests SET status='succeeded', code='123456' WHERE request_id=OLD.request_id; SELECT RAISE(IGNORE); END"); err != nil {
		t.Fatal(err)
	}
	replay, err := st.GetVerificationRequestByIdempotencyKey(context.Background(), req.PrincipalKind, req.PrincipalID, req.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	durable, err := st.GetVerificationRequest(context.Background(), req.RequestID, req.PrincipalKind, req.PrincipalID)
	if err != nil {
		t.Fatal(err)
	}
	if durable.Status != "succeeded" {
		t.Fatalf("failure injection did not install the expected CAS winner: %+v", durable)
	}
	if replay.Status != durable.Status || replay.Code != durable.Code {
		t.Fatalf("replay reports status=%s code=%q while CAS winner is status=%s code=%q", replay.Status, replay.Code, durable.Status, durable.Code)
	}
}
