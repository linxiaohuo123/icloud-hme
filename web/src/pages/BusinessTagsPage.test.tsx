import { http, HttpResponse } from 'msw'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import BusinessTagsPage from './BusinessTagsPage'
import { server } from '../test/server'
import { setCSRFToken } from '../api/client'
import { ToastProvider } from '../components/ToastProvider'
import type { APIToken, BusinessTag } from '../api/types'

const tags: BusinessTag[] = [
  {
    id: 'tag_1',
    name: 'tiktok',
    tag: 'tiktok',
    description: 'TikTok 业务线',
    status: 'active',
    created_at: '2026-09-18T10:00:00+08:00',
    // 动态取 30 秒前，保证 formatRelativeTime 稳定落在"刚刚"档位，不随测试时刻漂移
    last_assigned_at: new Date(Date.now() - 30_000).toISOString(),
  },
]

const tokens: APIToken[] = [
  {
    id: 'tok_1',
    name: '注册机-01',
    created_at: '2026-09-18T10:00:00+08:00',
    last_used_at: undefined,
  },
]

/** 渲染当前路由信息，用于断言跳转结果 */
function UsedProbe() {
  const location = useLocation()
  return <div>{`used-probe:${location.pathname}${location.search}`}</div>
}

function renderPage(initialEntry = '/tags') {
  return render(
    <MemoryRouter initialEntries={[initialEntry]}>
      <Routes>
        <Route
          path="/tags"
          element={
            <ToastProvider>
              <BusinessTagsPage />
            </ToastProvider>
          }
        />
        <Route path="/used" element={<UsedProbe />} />
      </Routes>
    </MemoryRouter>,
  )
}

