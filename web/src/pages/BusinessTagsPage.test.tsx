import { http, HttpResponse } from 'msw'
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
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
    token_prefix: 'ihme_live_tok1',
    created_at: '2026-09-18T10:00:00+08:00',
    last_used_at: undefined,
    needs_rotation: false,
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

  it('编辑业务标识：PATCH 只提交编辑字段，不回传旧状态和活动时间', async () => {
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
    expect(patchedBody).toEqual({
      tag: 'tiktok',
      name: 'tiktok',
      description: '新描述',
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

  it('TestTokenListNeverStoresPlaintextSecret: 令牌列表只展示脱敏前缀，绝不存储或泄漏明文 Secret', async () => {
    const sensitiveTokens = [
      {
        id: 'tok_sec_1',
        name: '安全检测令牌',
        token_prefix: 'sec_pref',
        created_at: '2026-09-18T10:00:00+08:00',
        scopes: 'allocate,verify',
        needs_rotation: false,
      },
    ]
    server.use(
      http.get('/api/tags', () => HttpResponse.json({ success: true, data: tags })),
      http.get('/api/tokens', () => HttpResponse.json({ success: true, data: sensitiveTokens })),
    )
    renderPage()
    expect(await screen.findByText('安全检测令牌')).toBeInTheDocument()
    expect(screen.getByText('sec_pref****')).toBeInTheDocument()
    // DOM 中绝不包含任何未脱敏长密钥
    expect(screen.queryByText(/sec_pref[a-zA-Z0-9]{10,}/)).toBeNull()
  })

  it('TestLegacyTokenNeedsRotationShown: 历史遗留令牌展示"待轮换"醒目标签', async () => {
    const legacyTokens = [
      {
        id: 'tok_legacy_1',
        name: '老旧令牌',
        token_prefix: 'leg_pref',
        created_at: '2026-09-18T10:00:00+08:00',
        scopes: 'admin',
        needs_rotation: true,
      },
    ]
    server.use(
      http.get('/api/tags', () => HttpResponse.json({ success: true, data: tags })),
      http.get('/api/tokens', () => HttpResponse.json({ success: true, data: legacyTokens })),
    )
    renderPage()
    expect(await screen.findByText('待轮换')).toBeInTheDocument()
    expect(screen.getByTitle(/建议立即轮换/)).toBeInTheDocument()
  })

  it('TestRotateTokenShowsSecretOnce: 点击轮换调用 POST /api/tokens/:id/rotate，新令牌仅在横幅展示一次', async () => {
    const user = userEvent.setup()
    let rotateCalled = false
    const activeTokens = [
      {
        id: 'tok_rot_1',
        name: '待轮换令牌',
        token_prefix: 'rot_pref',
        created_at: '2026-09-18T10:00:00+08:00',
        scopes: 'allocate,verify',
        needs_rotation: false,
      },
    ]
    server.use(
      http.get('/api/tags', () => HttpResponse.json({ success: true, data: tags })),
      http.get('/api/tokens', () => HttpResponse.json({ success: true, data: activeTokens })),
      http.post('/api/tokens/tok_rot_1/rotate', () => {
        rotateCalled = true
        return HttpResponse.json({
          success: true,
          data: {
            id: 'tok_rot_1',
            name: '待轮换令牌',
            token_prefix: 'new_rot_',
            token: 'new_rotated_plaintext_secret_999',
            created_at: '2026-09-18T10:00:00+08:00',
            scopes: 'allocate,verify',
            needs_rotation: false,
          },
        })
      }),
    )
    renderPage()
    expect(await screen.findByText('待轮换令牌')).toBeInTheDocument()

    const rotateBtn = screen.getByRole('button', { name: /轮换/ })
    await user.click(rotateBtn)

    await waitFor(() => expect(rotateCalled).toBe(true))
    // 横幅中展示了一次性新密钥
    expect(await screen.findByText('new_rotated_plaintext_secret_999')).toBeInTheDocument()
  })

  it('TestRotateRefreshKeepsSameTokenID: 轮换成功后保持相同的 Token ID', async () => {
    const user = userEvent.setup()
    let tokensData = [
      {
        id: 'tok_stable_id',
        name: '稳定标识令牌',
        token_prefix: 'old_pref',
        created_at: '2026-09-18T10:00:00+08:00',
        scopes: 'allocate,verify',
        needs_rotation: true,
      },
    ]
    server.use(
      http.get('/api/tags', () => HttpResponse.json({ success: true, data: tags })),
      http.get('/api/tokens', () => HttpResponse.json({ success: true, data: tokensData })),
      http.post('/api/tokens/tok_stable_id/rotate', () => {
        tokensData = [
          {
            id: 'tok_stable_id',
            name: '稳定标识令牌',
            token_prefix: 'new_pref',
            created_at: '2026-09-18T10:00:00+08:00',
            scopes: 'allocate,verify',
            needs_rotation: false,
          },
        ]
        return HttpResponse.json({
          success: true,
          data: {
            ...tokensData[0],
            token: 'new_secret_abc',
          },
        })
      }),
    )
    renderPage()
    expect(await screen.findByText('old_pref****')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: /轮换/ }))
    expect(await screen.findByText('new_secret_abc')).toBeInTheDocument()
    // 列表刷新后，Token ID 保持一致，前缀更新为新值且不再待轮换
    expect(await screen.findByText('new_pref****')).toBeInTheDocument()
    expect(screen.getByText('正常')).toBeInTheDocument()
    expect(screen.queryByText('待轮换')).toBeNull()
  })

  it('TestRevokedTokenCannotBeRotatedFromUI: 已作废令牌在 UI 上显示"已作废"，不提供作废或轮换按钮', async () => {
    const revokedTokens = [
      {
        id: 'tok_rev_1',
        name: '作废令牌',
        token_prefix: 'rev_pref',
        created_at: '2026-09-18T10:00:00+08:00',
        revoked_at: '2026-09-20T10:00:00+08:00',
        scopes: 'allocate',
        needs_rotation: false,
      },
    ]
    server.use(
      http.get('/api/tags', () => HttpResponse.json({ success: true, data: tags })),
      http.get('/api/tokens', () => HttpResponse.json({ success: true, data: revokedTokens })),
    )
    renderPage()
    expect(await screen.findByText('作废令牌')).toBeInTheDocument()
    // 状态与操作列均明确展示已作废
    const revokedTexts = screen.getAllByText('已作废')
    expect(revokedTexts.length).toBeGreaterThanOrEqual(1)
    // 不显示轮换或作废按钮
    expect(screen.queryByRole('button', { name: /轮换/ })).toBeNull()
    expect(screen.queryByTitle('作废令牌')).toBeNull()
  })

  it('TestExpiredTokenStatusRenderedCorrectly: 已过期令牌渲染"已过期"状态，不提供作废或轮换按钮', async () => {
    const expiredTokens = [
      {
        id: 'tok_exp_1',
        name: '过期令牌',
        token_prefix: 'exp_pref',
        created_at: '2026-01-01T00:00:00Z',
        expires_at: '2026-01-02T00:00:00Z',
        scopes: 'allocate',
        needs_rotation: false,
      },
    ]
    server.use(
      http.get('/api/tags', () => HttpResponse.json({ success: true, data: tags })),
      http.get('/api/tokens', () => HttpResponse.json({ success: true, data: expiredTokens })),
    )
    renderPage()
    expect(await screen.findByText('过期令牌')).toBeInTheDocument()
    const expiredTexts = screen.getAllByText('已过期')
    expect(expiredTexts.length).toBeGreaterThanOrEqual(1)
    expect(screen.queryByRole('button', { name: /轮换/ })).toBeNull()
    expect(screen.queryByTitle('作废令牌')).toBeNull()
  })

  it('TestPurgeRevokedToken: 已作废或过期令牌支持彻底删除记录', async () => {
    let purged = false
    const revokedTokens = [
      {
        id: 'tok_purge_1',
        name: '需清理废弃令牌',
        token_prefix: 'purg',
        created_at: '2026-09-18T10:00:00+08:00',
        revoked_at: '2026-09-20T10:00:00+08:00',
        scopes: 'allocate',
        needs_rotation: false,
      },
    ]
    server.use(
      http.get('/api/tags', () => HttpResponse.json({ success: true, data: tags })),
      http.get('/api/tokens', () => HttpResponse.json({ success: true, data: purged ? [] : revokedTokens })),
      http.delete('/api/tokens/tok_purge_1', ({ request }) => {
        const url = new URL(request.url)
        if (url.searchParams.get('purge') === 'true') {
          purged = true
          return HttpResponse.json({ success: true, data: { deleted: true, purged: true } })
        }
        return HttpResponse.json({ success: false, error: 'missing purge param' }, { status: 400 })
      }),
    )
    renderPage()
    expect(await screen.findByText('需清理废弃令牌')).toBeInTheDocument()

    // 点击彻底删除按钮
    const deleteBtn = screen.getByTitle('删除已作废令牌记录')
    await userEvent.click(deleteBtn)

    // 弹出确认弹窗
    expect(screen.getByText('彻底删除令牌记录')).toBeInTheDocument()
    const confirmBtn = screen.getByRole('button', { name: '彻底删除' })
    await userEvent.click(confirmBtn)

    // 确认后调用 DELETE /api/tokens/:id?purge=true 并从页面消失
    await waitFor(() => {
      expect(screen.queryByText('需清理废弃令牌')).toBeNull()
    })
  })
})

