import { http, HttpResponse } from 'msw'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { beforeEach, describe, expect, it } from 'vitest'
import AccountWorkspace from './AccountWorkspace'
import { server } from '../test/server'
import { setCSRFToken } from '../api/client'
import { ToastProvider } from '../components/ToastProvider'
import type { AccountSummary, Alias, InboxResult } from '../api/types'

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
})
