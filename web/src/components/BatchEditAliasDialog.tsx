/**
 * [INPUT]: 依赖 React 基础能力、Dialog 基础容器及 API 客户端
 * [OUTPUT]: 对外提供 BatchEditAliasDialog 批量修改别名备注与说明对话框
 * [POS]: web/src/components 的别名批量操作浮层，被 AccountWorkspace 和 AliasesPage 消费
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useState, useEffect } from 'react'
import Dialog from './Dialog'
import { request, ApiError } from '../api/client'

interface BatchEditAliasDialogProps {
  accountId: string
  selectedIds: string[]
  aliases?: Array<{ anonymousId: string; accountId?: string; account_id?: string }>
  open: boolean
  onClose: () => void
  onSaved: (succeededIds: string[], newLabel: string) => void
}

interface BatchUpdateResponse {
  total: number
  succeeded: string[]
  failed: string[]
}

export default function BatchEditAliasDialog({
  accountId,
  selectedIds,
  aliases,
  open,
  onClose,
  onSaved,
}: BatchEditAliasDialogProps) {
  const [label, setLabel] = useState('')
  const [note, setNote] = useState('')
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)

  useEffect(() => {
    if (open) {
      setLabel('')
      setNote('')
      setError('')
    }
  }, [open])

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (selectedIds.length === 0 || submitting) return

    setSubmitting(true)
    setError('')
    const trimmedLabel = label.trim()
    const trimmedNote = note.trim()

    try {
      const allSucceeded: string[] = []

      // 辅助函数：按 100 上限分批提交单个账号下的别名
      async function processAccountBatch(targetAcc: string, ids: string[]) {
        const chunkSize = 100
        for (let i = 0; i < ids.length; i += chunkSize) {
          const slice = ids.slice(i, i + chunkSize)
          const res = await request<BatchUpdateResponse>('/api/aliases/batch-update', {
            method: 'POST',
            body: JSON.stringify({
              account_id: targetAcc,
              anonymous_ids: slice,
              label: trimmedLabel,
              note: trimmedNote,
            }),
          })
          if (res.succeeded) allSucceeded.push(...res.succeeded)
        }
      }

      // 若在全局聚合模式或传入了带账号信息的别名列表，按所属母账号分组发起批量更新
      if (aliases && aliases.length > 0 && (!accountId || accountId === 'all')) {
        const idToAcc = new Map<string, string>()
        for (const a of aliases) {
          const acc = a.accountId || a.account_id
          if (acc) idToAcc.set(a.anonymousId, acc)
        }
        const accGroups = new Map<string, string[]>()
        for (const id of selectedIds) {
          const acc = idToAcc.get(id) || (accountId !== 'all' ? accountId : '')
          if (acc) {
            if (!accGroups.has(acc)) accGroups.set(acc, [])
            accGroups.get(acc)!.push(id)
          }
        }
        for (const [grpAcc, grpIds] of accGroups.entries()) {
          await processAccountBatch(grpAcc, grpIds)
        }
      } else {
        await processAccountBatch(accountId, selectedIds)
      }

      if (allSucceeded.length > 0) {
        onSaved(allSucceeded, trimmedLabel)
      } else {
        setError('批量修改未成功，请检查账号凭据有效性或网络连接')
      }
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '批量更新别名失败')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog
      open={open}
      onClose={submitting ? () => {} : onClose}
      title="批量修改别名备注"
    >
      <form onSubmit={handleSubmit}>
        <div
          style={{
            background: 'var(--color-bg-subtle)',
            padding: '10px 14px',
            borderRadius: 'var(--radius)',
            marginBottom: 16,
            fontSize: 13,
            color: 'var(--color-text-secondary)',
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'space-between',
          }}
        >
          <span>已选中待修改别名：</span>
          <span
            style={{
              fontWeight: 700,
              color: 'var(--color-primary)',
              background: 'var(--color-primary-soft, rgba(0, 113, 227, 0.1))',
              padding: '2px 8px',
              borderRadius: 6,
            }}
          >
            {selectedIds.length} 个{selectedIds.length > 100 ? ` (自动分 ${Math.ceil(selectedIds.length / 100)} 批执行)` : ''}
          </span>
        </div>

        {error && (
          <div className="alert-error" role="alert" style={{ marginBottom: 16 }}>
            {error}
          </div>
        )}

        <div className="form-field" style={{ marginBottom: 12 }}>
          <label htmlFor="batch-alias-label-input">统一备注名称 (用途标签)</label>
          <input
            id="batch-alias-label-input"
            type="text"
            className="input"
            value={label}
            onChange={(e) => setLabel(e.target.value)}
            placeholder="例如: Twitter注册批次, OpenAI主号, 游戏账号等"
            maxLength={200}
          />
        </div>

        <div className="form-field" style={{ marginBottom: 16 }}>
          <label htmlFor="batch-alias-note-input">统一补充说明 (可选)</label>
          <input
            id="batch-alias-note-input"
            type="text"
            className="input"
            value={note}
            onChange={(e) => setNote(e.target.value)}
            placeholder="统一填写的备忘说明 (留空则不设置)"
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
            disabled={submitting || selectedIds.length === 0}
          >
            {submitting ? '正在批量向 Apple 提交...' : `确认修改 (${selectedIds.length} 个)`}
          </button>
        </div>
      </form>
    </Dialog>
  )
}
