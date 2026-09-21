/**
 * [INPUT]: 依赖 react, api/client 的 request/ApiError, api/types 的 NotifySettings/NotifyChannelResult, components/ToastProvider, components/icons
 * [OUTPUT]: 对外提供 SettingsPage 系统设置组件 (通知渠道配置、事件开关、配额阈值与一键测试推送)
 * [POS]: web/src/pages 的系统设置页面，通知配置随存随生效
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useEffect, useState } from 'react'
import { ApiError, request } from '../api/client'
import type { NotifyChannelResult, NotifySettings } from '../api/types'
import { useToast } from '../components/ToastProvider'
import { IconCheck, IconShield, IconSliders, IconZap } from '../components/icons'

const DEFAULT_SETTINGS: NotifySettings = {
  feishu_webhook: '',
  bark_url: '',
  telegram_token: '',
  telegram_chat: '',
  event_kinds: {
    cookie_expired: true,
    cookie_recovered: true,
    quota_low: true,
  },
  quota_threshold: 0,
}

const EVENT_ITEMS: Array<{ key: string; label: string; desc: string }> = [
  { key: 'cookie_expired', label: 'Cookie 失效', desc: '账号凭据失效被标记为 error 时推送' },
  { key: 'cookie_recovered', label: 'Cookie 恢复', desc: '失效账号校验恢复通过时推送' },
  { key: 'quota_low', label: '配额水位告警', desc: '活跃别名数首次越过阈值时推送' },
]

const CHANNEL_LABELS: Record<string, string> = {
  feishu: '飞书',
  bark: 'Bark',
  telegram: 'Telegram',
}

export default function SettingsPage() {
  const [settings, setSettings] = useState<NotifySettings>(DEFAULT_SETTINGS)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [testing, setTesting] = useState(false)
  const [error, setError] = useState('')
  const [testResults, setTestResults] = useState<NotifyChannelResult[] | null>(null)
  const { show } = useToast()

  useEffect(() => {
    let unmounted = false
    request<NotifySettings>('/api/settings/notify')
      .then((data) => {
        if (unmounted) return
        setSettings({ ...DEFAULT_SETTINGS, ...data })
      })
      .catch((err: unknown) => {
        if (!unmounted) {
          show(err instanceof Error ? err.message : '加载通知设置失败')
        }
      })
      .finally(() => {
        if (!unmounted) setLoading(false)
      })
    return () => {
      unmounted = true
    }
  }, [show])

  function update<K extends keyof NotifySettings>(key: K, value: NotifySettings[K]) {
    setSettings((prev) => ({ ...prev, [key]: value }))
  }

  function eventEnabled(key: string): boolean {
    if (!settings.event_kinds) return true
    return settings.event_kinds[key] ?? true
  }

  function toggleEvent(key: string) {
    const kinds = { ...(settings.event_kinds ?? {}) }
    kinds[key] = !eventEnabled(key)
    update('event_kinds', kinds)
  }

  async function handleSave() {
    if (saving) return
    setSaving(true)
    setError('')
    try {
      const saved = await request<NotifySettings>('/api/settings/notify', {
        method: 'PUT',
        body: JSON.stringify(settings),
      })
      setSettings({ ...DEFAULT_SETTINGS, ...saved })
      show('通知配置已保存')
    } catch (err) {
      const message = err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态'
      setError(message)
      show(message)
    } finally {
      setSaving(false)
    }
  }

  async function handleTest() {
    if (testing) return
    setTesting(true)
    setTestResults(null)
    try {
      const res = await request<{ results: NotifyChannelResult[] }>('/api/settings/notify/test', {
        method: 'POST',
        body: JSON.stringify({}),
      })
      setTestResults(res.results ?? [])
      const okCount = (res.results ?? []).filter((r) => r.ok).length
      if ((res.results ?? []).length === 0) {
        show('尚未配置任何通知渠道')
      } else if (okCount === (res.results ?? []).length) {
        show(`测试通知已发送 (${okCount} 个渠道成功)`)
      } else {
        show(`部分渠道发送失败 (${okCount}/${res.results?.length})`)
      }
    } catch (err) {
      const message = err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态'
      setError(message)
      show(message)
    } finally {
      setTesting(false)
    }
  }

  if (loading) {
    return (
      <div className="page-container">
        <p className="empty-state" aria-busy="true">加载中…</p>
      </div>
    )
  }

  return (
    <div className="page-container">
      <div className="settings-layout">
        <div className="page-header">
          <div>
            <h1 className="page-title">系统设置</h1>
            <p className="page-desc">通知渠道与告警策略，保存后立即生效，无需重启服务</p>
          </div>
        </div>

        {error && (
          <div className="alert-error" role="alert">
            {error}
          </div>
        )}

        {/* 卡片 1: 推送渠道配置 */}
        <div className="card">
          <div className="card-header">
            <h2 className="card-title">
              <IconZap size={16} />
              推送渠道配置
            </h2>
            <span className="card-badge">支持多渠道并行</span>
          </div>

          <div className="card-body">
            <div className="form-field">
              <label htmlFor="feishu_webhook">飞书自定义机器人 Webhook</label>
              <input
                id="feishu_webhook"
                className="input"
                type="text"
                placeholder="https://open.feishu.cn/open-apis/bot/v2/hook/xxxx"
                value={settings.feishu_webhook}
                onChange={(e) => update('feishu_webhook', e.target.value)}
                autoComplete="off"
              />
              <span className="hint">飞书群 → 设置 → 群机器人 → 添加自定义机器人，粘贴 Webhook 地址</span>
            </div>

            <div className="form-field">
              <label htmlFor="bark_url">Bark 推送地址 (iOS)</label>
              <input
                id="bark_url"
                className="input"
                type="text"
                placeholder="https://api.day.app/你的DeviceKey"
                value={settings.bark_url}
                onChange={(e) => update('bark_url', e.target.value)}
                autoComplete="off"
              />
              <span className="hint">App Store 安装 Bark 后复制推送 URL，保留到 DeviceKey 即可</span>
            </div>

            <div className="form-grid-2">
              <div className="form-field">
                <label htmlFor="telegram_token">Telegram Bot Token</label>
                <input
                  id="telegram_token"
                  className="input"
                  type="text"
                  placeholder="123456789:AAF..."
                  value={settings.telegram_token}
                  onChange={(e) => update('telegram_token', e.target.value)}
                  autoComplete="off"
                />
                <span className="hint">与 @BotFather 创建机器人获取；需能直连 api.telegram.org</span>
              </div>

              <div className="form-field">
                <label htmlFor="telegram_chat">Telegram Chat ID</label>
                <input
                  id="telegram_chat"
                  className="input"
                  type="text"
                  placeholder="如 123456789"
                  value={settings.telegram_chat}
                  onChange={(e) => update('telegram_chat', e.target.value)}
                  autoComplete="off"
                />
                <span className="hint">与 @userinfobot 对话可查询自己的 Chat ID</span>
              </div>
            </div>
          </div>
        </div>

        {/* 卡片 2: 告警策略与触发事件 */}
        <div className="card">
          <div className="card-header">
            <h2 className="card-title">
              <IconSliders size={16} />
              告警策略与触发事件
            </h2>
          </div>

          <div className="card-body">
            <div className="form-field">
              <div className="form-field-label">通知触发事件</div>
              <div className="notify-event-list">
                {EVENT_ITEMS.map((item) => (
                  <div key={item.key} className="notify-event-card">
                    <div className="notify-event-info">
                      <label htmlFor={`event_${item.key}`} className="notify-event-label">
                        {item.label}
                      </label>
                      <span className="notify-event-desc">{item.desc}</span>
                    </div>
                    <label className="switch" htmlFor={`event_${item.key}`}>
                      <input
                        id={`event_${item.key}`}
                        type="checkbox"
                        checked={eventEnabled(item.key)}
                        onChange={() => toggleEvent(item.key)}
                      />
                      <span className="slider" />
                      <span className="sr-only">切换</span>
                    </label>
                  </div>
                ))}
              </div>
            </div>

            <div className="form-field">
              <label htmlFor="quota_threshold">配额水位阈值 (活跃别名数)</label>
              <div className="input-with-suffix">
                <input
                  id="quota_threshold"
                  className="input"
                  type="number"
                  min={0}
                  max={2000}
                  value={settings.quota_threshold}
                  onChange={(e) => update('quota_threshold', Number(e.target.value))}
                />
                <span className="input-suffix">个别名</span>
              </div>
              <span className="hint">设为 0 表示关闭。活跃别名数首次越过阈值时推送告警 (每个账号每天最多提醒一次)</span>
            </div>
          </div>
        </div>

        {/* 操作区 */}
        <div className="settings-actions">
          <button
            type="button"
            className="btn btn-secondary"
            onClick={() => void handleTest()}
            disabled={testing}
          >
            <IconZap size={14} />
            {testing ? '发送中…' : '发送测试通知'}
          </button>
          <button
            type="button"
            className="btn btn-primary"
            onClick={() => void handleSave()}
            disabled={saving}
          >
            <IconCheck size={14} />
            {saving ? '保存中…' : '保存配置'}
          </button>
        </div>

        {/* 测试结果 */}
        {testResults && testResults.length > 0 && (
          <div className="card">
            <div className="card-header">
              <h3 className="card-title">
                <IconShield size={16} />
                测试推送诊断结果
              </h3>
            </div>
            <div className="card-body">
              <div className="notify-test-results">
                {testResults.map((r) => (
                  <div key={r.channel} className={`notify-test-row ${r.ok ? 'ok' : 'fail'}`}>
                    <span className="channel-badge">{CHANNEL_LABELS[r.channel] ?? r.channel}</span>
                    <span className="notify-test-status">
                      {r.ok ? (
                        <>
                          <IconCheck size={14} /> 发送成功
                        </>
                      ) : (
                        <>发送失败{r.error ? `: ${r.error}` : ''}</>
                      )}
                    </span>
                  </div>
                ))}
              </div>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
