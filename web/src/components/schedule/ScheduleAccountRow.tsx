/**
 * [INPUT]: 依赖 api/types 的 AccountSummary/ScheduleConfig, components/Select 的 Select/SelectOption
 * [OUTPUT]: 对外提供 ScheduleAccountRow 单行账号调度策略配置组件 (React.memo 隔离重绘)
 * [POS]: web/src/components/schedule 的表格行组件，内聚配额与模板草稿状态，杜绝轮询干扰用户输入
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { memo, useEffect, useState } from 'react'
import type { AccountSummary, ScheduleConfig } from '../../api/types'
import Select, { type SelectOption } from '../Select'

const MODE_OPTIONS: SelectOption[] = [
  { value: 'always', label: '全天常驻 (24h)' },
  { value: 'daily_window', label: '每日时段窗口' },
  { value: 'duration', label: '持续时长 (自动停机)' },
]

interface ScheduleAccountRowProps {
  acc: AccountSummary
  cfg: ScheduleConfig
  onToggle: (acc: AccountSummary) => Promise<void>
  onUpdateQuota: (accountId: string, quota: number) => Promise<void>
  onUpdateLabel: (accountId: string, label: string) => Promise<void>
  onUpdateMode: (accountId: string, patch: Partial<ScheduleConfig>) => Promise<void>
  onApplyPreset: (accountId: string, preset: string) => void
}

export const ScheduleAccountRow = memo(function ScheduleAccountRow({
  acc,
  cfg,
  onToggle,
  onUpdateQuota,
  onUpdateLabel,
  onUpdateMode,
  onApplyPreset,
}: ScheduleAccountRowProps) {
  // 单行内聚草稿状态，防止全局 3 秒轮询重绘打断用户输入
  const [quotaInput, setQuotaInput] = useState(String(cfg.hourly_quota))
  const [isEditingQuota, setIsEditingQuota] = useState(false)

  const [labelInput, setLabelInput] = useState(cfg.alias_label || 'scheduled')
  const [isEditingLabel, setIsEditingLabel] = useState(false)

  useEffect(() => {
    if (!isEditingQuota) {
      setQuotaInput(String(cfg.hourly_quota))
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cfg.hourly_quota])

  useEffect(() => {
    if (!isEditingLabel) {
      setLabelInput(cfg.alias_label || 'scheduled')
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cfg.alias_label])

  const meterClass =
    cfg.enabled && cfg.hourly_quota > 0 && cfg.current_hour_count >= cfg.hourly_quota
      ? ' meter-full'
      : cfg.enabled && cfg.hourly_quota > 0 && cfg.current_hour_count * 4 >= cfg.hourly_quota * 3
        ? ' meter-warn'
        : ''

  const meterWidth =
    cfg.enabled && cfg.hourly_quota > 0
      ? Math.min(100, Math.round((cfg.current_hour_count / cfg.hourly_quota) * 100))
      : 0

  return (
    <tr>
      <td>
        <div className="schedule-acc-cell-compact">
          <div className="schedule-acc-top">
            <span className="schedule-acc-name">{acc.name || acc.real_email}</span>
            {acc.name && <span className="schedule-acc-email-dim">({acc.real_email})</span>}
          </div>
          <div className="schedule-acc-progress-inline">
            <div className={`schedule-acc-meter schedule-acc-meter-mini${meterClass}`} aria-hidden="true">
              <span style={{ width: `${meterWidth}%` }} />
            </div>
            <span className="schedule-acc-count-text">
              已产出 <b className="schedule-count-highlight">{cfg.current_hour_count}</b> / {cfg.hourly_quota} 个
            </span>
          </div>
        </div>
      </td>
      <td style={{ textAlign: 'center' }}>
        <label className="switch" title={cfg.enabled ? '自动补货已开启' : '自动补货已暂停'}>
          <input
            type="checkbox"
            checked={cfg.enabled}
            aria-label={`账号 ${acc.name || acc.real_email} 自动补货开关`}
            onChange={() => void onToggle(acc)}
          />
          <span className="slider" />
        </label>
      </td>
      <td style={{ textAlign: 'center' }}>
        <div className="schedule-quota-box">
          <input
            type="number"
            min="1"
            max="30"
            className="schedule-quota-input"
            value={quotaInput}
            onFocus={() => setIsEditingQuota(true)}
            onChange={(e) => setQuotaInput(e.target.value)}
            onBlur={(e) => {
              setIsEditingQuota(false)
              const raw = e.target.value
              if (raw.trim() === '') {
                setQuotaInput(String(cfg.hourly_quota))
                return
              }
              const safe = Math.max(1, Math.min(30, Number(raw) || 1))
              setQuotaInput(String(safe))
              if (safe !== cfg.hourly_quota) {
                void onUpdateQuota(acc.id, safe)
              }
            }}
          />
        </div>
      </td>
      <td>
        <div className="schedule-mode-box">
          <div className="schedule-mode-select-row">
            <Select
              size="sm"
              value={cfg.mode || 'always'}
              onChange={(val) => {
                const m = val as 'always' | 'daily_window' | 'duration'
                void onUpdateMode(acc.id, {
                  mode: m,
                  start_time: cfg.start_time || '09:00',
                  end_time: cfg.end_time || '18:00',
                  duration_hours: cfg.duration_hours || 12,
                  started_at:
                    m === 'duration' && cfg.enabled ? new Date().toISOString() : cfg.started_at,
                })
              }}
              options={MODE_OPTIONS}
              aria-label="调度模式"
            />
          </div>
          {cfg.mode === 'daily_window' && (
            <div className="schedule-window-inputs">
              <input
                type="time"
                className="schedule-time-input"
                value={cfg.start_time || '09:00'}
                onChange={(e) => {
                  void onUpdateMode(acc.id, { start_time: e.target.value })
                }}
                title="开始时间"
              />
              <span className="schedule-window-sep">~</span>
              <input
                type="time"
                className="schedule-time-input"
                value={cfg.end_time || '18:00'}
                onChange={(e) => {
                  void onUpdateMode(acc.id, { end_time: e.target.value })
                }}
                title="结束时间"
              />
            </div>
          )}
          {cfg.mode === 'duration' && (
            <div className="schedule-duration-inputs">
              <span className="schedule-duration-label">持续</span>
              <input
                type="number"
                min="1"
                max="72"
                className="schedule-duration-input"
                value={cfg.duration_hours || 12}
                onChange={(e) => {
                  const num = Math.max(1, Math.min(72, Number(e.target.value) || 1))
                  void onUpdateMode(acc.id, { duration_hours: num })
                }}
              />
              <span className="schedule-duration-unit">小时后停机</span>
            </div>
          )}
        </div>
      </td>
      <td>
        <div className="schedule-label-row-compact">
          <div
            className="schedule-label-box-compact"
            title="支持宏变量: {date} 日期、{time} 时分、{seq} 序号、{account} 账号名"
          >
            <input
              type="text"
              maxLength={100}
              className="schedule-label-input"
              placeholder="默认 scheduled"
              value={labelInput}
              onFocus={() => setIsEditingLabel(true)}
              onChange={(e) => setLabelInput(e.target.value)}
              onBlur={(e) => {
                setIsEditingLabel(false)
                const raw = e.target.value.trim()
                const safe = raw === '' ? 'scheduled' : raw
                setLabelInput(safe)
                if (safe !== (cfg.alias_label || 'scheduled')) {
                  void onUpdateLabel(acc.id, safe)
                }
              }}
            />
          </div>
          <div className="schedule-preset-chips-inline">
            <button
              type="button"
              className={`schedule-preset-chip ${labelInput === 'gpt' ? 'active' : ''}`}
              onClick={() => {
                setLabelInput('gpt')
                onApplyPreset(acc.id, 'gpt')
              }}
              title="快速设为 gpt"
            >
              gpt
            </button>
            <button
              type="button"
              className={`schedule-preset-chip ${labelInput === 'gpt-{seq}' ? 'active' : ''}`}
              onClick={() => {
                setLabelInput('gpt-{seq}')
                onApplyPreset(acc.id, 'gpt-{seq}')
              }}
              title="带轮次序号: gpt-1, gpt-2..."
            >
              gpt-&#123;seq&#125;
            </button>
            <button
              type="button"
              className={`schedule-preset-chip ${labelInput === 'gpt-{date}' ? 'active' : ''}`}
              onClick={() => {
                setLabelInput('gpt-{date}')
                onApplyPreset(acc.id, 'gpt-{date}')
              }}
              title="带年月日: gpt-20260920"
            >
              gpt-&#123;date&#125;
            </button>
            <button
              type="button"
              className={`schedule-preset-chip ${(labelInput === 'scheduled' || !labelInput) ? 'active' : ''}`}
              onClick={() => {
                setLabelInput('scheduled')
                onApplyPreset(acc.id, 'scheduled')
              }}
              title="恢复默认 scheduled"
            >
              默认
            </button>
          </div>
        </div>
      </td>
    </tr>
  )
})
