import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { expect, it, vi } from 'vitest'
import { server } from '../test/server'
import { setCSRFToken } from '../api/client'
import type { AccountSummary } from '../api/types'
import BatchCreateAliasDialog from './BatchCreateAliasDialog'

it.each(['x', '😀'])('limits batch prefixes to 100 Unicode characters: %s', async char => {
  setCSRFToken('csrf-test')
  let body: Record<string, unknown> | undefined
  server.use(http.post('/api/create/batch', async ({ request }) => {
    body = await request.json() as Record<string, unknown>
    return HttpResponse.json({ success: true, data: { created: [], created_count: 0, requested: 1 } })
  }))
  const account = { id: 'acc_1', name: 'Fixture', real_email: 'fixture@example.com' } as AccountSummary
  render(<BatchCreateAliasDialog open accounts={[account]} onClose={vi.fn()} onSuccess={vi.fn()} />)
  const input = screen.getByLabelText('备注前缀 (可选)')
  fireEvent.change(input, { target: { value: char.repeat(100) } })
  expect(input).toHaveValue(char.repeat(100))
  fireEvent.change(input, { target: { value: char.repeat(101) } })
  expect(input).toHaveValue(char.repeat(100))
  await userEvent.setup().click(screen.getByRole('button', { name: '生成 1 个别名' }))
  await waitFor(() => expect(body?.label_prefix).toBe(char.repeat(100)))
  await screen.findByText('别名生成结果')
})