it('selecting placeholder must remove the real token from examples', async () => {
  const realToken = 'ihme_AUDIT_FIXTURE_NOT_A_SECRET'
  const token = { id: 'audit_token', name: 'Audit token', token_prefix: 'ihme_AUDIT', created_at: '2026-10-01T00:00:00Z', needs_rotation: false }
  server.use(
    http.get('/api/tags', () => HttpResponse.json({ success: true, data: [] })),
    http.get('/api/tokens', () => HttpResponse.json({ success: true, data: [token] })),
    http.post('/api/tokens', () => HttpResponse.json({ success: true, data: { ...token, token: realToken } })),
  )
  const scrollDescriptor = Object.getOwnPropertyDescriptor(Element.prototype, 'scrollIntoView')
  Object.defineProperty(Element.prototype, 'scrollIntoView', { configurable: true, value: vi.fn() })
  setCSRFToken('csrf-test')
  renderPage()
  const user = userEvent.setup()
  await screen.findByText('Audit token')
  await user.type(screen.getByPlaceholderText('令牌备注 (例: 注册机-01)'), 'New token')
  await user.click(screen.getByRole('button', { name: '生成令牌' }))
  await screen.findByText(realToken)
  await user.selectOptions(screen.getByLabelText('示范令牌'), '__placeholder__')
  const example = document.querySelector('.code-box')!
  expect(example.textContent).not.toContain(realToken)
  expect(example.textContent).toContain('<YOUR_API_TOKEN>')
  await user.click(screen.getByRole('button', { name: 'Python (完整流水线)' }))
  expect(document.querySelector('.code-box')).not.toHaveTextContent(realToken)
  expect(document.querySelector('.code-box')).toHaveTextContent('<YOUR_API_TOKEN>')
  if (scrollDescriptor) Object.defineProperty(Element.prototype, 'scrollIntoView', scrollDescriptor)
  else Reflect.deleteProperty(Element.prototype, 'scrollIntoView')
})


