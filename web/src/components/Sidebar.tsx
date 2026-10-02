/**
 * [INPUT]: 依赖 react-router-dom 的 NavLink/useNavigate/useLocation, api/client 的 request, api/types 的 AccountSummary, components/ThemeProvider 的 useTheme, components/icons
 * [OUTPUT]: 对外提供 Sidebar 导航组件 (props: onAddAccount, open, onClose)，区分账号加载中、失败与空列表；窄屏抽屉打开时聚焦首个导航项，Escape 或切换到桌面宽度时收起
 * [POS]: web/src/components 的核心布局组件，贯穿整个应用左侧，提供全局中枢、主题切换与多账号工作台切换
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { memo, useEffect, useRef } from 'react'
import { NavLink, useLocation, useNavigate } from 'react-router-dom'
import type { AccountSummary } from '../api/types'
import { useAuth } from '../auth/AuthProvider'
import { ApiError } from '../api/client'
import { useAccounts } from '../hooks/useAccounts'
import { useTheme } from './ThemeProvider'
import { useToast } from './ToastProvider'
import {
  IconAccounts,
  IconAliases,
  IconBrandLogo,
  IconFileText,
  IconLogout,
  IconMoon,
  IconPlus,
  IconSliders,
  IconSun,
  IconTag,
  IconZap,
} from './icons'

interface SidebarProps {
  onAddAccount?: () => void
  /** 窄屏抽屉是否展开；桌面端始终常驻，忽略此值 */
  open?: boolean
  /** 关闭抽屉；restoreFocus 为 true 时把焦点还给顶栏菜单按钮 */
  onClose?: (restoreFocus: boolean) => void
}

const AccountItem = memo(function AccountItem({ acc, active }: { acc: AccountSummary; active: boolean }) {
  const dotColor =
    acc.status === 'active'
      ? 'var(--color-success)'
      : acc.status === 'pending'
        ? 'var(--color-warning)'
        : 'var(--color-danger)'

  const activeCount = acc.alias_active ?? 0

  return (
    <NavLink
      to={`/workspace/${encodeURIComponent(acc.id)}`}
      className={`sidebar-account-item ${active ? 'active' : ''}`}
      title={`${acc.name || acc.real_email} (${acc.status}) - 活跃别名: ${activeCount} 个`}
    >
      <span className="account-dot" style={{ backgroundColor: dotColor }} />
      <span className="account-label">{acc.name || acc.real_email}</span>
      <span
        className="account-count font-mono"
        title={`活跃别名数: ${activeCount} 个 (总计 ${acc.alias_total ?? activeCount} 个)`}
      >
        {activeCount}
      </span>
    </NavLink>
  )
})

