import { http, HttpResponse } from 'msw'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { beforeEach, describe, expect, it } from 'vitest'
import AliasesPage from './AliasesPage'
import { server } from '../test/server'
import { setCSRFToken } from '../api/client'
import { ToastProvider } from '../components/ToastProvider'
import { clearAccountsCache } from '../hooks/useAccounts'
import type { AccountSummary, Alias } from '../api/types'

function createDeferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

const accounts: AccountSummary[] = [
  {
    id: 'acc_1',
    name: '主号',
    real_email: 'a@example.com',
    icloud_email: 'a@icloud.com',
    host: 'icloud.com',
    status: 'active',
    alias_total: 2,
    alias_active: 1,
    has_cookies: true,
    has_app_password: false,
    has_proxy: false,
    last_validated: '2026-08-04T09:00:00+08:00',
    created_at: '2026-08-01T09:00:00+08:00',
  },
  {
    id: 'acc_2',
    name: '备用号',
    real_email: 'b@example.com',
    icloud_email: 'b@icloud.com',
    host: 'icloud.com',
    status: 'active',
    alias_total: 1,
    alias_active: 1,
    has_cookies: true,
    has_app_password: false,
    has_proxy: false,
    last_validated: '2026-08-04T09:00:00+08:00',
    created_at: '2026-08-01T09:00:00+08:00',
  },
]

const aliases: Alias[] = [
  {
    email: 'alpha@icloud.com',
    anonymousId: 'anon_alpha',
    label: 'Alpha',
    active: true,
    createdAt: '2026-07-01T00:00:00+08:00',
  },
  {
    email: 'beta@icloud.com',
    anonymousId: 'anon_beta',
    label: 'Beta',
    active: false,
    createdAt: '2026-07-02T00:00:00+08:00',
  },
]

function renderPage(initialPath = '/aliases') {
  return render(
    <MemoryRouter initialEntries={[initialPath]}>
      <Routes>
        <Route
          path="/aliases"
          element={
            <ToastProvider>
              <AliasesPage />
            </ToastProvider>
          }
        />
      </Routes>
    </MemoryRouter>,
  )
}

