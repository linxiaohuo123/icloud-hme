import { http, HttpResponse } from 'msw'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it } from 'vitest'
import SettingsPage from './SettingsPage'
import { server } from '../test/server'
import { setCSRFToken } from '../api/client'
import { ToastProvider } from '../components/ToastProvider'
import type { NotifySettingsResponse, UpdateNotifySettingsRequest } from '../api/types'

const storedResponse: NotifySettingsResponse = {
  feishu_configured: true,
  feishu_webhook_masked: 'https://open.feishu.cn/open-apis/bot/v2/hook/abc****',
  bark_configured: true,
  bark_url_masked: 'https://api.day.app/dev****',
  telegram_configured: true,
  telegram_token_masked: '123456****',
  telegram_chat: '987654321',
  event_kinds: { cookie_expired: true, cookie_recovered: false, quota_low: true },
  quota_threshold: 700,
}

function mockGet(data: NotifySettingsResponse = storedResponse) {
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

  it('TestNotifyMaskedSecretIsNeverTreatedAsRealInput: 输入框默认为空且展示脱敏徽章', async () => {
    mockGet()
    renderPage()

    // 密文输入框均必须为空，绝不以脱敏掩码作为输入值
    const feishuInput = await screen.findByLabelText('飞书自定义机器人 Webhook')
    expect(feishuInput).toHaveValue('')
    expect(screen.getByLabelText('Bark 推送地址 (iOS)')).toHaveValue('')
    expect(screen.getByLabelText('Telegram Bot Token')).toHaveValue('')

    // 非密文字段正常回显
    expect(screen.getByLabelText('Telegram Chat ID')).toHaveValue('987654321')
    expect(screen.getByLabelText('配额水位阈值 (活跃别名数)')).toHaveValue(700)

    // 脱敏徽章与清除按钮正确展示
    expect(screen.getByText(/已配置: https:\/\/open\.feishu\.cn.*abc\*\*\*\*/)).toBeInTheDocument()
    expect(screen.getByText(/已配置: https:\/\/api\.day\.app.*dev\*\*\*\*/)).toBeInTheDocument()
    expect(screen.getByText(/已配置: 123456\*\*\*\*/)).toBeInTheDocument()
  })

  it('TestNotifySavingQuotaDoesNotResendMaskedSecrets: 仅改动配额时绝不回传脱敏掩码', async () => {
    mockGet()
    const putBodies: UpdateNotifySettingsRequest[] = []
    server.use(
      http.put('/api/settings/notify', async ({ request }) => {
        putBodies.push((await request.json()) as UpdateNotifySettingsRequest)
        return HttpResponse.json({ success: true, data: { ...storedResponse, quota_threshold: 850 } })
      }),
    )

    renderPage()
    const quotaInput = await screen.findByLabelText('配额水位阈值 (活跃别名数)')
    await userEvent.clear(quotaInput)
    await userEvent.type(quotaInput, '850')

    await userEvent.click(screen.getByRole('button', { name: '保存配置' }))

    await waitFor(() => expect(putBodies).toHaveLength(1))
    const body = putBodies[0]
    expect(body.quota_threshold).toBe(850)
    // Secret 字段必须为 undefined，严禁回传脱敏值或空值覆盖
    expect(body.feishu_webhook).toBeUndefined()
    expect(body.bark_url).toBeUndefined()
    expect(body.telegram_token).toBeUndefined()
    expect(JSON.stringify(body)).not.toContain('****')
  })

  it('TestNotifySecretReplacementSendsOnlyNewValue: 替换 Secret 时仅提交用户真实输入的明文', async () => {
    mockGet()
    const putBodies: UpdateNotifySettingsRequest[] = []
    server.use(
      http.put('/api/settings/notify', async ({ request }) => {
        putBodies.push((await request.json()) as UpdateNotifySettingsRequest)
        return HttpResponse.json({ success: true, data: storedResponse })
      }),
    )

    renderPage()
    const feishuInput = await screen.findByLabelText('飞书自定义机器人 Webhook')
    const newWebhook = 'https://open.feishu.cn/open-apis/bot/v2/hook/new-real-secret-1234'
    await userEvent.type(feishuInput, newWebhook)

    await userEvent.click(screen.getByRole('button', { name: '保存配置' }))

    await waitFor(() => expect(putBodies).toHaveLength(1))
    const body = putBodies[0]
    expect(body.feishu_webhook).toBe(newWebhook)
    // 其他未修改的 secret 不得发送
    expect(body.bark_url).toBeUndefined()
    expect(body.telegram_token).toBeUndefined()
    expect(JSON.stringify(body)).not.toContain('****')
  })

  it('TestNotifyExplicitClearUsesClearFlag: 显式清除必须携带 clear 标记', async () => {
    mockGet()
    const putBodies: UpdateNotifySettingsRequest[] = []
    server.use(
      http.put('/api/settings/notify', async ({ request }) => {
        putBodies.push((await request.json()) as UpdateNotifySettingsRequest)
        return HttpResponse.json({
          success: true,
          data: { ...storedResponse, feishu_configured: false, feishu_webhook_masked: '' },
        })
      }),
    )

    renderPage()
    await screen.findByLabelText('飞书自定义机器人 Webhook')

    // 点击飞书清除配置
    const clearBtns = screen.getAllByRole('button', { name: '清除配置' })
    await userEvent.click(clearBtns[0])

    expect(screen.getByText('已标记清除')).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: '保存配置' }))

    await waitFor(() => expect(putBodies).toHaveLength(1))
    const body = putBodies[0]
    expect(body.clear_feishu).toBe(true)
    expect(body.feishu_webhook).toBeUndefined()
  })

  it('TestNotifyResponseNeverDisplaysRawSecret: 页面与 DOM 绝不包含明文密钥', async () => {
    mockGet({
      ...storedResponse,
      feishu_webhook_masked: 'https://open.feishu.cn/open-apis/bot/v2/hook/masked****',
    })
    renderPage()
    await screen.findByText(/已配置: https:\/\/open\.feishu\.cn.*masked\*\*\*\*/)

    const dom = document.body.innerHTML
    // 确保绝对不包含真实未脱敏的 secret 标记
    expect(dom).not.toContain('unmasked-super-secret')
    // 输入框值为空
    expect(screen.getByLabelText('飞书自定义机器人 Webhook')).toHaveValue('')
    expect(screen.getByLabelText('Bark 推送地址 (iOS)')).toHaveValue('')
    expect(screen.getByLabelText('Telegram Bot Token')).toHaveValue('')
  })

  it('切换事件开关并保存', async () => {
    mockGet()
    const putBodies: UpdateNotifySettingsRequest[] = []
    server.use(
      http.put('/api/settings/notify', async ({ request }) => {
        putBodies.push((await request.json()) as UpdateNotifySettingsRequest)
        return HttpResponse.json({ success: true, data: storedResponse })
      }),
    )
    renderPage()
    await screen.findByLabelText('飞书自定义机器人 Webhook')
    await userEvent.click(screen.getByLabelText(/Cookie 恢复/)) // false → true
    await userEvent.click(screen.getByRole('button', { name: '保存配置' }))
    await waitFor(() => expect(putBodies).toHaveLength(1))
    expect(putBodies[0].event_kinds?.cookie_recovered).toBe(true)
    // 没有修改 secret，不发任何 secret
    expect(putBodies[0].feishu_webhook).toBeUndefined()
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
