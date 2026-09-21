/**
 * [INPUT]: 依赖 React 基础能力、Dialog 基础容器及 API 客户端
 * [OUTPUT]: 对外提供 EditAliasDialog 别名备注与说明修改对话框
 * [POS]: web/src/components 的别名元数据编辑浮层，被 AccountWorkspace 和 AliasesPage 消费
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useState, useEffect } from 'react'
import Dialog from './Dialog'
import { request, ApiError } from '../api/client'

interface EditAliasDialogProps {
  accountId: string
  alias: {
    anonymousId: string
    email: string
    label: string
    accountId?: string
    account_id?: string
  } | null
  open: boolean
  onClose: () => void
  onSaved: (newLabel: string) => void
}

export default function EditAliasDialog({
  accountId,
  alias,
  open,
  onClose,
  onSaved,
}: EditAliasDialogProps) {
  const [label, setLabel] = useState(alias?.label ?? '')
  const [note, setNote] = useState('')
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)

  // 当选中的别名变化或弹窗打开时同步当前备注
  useEffect(() => {
    if (open && alias) {
      setLabel(alias.label || '')
      setNote('')
      setError('')
    }
  }, [open, alias])

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!alias || submitting) return
    setSubmitting(true)
    setError('')
    const trimmedLabel = label.trim()
    const targetAccId = alias.accountId || alias.account_id || accountId
    try {
      await request<{ anonymous_id: string; label: string }>(
        `/api/aliases/${encodeURIComponent(alias.anonymousId)}`,
        {
          method: 'PATCH',
          body: JSON.stringify({
            account_id: targetAccId,
            label: trimmedLabel,
            note: note.trim(),
          }),
        },
      )
      onSaved(trimmedLabel)
      onClose()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog
      title="修改别名备注"
      open={open}
      onClose={() => {
        if (!submitting) {
          setError('')
          onClose()
        }
      }}
    >
      {error && (
        <div className="alert-error" role="alert" style={{ marginBottom: 12 }}>
          {error}
        </div>
      )}
      <form onSubmit={handleSubmit}>
        <div className="form-field" style={{ marginBottom: 12 }}>
          <label htmlFor="alias-email-readonly">别名邮箱</label>
          <input
            id="alias-email-readonly"
            type="text"
            className="input"
            value={alias?.email ?? ''}
            disabled
            readOnly
            style={{ opacity: 0.7, fontFamily: 'monospace' }}
          />
        </div>

        <div className="form-field" style={{ marginBottom: 12 }}>
          <label htmlFor="alias-label-input">备注名称 (用途标签)</label>
          <input
            id="alias-label-input"
            type="text"
            className="input"
            value={label}
            onChange={(e) => setLabel(e.target.value)}
            placeholder="例如: Twitter注册, OpenAI主号, 游戏账号等"
            maxLength={200}
          />
        </div>

        <div className="form-field" style={{ marginBottom: 16 }}>
          <label htmlFor="alias-note-input">补充说明 (可选)</label>
          <input
            id="alias-note-input"
            type="text"
            className="input"
            value={note}
            onChange={(e) => setNote(e.target.value)}
            placeholder="额外备忘说明，如绑定手机或注册日期"
            maxLength={500}
          />
        </div>

        <div className="dialog-actions" style={{ display: 'flex', justifyContent: 'flex-end', gap: 8 }}>
          <button
            type="button"
            className="btn btn-secondary"
            onClick={onClose}
            disabled={submitting}
          >
            取消
          </button>
          <button
            type="submit"
            className="btn btn-primary"
            disabled={submitting}
          >
            {submitting ? '保存中…' : '保存备注'}
          </button>
        </div>
      </form>
    </Dialog>
  )
}
