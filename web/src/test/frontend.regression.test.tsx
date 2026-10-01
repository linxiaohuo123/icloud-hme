/**
 * [INPUT]: 依赖 React Testing Library、MSW、路由、账号与收件箱组件及系统设置页面
 * [OUTPUT]: 覆盖配置读取失败、账号能力更新、批量生成关闭边界、URL 筛选、缓存失效、弹窗键盘与窗口事件、异步启停及正文验证码同步的集成回归测试
 * [POS]: web/src/test 的跨组件业务流程回归防线；所有写入均由 MSW 拦截
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { MemoryRouter, Route, Routes, useLocation, useNavigate } from 'react-router-dom'
import { beforeEach, expect, it, vi } from 'vitest'
import { server } from './server'
import { invalidateAccounts } from '../hooks/useAccounts'
import Dialog from '../components/Dialog'
import Select from '../components/Select'
import { ToastProvider } from '../components/ToastProvider'
import InboxTableRow from '../components/inbox/InboxTableRow'
import InboxTableView, { clearInboxSnapshotCache, getModuleMessageCache } from '../components/inbox/InboxTableView'
import AccountWorkspace from '../pages/AccountWorkspace'
import SettingsPage from '../pages/SettingsPage'
import AliasesPage from '../pages/AliasesPage'
import { buildMailCacheKey } from '../utils/mail'
import type { AccountSummary, UpdateNotifySettingsRequest } from '../api/types'

const accounts = ['account_a', 'account_b'].map(id => ({
  id, name: id, real_email: `${id}@example.com`, icloud_email: `${id}@icloud.com`,
  host: 'icloud.com', status: 'active', has_app_password: true, has_cookies: true,
  has_proxy: false, alias_total: 2, alias_active: 2, created_at: '', last_validated: '',
}))
const message = {
  id: '1', message_ref: 'INBOX:100:1', from: 'sender@example.com',
  to: 'old@icloud.com', subject: '测试邮件', preview: 'Verification code 123456',
  date: '2026-10-01T00:00:00Z', folder: 'INBOX',
}
function Navigation() {
  const navigate = useNavigate()
  const location = useLocation()
  return <>
    <output data-testid="url">{location.pathname + location.search}</output>
    <button onClick={() => navigate('/inbox?account_id=account_b&alias=new@icloud.com&folder=Junk&limit=100&days=30')}>前往账号 B 收件箱</button>
    <button onClick={() => navigate(-1)}>后退</button>
  </>
}
beforeEach(() => {
  clearInboxSnapshotCache()
  server.use(
    http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
    http.get('/api/accounts/:id', ({ params }) => HttpResponse.json({ success: true, data: accounts.find(a => a.id === params.id) })),
    http.get('/api/aliases', () => HttpResponse.json({ success: true, data: { aliases: [
      { anonymousId: 'old', email: 'old@icloud.com', label: '', active: true, createdAt: '' },
      { anonymousId: 'new', email: 'new@icloud.com', label: '', active: true, createdAt: '' },
    ] } })),
    http.get('/api/inbox', ({ request }) => HttpResponse.json({ success: true, data: {
      account_id: new URL(request.url).searchParams.get('account_id'), count: 1, method: 'imap', messages: [message],
    } })),
  )
})

it('Escape 优先关闭下拉，第二次才关闭父弹窗', async () => {
  const close = vi.fn()
  const user = userEvent.setup()
  render(<Dialog open title="测试弹窗" onClose={close}>
    <Select value="a" onChange={() => {}} options={[{ value: 'a', label: '选项 A' }, { value: 'b', label: '选项 B' }]} />
    <button>保存</button>
  </Dialog>)
  await user.click(screen.getByRole('button', { name: '选项 A' }))
  expect(screen.getByRole('listbox')).toBeInTheDocument()
  await user.keyboard('{Escape}')
  expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
  expect(close).not.toHaveBeenCalled()
  expect(screen.getByRole('button', { name: '选项 A' })).toHaveFocus()
  await user.keyboard('{Escape}')
  expect(close).toHaveBeenCalledTimes(1)
})

it('键盘 Enter/空格可复制验证码，行本身仍可打开详情', async () => {
  const open = vi.fn(), copy = vi.fn()
  const user = userEvent.setup()
  render(<table><tbody><InboxTableRow message={message} copiedCode={null} copiedAlias={null}
    onOpenMessage={open} onCopyCode={copy} onCopyAlias={() => {}} onDelete={() => {}} /></tbody></table>)
  screen.getByRole('button', { name: /123456/ }).focus()
  await user.keyboard('{Enter} ')
  expect(copy).toHaveBeenCalledTimes(2)
  expect(copy).toHaveBeenCalledWith('123456')
  expect(open).not.toHaveBeenCalled()
  screen.getByRole('button', { name: /查看发自/ }).focus()
  await user.keyboard('{Enter}')
  expect(open).toHaveBeenCalledTimes(1)
})

it('全局收件箱同页导航和后退均同步账号、别名及查询参数', async () => {
  const queries: URLSearchParams[] = []
  server.use(http.get('/api/inbox', ({ request }) => {
    queries.push(new URL(request.url).searchParams)
    return HttpResponse.json({ success: true, data: { count: 1, method: 'imap', messages: [message] } })
  }))
  render(<MemoryRouter initialEntries={['/inbox?account_id=account_a&alias=old@icloud.com']}>
    <ToastProvider><Navigation /><InboxTableView accountId="account_a" /></ToastProvider>
  </MemoryRouter>)
  await screen.findByText('测试邮件')
  fireEvent.click(screen.getByRole('button', { name: '前往账号 B 收件箱' }))
  await waitFor(() => expect(screen.getByRole('combobox', { name: '账号' })).toHaveValue('account_b'))
  expect(screen.getByRole('combobox', { name: '别名' })).toHaveValue('new@icloud.com')
  expect(screen.getByRole('combobox', { name: '文件夹' })).toHaveValue('Junk')
  await waitFor(() => expect(queries.at(-1)?.get('account_id')).toBe('account_b'))
  expect(queries.at(-1)?.get('alias')).toBe('new@icloud.com')
  expect(queries.at(-1)?.get('limit')).toBe('100')
  expect(queries.at(-1)?.get('days')).toBe('30')
  fireEvent.click(screen.getByRole('button', { name: '后退' }))
  await waitFor(() => expect(queries.at(-1)?.get('account_id')).toBe('account_a'))
  expect(screen.getByRole('combobox', { name: '别名' })).toHaveValue('old@icloud.com')
  expect(screen.getByRole('combobox', { name: '文件夹' })).toHaveValue('INBOX')
  expect(queries.at(-1)?.get('limit')).toBe('20')
  expect(queries.at(-1)?.get('days')).toBe('7')
})

it('工作台别名筛选同步 URL，切换标签后保持当前筛选', async () => {
  render(<MemoryRouter initialEntries={['/workspace/account_a?tab=inbox&alias=old@icloud.com']}>
    <ToastProvider><Navigation /><Routes><Route path="/workspace/:accountId" element={<AccountWorkspace />} /></Routes></ToastProvider>
  </MemoryRouter>)
  await screen.findByText('测试邮件')
  await userEvent.setup().click(screen.getByRole('button', { name: 'old@icloud.com' }))
  fireEvent.click(within(screen.getByRole('listbox')).getByRole('option', { name: 'new@icloud.com' }))
  await waitFor(() => expect(screen.getByRole('combobox', { name: '别名' })).toHaveValue('new@icloud.com'))
  expect(screen.getByTestId('url')).toHaveTextContent('alias=new%40icloud.com')
  fireEvent.click(screen.getByRole('button', { name: /别名管理/ }))
  fireEvent.click(screen.getByRole('button', { name: /收件箱与验证码/ }))
  expect(screen.getByRole('combobox', { name: '别名' })).toHaveValue('new@icloud.com')
})

it('通知配置读取失败时不可保存，重试后按已读取策略保存', async () => {
  let reads = 0
  const submitted: UpdateNotifySettingsRequest[] = []
  const settings = { event_kinds: { cookie_expired: true, cookie_recovered: false, quota_low: true }, quota_threshold: 700 }
  server.use(
    http.get('/api/settings/notify', () => ++reads === 1
      ? HttpResponse.json({ success: false, code: 'INTERNAL_ERROR', message: '配置读取失败' }, { status: 500 })
      : HttpResponse.json({ success: true, data: settings })),
    http.get('/api/settings/camoufox', () => HttpResponse.json({ success: true, data: { available: false, ready: false } })),
    http.put('/api/settings/notify', async ({ request }) => {
      submitted.push(await request.json() as UpdateNotifySettingsRequest)
      return HttpResponse.json({ success: true, data: settings })
    }),
  )
  render(<ToastProvider><SettingsPage /></ToastProvider>)
  await screen.findByText('配置读取失败')
  expect(screen.queryByRole('button', { name: '保存配置' })).not.toBeInTheDocument()
  expect(submitted).toHaveLength(0)
  fireEvent.click(screen.getByRole('button', { name: '重试' }))
  await screen.findByRole('button', { name: '保存配置' })
  expect(screen.getByLabelText('配额水位阈值 (活跃别名数)')).toHaveValue(700)
  fireEvent.click(screen.getByRole('button', { name: '保存配置' }))
  await waitFor(() => expect(submitted).toEqual([settings]))
})

it('收件箱卸载后的账号更新清理详情与列表快照，重新进入读取新正文', async () => {
  let detailReads = 0
  server.use(http.get('/api/inbox/:id', () => HttpResponse.json({ success: true, data: {
    account_id: 'account_a', message: { ...message, body: ++detailReads === 1 ? '旧收件箱正文' : '新收件箱正文', content_type: 'text/plain' },
  } })))
  function Inbox() {
    return <MemoryRouter><ToastProvider><InboxTableView accountId="account_a" accountSummary={accounts[0]} fixedAccount /></ToastProvider></MemoryRouter>
  }
  const view = render(<Inbox />)
  await screen.findByText('测试邮件')
  fireEvent.click(screen.getByRole('button', { name: '测试邮件' }))
  await screen.findByText('旧收件箱正文')
  const cacheKey = buildMailCacheKey('account_a', message)
  expect(getModuleMessageCache(cacheKey)).toBeDefined()
  view.unmount()
  act(() => window.dispatchEvent(new CustomEvent('account-updated', { detail: { accountId: 'account_a' } })))
  expect(getModuleMessageCache(cacheKey)).toBeUndefined()
  render(<Inbox />)
  expect(screen.queryByText('旧收件箱正文')).not.toBeInTheDocument()
  await screen.findByText('测试邮件')
  fireEvent.click(screen.getByRole('button', { name: '测试邮件' }))
  await screen.findByText('新收件箱正文')
  expect(detailReads).toBe(2)
})

it('批量生成成功后刷新账号列表仍保留生成结果', async () => {
  let created = false
  let accountReadsAfterCreate = 0
  server.use(
    http.get('/api/accounts', () => {
      if (created) accountReadsAfterCreate++
      return HttpResponse.json({ success: true, data: accounts })
    }),
    http.post('/api/create/batch', () => {
      created = true
      return HttpResponse.json({ success: true, data: { account_id: 'account_a', requested: 1,
        created_count: 1, created: [{ email: 'batch-created@icloud.com', anonymousId: 'batch' }], failed: [] } })
    }),
  )
  render(<MemoryRouter><ToastProvider><AliasesPage /></ToastProvider></MemoryRouter>)
  await waitFor(() => expect(screen.getByRole('button', { name: '批量生成' })).toBeEnabled())
  fireEvent.click(screen.getByRole('button', { name: '批量生成' }))
  fireEvent.click(screen.getByRole('button', { name: '生成 1 个别名' }))
  await waitFor(() => expect(accountReadsAfterCreate).toBeGreaterThan(0))
  expect(screen.getByRole('dialog', { name: '别名生成结果' })).toBeInTheDocument()
  expect(screen.getByText('batch-created@icloud.com')).toBeInTheDocument()
  expect(screen.getByRole('button', { name: '一键复制全部' })).toBeInTheDocument()
})

it('工作台接入外部 IMAP 后立即更新收件能力，无需切换标签', async () => {
  let bound = false
  let accountReadsAfterBinding = 0
  const queries: URLSearchParams[] = []
  const updatedAccount = () => ({ ...accounts[0], has_app_password: false,
    ...(bound ? { mailbox: { provider: 'qq', email: 'new-inbox@example.com', imap_host: 'imap.qq.com', imap_port: 993 } } : {}),
  })
  server.use(
    http.get('/api/accounts', () => HttpResponse.json({ success: true, data: [updatedAccount()] })),
    http.get('/api/accounts/:id', () => {
      if (bound) accountReadsAfterBinding++
      return HttpResponse.json({ success: true, data: updatedAccount() })
    }),
    http.put('/api/accounts/:id/mailbox', () => {
      bound = true
      return HttpResponse.json({ success: true, data: {} })
    }),
    http.get('/api/inbox', ({ request }) => {
      queries.push(new URL(request.url).searchParams)
      return HttpResponse.json({ success: true, data: { count: 1, method: bound ? 'imap' : 'webmail', messages: [message] } })
    }),
  )
  render(<MemoryRouter initialEntries={['/workspace/account_a?tab=inbox']}>
    <ToastProvider><Routes><Route path="/workspace/:accountId" element={<AccountWorkspace />} /></Routes></ToastProvider>
  </MemoryRouter>)
  await screen.findByText('测试邮件')
  expect(screen.getByRole('combobox', { name: '文件夹' })).toBeDisabled()
  expect(queries.at(-1)?.has('folder')).toBe(false)
  fireEvent.click(screen.getByTitle('尚未配置收件箱，点击配置'))
  fireEvent.change(screen.getByLabelText('收件邮箱'), { target: { value: 'new-inbox@example.com' } })
  fireEvent.change(screen.getByLabelText('邮箱授权码'), { target: { value: 'mock-only-code' } })
  fireEvent.click(screen.getByRole('button', { name: '验证并接入' }))
  await waitFor(() => expect(accountReadsAfterBinding).toBeGreaterThan(0))
  await screen.findByTitle('点击修改收件箱配置 (IMAP: imap.qq.com:993)')
  await waitFor(() => expect(screen.getByRole('combobox', { name: '文件夹' })).toBeEnabled())
  expect(screen.getByRole('combobox', { name: '时间范围' })).toBeEnabled()
  await waitFor(() => expect(queries.at(-1)?.get('folder')).toBe('INBOX'))
  expect(queries.at(-1)?.get('days')).toBe('7')
})

it('全局收件箱收到账号更新后重新读取能力，恢复 IMAP 查询参数', async () => {
  let configured = false
  let readsAfterUpdate = 0
  const queries: URLSearchParams[] = []
  server.use(
    http.get('/api/accounts', () => {
      if (configured) readsAfterUpdate++
      return HttpResponse.json({ success: true, data: [{ ...accounts[0], has_app_password: configured }] })
    }),
    http.get('/api/inbox', ({ request }) => {
      queries.push(new URL(request.url).searchParams)
      return HttpResponse.json({ success: true, data: { count: 1, method: configured ? 'imap' : 'webmail', messages: [message] } })
    }),
  )
  render(<MemoryRouter><ToastProvider><InboxTableView /></ToastProvider></MemoryRouter>)
  await screen.findByText('测试邮件')
  expect(screen.getByRole('combobox', { name: '文件夹' })).toBeDisabled()
  configured = true
  act(() => invalidateAccounts('account_a'))
  await waitFor(() => expect(readsAfterUpdate).toBeGreaterThan(0))
  await waitFor(() => expect(screen.getByRole('combobox', { name: '文件夹' })).toBeEnabled())
  await waitFor(() => expect(queries.at(-1)?.get('days')).toBe('7'))
  expect(queries.at(-1)?.get('folder')).toBe('INBOX')
})

it('账号配置变更后丢弃旧的服务端 WebMail 降级结论', async () => {
  let configured = false
  const queries: URLSearchParams[] = []
  server.use(http.get('/api/inbox', ({ request }) => {
    const query = new URL(request.url).searchParams
    queries.push(query)
    if (!configured && query.has('days')) {
      return HttpResponse.json({ success: false, code: 'CAPABILITY_UNSUPPORTED', message: '当前仅支持 WebMail' }, { status: 400 })
    }
    return HttpResponse.json({ success: true, data: { count: 1, method: configured ? 'imap' : 'webmail', messages: [message] } })
  }))
  function Inbox({ account }: { account: AccountSummary }) {
    return <MemoryRouter><ToastProvider><InboxTableView accountId={account.id} accountSummary={account} fixedAccount /></ToastProvider></MemoryRouter>
  }
  const view = render(<Inbox account={accounts[0]} />)
  await screen.findByText('测试邮件')
  expect(screen.getByRole('combobox', { name: '文件夹' })).toBeDisabled()
  expect(queries.at(-1)?.has('days')).toBe(false)
  configured = true
  view.rerender(<Inbox account={{ ...accounts[0], mailbox: {
    provider: 'qq', email: 'bound@example.com', imap_host: 'imap.qq.com', imap_port: 993,
  } }} />)
  await waitFor(() => expect(screen.getByRole('combobox', { name: '文件夹' })).toBeEnabled())
  await waitFor(() => expect(queries.at(-1)?.get('days')).toBe('7'))
  expect(queries.at(-1)?.get('folder')).toBe('INBOX')
})

it('批量申请中 Escape 和遮罩点击均不能关闭，完成后保留结果并允许关闭', async () => {
  let started = false
  let release!: () => void
  const pending = new Promise<void>(resolve => { release = resolve })
  server.use(http.post('/api/create/batch', async () => {
    started = true
    await pending
    return HttpResponse.json({ success: true, data: { account_id: 'account_a', requested: 1, created_count: 1,
      created: [{ email: 'pending-batch@icloud.com', anonymousId: 'pending' }], failed: [] } })
  }))
  const user = userEvent.setup()
  render(<MemoryRouter><ToastProvider><AliasesPage /></ToastProvider></MemoryRouter>)
  try {
    await waitFor(() => expect(screen.getByRole('button', { name: '批量生成' })).toBeEnabled())
    await user.click(screen.getByRole('button', { name: '批量生成' }))
    await user.click(screen.getByRole('button', { name: '生成 1 个别名' }))
    await waitFor(() => expect(started).toBe(true))
    await user.keyboard('{Escape}')
    expect(screen.getByRole('dialog', { name: '批量生成别名' })).toBeInTheDocument()
    await user.click(screen.getByRole('dialog').parentElement!)
    expect(screen.getByRole('dialog', { name: '批量生成别名' })).toBeInTheDocument()
    await act(async () => release())
    await screen.findByText('pending-batch@icloud.com')
    expect(screen.getByRole('dialog', { name: '别名生成结果' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '一键复制全部' })).toBeEnabled()
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  } finally {
    release()
  }
})

it('弹窗下拉框支持方向键、Home/End、Enter/空格，跳过禁用项并恢复触发器焦点', async () => {
  const changed = vi.fn()
  const user = userEvent.setup()
  render(<Dialog title="键盘导航" open onClose={() => {}}>
    <Select value="a" onChange={changed} options={[
      { value: 'a', label: '选项 A' }, { value: 'disabled', label: '禁用项', disabled: true }, { value: 'b', label: '选项 B' },
    ]} />
    <button>保存</button>
  </Dialog>)
  const trigger = screen.getByRole('button', { name: '选项 A' })
  expect(trigger).toHaveFocus()
  await user.keyboard('{ArrowDown}')
  expect(within(screen.getByRole('listbox')).getByRole('option', { name: '选项 A' })).toHaveFocus()
  await user.keyboard('{ArrowDown}')
  expect(within(screen.getByRole('listbox')).getByRole('option', { name: '选项 B' })).toHaveFocus()
  await user.keyboard('{Home}')
  expect(within(screen.getByRole('listbox')).getByRole('option', { name: '选项 A' })).toHaveFocus()
  await user.keyboard('{End}{Enter}')
  expect(changed).toHaveBeenLastCalledWith('b')
  expect(trigger).toHaveFocus()
  expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
  await user.keyboard('{Enter}{ArrowUp} ')
  expect(changed).toHaveBeenCalledTimes(2)
  expect(changed).toHaveBeenLastCalledWith('b')
  expect(trigger).toHaveFocus()
})

it('弹窗内下拉框按 Tab/Shift+Tab 关闭浮层并继续表单焦点顺序', async () => {
  const user = userEvent.setup()
  render(<Dialog title="Tab 导航" open onClose={() => {}}>
    <button>上一个字段</button>
    <Select value="a" onChange={() => {}} options={[{ value: 'a', label: '选项 A' }, { value: 'b', label: '选项 B' }]} />
    <button>保存</button>
  </Dialog>)
  const trigger = screen.getByRole('button', { name: '选项 A' })
  trigger.focus()
  await user.keyboard('{Enter}')
  expect(within(screen.getByRole('listbox')).getByRole('option', { name: '选项 A' })).toHaveFocus()
  await user.tab()
  expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
  expect(screen.getByRole('button', { name: '保存' })).toHaveFocus()
  await user.tab()
  expect(screen.getByRole('button', { name: '上一个字段' })).toHaveFocus()
  trigger.focus()
  await user.keyboard('{Enter}')
  await user.tab({ shift: true })
  expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
  expect(screen.getByRole('button', { name: '上一个字段' })).toHaveFocus()
})

it('业务弹窗打开时 Ctrl+K 保持焦点，关闭后恢复账号搜索快捷键', async () => {
  const scrollDescriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'scrollIntoView')
  Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', { configurable: true, value: vi.fn() })
  try {
    render(<MemoryRouter initialEntries={['/workspace/account_a?tab=inbox']}>
      <ToastProvider><Routes><Route path="/workspace/:accountId" element={<AccountWorkspace />} /></Routes></ToastProvider>
    </MemoryRouter>)
    await screen.findByText('测试邮件')
    fireEvent.click(screen.getByTitle('iCloud IMAP 已配置'))
    const emailInput = screen.getByLabelText('收件邮箱')
    emailInput.focus()
    const user = userEvent.setup()
    await user.keyboard('{Control>}k{/Control}')
    expect(screen.queryByPlaceholderText('搜索账号或邮箱...')).not.toBeInTheDocument()
    expect(emailInput).toHaveFocus()
    expect(screen.getByRole('dialog', { name: '接入收件邮箱' })).toBeInTheDocument()
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    await user.keyboard('{Control>}k{/Control}')
    expect(await screen.findByPlaceholderText('搜索账号或邮箱...')).toHaveFocus()
  } finally {
    if (scrollDescriptor) Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', scrollDescriptor)
    else Reflect.deleteProperty(HTMLElement.prototype, 'scrollIntoView')
  }
})

it.each([true, false])('工作台启停等待写入和刷新完成，切换标签仍不重复提交 (原 active=%s)', async (initialActive) => {
  let active = initialActive
  let writes = 0
  let refreshing = false
  let releaseWrite!: () => void
  let releaseRead!: () => void
  const writePending = new Promise<void>(resolve => { releaseWrite = resolve })
  const readPending = new Promise<void>(resolve => { releaseRead = resolve })
  server.use(
    http.get('/api/aliases', async () => {
      if (writes > 0) {
        refreshing = true
        await readPending
      }
      return HttpResponse.json({ success: true, data: { aliases: [
        { anonymousId: 'old', email: 'old@icloud.com', label: '', active, createdAt: '' },
        { anonymousId: 'new', email: 'new@icloud.com', label: '', active: true, createdAt: '' },
      ] } })
    }),
    http.post(`/api/aliases/old/${initialActive ? 'deactivate' : 'reactivate'}`, async () => {
      writes++
      await writePending
      active = !initialActive
      return HttpResponse.json({ success: true, data: {} })
    }),
  )
  render(<MemoryRouter initialEntries={['/workspace/account_a']}>
    <ToastProvider><Routes><Route path="/workspace/:accountId" element={<AccountWorkspace />} /></Routes></ToastProvider>
  </MemoryRouter>)
  try {
    let row = (await screen.findByText('old@icloud.com')).closest('tr')!
    let otherRow = screen.getByText('new@icloud.com').closest('tr')!
    let button = within(row).getByTitle(initialActive ? '停用此别名' : '启用此别名')
    const user = userEvent.setup()
    await user.click(button)
    await waitFor(() => expect(writes).toBe(1))
    expect(button).toBeDisabled()
    expect(within(otherRow).getByTitle('停用此别名')).toBeDisabled()
    await user.click(button)
    expect(writes).toBe(1)
    await user.click(screen.getByRole('button', { name: /收件箱与验证码/ }))
    await screen.findByText('测试邮件')
    await user.click(screen.getByRole('button', { name: /别名管理/ }))
    row = screen.getByText('old@icloud.com').closest('tr')!
    otherRow = screen.getByText('new@icloud.com').closest('tr')!
    button = within(row).getByTitle(initialActive ? '停用此别名' : '启用此别名')
    expect(button).toBeDisabled()
    await user.click(button)
    expect(writes).toBe(1)
    await act(async () => releaseWrite())
    await waitFor(() => expect(refreshing).toBe(true))
    expect(button).toBeDisabled()
    expect(button).toHaveTextContent('处理中…')
    await user.click(button)
    expect(writes).toBe(1)
    await act(async () => releaseRead())
    await waitFor(() => expect(within(row).getByTitle(initialActive ? '启用此别名' : '停用此别名')).toBeEnabled())
    expect(within(row).getByText(initialActive ? '已停用' : '活跃')).toBeInTheDocument()
    expect(within(otherRow).getByTitle('停用此别名')).toBeEnabled()
    expect(writes).toBe(1)
  } finally {
    releaseWrite()
    releaseRead()
  }
})

it('工作台启停失败后显示错误并恢复操作，再次提交可成功', async () => {
  let writes = 0
  server.use(
    http.post('/api/aliases/old/deactivate', () => ++writes === 1
      ? HttpResponse.json({ success: false, code: 'UPSTREAM_FAILURE', message: '停用失败' }, { status: 502 })
      : HttpResponse.json({ success: true, data: {} })),
    http.get('/api/aliases', () => HttpResponse.json({ success: true, data: { aliases: [
      { anonymousId: 'old', email: 'old@icloud.com', label: '', active: writes < 2, createdAt: '' },
    ] } })),
  )
  render(<MemoryRouter initialEntries={['/workspace/account_a']}>
    <ToastProvider><Routes><Route path="/workspace/:accountId" element={<AccountWorkspace />} /></Routes></ToastProvider>
  </MemoryRouter>)
  await screen.findByText('old@icloud.com')
  fireEvent.click(screen.getByTitle('停用此别名'))
  await screen.findByText('停用失败')
  expect(screen.getByTitle('停用此别名')).toBeEnabled()
  fireEvent.click(screen.getByTitle('停用此别名'))
  await waitFor(() => expect(screen.getByTitle('启用此别名')).toBeEnabled())
  expect(writes).toBe(2)
})

it.each(['resize', 'window-scroll', 'document-scroll'])('下拉框在 %s 时正常关闭，无异常且恢复触发器焦点', async (event) => {
  const errors: string[] = []
  const handleError = (e: ErrorEvent) => { errors.push(e.message); e.preventDefault() }
  window.addEventListener('error', handleError)
  try {
    render(<Select value="a" onChange={() => {}} options={[{ value: 'a', label: '选项 A' }]} />)
    const trigger = screen.getByRole('button', { name: '选项 A' })
    await userEvent.setup().click(trigger)
    expect(within(screen.getByRole('listbox')).getByRole('option', { name: '选项 A' })).toHaveFocus()
    if (event === 'resize') fireEvent.resize(window)
    else fireEvent.scroll(event === 'window-scroll' ? window : document)
    expect(errors).toEqual([])
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
    expect(trigger).toHaveFocus()
  } finally {
    window.removeEventListener('error', handleError)
  }
})

it('下拉菜单自身滚动不关闭，外部滚动关闭时保留已移出的焦点', async () => {
  render(<>
    <Select value="a" onChange={() => {}} options={[{ value: 'a', label: '选项 A' }]} />
    <button>外部控件</button>
  </>)
  await userEvent.setup().click(screen.getByRole('button', { name: '选项 A' }))
  const menu = screen.getByRole('listbox')
  fireEvent.scroll(menu)
  expect(menu).toBeInTheDocument()
  expect(within(menu).getByRole('option', { name: '选项 A' })).toHaveFocus()
  screen.getByRole('button', { name: '外部控件' }).focus()
  fireEvent.scroll(document)
  expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
  expect(screen.getByRole('button', { name: '外部控件' })).toHaveFocus()
})

it('详情读取正文验证码后立即同步列表，保留摘要未读状态并隔离同 UID 不同文件夹', async () => {
  const summary = { ...message, id: '7', message_ref: 'INBOX:300:7', subject: '正文验证码邮件', preview: '请查看邮件正文', unread: true }
  const other = { ...summary, message_ref: 'Junk:300:7', folder: 'Junk', subject: '另一文件夹邮件' }
  server.use(
    http.get('/api/inbox', () => HttpResponse.json({ success: true, data: {
      account_id: 'account_a', count: 2, method: 'imap', messages: [summary, other],
    } })),
    http.get('/api/inbox/:id', () => HttpResponse.json({ success: true, data: {
      account_id: 'account_a', message: { ...summary, unread: false, body: 'Your verification code is 876543', content_type: 'text/plain', body_complete: true },
    } })),
  )
  render(<MemoryRouter><ToastProvider><InboxTableView accountId="account_a" accountSummary={accounts[0]} fixedAccount /></ToastProvider></MemoryRouter>)
  const subject = await screen.findByRole('button', { name: '正文验证码邮件' })
  const row = subject.closest('tr')!
  expect(within(row).queryByRole('button', { name: /876543/ })).not.toBeInTheDocument()
  fireEvent.click(subject)
  await within(await screen.findByRole('dialog')).findByText('876543')
  await userEvent.setup().keyboard('{Escape}')
  expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  expect(within(row).getByRole('button', { name: /876543/ })).toBeInTheDocument()
  const otherRow = screen.getByRole('button', { name: '另一文件夹邮件' }).closest('tr')!
  expect(within(otherRow).queryByRole('button', { name: /876543/ })).not.toBeInTheDocument()
  fireEvent.click(screen.getByRole('checkbox', { name: '仅看未读' }))
  expect(screen.getByRole('button', { name: '正文验证码邮件' })).toBeInTheDocument()
})
