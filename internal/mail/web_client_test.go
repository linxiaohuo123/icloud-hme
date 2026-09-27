package mail

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestWebClientFindByAliasExpandsSaturatedSearch(t *testing.T) {
	const alias = "target@icloud.com"
	var limits []int
	var limitsMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			MaxResults int `json:"maxResults"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode search: %v", err)
		}
		limitsMu.Lock()
		limits = append(limits, request.MaxResults)
		limitsMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if request.MaxResults == 2 {
			_, _ = io.WriteString(w, `{"threadList":[{"threadId":"wrong","to":"other@example.com","subject":"target@icloud.com"},{"threadId":"right-1","to":"target@icloud.com"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"threadList":[{"threadId":"wrong","to":"other@example.com","subject":"target@icloud.com"},{"threadId":"right-1","to":"target@icloud.com"},{"threadId":"right-2","to":"target@icloud.com"}]}`)
	}))
	defer server.Close()
	client, err := NewWebClient(nil, "test-dsid", "icloud.com", "")
	if err != nil {
		t.Fatal(err)
	}
	client.mccGatewayURL = server.URL

	matches, err := client.FindByAliasContext(context.Background(), alias, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 || matches[0].ID != "right-1" || matches[1].ID != "right-2" {
		t.Fatalf("search should include later verified matches: %+v", matches)
	}
	limitsMu.Lock()
	defer limitsMu.Unlock()
	if len(limits) != 2 || limits[0] != 2 || limits[1] != 4 {
		t.Fatalf("expected candidate windows 2,4; got %v", limits)
	}
}

func TestWebClientGetThreadContextReadsOlderThread(t *testing.T) {
	const threadID = "older-thread"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mailws2/v1/thread/get" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var request struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.ThreadID != threadID {
			t.Errorf("unexpected thread request: %+v, %v", request, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"messageMetadataList":[{"subject":"Older mail","preview":"stale preview","date":1789891200000},{"subject":"Old mail","preview":"older preview","from":{"email":"sender@example.com"},"to":[{"email":"target@icloud.com"}],"date":1789977600000}]}`)
	}))
	defer server.Close()
	client, err := NewWebClient(nil, "test-dsid", "icloud.com", "")
	if err != nil {
		t.Fatal(err)
	}
	client.mccGatewayURL = server.URL

	message, err := client.GetThreadContext(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	if message == nil || message.ID != threadID || message.Subject != "Old mail" || message.From != "sender@example.com" || message.To != "target@icloud.com" || message.Preview != "older preview" || message.Date == "" {
		t.Fatalf("thread detail was not decoded: %+v", message)
	}
}

func TestWebClientFindByAliasReportsSaturatedSearch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			MaxResults int `json:"maxResults"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode search: %v", err)
		}
		threads := make([]map[string]string, request.MaxResults)
		for i := range threads {
			threads[i] = map[string]string{"threadId": strconv.Itoa(i), "to": "other@example.com", "subject": "target@icloud.com"}
		}
		threads[0]["to"] = "target@icloud.com"
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"threadList": threads})
	}))
	defer server.Close()
	client, err := NewWebClient(nil, "test-dsid", "icloud.com", "")
	if err != nil {
		t.Fatal(err)
	}
	client.mccGatewayURL = server.URL

	if _, err := client.FindByAliasContext(context.Background(), "target@icloud.com", 2); err == nil || !strings.Contains(err.Error(), "超过可核验上限") {
		t.Fatalf("saturated search must not report partial success: %v", err)
	}
}

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
