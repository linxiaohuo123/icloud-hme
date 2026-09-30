package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"icloud-hme/internal/account"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

const previewTestAPIKey = "preview-test-api-key-strong"
const previewStyledHTML = `<html><body><style>p{font-size:41px}</style><p id="styled" style="color:rgb(255,0,0)">Styled email</p></body></html>`

func newMailPreviewTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	st, err := store.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "preview_account", Name: "Preview", Status: "active"}},
		inbox: InboxResult{AccountID: "preview_account", Count: 3, Messages: []mail.Message{
			{ID: "rich", Subject: "Rich HTML", From: "fixture@example.com"},
			{ID: "older", Subject: "Older HTML", From: "fixture@example.com"},
			{ID: "magic", Subject: "Sign in link", From: "fixture@example.com"},
		}},
		getMessageFunc: func(accountID, id string) (*mail.FullMessage, error) {
			ref, err := mail.ParseMessageRef(id, accountID)
			if err != nil {
				return nil, err
			}
			id = ref.ThreadID
			body, subject := previewStyledHTML, "Rich HTML"
			contentType := "text/html; charset=utf-8"
			if id == "older" {
				body, subject = `<html><body><p>Older email</p></body></html>`, "Older HTML"
			}
			if id == "magic" {
				body, subject = "Use https://example.com/verify?token=sample to sign in", "Sign in link"
				contentType = "text/plain; charset=utf-8"
			}
			return &mail.FullMessage{Message: mail.Message{ID: id, MessageRef: ref.Encode(), Subject: subject, From: "fixture@example.com"}, Body: body, ContentType: contentType, BodyComplete: true}, nil
		},
	}
	return newWithBackendAndStore(fb, Config{APIKey: previewTestAPIKey, DataDir: dir}, st)
}

func TestMailHTMLPreviewSelectsRequestedMessageWithIsolatedPolicy(t *testing.T) {
	srv := newMailPreviewTestServer(t)
	for _, tc := range []struct {
		query           string
		status          int
		body, ancestors string
	}{
		{"&format=html", http.StatusOK, "Styled email", "'none'"},
		{"&format=html&frame=1&message_id=older", http.StatusOK, "Older email", "'self'"},
		{"&format=html&frame=1&message_id=missing", http.StatusNotFound, "MESSAGE_NOT_FOUND", ""},
	} {
		req := httptest.NewRequest("GET", "/mail/raw?email=fixture@icloud.com&api_key="+previewTestAPIKey+tc.query, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.body) {
			t.Fatalf("wrong selected mail: query=%s status=%d body=%s", tc.query, rec.Code, rec.Body)
		}
		if tc.ancestors != "" {
			policy := rec.Header().Get("Content-Security-Policy")
			for _, directive := range []string{"sandbox allow-popups;", "script-src 'none'", "style-src 'unsafe-inline'", "connect-src 'none'", "form-action 'none'", "frame-ancestors " + tc.ancestors} {
				if !strings.Contains(policy, directive) {
					t.Fatalf("missing isolation directive %q: %s", directive, policy)
				}
			}
			if strings.Contains(policy, "allow-same-origin") {
				t.Fatal("mail HTML regained same-origin permissions")
			}
		}
	}
}

func TestMailViewIncludesMagicLinkWithoutCode(t *testing.T) {
	srv := newMailPreviewTestServer(t)
	req := httptest.NewRequest("GET", "/mail/view?email=fixture@icloud.com&api_key="+previewTestAPIKey+"&format=json", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var response struct {
		Success bool         `json:"success"`
		Data    mailViewData `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || !response.Success || len(response.Data.AllOTPs) != 1 {
		t.Fatalf("magic-only mail excluded: %s", rec.Body)
	}
	item := response.Data.AllOTPs[0]
	if !item.HasOTP || item.Code != "" || item.MagicLink != "https://example.com/verify?token=sample" {
		t.Fatalf("wrong verification information: %+v", item)
	}
}
