/**
 * [INPUT]: 依赖 net/http, encoding/json, strings, gin, icloud-hme/internal/notify
 * [OUTPUT]: 对外提供 getNotifySettingsHandler, updateNotifySettingsHandler, testNotifyHandler
 * [POS]: server 的系统设置 HTTP Handlers，通知配置的读取、校验、持久化与一键测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/notify"
)

// settingKeyNotify 是通知配置在 store settings KV 中的键名。
const settingKeyNotify = "notify_settings"

// loadNotifySettings 从 store 读取通知配置;store 不可用或内容为空时返回默认配置。
func (s *Server) loadNotifySettings() notify.Settings {
	if s.store == nil {
		return notify.Settings{EventKinds: notify.DefaultEventKinds()}
	}
	raw := s.store.GetSetting(settingKeyNotify)
	if raw == "" {
		return notify.Settings{EventKinds: notify.DefaultEventKinds()}
	}
	var settings notify.Settings
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return notify.Settings{EventKinds: notify.DefaultEventKinds()}
	}
	if settings.EventKinds == nil {
		settings.EventKinds = notify.DefaultEventKinds()
	}
	return settings
}

// getNotifySettingsHandler 处理 GET /api/settings/notify。
func (s *Server) getNotifySettingsHandler(c *gin.Context) {
	ok(c, s.notifier.Settings())
}

// updateNotifySettingsHandler 处理 PUT /api/settings/notify。
func (s *Server) updateNotifySettingsHandler(c *gin.Context) {
	var settings notify.Settings
	if err := c.ShouldBindJSON(&settings); err != nil {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误")
		return
	}
	if err := validateNotifySettings(settings); err != nil {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	if settings.EventKinds == nil {
		settings.EventKinds = notify.DefaultEventKinds()
	}

	if s.store != nil {
		raw, err := json.Marshal(settings)
		if err != nil {
			failCode(c, http.StatusInternalServerError, "INTERNAL_ERROR", "配置序列化失败")
			return
		}
		if err := s.store.SaveSetting(settingKeyNotify, string(raw)); err != nil {
			failCode(c, http.StatusInternalServerError, "INTERNAL_ERROR", "保存通知配置失败")
			return
		}
	}
	s.notifier.UpdateSettings(settings)
	ok(c, s.notifier.Settings())
}

// testNotifyHandler 处理 POST /api/settings/notify/test, 同步向全部已配置渠道发送测试通知。
func (s *Server) testNotifyHandler(c *gin.Context) {
	results := s.notifier.SendTest()
	ok(c, gin.H{"results": results})
}

// validateNotifySettings 校验通知配置: URL 格式、Telegram 成对出现、阈值范围与事件开关白名单。
func validateNotifySettings(settings notify.Settings) error {
	if err := validateOutboundPublicURL(settings.FeishuWebhook, "飞书 Webhook"); err != nil {
		return err
	}
	if err := validateOutboundPublicURL(settings.BarkURL, "Bark 推送地址"); err != nil {
		return err
	}
	if (settings.TelegramToken == "") != (settings.TelegramChat == "") {
		return errNotify("Telegram Bot Token 与 Chat ID 必须同时填写")
	}
	if settings.QuotaThreshold < 0 || settings.QuotaThreshold > 2000 {
		return errNotify("配额水位阈值需在 0-2000 之间 (0 表示关闭)")
	}
	if settings.ResendMinutes < 0 || settings.ResendMinutes > 7*24*60 {
		return errNotify("重提醒间隔需在 0-10080 分钟之间")
	}
	for kind := range settings.EventKinds {
		switch kind {
		case notify.KindCookieExpired, notify.KindCookieRecovered, notify.KindQuotaLow:
		default:
			return errNotify("未知的事件类型: " + kind)
		}
	}
	return nil
}

type notifyConfigError struct{ msg string }

func (e *notifyConfigError) Error() string { return e.msg }

func errNotify(msg string) error { return &notifyConfigError{msg: msg} }
