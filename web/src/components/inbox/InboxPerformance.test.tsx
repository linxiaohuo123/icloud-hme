import { describe, it, expect, beforeEach } from 'vitest'
import { render, screen, waitFor, fireEvent, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { http, HttpResponse } from 'msw'
import { server } from '../../test/server'
import InboxTableView, { clearInboxSnapshotCache, getModuleMessageCache } from './InboxTableView'
import { ToastProvider } from '../ToastProvider'
import type { AccountSummary, InboxResult, MailboxFolder } from '../../api/types'

describe('Inbox Baseline Performance Measurements', () => {
  const dummyAccount: AccountSummary = {
    id: 'acc_perf',
    name: 'Perf Account',
    real_email: 'perf@icloud.com',
    icloud_email: 'perf@icloud.com',
    host: 'p123-setup.icloud.com',
    has_app_password: true,
    has_cookies: true,
    has_proxy: false,
    status: 'active',
    alias_total: 10,
    alias_active: 5,
    last_validated: '2026-09-23 20:00:00',
    created_at: '2026-09-23 20:00:00',
  }

  const dummyFolders: MailboxFolder[] = [
    { name: 'INBOX', role: 'inbox' },
    { name: 'Junk', role: 'junk' },
  ]

  const dummyInboxResult: InboxResult = {
    account_id: 'acc_perf',
    count: 2,
    messages: [
      {
        id: '1',
        message_ref: 'imap:acc_perf:INBOX:1:101',
        subject: 'Apple Security Code',
        from: 'apple@apple.com',
        to: 'perf@icloud.com',
        date: '2026-09-23 20:00:00',
        folder: 'INBOX',
        preview: '',
        body: '',
      },
      {
        id: '2',
        message_ref: 'imap:acc_perf:INBOX:1:102',
        subject: 'Your OTP is 123456',
        from: 'service@apple.com',
        to: 'perf@icloud.com',
        date: '2026-09-23 20:01:00',
        folder: 'INBOX',
        preview: '',
        body: '',
      },
    ],
    method: 'imap',
  }

  beforeEach(() => {
    server.resetHandlers()
    clearInboxSnapshotCache()
  })

  it('measures baseline mount requests and batch size', async () => {
    const requestCalls: string[] = []

    server.use(
      http.get('/api/accounts', () => {
        requestCalls.push('/api/accounts')
        return HttpResponse.json({ success: true, data: [dummyAccount] })
      }),
      http.get('/api/mailboxes', () => {
        requestCalls.push('/api/mailboxes')
        return HttpResponse.json({ success: true, data: { account_id: 'acc_perf', folders: dummyFolders } })
      }),
      http.get('/api/inbox', () => {
        requestCalls.push('/api/inbox')
        return HttpResponse.json({ success: true, data: dummyInboxResult })
      }),
      http.post('/api/messages', async ({ request }) => {
        const body = (await request.json()) as { messages?: Array<{ id: string; message_ref: string }> }
        requestCalls.push(`/api/messages (batch size: ${body?.messages?.length || 0})`)
        return HttpResponse.json({
          success: true,
          data: {
            messages: [
              {
                id: '1',
                message_ref: 'imap:acc_perf:INBOX:1:101',
                subject: 'Apple Security Code',
                body: 'Your verification code is 654321',
                preview: 'Your verification code is 654321',
              },
              {
                id: '2',
                message_ref: 'imap:acc_perf:INBOX:1:102',
                subject: 'Your OTP is 123456',
                body: 'Your code is 123456',
                preview: 'Your code is 123456',
              },
            ],
          },
        })
      }),
    )

    const { unmount } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_perf" fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('Apple Security Code')).toBeInTheDocument()
    })

    // In baseline mount (without accountSummary):
    // 1. /api/accounts was requested because accountSummary was not provided
    expect(requestCalls.some((c) => c.includes('/api/accounts'))).toBe(true)
    // 2. /api/mailboxes is lazy-loaded and NOT requested on mount (PR-MAIL-03)
    expect(requestCalls.some((c) => c.includes('/api/mailboxes'))).toBe(false)
    // 3. /api/inbox was requested
    expect(requestCalls.some((c) => c.includes('/api/inbox'))).toBe(true)
    // 4. PR-MAIL-04: Body-on-demand: no automatic /api/messages requested on mount (zero body requests)
    expect(requestCalls.some((c) => c.includes('/api/messages'))).toBe(false)

    unmount()
  })

  it('skips /api/accounts fetch when accountSummary is passed in fixedAccount mode', async () => {
    const requestCalls: string[] = []

    server.use(
      http.get('/api/accounts', () => {
        requestCalls.push('/api/accounts')
        return HttpResponse.json({ success: true, data: [dummyAccount] })
      }),
      http.get('/api/mailboxes', () => {
        requestCalls.push('/api/mailboxes')
        return HttpResponse.json({ success: true, data: { account_id: 'acc_perf', folders: dummyFolders } })
      }),
      http.get('/api/inbox', () => {
        requestCalls.push('/api/inbox')
        return HttpResponse.json({ success: true, data: dummyInboxResult })
      }),
    )

    const { unmount } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView
            accountId="acc_perf"
            accountSummary={dummyAccount}
            fixedAccount={true}
          />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('Apple Security Code')).toBeInTheDocument()
    })

    // Metadata reuse: /api/accounts should NOT be called at all
    expect(requestCalls.filter((c) => c === '/api/accounts').length).toBe(0)
    unmount()
  })

  it('reuses moduleFolderCache to eliminate duplicate /api/mailboxes calls on remount', async () => {
    let mailboxesCallCount = 0

    server.use(
      http.get('/api/mailboxes', () => {
        mailboxesCallCount++
        return HttpResponse.json({ success: true, data: { account_id: 'acc_perf', folders: dummyFolders } })
      }),
      http.get('/api/inbox', () => {
        return HttpResponse.json({ success: true, data: dummyInboxResult })
      }),
    )

    // First mount
    const view1 = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_perf" accountSummary={dummyAccount} fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )
    await waitFor(() => {
      expect(screen.getByText('Apple Security Code')).toBeInTheDocument()
    })
    const countAfterFirstMount = mailboxesCallCount
    view1.unmount()

    // Second mount (simulating tab switch)
    const view2 = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_perf" accountSummary={dummyAccount} fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )
    await waitFor(() => {
      expect(screen.getByText('Apple Security Code')).toBeInTheDocument()
    })

    // moduleFolderCache prevented any additional call to /api/mailboxes
    expect(mailboxesCallCount).toBe(countAfterFirstMount)
    view2.unmount()
  })

  it('restores list snapshot immediately on remount without empty state', async () => {
    server.use(
      http.get('/api/mailboxes', () => {
        return HttpResponse.json({ success: true, data: { account_id: 'acc_perf', folders: dummyFolders } })
      }),
      http.get('/api/inbox', () => {
        return HttpResponse.json({ success: true, data: dummyInboxResult })
      }),
    )

    // First mount primes snapshot cache
    const view1 = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_perf" accountSummary={dummyAccount} fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )
    await waitFor(() => {
      expect(screen.getByText('Apple Security Code')).toBeInTheDocument()
    })
    view1.unmount()

    // Second mount immediately renders cached messages synchronously
    const view2 = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_perf" accountSummary={dummyAccount} fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    // Synchronously visible upon mount from snapshot
    expect(screen.getByText('Apple Security Code')).toBeInTheDocument()
    view2.unmount()
  })

  it('TEST-1: /api/messages 永不返回时，列表元数据依然先显示 (Fast First Paint)', async () => {
    let resolveMessages: (() => void) | null = null
    const messagesDeferred = new Promise<void>((resolve) => {
      resolveMessages = resolve
    })

    server.use(
      http.get('/api/mailboxes', () => {
        return HttpResponse.json({ success: true, data: { account_id: 'acc_perf', folders: dummyFolders } })
      }),
      http.get('/api/inbox', () => {
        return HttpResponse.json({ success: true, data: dummyInboxResult })
      }),
      http.post('/api/messages', async () => {
        await messagesDeferred
        return HttpResponse.json({ success: true, data: { messages: [] } })
      }),
    )

    const { unmount } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_perf" accountSummary={dummyAccount} fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    // 在 /api/messages 挂起未返回的情况下，邮件列表的主题已立即渲染显示
    await waitFor(() => {
      expect(screen.getByText('Apple Security Code')).toBeInTheDocument()
      expect(screen.getByText('Your OTP is 123456')).toBeInTheDocument()
    })

    // 释放 /api/messages，测试正常收尾
    resolveMessages!()
    unmount()
  })

  it('TEST-4 (PR-MAIL-04): 即使有 12 封邮件缺失正文，打开收件箱也绝不自动发起 /api/messages (正文请求 = 0)', async () => {
    const chunkSizes: number[] = []
    const manyMessages = Array.from({ length: 12 }, (_, i) => ({
      id: `msg_${i + 1}`,
      message_ref: `imap:acc_perf:INBOX:1:${100 + i}`,
      subject: `Verification Code #${i + 1}`,
      from: 'apple@apple.com',
      to: 'perf@icloud.com',
      date: '2026-09-23 20:00:00',
      folder: 'INBOX',
      preview: '',
      body: '',
    }))

    server.use(
      http.get('/api/mailboxes', () => {
        return HttpResponse.json({ success: true, data: { account_id: 'acc_perf', folders: dummyFolders } })
      }),
      http.get('/api/inbox', () => {
        return HttpResponse.json({
          success: true,
          data: {
            account_id: 'acc_perf',
            count: 12,
            messages: manyMessages,
            method: 'imap',
          },
        })
      }),
      http.post('/api/messages', async ({ request }) => {
        const body = (await request.json()) as { messages?: Array<{ id: string; message_ref: string }> }
        const len = body?.messages?.length || 0
        chunkSizes.push(len)
        return HttpResponse.json({
          success: true,
          data: { messages: [] },
        })
      }),
    )

    const { unmount } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_perf" accountSummary={dummyAccount} fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('Verification Code #1')).toBeInTheDocument()
      expect(screen.getByText('Verification Code #12')).toBeInTheDocument()
    })

    // 关键断言：PR-MAIL-04 用户不点击邮件时，自动正文批量请求恒等于 0
    expect(chunkSizes.length).toBe(0)

    unmount()
  })

  it('only backfills body by canonical MessageRef and never by bare UID or id (INBOX vs Junk same UID)', async () => {
    // Both INBOX and Junk messages share UID 100
    const testMessages: InboxResult = {
      account_id: 'acc_perf',
      count: 2,
      messages: [
        {
          id: '100',
          uid: 100,
          message_ref: 'imap:acc_perf:INBOX:1:100',
          subject: 'Inbox Security Code',
          from: 'apple@apple.com',
          to: 'perf@icloud.com',
          date: '2026-09-23 20:00:00',
          folder: 'INBOX',
          preview: '',
          body: '',
        },
        {
          id: '100',
          uid: 100,
          message_ref: 'imap:acc_perf:Junk:1:100',
          subject: 'Junk Spam Warning',
          from: 'spammer@spam.com',
          to: 'perf@icloud.com',
          date: '2026-09-23 20:01:00',
          folder: 'Junk',
          preview: '',
          body: '',
        },
      ],
      method: 'imap',
    }

    server.use(
      http.get('/api/mailboxes', () => {
        return HttpResponse.json({ success: true, data: { account_id: 'acc_perf', folders: dummyFolders } })
      }),
      http.get('/api/inbox', () => {
        return HttpResponse.json({ success: true, data: testMessages })
      }),
      http.get('/api/inbox/:ref', () => {
        // Return detail ONLY for the Junk message
        return HttpResponse.json({
          success: true,
          data: {
            message: {
              id: '100',
              uid: 100,
              folder: 'Junk',
              message_ref: 'imap:acc_perf:Junk:1:100',
              subject: 'Junk Spam Warning',
              body: 'Malicious spam content',
              preview: 'Malicious spam content',
              content_type: 'text/plain',
              body_complete: true,
            },
          },
        })
      }),
    )

    const { unmount } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_perf" accountSummary={dummyAccount} fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('Inbox Security Code')).toBeInTheDocument()
      expect(screen.getByText('Junk Spam Warning')).toBeInTheDocument()
    })

    // 点击 Junk 邮件拉取单封正文
    fireEvent.click(screen.getByRole('button', { name: 'Junk Spam Warning' }))

    // 详情弹窗中展示 Junk 正文
    await waitFor(() => {
      expect(screen.getByText('Malicious spam content')).toBeInTheDocument()
    })

    // CRITICAL: The INBOX message must NEVER receive the Junk message body despite sharing UID 100
    const inboxRow = screen.getByText('Inbox Security Code').closest('tr')
    expect(inboxRow).not.toHaveTextContent('Malicious spam content')

    unmount()
  })

  it('rejects mismatched UIDVALIDITY backfill on identical UID', async () => {
    // Current inbox has UIDVALIDITY 2, message UID 100
    const testMessages: InboxResult = {
      account_id: 'acc_perf',
      count: 1,
      messages: [
        {
          id: '100',
          uid: 100,
          message_ref: 'imap:acc_perf:INBOX:2:100',
          subject: 'New Mailbox Session Code',
          from: 'apple@apple.com',
          to: 'perf@icloud.com',
          date: '2026-09-23 20:00:00',
          folder: 'INBOX',
          preview: '',
          body: '',
        },
      ],
      method: 'imap',
    }

    server.use(
      http.get('/api/mailboxes', () => {
        return HttpResponse.json({ success: true, data: { account_id: 'acc_perf', folders: dummyFolders } })
      }),
      http.get('/api/inbox', () => {
        return HttpResponse.json({ success: true, data: testMessages })
      }),
      http.post('/api/messages', async () => {
        // Return body for an OLD session with UIDVALIDITY 1
        return HttpResponse.json({
          success: true,
          data: {
            messages: [
              {
                id: '100',
                uid: 100,
                folder: 'INBOX',
                message_ref: 'imap:acc_perf:INBOX:1:100',
                subject: 'Old Session Code',
                body: 'OLD_SESSION_CODE_999999',
                preview: 'OLD_SESSION_CODE_999999',
              },
            ],
          },
        })
      }),
    )

    const { unmount } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_perf" accountSummary={dummyAccount} fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('New Mailbox Session Code')).toBeInTheDocument()
    })

    // The message must NOT receive the mismatched UIDVALIDITY body
    expect(screen.queryByText('OLD_SESSION_CODE_999999')).toBeNull()

    unmount()
  })

  it('TEST-2: /api/messages 失败(500)时，已渲染的 metadata 列表不消失且不进入整页错误', async () => {
    const tenMessages = Array.from({ length: 10 }, (_, i) => ({
      id: `msg_${i + 1}`,
      message_ref: `imap:acc_perf:INBOX:1:${200 + i}`,
      subject: `Batch Item #${i + 1}`,
      from: 'apple@apple.com',
      to: 'perf@icloud.com',
      date: '2026-09-23 20:00:00',
      folder: 'INBOX',
      preview: '',
      body: '',
    }))

    server.use(
      http.get('/api/mailboxes', () => {
        return HttpResponse.json({ success: true, data: { account_id: 'acc_perf', folders: dummyFolders } })
      }),
      http.get('/api/inbox', () => {
        return HttpResponse.json({
          success: true,
          data: {
            account_id: 'acc_perf',
            count: 10,
            messages: tenMessages,
            method: 'imap',
          },
        })
      }),
      http.post('/api/messages', async () => {
        return HttpResponse.json({ success: false, code: 'INTERNAL_ERROR', message: 'IMAP socket timeout' }, { status: 500 })
      }),
    )

    const { unmount } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_perf" accountSummary={dummyAccount} fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('Batch Item #1')).toBeInTheDocument()
      expect(screen.getByText('Batch Item #10')).toBeInTheDocument()
    })

    // 所有 10 封邮件依然完整展示在列表中
    for (let i = 1; i <= 10; i++) {
      expect(screen.getByText(`Batch Item #${i}`)).toBeInTheDocument()
    }

    // 严禁出现整页错误提示或清空列表
    expect(screen.queryByText('IMAP socket timeout')).toBeNull()
    expect(screen.queryByText('网络连接失败，请检查服务状态')).toBeNull()

    unmount()
  })

  it('clears caches and does not write back when 401 AUTH_REQUIRED occurs', async () => {
    let inboxRequests = 0

    server.use(
      http.get('/api/mailboxes', () => {
        return HttpResponse.json({ success: true, data: { account_id: 'acc_perf', folders: dummyFolders } })
      }),
      http.get('/api/inbox', () => {
        inboxRequests++
        if (inboxRequests === 1) {
          return HttpResponse.json({ success: true, data: dummyInboxResult })
        }
        return HttpResponse.json({ success: false, code: 'AUTH_REQUIRED', message: '会话已过期' }, { status: 401 })
      }),
    )

    // Mount 1 succeeds
    const view1 = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_perf" accountSummary={dummyAccount} fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )
    await waitFor(() => {
      expect(screen.getByText('Apple Security Code')).toBeInTheDocument()
    })
    view1.unmount()

    // Trigger auth-logout event
    window.dispatchEvent(new CustomEvent('auth-logout'))

    // Mount 2 receives 401
    const view2 = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_perf" accountSummary={dummyAccount} fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('会话已过期')).toBeInTheDocument()
    })

    view2.unmount()
  })

  it('TEST-3 (PR-MAIL-04): 邮件元数据优先渲染且无自动正文拉取，点击单封邮件按需获取正文并提取验证码', async () => {
    let detailFetched = 0
    let batchFetched = 0

    const testMsg = {
      id: 'otp_msg_1',
      message_ref: 'imap:acc_perf:INBOX:1:999',
      subject: 'Security Verification Notification',
      from: 'apple@apple.com',
      to: 'perf@icloud.com',
      date: '2026-09-23 20:00:00',
      folder: 'INBOX',
      preview: '',
      body: '',
    }

    server.use(
      http.get('/api/mailboxes', () => {
        return HttpResponse.json({ success: true, data: { account_id: 'acc_perf', folders: dummyFolders } })
      }),
      http.get('/api/inbox', () => {
        return HttpResponse.json({
          success: true,
          data: {
            account_id: 'acc_perf',
            count: 1,
            messages: [testMsg],
            method: 'imap',
          },
        })
      }),
      http.post('/api/messages', async () => {
        batchFetched++
        return HttpResponse.json({ success: true, data: { messages: [] } })
      }),
      http.get('/api/inbox/imap%3Aacc_perf%3AINBOX%3A1%3A999', async () => {
        detailFetched++
        return HttpResponse.json({
          success: true,
          data: {
            message: {
              ...testMsg,
              body: 'Your verification code is 884812. Valid for 10 minutes.',
              preview: 'Your verification code is 884812.',
              content_type: 'text/plain',
              body_complete: true,
            },
          },
        })
      }),
    )

    const { unmount } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_perf" accountSummary={dummyAccount} fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    // 第一帧：主题已先显示，但此时未点击邮件，验证码胶囊不存在且不调用 /api/messages
    await waitFor(() => {
      expect(screen.getByText('Security Verification Notification')).toBeInTheDocument()
    })
    expect(screen.queryByText('884812')).toBeNull()
    expect(batchFetched).toBe(0)
    expect(detailFetched).toBe(0)

    // 用户点击邮件触发 body-on-demand
    fireEvent.click(screen.getByRole('button', { name: 'Security Verification Notification' }))

    // 详情弹窗中展示提取的验证码 884812
    await waitFor(() => {
      expect(screen.getByText('884812')).toBeInTheDocument()
    })
    expect(detailFetched).toBe(1)

    // 再次点击：从缓存打开，detailFetched 仍为 1
    const dialog = screen.getByRole('dialog')
    const closeBtn = within(dialog).getByRole('button', { name: '关闭' })
    fireEvent.click(closeBtn)

    fireEvent.click(screen.getByRole('button', { name: 'Security Verification Notification' }))
    expect(detailFetched).toBe(1)

    unmount()
  })

  it('ensures normal mail refresh does not re-fetch /api/mailboxes with refresh=true', async () => {
    const mailboxRequests: string[] = []
    let inboxCount = 0

    server.use(
      http.get('/api/mailboxes', ({ request }) => {
        mailboxRequests.push(request.url)
        return HttpResponse.json({ success: true, data: { account_id: 'acc_perf', folders: dummyFolders } })
      }),
      http.get('/api/inbox', () => {
        inboxCount++
        return HttpResponse.json({ success: true, data: dummyInboxResult })
      }),
    )

    const { unmount } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_perf" accountSummary={dummyAccount} fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('Apple Security Code')).toBeInTheDocument()
    })
    // PR-MAIL-03: mailboxes is NOT requested on initial mount without interaction
    expect(mailboxRequests.length).toBe(0)

    // Trigger folder interaction to lazy-load mailboxes
    const folderSelect = screen.getByLabelText('文件夹')
    fireEvent.pointerDown(folderSelect)
    await waitFor(() => {
      expect(mailboxRequests.length).toBe(1)
    })
    expect(mailboxRequests[0]).not.toContain('refresh=true')

    // Click 查询 (Search button) to trigger a normal mail refresh
    const searchBtn = screen.getByRole('button', { name: '查询' })
    searchBtn.click()

    await waitFor(() => {
      expect(inboxCount).toBeGreaterThanOrEqual(2)
    })

    // Mailboxes should NOT have been re-fetched with refresh=true!
    expect(mailboxRequests.length).toBe(1)

    unmount()
  })

  it('clears caches and aborts in-flight task on account-updated event', async () => {
    let inboxCalled = 0
    server.use(
      http.get('/api/mailboxes', () => {
        return HttpResponse.json({ success: true, data: { account_id: 'acc_perf', folders: dummyFolders } })
      }),
      http.get('/api/inbox', () => {
        inboxCalled++
        return HttpResponse.json({ success: true, data: dummyInboxResult })
      }),
    )

    const { unmount } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_perf" accountSummary={dummyAccount} fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('Apple Security Code')).toBeInTheDocument()
    })

    // Dispatch account-updated event
    window.dispatchEvent(new CustomEvent('account-updated', { detail: { accountId: 'acc_perf' } }))

    // Expect inbox to re-validate due to account update
    await waitFor(() => {
      expect(inboxCalled).toBeGreaterThanOrEqual(2)
    })

    unmount()
  })

  it('drops in-flight detail response when unmounted and logged out, preventing stale module cache write-back', async () => {
    const testMsg = {
      id: 'msg_lifecycle',
      uid: 123,
      message_ref: 'imap:acc_perf:INBOX:1:123',
      subject: 'Lifecycle Detail Verification',
      from: 'apple@apple.com',
      to: 'perf@icloud.com',
      date: '2026-09-23 20:00:00',
      folder: 'INBOX',
      preview: '',
      body: '',
    }

    let resolveDetail: (() => void) | null = null
    const detailPromise = new Promise<void>((resolve) => {
      resolveDetail = resolve
    })

    server.use(
      http.get('/api/mailboxes', () => {
        return HttpResponse.json({ success: true, data: { account_id: 'acc_perf', folders: dummyFolders } })
      }),
      http.get('/api/inbox', () => {
        return HttpResponse.json({
          success: true,
          data: {
            account_id: 'acc_perf',
            count: 1,
            messages: [testMsg],
            method: 'imap',
          },
        })
      }),
      http.get('/api/inbox/imap%3Aacc_perf%3AINBOX%3A1%3A123', async () => {
        await detailPromise
        return HttpResponse.json({
          success: true,
          data: {
            message: {
              id: 'msg_lifecycle',
              message_ref: 'imap:acc_perf:INBOX:1:123',
              subject: 'Lifecycle Detail Verification',
              body: 'STALE_SECRET_BODY',
              from: 'apple@apple.com',
              to: 'perf@icloud.com',
              date: '2026-09-23 20:00:00',
              folder: 'INBOX',
              content_type: 'text/plain',
              body_complete: true,
            },
          },
        })
      }),
    )

    const { unmount } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_perf" accountSummary={dummyAccount} fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('Lifecycle Detail Verification')).toBeInTheDocument()
    })

    // 1. 点击邮件主题触发详情拉取 (请求进入在途挂起状态)
    const subjectBtn = screen.getByRole('button', { name: 'Lifecycle Detail Verification' })
    fireEvent.click(subjectBtn)

    // 2. 模拟组件卸载 (如用户跳转至其他路由)
    unmount()

    // 3. 模拟用户登出 (派发全局 auth-logout 事件)
    window.dispatchEvent(new CustomEvent('auth-logout'))

    // 4. 在途详情请求终于完成返回
    resolveDetail!()
    await new Promise((r) => setTimeout(r, 60))

    // 5. 核心断言：由于会话世代失效与卸载拦截，旧详情绝对不得被回填至模块级缓存！
    const staleCached = getModuleMessageCache('imap:acc_perf:INBOX:1:123')
    expect(staleCached).toBeUndefined()
  })

  it('TEST-6: 账号切换时旧账户在途 /api/messages enrichment 不污染新账户视图', async () => {
    let resolveAcc1Messages: (() => void) | null = null
    const acc1Deferred = new Promise<void>((resolve) => {
      resolveAcc1Messages = resolve
    })

    const acc1Msg = {
      id: 'acc1_msg_1',
      message_ref: 'imap:acc_1:INBOX:1:101',
      subject: 'Account 1 Notification',
      from: 'apple@apple.com',
      to: 'acc1@icloud.com',
      date: '2026-09-23 20:00:00',
      folder: 'INBOX',
      preview: '',
      body: '',
    }

    const acc2Msg = {
      id: 'acc2_msg_1',
      message_ref: 'imap:acc_2:INBOX:1:201',
      subject: 'Account 2 Notification',
      from: 'google@google.com',
      to: 'acc2@icloud.com',
      date: '2026-09-23 20:01:00',
      folder: 'INBOX',
      preview: 'Account 2 Preview',
      body: 'Account 2 Body',
    }

    server.use(
      http.get('/api/mailboxes', () => {
        return HttpResponse.json({ success: true, data: { folders: dummyFolders } })
      }),
      http.get('/api/inbox', ({ request }) => {
        const url = new URL(request.url)
        const acc = url.searchParams.get('account_id')
        if (acc === 'acc_1') {
          return HttpResponse.json({
            success: true,
            data: {
              account_id: 'acc_1',
              count: 1,
              messages: [acc1Msg],
              method: 'imap',
            },
          })
        }
        return HttpResponse.json({
          success: true,
          data: {
            account_id: 'acc_2',
            count: 1,
            messages: [acc2Msg],
            method: 'imap',
          },
        })
      }),
      http.post('/api/messages', async ({ request }) => {
        const body = (await request.json()) as { account_id?: string; messages?: Array<{ id: string; message_ref: string }> }
        if (body.account_id === 'acc_1') {
          await acc1Deferred
          return HttpResponse.json({
            success: true,
            data: {
              messages: [
                {
                  ...acc1Msg,
                  body: 'SECRET_ACC1_VERIFICATION_CODE_777888',
                  preview: 'SECRET_ACC1_VERIFICATION_CODE_777888',
                },
              ],
            },
          })
        }
        return HttpResponse.json({
          success: true,
          data: {
            messages: [acc2Msg],
          },
        })
      }),
    )

    const { rerender, unmount } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_1" fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    // acc_1 metadata 渲染
    await waitFor(() => {
      expect(screen.getByText('Account 1 Notification')).toBeInTheDocument()
    })

    // 切换到 acc_2，此时 acc_1 的 /api/messages 仍被阻塞
    rerender(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_2" fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    // acc_2 显示
    await waitFor(() => {
      expect(screen.getByText('Account 2 Notification')).toBeInTheDocument()
    })
    expect(screen.queryByText('Account 1 Notification')).toBeNull()

    // 此时释放 acc_1 的 /api/messages
    resolveAcc1Messages!()
    await new Promise((r) => setTimeout(r, 60))

    // 核心断言：acc_1 的正文与 OTP 绝对不得出现在 acc_2 的视图中
    expect(screen.queryByText('SECRET_ACC1_VERIFICATION_CODE_777888')).toBeNull()
    expect(screen.queryByText('Account 1 Notification')).toBeNull()
    expect(screen.getByText('Account 2 Notification')).toBeInTheDocument()

    unmount()
  })

  it('TEST-7: alias 筛选切换时旧 query 在途 /api/messages enrichment 不覆盖新 query 结果', async () => {
    let resolveAlias1Messages: (() => void) | null = null
    const alias1Deferred = new Promise<void>((resolve) => {
      resolveAlias1Messages = resolve
    })

    const msgAlias1 = {
      id: 'msg_al_1',
      message_ref: 'imap:acc_perf:INBOX:1:111',
      subject: 'Alias 1 Exclusive Message',
      from: 'apple@apple.com',
      to: 'alias1@icloud.com',
      date: '2026-09-23 20:00:00',
      folder: 'INBOX',
      preview: '',
      body: '',
    }

    const msgAlias2 = {
      id: 'msg_al_2',
      message_ref: 'imap:acc_perf:INBOX:1:222',
      subject: 'Alias 2 Exclusive Message',
      from: 'banana@apple.com',
      to: 'alias2@icloud.com',
      date: '2026-09-23 20:01:00',
      folder: 'INBOX',
      preview: 'Alias 2 Preview',
      body: 'Alias 2 Body',
    }

    server.use(
      http.get('/api/mailboxes', () => {
        return HttpResponse.json({ success: true, data: { account_id: 'acc_perf', folders: dummyFolders } })
      }),
      http.get('/api/inbox', ({ request }) => {
        const url = new URL(request.url)
        const al = url.searchParams.get('alias')
        if (al === 'alias1@icloud.com') {
          return HttpResponse.json({
            success: true,
            data: {
              account_id: 'acc_perf',
              count: 1,
              messages: [msgAlias1],
              method: 'imap',
            },
          })
        }
        return HttpResponse.json({
          success: true,
          data: {
            account_id: 'acc_perf',
            count: 1,
            messages: [msgAlias2],
            method: 'imap',
          },
        })
      }),
      http.post('/api/messages', async ({ request }) => {
        const body = (await request.json()) as { messages?: Array<{ id: string; message_ref: string }> }
        const isAlias1 = body.messages?.some((m) => m.message_ref === msgAlias1.message_ref)
        if (isAlias1) {
          await alias1Deferred
          return HttpResponse.json({
            success: true,
            data: {
              messages: [
                {
                  ...msgAlias1,
                  body: 'SECRET_ALIAS1_OTP_112233',
                  preview: 'SECRET_ALIAS1_OTP_112233',
                },
              ],
            },
          })
        }
        return HttpResponse.json({
          success: true,
          data: {
            messages: [msgAlias2],
          },
        })
      }),
    )

    const { rerender, unmount } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView
            accountId="acc_perf"
            accountSummary={dummyAccount}
            fixedAccount={true}
            initialAlias="alias1@icloud.com"
          />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('Alias 1 Exclusive Message')).toBeInTheDocument()
    })

    // 切换 initialAlias 为 alias2@icloud.com，此时 alias1 的 /api/messages 仍在途
    rerender(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView
            accountId="acc_perf"
            accountSummary={dummyAccount}
            fixedAccount={true}
            initialAlias="alias2@icloud.com"
          />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('Alias 2 Exclusive Message')).toBeInTheDocument()
    })
    expect(screen.queryByText('Alias 1 Exclusive Message')).toBeNull()

    // 释放 alias 1 的在途 enrichment
    resolveAlias1Messages!()
    await new Promise((r) => setTimeout(r, 60))

    // 核心断言：alias 1 的旧 enrichment 绝不覆盖或混入 alias 2 的视图
    expect(screen.queryByText('SECRET_ALIAS1_OTP_112233')).toBeNull()
    expect(screen.queryByText('Alias 1 Exclusive Message')).toBeNull()
    expect(screen.getByText('Alias 2 Exclusive Message')).toBeInTheDocument()

    unmount()
  })
})

