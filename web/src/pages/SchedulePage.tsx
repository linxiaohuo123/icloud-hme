/**
 * [INPUT]: 依赖 hooks/useAccounts, api/client 的 request/ApiError, api/types 的 ScheduleConfig/ScheduleLog/ScheduleStatus/AccountSummary, components/ToastProvider, components/icons, components/schedule
 * [OUTPUT]: 对外提供桌面分页调度大盘，保存与手动执行隔离、账号重试、保护策略展示与独立读取状态
 * [POS]: web/src/pages 的核心页面，组装 ScheduleMetrics, ScheduleAccountRow, ScheduleMacroHintBar, ScheduleLogConsole，支持常规补货与强制全员补货(?all=true)
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { ApiError, request } from '../api/client'
import type { AccountSummary, ScheduleConfig, ScheduleLog, ScheduleStatus } from '../api/types'
import { useToast } from '../components/ToastProvider'
import { IconInfo, IconZap } from '../components/icons'
import { ScheduleAccountRow } from '../components/schedule/ScheduleAccountRow'
import { ScheduleLogConsole } from '../components/schedule/ScheduleLogConsole'
import { ScheduleMacroHintBar } from '../components/schedule/ScheduleMacroHintBar'
import { ScheduleMetrics } from '../components/schedule/ScheduleMetrics'
import { useAccounts } from '../hooks/useAccounts'
import { isScheduleExpired } from '../utils/schedule'

const PAGE_SIZE = 25

export default function SchedulePage() {
  const { accounts, loading: accountsLoading, error: accountsError } = useAccounts()
  const [configs, setConfigs] = useState<Record<string, ScheduleConfig>>({})
  const [logs, setLogs] = useState<ScheduleLog[]>([])
  const [status, setStatus] = useState<ScheduleStatus | null>(null)
  const [logFilter, setLogFilter] = useState<'all' | 'success' | 'error'>('all')
  const [triggering, setTriggering] = useState(false)
  const [configsLoaded, setConfigsLoaded] = useState(false)
  const [logsLoaded, setLogsLoaded] = useState(false)
  const [pollErrors, setPollErrors] = useState<Record<'configs' | 'logs' | 'status', string | null>>({ configs: null, logs: null, status: null })
  const [savingAccounts, setSavingAccounts] = useState<Record<string, number>>({})
  const [page, setPage] = useState(1)
  const [now, setNow] = useState(Date.now)
  const { show } = useToast()

  const [showMacroHint, setShowMacroHint] = useState(() => {
    try {
      return localStorage.getItem('schedule_macro_hint') !== 'false'
    } catch {
      return true
    }
  })

  const toggleMacroHint = useCallback(() => {
    setShowMacroHint((prev) => {
      const next = !prev
      try {
        localStorage.setItem('schedule_macro_hint', next ? 'true' : 'false')
      } catch {
        // ignore localStorage errors in restricted contexts
      }
      return next
    })
  }, [])

  const pollGenerationRef = useRef(0)
  const pollAbortRef = useRef<AbortController | null>(null)
  const pollEnabledRef = useRef(false)
  const pendingWritesRef = useRef(0)
  const triggeringRef = useRef(false)
  const writeQueuesRef = useRef(new Map<string, Promise<ScheduleConfig>>())
  const configsRef = useRef(configs)
  const writeVersionsRef = useRef(new Map<string, number>())

  const runPoll = useCallback(async (force = false) => {
    if (!pollEnabledRef.current || triggeringRef.current) return
    if (!force && pollAbortRef.current && !pollAbortRef.current.signal.aborted) return
    pollAbortRef.current?.abort()
    pollGenerationRef.current++
    const gen = pollGenerationRef.current
    const controller = new AbortController()
    pollAbortRef.current = controller
    const versions = new Map(writeVersionsRef.current)
    const pendingAccounts = new Set(writeQueuesRef.current.keys())
    const isCurrent = () => gen === pollGenerationRef.current && !controller.signal.aborted
    const read = async <T,>(kind: 'configs' | 'logs' | 'status', commit: (data: T) => void) => {
      try {
        const data = await request<T>(`/api/schedule/${kind}`, { signal: controller.signal })
        if (!isCurrent()) return
        commit(data)
        setPollErrors((prev) => prev[kind] === null ? prev : { ...prev, [kind]: null })
      } catch (err) {
        if (!isCurrent()) return
        const message = err instanceof Error ? err.message : '读取失败'
        setPollErrors((prev) => prev[kind] === message ? prev : { ...prev, [kind]: message })
      }
    }

    try {
      await Promise.all([
        read<ScheduleLog[]>('logs', (data) => {
          if (!Array.isArray(data)) throw new Error('调度日志响应格式错误')
          setLogs((prev) => prev.length === data.length && prev.every((entry, i) => entry.time === data[i].time && entry.message === data[i].message) ? prev : data)
          setLogsLoaded(true)
        }),
        read<ScheduleConfig[]>('configs', (data) => {
          if (!Array.isArray(data)) throw new Error('调度配置响应格式错误')
          const map: Record<string, ScheduleConfig> = {}
          data.forEach((cfg) => {
            const id = cfg.account_id
            const previous = configsRef.current[id]
            if (pendingAccounts.has(id) || writeQueuesRef.current.has(id) || versions.get(id) !== writeVersionsRef.current.get(id)) {
              if (previous) map[id] = previous
              return
            }
            const keys = Object.keys(cfg) as (keyof ScheduleConfig)[]
            map[id] = previous && Object.keys(previous).length === keys.length && keys.every((key) => previous[key] === cfg[key]) ? previous : cfg
          })
          // GET 开始时保存中的账号可能尚无数据库记录，也必须保留本地权威结果。
          for (const [id, cfg] of Object.entries(configsRef.current)) {
            if (pendingAccounts.has(id) || writeQueuesRef.current.has(id) || versions.get(id) !== writeVersionsRef.current.get(id)) map[id] = cfg
          }
          if (Object.keys(map).length !== Object.keys(configsRef.current).length || Object.keys(map).some((id) => map[id] !== configsRef.current[id])) {
            configsRef.current = map
            setConfigs(map)
          }
          setConfigsLoaded(true)
        }),
        read<ScheduleStatus>('status', (data) => {
          if (!data || typeof data.running !== 'boolean' || typeof data.interval_seconds !== 'number') throw new Error('调度状态响应格式错误')
          setStatus((prev) => prev?.running === data.running && prev?.last_run_at === data.last_run_at && prev?.interval_seconds === data.interval_seconds ? prev : data)
        }),
      ])
    } finally {
      if (pollAbortRef.current === controller) {
        pollAbortRef.current = null
      }
    }
  }, [])

  useEffect(() => {
    let timer: ReturnType<typeof setInterval> | null = null

    const start = () => {
      pollEnabledRef.current = true
      if (timer) clearInterval(timer)
      void runPoll()
      timer = setInterval(() => {
        if (typeof document !== 'undefined' && document.hidden) return
        setNow(Date.now())
        void runPoll()
      }, 3000)
    }

    const stop = () => {
      pollEnabledRef.current = false
      if (timer) {
        clearInterval(timer)
        timer = null
      }
      pollAbortRef.current?.abort()
    }

    const handleVisibilityChange = () => {
      if (typeof document !== 'undefined' && document.hidden) {
        stop()
      } else {
        start()
      }
    }

    const handleLogout = () => {
      stop()
    }

    start()
    if (typeof document !== 'undefined') {
      document.addEventListener('visibilitychange', handleVisibilityChange)
    }
    if (typeof window !== 'undefined') {
      window.addEventListener('auth-logout', handleLogout)
    }

    return () => {
      stop()
      if (typeof document !== 'undefined') {
        document.removeEventListener('visibilitychange', handleVisibilityChange)
      }
      if (typeof window !== 'undefined') {
        window.removeEventListener('auth-logout', handleLogout)
      }
    }
  }, [runPoll])

  const saveSchedulePatch = useCallback((
    accountId: string,
    buildPatch: (current: ScheduleConfig | undefined) => Partial<ScheduleConfig>,
  ): Promise<ScheduleConfig> => {
    pendingWritesRef.current++
    writeVersionsRef.current.set(accountId, (writeVersionsRef.current.get(accountId) ?? 0) + 1)
    setSavingAccounts((prev) => ({ ...prev, [accountId]: (prev[accountId] ?? 0) + 1 }))
    pollAbortRef.current?.abort()
    pollGenerationRef.current++

    const execute = async () => {
      const saved = await request<ScheduleConfig>(
        `/api/schedule/configs/${encodeURIComponent(accountId)}`,
        { method: 'PUT', body: buildPatch(configsRef.current[accountId]) },
      )
      configsRef.current = { ...configsRef.current, [accountId]: saved }
      setConfigs(configsRef.current)
      return saved
    }
    const previous = writeQueuesRef.current.get(accountId)
    const pending = previous ? previous.then(execute, execute) : Promise.resolve().then(execute)
    const queued = pending.finally(() => {
      pendingWritesRef.current--
      setSavingAccounts((prev) => ({ ...prev, [accountId]: prev[accountId] - 1 }))
      if (writeQueuesRef.current.get(accountId) === queued) {
        writeQueuesRef.current.delete(accountId)
      }
      if (pendingWritesRef.current === 0) void runPoll(true)
    })
    writeQueuesRef.current.set(accountId, queued)
    return queued
  }, [runPoll])

  const handleToggleAccount = useCallback(
    async (acc: AccountSummary) => {
      try {
        const saved = await saveSchedulePatch(acc.id, (current) => ({
          enabled: !current?.enabled,
          ...(!current?.enabled && current?.mode === 'duration'
            ? { started_at: new Date().toISOString() }
            : {}),
        }))
        show(`账号 [${acc.name || acc.real_email}] 定时任务已${saved.enabled ? '开启' : '关闭'}`)
        return true
      } catch (err) {
        show(err instanceof ApiError ? err.message : '更新配置失败')
        return false
      }
    },
    [show, saveSchedulePatch],
  )

  const handleUpdateScheduleMode = useCallback(
    async (accId: string, patch: Partial<ScheduleConfig>) => {
      try {
        await saveSchedulePatch(accId, (current) => ({
          ...patch,
          ...(patch.mode === 'daily_window' ? {
            ...(!current?.start_time ? { start_time: '09:00' } : {}),
            ...(!current?.end_time ? { end_time: '18:00' } : {}),
          } : {}),
          ...(patch.mode === 'duration' ? {
            ...(!current?.duration_hours ? { duration_hours: 12 } : {}),
            ...(current?.enabled ? { started_at: new Date().toISOString() } : {}),
          } : {}),
        }))
        show('调度策略已更新')
        return true
      } catch (err) {
        show(err instanceof ApiError ? err.message : '更新调度策略失败')
        return false
      }
    },
    [show, saveSchedulePatch],
  )

  const handleUpdateQuota = useCallback(
    async (accId: string, quota: number) => {
      try {
        await saveSchedulePatch(accId, () => ({ hourly_quota: quota }))
        show('每小时配额已更新')
        return true
      } catch (err) {
        show(err instanceof ApiError ? err.message : '更新配额失败')
        return false
      }
    },
    [show, saveSchedulePatch],
  )

  const handleUpdateLabel = useCallback(
    async (accId: string, label: string) => {
      const clean = label.trim() || 'scheduled'
      try {
        await saveSchedulePatch(accId, () => ({ alias_label: clean }))
        show('别名备注模板已更新')
        return true
      } catch (err) {
        show(err instanceof ApiError ? err.message : '更新别名备注模板失败')
        return false
      }
    },
    [show, saveSchedulePatch],
  )

  const accountsUnavailable = accountsLoading || accountsError !== null
  const configsUnavailable = !configsLoaded || pollErrors.configs !== null
  const statusUnavailable = !status || pollErrors.status !== null
  const saving = Object.values(savingAccounts).some((count) => count > 0)
  const triggerDisabled = triggering || saving || accountsUnavailable || configsUnavailable || statusUnavailable || status?.running === true

  const handleTriggerNow = useCallback(async (all = false) => {
    // 失焦保存和点击可以发生在同一事件序列，不能只依赖渲染后的 disabled。
    if (pendingWritesRef.current > 0 || triggeringRef.current || accountsUnavailable || configsUnavailable || statusUnavailable || status?.running) return
    triggeringRef.current = true
    pollAbortRef.current?.abort()
    pollGenerationRef.current++
    setStatus(null)
    setTriggering(true)
    try {
      const url = all ? '/api/schedule/run-now?all=true' : '/api/schedule/run-now'
      await request(url, { method: 'POST' })
      show(all ? '已提交全员补货请求，请查看日志确认执行结果' : '已提交补货请求，请查看日志确认执行结果')
    } catch (err) {
      show(err instanceof ApiError ? err.message : '触发任务失败')
    } finally {
      triggeringRef.current = false
      setTriggering(false)
      void runPoll(true)
    }
  }, [show, runPoll, accountsUnavailable, configsUnavailable, statusUnavailable, status])

  const handleCopyMacro = useCallback((macroText: string) => {
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
  }, [show])
  const handleRefreshLogs = useCallback(() => void runPoll(true), [runPoll])

  // 统计指标
  const defaults = useMemo(() => Object.fromEntries(accounts.map((acc) => [acc.id, {
    account_id: acc.id, enabled: false, hourly_quota: 5, alias_label: 'scheduled', current_hour_count: 0,
  } satisfies ScheduleConfig])), [accounts])
  const pageCount = Math.max(1, Math.ceil(accounts.length / PAGE_SIZE))
  const currentPage = Math.min(page, pageCount)
  const visibleAccounts = accounts.slice((currentPage - 1) * PAGE_SIZE, currentPage * PAGE_SIZE)
  const enabledCount = accounts.filter((a) => !a.schedule_protected && configs[a.id]?.enabled && !isScheduleExpired(configs[a.id], now)).length
  const totalHourlyQuota = accounts.reduce(
    (acc, a) => (!a.schedule_protected && configs[a.id]?.enabled && !isScheduleExpired(configs[a.id], now) ? acc + configs[a.id].hourly_quota : acc),
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
        <div className="schedule-header-actions">
          <button
            type="button"
            className="btn btn-secondary"
            onClick={() => void handleTriggerNow(true)}
            disabled={triggerDisabled}
            title="对全部非保护账号（包含未启用账号）补货，绕过时段和到期限制"
          >
            <span>强制全员补货</span>
          </button>
          <button
            type="button"
            className="btn btn-primary"
            onClick={() => void handleTriggerNow(false)}
            disabled={triggerDisabled}
            title={saving ? '请等待配置保存完成' : '为已启用的非保护账号补货，绕过时段和到期限制'}
          >
            <IconZap size={15} />
            <span>
              {status?.running ? '执行中…' : triggering ? '触发中…' : '立即执行一轮'}
            </span>
          </button>
        </div>
      </div>

      <p className="text-muted schedule-execution-note">普通补货仅针对已启用账号，强制全员包含暂停账号；两者均绕过时段和到期限制，遵守配额与保护规则。{saving && '配置正在保存，完成后可执行。'}</p>
      {Object.values(pollErrors).some(Boolean) && <div className="schedule-read-error" role="alert">
        <div>{pollErrors.configs && <p>调度配置读取失败：{pollErrors.configs}。{configsLoaded ? '当前显示上次成功读取的数据，编辑暂不可用。' : '尚未确认账号配置，编辑暂不可用。'}</p>}
        {pollErrors.status && <p>运行状态读取失败：{pollErrors.status}。当前状态未确认。</p>}
        {pollErrors.logs && <p>日志读取失败：{pollErrors.logs}。{logsLoaded ? '当前显示历史日志。' : '尚未获取日志。'}</p>}</div>
        <button type="button" className="btn btn-sm btn-secondary" onClick={() => void runPoll(true)}>重新读取</button>
      </div>}

      {/* 顶部指标统计 */}
      <ScheduleMetrics
        enabledCount={enabledCount}
        accountsCount={accounts.length}
        totalHourlyQuota={totalHourlyQuota}
        status={status}
        logsCount={logs.length}
        accountsKnown={!accountsUnavailable}
        configsKnown={configsLoaded && !pollErrors.configs && !accountsUnavailable}
        statusKnown={!statusUnavailable}
        logsKnown={logsLoaded && !pollErrors.logs}
      />

      <div className="schedule-layout">
        {/* 账号调度策略配置大盘 */}
        <div className="card">
          <div
            className="card-header"
            style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}
          >
            <h2 className="card-title">各账号调度策略 ({accountsUnavailable ? '—' : accounts.length})</h2>
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
            <table className="table schedule-table-compact" aria-label="账号调度策略" aria-busy={!configsLoaded && !pollErrors.configs}>
              <thead>
                <tr>
                  <th>账号信息</th>
                  <th style={{ textAlign: 'center', width: 80 }}>自动补货</th>
                  <th style={{ textAlign: 'center', width: 80 }}>配额/时</th>
                  <th>调度模式与时段</th>
                  <th>
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
                      {accountsLoading ? '账号加载中…' : accountsError ? '账号加载失败，请点击顶部重试' : '暂无账号，请先在「账号管理」页添加账号'}
                    </td>
                  </tr>
                ) : (
                  visibleAccounts.map((acc) => {
                    const cfg = configs[acc.id] || defaults[acc.id]
                    return (
                      <ScheduleAccountRow
                        key={acc.id}
                        acc={acc}
                        cfg={cfg}
                        onToggle={handleToggleAccount}
                        onUpdateQuota={handleUpdateQuota}
                        onUpdateLabel={handleUpdateLabel}
                        onUpdateMode={handleUpdateScheduleMode}
                        disabled={configsUnavailable || accountsUnavailable || triggering}
                        saving={(savingAccounts[acc.id] ?? 0) > 0}
                        expired={isScheduleExpired(cfg, now)}
                      />
                    )
                  })
                )}
              </tbody>
            </table>
          </div>

          {accounts.length > PAGE_SIZE && <nav className="pagination-bar" aria-label="调度账号分页">
            <span className="pagination-info">共 {accounts.length} 个账号，每页 {PAGE_SIZE} 个 · 第 {currentPage} / {pageCount} 页</span>
            <div className="pagination-controls">
              <button type="button" className="pagination-btn" disabled={currentPage === 1} onClick={() => setPage(currentPage - 1)}>上一页</button>
              <button type="button" className="pagination-btn" disabled={currentPage === pageCount} onClick={() => setPage(currentPage + 1)}>下一页</button>
            </div>
          </nav>}

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
          triggering={triggering}
          onTriggerNow={handleTriggerNow}
          onRefreshLogs={handleRefreshLogs}
          loading={!logsLoaded && !pollErrors.logs}
          error={pollErrors.logs}
          triggerDisabled={triggerDisabled}
          intervalSeconds={status?.interval_seconds}
        />
      </div>
    </div>
  )
}
