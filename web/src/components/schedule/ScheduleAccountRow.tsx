/**
 * [INPUT]: api/types 的账号和配置，Select、ScheduleDraftInput 与字段补丁保存回调
 * [OUTPUT]: ScheduleAccountRow，提供保护状态、真实启用开关、行级保存反馈和独立重新计时入口
 * [POS]: schedule 分页表格行；服务端值为真相源，仅聚焦/保存中的字段持有草稿
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */
import { memo, useRef, useState } from 'react'
import type { AccountSummary, ScheduleConfig } from '../../api/types'
import Select, { type SelectOption } from '../Select'
import { ScheduleDraftInput } from './ScheduleDraftInput'

const MODE_OPTIONS: SelectOption[] = [
  { value: 'always', label: '全天常驻 (24h)' },
  { value: 'daily_window', label: '每日时段窗口' },
  { value: 'duration', label: '持续时长 (自动停机)' },
]
const PRESETS = [
  { value: 'gpt', label: 'gpt', title: '快速设为 gpt' },
  { value: 'gpt-{seq}', label: 'gpt-{seq}', title: '本轮账号内序号；每轮创建一个时为 gpt-1' },
  { value: 'gpt-{date}', label: 'gpt-{date}', title: '带年月日: gpt-20260920' },
  { value: 'scheduled', label: '默认', title: '恢复默认 scheduled' },
]
interface ScheduleAccountRowProps {
  acc: AccountSummary
  cfg: ScheduleConfig
  disabled: boolean
  saving: boolean
  expired: boolean
  onToggle: (acc: AccountSummary) => Promise<boolean>
  onUpdateQuota: (accountId: string, quota: number) => Promise<boolean>
  onUpdateLabel: (accountId: string, label: string) => Promise<boolean>
  onUpdateMode: (accountId: string, patch: Partial<ScheduleConfig>) => Promise<boolean>
}