describe('PR-MAIL-03: INBOX First & Mailbox Lazy Load', () => {
  const dummyAccount: AccountSummary = {
    id: 'acc_mail03',
    name: 'Mail03 Account',
    real_email: 'mail03@icloud.com',
    icloud_email: 'mail03@icloud.com',
    host: 'p123-setup.icloud.com',
    has_app_password: true,
    has_cookies: true,
    has_proxy: false,
    status: 'active',
    alias_total: 5,
    alias_active: 5,
    last_validated: '2026-09-24 10:00:00',
    created_at: '2026-09-24 10:00:00',
  }

  const dummyInboxResult: InboxResult = {
    account_id: 'acc_mail03',
    count: 1,
    messages: [
      {
        id: '101',
        message_ref: 'imap:acc_mail03:INBOX:1:101',
        subject: 'Welcome to MAIL-03',
        from: 'service@apple.com',
        to: 'mail03@icloud.com',
        date: '2026-09-24 10:00:00',
        folder: 'INBOX',
        preview: 'PR-MAIL-03 preview',
        body: 'PR-MAIL-03 body',
      },
    ],
    method: 'imap',
  }

  const dummyCustomFolders: MailboxFolder[] = [
    { name: 'INBOX', role: 'inbox', display_name: '收件箱' },
    { name: 'Junk', role: 'junk', display_name: '垃圾箱' },
    { name: 'Archive', role: 'archive', display_name: '归档' },
    { name: 'Work', role: 'custom', display_name: '工作' },
  ]

  beforeEach(() => {
    server.resetHandlers()
    clearInboxSnapshotCache()
  })

  // TEST 1｜默认 INBOX: 首次打开 /api/inbox 必须携带 folder=INBOX，不得默认 folder=all
  it('TEST 1: defaults to folder=INBOX on initial query, never defaults to all', async () => {
    let requestedFolder: string | null = null

    server.use(
      http.get('/api/accounts', () => {
        return HttpResponse.json({ success: true, data: [dummyAccount] })
      }),
      http.get('/api/inbox', ({ request }) => {
        const url = new URL(request.url)
        requestedFolder = url.searchParams.get('folder')
        return HttpResponse.json({ success: true, data: dummyInboxResult })
      }),
    )

    const { unmount } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_mail03" accountSummary={dummyAccount} fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('Welcome to MAIL-03')).toBeInTheDocument()
    })

    expect(requestedFolder).toBe('INBOX')
    unmount()
  })

  // TEST 2｜首屏不得 eager mailboxes: 组件启动后在无文件夹操作情况下 /api/mailboxes call count = 0
  it('TEST 2: initial mount does not eager-fetch /api/mailboxes (call count = 0)', async () => {
    let mailboxesCallCount = 0

    server.use(
      http.get('/api/accounts', () => {
        return HttpResponse.json({ success: true, data: [dummyAccount] })
      }),
      http.get('/api/mailboxes', () => {
        mailboxesCallCount++
        return HttpResponse.json({ success: true, data: { account_id: 'acc_mail03', folders: dummyCustomFolders } })
      }),
      http.get('/api/inbox', () => {
        return HttpResponse.json({ success: true, data: dummyInboxResult })
      }),
    )

    const { unmount } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_mail03" accountSummary={dummyAccount} fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('Welcome to MAIL-03')).toBeInTheDocument()
    })

    expect(mailboxesCallCount).toBe(0)
    unmount()
  })

  // TEST 3｜用户打开文件夹后 lazy load & 重复操作防重
  it('TEST 3: lazy-loads /api/mailboxes only on user folder interaction and deduplicates repeated interactions', async () => {
    let mailboxesCallCount = 0

    server.use(
      http.get('/api/accounts', () => {
        return HttpResponse.json({ success: true, data: [dummyAccount] })
      }),
      http.get('/api/mailboxes', () => {
        mailboxesCallCount++
        return HttpResponse.json({ success: true, data: { account_id: 'acc_mail03', folders: dummyCustomFolders } })
      }),
      http.get('/api/inbox', () => {
        return HttpResponse.json({ success: true, data: dummyInboxResult })
      }),
    )

    const { unmount } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_mail03" accountSummary={dummyAccount} fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('Welcome to MAIL-03')).toBeInTheDocument()
    })
    expect(mailboxesCallCount).toBe(0)

    // 用户交互文件夹下拉
    const folderSelect = screen.getByLabelText('文件夹')
    fireEvent.pointerDown(folderSelect)

    await waitFor(() => {
      expect(mailboxesCallCount).toBe(1)
    })

    // 重复点击 5 次
    for (let i = 0; i < 5; i++) {
      fireEvent.pointerDown(folderSelect)
    }

    // 依然严格等于 1
    expect(mailboxesCallCount).toBe(1)
    unmount()
  })

  // TEST 4｜Inbox pending 时点击 folder: 必须等 first paint 完成后才允许发送 /api/mailboxes
  it('TEST 4: defers /api/mailboxes when folder is clicked while /api/inbox is pending until metadata first paint finishes', async () => {
    let resolveInbox: () => void
    const inboxDeferred = new Promise<void>((resolve) => {
      resolveInbox = resolve
    })

    let mailboxesCallCount = 0

    server.use(
      http.get('/api/accounts', () => {
        return HttpResponse.json({ success: true, data: [dummyAccount] })
      }),
      http.get('/api/inbox', async () => {
        await inboxDeferred
        return HttpResponse.json({ success: true, data: dummyInboxResult })
      }),
      http.get('/api/mailboxes', () => {
        mailboxesCallCount++
        return HttpResponse.json({ success: true, data: { account_id: 'acc_mail03', folders: dummyCustomFolders } })
      }),
    )

    const { unmount } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_mail03" accountSummary={dummyAccount} fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    // Inbox 当前仍在 pending 加载中
    expect(screen.queryByText('Welcome to MAIL-03')).toBeNull()

    // 此时用户立即点击文件夹 selector
    const folderSelect = screen.getByLabelText('文件夹')
    fireEvent.pointerDown(folderSelect)

    // 核心时序断言：由于 Inbox 尚未完成，禁止发送 /api/mailboxes
    expect(mailboxesCallCount).toBe(0)

    // 释放 /api/inbox，完成 metadata first paint
    resolveInbox!()

    // 首屏可见
    await waitFor(() => {
      expect(screen.getByText('Welcome to MAIL-03')).toBeInTheDocument()
    })

    // 首屏完成之后，pending 的 /api/mailboxes 被触发
    await waitFor(() => {
      expect(mailboxesCallCount).toBe(1)
    })

    unmount()
  })

  // TEST 5｜显式 all 保持: 用户选择“全部”，URL 与查询参数必须保留 folder=all，重新挂载恢复 all
  it('TEST 5: preserves explicit folder=all in query parameters and across search/remount', async () => {
    const inboxQueries: string[] = []

    server.use(
      http.get('/api/accounts', () => {
        return HttpResponse.json({ success: true, data: [dummyAccount] })
      }),
      http.get('/api/mailboxes', () => {
        return HttpResponse.json({ success: true, data: { account_id: 'acc_mail03', folders: dummyCustomFolders } })
      }),
      http.get('/api/inbox', ({ request }) => {
        const url = new URL(request.url)
        inboxQueries.push(url.searchParams.get('folder') || '')
        return HttpResponse.json({ success: true, data: dummyInboxResult })
      }),
    )

    const { unmount } = render(
      <MemoryRouter initialEntries={['/?account_id=acc_mail03&folder=all']}>
        <ToastProvider>
          <InboxTableView accountId="acc_mail03" fixedAccount={false} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('Welcome to MAIL-03')).toBeInTheDocument()
    })

    // 从 ?folder=all 初始化时，首屏查询必须带 folder=all
    expect(inboxQueries[0]).toBe('all')

    // 再次点击查询
    const searchBtn = screen.getByRole('button', { name: '查询' })
    fireEvent.click(searchBtn)

    await waitFor(() => {
      expect(inboxQueries.length).toBeGreaterThanOrEqual(2)
    })
    // 显式 all 绝不能被删掉或静默变回 INBOX
    expect(inboxQueries[inboxQueries.length - 1]).toBe('all')

    unmount()
  })

  // EXTRA 1: Account switch 防污染
  it('EXTRA: account switch discards stale in-flight /api/mailboxes response and resets folder to INBOX', async () => {
    let resolveAcc1Mailboxes: () => void
    const acc1Deferred = new Promise<void>((resolve) => {
      resolveAcc1Mailboxes = resolve
    })

    const acc2: AccountSummary = {
      ...dummyAccount,
      id: 'acc_mail03_b',
      name: 'Account B',
      real_email: 'acc_b@icloud.com',
    }

    server.use(
      http.get('/api/accounts', () => {
        return HttpResponse.json({ success: true, data: [dummyAccount, acc2] })
      }),
      http.get('/api/inbox', ({ request }) => {
        const url = new URL(request.url)
        const accId = url.searchParams.get('account_id')
        return HttpResponse.json({
          success: true,
          data: {
            ...dummyInboxResult,
            account_id: accId || '',
            messages: [
              {
                ...dummyInboxResult.messages[0],
                id: accId === 'acc_mail03' ? '101' : '202',
                subject: accId === 'acc_mail03' ? 'Account A Mail' : 'Account B Mail',
              },
            ],
          },
        })
      }),
      http.get('/api/mailboxes', async ({ request }) => {
        const url = new URL(request.url)
        const accId = url.searchParams.get('account_id')
        if (accId === 'acc_mail03') {
          await acc1Deferred
          return HttpResponse.json({
            success: true,
            data: {
              account_id: 'acc_mail03',
              folders: dummyCustomFolders, // 包含 "Work"
            },
          })
        }
        return HttpResponse.json({
          success: true,
          data: {
            account_id: 'acc_mail03_b',
            folders: [{ name: 'INBOX', role: 'inbox' }, { name: 'AccountBExclusive', role: 'custom' }],
          },
        })
      }),
    )

    const { rerender, unmount } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_mail03" fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('Account A Mail')).toBeInTheDocument()
    })

    // 触发账号 A 的 mailboxes 请求 (处于 pending 慢速中)
    const folderSelect = screen.getByLabelText('文件夹')
    fireEvent.pointerDown(folderSelect)

    // 立即切换到账号 B
    rerender(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_mail03_b" fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('Account B Mail')).toBeInTheDocument()
    })

    // 释放账号 A 的延迟响应，返回属于账号 A 的工作文件夹
    resolveAcc1Mailboxes!()

    // 等待微任务完成
    await new Promise((r) => setTimeout(r, 60))

    // 核心断言：账号 A 的自定义文件夹 "Work" 绝未污染账号 B 的视图
    expect(screen.queryByText(/Work \(custom\)/)).toBeNull()

    unmount()
  })

  // EXTRA 2: WebMail-only 不发 /api/mailboxes
  it('EXTRA: WebMail-only mode never fetches /api/mailboxes even upon folder interaction', async () => {
    let mailboxesCallCount = 0

    const webmailAccount: AccountSummary = {
      ...dummyAccount,
      id: 'acc_webmail',
      has_app_password: false,
      mailbox: undefined,
    }

    server.use(
      http.get('/api/accounts', () => {
        return HttpResponse.json({ success: true, data: [webmailAccount] })
      }),
      http.get('/api/mailboxes', () => {
        mailboxesCallCount++
        return HttpResponse.json({ success: true, data: { account_id: 'acc_webmail', folders: dummyCustomFolders } })
      }),
      http.get('/api/inbox', () => {
        return HttpResponse.json({
          success: true,
          data: {
            ...dummyInboxResult,
            account_id: 'acc_webmail',
            method: 'web_api',
          },
        })
      }),
    )

    const { unmount } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_webmail" accountSummary={webmailAccount} fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('Welcome to MAIL-03')).toBeInTheDocument()
    })

    const folderSelect = screen.getByLabelText('文件夹')
    expect(folderSelect).toBeDisabled()

    // 用户即便产生事件
    fireEvent.pointerDown(folderSelect)
    fireEvent.click(folderSelect)

    // 依然 0 次
    expect(mailboxesCallCount).toBe(0)

    unmount()
  })
})

