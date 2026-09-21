/**
 * [INPUT]: 依赖 api/client (request, ApiError), api/types (AccountSummary, BatchCreateResult), components/Dialog, components/Select, utils/clipboard
 * [OUTPUT]: 对外提供 BatchCreateAliasDialog 批量生成别名弹窗组件
 * [POS]: web/src/components 的交互组件，为 AliasesPage 与 AccountWorkspace 提供 1-5 个别名的高并发原子生成
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useEffect, useState } from 'react'
import Dialog from './Dialog'
import Select from './Select'
import { request, ApiError } from '../api/client'
import type { AccountSummary, BatchCreateResult } from '../api/types'
import { copyText } from '../utils/clipboard'
import { IconCheck, IconCopy } from './icons'

interface BatchCreateAliasDialogProps {
  open: boolean
  accounts: AccountSummary[]
  defaultAccountId?: string
  onClose: () => void
  onSuccess: (result: BatchCreateResult) => void
}

export default function BatchCreateAliasDialog({
  open,
  accounts,
  defaultAccountId,
  onClose,
  onSuccess,
}: BatchCreateAliasDialogProps) {
  const [accountId, setAccountId] = useState('')
  const [count, setCount] = useState(1)
  const [note, setNote] = useState('')
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [result, setResult] = useState<BatchCreateResult | null>(null)
  const [copied, setCopied] = useState(false)

  // 每次弹窗打开或目标账号变化时，重置所有表单状态
  useEffect(() => {
    if (!open) return
    const id = defaultAccountId && defaultAccountId !== 'all'
      ? defaultAccountId : (accounts[0]?.id ?? '')
    setAccountId(id)
    setCount(1)
    setNote('')
    setResult(null)
    setError('')
    setCopied(false)
  }, [open, defaultAccountId, accounts])

  const accountOptions = accounts.map((a) => ({
    value: a.id,
    label: `${a.name || a.real_email} (${a.alias_total ?? 0})`,
  }))

  async function handleSubmit() {
    if (!accountId) {
      setError('请选择所属母账号')
      return
    }
    setLoading(true)
    setError('')
    try {
      const data = await request<BatchCreateResult>('/api/create/batch', {
        method: 'POST',
        body: JSON.stringify({
          account_id: accountId,
          count,
          label_prefix: note.trim(),
        }),
      })
      setResult(data)
      onSuccess(data)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
    } finally {
      setLoading(false)
    }
  }

  function handleClose() {
    setResult(null)
    setError('')
    setCopied(false)
    onClose()
  }

  async function handleCopyAll() {
    if (!result?.created?.length) return
    const text = result.created.map((item) => item.email).join('\n')
    await copyText(text)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  return (
    <Dialog
      title={result ? '别名生成结果' : '批量生成别名'}
      open={open}
      onClose={handleClose}
    >
      {error && (
        <div className="alert-error" role="alert" style={{ marginBottom: 16 }}>
          {error}
        </div>
      )}

      {!result ? (
        <form
          onSubmit={(e) => {
            e.preventDefault()
            void handleSubmit()
          }}
        >
          <div className="form-field">
            <label htmlFor="batch-account">所属母账号</label>
            <Select
              id="batch-account"
              value={accountId}
              onChange={(val) => setAccountId(val)}
              options={accountOptions}
              disabled={loading}
            />
            <p className="hint">新别名将被分配到此母账号的名下。</p>
          </div>

          <div className="form-field">
            <label htmlFor="batch-count">生成数量 (1 - 5)</label>
            <div style={{ display: 'flex', gap: 8, marginTop: 4 }}>
              {[1, 2, 3, 4, 5].map((num) => (
                <button
                  key={num}
                  type="button"
                  className={`btn btn-xs ${count === num ? 'btn-primary' : 'btn-secondary'}`}
                  style={{ flex: 1, minHeight: 32, fontSize: 13 }}
                  onClick={() => setCount(num)}
                  disabled={loading}
                >
                  {num} 个
                </button>
              ))}
            </div>
            <p className="hint">受 Apple 速率风控策略约束，单次批量建议不超过 5 个。</p>
          </div>

          <div className="form-field">
            <label htmlFor="batch-note">备注前缀 (可选)</label>
            <input
              id="batch-note"
              type="text"
              value={note}
              onChange={(e) => setNote(e.target.value.slice(0, 200))}
              placeholder="例如：注册测试、社交订阅"
              maxLength={200}
              disabled={loading}
            />
            <p className="hint">若生成多个别名，将统一附加此前缀作为标签。</p>
          </div>

          <div className="form-actions" style={{ display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 20 }}>
            <button type="button" className="btn btn-secondary" onClick={handleClose} disabled={loading}>
              取消
            </button>
            <button type="submit" className="btn btn-primary" disabled={loading || !accountId}>
              {loading ? '正在向 Apple 申请…' : `生成 ${count} 个别名`}
            </button>
          </div>
        </form>
      ) : (
        <div>
          <div className="alert-info" style={{ marginBottom: 16 }}>
            <span>
              成功申请 <b>{result.created_count}</b> / {result.requested} 个别名
            </span>
          </div>

          {result.created?.length > 0 && (
            <div style={{ marginBottom: 16 }}>
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 8 }}>
                <span style={{ fontSize: 13, fontWeight: 600 }}>生成的邮箱列表：</span>
                <button
                  type="button"
                  className="btn btn-xs btn-secondary"
                  onClick={() => void handleCopyAll()}
                  style={{ display: 'inline-flex', alignItems: 'center', gap: 4 }}
                >
                  {copied ? <IconCheck size={12} /> : <IconCopy size={12} />}
                  <span>{copied ? '已复制' : '一键复制全部'}</span>
                </button>
              </div>
              <div style={{ background: 'var(--color-bg-subtle)', borderRadius: 6, padding: 8, maxHeight: 180, overflowY: 'auto' }}>
                {result.created.map((item, idx) => (
                  <div
                    key={idx}
                    style={{
                      display: 'flex',
                      justifyContent: 'space-between',
                      alignItems: 'center',
                      padding: '4px 8px',
                      fontFamily: 'monospace',
                      fontSize: 13,
                    }}
                  >
                    <span>{item.email}</span>
                    <button
                      type="button"
                      className="link-button"
                      style={{ fontSize: 12 }}
                      onClick={() => void copyText(item.email)}
                    >
                      复制
                    </button>
                  </div>
                ))}
              </div>
            </div>
          )}

          {result.last_error && (
            <div className="alert-error" style={{ marginBottom: 16 }}>
              <p style={{ fontWeight: 600, marginBottom: 4 }}>部分生成失败：</p>
              <p style={{ margin: 0, fontSize: 12 }}>{result.last_error}</p>
            </div>
          )}

          <div className="form-actions" style={{ display: 'flex', justifyContent: 'flex-end', marginTop: 16 }}>
            <button type="button" className="btn btn-primary" onClick={handleClose}>
              完成
            </button>
          </div>
        </div>
      )}
    </Dialog>
  )
}
