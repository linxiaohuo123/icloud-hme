/**
 * [INPUT]: 依赖 React Testing Library、Vitest、MSW、工作台、收件箱、邮件详情、批量生成与业务标识页面
 * [OUTPUT]: 复制成功/失败与失败后重试的交互回归测试，验证提示和按钮反馈遵循剪贴板实际结果
 * [POS]: web/src/test 的跨组件剪贴板回归防线；接口写入由 MSW 拦截，剪贴板返回值独立模拟
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { beforeEach, expect, it, vi } from 'vitest'
import { server } from './server'
import { copyText } from '../utils/clipboard'
import { ToastProvider } from '../components/ToastProvider'
import InboxTableView, { clearInboxSnapshotCache } from '../components/inbox/InboxTableView'
import MailDetailDialog from '../components/inbox/MailDetailDialog'
import BatchCreateAliasDialog from '../components/BatchCreateAliasDialog'
import AccountWorkspace from '../pages/AccountWorkspace'
import BusinessTagsPage from '../pages/BusinessTagsPage'
import type { AccountSummary } from '../api/types'

vi.mock('../utils/clipboard', () => ({ copyText: vi.fn() }))
const copy = vi.mocked(copyText)
const account: AccountSummary = {
  id: 'copy_account', name: '复制测试账号', real_email: 'copy@example.com', icloud_email: 'copy@icloud.com',
  host: 'icloud.com', status: 'active', has_app_password: true, has_cookies: true, has_proxy: false,
  alias_total: 1, alias_active: 1, created_at: '', last_validated: '',
}
const message = {
  id: '7', message_ref: 'INBOX:300:7', subject: '复制测试邮件', preview: 'Your verification code is 876543',
  from: 'sender@example.com', to: 'copy-alias@icloud.com', date: '2026-10-01T00:00:00Z', folder: 'INBOX',
}

beforeEach(() => {
  clearInboxSnapshotCache()
  copy.mockReset().mockResolvedValue(false)
  server.use(
    http.get('/api/accounts', () => HttpResponse.json({ success: true, data: [account] })),
    http.get('/api/accounts/:id', () => HttpResponse.json({ success: true, data: account })),
    http.get('/api/aliases', () => HttpResponse.json({ success: true, data: { aliases: [
      { anonymousId: 'copy_alias', email: message.to, label: '', active: true, createdAt: '' },
    ] } })),
    http.get('/api/inbox', () => HttpResponse.json({ success: true, data: {
      account_id: account.id, count: 1, method: 'imap', messages: [message],
    } })),
  )
})

it.each([false, true])('工作台别名与账号邮箱反馈遵循复制结果 (success=%s)', async (ok) => {
  copy.mockResolvedValue(ok)
  render(<MemoryRouter initialEntries={['/workspace/copy_account']}>
    <ToastProvider><Routes><Route path="/workspace/:accountId" element={<AccountWorkspace />} /></Routes></ToastProvider>
  </MemoryRouter>)
  const row = (await screen.findByText(message.to)).closest('tr')!
  const aliasButton = within(row).getByRole('button', { name: '复制邮箱' })
  fireEvent.click(aliasButton)
  await screen.findByText(ok ? '别名邮箱已复制' : '复制失败，请手动复制邮箱')
  expect(aliasButton).toHaveAttribute('title', ok ? '已复制到剪贴板！' : '复制邮箱')
  expect(copy).toHaveBeenLastCalledWith(message.to)

  const accountButton = screen.getByTitle(`点击复制账号邮箱 (${account.icloud_email})`)
  fireEvent.click(accountButton)
  await screen.findByText(ok ? `已复制账号邮箱: ${account.icloud_email}` : '复制失败，请手动复制账号邮箱')
  expect(accountButton).toHaveAttribute('title', ok ? '已复制到剪贴板！' : `点击复制账号邮箱 (${account.icloud_email})`)
  expect(copy).toHaveBeenLastCalledWith(account.icloud_email)
  if (!ok) expect(screen.queryByTitle('已复制到剪贴板！')).not.toBeInTheDocument()
})

it('剪贴板尚未完成时工作台不提前显示复制成功', async () => {
  let release!: (ok: boolean) => void
  const pending = new Promise<boolean>(resolve => { release = resolve })
  copy.mockReturnValue(pending)
  render(<MemoryRouter initialEntries={['/workspace/copy_account']}>
    <ToastProvider><Routes><Route path="/workspace/:accountId" element={<AccountWorkspace />} /></Routes></ToastProvider>
  </MemoryRouter>)
  await screen.findByText(message.to)
  const user = userEvent.setup()
  try {
    await user.click(screen.getByRole('button', { name: '复制邮箱' }))
    await user.click(screen.getByTitle(`点击复制账号邮箱 (${account.icloud_email})`))
    expect(copy).toHaveBeenCalledTimes(2)
    expect(screen.queryByTitle('已复制到剪贴板！')).not.toBeInTheDocument()
    expect(screen.queryByText('别名邮箱已复制')).not.toBeInTheDocument()
    expect(screen.queryByText(`已复制账号邮箱: ${account.icloud_email}`)).not.toBeInTheDocument()
    release(false)
    await screen.findByText('复制失败，请手动复制邮箱')
    await screen.findByText('复制失败，请手动复制账号邮箱')
  } finally {
    release(false)
  }
})

it.each([false, true])('收件列表验证码与别名反馈遵循复制结果 (success=%s)', async (ok) => {
  copy.mockResolvedValue(ok)
  render(<MemoryRouter><ToastProvider><InboxTableView accountId={account.id} accountSummary={account} fixedAccount /></ToastProvider></MemoryRouter>)
  const codeButton = await screen.findByRole('button', { name: /876543/ })
  fireEvent.click(codeButton)
  await screen.findByText(ok ? '验证码 [876543] 已复制' : '复制失败，请手动复制验证码：876543')
  expect(codeButton).toHaveTextContent(ok ? '已复制' : '复制')
  if (!ok) expect(codeButton).not.toHaveTextContent('已复制')
  expect(copy).toHaveBeenLastCalledWith('876543')

  const aliasButton = screen.getByTitle('复制收件别名')
  fireEvent.click(aliasButton)
  await screen.findByText(ok ? `别名 [${message.to}] 已复制` : '复制失败，请手动复制收件别名')
  expect(copy).toHaveBeenLastCalledWith(message.to)
  expect(aliasButton.querySelector('svg')?.style.color).toBe(ok ? 'var(--color-success)' : '')
  expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
})

it.each([false, true])('邮件详情验证码与收件地址反馈遵循复制结果 (success=%s)', async (ok) => {
  copy.mockResolvedValue(ok)
  const notify = vi.fn()
  render(<MailDetailDialog detail={{ ...message, body: message.preview, content_type: 'text/plain', body_complete: true }} onClose={() => {}} onCopySuccess={notify} />)
  fireEvent.click(screen.getByRole('button', { name: '复制验证码' }))
  await waitFor(() => expect(notify).toHaveBeenLastCalledWith(ok ? '验证码 [876543] 已复制' : '复制失败，请手动复制验证码：876543'))
  expect(screen.getByRole('button', { name: ok ? '已复制' : '复制验证码' })).toBeInTheDocument()
  if (!ok) expect(screen.queryByRole('button', { name: '已复制' })).not.toBeInTheDocument()
  expect(copy).toHaveBeenLastCalledWith('876543')
  const aliasButton = screen.getByTitle('复制收件别名')
  fireEvent.click(aliasButton)
  await waitFor(() => expect(notify).toHaveBeenLastCalledWith(ok ? `别名 [${message.to}] 已复制` : '复制失败，请手动复制收件别名'))
  expect(copy).toHaveBeenLastCalledWith(message.to)
  expect(aliasButton.querySelector('svg')?.style.color).toBe(ok ? 'var(--color-success)' : '')
})

it('批量结果复制失败时保留邮箱列表，重试成功后清除错误并显示成功', async () => {
  server.use(http.post('/api/create/batch', () => HttpResponse.json({ success: true, data: {
    account_id: account.id, requested: 2, created_count: 2,
    created: [{ email: message.to, anonymousId: 'copy_alias' }, { email: 'second@icloud.com', anonymousId: 'second' }], failed: [],
  } })))
  render(<BatchCreateAliasDialog open accounts={[account]} onClose={() => {}} onSuccess={() => {}} />)
  fireEvent.click(screen.getByRole('button', { name: '2 个' }))
  fireEvent.click(screen.getByRole('button', { name: '生成 2 个别名' }))
  await screen.findByText('second@icloud.com')
  fireEvent.click(screen.getByRole('button', { name: '一键复制全部' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('复制失败，请手动复制邮箱列表')
  expect(screen.queryByRole('button', { name: '已复制' })).not.toBeInTheDocument()
  expect(screen.getByText(message.to)).toBeInTheDocument()
  expect(copy).toHaveBeenLastCalledWith(`${message.to}\nsecond@icloud.com`)
  copy.mockResolvedValue(true)
  fireEvent.click(screen.getByRole('button', { name: '一键复制全部' }))
  await screen.findByRole('button', { name: '已复制' })
  expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  expect(screen.getByText('second@icloud.com')).toBeInTheDocument()
})

it.each([false, true])('令牌、完整接入命令和两种示例按真实复制结果提示 (success=%s)', async (ok) => {
  copy.mockResolvedValue(ok)
  server.use(
    http.get('/api/tags', () => HttpResponse.json({ success: true, data: [
      { id: 'tag_copy', tag: 'test', name: 'test', description: '', status: 'active', created_at: '' },
    ] })),
    http.get('/api/tokens', () => HttpResponse.json({ success: true, data: [] })),
    http.post('/api/tokens', () => HttpResponse.json({ success: true, data: {
      id: 'token_copy', name: '测试令牌', token: 'mock-only-copy-token', created_at: '',
    } })),
  )
  const descriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'scrollIntoView')
  Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', { configurable: true, value: vi.fn() })
  try {
    render(<MemoryRouter><ToastProvider><BusinessTagsPage /></ToastProvider></MemoryRouter>)
    const user = userEvent.setup()
    await user.type(await screen.findByPlaceholderText('令牌备注 (例: 注册机-01)'), '测试令牌')
    await user.click(screen.getByRole('button', { name: '生成令牌' }))
    await screen.findByText('mock-only-copy-token')
    await user.click(screen.getByRole('button', { name: '复制令牌' }))
    await screen.findByText(ok ? '令牌已复制到剪贴板' : '复制失败，请手动复制令牌')
    expect(copy).toHaveBeenLastCalledWith('mock-only-copy-token')
    await user.click(screen.getByRole('button', { name: '复制完整接入命令' }))
    await screen.findByText(ok ? '完整接入命令已复制' : '复制失败，请手动复制接入命令')
    expect(copy).toHaveBeenLastCalledWith(expect.stringContaining('mock-only-copy-token'))
    await user.click(screen.getByRole('button', { name: '复制 cURL 命令' }))
    await screen.findByText(ok ? 'cURL 示例命令已复制' : '复制失败，请手动复制接入示例')
    await user.click(screen.getByRole('button', { name: 'Python (完整流水线)' }))
    await user.click(screen.getByRole('button', { name: '复制 Python 脚本' }))
    await screen.findByText(ok ? 'Python 自动化脚本已复制' : '复制失败，请手动复制接入示例')
    expect(copy).toHaveBeenCalledTimes(4)
    if (!ok) expect(screen.queryByText(/已复制/)).not.toBeInTheDocument()
  } finally {
    if (descriptor) Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', descriptor)
    else Reflect.deleteProperty(HTMLElement.prototype, 'scrollIntoView')
  }
})
