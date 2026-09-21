import { http, HttpResponse } from 'msw'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it } from 'vitest'
import SchedulePage from './SchedulePage'
import { server } from '../test/server'
import { setCSRFToken } from '../api/client'
import { ToastProvider } from '../components/ToastProvider'
import type { AccountSummary, ScheduleConfig, ScheduleLog } from '../api/types'

const accounts: AccountSummary[] = [
  {
    id: 'acc_enabled',
    name: '启用号',
    real_email: 'on@example.com',
    icloud_email: 'on@icloud.com',
    host: 'icloud.com',
    status: 'active',
    alias_total: 5,
    alias_active: 5,
    has_cookies: true,
    has_app_password: false,
    has_proxy: false,
    last_validated: '',
    created_at: '',
  },
  {
    id: 'acc_off',
    name: '未启用号',
    real_email: 'off@example.com',
    icloud_email: 'off@icloud.com',
    host: 'icloud.com',
    status: 'active',
    alias_total: 0,
    alias_active: 0,
    has_cookies: true,
    has_app_password: false,
    has_proxy: false,
    last_validated: '',
    created_at: '',
  },
]

// 只有 acc_enabled 有配置行;acc_off 从未保存过配置,不应贡献幻影默认配额
const configs: ScheduleConfig[] = [
  { account_id: 'acc_enabled', enabled: true, hourly_quota: 2, current_hour_count: 1 },
]

const logs: ScheduleLog[] = [
  { time: '10:00:00', message: '账号=启用号 创建成功: alias@icloud.com' },
  { time: '10:00:05', message: '账号=启用号 Cookie 失效，自动熔断' },
  { time: '10:00:10', message: '账号=启用号 创建失败: timeout' },
  { time: '10:00:15', message: 'scheduled任务开始 accounts=1 requestedPerAccount=1' },
]

function mockApi() {
  server.use(
    http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
    http.get('/api/schedule/configs', () => HttpResponse.json({ success: true, data: configs })),
    http.get('/api/schedule/logs', () => HttpResponse.json({ success: true, data: logs })),
    http.get('/api/schedule/status', () =>
      HttpResponse.json({ success: true, data: { running: false, interval_seconds: 300 } }),
    ),
  )
}

function renderPage() {
  return render(
    <ToastProvider>
      <SchedulePage />
    </ToastProvider>,
  )
}

