/**
 * [INPUT]: 依赖 react-router-dom, auth/AuthProvider, components/ToastProvider, components/ThemeProvider, components/AppShell 及各 pages
 * [OUTPUT]: 对外提供 App 根组件与主路由拓扑
 * [POS]: web/src 的总路由与全局上下文挂载点
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom'
import { AuthProvider, useAuth } from './auth/AuthProvider'
import { ToastProvider } from './components/ToastProvider'
import { ThemeProvider } from './components/ThemeProvider'
import AppShell from './components/AppShell'
import LoginPage from './pages/LoginPage'
import AccountsPage from './pages/AccountsPage'
import AccountWorkspace from './pages/AccountWorkspace'
import AliasesPage from './pages/AliasesPage'
import InboxPage from './pages/InboxPage'
import SchedulePage from './pages/SchedulePage'
import BusinessTagsPage from './pages/BusinessTagsPage'
import UsedAliasesPage from './pages/UsedAliasesPage'
import SettingsPage from './pages/SettingsPage'

function ProtectedLayout() {
  const { status } = useAuth()
  if (status === 'checking') {
    return <p className="empty-state" aria-busy="true">加载中…</p>
  }
  if (status === 'anonymous') {
    return <Navigate to="/login" replace />
  }
  return <AppShell />
}

export default function App() {
  return (
    <ThemeProvider>
      <BrowserRouter>
        <AuthProvider>
          <ToastProvider>
            <Routes>
              <Route path="/login" element={<LoginPage />} />
              <Route element={<ProtectedLayout />}>
                <Route path="/schedule" element={<SchedulePage />} />
                <Route path="/tags" element={<BusinessTagsPage />} />
                <Route path="/used" element={<UsedAliasesPage />} />
                <Route path="/accounts" element={<AccountsPage />} />
                <Route path="/workspace/:accountId" element={<AccountWorkspace />} />
                <Route path="/aliases" element={<AliasesPage />} />
                <Route path="/inbox" element={<InboxPage />} />
                <Route path="/settings" element={<SettingsPage />} />
                <Route path="*" element={<Navigate to="/schedule" replace />} />
              </Route>
            </Routes>
          </ToastProvider>
        </AuthProvider>
      </BrowserRouter>
    </ThemeProvider>
  )
}