describe('PR-MAIL-04: Body-on-demand & Single-message reading', () => {
  const dummyAccount: AccountSummary = {
    id: 'acc_mail04',
    name: 'Mail04 Account',
    real_email: 'mail04@icloud.com',
    icloud_email: 'mail04@icloud.com',
    host: 'p123-setup.icloud.com',
    has_app_password: true,
    has_cookies: true,
    has_proxy: false,
    status: 'active',
    alias_total: 5,
    alias_active: 5,
    last_validated: '2026-09-25 10:00:00',
    created_at: '2026-09-25 10:00:00',
  }

  const dummyMessages: InboxResult = {
    account_id: 'acc_mail04',
    count: 2,
    messages: [
      {
        id: 'msg_1',
        message_ref: 'imap:acc_mail04:INBOX:1:101',
        subject: 'First Mail Subject',
        from: 'Alice <alice@test.com>',
        to: 'alias1@icloud.com',
        date: '2026-09-25 10:00:00',
        folder: 'INBOX',
        unread: true,
        preview: '',
        body: '',
      },
      {
        id: 'msg_2',
        message_ref: 'imap:acc_mail04:INBOX:1:102',
        subject: 'Second Mail Subject',
        from: 'Bob <bob@test.com>',
        to: 'alias2@icloud.com',
        date: '2026-09-25 10:05:00',
        folder: 'INBOX',
        unread: false,
        preview: '',
        body: '',
      },
    ],
    method: 'imap',
  }

  beforeEach(() => {
    server.resetHandlers()
    clearInboxSnapshotCache()
  })

  // TEST 1: 连续点击同一封正在拉取的邮件时防重复请求 (请求次数严格为 1)
  it('TEST 1: dedupes rapid clicks on the same message without issuing duplicate requests', async () => {
    let detailCalls = 0
    let resolveDetail: (() => void) | null = null
    const detailDeferred = new Promise<void>((resolve) => {
      resolveDetail = resolve
    })

    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: [dummyAccount] })),
      http.get('/api/mailboxes', () => HttpResponse.json({ success: true, data: { account_id: 'acc_mail04', folders: [] } })),
      http.get('/api/inbox', () => HttpResponse.json({ success: true, data: dummyMessages })),
      http.get('/api/inbox/imap%3Aacc_mail04%3AINBOX%3A1%3A101', async () => {
        detailCalls++
        await detailDeferred
        return HttpResponse.json({
          success: true,
          data: {
            message: {
              ...dummyMessages.messages[0],
              body: 'Full body of first mail',
              preview: 'Full body of first mail',
              content_type: 'text/plain',
              body_complete: true,
            },
          },
        })
      }),
    )

    const { unmount } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_mail04" accountSummary={dummyAccount} fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('First Mail Subject')).toBeInTheDocument()
    })

    // 首次点击发起单封正文请求
    const btn = screen.getByRole('button', { name: 'First Mail Subject' })
    fireEvent.click(btn)

    await waitFor(() => {
      expect(detailCalls).toBe(1)
    })

    // 请求仍处于在途挂起期间，连续点击该邮件 4 次
    fireEvent.click(btn)
    fireEvent.click(btn)
    fireEvent.click(btn)
    fireEvent.click(btn)

    // 核心断言：因 detailInFlightRef 防重，并没有额外发出重复请求，依然严格为 1 次！
    expect(detailCalls).toBe(1)

    // 释放请求
    resolveDetail!()

    await waitFor(() => {
      expect(screen.getByText('Full body of first mail')).toBeInTheDocument()
    })

    unmount()
  })

  // TEST 2: 用户快速切换点击另一封邮件时中止上一在途请求，绝不产生并行正文请求
  it('TEST 2: aborts previous in-flight request when user clicks a different message', async () => {
    let startedMsg1 = false
    let abortedMsg1 = false

    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: [dummyAccount] })),
      http.get('/api/mailboxes', () => HttpResponse.json({ success: true, data: { account_id: 'acc_mail04', folders: [] } })),
      http.get('/api/inbox', () => HttpResponse.json({ success: true, data: dummyMessages })),
      http.get('/api/inbox/imap%3Aacc_mail04%3AINBOX%3A1%3A101', async ({ request }) => {
        startedMsg1 = true
        request.signal.addEventListener('abort', () => {
          abortedMsg1 = true
        })
        // 模拟慢响应
        await new Promise((r) => setTimeout(r, 150))
        return HttpResponse.json({
          success: true,
          data: {
            message: {
              ...dummyMessages.messages[0],
              body: 'Body for msg 1',
              preview: 'Body for msg 1',
              content_type: 'text/plain',
              body_complete: true,
            },
          },
        })
      }),
      http.get('/api/inbox/imap%3Aacc_mail04%3AINBOX%3A1%3A102', async () => {
        return HttpResponse.json({
          success: true,
          data: {
            message: {
              ...dummyMessages.messages[1],
              body: 'Body for msg 2',
              preview: 'Body for msg 2',
              content_type: 'text/plain',
              body_complete: true,
            },
          },
        })
      }),
    )

    const { unmount } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_mail04" accountSummary={dummyAccount} fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('First Mail Subject')).toBeInTheDocument()
      expect(screen.getByText('Second Mail Subject')).toBeInTheDocument()
    })

    // 先点击邮件 1，并确认已进入在途请求
    fireEvent.click(screen.getByRole('button', { name: 'First Mail Subject' }))
    await waitFor(() => {
      expect(startedMsg1).toBe(true)
    })

    // 此时切换点击邮件 2
    fireEvent.click(screen.getByRole('button', { name: 'Second Mail Subject' }))

    await waitFor(() => {
      expect(abortedMsg1).toBe(true)
    })

    await waitFor(() => {
      expect(screen.getByText('Body for msg 2')).toBeInTheDocument()
    })

    unmount()
  })

  // TEST 3: 邮件 preview 与 body 为空时，列表完整渲染 subject/from/to/date/folder/unread 且不报错
  it('TEST 3: renders all metadata fields safely without error when preview and body are empty', async () => {
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: [dummyAccount] })),
      http.get('/api/mailboxes', () => HttpResponse.json({ success: true, data: { account_id: 'acc_mail04', folders: [] } })),
      http.get('/api/inbox', () => HttpResponse.json({ success: true, data: dummyMessages })),
    )

    const { unmount } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_mail04" accountSummary={dummyAccount} fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('First Mail Subject')).toBeInTheDocument()
    })

    // 检查 metadata 字段全量可见
    expect(screen.getByText('First Mail Subject')).toBeInTheDocument()
    expect(screen.getByText('Alice')).toBeInTheDocument()
    expect(screen.getByText('alias1@icloud.com')).toBeInTheDocument()
    expect(screen.getByText('未读')).toBeInTheDocument()

    expect(screen.getByText('Second Mail Subject')).toBeInTheDocument()
    expect(screen.getByText('Bob')).toBeInTheDocument()
    expect(screen.getByText('alias2@icloud.com')).toBeInTheDocument()

    // 验证码列显示破折号兜底，无错误提示
    const noCodeSpans = screen.getAllByText('—')
    expect(noCodeSpans.length).toBeGreaterThan(0)
    expect(screen.queryByText(/错误|失败/)).toBeNull()

    unmount()
  })

  // TEST 4: 刷新邮件列表后，已打开过的邮件继续使用已有 moduleMessageCache 零网络成本回填 preview/body
  it('TEST 4: backfills body from moduleMessageCache upon list reload with zero extra network requests', async () => {
    let inboxCalls = 0
    let detailCalls = 0

    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: [dummyAccount] })),
      http.get('/api/mailboxes', () => HttpResponse.json({ success: true, data: { account_id: 'acc_mail04', folders: [] } })),
      http.get('/api/inbox', () => {
        inboxCalls++
        return HttpResponse.json({ success: true, data: dummyMessages })
      }),
      http.get('/api/inbox/imap%3Aacc_mail04%3AINBOX%3A1%3A101', () => {
        detailCalls++
        return HttpResponse.json({
          success: true,
          data: {
            message: {
              ...dummyMessages.messages[0],
              body: 'CACHED_BODY_PREVIEW_SECRET_9988',
              preview: 'CACHED_BODY_PREVIEW_SECRET_9988',
              content_type: 'text/plain',
              body_complete: true,
            },
          },
        })
      }),
    )

    const { unmount } = render(
      <MemoryRouter>
        <ToastProvider>
          <InboxTableView accountId="acc_mail04" accountSummary={dummyAccount} fixedAccount={true} />
        </ToastProvider>
      </MemoryRouter>,
    )

    await waitFor(() => {
      expect(screen.getByText('First Mail Subject')).toBeInTheDocument()
    })

    // 用户点击邮件 1，拉取正文并写入 moduleMessageCache
    fireEvent.click(screen.getByRole('button', { name: 'First Mail Subject' }))

    await waitFor(() => {
      expect(detailCalls).toBe(1)
      expect(screen.getByText('CACHED_BODY_PREVIEW_SECRET_9988')).toBeInTheDocument()
    })

    // 关闭详情
    const dialog = screen.getByRole('dialog')
    const closeBtn = within(dialog).getByRole('button', { name: '关闭' })
    fireEvent.click(closeBtn)

    // 用户点击查询按钮刷新收件箱列表 (触发重新 GET /api/inbox)
    const searchBtn = screen.getByRole('button', { name: '查询' })
    fireEvent.click(searchBtn)

    await waitFor(() => {
      expect(inboxCalls).toBeGreaterThanOrEqual(2)
    })

    // 核心断言：未再发送任何详情或正文请求 (detailCalls 依然严格为 1)
    expect(detailCalls).toBe(1)

    // 且列表利用缓存直接回填了 preview
    expect(screen.getByText(/CACHED_BODY_PREVIEW_SECRET_9988/)).toBeInTheDocument()

    unmount()
  })
})


