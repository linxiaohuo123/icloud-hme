/**
 * [INPUT]: 依赖 api/client、Dialog、useDialogSession 和 invalidateAccounts
 * [OUTPUT]: 对外提供 AppPasswordDialog，保存期间锁定交互并按账号和打开会话隔离异步结果
 * [POS]: web/src/components 的凭据配置弹窗，用于配置 iCloud App 专用密码
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useEffect, useState } from 'react'
import Dialog from './Dialog'
import { request, ApiError } from '../api/client'
import { invalidateAccounts } from '../hooks/useAccounts'
import { useDialogSession } from '../hooks/useDialogSession'

interface AppPasswordDialogProps {
  accountId: string
  defaultEmail?: string
  open: boolean
  onClose: () => void
  onSaved: () => void
}

/** 设置 App 专用密码对话框:支持邮箱默认预填 */
export default function AppPasswordDialog({
  accountId,
  defaultEmail = '',
  open,
  onClose,
  onSaved,
}: AppPasswordDialogProps) {
  const [email, setEmail] = useState(defaultEmail)
  const [appPassword, setAppPassword] = useState('')
  const [error, setError] = useState('')
  const { busy: submitting, sessionRef, begin, finish } = useDialogSession(open, accountId)

  useEffect(() => {
    if (!open) return
    setEmail(defaultEmail || '')
    setAppPassword('')
    setError('')
  }, [open, accountId, defaultEmail])

  function handleClose() {
    if (sessionRef.current?.pending) return
    setEmail('')
    setAppPassword('')
    setError('')
    onClose()
  }

  async function handleSubmit() {
    if (sessionRef.current?.pending) return
    if (!email.trim() || !appPassword.trim()) {
      setError('请输入邮箱与 App 专用密码')
      return
    }
    const session = begin()
    if (!session) return
    setError('')
    try {
      await request(`/api/accounts/${accountId}/password`, {
        method: 'POST',
        body: JSON.stringify({ icloud_email: email.trim(), app_password: appPassword.trim() }),
      })
      invalidateAccounts(accountId)
      if (!session.active) return
      setEmail('')
      setAppPassword('')
      onSaved()
    } catch (err) {
      if (!session.active) return
      setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
    } finally {
      finish(session)
    }
  }

  return (
    <Dialog
      title="设置 App 专用密码"
      open={open}
      onClose={handleClose}
    >
      {error && (
        <div className="alert-error" role="alert">
          {error}
        </div>
      )}
      <div className="form-field">
        <label htmlFor="apppwd-email">邮箱</label>
        <input
          id="apppwd-email"
          type="email"
          value={email}
          disabled={submitting}
          onChange={(e) => setEmail(e.target.value)}
          placeholder="your_apple_id@icloud.com"
        />
      </div>
      <div className="form-field">
        <label htmlFor="apppwd-value">App 专用密码</label>
        <input
          id="apppwd-value"
          type="password"
          autoComplete="off"
          value={appPassword}
          disabled={submitting}
          onChange={(e) => setAppPassword(e.target.value)}
          placeholder="xxxx-xxxx-xxxx-xxxx"
        />
      </div>
      <div className="form-actions">
        <button type="button" onClick={handleClose} disabled={submitting}>取消</button>
        <button type="button" className="primary" onClick={() => void handleSubmit()} disabled={submitting}>
          {submitting ? '保存中…' : '保存'}
        </button>
      </div>
    </Dialog>
  )
}
