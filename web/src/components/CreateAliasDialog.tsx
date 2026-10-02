/**
 * [INPUT]: 依赖 components/Dialog, api/client、useDialogSession 和 invalidateAccounts
 * [OUTPUT]: 对外提供 CreateAliasDialog，提交时锁定表单，旧会话只使对应账号缓存失效
 * [POS]: web/src/components 的别名操作层，供工作台别名列表消费
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useEffect, useState } from 'react'
import Dialog from './Dialog'
import { request, ApiError } from '../api/client'
import { invalidateAccounts } from '../hooks/useAccounts'
import { useDialogSession } from '../hooks/useDialogSession'

interface CreateAliasDialogProps {
  accountId: string
  open: boolean
  onClose: () => void
  onCreated: (email: string, auditRecorded: boolean) => void
}

/** 创建别名对话框 */
export default function CreateAliasDialog({
  accountId,
  open,
  onClose,
  onCreated,
}: CreateAliasDialogProps) {
  const [label, setLabel] = useState('')
  const [error, setError] = useState('')
  const { busy: submitting, sessionRef, begin, finish } = useDialogSession(open, accountId)
  useEffect(() => {
    setLabel('')
    setError('')
  }, [open, accountId])

  function handleClose() {
    if (sessionRef.current?.pending) return
    setLabel('')
    setError('')
    onClose()
  }

  async function handleSubmit() {
    if (sessionRef.current?.pending) return
    if (!label.trim()) {
      setError('请输入标签')
      return
    }
    const session = begin()
    if (!session) return
    setError('')
    try {
      const data = await request<{ email: string; audit_recorded?: boolean }>('/api/create', {
        method: 'POST',
        body: JSON.stringify({ account_id: accountId, label: label.trim() }),
      })
      invalidateAccounts(accountId)
      if (!session.active) return
      setLabel('')
      onCreated(data.email, data.audit_recorded !== false)
    } catch (err) {
      if (!session.active) return
      setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
    } finally {
      finish(session)
    }
  }

  return (
    <Dialog
      title="创建别名"
      open={open}
      onClose={handleClose}
    >
      {error && (
        <div className="alert-error" role="alert">
          {error}
        </div>
      )}
      <div className="form-field">
        <label htmlFor="alias-label">标签</label>
        <input
          id="alias-label"
          value={label}
          disabled={submitting}
          onChange={(e) => setLabel(e.target.value.slice(0, 200))}
          maxLength={200}
          placeholder="例如：购物、订阅"
        />
        <p className="hint">标签最长 200 字符；创建后会自动生成新的隐私邮箱。</p>
      </div>
      <div className="form-actions">
        <button type="button" onClick={handleClose} disabled={submitting}>取消</button>
        <button type="button" className="primary" onClick={() => void handleSubmit()} disabled={submitting}>
          {submitting ? '创建中…' : '创建'}
        </button>
      </div>
    </Dialog>
  )
}
