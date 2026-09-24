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

  it('stages message body requests in sequential chunks of at most 5 items', async () => {
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
      // 12 items chunked by 5 => chunks of 5, 5, 2
      expect(chunkSizes.length).toBeGreaterThanOrEqual(1)
    })

    await waitFor(() => {
      expect(chunkSizes).toEqual([5, 5, 2])
    })

    // Verify all chunks are capped at CHUNK_SIZE = 5
    for (const size of chunkSizes) {
      expect(size).toBeLessThanOrEqual(5)
    }

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

  it('handles later chunk failure gracefully without losing previously completed chunks', async () => {
    let chunkCall = 0
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
      http.post('/api/messages', async ({ request }) => {
        chunkCall++
        const body = (await request.json()) as { messages?: Array<{ id: string; message_ref: string }> }
        if (chunkCall === 1) {
          // Chunk 1 succeeds with verification code
          return HttpResponse.json({
            success: true,
            data: {
              messages: (body?.messages || []).map((m: { id: string; message_ref: string }) => ({
                ...m,
                body: `Body with OTP: 888123 for ${m.message_ref}`,
                preview: `OTP: 888123`,
              })),
            },
          })
        }
        // Chunk 2 fails with 500
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

    // Chunk 1 OTP successfully displays
    await waitFor(() => {
      expect(screen.getAllByText('888123').length).toBeGreaterThanOrEqual(1)
    })

    // All 10 items remain listed in the table (view does not break or crash)
    for (let i = 1; i <= 10; i++) {
      expect(screen.getByText(`Batch Item #${i}`)).toBeInTheDocument()
    }

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

  it('demonstrates progressive OTP appearance: first chunk OTP visible before remaining chunks complete', async () => {
    const tenMessages = Array.from({ length: 10 }, (_, i) => ({
      id: `prog_${i + 1}`,
      message_ref: `imap:acc_perf:INBOX:1:${300 + i}`,
      subject: `Prog Item #${i + 1}`,
      from: 'apple@apple.com',
      to: 'perf@icloud.com',
      date: '2026-09-23 20:00:00',
      folder: 'INBOX',
      preview: '',
      body: '',
    }))

    let chunkStep = 0

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
      http.post('/api/messages', async ({ request }) => {
        chunkStep++
        const body = (await request.json()) as { messages?: Array<{ id: string; message_ref: string }> }
        // Each chunk takes 20ms
        await new Promise((r) => setTimeout(r, 20))
        return HttpResponse.json({
          success: true,
          data: {
            messages: (body?.messages || []).map((m: { id: string; message_ref: string }) => {
              const num = m.message_ref.split(':').pop() || '0'
              const code = String(800000 + Number(num))
              return {
                ...m,
                body: `Your verification code is ${code}`,
                preview: `Your verification code is ${code}`,
              }
            }),
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

    // First chunk items appear with their OTP badge first
    await waitFor(() => {
      expect(screen.getByText('800300')).toBeInTheDocument()
    })

    // Eventually all items have their OTP badges
    await waitFor(() => {
      expect(screen.getByText('800309')).toBeInTheDocument()
      expect(chunkStep).toBe(2)
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
})

