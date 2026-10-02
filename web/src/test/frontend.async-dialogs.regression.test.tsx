/**
 * [INPUT]: 真实业务页面与弹窗、MSW 延迟请求、React Testing Library
 * [OUTPUT]: 编辑草稿、保存锁、弹窗会话、代理检测、创建表单及别名说明的回归覆盖
 * [POS]: 前端跨组件异步业务流程测试；所有写入使用虚构数据与 MSW
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { beforeEach, expect, it, vi } from 'vitest'
import type { ReactElement } from 'react'
import type { AccountSummary, Alias, FullMessage } from '../api/types'
import { setCSRFToken } from '../api/client'
import { invalidateAccounts } from '../hooks/useAccounts'
import AccountsPage from '../pages/AccountsPage'
import AccountWorkspace from '../pages/AccountWorkspace'
import AliasesPage from '../pages/AliasesPage'
import BusinessTagsPage from '../pages/BusinessTagsPage'
import CreateAliasDialog from '../components/CreateAliasDialog'
import AppPasswordDialog from '../components/AppPasswordDialog'
import ProxyDialog from '../components/ProxyDialog'
import { ToastProvider } from '../components/ToastProvider'
import MailDetailDialog from '../components/inbox/MailDetailDialog'
import { server } from './server'

const accounts: AccountSummary[] = ['a', 'b'].map(id => ({
  id, name: `Account ${id}`, real_email: `${id}@example.com`, icloud_email: `${id}@icloud.com`,
  host: 'icloud.com', status: 'active', alias_total: 0, alias_active: 0,
  has_cookies: true, has_app_password: true, has_proxy: false, last_validated: '', created_at: '',
}))
function gate() {
  let resolve!: () => void
  const promise = new Promise<void>(r => { resolve = r })
  return { promise, resolve }
}
function page(element: ReactElement) {
  return render(<MemoryRouter><ToastProvider>{element}</ToastProvider></MemoryRouter>)
}
async function editAccount(index = 0) {
  await userEvent.click(screen.getAllByRole('button', { name: /更多操作/ })[index])
  await userEvent.click(screen.getByRole('button', { name: '编辑' }))
}
beforeEach(() => {
  setCSRFToken('test-csrf')
  server.use(http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })))
})

it('真实账号页刷新保留编辑草稿，在途 PATCH 始终锁定且只提交一次', async () => {
  const pending = gate()
  let gets = 0
  const bodies: unknown[] = []
  server.use(
    http.get('/api/accounts', () => { gets++; return HttpResponse.json({ success: true, data: accounts }) }),
    http.patch('/api/accounts/a', async ({ request }) => {
      bodies.push(await request.json())
      await pending.promise
      return HttpResponse.json({ success: true, data: accounts[0] })
    }),
  )
  page(<AccountsPage />)
  await screen.findByText('Account a')
  await editAccount()
  fireEvent.change(screen.getByLabelText('名称'), { target: { value: 'Unsaved draft' } })
  act(() => invalidateAccounts('b'))
  await waitFor(() => expect(gets).toBe(2))
  await screen.findByText('Account a')
  expect(screen.getByLabelText('名称')).toHaveValue('Unsaved draft')
  await userEvent.click(screen.getByRole('button', { name: '保存' }))
  await waitFor(() => expect(bodies).toHaveLength(1))
  act(() => invalidateAccounts('b'))
  await waitFor(() => expect(gets).toBe(3))
  await screen.findByText('Account a')
  expect(screen.getByRole('button', { name: '保存中…' })).toBeDisabled()
  expect(screen.getByLabelText('名称')).toBeDisabled()
  await userEvent.click(screen.getByRole('button', { name: '保存中…' }))
  fireEvent.keyDown(document, { key: 'Escape' })
  expect(screen.getByRole('dialog', { name: '编辑账号' })).toBeInTheDocument()
  await act(async () => { pending.resolve() })
  await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  expect(bodies).toEqual([{ name: 'Unsaved draft', icloud_email: 'a@icloud.com', host: 'icloud.com' }])
})

it('旧账号编辑卸载后的保存成功不关闭新的编辑，也不重置新草稿', async () => {
  const pending = gate()
  let started = false
  server.use(http.patch('/api/accounts/a', async () => {
    started = true
    await pending.promise
    return HttpResponse.json({ success: true, data: accounts[0] })
  }))
  const first = page(<AccountsPage />)
  await screen.findByText('Account a')
  await editAccount()
  await userEvent.click(screen.getByRole('button', { name: '保存' }))
  await waitFor(() => expect(started).toBe(true))
  first.unmount()
  page(<AccountsPage />)
  await screen.findByText('Account b')
  await editAccount(1)
  fireEvent.change(screen.getByLabelText('名称'), { target: { value: 'New B draft' } })
  await act(async () => { pending.resolve() })
  await waitFor(() => expect(screen.queryByRole('status', { name: '加载中' })).not.toBeInTheDocument())
  expect(screen.getByRole('dialog', { name: '编辑账号' })).toBeInTheDocument()
  expect(screen.getByLabelText('名称')).toHaveValue('New B draft')
})

const dialogCases = [
  { name: '创建别名', url: '/api/create', method: http.post, field: '标签', save: '创建',
    result: { email: 'new-test@icloud.com' },
    element: (id: string, onClose: () => void, onSaved: () => void) => <CreateAliasDialog open accountId={id} onClose={onClose} onCreated={onSaved} /> },
  { name: 'App 密码', url: '/api/accounts/:id/password', method: http.post, field: 'App 专用密码', save: '保存',
    result: accounts[0],
    element: (id: string, onClose: () => void, onSaved: () => void) => <AppPasswordDialog open accountId={id} defaultEmail={`${id}@icloud.com`} onClose={onClose} onSaved={onSaved} /> },
  { name: '代理保存', url: '/api/accounts/:id/proxy', method: http.put, field: '代理地址', save: '保存',
    result: accounts[0],
    element: (id: string, onClose: () => void, onSaved: () => void) => <ProxyDialog open accountId={id} onClose={onClose} onSaved={onSaved} /> },
]

it.each(dialogCases)('$name 保存锁定输入和所有关闭入口，失败保留草稿并允许重试', async (tc) => {
  const pending = gate()
  let posts = 0
  server.use(tc.method(tc.url, async () => {
    posts++
    if (posts === 1) {
      await pending.promise
      return HttpResponse.json({ success: false, message: 'injected failure' }, { status: 500 })
    }
    return HttpResponse.json({ success: true, data: tc.result })
  }))
  const onClose = vi.fn(), onSaved = vi.fn()
  render(tc.element('a', onClose, onSaved))
  fireEvent.change(screen.getByLabelText(tc.field), { target: { value: 'First draft' } })
  await userEvent.click(screen.getByRole('button', { name: tc.save }))
  await waitFor(() => expect(posts).toBe(1))
  expect(screen.getByLabelText(tc.field)).toBeDisabled()
  expect(screen.getByRole('button', { name: '取消' })).toBeDisabled()
  await userEvent.type(screen.getByLabelText(tc.field), 'lost draft')
  await userEvent.click(screen.getByRole('button', { name: '取消' }))
  fireEvent.keyDown(document, { key: 'Escape' })
  fireEvent.click(screen.getByRole('dialog').parentElement!)
  expect(onClose).not.toHaveBeenCalled()
  expect(screen.getByLabelText(tc.field)).toHaveValue('First draft')
  await act(async () => { pending.resolve() })
  await screen.findByText('injected failure')
  expect(screen.getByLabelText(tc.field)).toBeEnabled()
  expect(screen.getByLabelText(tc.field)).toHaveValue('First draft')
  await userEvent.click(screen.getByRole('button', { name: tc.save }))
  await waitFor(() => expect(onSaved).toHaveBeenCalledOnce())
  expect(posts).toBe(2)
})

it.each(dialogCases.flatMap(tc => [false, true].map(fail => ({ ...tc, fail }))))('$name A→B→A 隔离旧响应，旧请求失败=$fail', async (tc) => {
  const pending = gate(), completed = gate()
  let started = false
  server.use(tc.method(tc.url, async () => {
    started = true
    await pending.promise
    return tc.fail ? HttpResponse.json({ success: false, message: 'old failure' }, { status: 500 })
      : HttpResponse.json({ success: true, data: tc.result })
  }))
  const onClose = vi.fn(), onSaved = vi.fn()
  const current = render(tc.element('a', onClose, onSaved))
  fireEvent.change(screen.getByLabelText(tc.field), { target: { value: 'First draft' } })
  await userEvent.click(screen.getByRole('button', { name: tc.save }))
  await waitFor(() => expect(started).toBe(true))
  // Forced target replacement models navigation/parent lifecycle, independent of locked user controls.
  current.rerender(tc.element('b', onClose, onSaved))
  current.rerender(tc.element('a', onClose, onSaved))
  fireEvent.change(screen.getByLabelText(tc.field), { target: { value: 'New A draft' } })
  const onResponse = () => completed.resolve()
  server.events.on('response:mocked', onResponse)
  try {
    await act(async () => { pending.resolve(); await completed.promise })
    expect(screen.getByLabelText(tc.field)).toHaveValue('New A draft')
    expect(screen.queryByText('old failure')).not.toBeInTheDocument()
    expect(onSaved).not.toHaveBeenCalled()
    expect(screen.getByRole('button', { name: tc.save })).toBeEnabled()
  } finally { server.events.removeListener('response:mocked', onResponse) }
})

it.each(dialogCases)('$name 旧请求完成不能解除新会话正在保存的锁', async tc => {
  const old = gate(), current = gate(), oldCompleted = gate()
  let posts = 0
  server.use(tc.method(tc.url, async () => {
    const index = ++posts
    await (index === 1 ? old.promise : current.promise)
    return HttpResponse.json({ success: true, data: tc.result })
  }))
  const onSaved = vi.fn(), onClose = vi.fn()
  const view = render(tc.element('a', onClose, onSaved))
  fireEvent.change(screen.getByLabelText(tc.field), { target: { value: 'First draft' } })
  await userEvent.click(screen.getByRole('button', { name: tc.save }))
  await waitFor(() => expect(posts).toBe(1))
  view.rerender(tc.element('b', onClose, onSaved))
  fireEvent.change(screen.getByLabelText(tc.field), { target: { value: 'Current draft' } })
  await userEvent.click(screen.getByRole('button', { name: tc.save }))
  await waitFor(() => expect(posts).toBe(2))
  const onResponse = () => oldCompleted.resolve()
  server.events.on('response:mocked', onResponse)
  try {
    await act(async () => { old.resolve(); await oldCompleted.promise })
    expect(screen.getByLabelText(tc.field)).toBeDisabled()
    expect(screen.getByLabelText(tc.field)).toHaveValue('Current draft')
    expect(screen.getByRole('button', { name: '取消' })).toBeDisabled()
    expect(onSaved).not.toHaveBeenCalled()
  } finally {
    server.events.removeListener('response:mocked', onResponse)
    await act(async () => { current.resolve() })
  }
  await waitFor(() => expect(onSaved).toHaveBeenCalledOnce())
})

it('代理改输入取消旧检测，新检测结果只能属于新输入', async () => {
  const old = gate()
  let started = false
  const tested: string[] = []
  server.use(http.post('/api/proxy/check', async ({ request }) => {
    const { proxy } = await request.json() as { proxy: string }
    tested.push(proxy)
    if (proxy.endsWith(':8001')) { started = true; await old.promise }
    return HttpResponse.json({ success: true, data: { ok: true, latency_ms: proxy.endsWith(':8001') ? 11 : 22, message: '' } })
  }))
  render(<ProxyDialog open accountId="a" onClose={vi.fn()} onSaved={vi.fn()} />)
  fireEvent.change(screen.getByLabelText('代理地址'), { target: { value: 'http://proxy.test:8001' } })
  await userEvent.click(screen.getByRole('button', { name: '测试连接' }))
  await waitFor(() => expect(started).toBe(true))
  fireEvent.change(screen.getByLabelText('代理地址'), { target: { value: 'http://proxy.test:8002' } })
  await userEvent.click(screen.getByRole('button', { name: '测试连接' }))
  await screen.findByText(/延迟: 22ms/)
  await act(async () => { old.resolve() })
  expect(screen.queryByText(/延迟: 11ms/)).not.toBeInTheDocument()
  expect(screen.getByText(/延迟: 22ms/)).toBeInTheDocument()
  expect(tested).toEqual(['http://proxy.test:8001', 'http://proxy.test:8002'])
})

it.each(['tag', 'token'])('%s 创建期间表单锁定，同一事件批次不重复提交，失败保留草稿', async kind => {
  const pending = gate()
  const url = kind === 'tag' ? '/api/tags' : '/api/tokens'
  let posts = 0
  server.use(
    http.get('/api/tags', () => HttpResponse.json({ success: true, data: [] })),
    http.get('/api/tokens', () => HttpResponse.json({ success: true, data: [] })),
    http.post(url, async () => {
      posts++
      await pending.promise
      return HttpResponse.json({ success: false, message: 'create failure' }, { status: 500 })
    }),
  )
  page(<BusinessTagsPage />)
  const input = await screen.findByPlaceholderText(kind === 'tag' ? '标识名称 (例: tiktok)' : '令牌备注 (例: 注册机-01)')
  fireEvent.change(input, { target: { value: 'first draft' } })
  if (kind === 'tag') fireEvent.change(screen.getByPlaceholderText('描述备注'), { target: { value: 'description draft' } })
  const form = input.closest('form')!
  act(() => { fireEvent.submit(form); fireEvent.submit(form) })
  await waitFor(() => expect(posts).toBe(1))
  expect(input).toBeDisabled()
  if (kind === 'tag') {
    expect(screen.getByPlaceholderText('描述备注')).toBeDisabled()
    for (const preset of form.querySelectorAll('.tag-quick-chip')) expect(preset).toBeDisabled()
  } else {
    expect(screen.getByLabelText('令牌权限')).toBeDisabled()
    expect(screen.getByLabelText('令牌有效期')).toBeDisabled()
  }
  await userEvent.type(input, 'second draft')
  await act(async () => { pending.resolve() })
  await screen.findByText('create failure')
  expect(input).toBeEnabled()
  expect(input).toHaveValue('first draft')
  expect(posts).toBe(1)
})

it.each(['workspace', 'global'])('%s 别名说明保存后立即回显，改名称保留说明，允许明确清空', async kind => {
  let alias: Alias = { email: 'note-test@icloud.com', anonymousId: 'note-test', label: 'Old label', note: 'Existing important note', active: true, account_id: 'a' }
  const writes: unknown[] = []
  server.use(
    http.get('/api/accounts/a', () => HttpResponse.json({ success: true, data: accounts[0] })),
    http.get('/api/aliases', () => HttpResponse.json({ success: true, data: { aliases: [alias] } })),
    http.patch('/api/aliases/note-test', async ({ request }) => {
      const body = await request.json() as { label: string; note: string }
      writes.push(body)
      alias = { ...alias, ...body }
      return HttpResponse.json({ success: true, data: body })
    }),
  )
  render(<MemoryRouter initialEntries={[kind === 'workspace' ? '/workspace/a' : '/aliases']}><ToastProvider><Routes>
    <Route path="/workspace/:accountId" element={<AccountWorkspace />} />
    <Route path="/aliases" element={<AliasesPage />} />
  </Routes></ToastProvider></MemoryRouter>)
  await screen.findByText('note-test@icloud.com')
  async function openEdit() {
    await userEvent.click(screen.getByRole('button', { name: kind === 'workspace' ? '备注' : '修改备注' }))
  }
  await openEdit()
  expect(screen.getByLabelText('补充说明 (可选)')).toHaveValue('Existing important note')
  fireEvent.change(screen.getByLabelText('备注名称 (用途标签)'), { target: { value: 'Renamed' } })
  await userEvent.click(screen.getByRole('button', { name: '保存备注' }))
  await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  await openEdit()
  expect(screen.getByLabelText('补充说明 (可选)')).toHaveValue('Existing important note')
  fireEvent.change(screen.getByLabelText('补充说明 (可选)'), { target: { value: 'New note' } })
  await userEvent.click(screen.getByRole('button', { name: '保存备注' }))
  await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  await openEdit()
  expect(screen.getByLabelText('补充说明 (可选)')).toHaveValue('New note')
  fireEvent.change(screen.getByLabelText('补充说明 (可选)'), { target: { value: '' } })
  await userEvent.click(screen.getByRole('button', { name: '保存备注' }))
  await waitFor(() => expect(writes).toHaveLength(3))
  expect(writes).toEqual([
    { account_id: 'a', label: 'Renamed', note: 'Existing important note' },
    { account_id: 'a', label: 'Renamed', note: 'New note' },
    { account_id: 'a', label: 'Renamed', note: '' },
  ])
})

// These three fields came from the audit's actual Go MIME decoding fixtures.
it.each([
  { subject: 'Verification code', preview: '482019', body: '<p>482019</p>' },
  { subject: 'Login', preview: 'Your code is 482019.', body: '<head><title>Your code is 123456</title></head><p>Your code is 482019.</p>' },
  { subject: 'Login', preview: 'Your code is 482019.', body: '<p>Your code is &#52;&#56;&#50;&#48;&#49;&#57;.</p>' },
])('真实 MIME 字段在邮件详情中保留验证码和可读正文：$body', fixture => {
  const detail: FullMessage = { ...fixture, id: 'fixture', from: 'sender@example.com', to: 'to@example.com', date: '', content_type: 'text/html', body_complete: true }
  render(<MailDetailDialog detail={detail} onClose={vi.fn()} />)
  expect(screen.getByRole('dialog').querySelector('.email-code-value')).toHaveTextContent('482019')
  expect(within(screen.getByRole('dialog')).getByRole('button', { name: '复制验证码' })).toBeInTheDocument()
  expect(screen.queryByText(/&#52;/)).not.toBeInTheDocument()
})
