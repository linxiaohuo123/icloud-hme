import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'
import { AuthProvider } from '../auth/AuthProvider'
import { server } from '../test/server'
import AppShell from './AppShell'
import { ToastProvider } from './ToastProvider'
import { ThemeProvider } from './ThemeProvider'
import { clearAccountsCache } from '../hooks/useAccounts'

describe('AppShell', () => {
  it('账号加载失败时显示全局重试，成功后恢复侧栏', async () => {
    clearAccountsCache()
    server.use(
      http.get('/api/auth/session', () => HttpResponse.json({ success: true, data: { csrf_token: 'csrf-test' } })),
      http.get('/api/accounts', () =>
        HttpResponse.json({ success: false, code: 'INTERNAL_ERROR', message: '账号服务不可用' }, { status: 500 }),
      ),
    )
    render(
      <ThemeProvider><MemoryRouter initialEntries={['/schedule']}><AuthProvider><ToastProvider>
        <Routes><Route element={<AppShell />}><Route path="/schedule" element={<p>调度页面</p>} /></Route></Routes>
      </ToastProvider></AuthProvider></MemoryRouter></ThemeProvider>,
    )

    expect(await screen.findByRole('alert')).toHaveTextContent('账号列表加载失败：账号服务不可用')
    expect(screen.queryByText('暂无账号')).not.toBeInTheDocument()

    server.use(http.get('/api/accounts', () => HttpResponse.json({ success: true, data: [{ id: 'acc_1', name: '已恢复账号', status: 'active', has_cookies: true }] })))
    await userEvent.click(screen.getByRole('button', { name: '重试' }))
    expect(await screen.findByText('已恢复账号')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('仅有未录入凭据的新账号时展示待配置提醒而非失效警报', async () => {
    clearAccountsCache()
    server.use(
      http.get('/api/auth/session', () => HttpResponse.json({ success: true, data: { csrf_token: 'csrf-test' } })),
      http.get('/api/accounts', () =>
        HttpResponse.json({
          success: true,
          data: [
            { id: 'acc_new', name: '新号5', status: 'pending', has_cookies: false },
          ],
        }),
      ),
    )
    render(
      <ThemeProvider><MemoryRouter initialEntries={['/schedule']}><AuthProvider><ToastProvider>
        <Routes><Route element={<AppShell />}><Route path="/schedule" element={<p>调度页面</p>} /></Route></Routes>
      </ToastProvider></AuthProvider></MemoryRouter></ThemeProvider>,
    )

    expect(await screen.findByText(/账号待配置提示/)).toBeInTheDocument()
    expect(screen.getByText(/尚未配置凭据/)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '前往配置凭据' })).toBeInTheDocument()
  })

  it('在 /accounts 页面点击告警按钮时平滑滚动到目标行', async () => {
    clearAccountsCache()
    server.use(
      http.get('/api/auth/session', () => HttpResponse.json({ success: true, data: { csrf_token: 'csrf-test' } })),
      http.get('/api/accounts', () =>
        HttpResponse.json({
          success: true,
          data: [
            { id: 'acc_target', name: '目标账号', status: 'error', has_cookies: true },
          ],
        }),
      ),
    )

    const scrollMock = vi.fn()
    const originalQuery = document.querySelector
    const dummyRow = document.createElement('tr')
    dummyRow.scrollIntoView = scrollMock
    vi.spyOn(document, 'querySelector').mockImplementation((sel) => {
      if (sel === '[data-account-id="acc_target"]') return dummyRow
      return originalQuery.call(document, sel)
    })

    render(
      <ThemeProvider><MemoryRouter initialEntries={['/accounts']}><AuthProvider><ToastProvider>
        <Routes><Route element={<AppShell />}><Route path="/accounts" element={<p>账号页</p>} /></Route></Routes>
      </ToastProvider></AuthProvider></MemoryRouter></ThemeProvider>,
    )

    const btn = await screen.findByRole('link', { name: '前往重新登录 / 更新' })
    await userEvent.click(btn)
    expect(scrollMock).toHaveBeenCalled()
    vi.restoreAllMocks()
  })
})
