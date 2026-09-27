import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { HttpResponse, http } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import { server } from '../test/server'
import BatchEditAliasDialog from './BatchEditAliasDialog'

describe('BatchEditAliasDialog', () => {
  it('keeps completed updates when a later chunk fails', async () => {
    const ids = Array.from({ length: 101 }, (_, index) => `anon_${index}`)
    server.use(http.post('/api/aliases/batch-update', async ({ request }) => {
      const body = await request.json() as { anonymous_ids: string[] }
      if (body.anonymous_ids.length === 100) {
        return HttpResponse.json({
          success: true,
          data: {
            total: 100,
            succeeded: body.anonymous_ids.slice(0, 99),
            failed: body.anonymous_ids.slice(99),
            last_error: '账号会话保存失败，请刷新列表核对',
          },
        })
      }
      return HttpResponse.json(
        { success: false, code: 'UPSTREAM_FAILURE', message: '后续请求失败' },
        { status: 502 },
      )
    }))
    const onSaved = vi.fn()
    render(<BatchEditAliasDialog accountId="acc_1" selectedIds={ids} open onClose={vi.fn()} onSaved={onSaved} />)
    fireEvent.click(screen.getByRole('button', { name: '确认修改 (101 个)' }))

    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce())
    expect(onSaved.mock.calls[0][0]).toEqual(ids.slice(0, 99))
    expect(onSaved.mock.calls[0][2]).toContain('1 个别名修改失败')
    expect(onSaved.mock.calls[0][2]).toContain('账号会话保存失败')
    expect(onSaved.mock.calls[0][2]).toContain('后续批次未完成：后续请求失败')
  })

  it('shows the failed count when every update fails', async () => {
    server.use(http.post('/api/aliases/batch-update', () => HttpResponse.json({
      success: true,
      data: { total: 2, succeeded: [], failed: ['anon_1', 'anon_2'] },
    })))
    const onSaved = vi.fn()
    render(<BatchEditAliasDialog accountId="acc_1" selectedIds={['anon_1', 'anon_2']} open onClose={vi.fn()} onSaved={onSaved} />)
    fireEvent.click(screen.getByRole('button', { name: '确认修改 (2 个)' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('2 个别名修改失败')
    expect(onSaved).not.toHaveBeenCalled()
  })
})
