// [POS]: 默认通知发送器与空 client 的真实本机私网阻断回归
// [PROTOCOL]: 变更时检查 CLAUDE.md
package notify

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"icloud-hme/internal/security"
)

func TestNotificationDefaultClientsRejectPrivateTargets(t *testing.T) {
	t.Setenv(security.AllowPrivateOutboundEnv, "")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	if err := SendBark(nil, srv.URL, "title", "private content"); !errors.Is(err, security.ErrPrivateOutbound) {
		t.Fatalf("nil client did not enforce policy: %v", err)
	}
	sender := NewSender()
	defer sender.Stop()
	defer sender.client.CloseIdleConnections()
	sender.UpdateSettings(Settings{FeishuWebhook: srv.URL, BarkURL: srv.URL})
	for _, r := range sender.SendTest() {
		if r.OK {
			t.Fatalf("private target accepted: %+v", r)
		}
		if !strings.Contains(r.Error, "安全策略拒绝") || strings.Contains(r.Error, srv.URL) {
			t.Fatalf("unsafe or unclear policy error: %+v", r)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("private endpoint received %d notifications", hits.Load())
	}
}
