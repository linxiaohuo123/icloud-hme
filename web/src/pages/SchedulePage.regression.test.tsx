import { http, HttpResponse } from 'msw'
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'
import SchedulePage from './SchedulePage'
import { ToastProvider } from '../components/ToastProvider'
import { server } from '../test/server'
import { setCSRFToken } from '../api/client'
import type { ScheduleConfig } from '../api/types'

function deferred() {
  let resolve!: () => void
  const promise = new Promise<void>((done) => { resolve = done })
  return { promise, resolve }
}

const account = {
  id: 'probe', name: '测试账号', real_email: 'test@example.com', icloud_email: '',
  status: 'active', alias_total: 0, alias_active: 0, has_cookies: true,
  has_app_password: false, has_proxy: false, host: 'icloud.com', created_at: '', last_validated: '',
}

function mockSchedule(getConfigs: () => Promise<Response> | Response) {
  setCSRFToken('test')
  server.use(
    http.get('/api/accounts', () => HttpResponse.json({ success: true, data: [account] })),
    http.get('/api/schedule/configs', getConfigs),
    http.get('/api/schedule/logs', () => HttpResponse.json({ success: true, data: [] })),
    http.get('/api/schedule/status', () => HttpResponse.json({ success: true, data: { running: false, interval_seconds: 300 } })),
  )
}

afterEach(() => { vi.useRealTimers() })

it.each(['普通补货', '全员补货', '日志空态补货'])('保存中禁止%s，保存完成后执行读取新配置', async (entry) => {
  const gate = deferred()
  let stored: ScheduleConfig = { account_id: 'probe', enabled: true, hourly_quota: 2, current_hour_count: 0 }
  let saving = false
  const triggeredQuotas: number[] = []
  mockSchedule(() => HttpResponse.json({ success: true, data: [stored] }))
  server.use(
    http.put('/api/schedule/configs/probe', async ({ request }) => {
      const body = await request.json() as Partial<ScheduleConfig>
      saving = true
      await gate.promise
      stored = { ...stored, ...body }
      return HttpResponse.json({ success: true, data: stored })
    }),
    http.post('/api/schedule/run-now', () => {
      triggeredQuotas.push(stored.hourly_quota)
      return HttpResponse.json({ success: true, data: { triggered: true } })
    }),
  )
  render(<ToastProvider><SchedulePage /></ToastProvider>)
  const name = entry === '普通补货' ? '立即执行一轮' : entry === '全员补货' ? '强制全员补货' : '立即执行一轮补货'
  const button = await screen.findByRole('button', { name })
  await waitFor(() => expect(button).toBeEnabled())
  const quota = screen.getByRole('spinbutton', { name: /每小时配额/ })
  fireEvent.change(quota, { target: { value: '7' } })
  // 验证同一事件批次的失焦 + 点击，处理器也必须拦截，不能只靠下一次渲染禁用按钮。
  act(() => { fireEvent.blur(quota); fireEvent.click(button) })
  await waitFor(() => expect(saving).toBe(true))
  expect(button).toBeDisabled()
  expect(triggeredQuotas).toEqual([])
  await act(async () => { gate.resolve() })
  await waitFor(() => expect(button).toBeEnabled())
  fireEvent.click(button)
  await waitFor(() => expect(triggeredQuotas).toEqual([7]))
})

it('触发时淘汰旧状态请求，只用触发后的真实状态，不伪造执行中', async () => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
  const oldStatus = deferred()
  const trigger = deferred()
  let reads = 0
  let posted = false
  mockSchedule(() => HttpResponse.json({ success: true, data: [] }))
  server.use(
    http.get('/api/schedule/status', async () => {
      const running = posted
      reads++
      if (reads === 2) await oldStatus.promise
      return HttpResponse.json({ success: true, data: { running, interval_seconds: 300 } })
    }),
    http.post('/api/schedule/run-now', async () => {
      await trigger.promise
      posted = true
      return HttpResponse.json({ success: true, data: { triggered: true } })
    }),
  )
  render(<ToastProvider><SchedulePage /></ToastProvider>)
  await waitFor(() => expect(screen.getByRole('button', { name: '立即执行一轮' })).toBeEnabled())
  await act(async () => { vi.advanceTimersByTime(3000) })
  await waitFor(() => expect(reads).toBe(2))
  fireEvent.click(screen.getByRole('button', { name: '立即执行一轮' }))
  expect(await screen.findByRole('button', { name: '触发中…' })).toBeDisabled()
  expect(screen.getByText('● 未确认')).toBeInTheDocument()
  await act(async () => { oldStatus.resolve() })
  expect(screen.getByText('● 未确认')).toBeInTheDocument()
  await act(async () => { trigger.resolve() })
  expect(await screen.findByRole('button', { name: '执行中…' })).toBeDisabled()
  expect(reads).toBe(3)
  expect(screen.getByText('已提交补货请求，请查看日志确认执行结果')).toBeInTheDocument()
})

