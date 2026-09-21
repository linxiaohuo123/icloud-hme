/**
 * [INPUT]: 依赖 components/icons 的矢量图标，依赖 api/types 的 ScheduleStatus
 * [OUTPUT]: 对外提供 ScheduleMetrics 顶部指标网格组件
 * [POS]: web/src/components/schedule 的指标监控卡片，呈现自动补货账号、总配额、巡检周期与日志量
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { memo } from 'react'
import type { ScheduleStatus } from '../../api/types'
import {
  IconAccounts,
  IconShield,
  IconTerminal,
  IconZap,
} from '../icons'

interface ScheduleMetricsProps {
  enabledCount: number
  accountsCount: number
  totalHourlyQuota: number
  status: ScheduleStatus | null
  logsCount: number
}

export const ScheduleMetrics = memo(function ScheduleMetrics({
  enabledCount,
  accountsCount,
  totalHourlyQuota,
  status,
  logsCount,
}: ScheduleMetricsProps) {
  return (
    <div className="stat-grid stat-grid-compact">
      <div className="stat-card stat-card-blue">
        <div className="stat-card-header">
          <div className="stat-card-title-group">
            <div className="stat-icon stat-icon-blue">
              <IconAccounts size={16} />
            </div>
            <span className="stat-label">自动补货账号</span>
          </div>
        </div>
        <div className="stat-value">
          {enabledCount} / {accountsCount}
        </div>
      </div>

      <div className="stat-card stat-card-purple">
        <div className="stat-card-header">
          <div className="stat-card-title-group">
            <div className="stat-icon stat-icon-purple">
              <IconZap size={16} />
            </div>
            <span className="stat-label">总计每小时配额</span>
          </div>
        </div>
        <div className="stat-value">
          {totalHourlyQuota}
          <span className="stat-value-unit">个/小时</span>
        </div>
      </div>

      <div className="stat-card stat-card-green">
        <div className="stat-card-header">
          <div className="stat-card-title-group">
            <div className="stat-icon stat-icon-green">
              <IconShield size={16} />
            </div>
            <span className="stat-label">调度巡检周期</span>
          </div>
          <span className={`stat-badge ${status?.running ? 'stat-badge-green' : 'stat-badge-amber'}`}>
            {status?.running ? '● 执行中' : '● 空闲'}
          </span>
        </div>
        <div className="stat-value">
          {(status?.interval_seconds ?? 300) / 60}
          <span className="stat-value-unit">分钟 / 轮</span>
        </div>
      </div>

      <div className="stat-card stat-card-amber">
        <div className="stat-card-header">
          <div className="stat-card-title-group">
            <div className="stat-icon stat-icon-amber">
              <IconTerminal size={16} />
            </div>
            <span className="stat-label">实时日志缓冲区</span>
          </div>
        </div>
        <div className="stat-value">
          {logsCount}
          <span className="stat-value-unit">条</span>
        </div>
      </div>
    </div>
  )
})
