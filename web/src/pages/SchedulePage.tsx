/**
 * [INPUT]: 依赖 hooks/useAccounts, api/client 的 request/ApiError, api/types 的 ScheduleConfig/ScheduleLog/ScheduleStatus/AccountSummary, components/ToastProvider, components/icons, components/schedule
 * [OUTPUT]: 对外提供 SchedulePage 定时别名任务与调度大盘组件
 * [POS]: web/src/pages 的核心页面，组装 ScheduleMetrics, ScheduleAccountRow, ScheduleMacroHintBar, ScheduleLogConsole
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError, request } from '../api/client'
import type { AccountSummary, ScheduleConfig, ScheduleLog, ScheduleStatus } from '../api/types'
import { useToast } from '../components/ToastProvider'
import { IconInfo, IconZap } from '../components/icons'
import { ScheduleAccountRow } from '../components/schedule/ScheduleAccountRow'
import { ScheduleLogConsole } from '../components/schedule/ScheduleLogConsole'
import { ScheduleMacroHintBar } from '../components/schedule/ScheduleMacroHintBar'
import { ScheduleMetrics } from '../components/schedule/ScheduleMetrics'
import { useAccounts } from '../hooks/useAccounts'

export default function SchedulePage() {
  const { accounts } = useAccounts()
  const [configs, setConfigs] = useState<Record<string, ScheduleConfig>>({})
  const [logs, setLogs] = useState<ScheduleLog[]>([])
  const [status, setStatus] = useState<ScheduleStatus | null>(null)
  const [logFilter, setLogFilter] = useState<'all' | 'success' | 'error'>('all')
  const [triggering, setTriggering] = useState(false)
  const { show } = useToast()
  const terminalRef = useRef<HTMLDivElement>(null)

  const [showMacroHint, setShowMacroHint] = useState(() => {
    try {
      return localStorage.getItem('schedule_macro_hint') !== 'false'
    } catch {
      return true
    }
  })

  const toggleMacroHint = () => {
    setShowMacroHint((prev) => {
      const next = !prev
      try {
        localStorage.setItem('schedule_macro_hint', next ? 'true' : 'false')
      } catch {
        // ignore localStorage errors in restricted contexts
      }
      return next
    })
  }

  const fetchLogs = useCallback(() => {
    request<ScheduleLog[]>('/api/schedule/logs')
      .then((data) => {
        if (Array.isArray(data)) {
          setLogs(data)
        }
      })
      .catch(() => {})
  }, [])

  const fetchStatus = useCallback(() => {
    request<ScheduleStatus>('/api/schedule/status')
      .then((data) => {
        if (data && typeof data === 'object') {
          setStatus(data)
        }
      })
      .catch(() => {})
  }, [])

  const fetchConfigs = useCallback(() => {
    request<ScheduleConfig[]>('/api/schedule/configs')
      .then((data) => {
        if (!Array.isArray(data)) return
        setConfigs(() => {
          const map: Record<string, ScheduleConfig> = {}
          data.forEach((c) => {
            map[c.account_id] = c
          })
          return map
        })
      })
      .catch(() => {})
  }, [])

  useEffect(() => {
    fetchLogs()
    fetchConfigs()
    fetchStatus()
    const timer = setInterval(() => {
      fetchLogs()
      fetchConfigs()
      fetchStatus()
    }, 3000)
    return () => clearInterval(timer)
  }, [fetchLogs, fetchConfigs, fetchStatus])

  // 新日志到达时跟随滚动到底部; 用户手动上翻阅读历史时不打扰
  useEffect(() => {
    const el = terminalRef.current
    if (!el) return
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 60
    if (nearBottom) el.scrollTop = el.scrollHeight
  }, [logs])

  const configsRef = useRef(configs)
  configsRef.current = configs

  const handleToggleAccount = useCallback(
    async (acc: AccountSummary) => {
      const cur = configsRef.current[acc.id] || {
        account_id: acc.id,
        enabled: false,
        hourly_quota: 5,
        current_hour_count: 0,
        mode: 'always',
      }
      const updated: ScheduleConfig = {
        ...cur,
        enabled: !cur.enabled,
        started_at:
          !cur.enabled && cur.mode === 'duration' ? new Date().toISOString() : cur.started_at,
      }
      try {
        await request(`/api/schedule/configs/${encodeURIComponent(acc.id)}`, {
          method: 'PUT',
          body: updated,
        })
        setConfigs((prev) => ({ ...prev, [acc.id]: updated }))
        show(`账号 [${acc.name || acc.real_email}] 定时任务已${updated.enabled ? '开启' : '关闭'}`)
      } catch (err) {
        show(err instanceof ApiError ? err.message : '更新配置失败')
      }
    },
    [show],
  )

  const handleUpdateScheduleMode = useCallback(
    async (accId: string, patch: Partial<ScheduleConfig>) => {
      const cur = configsRef.current[accId] || {
        account_id: accId,
        enabled: false,
        hourly_quota: 5,
        current_hour_count: 0,
        mode: 'always',
      }
      const updated: ScheduleConfig = { ...cur, ...patch }
      try {
        await request(`/api/schedule/configs/${encodeURIComponent(accId)}`, {
          method: 'PUT',
          body: updated,
        })
        setConfigs((prev) => ({ ...prev, [accId]: updated }))
        show('调度策略已更新')
      } catch (err) {
        show(err instanceof ApiError ? err.message : '更新调度策略失败')
      }
    },
    [show],
  )

  const handleUpdateQuota = useCallback(
    async (accId: string, quota: number) => {
      const cur = configsRef.current[accId] || {
        account_id: accId,
        enabled: false,
        hourly_quota: 5,
        current_hour_count: 0,
      }
      const updated: ScheduleConfig = { ...cur, hourly_quota: quota }
      try {
        await request(`/api/schedule/configs/${encodeURIComponent(accId)}`, {
          method: 'PUT',
          body: updated,
        })
        setConfigs((prev) => ({ ...prev, [accId]: updated }))
        show('每小时配额已更新')
      } catch (err) {
        show(err instanceof ApiError ? err.message : '更新配额失败')
      }
    },
    [show],
  )

  const handleUpdateLabel = useCallback(
    async (accId: string, label: string) => {
      const cur = configsRef.current[accId] || {
        account_id: accId,
        enabled: false,
        hourly_quota: 5,
        alias_label: 'scheduled',
        current_hour_count: 0,
      }
      const clean = label.trim() || 'scheduled'
      const updated: ScheduleConfig = { ...cur, alias_label: clean }
      try {
        await request(`/api/schedule/configs/${encodeURIComponent(accId)}`, {
          method: 'PUT',
          body: updated,
        })
        setConfigs((prev) => ({ ...prev, [accId]: updated }))
        show('别名备注模板已更新')
      } catch (err) {
        show(err instanceof ApiError ? err.message : '更新别名备注模板失败')
      }
    },
    [show],
  )

  const handleApplyPreset = useCallback(
    (accountId: string, presetVal: string) => {
      const cfg = configsRef.current[accountId]
      const updated = {
        ...(cfg || {
          account_id: accountId,
          enabled: false,
          hourly_quota: 5,
          current_hour_count: 0,
        }),
        alias_label: presetVal,
      }
      setConfigs((prev) => ({
        ...prev,
        [accountId]: updated,
      }))
      void handleUpdateLabel(accountId, presetVal)
    },
    [handleUpdateLabel],
  )

  const handleTriggerNow = async () => {
    setTriggering(true)
    try {
      await request('/api/schedule/run-now', { method: 'POST' })
      setStatus((prev) => ({
        running: true,
        last_run_at: prev?.last_run_at,
        interval_seconds: prev?.interval_seconds ?? 300,
      }))
      show('已触发一轮定时别名补货任务')
    } catch (err) {
      show(err instanceof ApiError ? err.message : '触发任务失败')
    } finally {
      setTriggering(false)
    }
  }

  const handleCopyMacro = (macroText: string) => {
    if (navigator.clipboard?.writeText) {
      navigator.clipboard
        .writeText(macroText)
        .then(() => {
          show(`已复制占位符: ${macroText}`)
        })
        .catch(() => {
          show(`占位符: ${macroText}`)
        })
    } else {
      show(`占位符: ${macroText}`)
    }
  }

  // 统计指标
  const enabledCount = accounts.filter((a) => configs[a.id]?.enabled).length
  const totalHourlyQuota = accounts.reduce(
    (acc, a) => (configs[a.id]?.enabled ? acc + configs[a.id].hourly_quota : acc),
    0,
  )

  return (
    <div className="page-container">
      {/* 标题与动作按钮 */}
      <div className="page-header">
        <div>
          <h1 className="page-title">定时任务</h1>
          <p className="page-desc">
            定时为已启用的账号补充 HME 别名，支持设置每小时限额与 Cookie 失效自动暂停
          </p>
        </div>
        <div>
          <button
            type="button"
            className="btn btn-primary"
            onClick={() => void handleTriggerNow()}
            disabled={triggering || status?.running === true}
          >
            <IconZap size={15} />
            <span>
              {status?.running ? '执行中…' : triggering ? '触发中…' : '立即执行一轮'}
            </span>
          </button>
        </div>
      </div>

      {/* 顶部指标统计 */}
      <ScheduleMetrics
        enabledCount={enabledCount}
        accountsCount={accounts.length}
        totalHourlyQuota={totalHourlyQuota}
        status={status}
        logsCount={logs.length}
      />

      <div className="schedule-layout">
        {/* 账号调度策略配置大盘 */}
        <div className="card">
          <div
            className="card-header"
            style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}
          >
            <h2 className="card-title">各账号调度策略 ({accounts.length})</h2>
            <button
              type="button"
              className={`btn-hint-toggle ${showMacroHint ? 'active' : ''}`}
              onClick={toggleMacroHint}
              title={showMacroHint ? '收起动态占位符说明' : '展开动态占位符说明'}
            >
              <IconInfo size={13} />
              <span>{showMacroHint ? '收起说明' : '占位符说明'}</span>
            </button>
          </div>
          <div className="table-responsive">
            <table className="table schedule-table-compact">
              <thead>
                <tr>
                  <th style={{ minWidth: 200 }}>账号信息</th>
                  <th style={{ textAlign: 'center', width: 80 }}>自动补货</th>
                  <th style={{ textAlign: 'center', width: 80 }}>配额/时</th>
                  <th style={{ minWidth: 220 }}>调度模式与时段</th>
                  <th style={{ minWidth: 360 }}>
                    <div style={{ display: 'inline-flex', alignItems: 'center', gap: '6px' }}>
                      <span>别名备注模板</span>
                      <span
                        className="th-macro-tooltip-icon"
                        title="动态宏变量支持：&#10;• {date} - 当天年月日 (如 20260920)&#10;• {time} - 当前时分 (如 1405)&#10;• {seq}  - 当前批次序号 (如 1, 2)&#10;• {account} - 母账号名&#10;示例: gpt-{date} → gpt-20260920"
                      >
                        <IconInfo size={12} />
                      </span>
                    </div>
                  </th>
                </tr>
              </thead>
              <tbody>
                {accounts.length === 0 ? (
                  <tr>
                    <td colSpan={5} className="text-center text-muted" style={{ padding: '28px' }}>
                      暂无账号，请先在「账号管理」页添加账号
                    </td>
                  </tr>
                ) : (
                  accounts.map((acc) => {
                    const cfg = configs[acc.id] || {
                      account_id: acc.id,
                      enabled: false,
                      hourly_quota: 5,
                      alias_label: 'scheduled',
                      current_hour_count: 0,
                    }
                    return (
                      <ScheduleAccountRow
                        key={acc.id}
                        acc={acc}
                        cfg={cfg}
                        onToggle={handleToggleAccount}
                        onUpdateQuota={handleUpdateQuota}
                        onUpdateLabel={handleUpdateLabel}
                        onUpdateMode={handleUpdateScheduleMode}
                        onApplyPreset={handleApplyPreset}
                      />
                    )
                  })
                )}
              </tbody>
            </table>
          </div>

          <ScheduleMacroHintBar
            show={showMacroHint}
            onToggle={toggleMacroHint}
            onCopy={handleCopyMacro}
          />
        </div>

        {/* 右侧：调度运行日志 */}
        <ScheduleLogConsole
          logs={logs}
          status={status}
          logFilter={logFilter}
          setLogFilter={setLogFilter}
          terminalRef={terminalRef}
          triggering={triggering}
          onTriggerNow={handleTriggerNow}
          onRefreshLogs={fetchLogs}
        />
      </div>
    </div>
  )
}