it.each([false, true])('触发后服务端仍空闲则如实展示，触发失败=%s 也恢复读取和编辑', async (fail) => {
  mockSchedule(() => HttpResponse.json({ success: true, data: [] }))
  server.use(http.post('/api/schedule/run-now', () => fail
    ? HttpResponse.json({ success: false, message: '触发失败测试' }, { status: 500 })
    : HttpResponse.json({ success: true, data: { triggered: true } })))
  render(<ToastProvider><SchedulePage /></ToastProvider>)
  const button = screen.getByRole('button', { name: '立即执行一轮' })
  await waitFor(() => expect(button).toBeEnabled())
  fireEvent.click(button)
  await screen.findByText(fail ? '触发失败测试' : '已提交补货请求，请查看日志确认执行结果')
  await waitFor(() => expect(button).toBeEnabled())
  expect(screen.getByText('● 空闲')).toBeInTheDocument()
  expect(screen.getByRole('spinbutton', { name: /每小时配额/ })).toBeEnabled()
})

it('账号加载失败统计未知并禁止执行，提示使用顶部全局重试入口', async () => {
  mockSchedule(() => HttpResponse.json({ success: true, data: [] }))
  server.use(http.get('/api/accounts', () => HttpResponse.json({ success: false, message: '账号服务不可用' }, { status: 500 })))
  render(<ToastProvider><SchedulePage /></ToastProvider>)
  expect(await screen.findByText('账号加载失败，请点击顶部重试')).toBeInTheDocument()
  expect(document.querySelector('.stat-card-blue .stat-value')).toHaveTextContent('— / —')
  expect(screen.getByRole('button', { name: '立即执行一轮' })).toBeDisabled()
  expect(screen.queryByRole('button', { name: /重试账号加载|^重试$/ })).not.toBeInTheDocument()
})

it('受保护账号依据后端判定限制编辑、排除统计，不在前端重复名称/标签规则', async () => {
  const protectedAccount = { ...account, schedule_protected: true }
  const normalAccount = { ...account, id: 'normal', name: '普通大号', schedule_protected: false }
  mockSchedule(() => HttpResponse.json({ success: true, data: [protectedAccount, normalAccount].map((acc) => ({ account_id: acc.id, enabled: true, hourly_quota: 2, current_hour_count: 0 })) }))
  server.use(http.get('/api/accounts', () => HttpResponse.json({ success: true, data: [protectedAccount, normalAccount] })))
  render(<ToastProvider><SchedulePage /></ToastProvider>)
  await screen.findByText('受保护，不参与自动或手动补货')
  await waitFor(() => expect(document.querySelector('.stat-card-blue .stat-value')).toHaveTextContent('1 / 2'))
  expect(document.querySelector('.stat-card-purple .stat-value')).toHaveTextContent('2')
  expect(screen.getByRole('checkbox', { name: '账号 测试账号 自动补货开关' })).toBeDisabled()
  expect(screen.getByRole('spinbutton', { name: '账号 测试账号 每小时配额' })).toBeDisabled()
  expect(screen.getByRole('button', { name: '账号 测试账号 调度模式' })).toBeDisabled()
  expect(screen.getByRole('textbox', { name: '账号 测试账号 别名备注模板' })).toBeDisabled()
  expect(screen.getByRole('checkbox', { name: '账号 普通大号 自动补货开关' })).toBeEnabled()
})