it('编辑保存使用同步锁，保存期间禁止取消、换目标和改写草稿，失败保留输入', async () => {
  const second = { ...tags[0], id: 'tag_2', tag: 'second', name: 'second' }
  let release!: () => void
  const pending = new Promise<void>((resolve) => { release = resolve })
  let calls = 0
  server.use(
    http.get('/api/tags', () => HttpResponse.json({ success: true, data: [...tags, second] })),
    http.get('/api/tokens', () => HttpResponse.json({ success: true, data: [] })),
    http.patch('/api/tags/:id', async () => {
      calls += 1
      await pending
      return HttpResponse.json({ success: false, code: 'TAG_IN_USE', message: '业务标识仍被母号引用' }, { status: 409 })
    }),
  )
  renderPage()
  const user = userEvent.setup()
  await screen.findAllByText('tiktok')
  await user.click(screen.getAllByTitle('编辑标识')[0])
  const dialog = screen.getByRole('dialog', { name: '编辑业务标识' })
  const name = within(dialog).getByLabelText('标识名称')
  await user.clear(name)
  await user.type(name, 'draft')
  const form = name.closest('form')!
  act(() => { fireEvent.submit(form); fireEvent.submit(form) })
  await waitFor(() => expect(calls).toBe(1))
  expect(name).toBeDisabled()
  expect(within(dialog).getByLabelText('描述备注')).toBeDisabled()
  expect(within(dialog).getByRole('button', { name: '取消' })).toBeDisabled()
  await user.keyboard('{Escape}')
  fireEvent.click(document.querySelector('.dialog-backdrop')!)
  fireEvent.click(screen.getAllByTitle('编辑标识')[1])
  expect(name).toHaveValue('draft')
  expect(screen.getByRole('dialog')).toBe(dialog)
  await act(async () => { release() })
  await screen.findByText('业务标识仍被母号引用')
  expect(name).toHaveValue('draft')
  expect(name).toBeEnabled()
  expect(within(dialog).getByRole('button', { name: '保存' })).toBeEnabled()
})

