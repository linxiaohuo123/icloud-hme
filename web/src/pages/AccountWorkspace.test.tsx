import { http, HttpResponse } from 'msw'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useNavigate, MemoryRouter, Route, Routes } from 'react-router-dom'
import { beforeEach, describe, expect, it } from 'vitest'
import AccountWorkspace from './AccountWorkspace'
import { server } from '../test/server'
import { setCSRFToken } from '../api/client'
import { ToastProvider } from '../components/ToastProvider'
import type { AccountSummary, Alias, InboxResult } from '../api/types'

function createDeferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

function WorkspaceNavHelper() {
  const navigate = useNavigate()
  return (
    <div>
      <button onClick={() => navigate('/workspace/acc_a')}>Go Acc A</button>
      <button onClick={() => navigate('/workspace/acc_b')}>Go Acc B</button>
    </div>
  )
}

function renderWorkspaceWithNav(initialUrl = '/workspace/acc_a') {
  return render(
    <ToastProvider>
      <MemoryRouter initialEntries={[initialUrl]}>
        <WorkspaceNavHelper />
        <Routes>
          <Route path="/workspace/:accountId" element={<AccountWorkspace />} />
        </Routes>
      </MemoryRouter>
    </ToastProvider>,
  )
}

const testAccount: AccountSummary = {
  id: 'acc_test',
  name: '测试账号',
  real_email: 'test@example.com',
  icloud_email: 'test@icloud.com',
  host: 'icloud.com',
  status: 'active',
  alias_total: 2,
  alias_active: 1,
  has_cookies: true,
  has_app_password: true,
  has_proxy: false,
  last_validated: '2026-08-04T09:00:00+08:00',
  created_at: '2026-08-01T09:00:00+08:00',
}

const testAliases: Alias[] = [
  {
    email: 'work-1@icloud.com',
    anonymousId: 'anon_work_1',
    label: '工作',
    active: true,
    createdAt: '2026-07-01T00:00:00+08:00',
  },
  {
    email: 'shop-2@icloud.com',
    anonymousId: 'anon_shop_2',
    label: '购物',
    active: false,
    createdAt: '2026-07-02T00:00:00+08:00',
  },
]

const testInbox: InboxResult = {
  account_id: 'acc_test',
  method: 'web_api',
  count: 1,
  messages: [
    {
      id: 'msg_1',
      from: 'Apple <no-reply@apple.com>',
      to: 'work-1@icloud.com',
      subject: '您的验证码是 654321',
      preview: '请使用此验证码完成登录: 654321',
      date: '2026-08-04T10:00:00+08:00',
    },
  ],
}

function renderWorkspace(initialUrl = '/workspace/acc_test') {
  return render(
    <ToastProvider>
      <MemoryRouter initialEntries={[initialUrl]}>
        <Routes>
          <Route path="/workspace/:accountId" element={<AccountWorkspace />} />
        </Routes>
      </MemoryRouter>
    </ToastProvider>,
  )
}

