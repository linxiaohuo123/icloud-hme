package mail

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

func TestRepeatedDeliveryHeadersPreserveOriginalAlias(t *testing.T) {
	const alias = "original@icloud.com"
	for _, header := range []string{"Delivered-To", "X-Original-To", "Cc", "X-Forwarded-To", "Resent-To", "X-Apple-Original-To"} {
		for _, mode := range []string{"metadata", "full"} {
			t.Run(header+"/"+mode, func(t *testing.T) {
				raw := "To: undisclosed-recipients:;\r\n" + header + ": final@example.com\r\n" + header + ": " + alias + "\r\nContent-Type: text/plain\r\n\r\nYour verification code is 654321.\r\n"
				section := metadataRecipientHeaderSection()
				if mode == "full" {
					section = &imap.BodySectionName{}
				}
				section.Peek = false
				message := &imap.Message{Uid: 105, Body: map[*imap.BodySectionName]imap.Literal{section: strings.NewReader(raw)}}
				var parsed Message
				if mode == "full" {
					full := &FullMessage{}
					if err := decodeFullMessageBody(full, message, section); err != nil {
						t.Fatal(err)
					}
					parsed = full.Message
				} else {
					parsed = toMessageWithHeaderOnly(message, section, "INBOX")
				}
				if !parsed.matches(alias) || !parsed.matches("final@example.com") {
					t.Fatalf("lost repeated recipients: %v", parsed.RecipientAddresses())
				}
			})
		}
	}
}

func TestBatchBodyFailuresRetainHealthyMessages(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		body      string
		missing   bool
		transport bool
		permanent bool
	}{
		{name: "oversized", body: "Content-Type: text/plain\r\n\r\n" + strings.Repeat("x", 512*1024+1), permanent: true},
		{name: "malformed", body: "invalid header without colon\r\n\r\ncode", permanent: true},
		{name: "missing", missing: true},
		{name: "transport", body: "invalid header without colon\r\n\r\ncode", transport: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			port, stop := spinMockIMAPServer(t, func(tag, command string) []string {
				switch command {
				case "SELECT", "EXAMINE":
					return []string{"* 2 EXISTS", "* OK [UIDVALIDITY 1] generation", "* OK [UIDNEXT 107] next", tag + " OK selected"}
				case "UID":
					first := "* 1 FETCH (UID 105)"
					if !testCase.missing {
						first = fmt.Sprintf("* 1 FETCH (UID 105 BODY[] {%d}\r\n%s)", len(testCase.body), testCase.body)
					}
					valid := "To: healthy@icloud.com\r\nContent-Type: text/plain\r\n\r\nYour verification code is 654321.\r\n"
					last := tag + " OK fetched"
					if testCase.transport {
						last = tag + " NO fetch failed"
					}
					return []string{first, fmt.Sprintf("* 2 FETCH (UID 106 BODY[] {%d}\r\n%s)", len(valid), valid), last}
				default:
					return []string{tag + " OK done"}
				}
			})
			t.Cleanup(stop)
			connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { connection.Close() })
			imapClient, err := client.New(connection)
			if err != nil {
				t.Fatal(err)
			}
			imapClient.Timeout = 2 * time.Second
			if err := imapClient.Login("review", "test-only"); err != nil {
				t.Fatal(err)
			}
			mailClient := NewClientForTesting("review@example.com", "test-only", connection, imapClient)
			messages, err := mailClient.GetFullBatchInFolderWithValidity("INBOX", 1, []uint32{105, 106})
			var batchError *BatchReadError
			if testCase.transport {
				if err == nil || len(messages) != 0 || errors.As(err, &batchError) {
					t.Fatalf("transport failure became partial success: messages=%d err=%v", len(messages), err)
				}
				return
			}
			if !errors.As(err, &batchError) || len(batchError.Failures) != 1 {
				t.Fatalf("missing per-message failure: %v", err)
			}
			failure := batchError.Failures[0]
			if failure.Ref.UID != 105 || failure.Ref.UIDValidity != 1 || failure.Permanent != testCase.permanent {
				t.Fatalf("wrong failure identity or classification: %+v", failure)
			}
			if len(messages) != 1 || messages[0].UID != 106 || !messages[0].BodyComplete || !strings.Contains(messages[0].Body, "654321") {
				t.Fatalf("healthy message lost: %+v", messages)
			}
			if testCase.missing && !errors.Is(failure.Err, ErrMissingMessageBody) {
				t.Fatalf("missing body not retryable: %v", failure.Err)
			}
		})
	}
}

func TestBatchReadErrorMatchesOnlyRequestedIdentity(t *testing.T) {
	ref := MessageRef{Provider: "imap", AccountID: "account", Mailbox: "INBOX", UIDValidity: 1, UID: 105}
	want := errors.New("bad body")
	failure := &BatchReadError{Failures: []MessageReadFailure{{Ref: ref, Err: want, Permanent: true}}}
	for _, testCase := range []struct {
		name string
		ref  MessageRef
		want error
	}{
		{"canonical", ref, want},
		{"legacy", MessageRef{Provider: "imap", AccountID: "account", Mailbox: "INBOX", UID: 105}, want},
		{"generation", MessageRef{Provider: "imap", AccountID: "account", Mailbox: "INBOX", UIDValidity: 2, UID: 105}, nil},
		{"account", MessageRef{Provider: "imap", AccountID: "other", Mailbox: "INBOX", UIDValidity: 1, UID: 105}, nil},
		{"folder", MessageRef{Provider: "imap", AccountID: "account", Mailbox: "Junk", UIDValidity: 1, UID: 105}, nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := failure.ErrorFor(testCase.ref); got != testCase.want {
				t.Fatalf("error=%v, want %v", got, testCase.want)
			}
		})
	}
}