it('到期任务可真正暂停，重新启用会重置开始时间', async () => {
  let stored: ScheduleConfig = { account_id: 'probe', enabled: true, hourly_quota: 2, current_hour_count: 0, mode: 'duration', duration_hours: 1, started_at: '2000-01-01T00:00:00Z' }
  const bodies: Partial<ScheduleConfig>[] = []
  mockSchedule(() => HttpResponse.json({ success: true, data: [stored] }))
  server.use(http.put('/api/schedule/configs/probe', async ({ request }) => {
    const body = await request.json() as Partial<ScheduleConfig>
    bodies.push(body)
    stored = { ...stored, ...body }
    return HttpResponse.json({ success: true, data: stored })
  }))
  render(<ToastProvider><SchedulePage /></ToastProvider>)
  await screen.findByText('已到期，自动补货已停止；手动仍可执行')
  fireEvent.click(screen.getByRole('checkbox'))
  await waitFor(() => expect(screen.getByRole('checkbox')).not.toBeChecked())
  expect(bodies[0]).toEqual({ enabled: false })
  expect(await screen.findByText('已暂停')).toBeInTheDocument()
  expect(screen.queryByRole('button', { name: /重新计时/ })).not.toBeInTheDocument()
  await waitFor(() => expect(screen.getByRole('checkbox')).toBeEnabled())
  fireEvent.click(screen.getByRole('checkbox'))
  await waitFor(() => expect(document.querySelector('.stat-card-blue .stat-value')).toHaveTextContent('1 / 1'))
  expect(bodies[1].enabled).toBe(true)
  expect(Date.parse(bodies[1].started_at || '')).toBeGreaterThan(Date.now() - 5000)
})

it.each([false, true])('顺序保存配额与暂停，前一保存失败=%s 时队列仍可继续', async (failQuota) => {
  const pendingQuota = deferred()
  let stored: ScheduleConfig = { account_id: 'probe', enabled: true, hourly_quota: 2, current_hour_count: 0, mode: 'always' }
  const bodies: Partial<ScheduleConfig>[] = []
  mockSchedule(() => HttpResponse.json({ success: true, data: [stored] }))
  server.use(http.put('/api/schedule/configs/probe', async ({ request }) => {
    const body = await request.json() as Partial<ScheduleConfig>
    bodies.push(body)
    if (bodies.length === 1) {
      await pendingQuota.promise
      if (failQuota) return HttpResponse.json({ success: false, code: 'PERSISTENCE_ERROR', message: '写入失败' }, { status: 500 })
    }
    stored = { ...stored, ...body }
    return HttpResponse.json({ success: true, data: stored })
  }))
  render(<ToastProvider><SchedulePage /></ToastProvider>)
  const quota = await screen.findByDisplayValue('2')
  fireEvent.change(quota, { target: { value: '7' } })
  fireEvent.blur(quota)
  await waitFor(() => expect(bodies).toHaveLength(1))
  expect(bodies[0]).toEqual({ hourly_quota: 7 })
  await act(async () => { fireEvent.click(screen.getByRole('checkbox', { name: /自动补货开关/ })) })
  expect(bodies).toHaveLength(1)
  pendingQuota.resolve()
  await waitFor(() => expect(bodies).toHaveLength(2))
  expect(bodies[1]).toEqual({ enabled: false })
  await waitFor(() => expect(stored.enabled).toBe(false))
  expect(stored.hourly_quota).toBe(failQuota ? 2 : 7)
  await waitFor(() => expect(screen.getByRole('checkbox', { name: /自动补货开关/ })).not.toBeChecked())
})

it('轮询耗时超过三个周期仍会提交结果，不取消或重叠慢请求', async () => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
  const gate = deferred()
  let calls = 0
  mockSchedule(async () => {
    calls++
    await gate.promise
    return HttpResponse.json({ success: true, data: [{ account_id: 'probe', enabled: true, hourly_quota: 9, current_hour_count: 0 }] })
  })
  render(<ToastProvider><SchedulePage /></ToastProvider>)
  await screen.findByText('测试账号')
  await waitFor(() => expect(calls).toBe(1))
  await act(async () => { vi.advanceTimersByTime(9000) })
  expect(calls).toBe(1)
  gate.resolve()
  expect(await screen.findByDisplayValue('9')).toBeInTheDocument()
  await act(async () => { vi.advanceTimersByTime(3000) })
  await waitFor(() => expect(calls).toBe(2))
})