describe('AliasesPage', () => {
  beforeEach(() => {
    clearAccountsCache()
    setCSRFToken('csrf-test')
    server.resetHandlers()
  })

  it('无账号时显示引导', async () => {
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: [] })),
    )
    renderPage()
    expect(await screen.findByText(/暂无账号/)).toBeInTheDocument()
  })

  it('账号切换:URL query 优先,回退到第一个账号', async () => {
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/aliases', ({ request }) => {
        const url = new URL(request.url)
        const id = url.searchParams.get('account_id')
        return HttpResponse.json({
          success: true,
          data: {
            account_id: id,
            count: id === 'acc_2' ? 1 : 2,
            aliases: id === 'acc_2' ? [aliases[0]] : aliases,
          },
        })
      }),
    )
    const { unmount } = renderPage('/aliases?account_id=acc_2')
    expect(await screen.findByText('alpha@icloud.com')).toBeInTheDocument()
    expect(screen.queryByText('beta@icloud.com')).toBeNull()
    unmount()
    renderPage('/aliases?account_id=bad_id')
    // 回退到第一个账号 acc_1,显示 2 个别名
    expect(await screen.findByText('beta@icloud.com')).toBeInTheDocument()
  })

  it('loading/empty/error/retry 状态', async () => {
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/aliases', () =>
        HttpResponse.json(
          { success: false, code: 'UPSTREAM_FAILURE', message: '获取别名列表失败' },
          { status: 502 },
        ),
      ),
    )
    renderPage()
    const retry = await screen.findByRole('button', { name: /重试/ })
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/aliases', () =>
        HttpResponse.json({ success: true, data: { account_id: 'acc_1', count: 0, aliases: [] } }),
      ),
    )
    await userEvent.click(retry)
    expect(await screen.findByText(/暂无别名/)).toBeInTheDocument()
  })

  it('按 email/label 大小写不敏感搜索与 active 状态筛选', async () => {
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/aliases', () =>
        HttpResponse.json({ success: true, data: { account_id: 'acc_1', count: 2, aliases } }),
      ),
    )
    renderPage()
    expect(await screen.findByText('alpha@icloud.com')).toBeInTheDocument()
    const user = userEvent.setup()
    await user.type(screen.getByLabelText(/搜索/), 'ALPHA')
    expect(screen.getByText('alpha@icloud.com')).toBeInTheDocument()
    expect(screen.queryByText('beta@icloud.com')).toBeNull()
    await user.clear(screen.getByLabelText(/搜索/))
    await user.selectOptions(screen.getByLabelText(/状态/), 'active')
    expect(screen.getByText('alpha@icloud.com')).toBeInTheDocument()
    expect(screen.queryByText('beta@icloud.com')).toBeNull()
  })

  it('创建时间默认倒序且可切换正序,兼容时间戳并按收件箱格式显示', async () => {
    const timestampAliases: Alias[] = [
      { ...aliases[0], createdAt: '1787406420000' },
      { ...aliases[1], createdAt: '1787406360000' },
    ]
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/aliases', () =>
        HttpResponse.json({
          success: true,
          data: { account_id: 'acc_1', count: 2, aliases: timestampAliases },
        }),
      ),
    )
    renderPage()
    await screen.findByText('alpha@icloud.com')
    const table = screen.getByRole('table')
    const emailButtons = () => within(table).getAllByRole('button', { name: /@icloud\.com/ })
    expect(emailButtons().map((button) => button.textContent)).toEqual([
      'alpha@icloud.com',
      'beta@icloud.com',
    ])
    expect(screen.getAllByText(/^\d{4}\/\d{2}\/\d{2} \d{2}:\d{2}$/)).toHaveLength(2)

    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: /创建时间排序/ }))
    expect(emailButtons().map((button) => button.textContent)).toEqual([
      'beta@icloud.com',
      'alpha@icloud.com',
    ])

    const alphaRow = within(table).getByRole('row', { name: /alpha@icloud\.com/ })
    expect(within(alphaRow).getByRole('link', { name: /收件箱/ })).toHaveAttribute(
      'href',
      '/inbox?account_id=acc_1&alias=alpha%40icloud.com',
    )
  })

  it('点击刷新号池重新拉取最新别名列表', async () => {
    let refreshed = false
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/aliases', () =>
        HttpResponse.json({
          success: true,
          data: {
            account_id: 'acc_1',
            count: refreshed ? 3 : 2,
            aliases: refreshed
              ? [...aliases, { email: 'gamma@icloud.com', anonymousId: 'anon_gamma', label: 'Gamma', active: true }]
              : aliases,
          },
        }),
      ),
    )
    renderPage()
    await screen.findByText('alpha@icloud.com')
    refreshed = true
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: /刷新号池/ }))
    expect(await screen.findByText('gamma@icloud.com')).toBeInTheDocument()
  })

  it('停用别名:显示目标邮箱并二次确认', async () => {
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/aliases', () =>
        HttpResponse.json({ success: true, data: { account_id: 'acc_1', count: 2, aliases } }),
      ),
      http.post('/api/aliases/:id/deactivate', () =>
        HttpResponse.json({ success: true, data: { anonymous_id: 'anon_alpha', success: true } }),
      ),
    )
    renderPage()
    await screen.findByText('alpha@icloud.com')
    const user = userEvent.setup()
    await user.click(screen.getAllByRole('button', { name: /停用/ })[0])
    expect(screen.getByRole('dialog')).toHaveTextContent('alpha@icloud.com')
    await user.click(screen.getByRole('button', { name: /确认停用/ }))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  })

  it('激活别名:显示目标邮箱并二次确认', async () => {
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/aliases', () =>
        HttpResponse.json({ success: true, data: { account_id: 'acc_1', count: 2, aliases } }),
      ),
      http.post('/api/aliases/:id/reactivate', () =>
        HttpResponse.json({ success: true, data: { anonymous_id: 'anon_beta', success: true } }),
      ),
    )
    renderPage()
    await screen.findByText('beta@icloud.com')
    const user = userEvent.setup()
    await user.click(screen.getAllByRole('button', { name: /激活/ })[0])
    expect(screen.getByRole('dialog')).toHaveTextContent('beta@icloud.com')
    await user.click(screen.getByRole('button', { name: /确认激活/ }))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  })

  it('删除别名:要求输入完整邮箱,用 anonymousId 构造 URL 并编码', async () => {
    let deletedUrl = ''
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/aliases', () =>
        HttpResponse.json({ success: true, data: { account_id: 'acc_1', count: 2, aliases } }),
      ),
      http.delete('/api/aliases/:id', ({ request }) => {
        deletedUrl = request.url
        return HttpResponse.json({ success: true, data: { anonymous_id: 'anon_alpha' } })
      }),
    )
    renderPage()
    await screen.findByText('alpha@icloud.com')
    const user = userEvent.setup()
    const alphaRow = within(screen.getByRole('table')).getByRole('row', { name: /alpha@icloud\.com/ })
    await user.click(within(alphaRow).getByRole('button', { name: /删除/ }))
    expect(screen.getByRole('dialog')).toHaveTextContent('alpha@icloud.com')
    // 输入不完整邮箱时按钮禁用
    await user.type(screen.getByLabelText(/输入完整邮箱/), 'alpha@icloud')
    expect(screen.getByRole('button', { name: /确认删除/ })).toBeDisabled()
    await user.type(screen.getByLabelText(/输入完整邮箱/), '.com')
    await user.click(screen.getByRole('button', { name: /确认删除/ }))
    await waitFor(() => expect(deletedUrl).toContain('anon_alpha'))
  })

  it('操作失败保留列表并显示错误', async () => {
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/aliases', () =>
        HttpResponse.json({ success: true, data: { account_id: 'acc_1', count: 2, aliases } }),
      ),
      http.post('/api/aliases/:id/deactivate', () =>
        HttpResponse.json(
          { success: false, code: 'UPSTREAM_FAILURE', message: '停用失败' },
          { status: 502 },
        ),
      ),
    )
    renderPage()
    await screen.findByText('alpha@icloud.com')
    const user = userEvent.setup()
    await user.click(screen.getAllByRole('button', { name: /停用/ })[0])
    await user.click(screen.getByRole('button', { name: /确认停用/ }))
    expect(await screen.findByRole('alert')).toHaveTextContent('停用失败')
    expect(screen.getByText('alpha@icloud.com')).toBeInTheDocument()
  })

  it('支持切换每页条数(20/50/100)并记住偏好', async () => {
    localStorage.clear()
    const manyAliases: Alias[] = Array.from({ length: 45 }, (_, i) => ({
      email: `test_${i + 1}@icloud.com`,
      anonymousId: `anon_${i + 1}`,
      label: `Alias ${i + 1}`,
      active: true,
      createdAt: new Date(1787406000000 + i * 1000).toISOString(),
    }))

    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/aliases', () =>
        HttpResponse.json({
          success: true,
          data: { account_id: 'acc_1', count: 45, aliases: manyAliases },
        }),
      ),
    )
    renderPage()
    await screen.findByText('test_45@icloud.com')

    // 默认每页 20 条，共 3 页
    expect(screen.getByText(/共/)).toHaveTextContent('共 45 个别名，当前第 1 / 3 页')

    const user = userEvent.setup()
    const sizeSelect = screen.getByLabelText('每页显示条数')
    await user.selectOptions(sizeSelect, '50')

    // 切换到 50 条后，45 条在第 1 页内全部展示，共 1 页
    expect(screen.getByText(/共/)).toHaveTextContent('共 45 个别名，当前第 1 / 1 页')
    expect(localStorage.getItem('icloud_hme_alias_page_size')).toBe('50')
  })

  it('TestAliasesPage_StaleRefreshCannotOverwriteNewAccountSelection: 账号 A 刷新慢请求返回绝不覆盖已切换的账号 B', async () => {
    const user = userEvent.setup()
    const deferredRefreshA = createDeferred<{ account_id: string; count: number; aliases: Alias[] }>()

    const aliasA: Alias = {
      email: 'a-refresh@icloud.com',
      anonymousId: 'anon_a_refresh',
      label: 'A专属别名',
      active: true,
      account_id: 'acc_1',
    }

    const aliasB: Alias = {
      email: 'b-alias@icloud.com',
      anonymousId: 'anon_b',
      label: 'B专属别名',
      active: true,
      account_id: 'acc_2',
    }

    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/aliases', async ({ request }) => {
        const url = new URL(request.url)
        const id = url.searchParams.get('account_id')
        const refresh = url.searchParams.get('refresh') === 'true'

        // 当 acc_1 手动点击刷新时，挂起该请求
        if (id === 'acc_1' && refresh) {
          const data = await deferredRefreshA.promise
          return HttpResponse.json({ success: true, data })
        }
        if (id === 'acc_2') {
          return HttpResponse.json({
            success: true,
            data: { account_id: 'acc_2', count: 1, aliases: [aliasB] },
          })
        }
        return HttpResponse.json({
          success: true,
          data: { account_id: 'acc_1', count: 1, aliases: [aliases[0]] },
        })
      }),
    )

    // 1. 打开页面，默认定位到 acc_1
    renderPage('/aliases?account_id=acc_1')
    expect(await screen.findByText('alpha@icloud.com')).toBeInTheDocument()

    // 2. 点击 "刷新号池" (触发 acc_1 的慢刷新)
    const refreshBtn = screen.getByRole('button', { name: /刷新号池/i })
    await user.click(refreshBtn)

    // 3. 用户在下拉框快速切换到 acc_2
    await user.selectOptions(screen.getByLabelText(/所属账号/), 'acc_2')

    // 4. 等待 acc_2 的数据展示出来
    expect(await screen.findByText('b-alias@icloud.com')).toBeInTheDocument()

    // 5. 放行 acc_1 的慢刷新响应
    deferredRefreshA.resolve({
      account_id: 'acc_1',
      count: 1,
      aliases: [aliasA],
    })

    // 6. 验证: 页面必须继续保持 acc_2 的别名，acc_1 的晚返回结果被丢弃
    await new Promise((r) => setTimeout(r, 50))
    expect(screen.getByText('b-alias@icloud.com')).toBeInTheDocument()
    expect(screen.queryByText('a-refresh@icloud.com')).toBeNull()
  })
})
