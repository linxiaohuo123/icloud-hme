import { http, HttpResponse } from 'msw'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { beforeEach, describe, expect, it } from 'vitest'
import UsedAliasesPage from './UsedAliasesPage'
import { server } from '../test/server'
import { setCSRFToken } from '../api/client'
import { ToastProvider } from '../components/ToastProvider'
import type { LeaseRecord } from '../api/types'

const leases: LeaseRecord[] = Array.from({ length: 25 }, (_, i) => ({
  id: `lease_${i + 1}`,
  email: `alias${i + 1}_abc@icloud.com`,
  account_id: `acc_${1000 + i}`,
  tag: i % 2 === 0 ? 'tiktok' : 'walmart',
  // 覆盖三种展示分支：正常映射(已完成)、正常映射(已分配)、未知状态原文兜底
  status: i === 0 ? 'allocated' : i === 1 ? 'weird_state' : 'completed',
  allocated_at: `2026-09-19T${String(23 - (i % 24)).padStart(2, '0')}:30:00Z`,
}))

const tags = [
  { id: 'tag_1', name: 'tiktok', tag: 'tiktok', description: '', status: 'active' },
  { id: 'tag_2', name: 'walmart', tag: 'walmart', description: '', status: 'active' },
]

function leaseHandler() {
  return http.get('/api/leases', ({ request }) => {
    const url = new URL(request.url)
    const alias = (url.searchParams.get('alias') || '').toLowerCase()
    const tag = url.searchParams.get('tag') || ''
    const limit = Number(url.searchParams.get('limit') || 20)
    const offset = Number(url.searchParams.get('offset') || 0)
    let list = leases
    if (alias) {
      list = list.filter(
        (r) => r.email.toLowerCase().includes(alias) || r.account_id.toLowerCase().includes(alias),
      )
    }
    if (tag) {
      list = list.filter((r) => r.tag === tag)
    }
    return HttpResponse.json({
      success: true,
      data: { records: list.slice(offset, offset + limit), total: list.length },
    })
  })
}

function tagsHandler() {
  return http.get('/api/tags', () => HttpResponse.json({ success: true, data: tags }))
}

function renderPage(initialEntry = '/used') {
  return render(
    <MemoryRouter initialEntries={[initialEntry]}>
      <Routes>
        <Route
          path="/used"
          element={
            <ToastProvider>
              <UsedAliasesPage />
            </ToastProvider>
          }
        />
      </Routes>
    </MemoryRouter>,
  )
}

async function waitForRows() {
  await screen.findByText('alias1_abc@icloud.com')
}

