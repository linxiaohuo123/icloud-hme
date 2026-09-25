/**
 * [INPUT]: 依赖 net/http, encoding/json, strings, gin, icloud-hme/internal/notify, icloud-hme/internal/security
 * [OUTPUT]: 对外提供 getNotifySettingsHandler, updateNotifySettingsHandler, testNotifyHandler, loadNotifySettings
 * [POS]: server 的系统设置 HTTP Handlers (PR-07: 通知配置密文加密、GET 响应脱敏、PATCH 保留原值与 Fail-Closed 门禁)
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/notify"
	"icloud-hme/internal/security"
)

// settingKeyNotify 是通知配置在 store settings KV 中的键名。
const settingKeyNotify = "notify_settings"

// loadNotifySettings 从 store 读取通知配置。若配置存在但解密失败或格式受损，必须 fail closed 返回 error。
func (s *Server) loadNotifySettings() (notify.Settings, error) {
	defaultSettings := notify.Settings{EventKinds: notify.DefaultEventKinds()}
	if s.store == nil {
		return defaultSettings, nil
	}
	raw := s.store.GetSetting(settingKeyNotify)
	if raw == "" {
		return defaultSettings, nil
	}

	// 必须解密成功，严禁解密失败静默返回默认值
	plainJSON, err := s.store.GetEncryptedSetting(settingKeyNotify, security.NotifySettingsAAD())
	if err != nil {
		return defaultSettings, fmt.Errorf("解密通知配置失败 (Fail Closed): %w", err)
	}

	var settings notify.Settings
	if err := json.Unmarshal([]byte(plainJSON), &settings); err != nil {
		return defaultSettings, fmt.Errorf("解析通知配置 JSON 失败 (Fail Closed): %w", err)
	}
	if settings.EventKinds == nil {
		settings.EventKinds = notify.DefaultEventKinds()
	}
	return settings, nil
}

// NotifySettingsResponse 脱敏后的通知设置只读视图 (绝不回显真实 Secret)
type NotifySettingsResponse struct {
	FeishuConfigured    bool            `json:"feishu_configured"`
	FeishuWebhookMasked string          `json:"feishu_webhook_masked,omitempty"`
	FeishuWebhook       string          `json:"feishu_webhook"` // 兼容旧前端
	BarkConfigured      bool            `json:"bark_configured"`
	BarkURLMasked       string          `json:"bark_url_masked,omitempty"`
	BarkURL             string          `json:"bark_url"` // 兼容旧前端
	TelegramConfigured  bool            `json:"telegram_configured"`
	TelegramTokenMasked string          `json:"telegram_token_masked,omitempty"`
	TelegramToken       string          `json:"telegram_token"` // 兼容旧前端
	TelegramChat        string          `json:"telegram_chat"`
	EventKinds          map[string]bool `json:"event_kinds"`
	QuotaThreshold      int             `json:"quota_threshold"`
	ResendMinutes       int             `json:"resend_minutes,omitempty"`
}

// getNotifySettingsHandler 处理 GET /api/settings/notify。
func (s *Server) getNotifySettingsHandler(c *gin.Context) {
	st := s.notifier.Settings()
	feishuMasked := maskOutboundURLSecret(st.FeishuWebhook)
	barkMasked := maskOutboundURLSecret(st.BarkURL)
	tgMasked := maskTokenSecret(st.TelegramToken)

	resp := NotifySettingsResponse{
		FeishuConfigured:    st.FeishuWebhook != "",
		FeishuWebhookMasked: feishuMasked,
		FeishuWebhook:       feishuMasked,
		BarkConfigured:      st.BarkURL != "",
		BarkURLMasked:       barkMasked,
		BarkURL:             barkMasked,
		TelegramConfigured:  st.TelegramToken != "",
		TelegramTokenMasked: tgMasked,
		TelegramToken:       tgMasked,
		TelegramChat:        st.TelegramChat,
		EventKinds:          st.EventKinds,
		QuotaThreshold:      st.QuotaThreshold,
		ResendMinutes:       st.ResendMinutes,
	}
	ok(c, resp)
}

// UpdateNotifySettingsRequest PATCH-like 更新通知配置契约
type UpdateNotifySettingsRequest struct {
	FeishuWebhook  *string         `json:"feishu_webhook"`
	BarkURL        *string         `json:"bark_url"`
	TelegramToken  *string         `json:"telegram_token"`
	TelegramChat   *string         `json:"telegram_chat"`
	EventKinds     map[string]bool `json:"event_kinds"`
	QuotaThreshold *int            `json:"quota_threshold"`
	ResendMinutes  *int            `json:"resend_minutes"`

	ClearFeishu   bool `json:"clear_feishu"`
	ClearBark     bool `json:"clear_bark"`
	ClearTelegram bool `json:"clear_telegram"`
}

// updateNotifySettingsHandler 处理 PUT /api/settings/notify。
func (s *Server) updateNotifySettingsHandler(c *gin.Context) {
	var req UpdateNotifySettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误")
		return
	}

	current := s.notifier.Settings()

	// 1. 飞书 Webhook 更新策略: 显式传空或 clear 标记均执行清空; 脱敏值静默保持
	if req.ClearFeishu || (req.FeishuWebhook != nil && strings.TrimSpace(*req.FeishuWebhook) == "") {
		current.FeishuWebhook = ""
	} else if req.FeishuWebhook != nil {
		val := strings.TrimSpace(*req.FeishuWebhook)
		if val != "" && !strings.Contains(val, "****") {
			if err := validateOutboundPublicURL(val, "飞书 Webhook"); err != nil {
				failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
				return
			}
			current.FeishuWebhook = val
		}
	}

	// 2. Bark 推送地址更新策略: 显式传空或 clear 标记均执行清空; 脱敏值静默保持
	if req.ClearBark || (req.BarkURL != nil && strings.TrimSpace(*req.BarkURL) == "") {
		current.BarkURL = ""
	} else if req.BarkURL != nil {
		val := strings.TrimSpace(*req.BarkURL)
		if val != "" && !strings.Contains(val, "****") {
			if err := validateOutboundPublicURL(val, "Bark 推送地址"); err != nil {
				failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
				return
			}
			current.BarkURL = val
		}
	}

	// 3. Telegram 更新策略: 显式传空或 clear 标记均执行清空; 脱敏值静默保持
	if req.ClearTelegram {
		current.TelegramToken = ""
		current.TelegramChat = ""
	} else {
		if req.TelegramToken != nil {
			val := strings.TrimSpace(*req.TelegramToken)
			if val == "" {
				current.TelegramToken = ""
			} else if !strings.Contains(val, "****") {
				current.TelegramToken = val
			}
		}
		if req.TelegramChat != nil {
			current.TelegramChat = strings.TrimSpace(*req.TelegramChat)
		}
	}

	// 校验 Telegram 成对出现
	if (current.TelegramToken == "") != (current.TelegramChat == "") {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "Telegram Bot Token 与 Chat ID 必须同时填写")
		return
	}

	// 4. 数值与开关字段更新
	if req.QuotaThreshold != nil {
		if *req.QuotaThreshold < 0 || *req.QuotaThreshold > 2000 {
			failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "配额水位阈值需在 0-2000 之间 (0 表示关闭)")
			return
		}
		current.QuotaThreshold = *req.QuotaThreshold
	}

	if req.ResendMinutes != nil {
		if *req.ResendMinutes < 0 || *req.ResendMinutes > 7*24*60 {
			failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "重提醒间隔需在 0-10080 分钟之间")
			return
		}
		current.ResendMinutes = *req.ResendMinutes
	}

	if req.EventKinds != nil {
		for kind := range req.EventKinds {
			switch kind {
			case notify.KindCookieExpired, notify.KindCookieRecovered, notify.KindQuotaLow:
			default:
				failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "未知的事件类型: "+kind)
				return
			}
		}
		current.EventKinds = req.EventKinds
	}

	// 5. 认证加密持久化到 Store
	if s.store != nil {
		raw, err := json.Marshal(current)
		if err != nil {
			failCode(c, http.StatusInternalServerError, "INTERNAL_ERROR", "配置序列化失败")
			return
		}
		if err := s.store.SaveEncryptedSetting(settingKeyNotify, string(raw), security.NotifySettingsAAD()); err != nil {
			failCode(c, http.StatusInternalServerError, "INTERNAL_ERROR", "保存加密通知配置失败")
			return
		}
	}

	s.notifier.UpdateSettings(current)

	// 返回脱敏后的响应视图
	feishuMasked := maskOutboundURLSecret(current.FeishuWebhook)
	barkMasked := maskOutboundURLSecret(current.BarkURL)
	tgMasked := maskTokenSecret(current.TelegramToken)

	ok(c, NotifySettingsResponse{
		FeishuConfigured:    current.FeishuWebhook != "",
		FeishuWebhookMasked: feishuMasked,
		FeishuWebhook:       feishuMasked,
		BarkConfigured:      current.BarkURL != "",
		BarkURLMasked:       barkMasked,
		BarkURL:             barkMasked,
		TelegramConfigured:  current.TelegramToken != "",
		TelegramTokenMasked: tgMasked,
		TelegramToken:       tgMasked,
		TelegramChat:        current.TelegramChat,
		EventKinds:          current.EventKinds,
		QuotaThreshold:      current.QuotaThreshold,
		ResendMinutes:       current.ResendMinutes,
	})
}

// testNotifyHandler 处理 POST /api/settings/notify/test, 同步向全部已配置渠道发送测试通知。
func (s *Server) testNotifyHandler(c *gin.Context) {
	results := s.notifier.SendTest()
	ok(c, gin.H{"results": results})
}

func maskTokenSecret(secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ""
	}
	if len(secret) <= 8 {
		return "********"
	}
	return secret[:4] + "****" + secret[len(secret)-4:]
}

func maskOutboundURLSecret(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	idx := strings.LastIndex(rawURL, "/")
	if idx >= 0 && idx < len(rawURL)-1 {
		prefix := rawURL[:idx+1]
		return prefix + "********"
	}
	return maskTokenSecret(rawURL)
}

type notifyConfigError struct{ msg string }

func (e *notifyConfigError) Error() string { return e.msg }

func errNotify(msg string) error { return &notifyConfigError{msg: msg} }