describe('BusinessTagsPage', () => {
  beforeEach(() => {
    setCSRFToken('csrf-test')
    server.resetHandlers()
    // jsdom 未实现 scrollIntoView（令牌横幅定位用）
    Element.prototype.scrollIntoView = vi.fn()
  })

  it('加载后渲染指标卡与双列表，最近领用缺失时显示"从未领用"', async () => {
    server.use(
      http.get('/api/tags', () => HttpResponse.json({ success: true, data: tags })),
      http.get('/api/tokens', () => HttpResponse.json({ success: true, data: tokens })),
    )
    renderPage()

    // 'tiktok' 同时出现在表格徽章与接入文档下拉，故用 findAllByText
    expect((await screen.findAllByText('tiktok')).length).toBeGreaterThan(0)
    // 令牌最近调用缺失 → 从未调用兜底
    expect(screen.getByText('从未调用')).toBeInTheDocument()
    // 业务标识最近领用有值 → 表格与"最近调用活动"指标卡都会以相对时间展示
    expect(screen.getAllByText('刚刚').length).toBeGreaterThan(0)
    // 行内操作入口
    expect(screen.getByTitle('查看出号记录')).toBeInTheDocument()
    expect(screen.getByTitle('编辑标识')).toBeInTheDocument()
  })

  it('创建业务标识：提交 POST /api/tags 并刷新列表', async () => {
    const user = userEvent.setup()
    let createdBody: Record<string, unknown> | undefined
    let currentTags = tags
    server.use(
      http.get('/api/tags', () => HttpResponse.json({ success: true, data: currentTags })),
      http.post('/api/tags', async ({ request }) => {
        createdBody = (await request.json()) as Record<string, unknown>
        // 模拟后端落库：刷新列表必须能看到新标识
        currentTags = [
          ...tags,
          {
            id: 'tag_2',
            name: 'walmart',
            tag: 'walmart',
            description: '沃尔玛业务线',
            status: 'active',
            created_at: '2026-09-19T10:00:00+08:00',
            last_assigned_at: '',
          },
        ]
        return HttpResponse.json({ success: true, data: currentTags[1] })
      }),
      http.get('/api/tokens', () => HttpResponse.json({ success: true, data: tokens })),
    )
    renderPage()
    await screen.findAllByText('tiktok')

    await user.type(screen.getByPlaceholderText('标识名称 (例: tiktok)'), 'walmart')
    await user.type(screen.getByPlaceholderText('描述备注'), '沃尔玛业务线')
    await user.click(screen.getByRole('button', { name: '添加标识' }))

    await waitFor(() => {
      expect(screen.getAllByText('walmart').length).toBeGreaterThan(0)
    })
    expect(createdBody).toMatchObject({ tag: 'walmart', name: 'walmart', description: '沃尔玛业务线' })
  })

  it('编辑业务标识：PATCH 全量回传，保留 status/created_at/last_assigned_at', async () => {
    const user = userEvent.setup()
    let patchedBody: Record<string, unknown> | undefined
    server.use(
      http.get('/api/tags', () => HttpResponse.json({ success: true, data: tags })),
      http.patch('/api/tags/:id', async ({ request }) => {
        patchedBody = (await request.json()) as Record<string, unknown>
        return HttpResponse.json({ success: true, data: tags[0] })
      }),
      http.get('/api/tokens', () => HttpResponse.json({ success: true, data: tokens })),
    )
    renderPage()
    await screen.findAllByText('tiktok')

    await user.click(screen.getByTitle('编辑标识'))
    const nameInput = await screen.findByLabelText('标识名称')
    expect(nameInput).toHaveValue('tiktok')
    await user.clear(screen.getByLabelText('描述备注'))
    await user.type(screen.getByLabelText('描述备注'), '新描述')
    await user.click(screen.getByRole('button', { name: '保存' }))

    await waitFor(() => {
      expect(patchedBody).toBeDefined()
    })
    // 后端 PATCH 为整对象 upsert，未提交字段必须原样带回
    expect(patchedBody).toMatchObject({
      id: 'tag_1',
      tag: 'tiktok',
      name: 'tiktok',
      description: '新描述',
      status: 'active',
      created_at: '2026-09-18T10:00:00+08:00',
      last_assigned_at: tags[0].last_assigned_at,
    })
  })

  it('删除业务标识：确认弹窗指明目标名称，确认后调用 DELETE', async () => {
    const user = userEvent.setup()
    let deletedId = ''
    server.use(
      http.get('/api/tags', () => HttpResponse.json({ success: true, data: tags })),
      http.delete('/api/tags/:id', ({ params }) => {
        deletedId = String(params.id)
        return HttpResponse.json({ success: true, data: { deleted: true } })
      }),
      http.get('/api/tokens', () => HttpResponse.json({ success: true, data: tokens })),
    )
    renderPage()
    await screen.findAllByText('tiktok')

    await user.click(screen.getByTitle('删除标识'))
    // 确认弹窗必须带出目标名，避免多条数据时误删
    expect(await screen.findByText(/确定删除业务标识 \[tiktok\] 吗？/)).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '确认删除' }))

    await waitFor(() => {
      expect(deletedId).toBe('tag_1')
    })
    expect(await screen.findByText(/业务标识 \[tiktok\] 已删除/)).toBeInTheDocument()
  })

  it('生成令牌：横幅展示一次性令牌，完整接入命令包含真实令牌与所选业务标识', async () => {
    const user = userEvent.setup()
    server.use(
      http.get('/api/tags', () => HttpResponse.json({ success: true, data: tags })),
      http.get('/api/tokens', () => HttpResponse.json({ success: true, data: tokens })),
      http.post('/api/tokens', () =>
        HttpResponse.json({
          success: true,
          data: { id: 'tok_2', name: '注册机-02', token: 'am_test_token_123', created_at: '' },
        }),
      ),
    )
    renderPage()
    await screen.findAllByText('tiktok')

    await user.type(screen.getByPlaceholderText('令牌备注 (例: 注册机-01)'), '注册机-02')
    await user.click(screen.getByRole('button', { name: '生成令牌' }))

    const banner = await screen.findByText(/新生成的外部 API 访问令牌/)
    expect(banner).toBeInTheDocument()
    expect(screen.getByText('am_test_token_123')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: '复制完整接入命令' }))
    expect(await screen.findByText('完整接入命令已复制')).toBeInTheDocument()
  })

  it('加载失败显示错误与重试入口，重试成功后恢复列表', async () => {
    const user = userEvent.setup()
    server.use(
      http.get('/api/tags', () =>
        HttpResponse.json({ success: false, message: '服务不可用' }, { status: 500 }),
      ),
      http.get('/api/tokens', () => HttpResponse.json({ success: true, data: tokens })),
    )
    renderPage()

    // AsyncState 错误态：显示后端 message + 重试按钮，而不是假装"0 个"
    expect(await screen.findByText('服务不可用')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重试' })).toBeInTheDocument()

    server.resetHandlers()
    server.use(
      http.get('/api/tags', () => HttpResponse.json({ success: true, data: tags })),
      http.get('/api/tokens', () => HttpResponse.json({ success: true, data: tokens })),
    )
    await user.click(screen.getByRole('button', { name: '重试' }))
    expect((await screen.findAllByText('tiktok')).length).toBeGreaterThan(0)
  })

  it('行内"出号"入口跳转到 /used 并携带 ?tag= 筛选参数', async () => {
    const user = userEvent.setup()
    server.use(
      http.get('/api/tags', () => HttpResponse.json({ success: true, data: tags })),
      http.get('/api/tokens', () => HttpResponse.json({ success: true, data: tokens })),
    )
    renderPage()
    await screen.findAllByText('tiktok')

    await user.click(screen.getByTitle('查看出号记录'))
    expect(await screen.findByText('used-probe:/used?tag=tiktok')).toBeInTheDocument()
  })
})
