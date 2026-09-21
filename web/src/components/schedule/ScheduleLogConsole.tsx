/**
 * [INPUT]: 依赖 components/icons 的 IconTerminal/IconRefresh/IconZap，依赖 api/types 的 ScheduleLog/ScheduleStatus
 * [OUTPUT]: 对外提供 ScheduleLogConsole 终端日志控制台组件、LogLevel 与 getLogLevel 诊断工具
 * [POS]: web/src/components/schedule 的日志视口，提供等级过滤、自动滚动与即时重载
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { memo, useMemo } from 'react'
import type { ScheduleLog, ScheduleStatus } from '../../api/types'
import { IconRefresh, IconTerminal, IconZap } from '../icons'

export type LogLevel = 'success' | 'error' | 'warn' | 'info'

export function getLogLevel(msg: string): LogLevel {
  if (msg.includes('失败') || msg.includes('ERROR') || msg.includes('401')) {
    return 'error'
  }
  if (
    msg.includes('熔断') ||
    msg.includes('跳过') ||
    msg.includes('达到限额') ||
    msg.includes('额度已用满') ||
    msg.includes('配额已满') ||
    msg.includes('中断')
  ) {
    return 'warn'
  }
  if (msg.includes('成功') || msg.includes('SUCCESS')) {
    return 'success'
  }
  return 'info'
}

export const LOG_LEVEL_META: Record<LogLevel, { text: string; className: string }> = {
  error: { text: 'ERROR', className: 'log-badge-error' },
  warn: { text: 'WARN', className: 'log-badge-warn' },
  success: { text: 'SUCCESS', className: 'log-badge-success' },
  info: { text: 'INFO', className: 'log-badge-info' },
}

interface ScheduleLogConsoleProps {
  logs: ScheduleLog[]
  status: ScheduleStatus | null
  logFilter: 'all' | 'success' | 'error'
  setLogFilter: (filter: 'all' | 'success' | 'error') => void
  terminalRef: React.RefObject<HTMLDivElement | null>
  triggering: boolean
  onTriggerNow: () => Promise<void>
  onRefreshLogs: () => void
}

export const ScheduleLogConsole = memo(function ScheduleLogConsole({
  logs,
  status,
  logFilter,
  setLogFilter,
  terminalRef,
  triggering,
  onTriggerNow,
  onRefreshLogs,
}: ScheduleLogConsoleProps) {
  const successCount = useMemo(
    () => logs.filter((l) => getLogLevel(l.message) === 'success').length,
    [logs],
  )
  const errorCount = useMemo(
    () => logs.filter((l) => ['error', 'warn'].includes(getLogLevel(l.message))).length,
    [logs],
  )

  const filteredLogs = useMemo(() => {
    if (logFilter === 'all') return logs
    if (logFilter === 'success') {
      return logs.filter((l) => getLogLevel(l.message) === 'success')
    }
    return logs.filter((l) => ['error', 'warn'].includes(getLogLevel(l.message)))
  }, [logs, logFilter])

  return (
    <div className="log-feed-card">
      <div className="log-feed-header">
        <div style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
          <IconTerminal size={16} style={{ color: 'var(--color-primary)' }} />
          <span style={{ fontWeight: 600, fontSize: '14px', color: 'var(--color-text)' }}>
            实时调度日志
          </span>
          <span className="status-pill active" style={{ fontSize: '11px' }}>
            <span className="status-dot" />
            自动监听中
          </span>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
          <div className="log-feed-filters">
            <button
              type="button"
              className={`log-filter-btn ${logFilter === 'all' ? 'active' : ''}`}
              onClick={() => setLogFilter('all')}
            >
              全部 ({logs.length})
            </button>
            <button
              type="button"
              className={`log-filter-btn ${logFilter === 'success' ? 'active' : ''}`}
              onClick={() => setLogFilter('success')}
            >
              成功 ({successCount})
            </button>
            <button
              type="button"
              className={`log-filter-btn ${logFilter === 'error' ? 'active' : ''}`}
              onClick={() => setLogFilter('error')}
            >
              异常 ({errorCount})
            </button>
          </div>
          <button
            type="button"
            className="btn-icon"
            onClick={onRefreshLogs}
            title="立即刷新日志"
          >
            <IconRefresh size={14} />
          </button>
        </div>
      </div>

      <div className="log-feed-body" ref={terminalRef}>
        {filteredLogs.length === 0 ? (
          <div className="log-feed-empty">
            <div className="log-empty-icon">
              <IconTerminal size={24} />
            </div>
            <div className="log-empty-title">暂无调度运行日志</div>
            <div className="log-empty-desc">
              后台调度器每 5 分钟自动巡检活跃账号。你可以点击右上角「立即执行一轮」发起实时补货测试。
            </div>
            <button
              type="button"
              className="btn btn-sm btn-secondary"
              onClick={() => void onTriggerNow()}
              disabled={triggering || status?.running === true}
            >
              <IconZap size={13} />
              <span>立即执行一轮测试</span>
            </button>
          </div>
        ) : (
          filteredLogs.map((log, idx) => {
            const level = getLogLevel(log.message)
            return (
              <div key={`${idx}-${log.time}-${log.message}`} className="log-feed-item">
                <span className="log-feed-time">{log.time || '—'}</span>
                <span className={`log-feed-badge ${LOG_LEVEL_META[level].className}`}>
                  {LOG_LEVEL_META[level].text}
                </span>
                <span className="log-feed-msg">{log.message}</span>
              </div>
            )
          })
        )}
      </div>
    </div>
  )
})
