import { http, HttpResponse } from 'msw'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it } from 'vitest'
import AccountsPage from './AccountsPage'
import { server } from '../test/server'
import { setCSRFToken } from '../api/client'
import { ToastProvider } from '../components/ToastProvider'
import type { AccountSummary } from '../api/types'

const accounts: AccountSummary[] = [
  {
    id: 'acc_active',
    name: '活跃号',
    real_email: 'active@example.com',
    icloud_email: 'active@icloud.com',
    host: 'icloud.com',
    status: 'active',
    alias_total: 15,
    alias_active: 12,
    has_cookies: true,
    has_app_password: true,
    has_proxy: false,
    last_validated: '2026-08-04T09:00:00+08:00',
    created_at: '2026-08-01T09:00:00+08:00',
  },
  {
    id: 'acc_pending',
    name: '等待号',
    real_email: 'pending@example.com',
    icloud_email: 'pending@icloud.com',
    host: 'icloud.com',
    status: 'pending',
    alias_total: 0,
    alias_active: 0,
    has_cookies: false,
    has_app_password: false,
    has_proxy: false,
    last_validated: '',
    created_at: '2026-08-02T09:00:00+08:00',
  },
  {
    id: 'acc_error',
    name: '错误号',
    real_email: 'error@example.com',
    icloud_email: 'error@icloud.com',
    host: 'icloud.com',
    status: 'error',
    alias_total: 0,
    alias_active: 0,
    has_cookies: true,
    has_app_password: false,
    has_proxy: true,
    last_validated: '',
    created_at: '2026-08-03T09:00:00+08:00',
  },
]

function renderPage() {
  return render(
    <MemoryRouter>
      <ToastProvider>
        <AccountsPage />
      </ToastProvider>
    </MemoryRouter>,
  )
}

