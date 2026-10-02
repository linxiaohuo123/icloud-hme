// [INPUT]: EventBus cache and boundary subscriptions.
// [OUTPUT]: Physical-source isolation for cached and live OTP events.
// [POS]: internal/mail verification event identity.
// [PROTOCOL]: Update this header and CLAUDE.md when changing this file.
package mail

import (
	"testing"
	"time"
)

func TestEventBusPhysicalSourceIsolation(t *testing.T) {
	b := NewEventBus(time.Minute)
	email := "target@icloud.com"
	old := &CachedOTP{Source: "old", Email: email, Folder: "INBOX", UIDValidity: 1, UID: 150, OTP: &OTPResult{Code: "111111"}}
	b.PublishEvent(old)
	id, ch := b.SubscribeWithBoundary(email, "INBOX", 1, 100, "new")
	defer b.Unsubscribe(email, id)
	select {
	case ev := <-ch:
		t.Fatalf("cached old source delivered: %+v", ev)
	default:
	}
	old.UIDValidity = 2
	b.PublishEvent(old)
	select {
	case ev := <-ch:
		t.Fatalf("old source invalidated new task: %+v", ev)
	default:
	}
	b.PublishEvent(&CachedOTP{Source: "new", Email: email, Folder: "INBOX", UIDValidity: 1, UID: 150, OTP: &OTPResult{Code: "222222"}})
	select {
	case ev := <-ch:
		if ev.Source != "new" || ev.OTP.Code != "222222" {
			t.Fatalf("wrong event: %+v", ev)
		}
	default:
		t.Fatal("new source not delivered")
	}
}

func TestEventBusConsumePreservesOtherPhysicalSource(t *testing.T) {
	b := NewEventBus(time.Minute)
	email := "target@icloud.com"
	for _, source := range []string{"old", "new"} {
		b.PublishEvent(&CachedOTP{Source: source, EventID: "same-message-ref", Email: email, Folder: "INBOX", UIDValidity: 1, UID: 150, OTP: &OTPResult{Code: "123456"}})
	}
	b.ConsumeEvent(email, "same-message-ref", "old")
	id, ch := b.SubscribeWithBoundary(email, "INBOX", 1, 100, "new")
	defer b.Unsubscribe(email, id)
	select {
	case ev := <-ch:
		if ev.Source != "new" {
			t.Fatalf("wrong source %s", ev.Source)
		}
	default:
		t.Fatal("consuming old inbox erased new inbox event")
	}
}
