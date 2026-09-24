/**
 * [INPUT]: 依赖 api/client 的 request/ApiError，依赖 components/Dialog
 * [OUTPUT]: 对外提供 CookieDialog 对话框组件 (Cookie 文本录入、保存成功自动清空)
 * [POS]: web/src/components 的凭据更新弹窗，用于向指定账号提交 iCloud Cookie 字符串
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useState } from 'react'
import Dialog from './Dialog'
import { request, ApiError } from '../api/client'

interface CookieDialogProps {
  accountId: string
  open: boolean
  onClose: () => void
  onSaved: () => void
}

/** 更新 Cookie 对话框:提交后清空 textarea */
export default function CookieDialog({ accountId, open, onClose, onSaved }: CookieDialogProps) {
  const [cookies, setCookies] = useState('')
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)

  async function handleSubmit() {
    if (submitting) return
    if (!cookies.trim()) {
      setError('请输入 Cookie')
      return
    }
    setSubmitting(true)
    setError('')
    try {
      await request(`/api/accounts/${accountId}/cookies`, {
        method: 'PUT',
        body: JSON.stringify({ cookies }),
      })
      setCookies('')
      onSaved()
      if (typeof window !== 'undefined') {
        window.dispatchEvent(new CustomEvent('account-updated', { detail: { accountId } }))
      }
    } catch (err) {      setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog
      title="更新 Cookie"
      open={open}
      onClose={() => {
        setCookies('')
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
        <label htmlFor="cookie-input">Cookie</label>
        <textarea
          id="cookie-input"
          value={cookies}
          onChange={(e) => setCookies(e.target.value)}
          spellCheck={false}
          placeholder="支持全格式智能识别：&#10;1. 键值对: key1=value1; key2=value2 (支持带 Cookie: 前缀)&#10;2. JSON 数组: [{'name':'...', 'value':'...'}] (Chrome 插件导出)&#10;3. JSON 对象: {'key': 'value'}&#10;4. Netscape 格式: 制表符分隔的 .txt 文件内容"
        />
      </div>
      <div className="form-actions">
        <button type="button" onClick={onClose}>取消</button>
        <button type="button" className="primary" onClick={() => void handleSubmit()} disabled={submitting}>
          {submitting ? '保存中…' : '保存'}
        </button>
      </div>
    </Dialog>
  )
}
