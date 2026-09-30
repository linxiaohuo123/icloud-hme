/**
 * [INPUT]: 依赖 components/icons 的 IconTerminal/IconRefresh/IconZap，依赖 api/types 的 ScheduleLog/ScheduleStatus
 * [OUTPUT]: 对外提供 ScheduleLogConsole 日志控制台、LogLevel 与 getLogLevel
 * [POS]: schedule 的日志视口，保留稳定记录标识、读取状态与用户控制的最新日志跟随
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { memo, useLayoutEffect, useMemo, useRef, useState } from 'react'
import type { ScheduleLog, ScheduleStatus } from '../../api/types'
import { IconRefresh, IconTerminal, IconZap } from '../icons'

export type LogLevel = 'success' | 'error' | 'warn' | 'info'

// eslint-disable-next-line react-refresh/only-export-components
export function getLogLevel(msg: string): LogLevel {
  if (msg.includes('失败') || msg.includes('ERROR') || msg.includes('401') || msg.includes('结果未知') || msg.includes('瞬态故障') || msg.includes('返回空结果')) {
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

// eslint-disable-next-line react-refresh/only-export-components
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
  triggering: boolean
  onTriggerNow: () => Promise<void>
  onRefreshLogs: () => void
  loading: boolean
  error: string | null
  triggerDisabled: boolean
  intervalSeconds?: number
}

export const ScheduleLogConsole = memo(function ScheduleLogConsole({
  logs,
  status,
  logFilter,
  setLogFilter,
  triggering,
  onTriggerNow,
  onRefreshLogs,
  loading,
  error,
  triggerDisabled,
  intervalSeconds,
}: ScheduleLogConsoleProps) {
  const terminalRef = useRef<HTMLDivElement>(null)
  const followingRef = useRef(true)
  const [following, setFollowing] = useState(true)
  const successCount = useMemo(
    () => logs.filter((l) => getLogLevel(l.message) === 'success').length,
    [logs],
  )
  const errorCount = useMemo(
    () => logs.filter((l) => ['error', 'warn'].includes(getLogLevel(l.message))).length,
    [logs],
  )

  const filteredLogs = useMemo(() => {
    const occurrences = new Map<string, number>()
    const entries = logs.map((log) => {
      const identity = JSON.stringify([log.time, log.message])
      const occurrence = occurrences.get(identity) ?? 0
      occurrences.set(identity, occurrence + 1)
      return { ...log, key: `${identity}-${occurrence}` }
    })
    if (logFilter === 'all') return entries
    if (logFilter === 'success') {
      return entries.filter((l) => getLogLevel(l.message) === 'success')
    }
    return entries.filter((l) => ['error', 'warn'].includes(getLogLevel(l.message)))
  }, [logs, logFilter])

  useLayoutEffect(() => {
    const el = terminalRef.current
    if (el && followingRef.current) el.scrollTop = el.scrollHeight
  }, [filteredLogs])

  const selectFilter = (filter: 'all' | 'success' | 'error') => {
    followingRef.current = true
    setFollowing(true)
    setLogFilter(filter)
    if (terminalRef.current) terminalRef.current.scrollTop = terminalRef.current.scrollHeight
  }

  return (
    <div className="log-feed-card">
      <div className="log-feed-header">
        <div style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
          <IconTerminal size={16} style={{ color: 'var(--color-primary)' }} />
          <span style={{ fontWeight: 600, fontSize: '14px', color: 'var(--color-text)' }}>
            实时调度日志
          </span>
          <span className={`status-pill ${!loading && !error ? 'active' : ''}`} style={{ fontSize: '11px' }}>
            <span className="status-dot" />
            {error ? '读取中断' : loading ? '连接中…' : '自动监听中'}
          </span>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
          <div className="log-feed-filters">
            <button
              type="button"
              className={`log-filter-btn ${logFilter === 'all' ? 'active' : ''}`}
              onClick={() => selectFilter('all')}
              aria-pressed={logFilter === 'all'}
            >
              全部 ({logs.length})
            </button>
            <button
              type="button"
              className={`log-filter-btn ${logFilter === 'success' ? 'active' : ''}`}
              onClick={() => selectFilter('success')}
              aria-pressed={logFilter === 'success'}
            >
              成功 ({successCount})
            </button>
            <button
              type="button"
              className={`log-filter-btn ${logFilter === 'error' ? 'active' : ''}`}
              onClick={() => selectFilter('error')}
              aria-pressed={logFilter === 'error'}
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

      {!following && filteredLogs.length > 0 && <button type="button" className="btn btn-sm btn-secondary schedule-follow-latest"
        onClick={() => {
          followingRef.current = true
          setFollowing(true)
          if (terminalRef.current) terminalRef.current.scrollTop = terminalRef.current.scrollHeight
        }}>查看最新日志</button>}
      <div className="log-feed-body" ref={terminalRef} aria-busy={loading} onScroll={(event) => {
        const el = event.currentTarget
        const next = el.scrollHeight - el.scrollTop - el.clientHeight < 60
        followingRef.current = next
        setFollowing(next)
      }}>
        {filteredLogs.length === 0 ? (
          <div className="log-feed-empty">
            <div className="log-empty-icon">
              <IconTerminal size={24} />
            </div>
            <div className="log-empty-title">{loading ? '日志加载中…' : error ? '日志读取失败' : logs.length > 0 ? '当前筛选没有匹配日志' : '暂无调度运行日志'}</div>
            <div className="log-empty-desc">
              {error ? '请重新读取日志以确认最新运行结果。' : logs.length > 0 ? '切换到「全部」查看其他运行记录。' : `后台调度器${intervalSeconds ? `每 ${intervalSeconds / 60} 分钟` : ''}自动巡检已启用账号。立即执行一轮会实际创建别名。`}
            </div>
            <button
              type="button"
              className="btn btn-sm btn-secondary"
              onClick={() => void onTriggerNow()}
              disabled={triggering || status?.running === true || triggerDisabled}
            >
              <IconZap size={13} />
              <span>立即执行一轮补货</span>
            </button>
          </div>
        ) : (
          filteredLogs.map((log) => {
            const level = getLogLevel(log.message)
            return (
              <div key={log.key} className="log-feed-item">
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
