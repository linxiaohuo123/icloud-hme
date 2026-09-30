/**
 * [INPUT]: 依赖 react-router-dom 的 Outlet/Link/useLocation, hooks/useAccounts 的 useAccounts, components/Sidebar, components/ErrorBoundary
 * [OUTPUT]: 对外提供 AppShell 现代工作台整体布局壳与账号加载失败重试入口
 * [POS]: web/src/components 的顶层受保护路由骨架，融合左侧暗色导航与右侧主视口，提供全局凭据与待配置告警条
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { Link, Outlet, useLocation } from 'react-router-dom'
import { useAccounts } from '../hooks/useAccounts'
import Sidebar from './Sidebar'
import ErrorBoundary from './ErrorBoundary'

export default function AppShell() {
  const location = useLocation()
  const { accounts, loading, error, refresh } = useAccounts()

  const errorAccounts = accounts.filter((a) => a.status === 'error')
  const pendingAccounts = accounts.filter(
    (a) => a.status !== 'error' && (!a.has_cookies || a.status === 'pending'),
  )

  const hasErrors = errorAccounts.length > 0
  const hasPending = pendingAccounts.length > 0
  const attentionAccounts = hasErrors ? errorAccounts : pendingAccounts
  const firstTargetId = attentionAccounts[0]?.id

  let bannerTheme = 'alert-danger'
  let bannerIcon = '⚠️'
  let bannerTitle = 'Cookie 失效警告'
  let bannerMessage = ''
  let buttonText = '前往重新登录 / 更新'

  if (hasErrors && !hasPending) {
    bannerTheme = 'alert-danger'
    bannerIcon = '⚠️'
    bannerTitle = 'Cookie 失效警告'
    bannerMessage = `检测到 ${errorAccounts.length} 个账号 (${errorAccounts
      .slice(0, 3)
      .map((a) => a.name || a.real_email || a.id)
      .join(', ')}${errorAccounts.length > 3 ? ` 等 ${errorAccounts.length} 个账号` : ''}) 的 Web Cookie 已过期或异常，出号接口已降级不可用！`
    buttonText = '前往重新登录 / 更新'
  } else if (!hasErrors && hasPending) {
    bannerTheme = 'alert-warning'
    bannerIcon = 'ℹ️'
    bannerTitle = '账号待配置提示'
    bannerMessage = `检测到 ${pendingAccounts.length} 个账号 (${pendingAccounts
      .slice(0, 3)
      .map((a) => a.name || a.real_email || a.id)
      .join(', ')}${pendingAccounts.length > 3 ? ` 等 ${pendingAccounts.length} 个账号` : ''}) 尚未配置凭据，完成配置后方可正常出号。`
    buttonText = '前往配置凭据'
  } else if (hasErrors && hasPending) {
    bannerTheme = 'alert-danger'
    bannerIcon = '⚠️'
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
      <Sidebar />
      <main id="main-content" className="app-main-content">
        {error && location.pathname !== '/accounts' && (
          <div className="alert-banner alert-danger" role="alert" style={{ margin: '16px 24px 0', display: 'flex', flexWrap: 'wrap', alignItems: 'center', justifyContent: 'space-between', gap: 12, padding: '12px 16px' }}>
            <span style={{ minWidth: 0, overflowWrap: 'anywhere' }}>账号列表加载失败：{error}</span>
            <button type="button" className="btn btn-sm btn-secondary" disabled={loading} onClick={() => void refresh(true).catch(() => {})}>
              {loading ? '重试中…' : '重试'}
            </button>
          </div>
        )}
        {(hasErrors || hasPending) && (
          <div
            className={`alert-banner ${bannerTheme}`}
            style={{
              margin: '16px 24px 0 24px',
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'space-between',
              gap: '12px',
              borderRadius: '8px',
              padding: '12px 16px',
            }}
          >
            <div style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
              <span style={{ fontSize: '16px' }}>{bannerIcon}</span>
              <span>
                <strong>{bannerTitle}：</strong>
                {bannerMessage}
              </span>
            </div>
            <Link
              to="/accounts"
              onClick={handleActionClick}
              className="btn btn-sm btn-primary"
              style={{ whiteSpace: 'nowrap' }}
            >
              {buttonText}
            </Link>
          </div>
        )}
        <ErrorBoundary>
          <Outlet />
        </ErrorBoundary>
      </main>
    </div>
  )
}
