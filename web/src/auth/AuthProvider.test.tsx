import { http, HttpResponse } from 'msw'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Routes, Route } from 'react-router-dom'
import { beforeEach, describe, expect, it } from 'vitest'
import type { ReactNode } from 'react'
import { AuthProvider, useAuth } from './AuthProvider'
import { server } from '../test/server'
import LoginPage from '../pages/LoginPage'
import { setCSRFToken } from '../api/client'

function ProtectedProbe() {
  const { status } = useAuth()
  if (status !== 'authenticated') return <p>loading…</p>
  return <p data-testid="protected">已登录页面</p>
}

/** 模拟 App.tsx 的认证分支逻辑 */
function TestApp({ children }: { children?: ReactNode }) {
  const { status } = useAuth()
  if (status === 'checking') return <p>loading…</p>
  if (status === 'anonymous') return <LoginPage />
  return <>{children ?? <ProtectedProbe />}</>
}

function renderApp(initialPath = '/accounts') {
  return render(
    <MemoryRouter initialEntries={[initialPath]}>
      <AuthProvider>
        <Routes>
          <Route path="/login" element={<TestApp />} />
          <Route path="/accounts" element={<TestApp />} />
        </Routes>
      </AuthProvider>
    </MemoryRouter>,
  )
}

describe('AuthProvider + LoginPage', () => {
  beforeEach(() => {
    setCSRFToken(null)
    server.resetHandlers()
  })

  it('首次访问显示 loading,会话校验通过后进入受保护页', async () => {
    server.use(
      http.get('/api/auth/session', () =>
        HttpResponse.json({
          success: true,
          data: {
            csrf_token: 'csrf-abc',
            expires_at: '2026-08-05T22:00:00+08:00',
          },
        }),
      ),
    )
    renderApp()
    expect(screen.getByText('loading…')).toBeInTheDocument()
    expect(await screen.findByTestId('protected')).toBeInTheDocument()
  })

  it('会话无效(401)跳转登录页', async () => {
    server.use(
      http.get('/api/auth/session', () =>
        HttpResponse.json(
          { success: false, code: 'AUTH_REQUIRED', message: '请先登录' },
          { status: 401 },
        ),
      ),
    )
    renderApp()
    expect(await screen.findByRole('heading', { name: 'iCloud HME 管理台' })).toBeInTheDocument()
  })

  it('登录成功进入 /accounts', async () => {
    server.use(
      http.get('/api/auth/session', () =>
        HttpResponse.json(
          { success: false, code: 'AUTH_REQUIRED', message: '请先登录' },
          { status: 401 },
        ),
      ),
      http.post('/api/auth/login', () =>
        HttpResponse.json({
          success: true,
          data: {
            csrf_token: 'csrf-new',
            expires_at: '2026-08-05T22:00:00+08:00',
          },
        }),
      ),
    )
    renderApp()
    const user = userEvent.setup()
    await screen.findByRole('heading', { name: 'iCloud HME 管理台' })
    await user.type(screen.getByLabelText(/管理员密码/), 'admin-pass-2026')
    await user.click(screen.getByRole('button', { name: /登录/ }))
    expect(await screen.findByTestId('protected')).toBeInTheDocument()
  })

  it('错误密码显示服务器消息且不回显密码', async () => {
    server.use(
      http.get('/api/auth/session', () =>
        HttpResponse.json(
          { success: false, code: 'AUTH_REQUIRED', message: '请先登录' },
          { status: 401 },
        ),
      ),
      http.post('/api/auth/login', () =>
        HttpResponse.json(
          { success: false, code: 'INVALID_CREDENTIALS', message: '管理员密码错误' },
          { status: 401 },
        ),
      ),
    )
    renderApp()
    const user = userEvent.setup()
    await screen.findByRole('heading', { name: 'iCloud HME 管理台' })
    await user.type(screen.getByLabelText(/管理员密码/), 'wrong-password')
    await user.click(screen.getByRole('button', { name: /登录/ }))
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('管理员密码错误')
    expect(alert).not.toHaveTextContent('wrong-password')
  })

  it('登录后刷新页面可恢复会话', async () => {
    server.use(
      http.get('/api/auth/session', () =>
        HttpResponse.json({
          success: true,
          data: {
            csrf_token: 'csrf-abc',
            expires_at: '2026-08-05T22:00:00+08:00',
          },
        }),
      ),
    )
    const { unmount } = renderApp()
    expect(await screen.findByTestId('protected')).toBeInTheDocument()
    unmount()
    renderApp()
    expect(await screen.findByTestId('protected')).toBeInTheDocument()
  })

  it('退出调用 logout 并回登录页', async () => {
    server.use(
      http.get('/api/auth/session', () =>
        HttpResponse.json({
          success: true,
          data: {
            csrf_token: 'csrf-abc',
            expires_at: '2026-08-05T22:00:00+08:00',
          },
        }),
      ),
      http.post('/api/auth/logout', () =>
        HttpResponse.json({ success: true, data: { logged_out: true } }),
      ),
    )
    render(
      <MemoryRouter initialEntries={['/accounts']}>
        <AuthProvider>
          <Routes>
            <Route path="/login" element={<TestApp />} />
            <Route path="/accounts" element={<LogoutProbe />} />
          </Routes>
        </AuthProvider>
      </MemoryRouter>,
    )
    const user = userEvent.setup()
    await screen.findByTestId('protected')
    await user.click(screen.getByRole('button', { name: /退出登录/ }))
    await waitFor(() =>
      expect(screen.getByRole('heading', { name: 'iCloud HME 管理台' })).toBeInTheDocument(),
    )
  })

  it('已认证状态访问 /login 自动重定向到 /schedule', async () => {
    server.use(
      http.get('/api/auth/session', () =>
        HttpResponse.json({
          success: true,
          data: {
            csrf_token: 'csrf-abc',
            expires_at: '2026-08-05T22:00:00+08:00',
          },
        }),
      ),
    )
    render(
      <MemoryRouter initialEntries={['/login']}>
        <AuthProvider>
          <Routes>
            <Route path="/login" element={<LoginPage />} />
            <Route path="/schedule" element={<p data-testid="schedule-page">调度大盘页</p>} />
          </Routes>
        </AuthProvider>
      </MemoryRouter>,
    )
    expect(await screen.findByTestId('schedule-page')).toBeInTheDocument()
  })
})

function LogoutProbe() {
  const { status, logout } = useAuth()
  if (status === 'checking') return <p>loading…</p>
  if (status === 'anonymous') return <LoginPage />
  return (
    <div>
      <p data-testid="protected">已登录页面</p>
      <button onClick={() => void logout()}>退出登录</button>
    </div>
  )
}