describe('SchedulePage', () => {
  beforeEach(() => {
    setCSRFToken('csrf-test')
    server.resetHandlers()
    mockApi()
  })

  it('总计每小时配额只统计已启用账号,未配置账号不计幻影默认配额', async () => {
    renderPage()
    await screen.findByText('自动补货账号')
    // acc_enabled 启用 quota 2;acc_off 无配置行,不贡献幻影默认配额 5
    const purpleValue = document.querySelector('.stat-card-purple .stat-value')
    expect(purpleValue?.childNodes[0]?.textContent).toBe('2')
    expect(screen.getByText('个/小时')).toBeInTheDocument()
  })

  it('异常过滤同时覆盖 ERROR 与 WARN(熔断),成功与普通 INFO 不出现', async () => {
    renderPage()
    await userEvent.click(await screen.findByRole('button', { name: /异常/ }))
    expect(screen.getByText('账号=启用号 Cookie 失效，自动熔断')).toBeInTheDocument()
    expect(screen.getByText('账号=启用号 创建失败: timeout')).toBeInTheDocument()
    expect(screen.queryByText(/创建成功/)).not.toBeInTheDocument()
    expect(screen.queryByText(/scheduled任务开始/)).not.toBeInTheDocument()
  })

  it('过滤按钮展示分级计数', async () => {
    renderPage()
    expect(await screen.findByRole('button', { name: '全部 (4)' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '成功 (1)' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '异常 (2)' })).toBeInTheDocument()
  })

  it('配额编辑:空串可暂存不误提交,失焦钳制到上限并写入配置', async () => {
    const putBodies: Array<{ hourly_quota?: number }> = []
    let storedQuota = 2
    server.use(
      http.get('/api/schedule/configs', () =>
        HttpResponse.json({
          success: true,
          data: [
            { account_id: 'acc_enabled', enabled: true, hourly_quota: storedQuota, current_hour_count: 1 },
          ] satisfies ScheduleConfig[],
        }),
      ),
      http.put('/api/schedule/configs/:account_id', async ({ request }) => {
        const body = (await request.json()) as { hourly_quota?: number }
        storedQuota = body?.hourly_quota ?? storedQuota
        putBodies.push(body)
        return HttpResponse.json({ success: true, data: null })
      }),
    )
    renderPage()
    const input = await screen.findByDisplayValue('2')
    await userEvent.clear(input)
    expect(input).toHaveValue(null)
    await userEvent.type(input, '50')
    fireEvent.blur(input)
    await waitFor(() => expect(putBodies).toHaveLength(1))
    expect(putBodies[0]?.hourly_quota).toBe(30)
    expect(screen.getByDisplayValue('30')).toBeInTheDocument()
  })

  it('调度器执行中时按钮禁用并显示执行中,空闲显示运行周期', async () => {
    renderPage()
    expect(await screen.findByRole('button', { name: '立即执行一轮' })).toBeEnabled()

    server.use(
      http.get('/api/schedule/status', () =>
        HttpResponse.json({ success: true, data: { running: true, interval_seconds: 300 } }),
      ),
    )
    // 等待下一次 3s 轮询拉到 running 状态
    const button = await screen.findByRole('button', { name: '执行中…' }, { timeout: 5000 })
    expect(button).toBeDisabled()
    expect(screen.getByText('● 执行中')).toBeInTheDocument()
  })

  it('配额条按当前小时用量展示比例', async () => {
    renderPage()
    await screen.findByText(/各账号调度策略/)
    // current_hour_count=1 / hourly_quota=2 → 50%
    const meterFill = document.querySelector<HTMLElement>('.schedule-acc-meter span')
    expect(meterFill).not.toBeNull()
    expect(meterFill?.style.width).toBe('50%')
  })

  it('别名备注模板回显默认 scheduled，可修改为自定义如 gpt', async () => {
    let putBody: { alias_label?: string } | null = null
    server.use(
      http.put('/api/schedule/configs/acc_enabled', async ({ request }) => {
        putBody = (await request.json()) as { alias_label?: string }
        return HttpResponse.json({ success: true, data: null })
      }),
    )
    renderPage()
    const labelInputs = await screen.findAllByDisplayValue('scheduled')
    expect(labelInputs[0]).toBeInTheDocument()
    await userEvent.clear(labelInputs[0]!)
    await userEvent.type(labelInputs[0]!, 'gpt')
    fireEvent.blur(labelInputs[0]!)
    await waitFor(() => expect(putBody?.alias_label).toBe('gpt'))
  })

  it('调度模式下拉框使用自定义 Select 组件并能正确触发模式切换', async () => {
    let putBody: Partial<ScheduleConfig> | null = null
    server.use(
      http.put('/api/schedule/configs/acc_enabled', async ({ request }) => {
        putBody = (await request.json()) as Partial<ScheduleConfig>
        return HttpResponse.json({ success: true, data: null })
      }),
    )
    renderPage()
    await screen.findByText('自动补货账号')
    const customTrigger = document.querySelector('.schedule-mode-select-row .ui-select-trigger')
    expect(customTrigger).not.toBeNull()
    expect(customTrigger?.textContent).toContain('全天常驻')

    const user = userEvent.setup()
    const selectElements = screen.getAllByLabelText('调度模式')
    await user.selectOptions(selectElements[0]!, 'daily_window')
    await waitFor(() => expect(putBody?.mode).toBe('daily_window'))
  })
})
