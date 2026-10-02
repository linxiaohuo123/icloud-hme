// [INPUT]: SQLite driver and real production completion methods.
// [OUTPUT]: Deterministic concurrent-writer regression without fabricated DB errors.
// [POS]: internal/store WAL snapshot-upgrade regression.
// [PROTOCOL]: Update this header and CLAUDE.md when changing the test.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"
)

// Schedule a real concurrent CAS after a read snapshot (legacy) or before the first write.
// The real SQLite driver and both production completion methods are unchanged.
type completionScheduleDriver struct {
	base   driver.Driver
	onRead func()

	once sync.Once
}

func (d *completionScheduleDriver) Open(name string) (driver.Conn, error) {
	c, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &completionScheduleConn{Conn: c, owner: d}, nil
}

type completionScheduleConn struct {
	driver.Conn
	owner *completionScheduleDriver
}

func (c *completionScheduleConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(q, "RETURNING request_id") && strings.Contains(q, "LOWER(TRIM(alias_email))") {
		c.owner.once.Do(c.owner.onRead)
	}
	r, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
	if err != nil {
		return nil, err
	}
	if strings.Contains(q, "SELECT request_id\n") && strings.Contains(q, "FROM verification_requests") {
		return &completionScheduleRows{Rows: r, owner: c.owner}, nil
	}
	return r, nil
}
func (c *completionScheduleConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
}

type completionScheduleRows struct {
	driver.Rows
	owner *completionScheduleDriver
}

func (r *completionScheduleRows) Close() error {
	err := r.Rows.Close()
	r.owner.once.Do(r.owner.onRead)
	return err
}

func TestCompletionSurvivesConcurrentResultWriter(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	originalDB := st.db
	defer originalDB.Close()
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	now := time.Now().UTC()
	for _, id := range []string{"target", "other"} {
		err := st.CreateVerificationRequest(ctx, &VerificationRequest{
			RequestID: id, PrincipalKind: "token", PrincipalID: "probe-owner",
			LeaseID: "probe-lease-" + id, AliasEmail: id + "@invalid.example", Status: "ready",
			CreatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339),
			BaselineProvider: "imap", BaselineMailbox: "INBOX", BaselineUIDValidity: 1, BaselineUID: 10,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var otherErr error
	var otherWon bool
	d := &completionScheduleDriver{base: &sqlite.Driver{}}
	d.onRead = func() {
		_, otherWon, otherErr = st.CompleteVerificationRequestResult(ctx, "other", VerificationCompletion{Code: "654321", MatchedEventRef: "probe-other"})
	}
	name := fmt.Sprintf("round4_snapshot_%d", time.Now().UnixNano())
	sql.Register(name, d)
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)", filepath.ToSlash(filepath.Join(dir, "icloud_hme.db")))
	wrappedDB, err := sql.Open(name, dsn)
	if err != nil {
		t.Fatal(err)
	}
	wrappedDB.SetMaxOpenConns(10)
	st.db = wrappedDB
	start := time.Now()
	completed, completeErr := st.CompleteMatchingVerificationRequests(ctx, VerificationEventInput{
		AliasEmail: "target@invalid.example", Provider: "imap", Mailbox: "INBOX", UIDValidity: 1, UID: 20,
		MessageRef: "probe-target", Code: "123456", Now: now,
	})
	target, readErr := st.GetVerificationRequest(ctx, "target", "token", "probe-owner")
	if readErr != nil {
		t.Fatal(readErr)
	}
	var sqliteErr *sqlite.Error
	code := 0
	if e, ok := completeErr.(*sqlite.Error); ok {
		sqliteErr = e
		code = sqliteErr.Code()
	}
	t.Logf("otherWon=%v otherErr=%v completionErr=%v sqliteCode=%d completed=%d targetStatus=%s elapsed=%v", otherWon, otherErr, completeErr, code, len(completed), target.Status, time.Since(start))
	if !otherWon || otherErr != nil {
		t.Fatalf("concurrent production CAS must commit successfully: won=%v err=%v", otherWon, otherErr)
	}
	if completeErr != nil || target.Status != "succeeded" {
		t.Fatalf("background completion failed after a valid concurrent HTTP result writer: err=%v target=%s", completeErr, target.Status)
	}
}