it.each([true, false])('旧编辑响应隔离卸载后新会话（旧请求成功=%s）', async (oldSuccess) => {
  const releases: Array<() => void> = []
  server.use(
    http.get('/api/tags', () => HttpResponse.json({ success: true, data: tags })),
    http.get('/api/tokens', () => HttpResponse.json({ success: true, data: [] })),
    http.patch('/api/tags/:id', async () => {
      const index = releases.length
      await new Promise<void>((resolve) => { releases.push(resolve) })
      if (index === 0 && !oldSuccess) return HttpResponse.json({ success: false, message: '旧请求失败' }, { status: 500 })
      return HttpResponse.json({ success: true, data: tags[0] })
    }),
  )
  const user = userEvent.setup()
  const oldPage = renderPage()
  await screen.findAllByText('tiktok')
  await user.click(screen.getByTitle('编辑标识'))
  await user.click(screen.getByRole('button', { name: '保存' }))
  await waitFor(() => expect(releases).toHaveLength(1))
  oldPage.unmount()
  renderPage()
  await screen.findAllByText('tiktok')
  await user.click(screen.getByTitle('编辑标识'))
  await user.clear(screen.getByLabelText('标识名称'))
  await user.type(screen.getByLabelText('标识名称'), 'new-draft')
  await user.click(screen.getByRole('button', { name: '保存' }))
  await waitFor(() => expect(releases).toHaveLength(2))
  await act(async () => { releases[0](); await new Promise((resolve) => setTimeout(resolve, 30)) })
  expect(screen.getByRole('dialog', { name: '编辑业务标识' })).toBeInTheDocument()
  expect(screen.getByLabelText('标识名称')).toHaveValue('new-draft')
  expect(screen.getByRole('button', { name: '保存中…' })).toBeDisabled()
  expect(screen.queryByText('旧请求失败')).not.toBeInTheDocument()
  expect(screen.queryByText('业务标识 [tiktok] 已更新')).not.toBeInTheDocument()
  await act(async () => { releases[1]() })
  await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
})
