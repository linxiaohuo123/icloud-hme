/**
 * [INPUT]: 依赖 api/client 的 request/ApiError，依赖 components/Dialog, components/Select, components/ConfirmDialog
 * [OUTPUT]: 对外提供 MailboxDialog 外部收件邮箱接入对话框组件
 * [POS]: web/src/components 的业务对话框，用于绑定和校验第三方 IMAP 收件邮箱
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useEffect, useState } from 'react'
import Dialog from './Dialog'
import ConfirmDialog from './ConfirmDialog'
import Select from './Select'
import { request, ApiError } from '../api/client'
import type { MailboxSummary } from '../api/types'

interface MailboxDialogProps {
  accountId: string
  current?: MailboxSummary
  open: boolean
  onClose: () => void
  onSaved: () => void
}

export default function MailboxDialog({ accountId, current, open, onClose, onSaved }: MailboxDialogProps) {
  const [provider, setProvider] = useState(current?.provider || 'qq')
  const [email, setEmail] = useState(current?.email || '')
  const [host, setHost] = useState(current?.imap_host || 'imap.qq.com')
  const [port, setPort] = useState(String(current?.imap_port || 993))
  const [code, setCode] = useState('')
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [confirmRemove, setConfirmRemove] = useState(false)

  useEffect(() => {
    if (!open) return
    setProvider(current?.provider || 'qq')
    setEmail(current?.email || '')
    setHost(current?.imap_host || 'imap.qq.com')
    setPort(String(current?.imap_port || 993))
    setCode('')
    setError('')
    setConfirmRemove(false)
  }, [open, current])

  function changeProvider(value: string) {
    setProvider(value)
    const presets: Record<string, [string, number]> = {
      qq: ['imap.qq.com', 993],
      '163': ['imap.163.com', 993],
      gmail: ['imap.gmail.com', 993],
      outlook: ['outlook.office365.com', 993],
    }
    if (presets[value]) {
      setHost(presets[value][0])
      setPort(String(presets[value][1]))
    }
  }

  async function handleSubmit() {
    if (submitting) return
    const cleanEmail = email.trim()
    const cleanHost = host.trim()
    const cleanCode = code.replace(/\s+/g, '')
    if (current && !cleanCode && (
      cleanEmail.toLowerCase() !== current.email.toLowerCase() ||
      cleanHost.toLowerCase() !== current.imap_host.toLowerCase() ||
      Number(port) !== current.imap_port
    )) {
      setError('更换收件邮箱或服务器时，请输入新授权码')
      return
    }
    setSubmitting(true)
    setError('')
    try {
      await request(`/api/accounts/${accountId}/mailbox`, {
        method: 'PUT',
        body: JSON.stringify({ provider, email: cleanEmail, imap_host: cleanHost, imap_port: Number(port), authorization_code: cleanCode }),
      })
      onSaved()
      if (typeof window !== 'undefined') {
        window.dispatchEvent(new CustomEvent('account-updated', { detail: { accountId } }))
      }
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '收件邮箱接入失败')
    } finally {
      setSubmitting(false)
    }
  }

  async function handleRemove() {
    if (submitting) return
    setSubmitting(true)
    setError('')
    try {
      await request(`/api/accounts/${accountId}/mailbox`, { method: 'DELETE' })
      onSaved()
      if (typeof window !== 'undefined') {
        window.dispatchEvent(new CustomEvent('account-updated', { detail: { accountId } }))
      }
    } catch (err) {
      setConfirmRemove(false)
      setError(err instanceof ApiError ? err.message : '解除收件邮箱绑定失败')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <>
      <Dialog title="接入收件邮箱" open={open} onClose={() => { if (!submitting) onClose() }}>
        {error && <div className="alert-error" role="alert">{error}</div>}
        <div className="form-field">
          <label htmlFor="mailbox-provider">邮箱服务商</label>
          <Select
            id="mailbox-provider"
            value={provider}
            onChange={(val) => changeProvider(val)}
            options={[
              { value: 'qq', label: 'QQ 邮箱' },
              { value: '163', label: '163 邮箱' },
              { value: 'gmail', label: 'Gmail' },
              { value: 'outlook', label: 'Outlook' },
              { value: 'custom', label: '其他' },
            ]}
          />
        </div>
        <div className="form-field">
          <label htmlFor="mailbox-email">收件邮箱</label>
          <input id="mailbox-email" type="email" value={email} onChange={(e) => setEmail(e.target.value)} />
          <small style={{ display: 'block', marginTop: 4, color: 'var(--color-text-secondary)', fontSize: '0.85em' }}>
            请确认此地址是 Apple 隐藏邮箱当前的转发目标；接入操作不会修改 Apple 设置。
          </small>
        </div>
        <div className="form-field"><label htmlFor="mailbox-host">IMAP 服务器</label><input id="mailbox-host" value={host} onChange={(e) => setHost(e.target.value)} /></div>
        <div className="form-field"><label htmlFor="mailbox-port">SSL 端口</label><input id="mailbox-port" type="number" min="1" max="65535" value={port} onChange={(e) => setPort(e.target.value)} /></div>
        <div className="form-field">
          <label htmlFor="mailbox-code">邮箱授权码</label>
          <input
            id="mailbox-code"
            type="password"
            autoComplete="off"
            value={code}
            placeholder={current?.email ? '邮箱和服务器不变时可留空' : '请输入邮箱授权码'}
            onChange={(e) => setCode(e.target.value)}
          />
          <small style={{ display: 'block', marginTop: 4, color: 'var(--color-text-secondary)', fontSize: '0.85em' }}>
            提示：QQ / 163 等邮箱须使用网页设置生成的 IMAP 独立授权码（支持直接粘贴带空格格式）
          </small>
        </div>
        <div className="form-actions">
          {current && <button type="button" className="danger" onClick={() => setConfirmRemove(true)} disabled={submitting}>解绑</button>}
          <button type="button" onClick={onClose} disabled={submitting}>取消</button>
          <button type="button" className="primary" onClick={() => void handleSubmit()} disabled={submitting}>{submitting ? '验证中…' : '验证并接入'}</button>
        </div>
      </Dialog>
      <ConfirmDialog
        title="解绑收件邮箱"
        message="仅移除本系统中的外部 IMAP 配置，不会修改 Apple 的转发目标。若 Apple 仍转发至此邮箱，解绑后将无法在本系统收取这些邮件。"
        confirmLabel="确认解绑"
        open={open && confirmRemove}
        busy={submitting}
        onClose={() => { if (!submitting) setConfirmRemove(false) }}
        onConfirm={() => void handleRemove()}
      />
    </>
  )
}
