package mail

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebClientFindByAliasRequiresRecipientMatch(t *testing.T) {
	const alias = "target@icloud.com"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), `"query"`) {
			_, _ = io.WriteString(w, `{"threadList":[{"threadId":"wrong","to":"other@example.com","subject":"target@icloud.com"},{"threadId":"unknown","subject":"target@icloud.com"},{"threadId":"right","to":"target@icloud.com"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"threadList":[{"threadId":"fallback-wrong","to":"other@example.com","preview":"target@icloud.com"}]}`)
	}))
	defer server.Close()

	client, err := NewWebClient(nil, "test-dsid", "icloud.com", "")
	if err != nil {
		t.Fatal(err)
	}
	client.mccGatewayURL = server.URL

	matches, err := client.FindByAliasContext(context.Background(), alias, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].ID != "right" || matches[0].To != alias {
		t.Fatalf("only verified recipient may match: %+v", matches)
	}
}

func TestWebClientFindByAliasDoesNotTrustSearchOrPreview(t *testing.T) {
	const alias = "target@icloud.com"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"threadList":[{"threadId":"wrong","to":"other@example.com","preview":"target@icloud.com"},{"threadId":"unknown","subject":"target@icloud.com"}]}`)
	}))
	defer server.Close()

	client, err := NewWebClient(nil, "test-dsid", "icloud.com", "")
	if err != nil {
		t.Fatal(err)
	}
	client.mccGatewayURL = server.URL

	matches, err := client.FindByAliasContext(context.Background(), alias, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("unverified search hits must not be assigned to alias: %+v", matches)
	}
}

func TestWebClientSearchRejectsBusinessErrors(t *testing.T) {
	for _, body := range []string{
		`{"success": false, "threadList": []}`,
		`{"errorCode":"INVALID_REQUEST","errorDescription":"invalid search","threadList":[]}`,
		`{"success":true}`,
		`{"threadList":null}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			client, err := NewWebClient(nil, "test-dsid", "icloud.com", "")
			if err != nil {
				t.Fatal(err)
			}
			client.mccGatewayURL = server.URL
			if _, err := client.ListInboxContext(context.Background(), 20); err == nil {
				t.Fatal("业务错误或缺失列表不能作为空邮箱")
			}
		})
	}
}

func TestWebClientSearchMapsUnreadFlags(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"threadList":[{"threadId":"unread","flags":[]},{"threadId":"read","flags":["\\Seen"]},{"threadId":"unknown"}]}`)
	}))
	defer server.Close()
	client, err := NewWebClient(nil, "test-dsid", "icloud.com", "")
	if err != nil {
		t.Fatal(err)
	}
	client.mccGatewayURL = server.URL
	messages, err := client.ListInboxContext(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || messages[0].Unread == nil || !*messages[0].Unread || messages[1].Unread == nil || *messages[1].Unread || messages[2].Unread != nil {
		t.Fatalf("flags 已读状态解析错误: %+v", messages)
	}
}
