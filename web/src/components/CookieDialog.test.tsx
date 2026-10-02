import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { HttpResponse, http } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import { server } from '../test/server'
import CookieDialog from './CookieDialog'

describe('CookieDialog', () => {
  it('refreshes account state when invalid cookies were saved', async () => {
    server.use(http.put('/api/accounts/:id/cookies', () => HttpResponse.json(
      { success: false, code: 'COOKIE_SAVED_INVALID', message: 'Cookie 已保存，但校验未通过' },
      { status: 422 },
    )))
    const onSaved = vi.fn()
    const onChanged = vi.fn()
    render(<CookieDialog accountId="acc_1" open onClose={vi.fn()} onSaved={onSaved} onChanged={onChanged} />)
    fireEvent.change(screen.getByLabelText('Cookie'), { target: { value: 'session=invalid' } })
    fireEvent.click(screen.getByRole('button', { name: '保存' }))

    await waitFor(() => expect(onChanged).toHaveBeenCalledOnce())
    expect(onSaved).not.toHaveBeenCalled()
    expect(screen.getByRole('alert')).toHaveTextContent('Cookie 已保存')
  })
})


function deferredSave() {
  let release!: () => void
  const promise = new Promise<void>(resolve => { release = resolve })
  let started = false
  server.use(http.put('/api/accounts/:id/cookies', async () => {
    started = true
    await promise
    return HttpResponse.json({ success: true, data: {} })
  }))
  return { release, started: () => started }
}

it('blocks cancel, Escape, backdrop and editing while saving', async () => {
  const pending = deferredSave()
  const onClose = vi.fn()
  const onSaved = vi.fn()
  render(<CookieDialog accountId="acc_1" open onClose={onClose} onSaved={onSaved} onChanged={vi.fn()} />)
  fireEvent.change(screen.getByLabelText('Cookie'), { target: { value: 'old=value' } })
  fireEvent.click(screen.getByRole('button', { name: '保存' }))
  await waitFor(() => expect(pending.started()).toBe(true))
  expect(screen.getByLabelText('Cookie')).toBeDisabled()
  expect(screen.getByRole('button', { name: '取消' })).toBeDisabled()
  fireEvent.click(screen.getByRole('button', { name: '取消' }))
  fireEvent.keyDown(document, { key: 'Escape' })
  fireEvent.click(document.querySelector('.dialog-backdrop')!)
  expect(onClose).not.toHaveBeenCalled()
  pending.release()
  await waitFor(() => expect(onSaved).toHaveBeenCalledOnce())
})

it.each(['reopen', 'account change', 'unmount'])('ignores stale save callbacks after %s', async mode => {
  const pending = deferredSave()
  const onSaved = vi.fn()
  const onChanged = vi.fn()
  const props = { accountId: 'acc_1', open: true, onClose: vi.fn(), onSaved, onChanged }
  const view = render(<CookieDialog {...props} />)
  fireEvent.change(screen.getByLabelText('Cookie'), { target: { value: 'old=value' } })
  fireEvent.click(screen.getByRole('button', { name: '保存' }))
  await waitFor(() => expect(pending.started()).toBe(true))
  if (mode === 'unmount') view.unmount()
  else {
    if (mode === 'reopen') view.rerender(<CookieDialog {...props} open={false} />)
    view.rerender(<CookieDialog {...props} accountId={mode === 'account change' ? 'acc_2' : 'acc_1'} />)
    fireEvent.change(screen.getByLabelText('Cookie'), { target: { value: 'new=draft' } })
  }
  const updated = vi.fn()
  window.addEventListener('account-updated', updated)
  try {
    await act(async () => { pending.release() })
    // Even a stale form's successful PUT invalidates the captured account.
    await waitFor(() => expect(updated).toHaveBeenCalledOnce())
    expect((updated.mock.calls[0][0] as CustomEvent).detail).toEqual({ accountId: 'acc_1' })
    expect(onSaved).not.toHaveBeenCalled()
    expect(onChanged).not.toHaveBeenCalled()
    if (mode !== 'unmount') expect(screen.getByLabelText('Cookie')).toHaveValue('new=draft')
  } finally {
    window.removeEventListener('account-updated', updated)
  }
})
