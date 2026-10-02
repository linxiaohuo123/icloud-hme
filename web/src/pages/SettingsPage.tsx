/**
 * [INPUT]: 依赖 react, api/client 的 request/ApiError, api/types 的 NotifyChannelResult/NotifySettingsResponse/UpdateNotifySettingsRequest, components/ToastProvider, components/icons
 * [OUTPUT]: 对外提供 SettingsPage 系统设置组件 (通知渠道配置、读取失败重试与保存保护、事件开关、配额阈值与一键测试推送)；左说明右控件的设置行布局 + 底部常驻保存栏
 * [POS]: web/src/pages 的系统设置页面，通知配置脱敏显示、按需安全更新与 Fail-Closed 契约对齐
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useEffect, useState } from 'react'
import { ApiError, request } from '../api/client'
import type {
  NotifyChannelResult,
  NotifySettingsResponse,
  UpdateNotifySettingsRequest,
} from '../api/types'
import { useToast } from '../components/ToastProvider'
import { IconCheck, IconShield, IconSliders, IconZap } from '../components/icons'

const DEFAULT_EVENT_KINDS: Record<string, boolean> = {
  cookie_expired: true,
  cookie_recovered: true,
  quota_low: true,
  mail_failed: true,
  mail_recovered: true,
}

const EVENT_ITEMS: Array<{ key: string; label: string; desc: string }> = [
  { key: 'cookie_expired', label: 'Cookie 失效', desc: '账号凭据失效被标记为 error 时推送' },
  { key: 'cookie_recovered', label: 'Cookie 恢复', desc: '失效账号校验恢复通过时推送' },
  { key: 'quota_low', label: '配额水位告警', desc: '活跃别名数首次越过阈值时推送' },
  { key: 'mail_failed', label: '收信邮箱故障', desc: '收信邮箱连续认证失败、账号暂停出号时推送' },
  { key: 'mail_recovered', label: '收信邮箱恢复', desc: '故障邮箱认证恢复、账号重新出号时推送' },
]

const CHANNEL_LABELS: Record<string, string> = {
  feishu: '飞书',
  bark: 'Bark',
  telegram: 'Telegram',
}

export default function SettingsPage() {
  const [serverSettings, setServerSettings] = useState<NotifySettingsResponse | null>(null)
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState('')
  const [loadRetryKey, setLoadRetryKey] = useState(0)
  const [saving, setSaving] = useState(false)
  const [testing, setTesting] = useState(false)
  const [error, setError] = useState('')
  const [testResults, setTestResults] = useState<NotifyChannelResult[] | null>(null)

  // Camoufox 代理连通性状态
  const [camoufoxInfo, setCamoufoxInfo] = useState<{
    url: string
    available: boolean
    ready: boolean
    latency_ms: number
    status_text: string
    error?: string
  } | null>(null)
  const [camoufoxTesting, setCamoufoxTesting] = useState(false)
  const [camoufoxTestResult, setCamoufoxTestResult] = useState<{
    success: boolean
    url: string
    latency_ms: number
    camoufox_ready?: boolean
    message: string
  } | null>(null)

  // 渠道 Secret 输入与清除状态 (输入框初始均为空，绝不以脱敏掩码作为输入值)
  const [feishuInput, setFeishuInput] = useState('')
  const [clearFeishu, setClearFeishu] = useState(false)

  const [barkInput, setBarkInput] = useState('')
  const [clearBark, setClearBark] = useState(false)

  const [telegramTokenInput, setTelegramTokenInput] = useState('')
  const [clearTelegram, setClearTelegram] = useState(false)
  const [telegramChatInput, setTelegramChatInput] = useState('')

  // 策略与开关状态
  const [eventKinds, setEventKinds] = useState<Record<string, boolean>>(DEFAULT_EVENT_KINDS)
  const [quotaThreshold, setQuotaThreshold] = useState<number>(0)

  const { show } = useToast()

  useEffect(() => {
    let unmounted = false
    setLoading(true)
    setLoadError('')
    request<NotifySettingsResponse>('/api/settings/notify')
      .then((data) => {
        if (unmounted) return
        setServerSettings(data)
        setEventKinds(data.event_kinds || DEFAULT_EVENT_KINDS)
        setQuotaThreshold(data.quota_threshold ?? 0)
        setTelegramChatInput(data.telegram_chat || '')
      })
      .catch((err: unknown) => {
        if (!unmounted) {
          setLoadError(err instanceof Error ? err.message : '加载通知设置失败')
        }
      })
      .finally(() => {
        if (!unmounted) setLoading(false)
      })

    return () => {
      unmounted = true
    }
  }, [loadRetryKey])

  useEffect(() => {
    let unmounted = false
    // 读取 Camoufox 状态
    request<{
      url: string
      available: boolean
      ready: boolean
      latency_ms: number
      status_text: string
      error?: string
    }>('/api/settings/camoufox')
      .then((data) => {
        if (!unmounted) setCamoufoxInfo(data)
      })
      .catch(() => {})

    return () => {
      unmounted = true
    }
  }, [])

  async function handleTestCamoufox() {
    if (camoufoxTesting) return
    setCamoufoxTesting(true)
    setCamoufoxTestResult(null)
    try {
      const res = await request<{
        success: boolean
        url: string
        latency_ms: number
        camoufox_ready?: boolean
        message: string
      }>('/api/settings/camoufox/test', {
        method: 'POST',
      })
      setCamoufoxTestResult(res)
      if (res.success) {
        show(`Camoufox 连通成功！延迟 ${res.latency_ms}ms`)
        setCamoufoxInfo((prev) =>
          prev
            ? {
                ...prev,
                available: true,
                ready: !!res.camoufox_ready,
                status_text: '在线就绪',
                latency_ms: res.latency_ms,
              }
            : null
        )
      } else {
        show(res.message || '连接失败')
      }
    } catch (err) {
      const msg = err instanceof ApiError ? err.message : '连接异常'
      setCamoufoxTestResult({
        success: false,
        url: camoufoxInfo?.url || 'http://127.0.0.1:8089',
        latency_ms: 0,
        message: msg,
      })
      show(msg)
    } finally {
      setCamoufoxTesting(false)
    }
  }

  function eventEnabled(key: string): boolean {
    return eventKinds[key] ?? true
  }

  function toggleEvent(key: string) {
    setEventKinds((prev) => ({
      ...prev,
      [key]: !eventEnabled(key),
    }))
  }

  async function handleSave() {
    if (saving || testing || loading || loadError || !serverSettings) return
    setSaving(true)
    setError('')
    try {
      const payload: UpdateNotifySettingsRequest = {
        event_kinds: eventKinds,
        quota_threshold: quotaThreshold,
      }

      if (clearFeishu) {
        payload.clear_feishu = true
      } else if (feishuInput.trim()) {
        payload.feishu_webhook = feishuInput.trim()
      }

      if (clearBark) {
        payload.clear_bark = true
      } else if (barkInput.trim()) {
        payload.bark_url = barkInput.trim()
      }

      if (clearTelegram) {
        payload.clear_telegram = true
      } else {
        if (telegramTokenInput.trim()) {
          payload.telegram_token = telegramTokenInput.trim()
        }
        if (telegramChatInput.trim() !== (serverSettings?.telegram_chat ?? '')) {
          payload.telegram_chat = telegramChatInput.trim()
        }
      }

      const saved = await request<NotifySettingsResponse>('/api/settings/notify', {
        method: 'PUT',
        body: JSON.stringify(payload),
      })

      setServerSettings(saved)
      setFeishuInput('')
      setClearFeishu(false)
      setBarkInput('')
      setClearBark(false)
      setTelegramTokenInput('')
      setClearTelegram(false)
      setTelegramChatInput(saved.telegram_chat || '')
      setEventKinds(saved.event_kinds || DEFAULT_EVENT_KINDS)
      setQuotaThreshold(saved.quota_threshold ?? 0)

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
    if (testing || saving) return
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

  if (loadError) {
    return (
      <div className="page-container">
        <h1 className="page-title">系统设置</h1>
        <div className="alert-error" role="alert">{loadError}</div>
        <button type="button" className="btn btn-secondary" onClick={() => setLoadRetryKey((key) => key + 1)}>
          重试
        </button>
      </div>
    )
  }

  // 已配置渠道的状态行：脱敏值 + 清除/撤销，置于输入框下方
  function renderChannelStatus(opts: {
    configured: boolean | undefined
    masked: string | undefined
    cleared: boolean
    clearLabel: string
    onClear: () => void
    onUndo: () => void
  }) {
    if (!opts.configured) return null
    return (
      <div className="settings-field-status">
        {opts.cleared ? (
          <>
            <span className="badge badge-error">已标记清除</span>
            <button type="button" className="btn btn-xs btn-secondary" disabled={saving} onClick={opts.onUndo}>
              撤销清除
            </button>
          </>
        ) : (
          <>
            <span className="badge badge-active" title={opts.masked}>
              已配置: {opts.masked || '已设置'}
            </span>
            <button type="button" className="btn btn-xs btn-ghost-danger" disabled={saving} onClick={opts.onClear}>
              {opts.clearLabel}
            </button>
          </>
        )}
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

        {/* 分组 1: 推送渠道 */}
        <section className="card settings-section">
          <div className="card-header">
            <h2 className="card-title">
              <IconZap size={16} />
              推送渠道配置
            </h2>
            <span className="settings-section-note">支持多渠道并行</span>
          </div>

          <div className="settings-rows">
            <div className="settings-row">
              <div className="settings-row-label">
                <label htmlFor="feishu_webhook">飞书自定义机器人 Webhook</label>
                <span className="hint">飞书群 → 设置 → 群机器人 → 添加自定义机器人，粘贴 Webhook 地址</span>
              </div>
              <div className="settings-row-control">
                <input
                  id="feishu_webhook"
                  className="input"
                  type="text"
                  placeholder={
                    serverSettings?.feishu_configured && !clearFeishu
                      ? '留空保持已配置 Webhook，或输入新地址覆盖'
                      : 'https://open.feishu.cn/open-apis/bot/v2/hook/xxxx'
                  }
                  value={feishuInput}
                  onChange={(e) => {
                    setFeishuInput(e.target.value)
                    if (e.target.value.trim()) {
                      setClearFeishu(false)
                    }
                  }}
                  autoComplete="off"
                  disabled={saving}
                />
                {renderChannelStatus({
                  configured: serverSettings?.feishu_configured,
                  masked: serverSettings?.feishu_webhook_masked,
                  cleared: clearFeishu,
                  clearLabel: '清除配置',
                  onClear: () => {
                    setClearFeishu(true)
                    setFeishuInput('')
                  },
                  onUndo: () => setClearFeishu(false),
                })}
              </div>
            </div>

            <div className="settings-row">
              <div className="settings-row-label">
                <label htmlFor="bark_url">Bark 推送地址 (iOS)</label>
                <span className="hint">App Store 安装 Bark 后复制推送 URL，保留到 DeviceKey 即可</span>
              </div>
              <div className="settings-row-control">
                <input
                  id="bark_url"
                  className="input"
                  type="text"
                  placeholder={
                    serverSettings?.bark_configured && !clearBark
                      ? '留空保持已配置地址，或输入新地址覆盖'
                      : 'https://api.day.app/你的DeviceKey'
                  }
                  value={barkInput}
                  onChange={(e) => {
                    setBarkInput(e.target.value)
                    if (e.target.value.trim()) {
                      setClearBark(false)
                    }
                  }}
                  autoComplete="off"
                  disabled={saving}
                />
                {renderChannelStatus({
                  configured: serverSettings?.bark_configured,
                  masked: serverSettings?.bark_url_masked,
                  cleared: clearBark,
                  clearLabel: '清除配置',
                  onClear: () => {
                    setClearBark(true)
                    setBarkInput('')
                  },
                  onUndo: () => setClearBark(false),
                })}
              </div>
            </div>

            <div className="settings-row">
              <div className="settings-row-label">
                <label htmlFor="telegram_token">Telegram Bot Token</label>
                <span className="hint">与 @BotFather 创建机器人获取；需能直连 api.telegram.org</span>
              </div>
              <div className="settings-row-control">
                <input
                  id="telegram_token"
                  className="input"
                  type="text"
                  placeholder={
                    serverSettings?.telegram_configured && !clearTelegram
                      ? '留空保持已配置 Token，或输入新 Token 覆盖'
                      : '123456789:AAF...'
                  }
                  value={telegramTokenInput}
                  onChange={(e) => {
                    setTelegramTokenInput(e.target.value)
                    if (e.target.value.trim()) {
                      setClearTelegram(false)
                    }
                  }}
                  autoComplete="off"
                  disabled={saving}
                />
                {renderChannelStatus({
                  configured: serverSettings?.telegram_configured,
                  masked: serverSettings?.telegram_token_masked,
                  cleared: clearTelegram,
                  clearLabel: '清除',
                  onClear: () => {
                    setClearTelegram(true)
                    setTelegramTokenInput('')
                  },
                  onUndo: () => setClearTelegram(false),
                })}
              </div>
            </div>

            <div className="settings-row">
              <div className="settings-row-label">
                <label htmlFor="telegram_chat">Telegram Chat ID</label>
                <span className="hint">与 @userinfobot 对话可查询自己的 Chat ID</span>
              </div>
              <div className="settings-row-control">
                <input
                  id="telegram_chat"
                  className="input"
                  type="text"
                  placeholder="如 123456789"
                  value={telegramChatInput}
                  onChange={(e) => setTelegramChatInput(e.target.value)}
                  autoComplete="off"
                  disabled={clearTelegram || saving}
                />
              </div>
            </div>
          </div>
        </section>

        {/* 分组 2: 告警策略 */}
        <section className="card settings-section">
          <div className="card-header">
            <h2 className="card-title">
              <IconSliders size={16} />
              告警策略与触发事件
            </h2>
          </div>

          <div className="settings-rows">
            {EVENT_ITEMS.map((item) => (
              <div key={item.key} className="settings-row is-toggle">
                <div className="settings-row-label">
                  <label htmlFor={`event_${item.key}`} className="notify-event-label">
                    {item.label}
                  </label>
                  <span className="hint">{item.desc}</span>
                </div>
                <div className="settings-row-control is-end">
                  <label className="switch" htmlFor={`event_${item.key}`}>
                    <input
                      id={`event_${item.key}`}
                      type="checkbox"
                      disabled={saving}
                      checked={eventEnabled(item.key)}
                      onChange={() => toggleEvent(item.key)}
                    />
                    <span className="slider" />
                    <span className="sr-only">切换</span>
                  </label>
                </div>
              </div>
            ))}

            <div className="settings-row">
              <div className="settings-row-label">
                <label htmlFor="quota_threshold">配额水位阈值 (活跃别名数)</label>
                <span className="hint">设为 0 表示关闭。活跃别名数首次越过阈值时推送告警 (每个账号每天最多提醒一次)</span>
              </div>
              <div className="settings-row-control">
                <div className="input-with-suffix settings-number">
                  <input
                    id="quota_threshold"
                    className="input"
                    type="number"
                    min={0}
                    max={2000}
                    disabled={saving}
                    value={quotaThreshold}
                    onChange={(e) => setQuotaThreshold(Number(e.target.value))}
                  />
                  <span className="input-suffix">个别名</span>
                </div>
              </div>
            </div>
          </div>
        </section>

        {/* 分组 3: Camoufox 自动化上号代理 */}
        <section className="card settings-section">
          <div className="card-header">
            <h2 className="card-title">
              <IconShield size={16} />
              Camoufox 自动化上号代理
            </h2>
            {camoufoxInfo?.available ? (
              <span className="status-pill active">
                <span className="status-dot" />
                在线就绪 ({camoufoxInfo.latency_ms}ms)
              </span>
            ) : (
              <span className="status-pill error">
                <span className="status-dot" />
                离线 / 未连接
              </span>
            )}
          </div>

          <div className="settings-rows">
            <div className="settings-row">
              <div className="settings-row-label">
                <label htmlFor="camoufox-agent-url">代理服务地址</label>
                <span className="hint">
                  可通过环境变量 <code>ICLOUD_HME_CAMOUFOX_URL</code> 自定义；在账号管理中进行 Apple ID 授权登录时自动使用。
                </span>
              </div>
              <div className="settings-row-control">
                <div className="input-row">
                  <input
                    id="camoufox-agent-url"
                    className="input font-mono"
                    type="text"
                    readOnly
                    value={camoufoxInfo?.url || 'http://127.0.0.1:8089'}
                  />
                  <button
                    type="button"
                    className="btn btn-secondary"
                    onClick={() => void handleTestCamoufox()}
                    disabled={camoufoxTesting}
                  >
                    <IconZap size={14} />
                    {camoufoxTesting ? '正在检测连通性…' : '测试代理连通性'}
                  </button>
                </div>

                {camoufoxTestResult && (
                  <div className={`notify-test-row ${camoufoxTestResult.success ? 'ok' : 'fail'}`}>
                    <span className="channel-badge">Camoufox Agent</span>
                    <span className="notify-test-status">
                      {camoufoxTestResult.success ? (
                        <>
                          <IconCheck size={14} /> 连通成功 (延迟 {camoufoxTestResult.latency_ms}ms, 内核就绪: {camoufoxTestResult.camoufox_ready ? '是' : '否'})
                        </>
                      ) : (
                        <>{camoufoxTestResult.message}</>
                      )}
                    </span>
                  </div>
                )}

                {!camoufoxInfo?.available && !camoufoxTestResult?.success && (
                  <div className="alert-info">
                    <strong>提示</strong>：在项目根目录配置 <code>ICLOUD_HME_CAMOUFOX_TOKEN</code> 后运行 <code>docker compose up -d --build</code>；本地 Windows 可运行 <code>scripts\camoufox-agent\start_agent.bat</code>。
                  </div>
                )}
              </div>
            </div>
          </div>
        </section>

        {/* 测试结果 */}
        {testResults && testResults.length > 0 && (
          <section className="card settings-section">
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
          </section>
        )}

        {/* 底部保存栏：滚动时始终可见 */}
        <div className="settings-savebar">
          <span className="settings-savebar-hint">修改通知渠道或告警策略后，保存即可立即生效</span>
          <div className="settings-actions">
            <button
              type="button"
              className="btn btn-secondary"
              onClick={() => void handleTest()}
              disabled={testing || saving}
            >
              <IconZap size={14} />
              {testing ? '发送中…' : '发送测试通知'}
            </button>
            <button
              type="button"
              className="btn btn-primary"
              onClick={() => void handleSave()}
              disabled={saving || testing || !serverSettings}
            >
              <IconCheck size={14} />
              {saving ? '保存中…' : '保存配置'}
            </button>
          </div>
        </div>
      </div>
    </div>
  )
}
