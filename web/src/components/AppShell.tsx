/**
 * [INPUT]: 依赖 react-router-dom 的 Outlet/Link, hooks/useAccounts 的 useAccounts, components/Sidebar, components/ErrorBoundary
 * [OUTPUT]: 对外提供 AppShell 现代工作台整体布局壳
 * [POS]: web/src/components 的顶层受保护路由骨架，融合左侧暗色导航与右侧主视口，提供全局 Cookie 失效告警条
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { Link, Outlet } from 'react-router-dom'
import { useAccounts } from '../hooks/useAccounts'
import Sidebar from './Sidebar'
import ErrorBoundary from './ErrorBoundary'

export default function AppShell() {
  const { accounts } = useAccounts()
  const expiredAccounts = accounts.filter((a) => a.status === 'error' || !a.has_cookies)

  return (
    <div className="app-layout">
      <a className="skip-link" href="#main-content">
        跳到主要内容
      </a>
      <Sidebar />
      <main id="main-content" className="app-main-content">
        {expiredAccounts.length > 0 && (
          <div
            className="alert-banner alert-danger"
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
              <span style={{ fontSize: '16px' }}>⚠️</span>
              <span>
                <strong>Cookie 失效警告：</strong>
                检测到 {expiredAccounts.length} 个账号 (
                {expiredAccounts.slice(0, 3).map((a) => a.name || a.real_email || a.id).join(', ')}
                {expiredAccounts.length > 3 ? ` 等 ${expiredAccounts.length} 个账号` : ''}
                ) 的 Web Cookie 已过期或异常，出号接口已降级不可用！
              </span>
            </div>
            <Link
              to="/accounts"
              className="btn btn-sm btn-primary"
              style={{ whiteSpace: 'nowrap' }}
            >
              前往重新登录 / 更新
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
