package mail

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

func fullFetchRecipientTestClient(t *testing.T, raw string) *Client {
	t.Helper()
	return fullFetchBodyTestClient(t, func() string { return raw })
}

func fullFetchBodyTestClient(t *testing.T, nextBody func() string) *Client {
	t.Helper()
	port, stop := spinMockIMAPServer(t, func(tag, cmd string) []string {
		switch cmd {
		case "SELECT", "EXAMINE":
			return []string{"* 1 EXISTS", "* OK [UIDVALIDITY 1] generation", "* OK [UIDNEXT 106] next", tag + " OK selected"}
		case "UID":
			raw := nextBody()
			return []string{fmt.Sprintf("* 1 FETCH (UID 105 ENVELOPE (\"Mon, 28 Sep 2026 01:00:00 +0000\" \"Login\" NIL NIL NIL ((NIL NIL \"forwarded\" \"example.com\")) NIL NIL NIL NIL) BODY[] {%d}\r\n%s)", len(raw), raw), tag + " OK fetched"}
		default:
			return []string{tag + " OK done"}
		}
	})
	t.Cleanup(stop)
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	imapClient, err := client.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	imapClient.Timeout = 2 * time.Second
	if err := imapClient.Login("review", "test-only"); err != nil {
		t.Fatal(err)
	}
	return NewClientForTesting("review@example.com", "test-only", conn, imapClient)
}

func TestFullFetchRejectsMalformedBodyAndReusesConnection(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, tc := range []struct{ name, raw string }{
			{"headers", "invalid header without colon\r\n\r\nYour verification code is 654321\r\n"},
			{"multipart", "Content-Type: multipart/mixed; boundary=missing\r\n\r\nnot a multipart body"},
		} {
			t.Run(fmt.Sprintf("batch=%v/%s", batch, tc.name), func(t *testing.T) {
				calls := 0
				c := fullFetchBodyTestClient(t, func() string {
					calls++
					if calls == 1 {
						return tc.raw
					}
					return "Content-Type: text/plain\r\n\r\nYour verification code is 654321\r\n"
				})
				fetch := func() (*FullMessage, error) {
					if !batch {
						return c.GetFullInFolderWithValidity("INBOX", 1, 105)
					}
					msgs, err := c.GetFullBatchInFolderWithValidity("INBOX", 1, []uint32{105})
					if err != nil {
						if len(msgs) != 0 {
							t.Fatal("failed batch returned partial success")
						}
						return nil, err
					}
					if len(msgs) != 1 {
						t.Fatalf("fetch count=%d", len(msgs))
					}
					return msgs[0], nil
				}
				if full, err := fetch(); err == nil || full != nil {
					t.Fatalf("malformed message returned as success: full=%+v err=%v", full, err)
				} else if strings.Contains(err.Error(), "invalid header without colon") {
					t.Fatal("parse error exposed the raw message header")
				}
				full, err := fetch()
				if err != nil || full == nil || !full.BodyComplete || !strings.Contains(full.Body, "654321") {
					t.Fatalf("connection unusable after failed parse: full=%+v err=%v", full, err)
				}
			})
		}
	}
}

func TestFullFetchMissingVersusEmptyBody(t *testing.T) {
	section := &imap.BodySectionName{}
	full := &FullMessage{}
	if err := decodeFullMessageBody(full, &imap.Message{Uid: 105}, section); err == nil || full.BodyComplete {
		t.Fatalf("missing body marked complete: err=%v", err)
	}
	msg := &imap.Message{Uid: 105, Body: map[*imap.BodySectionName]imap.Literal{
		section: strings.NewReader("Content-Type: text/plain\r\n\r\n"),
	}}
	if err := decodeFullMessageBody(full, msg, section); err != nil || !full.BodyComplete || full.Body != "" {
		t.Fatalf("valid empty body rejected: full=%+v err=%v", full, err)
	}
}

func TestFullFetchPreservesStructuralRecipients(t *testing.T) {
	const target = "target@icloud.com"
	raw := "To: forwarded@example.com\r\nX-Original-To: " + target + "\r\nCc: cc@icloud.com\r\nDelivered-To: delivered@icloud.com\r\nFrom: sender@example.com\r\nSubject: subject-only@example.com\r\nContent-Type: text/plain\r\n\r\nYour verification code is 654321. body-only@example.com\r\n"
	headerSection := metadataRecipientHeaderSection()
	headerSection.Peek = false // Server responses use BODY[...], never BODY.PEEK[...].
	metadata := toMessageWithHeaderOnly(&imap.Message{Uid: 105, Body: map[*imap.BodySectionName]imap.Literal{headerSection: strings.NewReader(raw)}}, headerSection, "INBOX")
	if !metadata.matches(target) {
		t.Fatal("invalid fixture: metadata must match alias")
	}
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprintf("batch=%v", batch), func(t *testing.T) {
			c := fullFetchRecipientTestClient(t, raw)
			var full *FullMessage
			if batch {
				msgs, err := c.GetFullBatchInFolderWithValidity("INBOX", 1, []uint32{105})
				if err != nil || len(msgs) != 1 {
					t.Fatalf("fetch: count=%d err=%v", len(msgs), err)
				}
				full = msgs[0]
			} else {
				var err error
				full, err = c.GetFullInFolderWithValidity("INBOX", 1, 105)
				if err != nil {
					t.Fatal(err)
				}
			}
			for _, recipient := range []string{target, "forwarded@example.com", "cc@icloud.com", "delivered@icloud.com"} {
				if !full.Message.matches(recipient) {
					t.Errorf("full fetch lost recipient %s: recipients=%v", recipient, full.RecipientAddresses())
				}
			}
			for _, unrelated := range []string{"sender@example.com", "subject-only@example.com", "body-only@example.com"} {
				if full.Message.matches(unrelated) {
					t.Errorf("non-recipient address matched: %s", unrelated)
				}
			}
			if !strings.Contains(full.Body, "654321") {
				t.Fatal("recipient extraction consumed the message body")
			}
		})
	}
}