describe('AccountWorkspace', () => {
  beforeEach(() => {
    setCSRFToken('csrf_test_token')
    server.use(
      http.get('/api/accounts', () => {
        return HttpResponse.json({ success: true, data: [testAccount] })
      }),
      http.get('/api/accounts/:id', () => {
        return HttpResponse.json({ success: true, data: testAccount })
      }),
      http.get('/api/aliases', ({ request }) => {
        const url = new URL(request.url)
        expect(url.searchParams.get('account_id')).toBe('acc_test')
        return HttpResponse.json({ success: true, data: { aliases: testAliases } })
      }),
      http.get('/api/inbox', ({ request }) => {
        const url = new URL(request.url)
        expect(url.searchParams.get('account_id')).toBe('acc_test')
        return HttpResponse.json({ success: true, data: testInbox })
      }),
    )
  })

  it('渲染账号头部、指标卡片与别名列表', async () => {
    renderWorkspace()

    // 账号身份卡
    await waitFor(() => {
      expect(screen.getByRole('heading', { name: '测试账号' })).toBeInTheDocument()
    })
    expect(screen.getByText('test@icloud.com')).toBeInTheDocument()

    // 指标卡
    expect(screen.getByText('活跃别名')).toBeInTheDocument()
    expect(screen.getByText('已建别名')).toBeInTheDocument()

    // 别名表格
    await waitFor(() => {
      expect(screen.getByText('work-1@icloud.com')).toBeInTheDocument()
      expect(screen.getByText('shop-2@icloud.com')).toBeInTheDocument()
    })
  })

  it('切换到收件箱 Tab 并展示邮件及验证码提取徽章', async () => {
    const user = userEvent.setup()
    renderWorkspace()

    await waitFor(() => {
      expect(screen.getByText('work-1@icloud.com')).toBeInTheDocument()
    })

    // 点击收件箱 Tab
    const inboxTab = screen.getByRole('button', { name: /收件箱与验证码/i })
    await user.click(inboxTab)

    // 收件箱邮件加载并展示
    await waitFor(() => {
      expect(screen.getByText('您的验证码是 654321')).toBeInTheDocument()
    })

    // 验证码一键复制徽章
    expect(screen.getByText('654321')).toBeInTheDocument()
  })

  it('按关键字搜索与活跃状态筛选别名', async () => {
    const user = userEvent.setup()
    renderWorkspace()

    await waitFor(() => {
      expect(screen.getByText('work-1@icloud.com')).toBeInTheDocument()
    })

    // 搜索 "购物"
    const searchInput = screen.getByPlaceholderText(/搜索别名邮箱或标签备注/i)
    await user.type(searchInput, '购物')

    expect(screen.queryByText('work-1@icloud.com')).not.toBeInTheDocument()
    expect(screen.getByText('shop-2@icloud.com')).toBeInTheDocument()

    // 清空搜索
    await user.clear(searchInput)
    expect(screen.getByText('work-1@icloud.com')).toBeInTheDocument()

    // 点击 "仅活跃" 过滤
    const activeFilterBtn = screen.getByRole('button', { name: /仅活跃/i })
    await user.click(activeFilterBtn)

    expect(screen.getByText('work-1@icloud.com')).toBeInTheDocument()
    expect(screen.queryByText('shop-2@icloud.com')).not.toBeInTheDocument()
  })

  it('TestAccountWorkspace_SlowAccountACannotOverwriteAccountB: 账号 A 请求挂起切到 B，A 慢返回绝不可覆盖 B', async () => {
    const user = userEvent.setup()
    const deferredAccountA = createDeferred<AccountSummary>()

    const accountA: AccountSummary = {
      ...testAccount,
      id: 'acc_a',
      name: '账号-Alpha',
      real_email: 'alpha@example.com',
      icloud_email: 'alpha@icloud.com',
    }

    const accountB: AccountSummary = {
      ...testAccount,
      id: 'acc_b',
      name: '账号-Beta',
      real_email: 'beta@example.com',
      icloud_email: 'beta@icloud.com',
    }

    server.use(
      http.get('/api/accounts/acc_a', async () => {
        const data = await deferredAccountA.promise
        return HttpResponse.json({ success: true, data })
      }),
      http.get('/api/accounts/acc_b', () => {
        return HttpResponse.json({ success: true, data: accountB })
      }),
      http.get('/api/aliases', ({ request }) => {
        const url = new URL(request.url)
        const accId = url.searchParams.get('account_id')
        if (accId === 'acc_b') {
          return HttpResponse.json({
            success: true,
            data: {
              aliases: [
                {
                  email: 'beta-alias@icloud.com',
                  anonymousId: 'anon_b',
                  label: 'Beta别名',
                  active: true,
                },
              ],
            },
          })
        }
        return HttpResponse.json({ success: true, data: { aliases: [] } })
      }),
    )

    // 1. 渲染 Workspace，初始路由为 /workspace/acc_a (Account A 请求挂起)
    renderWorkspaceWithNav('/workspace/acc_a')

    // 2. 路由快速切换到 /workspace/acc_b
    await user.click(screen.getByText('Go Acc B'))

    // 3. 等待 Account B 渲染成功
    await waitFor(() => {
      expect(screen.getByRole('heading', { name: '账号-Beta' })).toBeInTheDocument()
      expect(screen.getByText('beta@icloud.com')).toBeInTheDocument()
      expect(screen.getByText('beta-alias@icloud.com')).toBeInTheDocument()
    })

    // 4. 放行 Account A 的慢响应
    deferredAccountA.resolve(accountA)

    // 5. 验证: 页面必须继续保持账号 B，绝不可被 A 覆盖
    await new Promise((r) => setTimeout(r, 50))
    expect(screen.getByRole('heading', { name: '账号-Beta' })).toBeInTheDocument()
    expect(screen.getByText('beta@icloud.com')).toBeInTheDocument()
    expect(screen.queryByText('账号-Alpha')).not.toBeInTheDocument()
    expect(screen.queryByText('alpha@icloud.com')).not.toBeInTheDocument()
  })

  it('TestAccountWorkspace_SlowAliasACannotOverwriteAliasesB: 账号 A 别名慢请求返回绝不覆盖账号 B 别名', async () => {
    const user = userEvent.setup()
    const deferredAliasA = createDeferred<Alias[]>()

    const accountA: AccountSummary = {
      ...testAccount,
      id: 'acc_a',
      name: '账号-Alpha',
    }

    const accountB: AccountSummary = {
      ...testAccount,
      id: 'acc_b',
      name: '账号-Beta',
    }

    server.use(
      http.get('/api/accounts/acc_a', () => {
        return HttpResponse.json({ success: true, data: accountA })
      }),
      http.get('/api/accounts/acc_b', () => {
        return HttpResponse.json({ success: true, data: accountB })
      }),
      http.get('/api/aliases', async ({ request }) => {
        const url = new URL(request.url)
        const accId = url.searchParams.get('account_id')
        if (accId === 'acc_a') {
          const list = await deferredAliasA.promise
          return HttpResponse.json({ success: true, data: { aliases: list } })
        }
        if (accId === 'acc_b') {
          return HttpResponse.json({
            success: true,
            data: {
              aliases: [
                {
                  email: 'b-unique@icloud.com',
                  anonymousId: 'anon_b_unique',
                  label: 'B专属',
                  active: true,
                },
              ],
            },
          })
        }
        return HttpResponse.json({ success: true, data: { aliases: [] } })
      }),
    )

    // 1. 渲染 /workspace/acc_a，账号 A 别名请求挂起
    renderWorkspaceWithNav('/workspace/acc_a')
    await waitFor(() => {
      expect(screen.getByRole('heading', { name: '账号-Alpha' })).toBeInTheDocument()
    })

    // 2. 切换到 /workspace/acc_b
    await user.click(screen.getByText('Go Acc B'))

    // 3. 等待 B 的别名展示
    await waitFor(() => {
      expect(screen.getByText('b-unique@icloud.com')).toBeInTheDocument()
    })

    // 4. 放行 A 的慢别名数据
    deferredAliasA.resolve([
      {
        email: 'a-stale@icloud.com',
        anonymousId: 'anon_a_stale',
        label: 'A过期别名',
        active: true,
      },
    ])

    // 5. 验证: 页面必须继续只有 B 的别名，A 绝不写入
    await new Promise((r) => setTimeout(r, 50))
    expect(screen.getByText('b-unique@icloud.com')).toBeInTheDocument()
    expect(screen.queryByText('a-stale@icloud.com')).not.toBeInTheDocument()
  })
})
