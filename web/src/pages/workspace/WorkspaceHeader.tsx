/**
 * [INPUT]: 依赖 api/types 的 AccountSummary, components/icons 的各类图标
 * [OUTPUT]: 对外提供 WorkspaceHeader 账号头部身份卡与全局快捷操作条
 * [POS]: web/src/pages/workspace 的头部组件，展示账号身份并提供快速出号、同步与凭据配置入口
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import type { AccountSummary } from '../../api/types'
import {
  IconCloud,
  IconKey,
  IconMail,
  IconPlus,
  IconRefresh,
  IconZap,
} from '../../components/icons'

interface WorkspaceHeaderProps {
  account: AccountSummary | null
  quickCreating: boolean
  aliasLoading: boolean
  onQuickCreate: () => void
  onCreateCustom: () => void
  onSync: () => void
  onOpenCookie: () => void
  onOpenMailbox: () => void
  onCopyAccount: (email: string) => void
}

export default function WorkspaceHeader({
  account,
  quickCreating,
  aliasLoading,
  onQuickCreate,
  onCreateCustom,
  onSync,
  onOpenCookie,
  onOpenMailbox,
  onCopyAccount,
}: WorkspaceHeaderProps) {
  const emailToCopy = account?.icloud_email || account?.real_email || ''

  return (
    <div className="account-workspace-header">
      <div className="workspace-header-top">
        <div className="workspace-identity-group">
          <div className="workspace-avatar" title="iCloud Hide My Email">
            <IconCloud size={24} />
          </div>
          <div>
            <div className="workspace-title-row">
              <h1 className="workspace-name">{account?.name || account?.real_email || '加载中…'}</h1>
              <span className={`status-pill ${account?.status || 'pending'}`}>
                <span className="status-dot" />
                {account?.status === 'active' ? '正常运行' : account?.status === 'pending' ? '待配置' : '异常'}
              </span>
              <span className="badge badge-neutral" style={{ fontSize: '11px', fontWeight: 600 }}>主账号</span>
            </div>
            <div className="workspace-meta-chips">
              <button
                type="button"
                className="workspace-chip workspace-chip-copyable"
                onClick={() => onCopyAccount(emailToCopy)}
                title="点击复制账号"
              >
                <span>iCloud: <b>{account?.icloud_email || account?.real_email || '—'}</b></span>
              </button>
              <div className="workspace-chip">
                <span>节点: <b>{account?.host || '默认'}</b></span>
              </div>
              <div className="workspace-chip">
                <span className="status-dot status-dot-green" style={{ width: 6, height: 6 }} />
                <span>验活: <b>{account?.last_validated ? account.last_validated.slice(11, 16) : '刚刚'}</b></span>
              </div>
            </div>
          </div>
        </div>

        <div className="workspace-action-group">
          <button
            type="button"
            className="btn btn-primary"
            onClick={onQuickCreate}
            disabled={quickCreating}
          >
            <IconZap size={14} />
            <span>{quickCreating ? '生成中…' : '快速出号'}</span>
          </button>
          <button
            type="button"
            className="btn btn-secondary"
            onClick={onCreateCustom}
          >
            <IconPlus size={14} />
            <span>自定义别名</span>
          </button>
          <button
            type="button"
            className="btn btn-secondary"
            onClick={onSync}
            disabled={aliasLoading}
            title="与 Apple 官方服务器强制同步最新别名"
          >
            <IconRefresh size={14} className={aliasLoading ? 'animate-spin' : ''} />
            <span>{aliasLoading ? '同步中…' : '同步数据'}</span>
          </button>
          <button
            type="button"
            className="btn btn-secondary"
            onClick={onOpenCookie}
            title="更新 Cookie"
          >
            <IconKey size={14} /> Cookie
          </button>
          <button
            type="button"
            className="btn btn-secondary"
            onClick={onOpenMailbox}
            title="收件箱配置"
          >
            <IconMail size={14} /> IMAP
          </button>
        </div>
      </div>
    </div>
  )
}
