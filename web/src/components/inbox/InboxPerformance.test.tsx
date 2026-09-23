import { describe, it, expect, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { http, HttpResponse } from 'msw'
import { server } from '../../test/server'
import InboxTableView, { clearInboxSnapshotCache } from './InboxTableView'
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
})