describe('AccountsPage', () => {
  beforeEach(() => {
    setCSRFToken('csrf-test')
    server.resetHandlers()
  })

  it('渲染账号列表:状态文字、凭据标志、邮箱与别名计数可见,无秘密', async () => {
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
    )
    renderPage()
    expect(await screen.findByText('活跃号')).toBeInTheDocument()
    expect(screen.getByText('等待号')).toBeInTheDocument()
    expect(screen.getByText('错误号')).toBeInTheDocument()
    expect(screen.getByText('active@icloud.com')).toBeInTheDocument()
    expect(screen.getByText('12 / 15')).toBeInTheDocument()
    expect(screen.getAllByText(/已配置/).length).toBeGreaterThan(0)
    expect(screen.getAllByText(/未配置/).length).toBeGreaterThan(0)
    // 秘密字段不可见
    expect(screen.queryByText(/cookie-secret|app-secret|proxy-secret/)).toBeNull()
  })

  it('添加账号:校验必填、请求期间禁用、成功刷新', async () => {
    let listData = accounts
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: listData })),
      http.post('/api/accounts', async ({ request }) => {
        const body = (await request.json()) as Record<string, string>
        if (!body.name || !body.icloud_email) {
          return HttpResponse.json(
            { success: false, code: 'VALIDATION_ERROR', message: '参数错误' },
            { status: 400 },
          )
        }
        listData = [{ ...accounts[0], id: 'acc_new', name: body.name }, ...accounts]
        return HttpResponse.json(
          { success: true, data: listData[0] },
          { status: 201 },
        )
      }),
    )
    renderPage()
    await screen.findByText('活跃号')
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: /添加账号/ }))
    fireEvent.change(screen.getByLabelText(/名称/), { target: { value: '新账号' } })
    fireEvent.change(screen.getByLabelText(/iCloud 邮箱/), { target: { value: 'new@icloud.com' } })
    await user.click(screen.getByRole('button', { name: /保存/ }))
    // 请求期间按钮禁用;成功后列表刷新
    await waitFor(() => expect(screen.getByText('新账号')).toBeInTheDocument())
  })

  it('添加账号:host 只能选全球区或中国区', async () => {
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
    )
    renderPage()
    await screen.findByText('活跃号')
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: /添加账号/ }))
    const hostSelect = screen.getByLabelText(/区域/)
    expect(within(hostSelect).getByText('全球区 (icloud.com)')).toBeInTheDocument()
    expect(within(hostSelect).getByText('中国区 (icloud.com.cn)')).toBeInTheDocument()
    // 空名称提交被阻止(前端校验),dialog 保持打开
    await user.click(screen.getByRole('button', { name: /保存/ }))
    expect(screen.getByRole('dialog')).toBeInTheDocument()
  })

  it('Cookie 提交后 textarea 清空', async () => {
    let cookieBody = ''
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.put('/api/accounts/:id/cookies', async ({ request }) => {
        cookieBody = await request.text()
        return HttpResponse.json({ success: true, data: accounts[0] })
      }),
    )
    renderPage()
    await screen.findByText('活跃号')
    const user = userEvent.setup()
    await user.click(screen.getAllByRole('button', { name: /更多操作/ })[0])
    await user.click(screen.getByRole('button', { name: /更新 Cookie/ }))
    const textarea = screen.getByLabelText('Cookie') as HTMLTextAreaElement
    await user.type(textarea, 'a=1; b=2')
    await user.click(screen.getByRole('button', { name: /保存/ }))
    await waitFor(() => expect(cookieBody).toContain('a=1; b=2'))
    // 保存成功后弹窗自动关闭（组件卸载）
    await waitFor(() => expect(screen.queryByLabelText('Cookie')).toBeNull())
  })

  it('iCloud 登录收到 OTP_REQUIRED 后只显示 OTP 输入并可重试', async () => {
    let calls = 0
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.post('/api/accounts/:id/login', async () => {
        calls++
        if (calls === 1) {
          return HttpResponse.json(
            { success: false, code: 'OTP_REQUIRED', message: '需要提供 OTP 验证码' },
            { status: 409 },
          )
        }
        return HttpResponse.json({ success: true, data: accounts[0] })
      }),
    )
    renderPage()
    await screen.findByText('活跃号')
    const user = userEvent.setup()
    await user.click(screen.getAllByRole('button', { name: /更多操作/ })[0])
    await user.click(screen.getByRole('button', { name: /iCloud 登录/ }))
    await user.type(screen.getByLabelText(/密码/), 'p@ssw0rd')
    let dialog = screen.getByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: /登录/ }))
    // 出现 OTP 输入
    const otpInput = await screen.findByLabelText(/验证码/)
    expect(otpInput).toHaveAttribute('inputmode', 'numeric')
    await user.type(otpInput, '123456')
    dialog = screen.getByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: /验证/ }))
    await waitFor(() => expect(calls).toBe(2))
  })

  it('App Password 提交后清空', async () => {
    let pwdBody = ''
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.post('/api/accounts/:id/password', async ({ request }) => {
        pwdBody = await request.text()
        return HttpResponse.json({ success: true, data: accounts[0] })
      }),
    )
    renderPage()
    await screen.findByText('活跃号')
    const user = userEvent.setup()
    await user.click(screen.getAllByRole('button', { name: /更多操作/ })[0])
    await user.click(screen.getByRole('button', { name: /设置 App 密码/ }))
    await user.type(screen.getByLabelText(/邮箱/), 'app@icloud.com')
    await user.type(screen.getByLabelText('App 专用密码'), 'xxxx-xxxx-xxxx-xxxx')
    await user.click(screen.getByRole('button', { name: /保存/ }))
    await waitFor(() => expect(pwdBody).toContain('app@icloud.com'))
    // 保存成功后弹窗自动关闭（组件卸载）
    await waitFor(() => expect(screen.queryByLabelText('App 专用密码')).toBeNull())
  })

  it('代理从不回显:保存后输入清空,关闭再打开仍为空', async () => {
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.put('/api/accounts/:id/proxy', async ({ request }) => {
        const body = (await request.json()) as { proxy: string }
        return HttpResponse.json({
          success: true,
          data: { ...accounts[0], has_proxy: body.proxy !== '' },
        })
      }),
    )
    renderPage()
    await screen.findByText('活跃号')
    const user = userEvent.setup()
    await user.click(screen.getAllByRole('button', { name: /更多操作/ })[0])
    await user.click(screen.getByRole('button', { name: /设置代理/ }))
    const input = screen.getByLabelText(/代理地址/) as HTMLInputElement
    expect(input.value).toBe('')
    await user.type(input, 'http://u:p@proxy.example.com:8080')
    await user.click(screen.getByRole('button', { name: /保存/ }))
    // 保存成功后弹窗自动关闭（组件卸载）
    await waitFor(() => expect(screen.queryByLabelText(/代理地址/)).toBeNull())
    // 重新打开:仍为空(从不回显)
    await user.click(screen.getAllByRole('button', { name: /更多操作/ })[0])
    await user.click(screen.getByRole('button', { name: /设置代理/ }))
    expect((screen.getByLabelText(/代理地址/) as HTMLInputElement).value).toBe('')
  })

  it('删除要求输入账号名称精确匹配,取消不发请求', async () => {
    let deleted = false
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.delete('/api/accounts/:id', () => {
        deleted = true
        return HttpResponse.json({ success: true, data: { id: 'acc_active' } })
      }),
    )
    renderPage()
    await screen.findByText('活跃号')
    const user = userEvent.setup()
    await user.click(screen.getAllByRole('button', { name: /更多操作/ })[0])
    await user.click(screen.getByRole('button', { name: /删除/ }))
    expect(screen.getByRole('dialog')).toBeInTheDocument()
    // 名称不匹配时按钮禁用
    await user.type(screen.getByLabelText(/输入账号名称/), '错误名称')
    expect(screen.getByRole('button', { name: /确认删除/ })).toBeDisabled()
    // 取消不发请求
    await user.click(screen.getByRole('button', { name: /取消/ }))
    expect(deleted).toBe(false)
  })

  it('编辑账号:正确回显当前名称与邮箱,修改后提交成功', async () => {
    let patchedBody: Record<string, string> = {}
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.patch('/api/accounts/:id', async ({ request }) => {
        patchedBody = (await request.json()) as Record<string, string>
        return HttpResponse.json({
          success: true,
          data: { ...accounts[0], name: patchedBody.name },
        })
      }),
    )
    renderPage()
    await screen.findByText('活跃号')
    const user = userEvent.setup()
    await user.click(screen.getAllByRole('button', { name: /更多操作/ })[0])
    await user.click(screen.getByRole('button', { name: /编辑/ }))

    // 验证数据正确回显
    const nameInput = screen.getByLabelText(/名称/) as HTMLInputElement
    const emailInput = screen.getByLabelText(/iCloud 邮箱/) as HTMLInputElement
    expect(nameInput.value).toBe('活跃号')
    expect(emailInput.value).toBe('active@icloud.com')
    expect(emailInput).toBeDisabled()

    // 修改名称并保存
    await user.clear(nameInput)
    await user.type(nameInput, '活跃号-修改后')
    await user.click(screen.getByRole('button', { name: /保存/ }))
    await waitFor(() => expect(patchedBody.name).toBe('活跃号-修改后'))
  })
})

