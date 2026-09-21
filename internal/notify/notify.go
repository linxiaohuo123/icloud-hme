/**
 * [INPUT]: 依赖 bytes, encoding/json, errors, fmt, io, net/http, sync, time
 * [OUTPUT]: 对外提供 Sender, NewSender, Settings, Event, ChannelResult 与事件 Kind 常量
 * [POS]: internal/notify 的通知中枢，聚合飞书/Bark/Telegram 三渠道，提供事件开关、
 *        防重发节流与异步非阻塞投递，任何通知故障都不许影响出号与收码主链路
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

// Package notify 实现系统通知渠道。
//
// 设计约束:
//   - Emit 永不阻塞: 队列满或事件被开关/节流过滤时直接丢弃并记日志
//   - 边沿触发由上游(监控器)负责跳变检测, 本包按 (kind, account) 做最小重发间隔节流,
//     防止周期校验把同一失效事件每小时重发一遍
//   - SendTest 同步执行并返回逐渠道结果, 供设置页一键验证
package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 事件 Kind 常量。
const (
	KindCookieExpired   = "cookie_expired"   // Cookie 失效(账号标记 error)
	KindCookieRecovered = "cookie_recovered" // Cookie 恢复(error → active)
	KindQuotaLow        = "quota_low"        // 别名配额水位告警
	KindTest            = "test"             // 设置页测试推送
)

// DefaultEventKinds 返回全部业务事件的默认开关(默认全开)。
func DefaultEventKinds() map[string]bool {
	return map[string]bool{
		KindCookieExpired:   true,
		KindCookieRecovered: true,
		KindQuotaLow:        true,
	}
}

// Settings 是通知配置(持久化到 store 的 settings KV)。
type Settings struct {
	FeishuWebhook  string          `json:"feishu_webhook"`           // 飞书自定义机器人 webhook
	BarkURL        string          `json:"bark_url"`                 // Bark 推送地址(含 device key)
	TelegramToken  string          `json:"telegram_token"`           // Telegram Bot Token
	TelegramChat   string          `json:"telegram_chat"`            // Telegram Chat ID
	EventKinds     map[string]bool `json:"event_kinds"`              // 事件开关
	QuotaThreshold int             `json:"quota_threshold"`          // 别名配额水位阈值, 0=关闭
	ResendMinutes  int             `json:"resend_minutes,omitempty"` // 失效事件重提醒间隔(分钟), 0=6h
}

// Event 是一条待通知事件。
type Event struct {
	Kind        string
	AccountID   string
	AccountName string
	Title       string
	Message     string
}

// ChannelResult 是测试推送的逐渠道结果。
type ChannelResult struct {
	Channel string `json:"channel"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

// Sender 是通知发送中枢, 线程安全。
type Sender struct {
	mu           sync.RWMutex
	settings     Settings
	lastSent     map[string]time.Time // 节流键 kind+"/"+accountID → 上次发送时间
	queue        chan Event
	client       *http.Client
	telegramBase string // 可注入测试
	stopCh       chan struct{}
	once         sync.Once
	stopOnce     sync.Once
}

// NewSender 创建通知中枢。调用 Start 启动后台投递协程。
func NewSender() *Sender {
	return &Sender{
		settings:     defaultSettings(),
		lastSent:     make(map[string]time.Time),
		queue:        make(chan Event, 32),
		client:       &http.Client{Timeout: 10 * time.Second},
		telegramBase: "https://api.telegram.org",
		stopCh:       make(chan struct{}),
	}
}

// Start 启动后台投递协程(幂等)。
func (s *Sender) Start() {
	s.once.Do(func() {
		go s.loop()
	})
}

func defaultSettings() Settings {
	return Settings{EventKinds: DefaultEventKinds()}
}

// UpdateSettings 原子替换通知配置。
func (s *Sender) UpdateSettings(settings Settings) {
	if settings.EventKinds == nil {
		settings.EventKinds = DefaultEventKinds()
	}
	s.mu.Lock()
	s.settings = settings
	s.mu.Unlock()
}

// Settings 返回当前配置快照。
func (s *Sender) Settings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.settings
}

// QuotaThreshold 返回配额水位阈值(0=关闭)。
func (s *Sender) QuotaThreshold() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.settings.QuotaThreshold
}

// Stop 停止后台投递协程(线程安全且幂等)。
func (s *Sender) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
}

// Emit 非阻塞投递一条事件: 被开关过滤、节流命中或队列满时直接丢弃, 绝不阻塞业务路径。
//
// 【正确性】节流时间戳只在【入队成功之后】登记。若先登记再入队，队列满被丢弃的
// 告警会白白消耗掉 6h/24h 的重提醒窗口，导致 Cookie 失效告警被静默吞掉一整天。
func (s *Sender) Emit(ev Event) {
	if !s.eventEnabled(ev.Kind) {
		return
	}
	wait := s.throttleWindow(ev.Kind)
	if !s.throttleAllows(ev, wait) {
		return
	}
	select {
	case s.queue <- ev:
		s.markSent(ev, wait)
	default:
		log.Printf("[Notify] 通知队列已满，丢弃事件 kind=%s account=%s", ev.Kind, ev.AccountID)
	}
}

// eventEnabled 判断事件是否被配置开关放行 (test 事件永远放行)。
func (s *Sender) eventEnabled(kind string) bool {
	if kind == KindTest {
		return true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.settings.EventKinds) == 0 {
		return true
	}
	return s.settings.EventKinds[kind]
}

// throttleWindow 返回该事件的最小重发间隔(0 表示不节流)。
// cookie_expired / quota_low 持续存在时按间隔重提醒, 其余事件(如恢复)不节流。
func (s *Sender) throttleWindow(kind string) time.Duration {
	if kind == KindTest {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	switch kind {
	case KindCookieExpired:
		if s.settings.ResendMinutes > 0 {
			return time.Duration(s.settings.ResendMinutes) * time.Minute
		}
		return 6 * time.Hour
	case KindQuotaLow:
		return 24 * time.Hour
	}
	return 0
}

// throttleAllows 只读判断同 (kind, account) 事件是否已超出最小重发间隔。
func (s *Sender) throttleAllows(ev Event, wait time.Duration) bool {
	if wait == 0 {
		return true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	last, seen := s.lastSent[throttleKey(ev)]
	return !seen || time.Since(last) >= wait
}

// markSent 登记本次成功入队的发送时间。
func (s *Sender) markSent(ev Event, wait time.Duration) {
	if wait == 0 {
		return
	}
	s.mu.Lock()
	if s.lastSent == nil {
		s.lastSent = make(map[string]time.Time)
	}
	s.lastSent[throttleKey(ev)] = time.Now()
	s.mu.Unlock()
}

func throttleKey(ev Event) string { return ev.Kind + "/" + ev.AccountID }

func (s *Sender) loop() {
	for {
		select {
		case <-s.stopCh:
			return
		case ev := <-s.queue:
			s.dispatch(ev)
		}
	}
}

// dispatch 把事件发往全部已配置渠道, 单渠道失败只记日志。
func (s *Sender) dispatch(ev Event) {
	s.mu.RLock()
	settings := s.settings
	s.mu.RUnlock()

	text := formatText(ev)
	var channels []struct {
		name string
		send func() error
	}
	if settings.FeishuWebhook != "" {
		hook := settings.FeishuWebhook
		channels = append(channels, struct {
			name string
			send func() error
		}{"feishu", func() error { return SendFeishu(s.client, hook, text) }})
	}
	if settings.BarkURL != "" {
		bark := settings.BarkURL
		channels = append(channels, struct {
			name string
			send func() error
		}{"bark", func() error { return SendBark(s.client, bark, ev.Title, text) }})
	}
	if settings.TelegramToken != "" && settings.TelegramChat != "" {
		token, chat, base := settings.TelegramToken, settings.TelegramChat, s.telegramBase
		channels = append(channels, struct {
			name string
			send func() error
		}{"telegram", func() error { return SendTelegram(s.client, base, token, chat, text) }})
	}

	for _, ch := range channels {
		if err := ch.send(); err != nil {
			log.Printf("[Notify] %s 推送失败 kind=%s: %v", ch.name, ev.Kind, err)
		}
	}
}

// SendTest 同步向全部已配置渠道发送测试通知, 返回逐渠道结果。
func (s *Sender) SendTest() []ChannelResult {
	s.mu.RLock()
	settings := s.settings
	base := s.telegramBase
	s.mu.RUnlock()

	ev := Event{
		Kind:    KindTest,
		Title:   "iCloud HME 测试通知",
		Message: "如果你看到这条消息，说明通知渠道配置成功。\n时间: " + time.Now().Format("2006-01-02 15:04:05"),
	}
	text := formatText(ev)

	var results []ChannelResult
	run := func(name string, configured bool, send func() error) {
		if !configured {
			return
		}
		err := send()
		r := ChannelResult{Channel: name, OK: err == nil}
		if err != nil {
			r.Error = err.Error()
		}
		results = append(results, r)
	}
	run("feishu", settings.FeishuWebhook != "", func() error { return SendFeishu(s.client, settings.FeishuWebhook, text) })
	run("bark", settings.BarkURL != "", func() error { return SendBark(s.client, settings.BarkURL, ev.Title, text) })
	run("telegram", settings.TelegramToken != "" && settings.TelegramChat != "", func() error {
		return SendTelegram(s.client, base, settings.TelegramToken, settings.TelegramChat, text)
	})
	return results
}

func formatText(ev Event) string {
	var sb strings.Builder
	sb.WriteString(ev.Title)
	sb.WriteString("\n")
	sb.WriteString(ev.Message)
	if ev.AccountName != "" {
		sb.WriteString("\n账号: ")
		sb.WriteString(ev.AccountName)
	}
	return sb.String()
}

// SendFeishu 发送飞书自定义机器人文本消息。
func SendFeishu(client *http.Client, webhook, text string) error {
	payload, _ := json.Marshal(map[string]any{
		"msg_type": "text",
		"content":  map[string]string{"text": text},
	})
	body, err := postJSON(client, webhook, payload, "飞书")
	if err != nil {
		return err
	}
	// 飞书失败时也可能返回 200 + 业务错误码
	var decoded struct {
		Code int `json:"code"`
	}
	if json.Unmarshal(body, &decoded) == nil && decoded.Code != 0 {
		return errors.New("飞书 返回业务错误码 " + fmt.Sprint(decoded.Code))
	}
	return nil
}

// SendBark 发送 Bark 推送(URL 形如 https://api.day.app/<device_key>)。
func SendBark(client *http.Client, baseURL, title, body string) error {
	url := strings.TrimRight(baseURL, "/")
	payload, _ := json.Marshal(map[string]string{
		"title": title,
		"body":  body,
		"group": "icloud-hme",
	})
	_, err := postJSON(client, url, payload, "Bark")
	return err
}

// SendTelegram 发送 Telegram Bot 消息。
func SendTelegram(client *http.Client, apiBase, token, chatID, text string) error {
	url := fmt.Sprintf("%s/bot%s/sendMessage", strings.TrimRight(apiBase, "/"), token)
	payload, _ := json.Marshal(map[string]string{
		"chat_id": chatID,
		"text":    text,
	})
	_, err := postJSON(client, url, payload, "Telegram")
	return err
}

// postJSON 发送 JSON POST 并校验 2xx 响应, 返回响应体。
func postJSON(client *http.Client, url string, payload []byte, name string) ([]byte, error) {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%s 请求失败: %w", name, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s 返回 HTTP %d", name, resp.StatusCode)
	}
	return body, nil
}
