/**
 * [INPUT]: 依赖 react-router-dom 的 Outlet/Link/useLocation, hooks/useAccounts 的 useAccounts, components/Sidebar, components/ErrorBoundary
 * [OUTPUT]: 对外提供 AppShell 现代工作台整体布局壳与账号加载失败重试入口
 * [POS]: web/src/components 的顶层受保护路由骨架，融合左侧导航 (窄屏为抽屉 + 顶栏菜单按钮) 与右侧主视口，提供全局凭据与待配置告警条
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useCallback, useEffect, useRef, useState } from 'react'
import { Link, Outlet, useLocation } from 'react-router-dom'
import { useAccounts } from '../hooks/useAccounts'
import Sidebar from './Sidebar'
import ErrorBoundary from './ErrorBoundary'
import { IconAlert, IconBrandLogo, IconInfo, IconMenu } from './icons'

export default function AppShell() {
  const location = useLocation()
  const { accounts, loading, error, refresh } = useAccounts()
  const [navOpen, setNavOpen] = useState(false)
  const menuButtonRef = useRef<HTMLButtonElement>(null)
  const mainRef = useRef<HTMLElement>(null)

  // 路由切换时让右侧主内容滚动容器回到顶部
  useEffect(() => {
    if (!mainRef.current) return
    if (typeof mainRef.current.scrollTo === 'function') {
      mainRef.current.scrollTo({ top: 0, left: 0, behavior: 'instant' })
    } else {
      mainRef.current.scrollTop = 0
    }
  }, [location.pathname])

  // 窄屏抽屉：任意一次导航（含同路径、仅查询参数变化）后在渲染期收起
  const [navKey, setNavKey] = useState(location.key)
  if (navKey !== location.key) {
    setNavKey(location.key)
    setNavOpen(false)
  }

  const closeNav = useCallback((restoreFocus: boolean) => {
    setNavOpen(false)
    if (restoreFocus) menuButtonRef.current?.focus()
  }, [])

  const errorAccounts = accounts.filter((a) => a.status === 'error')
  const pendingAccounts = accounts.filter(
    (a) => a.status !== 'error' && (!a.has_cookies || a.status === 'pending'),
  )

  const hasErrors = errorAccounts.length > 0
  const hasPending = pendingAccounts.length > 0
  const attentionAccounts = hasErrors ? errorAccounts : pendingAccounts
  const firstTargetId = attentionAccounts[0]?.id

  let bannerTheme = 'alert-danger'
  let bannerIcon = <IconAlert size={16} />
  let bannerTitle = 'Cookie 失效警告'
  let bannerMessage = ''
  let buttonText = '前往重新登录 / 更新'

  if (hasErrors && !hasPending) {
    bannerTheme = 'alert-danger'
    bannerIcon = <IconAlert size={16} />
    bannerTitle = 'Cookie 失效警告'
    bannerMessage = `检测到 ${errorAccounts.length} 个账号 (${errorAccounts
      .slice(0, 3)
      .map((a) => a.name || a.real_email || a.id)
      .join(', ')}${errorAccounts.length > 3 ? ` 等 ${errorAccounts.length} 个账号` : ''}) 的 Web Cookie 已过期或异常，出号接口已降级不可用！`
    buttonText = '前往重新登录 / 更新'
  } else if (!hasErrors && hasPending) {
    bannerTheme = 'alert-warning'
    bannerIcon = <IconInfo size={16} />
    bannerTitle = '账号待配置提示'
    bannerMessage = `检测到 ${pendingAccounts.length} 个账号 (${pendingAccounts
      .slice(0, 3)
      .map((a) => a.name || a.real_email || a.id)
      .join(', ')}${pendingAccounts.length > 3 ? ` 等 ${pendingAccounts.length} 个账号` : ''}) 尚未配置凭据，完成配置后方可正常出号。`
    buttonText = '前往配置凭据'
  } else if (hasErrors && hasPending) {
    bannerTheme = 'alert-danger'
    bannerIcon = <IconAlert size={16} />
    bannerTitle = '凭据异常与待配置提醒'
    bannerMessage = `检测到 ${errorAccounts.length} 个账号 Cookie 失效，${pendingAccounts.length} 个账号尚未配置凭据。`
    buttonText = '前往处理账号'
  }

  const handleActionClick = (e: React.MouseEvent) => {
    if (location.pathname === '/accounts' && firstTargetId) {
      e.preventDefault()
      const row = document.querySelector(`[data-account-id="${firstTargetId}"]`)
      if (row) {
        row.scrollIntoView({ behavior: 'smooth', block: 'center' })
        row.classList.remove('row-highlight')
        void (row as HTMLElement).offsetWidth
        row.classList.add('row-highlight')
        setTimeout(() => row.classList.remove('row-highlight'), 2000)
      }
    }
  }

  return (
    <div className="app-layout">
      <a className="skip-link" href="#main-content">
        跳到主要内容
      </a>
      <div className="app-topbar">
        <button
          ref={menuButtonRef}
          type="button"
          className="app-topbar-menu"
          onClick={() => setNavOpen(true)}
          aria-label="打开导航菜单"
          aria-expanded={navOpen}
          aria-controls="app-sidebar"
        >
          <IconMenu size={20} />
        </button>
        <span className="sidebar-logo" aria-hidden="true">
          <IconBrandLogo size={22} />
        </span>
      </div>
      <Sidebar open={navOpen} onClose={closeNav} />
      <main id="main-content" ref={mainRef} className="app-main-content" inert={navOpen || undefined}>
        {((error && location.pathname !== '/accounts') || hasErrors || hasPending) && (
          <div className="app-banners">
            {error && location.pathname !== '/accounts' && (
              <div className="alert-banner alert-danger app-banner" role="alert">
                <span className="app-banner-body">账号列表加载失败：{error}</span>
                <button type="button" className="btn btn-sm btn-secondary" disabled={loading} onClick={() => void refresh(true).catch(() => {})}>
                  {loading ? '重试中…' : '重试'}
                </button>
              </div>
            )}
            {(hasErrors || hasPending) && (
              <div className={`alert-banner ${bannerTheme} app-banner`}>
                <div className="app-banner-body">
                  {bannerIcon}
                  <span>
                    <strong>{bannerTitle}：</strong>
                    {bannerMessage}
                  </span>
                </div>
                <Link to="/accounts" onClick={handleActionClick} className="btn btn-sm btn-secondary">
                  {buttonText}
                </Link>
              </div>
            )}
          </div>
        )}
        <ErrorBoundary>
          <Outlet />
        </ErrorBoundary>
      </main>
    </div>
  )
}
