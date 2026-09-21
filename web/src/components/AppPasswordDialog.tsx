/**
 * [INPUT]: 依赖 api/client 的 request/ApiError，依赖 components/Dialog
 * [OUTPUT]: 对外提供 AppPasswordDialog 对话框组件
 * [POS]: web/src/components 的凭据配置弹窗，用于配置 iCloud App 专用密码
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useEffect, useState } from 'react'
import Dialog from './Dialog'
import { request, ApiError } from '../api/client'

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
  const [submitting, setSubmitting] = useState(false)

  useEffect(() => {
    if (!open) return
    setEmail(defaultEmail || '')
    setAppPassword('')
    setError('')
    setSubmitting(false)
  }, [open, defaultEmail])

  async function handleSubmit() {
    if (submitting) return
    if (!email.trim() || !appPassword.trim()) {
      setError('请输入邮箱与 App 专用密码')
      return
    }
    setSubmitting(true)
    setError('')
    try {
      await request(`/api/accounts/${accountId}/password`, {
        method: 'POST',
        body: JSON.stringify({ icloud_email: email.trim(), app_password: appPassword.trim() }),
      })
      setEmail('')
      setAppPassword('')
      onSaved()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog
      title="设置 App 专用密码"
      open={open}
      onClose={() => {
        setEmail('')
        setAppPassword('')
        setError('')
        onClose()
      }}
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
          onChange={(e) => setAppPassword(e.target.value)}
          placeholder="xxxx-xxxx-xxxx-xxxx"
        />
      </div>
      <div className="form-actions">
        <button onClick={onClose}>取消</button>
        <button className="primary" onClick={() => void handleSubmit()} disabled={submitting}>
          {submitting ? '保存中…' : '保存'}
        </button>
      </div>
    </Dialog>
  )
}
