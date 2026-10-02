/**
 * [INPUT]: 依赖 api/client 的 request/ApiError，依赖 components/Dialog
 * [OUTPUT]: 对外提供 CookieDialog 对话框组件 (Cookie 文本录入、保存期间锁定表单并隔离旧会话响应)
 * [POS]: web/src/components 的凭据更新弹窗，用于向指定账号提交 iCloud Cookie 字符串
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useEffect, useRef, useState } from 'react'
import Dialog from './Dialog'
import { request, ApiError } from '../api/client'

interface CookieDialogProps {
  accountId: string
  open: boolean
  onClose: () => void
  onSaved: () => void
  onChanged: () => void
}

/** 更新 Cookie 对话框:提交后清空 textarea */
export default function CookieDialog({ accountId, open, onClose, onSaved, onChanged }: CookieDialogProps) {
  const [cookies, setCookies] = useState('')
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const sessionRef = useRef<{ active: boolean } | null>(null)
  const inFlightRef = useRef(false)

  // A parent may replace the account or close/reopen the dialog while PUT is
  // pending. Its result still updates that account, but cannot own a new form.
  useEffect(() => {
    const session = { active: true }
    sessionRef.current = session
    inFlightRef.current = false
    setSubmitting(false)
    setCookies('')
    setError('')
    return () => { session.active = false }
  }, [open, accountId])

  function handleClose() {
    if (inFlightRef.current) return
    setCookies('')
    setError('')
    onClose()
  }

  async function handleSubmit() {
    if (!open || inFlightRef.current) return
    if (!cookies.trim()) {
      setError('请输入 Cookie')
      return
    }
    const session = sessionRef.current
    if (!session?.active) return
    inFlightRef.current = true
    setSubmitting(true)
    setError('')
    try {
      await request(`/api/accounts/${accountId}/cookies`, {
        method: 'PUT',
        body: JSON.stringify({ cookies }),
      })
      if (typeof window !== 'undefined') {
        window.dispatchEvent(new CustomEvent('account-updated', { detail: { accountId } }))
      }
      if (!session.active) return
      setCookies('')
      onSaved()
    } catch (err) {
      if (err instanceof ApiError && err.code === 'COOKIE_SAVED_INVALID') {
        window.dispatchEvent(new CustomEvent('account-updated', { detail: { accountId } }))
        if (session.active) onChanged()
      }
      if (!session.active) return
      setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
    } finally {
      if (session.active) {
        inFlightRef.current = false
        setSubmitting(false)
      }
    }
  }

  return (
    <Dialog
      title="更新 Cookie"
      open={open}
      onClose={handleClose}
    >
      {error && (
        <div className="alert-error" role="alert">
          {error}
        </div>
      )}
      <div className="form-field">
        <label htmlFor="cookie-input">Cookie</label>
        <textarea
          id="cookie-input"
          value={cookies}
          onChange={(e) => setCookies(e.target.value)}
          spellCheck={false}
          disabled={submitting}
          placeholder="支持全格式智能识别：&#10;1. 键值对: key1=value1; key2=value2 (支持带 Cookie: 前缀)&#10;2. JSON 数组: [{'name':'...', 'value':'...'}] (Chrome 插件导出)&#10;3. JSON 对象: {'key': 'value'}&#10;4. Netscape 格式: 制表符分隔的 .txt 文件内容"
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
