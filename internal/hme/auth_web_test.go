package hme

import (
	"encoding/json"
	"strings"
	"testing"

	http "github.com/bogdanfinn/fhttp"
)

func TestAuthenticateWebEncodesOpaqueTokens(t *testing.T) {
	c, err := NewClient(nil, "icloud.com", "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	state := &authState{authToken: "opaque\"\\\nsecret", trustToken: "trust\"\\token"}
	c.httpc = &sessionTestHTTP{HttpClient: c.httpc, inner: &http.Client{Transport: sessionTransport(func(r *http.Request) (*http.Response, error) {
		var payload struct {
			AuthToken     string `json:"dsWebAuthToken"`
			TrustToken    string `json:"trustToken"`
			ExtendedLogin bool   `json:"extended_login"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("request must be valid JSON: %v", err)
		}
		if payload.AuthToken != state.authToken || payload.TrustToken != state.trustToken || !payload.ExtendedLogin {
			t.Fatal("opaque token changed or extended login missing")
		}
		return responseFor(r, http.StatusOK, `{"dsInfo":{"dsid":"123"}}`), nil
	})}}
	if err := c.authenticateWeb(state); err != nil || state.dsid != "123" {
		t.Fatalf("authenticateWeb failed: dsid=%q err=%v", state.dsid, err)
	}
}

func TestAuthenticateWebRejectsInvalidSuccessResponse(t *testing.T) {
	for _, body := range []string{`{`, `{}`, `null`, `[]`, `{"dsInfo":{"dsid":""}}`, `{"dsInfo":{"dsid":{}}}`} {
		t.Run(body, func(t *testing.T) {
			c, err := NewClient(nil, "icloud.com", "", false)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.httpc = &sessionTestHTTP{HttpClient: c.httpc, inner: &http.Client{Transport: sessionTransport(func(r *http.Request) (*http.Response, error) {
				return responseFor(r, http.StatusOK, body), nil
			})}}
			state := &authState{dsid: "existing"}
			if err := c.authenticateWeb(state); err == nil || !strings.Contains(err.Error(), "auth web") {
				t.Fatal("invalid HTTP 200 response must fail explicitly")
			}
			if state.dsid != "existing" {
				t.Fatal("failed response changed authentication state")
			}
		})
	}
}
