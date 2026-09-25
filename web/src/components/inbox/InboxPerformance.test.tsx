import { describe, it, expect, beforeEach } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
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
    // 2. /api/mailboxes was requested on mount
    expect(requestCalls.some((c) => c.includes('/api/mailboxes'))).toBe(true)
    // 3. /api/inbox was requested
    expect(requestCalls.some((c) => c.includes('/api/inbox'))).toBe(true)
    // 4. /api/messages was requested with chunked batch size <= 5
    expect(requestCalls.some((c) => c.includes('/api/messages (batch size: 2)'))).toBe(true)

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

  it('TEST-4: 当前可见缺失正文目标单次 background batch 请求 (12 封只发 1 次 batch_size=12)', async () => {
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
          data: {
            messages: (body?.messages || []).map((m: { id: string; message_ref: string }) => ({
              ...m,
              body: `Body for ${m.message_ref}`,
              preview: `Preview for ${m.message_ref}`,
            })),
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
      expect(screen.getByText('Verification Code #1')).toBeInTheDocument()
      expect(screen.getByText('Verification Code #12')).toBeInTheDocument()
    })

    // 关键断言：不再分片为 [5, 5, 2]，而是单次请求 12
    await waitFor(() => {
      expect(chunkSizes).toEqual([12])
    })
    expect(chunkSizes.length).toBe(1)

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
      http.post('/api/messages', async () => {
        // Return body ONLY for the Junk message
        return HttpResponse.json({
          success: true,
          data: {
            messages: [
              {
                id: '100',
                uid: 100,
                folder: 'Junk',
                message_ref: 'imap:acc_perf:Junk:1:100',
                subject: 'Junk Spam Warning',
                body: 'Malicious spam content',
                preview: 'Malicious spam content',
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
      expect(screen.getByText('Inbox Security Code')).toBeInTheDocument()
      expect(screen.getByText('Junk Spam Warning')).toBeInTheDocument()
    })

    // Verify Junk received its body
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

  it('TEST-3: 邮件元数据优先渲染，后台正文返回后渐进提取并显示验证码胶囊 (Progressive OTP Enrichment)', async () => {
    let resolveMessages: (() => void) | null = null
    const messagesDeferred = new Promise<void>((resolve) => {
      resolveMessages = resolve
    })

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
        await messagesDeferred
        return HttpResponse.json({
          success: true,
          data: {
            messages: [
              {
                ...testMsg,
                body: 'Your verification code is 884812. Valid for 10 minutes.',
                preview: 'Your verification code is 884812.',
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

    // 第一帧：主题已先显示，但此时正文未到，验证码胶囊不存在
    await waitFor(() => {
      expect(screen.getByText('Security Verification Notification')).toBeInTheDocument()
    })
    expect(screen.queryByText('884812')).toBeNull()

    // 释放后台 /api/messages 请求
    resolveMessages!()

    // 响应式合并后，验证码胶囊渐进出场
    await waitFor(() => {
      expect(screen.getByText('884812')).toBeInTheDocument()
    })

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
    expect(mailboxRequests.length).toBe(1)
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

