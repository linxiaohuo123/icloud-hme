/**
 * [INPUT]: 依赖 @testing-library/react, vitest, msw, react-router-dom, components/inbox/InboxTableView
 * [OUTPUT]: 对外提供 InboxTableView 跨账号并发防污染与详情缓存隔离单元测试
 * [POS]: web/src/components/inbox 的单元测试防线
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { http, HttpResponse, delay } from 'msw'
import { render, screen, waitFor, fireEvent, act } from '@testing-library/react'
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
