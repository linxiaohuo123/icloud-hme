import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import { AuthProvider } from '../auth/AuthProvider'
import { server } from '../test/server'
import AppShell from './AppShell'
import { ToastProvider } from './ToastProvider'
import { ThemeProvider } from './ThemeProvider'

describe('AppShell', () => {
  it('账号加载失败时显示全局重试，成功后恢复侧栏', async () => {
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
})
