/**
 * [INPUT]: 依赖 react-router-dom 的 NavLink/useNavigate/useLocation, api/client 的 request, api/types 的 AccountSummary, components/ThemeProvider 的 useTheme, components/icons
 * [OUTPUT]: 对外提供 Sidebar 导航组件
 * [POS]: web/src/components 的核心布局组件，贯穿整个应用左侧，提供全局中枢、主题切换与多账号工作台切换
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { memo, useMemo, useRef, useState } from 'react'
import { NavLink, useLocation, useNavigate } from 'react-router-dom'
import type { AccountSummary } from '../api/types'
import { useAuth } from '../auth/AuthProvider'
import { useAccounts } from '../hooks/useAccounts'
import { useTheme } from './ThemeProvider'
import {
  IconAccounts,
  IconAliases,
  IconFileText,
  IconLogout,
  IconMoon,
  IconPlus,
  IconShield,
  IconSliders,
  IconSun,
  IconTag,
  IconZap,
} from './icons'

interface SidebarProps {
  onAddAccount?: () => void
}

const ITEM_HEIGHT = 36 // 每项固定行高

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
      style={{ height: `${ITEM_HEIGHT}px`, boxSizing: 'border-box' }}
    >
      <span className="account-dot" style={{ backgroundColor: dotColor }} />
      <span className="account-label">{acc.name || acc.real_email}</span>
      <span
        className="account-count"
        title={`活跃别名数: ${activeCount} 个 (总计 ${acc.alias_total ?? activeCount} 个)`}
      >
        {activeCount}
      </span>
    </NavLink>
  )
})

export default function Sidebar({ onAddAccount }: SidebarProps) {
  const { accounts } = useAccounts()
  const [search, setSearch] = useState('')
  const [scrollTop, setScrollTop] = useState(0)
  const listRef = useRef<HTMLDivElement>(null)

  const { logout } = useAuth()
  const { theme, toggleTheme } = useTheme()
  const navigate = useNavigate()
  const location = useLocation()

  // 搜索过滤
  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase()
    if (!q) return accounts
    return accounts.filter(
      (a) =>
        (a.name && a.name.toLowerCase().includes(q)) ||
        (a.real_email && a.real_email.toLowerCase().includes(q)) ||
        (a.icloud_email && a.icloud_email.toLowerCase().includes(q)) ||
        (a.id && a.id.toLowerCase().includes(q)),
    )
  }, [accounts, search])

  // 虚拟滚动计算 (视口约维持 15-20 个 DOM 节点)
  const totalHeight = filtered.length * ITEM_HEIGHT
  const rawStartIndex = Math.max(0, Math.floor(scrollTop / ITEM_HEIGHT) - 3)
  const visibleCount = 20
  const maxStartIndex = Math.max(0, filtered.length - visibleCount)
  const startIndex = Math.min(maxStartIndex, rawStartIndex)
  const endIndex = Math.min(filtered.length, startIndex + visibleCount)
  const visibleItems = filtered.slice(startIndex, endIndex)
  const offsetY = startIndex * ITEM_HEIGHT

  async function handleLogout() {
    await logout()
    navigate('/login')
  }

  return (
    <aside className="sidebar-container" aria-label="侧边控制台">
      <div className="sidebar-header">
        <div className="sidebar-brand">
          <span className="sidebar-logo">
            <IconShield size={18} />
          </span>
          <div className="brand-text">
            <span className="brand-title">iCloud HME</span>
            <span className="brand-sub">Console</span>
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
          >
            <IconPlus size={14} />
          </button>
        </div>

        {accounts.length > 5 && (
          <div style={{ padding: '0 8px 6px 8px' }}>
            <input
              type="text"
              className="sidebar-search-input"
              placeholder={`搜索 ${accounts.length} 个账号...`}
              value={search}
              onChange={(e) => {
                setSearch(e.target.value)
                if (listRef.current) listRef.current.scrollTop = 0
                setScrollTop(0)
              }}
              style={{
                width: '100%',
                padding: '4px 8px',
                fontSize: '12px',
                borderRadius: '6px',
                border: '1px solid var(--color-border)',
                background: 'var(--color-bg)',
                color: 'var(--color-text)',
                boxSizing: 'border-box',
                outline: 'none',
              }}
            />
          </div>
        )}

        <div
          className="sidebar-accounts-list"
          ref={listRef}
          onScroll={(e) => setScrollTop(e.currentTarget.scrollTop)}
        >
          {filtered.length === 0 ? (
            <div className="sidebar-empty-accounts">
              {search ? '无匹配账号' : '暂无账号'}
            </div>
          ) : filtered.length <= 25 ? (
            // 数量较少时直接渲染
            filtered.map((acc) => (
              <AccountItem
                key={acc.id}
                acc={acc}
                active={location.pathname === `/workspace/${acc.id}`}
              />
            ))
          ) : (
            // 千号规模时启用虚拟列表视口，仅保留 20 个 DOM 节点
            <div style={{ height: `${totalHeight}px`, position: 'relative', width: '100%' }}>
              <div
                style={{
                  position: 'absolute',
                  top: 0,
                  left: 0,
                  right: 0,
                  transform: `translateY(${offsetY}px)`,
                }}
              >
                {visibleItems.map((acc) => (
                  <AccountItem
                    key={acc.id}
                    acc={acc}
                    active={location.pathname === `/workspace/${acc.id}`}
                  />
                ))}
              </div>
            </div>
          )}
        </div>
      </nav>

      <div className="sidebar-footer">
        <button
          type="button"
          className="sidebar-theme-btn"
          onClick={toggleTheme}
          title={theme === 'dark' ? '切换至浅色模式' : '切换至深色模式'}
        >
          {theme === 'dark' ? <IconSun size={15} /> : <IconMoon size={15} />}
          <span>{theme === 'dark' ? '切换浅色' : '切换深色'}</span>
        </button>
        <button
          type="button"
          className="sidebar-logout-btn"
          onClick={() => void handleLogout()}
          title="退出登录"
        >
          <IconLogout size={15} />
          <span>退出管理台</span>
        </button>
      </div>
    </aside>
  )
}
