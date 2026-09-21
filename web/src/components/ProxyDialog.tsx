/**
 * [INPUT]: 依赖 components/Dialog, api/client 的 request 与 ApiError
 * [OUTPUT]: 对外提供 ProxyDialog 代理配置与连通性测速弹窗组件
 * [POS]: web/src/components 的弹窗组件，供 AccountsPage 设置代理并实时探测代理延迟与连通性
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useState } from 'react'
import Dialog from './Dialog'
import { request, ApiError } from '../api/client'

interface ProxyDialogProps {
  accountId: string
  open: boolean
  onClose: () => void
  onSaved: () => void
}

interface CheckResult {
  ok: boolean
  latency_ms: number
  message: string
}

/** 更新代理对话框: 从不回显当前值，支持向 Apple 网关实时连通性探测 */
export default function ProxyDialog({ accountId, open, onClose, onSaved }: ProxyDialogProps) {
  const [proxy, setProxy] = useState('')
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [checking, setChecking] = useState(false)
  const [checkResult, setCheckResult] = useState<CheckResult | null>(null)

  function handleReset() {
    setProxy('')
    setError('')
    setCheckResult(null)
    setChecking(false)
    setSubmitting(false)
  }

  async function handleCheck() {
    const target = proxy.trim()
    if (!target) {
      setError('请输入待测试的代理地址')
      return
    }
    setChecking(true)
    setError('')
    setCheckResult(null)
    try {
      const res = await request<CheckResult>('/api/proxy/check', {
        method: 'POST',
        body: JSON.stringify({ proxy: target }),
      })
      setCheckResult(res)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '代理检测请求失败')
    } finally {
      setChecking(false)
    }
  }

  async function handleSubmit() {
    if (submitting) return
    setSubmitting(true)
    setError('')
    try {
      await request(`/api/accounts/${accountId}/proxy`, {
        method: 'PUT',
        body: JSON.stringify({ proxy: proxy.trim() }),
      })
      handleReset()
      onSaved()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog
      title="设置代理"
      open={open}
      onClose={() => {
        handleReset()
        onClose()
      }}
    >
      {error && (
        <div className="alert-error" role="alert">
          {error}
        </div>
      )}
      <div className="form-field">
        <label htmlFor="proxy-input">代理地址</label>
        <div style={{ display: 'flex', gap: 8 }}>
          <input
            id="proxy-input"
            type="text"
            style={{ flex: 1 }}
            value={proxy}
            onChange={(e) => {
              setProxy(e.target.value)
              setCheckResult(null)
            }}
            placeholder="http://user:pass@host:port 或 socks5://..."
            autoComplete="off"
          />
          <button
            type="button"
            onClick={() => void handleCheck()}
            disabled={checking || !proxy.trim()}
          >
            {checking ? '测试中…' : '测试连接'}
          </button>
        </div>
        <p className="hint">留空并保存可清除代理；出于安全考虑不回显当前值。</p>
        {checkResult && (
          <div className={checkResult.ok ? 'alert-info' : 'alert-error'} style={{ marginTop: 8 }}>
            {checkResult.ok
              ? `✅ 连通正常 (延迟: ${checkResult.latency_ms}ms)`
              : `❌ 连通失败: ${checkResult.message}`}
          </div>
        )}
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