describe('UsedAliasesPage', () => {
  beforeEach(() => {
    setCSRFToken('csrf-test')
    server.resetHandlers()
    server.use(leaseHandler(), tagsHandler())
  })

  it('指标卡展示全量口径，状态列按映射显示中文，未知状态原文兜底', async () => {
    renderPage()
    await waitForRows()

    const totalHeader = screen.getByText('累计出号流水').closest('.card-header') as HTMLElement
    expect(within(totalHeader).getByText('25')).toBeInTheDocument()

    // 服务端写 completed → 已完成；i=0 为 allocated → 已分配；第 1 页 20 行中 18 行已完成
    expect(screen.getAllByText('已完成')).toHaveLength(18)
    expect(screen.getByText('已分配')).toBeInTheDocument()
    // 未收录状态绝不误报语义色，原文 + 中性徽章
    expect(screen.getByText('weird_state')).toBeInTheDocument()
  })

  it('支持 ?tag= 深链初始化筛选，切换业务标识后表格随之更新', async () => {
    renderPage('/used?tag=tiktok')
    // 深链筛选下只有 tiktok 流水 (13 条)，且指标卡仍为全量 25
    expect(await screen.findByText('alias1_abc@icloud.com')).toBeInTheDocument()
    expect(screen.getByText('alias25_abc@icloud.com')).toBeInTheDocument()
    const user = userEvent.setup()
    await user.selectOptions(screen.getByLabelText('按业务标识筛选'), 'walmart')
    expect(await screen.findByText('alias2_abc@icloud.com')).toBeInTheDocument()
    expect(screen.queryByText('alias1_abc@icloud.com')).toBeNull()
  })

  it('搜索走服务端检索，分页翻页后展示第二页数据', async () => {
    renderPage()
    await waitForRows()
    const user = userEvent.setup()

    await user.type(screen.getByLabelText('搜索别名邮箱或账号 ID'), 'alias2')
    // alias2 模糊匹配 alias2/20-25 共 7 条；alias25 不在未筛选的第 1 页，可作刷新完成信号
    expect(await screen.findByText('alias25_abc@icloud.com')).toBeInTheDocument()
    expect(screen.queryByText('alias1_abc@icloud.com')).toBeNull()

    // 清空搜索恢复 25 条 → 第 1 页 20 条 → 下一页看第 2 页
    await user.clear(screen.getByLabelText('搜索别名邮箱或账号 ID'))
    await screen.findByText('alias1_abc@icloud.com')
    await user.click(screen.getByRole('button', { name: '下一页' }))
    expect(await screen.findByText('alias25_abc@icloud.com')).toBeInTheDocument()
    expect(screen.queryByText('alias5_abc@icloud.com')).toBeNull()
  })

  it('加载失败显示错误与重试，重试成功后恢复列表', async () => {
    server.use(
      http.get('/api/leases', () =>
        HttpResponse.json(
          { success: false, code: 'UPSTREAM_FAILURE', message: '获取流水失败' },
          { status: 502 },
        ),
      ),
    )
    renderPage()
    expect(await screen.findByRole('alert')).toHaveTextContent('获取流水失败')

    server.use(leaseHandler())
    await userEvent.click(screen.getByRole('button', { name: /重试/ }))
    expect(await screen.findByText('alias1_abc@icloud.com')).toBeInTheDocument()
  })

  it('无数据时空态提供出号引导，且不渲染分页栏', async () => {
    server.use(
      http.get('/api/leases', () =>
        HttpResponse.json({ success: true, data: { records: [], total: 0 } }),
      ),
      http.get('/api/tags', () => HttpResponse.json({ success: true, data: [] })),
    )
    renderPage()
    expect(await screen.findByText('暂无别名领用流水')).toBeInTheDocument()
    const guide = screen.getByRole('link', { name: /前往账号页生成别名/ })
    expect(guide).toHaveAttribute('href', '/accounts')
    expect(screen.queryByRole('button', { name: '下一页' })).toBeNull()
  })

  it('带筛选搜索无结果时提示调整筛选条件，且不出现引导按钮', async () => {
    renderPage('/used?tag=tiktok')
    await waitForRows()
    server.use(
      http.get('/api/leases', () =>
        HttpResponse.json({ success: true, data: { records: [], total: 0 } }),
      ),
    )
    const user = userEvent.setup()
    await user.type(screen.getByLabelText('搜索别名邮箱或账号 ID'), 'zzz')
    expect(await screen.findByText('没有匹配的领用流水，试试调整筛选条件')).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /前往账号页生成别名/ })).toBeNull()
  })

  it('支持切换每页条数(20/50/100)并重新拉取', async () => {
    localStorage.clear()
    renderPage()
    await waitForRows()

    expect(screen.getByText(/共/)).toHaveTextContent('共 25 条流水，当前第 1 / 2 页')

    const user = userEvent.setup()
    const sizeSelect = screen.getByLabelText('每页显示条数')
    await user.selectOptions(sizeSelect, '50')

    await waitFor(() => {
      expect(screen.getByText(/共/)).toHaveTextContent('共 25 条流水，当前第 1 / 1 页')
    })
    expect(localStorage.getItem('icloud_hme_used_alias_page_size')).toBe('50')
  })
})
