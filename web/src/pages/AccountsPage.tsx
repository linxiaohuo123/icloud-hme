/**
 * [INPUT]: 依赖 api/client, api/types, react-dom createPortal, components 下各 Dialog 与 ToastProvider, utils/date, components/icons
 * [OUTPUT]: 对外提供 AccountsPage 页面组件 (卡片顶栏内嵌状态胶囊 + 统一 1240px 容器 + 账号凭据与生命周期管理)
 * [POS]: web/src/pages 的核心页面，负责管理 iCloud 账号、凭据与快捷跳转；AccountActions 采用单次确定性 useLayoutEffect 计算定位并重置收起状态
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { Link } from 'react-router-dom'
import { request, ApiError } from '../api/client'
import type { AccountSummary } from '../api/types'
import { useAccounts, invalidateAccounts } from '../hooks/useAccounts'
import AsyncState from '../components/AsyncState'
import AccountFormDialog from '../components/AccountFormDialog'
import CookieDialog from '../components/CookieDialog'
import ICloudLoginDialog from '../components/ICloudLoginDialog'
import AppPasswordDialog from '../components/AppPasswordDialog'
import ProxyDialog from '../components/ProxyDialog'
import MailboxDialog from '../components/MailboxDialog'
import ConfirmDialog from '../components/ConfirmDialog'
import { useToast } from '../components/ToastProvider'
import { formatDate } from '../utils/date'
import {
  IconAccounts,
  IconAliases,
  IconPlus,
  IconEdit,
  IconTrash,
  IconKey,
  IconMail,
  IconZap,
  IconMoreHorizontal,
  IconCookie,
  IconGlobe,
  IconSliders,
} from '../components/icons'

function StatusBadge({ status }: { status: string }) {
  const meta: Record<string, { text: string; className: string }> = {
    active: { text: '正常运行', className: 'status-pill active' },
    pending: { text: '待配置', className: 'status-pill pending' },
    error: { text: '异常', className: 'status-pill error' },
  }
  const current = meta[status] ?? { text: status, className: 'status-pill' }
  return (
    <span className={current.className}>
      <span className="status-dot" />
      {current.text}
    </span>
  )
}

function CredentialCell({ acc }: { acc: AccountSummary }) {
  const parts: string[] = []
  if (acc.has_cookies) parts.push('Cookie')
  if (acc.has_app_password) parts.push('App专用密码')
  if (acc.mailbox) parts.push(`转发至 ${acc.mailbox.email}`)
  if (acc.has_proxy) parts.push('代理网络')

  if (parts.length === 0) {
    return (
      <span className="text-secondary text-xs" title="尚未录入该账号的任何凭据">
        未配置
      </span>
    )
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '3px' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: '6px', flexWrap: 'wrap' }}>
        <span className="badge badge-active" style={{ fontSize: '11px', padding: '1px 6px' }}>
          已配置
        </span>
        <span className="cell-secondary" style={{ fontSize: '12px' }}>
          {parts.join(' · ')}
        </span>
      </div>
    </div>
  )
}


interface AccountActionsProps {
  acc: AccountSummary
  onEdit: () => void
  onCookie: () => void
  onLogin: () => void
  onAppPwd: () => void
  onMailbox: () => void
  onProxy: () => void
  onDelete: () => void
}

function AccountActions({
  acc,
  onEdit,
  onCookie,
  onLogin,
  onAppPwd,
  onMailbox,
  onProxy,
  onDelete,
}: AccountActionsProps) {
  const [open, setOpen] = useState(false)
  const moreRef = useRef<HTMLButtonElement>(null)
  const [pos, setPos] = useState<{ top?: number; bottom?: number; right: number; maxHeight: number } | null>(null)

  // 表格容器 overflow 裁剪会截断 absolute 弹层，改用 Portal + fixed 挂在 body 上
  // 通过 CSS 锚定顶底边缘自然流式展开，彻底消除未挂载时测高的死代码与残影
  useLayoutEffect(() => {
    if (!open) {
      setPos(null)
      return
    }
    const btn = moreRef.current
    if (!btn) return
    const rect = btn.getBoundingClientRect()
    const spaceBelow = window.innerHeight - rect.bottom
    const openUpward = spaceBelow < 280 && rect.top > spaceBelow

    setPos({
      top: openUpward ? undefined : rect.bottom + 4,
      bottom: openUpward ? window.innerHeight - rect.top + 4 : undefined,
      right: window.innerWidth - rect.right,
      maxHeight: Math.max(120, openUpward ? rect.top - 16 : spaceBelow - 16),
    })
  }, [open])

  useEffect(() => {
    if (!open) return
    const close = () => setOpen(false)
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') setOpen(false)
    }
    window.addEventListener('scroll', close, true)
    window.addEventListener('resize', close)
    window.addEventListener('keydown', onKey)
    return () => {
      window.removeEventListener('scroll', close, true)
      window.removeEventListener('resize', close)
      window.removeEventListener('keydown', onKey)
    }
  }, [open])

  return (
    <div className="action-cell">
      <Link
        to={`/workspace/${encodeURIComponent(acc.id)}`}
        className="btn-action-primary"
        title="进入该账号工作台"
      >
        <IconZap size={13} />
        <span>工作台</span>
        <span className="alias-count-pill" title="活跃别名数">
          {acc.alias_active}
        </span>
      </Link>

      <Link
        to={`/workspace/${encodeURIComponent(acc.id)}?tab=inbox`}
        className="btn-action-icon"
        title="查看收件箱"
      >
        <IconMail size={15} />
      </Link>

      <div className="dropdown-container">
        <button
          type="button"
          ref={moreRef}
          className={`btn-action-icon ${open ? 'active' : ''}`}
          onClick={() => setOpen((prev) => !prev)}
          title="更多操作"
          aria-label="更多操作"
          aria-expanded={open}
        >
          <IconMoreHorizontal size={15} />
        </button>

        {open && pos && createPortal(
          <>
            <div className="dropdown-backdrop" role="presentation" onClick={() => setOpen(false)} />
            <div
              className="dropdown-popover"
              style={{
                position: 'fixed',
                top: pos.top,
                bottom: pos.bottom,
                right: pos.right,
                maxHeight: pos.maxHeight,
                overflowY: 'auto',
              }}
            >
              <div className="dropdown-section-title">凭据与认证</div>
              <button type="button" onClick={() => { setOpen(false); onCookie() }}>
                <IconCookie size={14} />
                <span>更新 Cookie</span>
              </button>
              <button type="button" onClick={() => { setOpen(false); onLogin() }}>
                <IconKey size={14} />
                <span>iCloud 登录</span>
              </button>
              <button type="button" onClick={() => { setOpen(false); onAppPwd() }}>
                <IconSliders size={14} />
                <span>设置 App 密码</span>
              </button>
              <button type="button" onClick={() => { setOpen(false); onMailbox() }}>
                <IconMail size={14} />
                <span>接入收件邮箱</span>
              </button>

              <div className="dropdown-divider" />
              <div className="dropdown-section-title">网络与基础</div>
              <button type="button" onClick={() => { setOpen(false); onProxy() }}>
                <IconGlobe size={14} />
                <span>设置代理</span>
              </button>
              <button type="button" onClick={() => { setOpen(false); onEdit() }}>
                <IconEdit size={14} />
                <span>编辑</span>
              </button>

              <div className="dropdown-divider" />
              <button type="button" className="menu-item-danger" onClick={() => { setOpen(false); onDelete() }}>
                <IconTrash size={14} />
                <span>删除</span>
              </button>
            </div>
          </>,
          document.body,
        )}
      </div>
    </div>
  )
}

export default function AccountsPage() {
  const { accounts, loading, error, refresh } = useAccounts()

  // dialog 状态
  const [formOpen, setFormOpen] = useState(false)
  const [editing, setEditing] = useState<AccountSummary | null>(null)
  const [cookieFor, setCookieFor] = useState<AccountSummary | null>(null)
  const [loginFor, setLoginFor] = useState<AccountSummary | null>(null)
  const [appPwdFor, setAppPwdFor] = useState<AccountSummary | null>(null)
  const [proxyFor, setProxyFor] = useState<AccountSummary | null>(null)
  const [mailboxFor, setMailboxFor] = useState<AccountSummary | null>(null)
  const [deleteFor, setDeleteFor] = useState<AccountSummary | null>(null)
  const [deleting, setDeleting] = useState(false)

  const { show } = useToast()

  function handleRetry() {
    void refresh(true)
  }

  async function handleDelete() {
    if (!deleteFor) return
    const targetId = deleteFor.id
    setDeleting(true)
    try {
      await request(`/api/accounts/${targetId}`, { method: 'DELETE' })
      setDeleteFor(null)
      show('账号已删除')
      invalidateAccounts(targetId)
    } catch (err) {
      show(err instanceof ApiError ? err.message : '删除失败')
    } finally {
      setDeleting(false)
    }
  }

  const { totalActiveAliases, totalAliases, activeAccountsCount } = useMemo(() => {
    let totalActive = 0
    let totalAll = 0
    let activeAccs = 0
    for (const acc of accounts) {
      totalActive += acc.alias_active || 0
      totalAll += acc.alias_total || 0
      if (acc.status === 'active' && acc.has_cookies) {
        activeAccs++
      }
    }
    return {
      totalActiveAliases: totalActive,
      totalAliases: totalAll,
      activeAccountsCount: activeAccs,
    }
  }, [accounts])

  return (
    <div className="page-container">
      {/* 顶部标题与主要操作 */}
      <div className="page-header">
        <div>
          <h1 className="page-title">账号管理</h1>
          <p className="page-desc">管理已接入的 iCloud 母账号、登录凭据与可用别名</p>
        </div>
        <div>
          <button
            type="button"
            className="btn btn-primary"
            onClick={() => {
              setEditing(null)
              setFormOpen(true)
            }}
          >
            <IconPlus size={15} />
            <span>添加账号</span>
          </button>
        </div>
      </div>

      <div className="card">
        <div className="card-header">
          <h2 className="card-title">
            <IconAccounts size={16} /> 账号列表 ({accounts.length})
          </h2>
          <div className="card-header-stats">
            <span
              className={`card-stat-pill ${
                accounts.length > 0 && activeAccountsCount === accounts.length
                  ? 'is-healthy'
                  : 'is-warning'
              }`}
            >
              <span className="status-ribbon-dot" />
              <span>
                {accounts.length === 0
                  ? '暂无账号'
                  : activeAccountsCount === accounts.length
                    ? '全部凭据有效'
                    : `${accounts.length - activeAccountsCount} 个需更新`}
              </span>
            </span>

            <span className="card-stat-pill">
              <IconAliases size={13} />
              <span>
                已分配 <strong>{totalAliases}</strong> 别名
                {accounts.length > 0 && (
                  <span className="card-stat-dim"> (上限 {accounts.length * 750})</span>
                )}
              </span>
            </span>

            <span className="card-stat-pill is-success">
              <IconZap size={13} />
              <span>
                <strong>{totalActiveAliases}</strong> 随时可用
              </span>
            </span>
          </div>
        </div>
        <AsyncState
          loading={loading}
          error={error || ''}
          empty={accounts.length === 0}
          emptyText="暂无账号，点击“添加账号”开始"
          onRetry={handleRetry}
        >
          <div className="table-responsive">
            <table className="table">
              <thead>
                <tr>
                  <th style={{ minWidth: '150px' }}>账号备注</th>
                  <th style={{ minWidth: '220px' }}>登录邮箱 / 账号ID</th>
                  <th style={{ width: '110px' }}>运行状态</th>
                  <th style={{ width: '130px' }}>别名 (可用/总数)</th>
                  <th style={{ minWidth: '200px' }}>已配凭据</th>
                  <th style={{ width: '150px' }}>最后检查时间</th>
                  <th style={{ width: '140px', textAlign: 'right' }}>快捷操作</th>
                </tr>
              </thead>
              <tbody>
                {accounts.map((acc) => (
                  <tr key={acc.id}>
                    <td>
                      <div className="cell-main">{acc.name}</div>
                      {acc.status_message && (
                        <span className="hint" style={{ display: 'block' }}>
                          {acc.status_message}
                        </span>
                      )}
                    </td>
                    <td>
                      <div>{acc.icloud_email || acc.real_email || '—'}</div>
                      <span className="cell-secondary">{acc.id}</span>
                    </td>
                    <td>
                      <StatusBadge status={acc.status} />
                    </td>
                    <td>
                      <div>
                        <span className="cell-strong">
                          {acc.alias_active} / {acc.alias_total}
                        </span>
                      </div>
                      <span className="cell-secondary" style={{ fontSize: '11px', display: 'block' }}>
                        {acc.alias_active} 个可用收信
                      </span>
                    </td>
                    <td>
                      <CredentialCell acc={acc} />
                    </td>
                    <td className="text-secondary text-xs">{formatDate(acc.last_validated)}</td>
                    <td style={{ textAlign: 'right' }}>
                      <AccountActions
                        acc={acc}
                        onEdit={() => { setEditing(acc); setFormOpen(true) }}
                        onCookie={() => setCookieFor(acc)}
                        onLogin={() => setLoginFor(acc)}
                        onAppPwd={() => setAppPwdFor(acc)}
                        onMailbox={() => setMailboxFor(acc)}
                        onProxy={() => setProxyFor(acc)}
                        onDelete={() => setDeleteFor(acc)}
                      />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </AsyncState>
      </div>

      {formOpen && (
        <AccountFormDialog
          open={formOpen}
          onClose={() => setFormOpen(false)}
          onSaved={() => {
            const targetId = editing?.id
            setFormOpen(false)
            show('账号已保存')
            invalidateAccounts(targetId)
          }}
          editing={editing ? { id: editing.id, name: editing.name, icloudEmail: editing.icloud_email, host: editing.host } : null}
        />
      )}
      {cookieFor && (
        <CookieDialog
          accountId={cookieFor.id}
          open
          onClose={() => setCookieFor(null)}
          onSaved={() => {
            const targetId = cookieFor.id
            setCookieFor(null)
            show('Cookie 已更新')
            invalidateAccounts(targetId)
          }}
        />
      )}
      {loginFor && (
        <ICloudLoginDialog
          accountId={loginFor.id}
          accountEmail={loginFor.icloud_email || loginFor.real_email}
          accountName={loginFor.name}
          open
          onClose={() => setLoginFor(null)}
          onSaved={() => {
            const targetId = loginFor.id
            setLoginFor(null)
            show('登录成功')
            invalidateAccounts(targetId)
          }}
        />
      )}
      {appPwdFor && (
        <AppPasswordDialog
          accountId={appPwdFor.id}
          defaultEmail={appPwdFor.icloud_email || appPwdFor.real_email}
          open
          onClose={() => setAppPwdFor(null)}
          onSaved={() => {
            const targetId = appPwdFor.id
            setAppPwdFor(null)
            show('App 专用密码已设置')
            invalidateAccounts(targetId)
          }}
        />
      )}
      {proxyFor && (
        <ProxyDialog
          accountId={proxyFor.id}
          open
          onClose={() => setProxyFor(null)}
          onSaved={() => {
            const targetId = proxyFor.id
            setProxyFor(null)
            show('代理已更新')
            invalidateAccounts(targetId)
          }}
        />
      )}
      {mailboxFor && (
        <MailboxDialog
          accountId={mailboxFor.id}
          current={mailboxFor.mailbox}
          open
          onClose={() => setMailboxFor(null)}
          onSaved={() => {
            const targetId = mailboxFor.id
            setMailboxFor(null)
            show('收件邮箱已接入')
            invalidateAccounts(targetId)
          }}
        />
      )}
      {deleteFor && (
        <ConfirmDialog
          title="删除账号"
          message={`将移除本地账号配置「${deleteFor.name}」，不会删除 Apple 账号本身。`}
          requireText={deleteFor.name}
          open
          busy={deleting}
          onClose={() => setDeleteFor(null)}
          onConfirm={() => void handleDelete()}
        />
      )}
    </div>
  )
}