it.each(['quota', 'label', 'preset'] as const)('保存失败时 %s 恢复服务端值，不显示未保存的新值', async (field) => {
  const stored: ScheduleConfig = { account_id: 'probe', enabled: true, hourly_quota: 2, current_hour_count: 0, alias_label: 'scheduled' }
  mockSchedule(() => HttpResponse.json({ success: true, data: [stored] }))
  server.use(http.put('/api/schedule/configs/probe', () => HttpResponse.json({ success: false, code: 'PERSISTENCE_ERROR', message: '保存失败测试' }, { status: 500 })))
  render(<ToastProvider><SchedulePage /></ToastProvider>)
  await screen.findByDisplayValue('2')
  if (field === 'preset') fireEvent.click(screen.getByRole('button', { name: 'gpt' }))
  else {
    const input = screen.getByRole(field === 'quota' ? 'spinbutton' : 'textbox', { name: field === 'quota' ? /每小时配额/ : /别名备注模板/ })
    fireEvent.focus(input)
    fireEvent.change(input, { target: { value: field === 'quota' ? '7' : 'custom' } })
    fireEvent.blur(input)
  }
  await screen.findByText('保存失败测试')
  await waitFor(() => expect(screen.getByRole('spinbutton', { name: /每小时配额/ })).toHaveValue(2))
  expect(screen.getByRole('textbox', { name: /别名备注模板/ })).toHaveValue('scheduled')
  expect(screen.getByRole('button', { name: 'gpt' })).toHaveAttribute('aria-pressed', 'false')
})

it('未读到配置时禁止编辑，读取失败显示未知和重试，恢复后回显真实开启状态', async () => {
  const gate = deferred()
  mockSchedule(async () => {
    await gate.promise
    return HttpResponse.json({ success: false, code: 'PERSISTENCE_ERROR', message: '配置服务不可用' }, { status: 500 })
  })
  render(<ToastProvider><SchedulePage /></ToastProvider>)
  await screen.findByText('测试账号')
  expect(screen.getByRole('checkbox')).toBeDisabled()
  gate.resolve()
  expect(await screen.findByRole('alert')).toHaveTextContent('配置服务不可用')
  expect(document.querySelector('.stat-card-blue .stat-value')).toHaveTextContent('— / 1')
  expect(screen.getByRole('checkbox')).toBeDisabled()
  server.use(http.get('/api/schedule/configs', () => HttpResponse.json({ success: true, data: [{ account_id: 'probe', enabled: true, hourly_quota: 9, current_hour_count: 0 }] })))
  fireEvent.click(screen.getByRole('button', { name: '重新读取' }))
  await waitFor(() => expect(screen.getByRole('checkbox')).toBeChecked())
  expect(screen.getByRole('checkbox')).toBeEnabled()
  expect(screen.queryByRole('alert')).not.toBeInTheDocument()
})

it('时间保存尚未完成时切换模式，仅提交模式不覆盖前一时间更新', async () => {
  const gate = deferred()
  let stored: ScheduleConfig = { account_id: 'probe', enabled: true, hourly_quota: 2, current_hour_count: 0, mode: 'daily_window', start_time: '09:00', end_time: '18:00' }
  const bodies: Partial<ScheduleConfig>[] = []
  mockSchedule(() => HttpResponse.json({ success: true, data: [stored] }))
  server.use(http.put('/api/schedule/configs/probe', async ({ request }) => {
    const body = await request.json() as Partial<ScheduleConfig>
    bodies.push(body)
    if (bodies.length === 1) await gate.promise
    stored = { ...stored, ...body }
    return HttpResponse.json({ success: true, data: stored })
  }))
  render(<ToastProvider><SchedulePage /></ToastProvider>)
  const start = await screen.findByDisplayValue('09:00')
  fireEvent.focus(start)
  fireEvent.change(start, { target: { value: '10:00' } })
  fireEvent.blur(start)
  await waitFor(() => expect(bodies).toHaveLength(1))
  fireEvent.click(screen.getByRole('button', { name: /调度模式/ }))
  fireEvent.click(screen.getByRole('option', { name: '全天常驻 (24h)' }))
  await act(async () => { gate.resolve() })
  await waitFor(() => expect(bodies).toHaveLength(2))
  expect(bodies).toEqual([{ start_time: '10:00' }, { mode: 'always' }])
  expect(stored.start_time).toBe('10:00')
})