export default function Sidebar({ onAddAccount, open = false, onClose }: SidebarProps) {
  const { accounts, loading, error } = useAccounts()
  const listRef = useRef<HTMLDivElement>(null)
  const asideRef = useRef<HTMLElement>(null)

  const { logout } = useAuth()
  const { show } = useToast()
  const { theme, toggleTheme } = useTheme()
  const navigate = useNavigate()
  const location = useLocation()

  useEffect(() => {
    if (!open || !onClose) return
    asideRef.current?.querySelector<HTMLElement>('.sidebar-nav a')?.focus()
    const onKey = (event: KeyboardEvent) => {
      // 抽屉上方还有弹窗时，Escape 只关闭最上层的弹窗
      if (event.key !== 'Escape' || event.defaultPrevented) return
      if (document.querySelector('[role="dialog"][aria-modal="true"]')) return
      onClose(true)
    }
    const desktop = typeof window.matchMedia === 'function' ? window.matchMedia('(min-width: 901px)') : null
    const onViewportChange = (event: MediaQueryListEvent) => {
      if (event.matches) onClose(false)
    }
    document.addEventListener('keydown', onKey)
    desktop?.addEventListener('change', onViewportChange)
    return () => {
      document.removeEventListener('keydown', onKey)
      desktop?.removeEventListener('change', onViewportChange)
    }
  }, [open, onClose])

  async function handleLogout() {
    try {
      await logout()
      navigate('/login')
    } catch (err) {
      show(err instanceof ApiError ? err.message : '退出登录失败，请重试')
    }
  }

  return (
    <>
    {open && (
      <button
        type="button"
        className="sidebar-overlay is-open"
        aria-label="关闭导航菜单"
        tabIndex={-1}
        onClick={() => onClose?.(true)}
      />
    )}
    <aside
      ref={asideRef}
      id="app-sidebar"
      className={`sidebar-container${open ? ' is-open' : ''}`}
      aria-label="侧边控制台"
    >
      <div className="sidebar-header">
        <div className="sidebar-brand">
          <span className="sidebar-logo">
            <IconBrandLogo size={24} />
          </span>
          <div className="brand-text">
            <span className="brand-title">iCloud HME Plus</span>
          </div>
        </div>
      </div>

      <nav className="sidebar-nav">
        <div className="sidebar-section-title">系统管理</div>
        <NavLink
          to="/accounts"
          className={({ isActive }) => `sidebar-nav-item ${isActive ? 'active' : ''}`}
        >
          <IconAccounts size={16} />
          <span>账号管理</span>
        </NavLink>
        <NavLink
          to="/aliases"
          className={({ isActive }) => `sidebar-nav-item ${isActive ? 'active' : ''}`}
        >
          <IconAliases size={16} />
          <span>别名号池</span>
        </NavLink>
        <NavLink
          to="/schedule"
          className={({ isActive }) => `sidebar-nav-item ${isActive ? 'active' : ''}`}
        >
          <IconZap size={16} />
          <span>定时任务</span>
        </NavLink>
        <NavLink
          to="/used"
          className={({ isActive }) => `sidebar-nav-item ${isActive ? 'active' : ''}`}
        >
          <IconFileText size={16} />
          <span>出号记录</span>
        </NavLink>
        <NavLink
          to="/tags"
          className={({ isActive }) => `sidebar-nav-item ${isActive ? 'active' : ''}`}
        >
          <IconTag size={16} />
          <span>业务与令牌</span>
        </NavLink>
        <NavLink
          to="/settings"
          className={({ isActive }) => `sidebar-nav-item ${isActive ? 'active' : ''}`}
        >
          <IconSliders size={16} />
          <span>系统设置</span>
        </NavLink>

        <div className="sidebar-section-header">
          <span className="sidebar-section-title">账号工作台</span>
          <button
            type="button"
            className="sidebar-add-btn"
            onClick={() => {
              if (onAddAccount) onAddAccount()
              else navigate('/accounts?add=true')
            }}
            title="添加新账号"
            aria-label="添加新账号"
          >
            <IconPlus size={14} />
          </button>
        </div>

        <div className="sidebar-accounts-list" ref={listRef}>
          {accounts.length === 0 ? (
            <div className="sidebar-empty-accounts">
              {loading ? '账号加载中…' : error ? '账号加载失败' : '暂无账号'}
            </div>
          ) : (
            accounts.map((acc) => (
              <AccountItem
                key={acc.id}
                acc={acc}
                active={
                  location.pathname === `/workspace/${encodeURIComponent(acc.id)}` ||
                  location.pathname === `/workspace/${acc.id}`
                }
              />
            ))
          )}
        </div>

        {accounts.length > 5 && (
          <div className="sidebar-all-accounts-wrapper">
            <NavLink
              to="/accounts"
              className="sidebar-accounts-footer-link"
              title="前往账号管理页查看全部账号大盘"
            >
              <IconAccounts size={14} />
              <span>全部账号大盘</span>
              <span className="account-count font-mono">{accounts.length}</span>
            </NavLink>
          </div>
        )}
      </nav>

      <div className="sidebar-footer">
        <div className="sidebar-session-card">
          <div className="session-user-badge">
            <span className="session-status-dot" aria-hidden="true" />
            <div className="session-user-info">
              <span className="session-user-name">管理员</span>
              <span className="session-user-role">已加密安全会话</span>
            </div>
          </div>
          <div className="session-actions">
            <button
              type="button"
              className="session-action-btn"
              onClick={toggleTheme}
              title={theme === 'dark' ? '切换至浅色模式' : '切换至深色模式'}
              aria-label={theme === 'dark' ? '切换至浅色模式' : '切换至深色模式'}
            >
              {theme === 'dark' ? <IconSun size={15} /> : <IconMoon size={15} />}
            </button>
            <button
              type="button"
              className="session-action-btn is-logout"
              onClick={() => void handleLogout()}
              title="退出管理台"
              aria-label="退出管理台"
            >
              <IconLogout size={15} />
            </button>
          </div>
        </div>
      </div>
    </aside>
    </>
  )
}
