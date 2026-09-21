import { http, HttpResponse } from 'msw'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it } from 'vitest'
import SettingsPage from './SettingsPage'
import { server } from '../test/server'
import { setCSRFToken } from '../api/client'
import { ToastProvider } from '../components/ToastProvider'
import type { NotifySettings } from '../api/types'

const stored: NotifySettings = {
  feishu_webhook: 'https://open.feishu.cn/open-apis/bot/v2/hook/abc',
  bark_url: '',
  telegram_token: '',
  telegram_chat: '',
  event_kinds: { cookie_expired: true, cookie_recovered: false, quota_low: true },
  quota_threshold: 700,
}

function mockGet(data: NotifySettings = stored) {
  server.use(http.get('/api/settings/notify', () => HttpResponse.json({ success: true, data })))
}

function renderPage() {
  return render(
    <ToastProvider>
      <SettingsPage />
    </ToastProvider>,
  )
}

describe('SettingsPage', () => {
  beforeEach(() => {
    setCSRFToken('csrf-test')
    server.resetHandlers()
  })

  it('加载并回显已保存的通知配置', async () => {
    mockGet()
    renderPage()
    expect(await screen.findByLabelText('飞书自定义机器人 Webhook')).toHaveValue(stored.feishu_webhook)
    expect(screen.getByLabelText('配额水位阈值 (活跃别名数)')).toHaveValue(700)
    // cookie_recovered 为 false, 复选框未勾选
    expect(screen.getByLabelText(/Cookie 恢复/)).not.toBeChecked()
    expect(screen.getByLabelText(/Cookie 失效/)).toBeChecked()
  })

  it('保存时把表单状态原样 PUT 到后端', async () => {
    mockGet()
    const putBodies: NotifySettings[] = []
    server.use(
      http.put('/api/settings/notify', async ({ request }) => {
        putBodies.push((await request.json()) as NotifySettings)
        return HttpResponse.json({ success: true, data: stored })
      }),
    )
    renderPage()
    await screen.findByLabelText('飞书自定义机器人 Webhook')
    await userEvent.click(screen.getByRole('button', { name: '保存配置' }))
    await waitFor(() => expect(putBodies).toHaveLength(1))
    expect(putBodies[0].feishu_webhook).toBe(stored.feishu_webhook)
    expect(putBodies[0].quota_threshold).toBe(700)
    expect(putBodies[0].event_kinds?.cookie_recovered).toBe(false)
  })

  it('切换事件开关并保存', async () => {
    mockGet()
    const putBodies: NotifySettings[] = []
    server.use(
      http.put('/api/settings/notify', async ({ request }) => {
        putBodies.push((await request.json()) as NotifySettings)
        return HttpResponse.json({ success: true, data: stored })
      }),
    )
    renderPage()
    await screen.findByLabelText('飞书自定义机器人 Webhook')
    await userEvent.click(screen.getByLabelText(/Cookie 恢复/)) // false → true
    await userEvent.click(screen.getByRole('button', { name: '保存配置' }))
    await waitFor(() => expect(putBodies).toHaveLength(1))
    expect(putBodies[0].event_kinds?.cookie_recovered).toBe(true)
  })

  it('测试推送展示逐渠道结果', async () => {
    mockGet()
    server.use(
      http.post('/api/settings/notify/test', () =>
        HttpResponse.json({
          success: true,
          data: {
            results: [
              { channel: 'feishu', ok: true },
              { channel: 'bark', ok: false, error: 'Bark 返回 HTTP 500' },
            ],
          },
        }),
      ),
    )
    renderPage()
    await screen.findByLabelText('飞书自定义机器人 Webhook')
    await userEvent.click(screen.getByRole('button', { name: '发送测试通知' }))
    expect(await screen.findByText('飞书')).toBeInTheDocument()
    expect(screen.getByText('Bark')).toBeInTheDocument()
    expect(screen.getByText(/发送失败: Bark 返回 HTTP 500/)).toBeInTheDocument()
  })

  it('后端校验错误时展示错误信息', async () => {
    mockGet()
    server.use(
      http.put('/api/settings/notify', () =>
        HttpResponse.json(
          { success: false, code: 'VALIDATION_ERROR', message: 'Telegram Bot Token 与 Chat ID 必须同时填写' },
          { status: 400 },
        ),
      ),
    )
    renderPage()
    await screen.findByLabelText('飞书自定义机器人 Webhook')
    await userEvent.click(screen.getByRole('button', { name: '保存配置' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('Telegram Bot Token 与 Chat ID 必须同时填写')
  })
})
