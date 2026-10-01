package notify

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"strings"
	"testing"
)

type failingNotificationTransport struct{ cause error }

func (f failingNotificationTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, f.cause
}

func TestNotificationFailuresDoNotExposeCredentials(t *testing.T) {
	const secret = "FICTIONAL_NOTIFICATION_SECRET"
	// Even a transport that echoes the URL must not leak credentials.
	cause := errors.New("connection failed for https://notify.invalid/" + secret)
	client := &http.Client{Transport: failingNotificationTransport{cause: cause}}
	sender := NewSender()
	sender.client = client
	sender.telegramBase = "https://telegram.invalid"
	sender.UpdateSettings(Settings{
		FeishuWebhook: "https://feishu.invalid/" + secret,
		BarkURL:       "https://bark.invalid/" + secret,
		TelegramToken: secret,
		TelegramChat:  "test-chat",
	})
	results := sender.SendTest()
	if len(results) != 3 {
		t.Fatalf("expected three channel failures, got %+v", results)
	}
	for _, result := range results {
		if result.OK || result.Error == "" || strings.Contains(result.Error, secret) || strings.Contains(result.Error, ".invalid") {
			t.Fatalf("unsafe or falsely successful channel result: %+v", result)
		}
	}
	err := SendTelegram(client, sender.telegramBase, secret, "test-chat", "test message")
	if !errors.Is(err, cause) {
		t.Fatalf("lost inspectable network cause: %v", err)
	}
	var output bytes.Buffer
	originalWriter := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(originalWriter)
	sender.dispatch(Event{Kind: KindTest, Title: "test"})
	if strings.Contains(output.String(), secret) || strings.Contains(output.String(), ".invalid") {
		t.Fatalf("notification log exposed request details: %s", output.String())
	}
	if strings.Count(output.String(), "推送失败") != 3 {
		t.Fatalf("channel failures were not logged: %s", output.String())
	}
}