it('到期账号不计入自动补货，保留真实开关，独立按钮重新计时', async () => {
  let stored: ScheduleConfig = { account_id: 'probe', enabled: true, hourly_quota: 2, current_hour_count: 0, mode: 'duration', duration_hours: 1, started_at: '2000-01-01T00:00:00Z' }
  let body: Partial<ScheduleConfig> | undefined
  mockSchedule(() => HttpResponse.json({ success: true, data: [stored] }))
  server.use(http.put('/api/schedule/configs/probe', async ({ request }) => {
    body = await request.json() as Partial<ScheduleConfig>
    stored = { ...stored, ...body }
    return HttpResponse.json({ success: true, data: stored })
  }))
  render(<ToastProvider><SchedulePage /></ToastProvider>)
  await screen.findByText('已到期，自动补货已停止；手动仍可执行')
  expect(screen.getByRole('checkbox')).toBeChecked()
  expect(document.querySelector('.stat-card-blue .stat-value')).toHaveTextContent('0 / 1')
  fireEvent.click(screen.getByRole('button', { name: /重新计时/ }))
  await waitFor(() => expect(document.querySelector('.stat-card-blue .stat-value')).toHaveTextContent('1 / 1'))
  expect(body?.enabled).toBeUndefined()
  expect(Date.parse(body?.started_at || '')).toBeGreaterThan(Date.now() - 5000)
  expect(screen.getByRole('checkbox')).toBeChecked()
  expect(screen.queryByRole('button', { name: /重新计时/ })).not.toBeInTheDocument()
})

it('千账号仅渲染一页，统计保持全量，翻页后可操作其他账号', async () => {
  const many = Array.from({ length: 1000 }, (_, i) => ({ ...account, id: `acc_${i}`, name: `分页账号 ${i + 1}` }))
  mockSchedule(() => HttpResponse.json({ success: true, data: many.map((acc) => ({ account_id: acc.id, enabled: true, hourly_quota: 2, current_hour_count: 0 })) }))
  server.use(http.get('/api/accounts', () => HttpResponse.json({ success: true, data: many })))
  render(<ToastProvider><SchedulePage /></ToastProvider>)
  await screen.findByText('分页账号 1')
  await waitFor(() => expect(document.querySelector('.stat-card-blue .stat-value')).toHaveTextContent('1000 / 1000'))
  expect(document.querySelectorAll('tbody tr')).toHaveLength(25)
  expect(screen.queryByText('分页账号 26')).not.toBeInTheDocument()
  fireEvent.click(screen.getByRole('button', { name: '下一页' }))
  expect(screen.getByText('分页账号 26')).toBeInTheDocument()
  expect(screen.queryByText('分页账号 1')).not.toBeInTheDocument()
  expect(document.querySelectorAll('tbody tr')).toHaveLength(25)
})

it('账号保存中仍拉取日志和运行状态，旧配置响应不能覆盖保存结果', async () => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
  const saveGate = deferred()
  const pollGate = deferred()
  let reads = 0
  let logReads = 0
  let stored: ScheduleConfig = { account_id: 'probe', enabled: true, hourly_quota: 2, current_hour_count: 0 }
  mockSchedule(async () => {
    reads++
    const snapshot = { ...stored }
    if (reads === 2) await pollGate.promise
    return HttpResponse.json({ success: true, data: [snapshot] })
  })
  server.use(
    http.get('/api/schedule/logs', () => { logReads++; return HttpResponse.json({ success: true, data: [{ time: '10:00:00', message: `创建成功 ${logReads}` }] }) }),
    http.put('/api/schedule/configs/probe', async ({ request }) => {
      const body = await request.json() as Partial<ScheduleConfig>
      await saveGate.promise
      stored = { ...stored, ...body }
      return HttpResponse.json({ success: true, data: stored })
    }),
  )
  render(<ToastProvider><SchedulePage /></ToastProvider>)
  const quota = await screen.findByDisplayValue('2')
  fireEvent.focus(quota)
  fireEvent.change(quota, { target: { value: '7' } })
  fireEvent.blur(quota)
  await screen.findByText('正在保存…')
  await act(async () => { vi.advanceTimersByTime(3000) })
  await screen.findByText('创建成功 2')
  expect(reads).toBe(2)
  await act(async () => { saveGate.resolve() })
  await waitFor(() => expect(quota).toHaveValue(7))
  await act(async () => { pollGate.resolve() })
  expect(quota).toHaveValue(7)
})
