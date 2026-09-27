import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { HttpResponse, http } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import { server } from '../test/server'
import MailboxDialog from './MailboxDialog'

const current = { provider: 'qq', email: 'owner@qq.com', imap_host: 'imap.qq.com', imap_port: 993 }

describe('MailboxDialog', () => {
  it('requires a new authorization code before changing the mailbox endpoint', async () => {
    const onSaved = vi.fn()
    render(<MailboxDialog accountId="acc_1" current={current} open onClose={vi.fn()} onSaved={onSaved} />)
    fireEvent.change(screen.getByLabelText('收件邮箱'), { target: { value: 'other@qq.com' } })
    fireEvent.click(screen.getByRole('button', { name: '验证并接入' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('请输入新授权码')
    expect(onSaved).not.toHaveBeenCalled()
  })

  it('confirms and removes the external mailbox binding', async () => {
    server.use(http.delete('/api/accounts/:id/mailbox', () => HttpResponse.json({ success: true, data: {} })))
    const onSaved = vi.fn()
    render(<MailboxDialog accountId="acc_1" current={current} open onClose={vi.fn()} onSaved={onSaved} />)
    fireEvent.click(screen.getByRole('button', { name: '解绑' }))
    const confirm = screen.getByRole('dialog', { name: '解绑收件邮箱' })
    fireEvent.click(within(confirm).getByRole('button', { name: '确认解绑' }))
    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce())
  })
})
