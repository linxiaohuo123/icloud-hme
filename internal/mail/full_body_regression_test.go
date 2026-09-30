package mail

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/emersion/go-imap"
)

func TestRegressionFullHTMLBodyMustRetainMarkup(t *testing.T) {
	section := &imap.BodySectionName{}
	raw := "Content-Type: text/html; charset=utf-8\r\n\r\n<html><body><p style=\"color:red\">Styled mail</p></body></html>"
	msg := &imap.Message{Uid: 1, Body: map[*imap.BodySectionName]imap.Literal{section: strings.NewReader(raw)}}
	full := &FullMessage{}
	if err := decodeFullMessageBody(full, msg, section); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(full.Body, `<p style="color:red">`) || !full.BodyComplete || full.ContentType != "text/html; charset=utf-8" || full.Preview != "Styled mail" {
		t.Fatalf("HTML markup lost on real MIME decode: ContentType=%q BodyComplete=%v Body=%q", full.ContentType, full.BodyComplete, full.Body)
	}
}

func TestFullMIMEBodyAndPreviewRemainIndependent(t *testing.T) {
	html := `<html><head><style>.code { color: red; }</style></head><body><p>Your verification code is <b>654321</b></p></body></html>`
	for _, tc := range []struct{ name, raw, body, contentType, previewContains string }{
		{"base64 HTML", "Content-Type: text/html\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString([]byte(html)), html, "text/html; charset=utf-8", "654321"},
		{"literal plain markup", "Content-Type: text/plain\r\n\r\n<p>literal plain text</p>", "<p>literal plain text</p>", "text/plain; charset=utf-8", "<p>literal plain text</p>"},
		{"empty HTML", "Content-Type: text/html\r\n\r\n", "", "text/html; charset=utf-8", ""},
		{"nested alternatives", "Content-Type: multipart/mixed; boundary=outer\r\n\r\n--outer\r\nContent-Type: multipart/alternative; boundary=inner\r\n\r\n--inner\r\nContent-Type: text/plain\r\n\r\nYour verification code is 654321\r\n--inner\r\nContent-Type: text/html\r\n\r\n<p style=\"color:red\">Sign in</p>\r\n--inner--\r\n--outer\r\nContent-Type: text/html\r\nContent-Disposition: attachment; filename=\"fake.html\"\r\n\r\n<p>Attachment code 123456</p>\r\n--outer--\r\n", `<p style="color:red">Sign in</p>`, "text/html; charset=utf-8", "654321"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			section := &imap.BodySectionName{}
			msg := &imap.Message{Uid: 1, Body: map[*imap.BodySectionName]imap.Literal{section: strings.NewReader(tc.raw)}}
			full := &FullMessage{}
			if err := decodeFullMessageBody(full, msg, section); err != nil {
				t.Fatal(err)
			}
			if full.Body != tc.body || full.ContentType != tc.contentType || !full.BodyComplete || !strings.Contains(full.Preview, tc.previewContains) || strings.Contains(full.Preview, "123456") {
				t.Fatalf("wrong MIME representations: %+v", full)
			}
			if strings.Contains(tc.contentType, "text/html") && strings.Contains(full.Preview, "<") {
				t.Fatalf("raw markup leaked into preview: %q", full.Preview)
			}
			if tc.previewContains == "654321" {
				otp := ExtractOTP("Verification", full.Preview)
				if otp == nil || otp.Code != "654321" {
					t.Fatalf("alternative code lost: %+v", otp)
				}
			}
		})
	}
}
