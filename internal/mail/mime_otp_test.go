// [POS]: 真实 MIME 解码、单封/批量 IMAP FETCH 与验证码字段仲裁回归
// [PROTOCOL]: 变更时检查 CLAUDE.md
package mail

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/emersion/go-imap"
)

func otpAlternativeMessage(plain, html string) string {
	return "Subject: Verification code\r\nContent-Type: multipart/alternative; boundary=otp\r\n\r\n--otp\r\nContent-Type: text/plain\r\n\r\n" + plain +
		"\r\n--otp\r\nContent-Type: text/html\r\n\r\n" + html + "\r\n--otp--\r\n"
}

func TestMIMEOTPFieldsStayIndependent(t *testing.T) {
	for _, tc := range []struct{ name, plain, html, code, link string }{
		{"duplicate alternatives", "Your verification code is 482019", "<p>482019</p><p>Expires in ten minutes</p>", "482019", ""},
		{"plain only", "Your verification code is 482019", "<p>Sign in</p>", "482019", ""},
		{"HTML only", "Sign in", "<p>Code: 482019</p>", "482019", ""},
		{"grouped code", "Code: 482\n019", "<p>Sign in</p>", "482019", ""},
		{"conflicting alternatives", "Code: 482019", "<p>Code: 765432</p>", "", ""},
		{"complementary link", "Code: 482019", `<a href="https://example.com/verify?token=abc">Verify</a>`, "482019", "https://example.com/verify?token=abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := otpAlternativeMessage(tc.plain, tc.html)
			section := &imap.BodySectionName{}
			full := &FullMessage{Message: Message{Subject: "Verification code"}}
			if err := decodeFullMessageBody(full, &imap.Message{Uid: 1, Body: map[*imap.BodySectionName]imap.Literal{section: strings.NewReader(raw)}}, section); err != nil {
				t.Fatal(err)
			}
			legacy := toMessageWithBody(&imap.Message{Uid: 1, Envelope: &imap.Envelope{Subject: "Verification code"}, Body: map[*imap.BodySectionName]imap.Literal{section: strings.NewReader(raw)}})
			for _, otp := range []*OTPResult{full.ExtractOTP(), legacy.ExtractOTP()} {
				if tc.code == "" {
					if otp != nil {
						t.Fatalf("ambiguous OTP accepted: %+v", otp)
					}
					continue
				}
				if otp == nil || otp.Code != tc.code || otp.MagicLink != tc.link {
					t.Fatalf("OTP=%+v", otp)
				}
			}
			data, err := json.Marshal(full)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "otpBodies") || strings.Contains(string(data), "otp_bodies") {
				t.Fatal("internal fields leaked into JSON")
			}
		})
	}
}

func TestFullFetchMIMEOTPInSingleAndBatchReads(t *testing.T) {
	raw := otpAlternativeMessage("Your verification code is 482019", "<p>482019</p><p>Expires in ten minutes</p>")
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprint(batch), func(t *testing.T) {
			c := fullFetchRecipientTestClient(t, raw)
			var full *FullMessage
			if batch {
				msgs, err := c.GetFullBatchInFolderWithValidity("INBOX", 1, []uint32{105})
				if err != nil || len(msgs) != 1 {
					t.Fatalf("count=%d err=%v", len(msgs), err)
				}
				full = msgs[0]
			} else {
				var err error
				full, err = c.GetFullInFolderWithValidity("INBOX", 1, 105)
				if err != nil {
					t.Fatal(err)
				}
			}
			if otp := full.ExtractOTP(); otp == nil || otp.Code != "482019" {
				t.Fatalf("real fetch OTP=%+v", otp)
			}
		})
	}
}
