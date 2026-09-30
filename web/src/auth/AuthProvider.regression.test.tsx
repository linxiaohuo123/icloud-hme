import { http, HttpResponse } from 'msw'
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'
import { AuthProvider, useAuth } from './AuthProvider'
import * as client from '../api/client'
import { server } from '../test/server'

function AuthProbe() {
  const { status, login } = useAuth()
  return <><output data-testid="status">{status}</output><button onClick={() => void login('test')}>登录</button></>
}

afterEach(() => { vi.restoreAllMocks() })

it.each([401, 200])('旧会话探测返回 %s 不覆盖新登录状态或 CSRF', async (oldStatus) => {
  client.setCSRFToken(null)
  let releaseSession!: () => void
  const pendingSession = new Promise<void>((resolve) => { releaseSession = resolve })
  const requestSpy = vi.spyOn(client, 'request')
  let seenCSRF: string | null = null
  server.use(
    http.get('/api/auth/session', async () => {
      await pendingSession
      return oldStatus === 401
        ? HttpResponse.json({ success: false, code: 'AUTH_REQUIRED', message: '请先登录' }, { status: 401 })
        : HttpResponse.json({ success: true, data: { csrf_token: 'old-session', expires_at: '' } })
    }),
    http.post('/api/auth/login', () => HttpResponse.json({ success: true, data: { csrf_token: 'new-session', expires_at: '' } })),
    http.post('/api/probe', ({ request }) => {
      seenCSRF = request.headers.get('X-CSRF-Token')
      return HttpResponse.json({ success: true, data: {} })
    }),
  )
  render(<AuthProvider><AuthProbe /></AuthProvider>)
  const originalSessionRequest = requestSpy.mock.results[0].value as Promise<unknown>
  fireEvent.click(screen.getByRole('button', { name: '登录' }))
  await waitFor(() => expect(screen.getByTestId('status')).toHaveTextContent('authenticated'))
  await act(async () => {
    releaseSession()
    await originalSessionRequest.catch(() => undefined)
  })
  expect(screen.getByTestId('status')).toHaveTextContent('authenticated')
  await client.request('/api/probe', { method: 'POST' })
  expect(seenCSRF).toBe('new-session')
})
