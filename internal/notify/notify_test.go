package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSendFeishuAndBark 校验两个渠道的请求体格式与 2xx 判定。
func TestSendFeishuAndBark(t *testing.T) {
	var mu sync.Mutex
	var feishuBody, barkBody map[string]any
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		_ = json.Unmarshal(raw, &feishuBody)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"success"}`))
	}))
	defer feishu.Close()

	bark := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		_ = json.Unmarshal(raw, &barkBody)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"code":200}`))
	}))
	defer bark.Close()

	if err := SendFeishu(http.DefaultClient, feishu.URL, "测试消息"); err != nil {
		t.Fatalf("飞书发送失败: %v", err)
	}
	mu.Lock()
	if feishuBody["msg_type"] != "text" {
		t.Fatalf("飞书消息类型错误: %v", feishuBody)
	}
	mu.Unlock()

	if err := SendBark(http.DefaultClient, bark.URL+"/", "标题", "内容"); err != nil {
		t.Fatalf("Bark 发送失败: %v", err)
	}
	mu.Lock()
	if barkBody["title"] != "标题" || barkBody["group"] != "icloud-hme" {
		t.Fatalf("Bark 请求体错误: %v", barkBody)
	}
	mu.Unlock()
}

// TestSendFeishuBusinessError 校验飞书 200 + 非零业务码按失败处理。
func TestSendFeishuBusinessError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":19001,"msg":"invalid key"}`))
	}))
	defer srv.Close()
	if err := SendFeishu(http.DefaultClient, srv.URL, "x"); err == nil {
		t.Fatal("非零业务码应返回错误")
	}
}

// newTestSender 构造已启动的 Sender。
func newTestSender(settings Settings) *Sender {
	s := NewSender()
	s.UpdateSettings(settings)
	s.Start()
	return s
}

// TestSenderEmitDispatchToChannels 校验 Emit 异步分发到全部已配置渠道。
func TestSenderEmitDispatchToChannels(t *testing.T) {
	var feishuHits, barkHits atomic.Int32
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		feishuHits.Add(1)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer feishu.Close()
	bark := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		barkHits.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer bark.Close()

	s := newTestSender(Settings{
		FeishuWebhook: feishu.URL,
		BarkURL:       bark.URL,
		EventKinds:    DefaultEventKinds(),
	})
	defer s.Stop()
	s.Emit(Event{Kind: KindCookieRecovered, AccountID: "acc_1", Title: "T", Message: "M"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if feishuHits.Load() >= 1 && barkHits.Load() >= 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("期望两个渠道各收到 1 条, 实际 feishu=%d bark=%d", feishuHits.Load(), barkHits.Load())
}

// TestSenderThrottle 同一账号的失效事件按间隔节流，恢复事件不节流。
func TestSenderThrottle(t *testing.T) {
	s := newTestSender(Settings{EventKinds: DefaultEventKinds()})
	defer s.Stop()

	expired := func(acc string) Event {
		return Event{Kind: KindCookieExpired, AccountID: acc}
	}
	if !s.throttleAllows(expired("a"), s.throttleWindow(KindCookieExpired)) {
		t.Fatal("首次失效事件应放行")
	}
	s.markSent(expired("a"), s.throttleWindow(KindCookieExpired))
	if s.throttleAllows(expired("a"), s.throttleWindow(KindCookieExpired)) {
		t.Fatal("间隔内的重复失效事件应被节流")
	}
	if !s.throttleAllows(expired("b"), s.throttleWindow(KindCookieExpired)) {
		t.Fatal("不同账号不应互相节流")
	}
	if !s.throttleAllows(Event{Kind: KindCookieRecovered, AccountID: "a"}, s.throttleWindow(KindCookieRecovered)) {
		t.Fatal("恢复事件不应节流")
	}
	if !s.throttleAllows(Event{Kind: KindTest}, s.throttleWindow(KindTest)) {
		t.Fatal("测试事件不应节流")
	}
}

// TestSenderThrottleNotConsumedWhenQueueFull 队列满导致事件被丢弃时，
// 绝不允许消耗掉重提醒窗口，否则 Cookie 失效告警会被静默吞掉一整天。
func TestSenderThrottleNotConsumedWhenQueueFull(t *testing.T) {
	// 刻意不调用 Start():让投递协程缺席，队列状态才可确定性复现
	s := NewSender()
	s.UpdateSettings(Settings{EventKinds: DefaultEventKinds()})
	defer s.Stop()

	for i := 0; i < cap(s.queue); i++ {
		s.queue <- Event{Kind: KindQuotaLow, AccountID: "filler"}
	}

	ev := Event{Kind: KindCookieExpired, AccountID: "acc_x"}
	s.Emit(ev)
	s.mu.RLock()
	_, recorded := s.lastSent[throttleKey(ev)]
	s.mu.RUnlock()
	if recorded {
		t.Fatal("入队失败的事件不得登记节流时间戳")
	}

	// 腾出队列后同一事件必须仍能投递出去
	<-s.queue
	s.Emit(ev)
	s.mu.RLock()
	_, recorded = s.lastSent[throttleKey(ev)]
	s.mu.RUnlock()
	if !recorded {
		t.Fatal("成功入队后应登记节流时间戳")
	}
}

// TestSenderEventSwitch 关闭的事件开关应直接丢弃。
func TestSenderEventSwitch(t *testing.T) {
	s := newTestSender(Settings{
		EventKinds: map[string]bool{
			KindCookieExpired:   false,
			KindCookieRecovered: true,
			KindQuotaLow:        true,
		},
	})
	defer s.Stop()
	if s.eventEnabled(KindCookieExpired) {
		t.Fatal("已关闭的事件应被过滤")
	}
	if !s.eventEnabled(KindCookieRecovered) {
		t.Fatal("已开启的事件应放行")
	}
}

// TestSendTestResults 校验测试推送返回逐渠道结果(含失败渠道)。
func TestSendTestResults(t *testing.T) {
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer feishu.Close()

	s := NewSender()
	defer s.Stop()
	s.telegramBase = "http://127.0.0.1:1" // 不可达端口, 制造失败
	s.UpdateSettings(Settings{
		FeishuWebhook: feishu.URL,
		TelegramToken: "tok",
		TelegramChat:  "chat",
		EventKinds:    DefaultEventKinds(),
	})
	results := s.SendTest()
	if len(results) != 2 {
		t.Fatalf("期望 2 个渠道结果, 实际 %v", results)
	}
	byName := map[string]ChannelResult{}
	for _, r := range results {
		byName[r.Channel] = r
	}
	if byName["feishu"].OK {
		t.Fatal("飞书 5xx 应判定失败")
	}
	if byName["telegram"].OK {
		t.Fatal("不可达 Telegram 应判定失败")
	}
}

// TestQuotaThresholdGetter 校验阈值读取。
func TestQuotaThresholdGetter(t *testing.T) {
	s := NewSender()
	defer s.Stop()
	s.UpdateSettings(Settings{QuotaThreshold: 700, EventKinds: DefaultEventKinds()})
	if got := s.QuotaThreshold(); got != 700 {
		t.Fatalf("期望 700, 实际 %d", got)
	}
}
