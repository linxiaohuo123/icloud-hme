/**
 * [INPUT]: 依赖 api/types 的 AccountSummary/Alias, components/icons 的 IconZap/IconAliases/IconCheck/IconShield
 * [OUTPUT]: 对外提供 WorkspaceMetrics 顶部指标卡片组组件
 * [POS]: web/src/pages/workspace 的指标统计组件，展示活跃别名、配额占用、运行状态与凭据健康度
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import type { AccountSummary, Alias } from '../../api/types'
import { IconAliases, IconCheck, IconShield, IconZap } from '../../components/icons'

interface WorkspaceMetricsProps {
  account: AccountSummary | null
  aliases: Alias[]
  activeAliasCount: number
}

export default function WorkspaceMetrics({
  account,
  aliases,
  activeAliasCount,
}: WorkspaceMetricsProps) {
  const MAX_ALIASES = 750
  const totalCount = aliases.length || account?.alias_total || 0
  const quotaPercent = Math.min(100, Math.round((totalCount / MAX_ALIASES) * 100))

  return (
    <div className="stat-grid">
      <div className="stat-card stat-card-blue">
        <div className="stat-card-header">
          <div className="stat-card-title-group">
            <div className="stat-icon stat-icon-blue">
              <IconZap size={16} />
            </div>
            <span className="stat-label">活跃别名</span>
          </div>
          <span className="stat-badge stat-badge-blue">全部可用</span>
        </div>
        <div className="stat-value">{activeAliasCount}</div>
        <div className="stat-subtext">可直接用于收发邮件</div>
      </div>

      <div className="stat-card stat-card-purple">
        <div className="stat-card-header">
          <div className="stat-card-title-group">
            <div className="stat-icon stat-icon-purple">
              <IconAliases size={16} />
            </div>
            <span className="stat-label">已建别名</span>
          </div>
          <span className="stat-badge stat-badge-purple">已用 {quotaPercent}%</span>
        </div>
        <div className="stat-value">
          {totalCount} <span className="stat-value-unit">/ {MAX_ALIASES}</span>
        </div>
        <div className="stat-subtext">官方配额上限 {MAX_ALIASES} 个</div>
      </div>

      <div className="stat-card stat-card-green">
        <div className="stat-card-header">
          <div className="stat-card-title-group">
            <div className="stat-icon stat-icon-green">
              <IconCheck size={16} />
            </div>
            <span className="stat-label">账号状态</span>
          </div>
          <span className="stat-badge stat-badge-green">
            {account?.status === 'active' ? '● 正常' : '● 待配置'}
          </span>
        </div>
        <div className="stat-value">
          {account?.status === 'active' ? '正常运行' : '待处理'}
        </div>
        <div className="stat-subtext">节点: {account?.host || '默认'}</div>
      </div>

      <div className="stat-card stat-card-amber">
        <div className="stat-card-header">
          <div className="stat-card-title-group">
            <div className="stat-icon stat-icon-amber">
              <IconShield size={16} />
            </div>
            <span className="stat-label">登录凭据</span>
          </div>
          <span className={`stat-badge ${account?.has_cookies ? 'stat-badge-green' : 'stat-badge-amber'}`}>
            {account?.has_cookies ? '有效' : '需更新'}
          </span>
        </div>
        <div className="stat-value">
          {account?.has_cookies ? 'Cookie 正常' : '密码登录'}
        </div>
        <div className="stat-subtext">{account?.mailbox ? 'IMAP 收件箱已连通' : '未配置收件箱'}</div>
      </div>
    </div>
  )
}
