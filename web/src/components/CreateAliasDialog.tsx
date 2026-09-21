/**
 * [INPUT]: 依赖 components/Dialog, api/client 的 request/ApiError
 * [OUTPUT]: 对外提供 CreateAliasDialog 单个别名创建弹窗 (带表单校验与异步状态)
 * [POS]: web/src/components 的别名操作层，供工作台别名列表消费
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useState } from 'react'
import Dialog from './Dialog'
import { request, ApiError } from '../api/client'

interface CreateAliasDialogProps {
  accountId: string
  open: boolean
  onClose: () => void
  onCreated: (email: string) => void
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
  const [submitting, setSubmitting] = useState(false)

  async function handleSubmit() {
    if (submitting) return
    if (!label.trim()) {
      setError('请输入标签')
      return
    }
    setSubmitting(true)
    setError('')
    try {
      const data = await request<{ email: string }>('/api/create', {
        method: 'POST',
        body: JSON.stringify({ account_id: accountId, label: label.trim() }),
      })
      setLabel('')
      onCreated(data.email)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog
      title="创建别名"
      open={open}
      onClose={() => {
        setLabel('')
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
        <label htmlFor="alias-label">标签</label>
        <input
          id="alias-label"
          value={label}
          onChange={(e) => setLabel(e.target.value.slice(0, 200))}
          maxLength={200}
          placeholder="例如：购物、订阅"
        />
        <p className="hint">标签最长 200 字符；创建后会自动生成新的隐私邮箱。</p>
      </div>
      <div className="form-actions">
        <button type="button" onClick={onClose}>取消</button>
        <button type="button" className="primary" onClick={() => void handleSubmit()} disabled={submitting}>
          {submitting ? '创建中…' : '创建'}
        </button>
      </div>
    </Dialog>
  )
}
