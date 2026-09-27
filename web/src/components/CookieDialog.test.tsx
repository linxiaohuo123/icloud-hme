import { fireEvent, render, screen, waitFor } from '@testing-library/react'
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
