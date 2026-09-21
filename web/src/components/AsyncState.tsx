/**
 * [INPUT]: 依赖 components/icons 的 IconAlert/IconInboxEmpty
 * [OUTPUT]: 对外提供 AsyncState 统一异步状态组件 (骨架屏 loading / error+retry / empty+引导动作 emptyAction / content)
 * [POS]: web/src/components 的列表页三态渲染基建，被 AliasesPage, UsedAliasesPage 消费
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import type { ReactNode } from 'react'
import { IconAlert, IconInboxEmpty } from './icons'

interface AsyncStateProps {
  loading: boolean
  error: string
  empty: boolean
  emptyText?: string
  /** 空态下方的引导动作 (如"前往生成别名"链接)；不传则只显示文案 */
  emptyAction?: ReactNode
  onRetry: () => void
  children: ReactNode
}

/** 统一异步状态:骨架屏 loading / error+retry / empty / content */
export default function AsyncState({
  loading,
  error,
  empty,
  emptyText = '暂无数据',
  emptyAction,
  onRetry,
  children,
}: AsyncStateProps) {
  if (loading) {
    return (
      <div className="skeleton" role="status" aria-label="加载中" aria-busy="true">
        <span className="visually-hidden">加载中</span>
        <div className="skeleton-line" style={{ width: '30%' }} />
        <div className="skeleton-line" style={{ width: '85%' }} />
        <div className="skeleton-line" style={{ width: '70%' }} />
        <div className="skeleton-line" style={{ width: '90%' }} />
      </div>
    )
  }
  if (error) {
    return (
      <div className="async-error" role="alert">
        <div className="alert-error" style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <IconAlert size={18} style={{ flexShrink: 0 }} />
          <span>{error}</span>
        </div>
        <button type="button" className="btn btn-sm btn-secondary" onClick={onRetry}>
          重试
        </button>
      </div>
    )
  }
  if (empty) {
    return (
      <div className="empty-state">
        <IconInboxEmpty className="empty-icon" />
        {emptyText}
        {emptyAction && <div className="empty-state-action">{emptyAction}</div>}
      </div>
    )
  }
  return <>{children}</>
}