export const ScheduleAccountRow = memo(function ScheduleAccountRow({
  acc, cfg, disabled, saving, expired, onToggle, onUpdateQuota, onUpdateLabel, onUpdateMode,
}: ScheduleAccountRowProps) {
  const [pendingAction, setPendingAction] = useState<'toggle' | 'mode' | 'preset' | 'restart' | null>(null)
  const pendingActionRef = useRef(false)
  const name = acc.name || acc.real_email
  const enabled = cfg.enabled
  const protectedAccount = acc.schedule_protected === true
  const editingDisabled = disabled || protectedAccount
  const label = cfg.alias_label || 'scheduled'
  const duration = String(cfg.duration_hours || 12)
  const start = cfg.start_time || '09:00'
  const end = cfg.end_time || '18:00'
  const action = async (kind: 'toggle' | 'mode' | 'preset' | 'restart', save: () => Promise<boolean>) => {
    if (pendingActionRef.current) return
    pendingActionRef.current = true
    setPendingAction(kind)
    try { await save() } finally { pendingActionRef.current = false; setPendingAction(null) }
  }
  const normalizeNumber = (raw: string, previous: string, max: number) =>
    raw.trim() === '' ? previous : String(Math.max(1, Math.min(max, Math.trunc(Number(raw)) || 1)))
  const meterWidth = enabled && !expired && !protectedAccount && cfg.hourly_quota > 0 ? Math.min(100, Math.round(cfg.current_hour_count / cfg.hourly_quota * 100)) : 0
  const meterClass = meterWidth >= 75 && meterWidth < 100 ? ' meter-warn' : ''
  return <tr aria-busy={saving}>
    <td>
      <div className="schedule-acc-cell-compact">
        <div className="schedule-acc-top">
          <span className="schedule-acc-name" title={name}>{name}</span>
          {acc.name && <span className="schedule-acc-email-dim" title={acc.real_email}>{acc.real_email}</span>}
        </div>
        <div className="schedule-acc-progress-inline">
          <div className={`schedule-acc-meter schedule-acc-meter-mini${meterClass}`} aria-hidden="true"><span style={{ width: `${meterWidth}%` }} /></div>
          <span className="schedule-acc-count-text">本小时 <b className="schedule-count-highlight">{cfg.current_hour_count}</b> / {cfg.hourly_quota} 个</span>
        </div>
        <span className="schedule-row-status" role="status">{saving ? '正在保存…' : protectedAccount ? '受保护，不参与自动或手动补货' : disabled ? '配置尚未确认' : !enabled ? '已暂停' : expired ? '已到期，自动补货已停止；手动仍可执行' : ''}</span>
      </div>
    </td>
    <td className="schedule-center-cell">
      <label className="switch" title={protectedAccount ? '受保护账号不参与补货' : expired && enabled ? '已到期，关闭开关可暂停该任务' : cfg.enabled ? '自动补货已开启' : '自动补货已暂停'}>
        <input type="checkbox" checked={enabled} disabled={editingDisabled || pendingAction !== null}
          aria-label={`账号 ${name} 自动补货开关`} onChange={() => void action('toggle', () => onToggle(acc))} />
        <span className="slider" />
      </label>
    </td>
    <td className="schedule-center-cell">
      <div className="schedule-quota-box"><ScheduleDraftInput type="number" min="1" max="30" step="1" className="schedule-quota-input"
        aria-label={`账号 ${name} 每小时配额`} value={String(cfg.hourly_quota)} disabled={editingDisabled}
        normalize={(raw) => normalizeNumber(raw, String(cfg.hourly_quota), 30)} onSave={(value) => onUpdateQuota(acc.id, Number(value))} /></div>
    </td>
    <td>
      <div className="schedule-mode-box"><div className="schedule-mode-select-row">
        <Select size="sm" value={cfg.mode || 'always'} disabled={editingDisabled || pendingAction !== null} aria-label={`账号 ${name} 调度模式`} triggerLabel={`账号 ${name} 调度模式`}
          onChange={(mode) => void action('mode', () => onUpdateMode(acc.id, { mode: mode as ScheduleConfig['mode'] }))} options={MODE_OPTIONS} />
      </div>
      {cfg.mode === 'daily_window' && <div className="schedule-window-inputs">
        <ScheduleDraftInput type="time" className="schedule-time-input" title="开始时间" aria-label={`账号 ${name} 开始时间`} value={start} disabled={editingDisabled}
          normalize={(raw) => raw || start} onSave={(value) => onUpdateMode(acc.id, { start_time: value })} />
        <span className="schedule-window-sep">~</span>
        <ScheduleDraftInput type="time" className="schedule-time-input" title="结束时间" aria-label={`账号 ${name} 结束时间`} value={end} disabled={editingDisabled}
          normalize={(raw) => raw || end} onSave={(value) => onUpdateMode(acc.id, { end_time: value })} />
      </div>}
      {cfg.mode === 'duration' && <div className="schedule-duration-inputs"><span className="schedule-duration-label">持续</span>
        <ScheduleDraftInput type="number" min="1" max="72" step="1" className="schedule-duration-input" aria-label={`账号 ${name} 持续小时数`} value={duration} disabled={editingDisabled}
          normalize={(raw) => normalizeNumber(raw, duration, 72)} onSave={(value) => onUpdateMode(acc.id, { duration_hours: Number(value) })} />
        <span className="schedule-duration-unit">小时后停机</span>
      </div>}
      {expired && enabled && !protectedAccount && <button type="button" className="btn btn-sm btn-secondary"
        disabled={editingDisabled || pendingAction !== null} aria-label={`账号 ${name} 重新计时`}
        onClick={() => void action('restart', () => onUpdateMode(acc.id, { mode: 'duration' }))}>重新计时</button>}
      </div>
    </td>
    <td>
      <div className="schedule-label-row-compact">
        <div className="schedule-label-box-compact" title="支持宏变量: {date} 日期、{time} 时分、{seq} 本轮序号、{account} 账号名">
          <ScheduleDraftInput type="text" maxLength={100} className="schedule-label-input" aria-label={`账号 ${name} 别名备注模板`}
            value={label} disabled={editingDisabled || pendingAction === 'preset'} placeholder="默认 scheduled"
            normalize={(raw) => raw.trim() || 'scheduled'} onSave={(value) => onUpdateLabel(acc.id, value)} />
        </div>
        <div className="schedule-preset-chips-inline">{PRESETS.map((preset) => <button key={preset.value} type="button"
          className={`schedule-preset-chip ${label === preset.value ? 'active' : ''}`} aria-pressed={label === preset.value}
          disabled={editingDisabled || pendingAction !== null} title={preset.title}
          onClick={() => void action('preset', () => onUpdateLabel(acc.id, preset.value))}>{preset.label}</button>)}</div>
      </div>
    </td>
  </tr>
})
