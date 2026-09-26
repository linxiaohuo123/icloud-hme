/**
 * [INPUT]: 依赖 @testing-library/react, vitest, msw, react-router-dom, components/inbox/InboxTableView
 * [OUTPUT]: 对外提供 InboxTableView 跨账号并发防污染、详情缓存隔离与 WebMail 首屏 capability 防竞争及退避重试单元测试
 * [POS]: web/src/components/inbox 的单元测试防线
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { http, HttpResponse, delay } from 'msw'
import { render, screen, waitFor, fireEvent, act, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import InboxTableView from './InboxTableView'
import { server } from '../../test/server'
import { ToastProvider } from '../ToastProvider'
import type { AccountSummary, InboxResult } from '../../api/types'

const mockAccounts: AccountSummary[] = [
  {
    id: 'acc_1',
    name: '账号1',
    real_email: 'acc1@example.com',
    icloud_email: 'acc1@icloud.com',
    host: 'icloud.com',
    status: 'active',
    alias_total: 1,
    alias_active: 1,
    has_cookies: true,
    has_app_password: true,
    has_proxy: false,
    last_validated: '2026-09-20T00:00:00Z',
    created_at: '2026-09-20T00:00:00Z',
  },
  {
    id: 'acc_2',
    name: '账号2',
    real_email: 'acc2@example.com',
    icloud_email: 'acc2@icloud.com',
    host: 'icloud.com',
    status: 'active',
    alias_total: 1,
    alias_active: 1,
    has_cookies: true,
    has_app_password: true,
    has_proxy: false,
    last_validated: '2026-09-20T00:00:00Z',
    created_at: '2026-09-20T00:00:00Z',
  },
]

describe('InboxTableView 跨账号防污染与缓存隔离 (PR-02)', () => {
  it('当 propAccountId 变更时隔离旧账户缓存并不污染新账户视图', async () => {
    server.use(
      http.get('/api/accounts', () => {
        return HttpResponse.json({ success: true, data: mockAccounts })
      }),
      http.get('/api/aliases', () => {
        return HttpResponse.json({ success: true, data: [] })
      }),
      http.get('/api/mailboxes', () => {
        return HttpResponse.json({ success: true, data: { folders: [] } })
      }),
      http.get('/api/inbox', ({ request }) => {
        const url = new URL(request.url)
        const accId = url.searchParams.get('account_id')
        const res: InboxResult = {
          account_id: accId || '',
          count: 1,
          method: 'imap',
          messages: [
            {
              id: accId === 'acc_1' ? '101' : '202',
              from: `${accId}@example.com`,
              to: 'to@icloud.com',
              subject: accId === 'acc_1' ? '账号1的专属邮件' : '账号2的专属邮件',
              date: '2026-09-20T10:00:00Z',
              preview: '预览内容',
            },
          ],
        }
        return HttpResponse.json({ success: true, data: res })
      }),
    )

    const { rerender } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_1" fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('账号1的专属邮件')).toBeInTheDocument()
    })

    // 切换外部 propAccountId 到 acc_2
    rerender(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_2" fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('账号2的专属邮件')).toBeInTheDocument()
    })
    expect(screen.queryByText('账号1的专属邮件')).not.toBeInTheDocument()
  })

  it('旧账户在途邮件详情响应不会写入新账户视图 (Generation 防污染保护)', async () => {
    server.use(
      http.get('/api/accounts', () => {
        return HttpResponse.json({ success: true, data: mockAccounts })
      }),
      http.get('/api/aliases', () => {
        return HttpResponse.json({ success: true, data: [] })
      }),
      http.get('/api/mailboxes', () => {
        return HttpResponse.json({ success: true, data: { folders: [] } })
      }),
      http.get('/api/inbox', ({ request }) => {
        const url = new URL(request.url)
        const accId = url.searchParams.get('account_id')
        return HttpResponse.json({
          success: true,
          data: {
            account_id: accId || '',
            count: 1,
            method: 'imap',
            messages: [
              {
                id: '42',
                from: 'sender@example.com',
                to: 'to@icloud.com',
                subject: '测试邮件 42',
                date: '2026-09-20T10:00:00Z',
                preview: '点击查看详情',
              },
            ],
          },
        })
      }),
      http.get('/api/inbox/:id', async ({ request }) => {
        const url = new URL(request.url)
        const accId = url.searchParams.get('account_id')
        // 慢速延迟模拟在途网络请求
        if (accId === 'acc_1') {
          await delay(300)
          return HttpResponse.json({
            success: true,
            data: {
              account_id: 'acc_1',
              message: {
                id: '42',
                from: 'sender@example.com',
                to: 'to@icloud.com',
                subject: '账号1延迟正文',
                date: '2026-09-20T10:00:00Z',
                body: '这是账号1的私密内容，严禁泄漏给账号2',
                content_type: 'text/plain',
                body_complete: true,
              },
              provider: 'imap',
              method: 'imap',
              cached: false,
            },
          })
        }
        return HttpResponse.json({
          success: true,
          data: {
            account_id: 'acc_2',
            message: {
              id: '42',
              from: 'sender@example.com',
              to: 'to@icloud.com',
              subject: '账号2正常邮件',
              date: '2026-09-20T10:00:00Z',
              body: '这是账号2的内容',
              content_type: 'text/plain',
              body_complete: true,
            },
            provider: 'imap',
            method: 'imap',
            cached: false,
          },
        })
      }),
    )

    const { rerender } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_1" fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('测试邮件 42')).toBeInTheDocument()
    })

    // 点击行以触发 acc_1 的慢速详情请求
    await act(async () => {
      fireEvent.click(screen.getByText('测试邮件 42'))
    })

    // 在 acc_1 详情尚未返回前，立即切换 propAccountId 到 acc_2
    await act(async () => {
      rerender(
        <MemoryRouter>
          <ToastProvider>
            <InboxTableView accountId="acc_2" fixedAccount={true} />
          </ToastProvider>
        </MemoryRouter>,
      )
    })

    // 等待 400ms，确保 acc_1 的慢请求已解析完成
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 400))
    })

    // 验证账号1的私密内容绝对不会在弹窗中展示出来
    expect(screen.queryByText('这是账号1的私密内容，严禁泄漏给账号2')).not.toBeInTheDocument()
  })
})

describe('InboxTableView WebMail 首屏 Capability 防竞争与退避重试 (PR-08 P0-8)', () => {
  const webmailAccount: AccountSummary = {
    id: 'acc_webmail',
    name: '网页版账号',
    real_email: 'webmail@example.com',
    icloud_email: 'webmail@icloud.com',
    host: 'icloud.com',
    status: 'active',
    alias_total: 1,
    alias_active: 1,
    has_cookies: true,
    has_app_password: false,
    has_proxy: false,
    last_validated: '2026-09-20T00:00:00Z',
    created_at: '2026-09-20T00:00:00Z',
  }

  it('WEBMAIL-01: 固定账号为 WebMail 模式时，等待能力就绪后发起请求且绝不携带 folder/days', async () => {
    const recordedUrls: string[] = []
    server.use(
      http.get('/api/accounts', async () => {
        await delay(50)
        return HttpResponse.json({ success: true, data: [webmailAccount] })
      }),
      http.get('/api/aliases', () => HttpResponse.json({ success: true, data: [] })),
      http.get('/api/mailboxes', () => HttpResponse.json({ success: true, data: { folders: [] } })),
      http.get('/api/inbox', ({ request }) => {
        recordedUrls.push(request.url)
        return HttpResponse.json({
          success: true,
          data: {
            account_id: 'acc_webmail',
            count: 1,
            method: 'web_api',
            messages: [
              {
                id: 'w1',
                from: 'apple@icloud.com',
                to: 'webmail@icloud.com',
                subject: 'WebMail 欢迎邮件',
                date: '2026-09-20T10:00:00Z',
                preview: '欢迎使用',
              },
            ],
          },
        })
      }),
    )

    render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_webmail" fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('WebMail 欢迎邮件')).toBeInTheDocument()
    })

    expect(recordedUrls.length).toBeGreaterThanOrEqual(1)
    for (const rawUrl of recordedUrls) {
      const url = new URL(rawUrl)
      expect(url.searchParams.get('folder')).toBeNull()
      expect(url.searchParams.get('days')).toBeNull()
      expect(url.searchParams.get('account_id')).toBe('acc_webmail')
    }
  })

  it('WEBMAIL-03: 捕获 CAPABILITY_UNSUPPORTED 后最多退避重试 1 次，剥离 folder/days 后成功渲染', async () => {
    let callCount = 0
    const queryParamsList: URLSearchParams[] = []
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: mockAccounts })),
      http.get('/api/aliases', () => HttpResponse.json({ success: true, data: [] })),
      http.get('/api/mailboxes', () => HttpResponse.json({ success: true, data: { folders: [] } })),
      http.get('/api/inbox', ({ request }) => {
        callCount++
        const url = new URL(request.url)
        queryParamsList.push(url.searchParams)
        // 第一次调用带有 days，模拟后端返回 CAPABILITY_UNSUPPORTED
        if (callCount === 1) {
          return HttpResponse.json(
            { success: false, code: 'CAPABILITY_UNSUPPORTED', message: 'WebMail 模式不支持指定文件夹或按天数筛选' },
            { status: 400 },
          )
        }
        // 第二次重试（剥离 folder/days）返回成功
        return HttpResponse.json({
          success: true,
          data: {
            account_id: 'acc_1',
            count: 1,
            method: 'web_api',
            messages: [
              {
                id: 'retry_ok',
                from: 'service@icloud.com',
                to: 'acc1@icloud.com',
                subject: '重试成功邮件',
                date: '2026-09-20T12:00:00Z',
                preview: '已自动退避并重试成功',
              },
            ],
          },
        })
      }),
    )

    render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_1" fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('重试成功邮件')).toBeInTheDocument()
    })

    expect(callCount).toBe(2)
    // 第一次带 days
    expect(queryParamsList[0].get('days')).toBe('7')
    // 第二次剥离了 days
    expect(queryParamsList[1].get('days')).toBeNull()
    expect(queryParamsList[1].get('folder')).toBeNull()
  })

  it('WEBMAIL-04: 重试仍失败时停止重试，严禁陷入死循环并安全展示错误', async () => {
    let callCount = 0
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: mockAccounts })),
      http.get('/api/aliases', () => HttpResponse.json({ success: true, data: [] })),
      http.get('/api/mailboxes', () => HttpResponse.json({ success: true, data: { folders: [] } })),
      http.get('/api/inbox', () => {
        callCount++
        return HttpResponse.json(
          { success: false, code: 'CAPABILITY_UNSUPPORTED', message: 'WebMail 模式持续不支持' },
          { status: 400 },
        )
      }),
    )

    render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_1" fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('WebMail 模式持续不支持')).toBeInTheDocument()
    })

    // 初始 1 次 + 最多重试 1 次 = 2 次，严禁无限循环
    expect(callCount).toBe(2)
  })

  it('PR-MAIL-04: 打开收件箱不自动拉取正文；点击邮件按需拉取单封正文并缓存，再次点击命中缓存', async () => {
    let detailRequestCount = 0
    let batchMessagesCount = 0

    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: mockAccounts })),
      http.get('/api/aliases', () => HttpResponse.json({ success: true, data: [] })),
      http.get('/api/mailboxes', () => HttpResponse.json({ success: true, data: { folders: [] } })),
      http.get('/api/inbox', () => {
        return HttpResponse.json({
          success: true,
          data: {
            account_id: 'acc_1',
            count: 1,
            method: 'imap',
            messages: [
              {
                id: '99',
                message_ref: 'ref_v1_test_99',
                folder: 'INBOX',
                uid: 99,
                from: 'OpenAI <noreply@tm.openai.com>',
                to: 'alias@icloud.com',
                subject: 'ChatGPT 临时登录验证',
                date: '2026-09-20T10:00:00Z',
                preview: '',
              },
            ],
          },
        })
      }),
      http.post('/api/messages', () => {
        batchMessagesCount++
        return HttpResponse.json({ success: true, data: { messages: [] } })
      }),
      http.get('/api/inbox/:ref', () => {
        detailRequestCount++
        return HttpResponse.json({
          success: true,
          data: {
            message: {
              id: '99',
              message_ref: 'ref_v1_test_99',
              folder: 'INBOX',
              uid: 99,
              from: 'OpenAI <noreply@tm.openai.com>',
              to: 'alias@icloud.com',
              subject: 'ChatGPT 临时登录验证',
              date: '2026-09-20T10:00:00Z',
              preview: '다음 임시 인증 코드를 입력해 계속하세요: 576932',
              body: '<p>다음 임시 인증 코드를 입력해 계속하세요: <strong>576932</strong></p>',
              content_type: 'text/html',
              body_complete: true,
            },
          },
        })
      }),
    )

    render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_1" fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    // 1. 初始首屏呈现信封元数据，且严禁自动发起 /api/messages 批量正文
    await waitFor(() => {
      expect(screen.getByText('ChatGPT 临时登录验证')).toBeInTheDocument()
    })
    expect(batchMessagesCount).toBe(0)
    expect(detailRequestCount).toBe(0)

    // 2. 用户点击单封邮件，触发按需拉取单封正文
    fireEvent.click(screen.getByRole('button', { name: 'ChatGPT 临时登录验证' }))

    await waitFor(() => {
      expect(detailRequestCount).toBe(1)
      expect(screen.getByText('576932')).toBeInTheDocument()
    })

    // 3. 关闭详情弹窗后，再次点击同一封邮件：直接命中缓存，detailRequestCount 保持为 1
    const dialog = screen.getByRole('dialog')
    const closeBtn = within(dialog).getByRole('button', { name: '关闭' })
    fireEvent.click(closeBtn)

    fireEvent.click(screen.getByRole('button', { name: 'ChatGPT 临时登录验证' }))
    expect(detailRequestCount).toBe(1)
  })
})

describe('InboxTableView 详情生命周期彻底收口 (FIX-1)', () => {
  it('Case A: A detail pending → close dialog → request abort, loading=false, dialog disappears, late response 不得重新打开', async () => {
    let detailResolve: (() => void) | null = null
    server.use(
      http.get('/api/inbox', () => {
        return HttpResponse.json({
          success: true,
          data: {
            account_id: 'acc_1',
            count: 1,
            method: 'imap',
            messages: [
              {
                id: '101',
                message_ref: 'ref_101',
                from: 'Sender <sender@example.com>',
                to: 'to@icloud.com',
                subject: '慢速邮件A',
                date: '2026-09-20T10:00:00Z',
                preview: '预览A',
              },
            ],
          },
        })
      }),
      http.get('/api/inbox/:ref', async () => {
        await new Promise<void>((resolve) => {
          detailResolve = resolve
        })
        return HttpResponse.json({
          success: true,
          data: {
            message: {
              id: '101',
              message_ref: 'ref_101',
              from: 'Sender <sender@example.com>',
              to: 'to@icloud.com',
              subject: '慢速邮件A',
              date: '2026-09-20T10:00:00Z',
              preview: '预览A',
              body: '<p>正文内容A</p>',
              content_type: 'text/html',
              body_complete: true,
            },
          },
        })
      }),
    )

    render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_1" fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('慢速邮件A')).toBeInTheDocument()
    })

    // 1. 点击邮件触发 loading
    fireEvent.click(screen.getByRole('button', { name: '慢速邮件A' }))

    await waitFor(() => {
      expect(screen.getByText('读取邮件正文中…')).toBeInTheDocument()
    })

    // 2. 在 pending 状态下直接关闭弹窗 (点击遮罩层或触发关闭)
    const backdrop = document.querySelector('.dialog-backdrop')
    expect(backdrop).not.toBeNull()
    fireEvent.click(backdrop!)

    // 3. 弹窗应当立即消失
    expect(screen.queryByRole('dialog')).toBeNull()

    // 4. 模拟慢速响应晚到返回
    await act(async () => {
      detailResolve?.()
      await delay(50)
    })

    // 5. late response 严禁重新打开弹窗
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(screen.queryByText('正文内容A')).toBeNull()
  })

  it('Case B: A detail 已显示 → click uncached B → A 内容立即消失，只显示 loading', async () => {
    let resolveB!: () => void
    const promiseB = new Promise<void>((resolve) => {
      resolveB = resolve
    })
    server.use(
      http.get('/api/inbox', () => {
        return HttpResponse.json({
          success: true,
          data: {
            account_id: 'acc_1',
            count: 2,
            method: 'imap',
            messages: [
              {
                id: '201',
                message_ref: 'ref_201',
                from: 'Sender A <senderA@example.com>',
                to: 'to@icloud.com',
                subject: '已缓存邮件A',
                date: '2026-09-20T10:00:00Z',
                preview: '预览A',
              },
              {
                id: '202',
                message_ref: 'ref_202',
                from: 'Sender B <senderB@example.com>',
                to: 'to@icloud.com',
                subject: '未缓存邮件B',
                date: '2026-09-20T11:00:00Z',
                preview: '预览B',
              },
            ],
          },
        })
      }),
      http.get('/api/inbox/:ref', async ({ params }) => {
        if (params.ref === 'ref_201') {
          return HttpResponse.json({
            success: true,
            data: {
              message: {
                id: '201',
                message_ref: 'ref_201',
                from: 'Sender A <senderA@example.com>',
                to: 'to@icloud.com',
                subject: '已缓存邮件A',
                date: '2026-09-20T10:00:00Z',
                preview: '预览A',
                body: '<p>我是邮件A的完整正文</p>',
                content_type: 'text/html',
                body_complete: true,
              },
            },
          })
        }
        await promiseB
        return HttpResponse.json({
          success: true,
          data: {
            message: {
              id: '202',
              message_ref: 'ref_202',
              from: 'Sender B <senderB@example.com>',
              to: 'to@icloud.com',
              subject: '未缓存邮件B',
              date: '2026-09-20T11:00:00Z',
              preview: '预览B',
              body: '<p>我是邮件B的完整正文</p>',
              content_type: 'text/html',
              body_complete: true,
            },
          },
        })
      }),
    )

    render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_1" fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('已缓存邮件A')).toBeInTheDocument()
      expect(screen.getByText('未缓存邮件B')).toBeInTheDocument()
    })

    // 1. 点击邮件 A 并等待其正文显示
    fireEvent.click(screen.getByRole('button', { name: '已缓存邮件A' }))
    await waitFor(() => {
      expect(screen.getByText('我是邮件A的完整正文')).toBeInTheDocument()
    })

    // 2. 点击邮件 B (此时 B 在途 pending)
    fireEvent.click(screen.getByRole('button', { name: '未缓存邮件B' }))

    // 3. 邮件 A 的正文必须立即消失，只显示 loading
    expect(screen.queryByText('我是邮件A的完整正文')).toBeNull()
    expect(screen.getByText('读取邮件正文中…')).toBeInTheDocument()

    // 4. 释放邮件 B 响应
    await act(async () => {
      resolveB?.()
      await delay(50)
    })

    // 5. 显示邮件 B 正文
    await waitFor(() => {
      expect(screen.getByText('我是邮件B的完整正文')).toBeInTheDocument()
    })
  })

  it('Case C: detail pending → switch account → detail=null, loading=false, old account response 不得污染', async () => {
    let resolveDetail: (() => void) | null = null
    server.use(
      http.get('/api/accounts', () => {
        return HttpResponse.json({ success: true, data: mockAccounts })
      }),
      http.get('/api/inbox', ({ request }) => {
        const url = new URL(request.url)
        const accId = url.searchParams.get('account_id')
        return HttpResponse.json({
          success: true,
          data: {
            account_id: accId || '',
            count: 1,
            method: 'imap',
            messages: [
              {
                id: accId === 'acc_1' ? '101' : '202',
                message_ref: accId === 'acc_1' ? 'ref_101' : 'ref_202',
                from: `${accId}@example.com`,
                to: 'to@icloud.com',
                subject: accId === 'acc_1' ? '账户1邮件' : '账户2邮件',
                date: '2026-09-20T10:00:00Z',
                preview: '预览',
              },
            ],
          },
        })
      }),
      http.get('/api/inbox/:ref', async () => {
        await new Promise<void>((resolve) => {
          resolveDetail = resolve
        })
        return HttpResponse.json({
          success: true,
          data: {
            message: {
              id: '101',
              message_ref: 'ref_101',
              from: 'acc_1@example.com',
              to: 'to@icloud.com',
              subject: '账户1邮件',
              date: '2026-09-20T10:00:00Z',
              preview: '预览',
              body: '<p>账户1正文</p>',
              content_type: 'text/html',
              body_complete: true,
            },
          },
        })
      }),
    )

    const { rerender } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_1" fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('账户1邮件')).toBeInTheDocument()
    })

    // 1. 点击账户1邮件进入 pending
    fireEvent.click(screen.getByRole('button', { name: '账户1邮件' }))
    await waitFor(() => {
      expect(screen.getByText('读取邮件正文中…')).toBeInTheDocument()
    })

    // 2. 外部切换账户为 acc_2
    rerender(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_2" fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    // 3. 弹窗立即关闭，loading 归零
    await waitFor(() => {
      expect(screen.getByText('账户2邮件')).toBeInTheDocument()
    })
    expect(screen.queryByRole('dialog')).toBeNull()

    // 4. 旧账户请求完成返回
    await act(async () => {
      resolveDetail?.()
      await delay(50)
    })

    // 5. 严禁旧请求回写并污染当前视图
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(screen.queryByText('账户1正文')).toBeNull()
  })
})

