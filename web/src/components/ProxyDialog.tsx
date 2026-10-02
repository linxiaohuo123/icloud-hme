/**
 * [INPUT]: 依赖 Dialog、api/client、useDialogSession、invalidateAccounts 和 AbortController
 * [OUTPUT]: 对外提供 ProxyDialog，检测绑定输入和会话，保存时锁定表单并隔离旧响应
 * [POS]: web/src/components 的弹窗组件，供 AccountsPage 设置代理并实时探测代理延迟与连通性
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useEffect, useRef, useState } from 'react'
import Dialog from './Dialog'
import { request, ApiError } from '../api/client'
import { invalidateAccounts } from '../hooks/useAccounts'
import { useDialogSession } from '../hooks/useDialogSession'

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
  const { busy: submitting, sessionRef, begin, finish } = useDialogSession(open, accountId)
  const [checking, setChecking] = useState(false)
  const [checkResult, setCheckResult] = useState<CheckResult | null>(null)
  const checkRef = useRef<AbortController | null>(null)

  useEffect(() => {
    setProxy('')
    setError('')
    setCheckResult(null)
    setChecking(false)
    return () => {
      checkRef.current?.abort()
      checkRef.current = null
    }
  }, [open, accountId])

  function cancelCheck() {
    checkRef.current?.abort()
    checkRef.current = null
    setChecking(false)
  }

  function handleReset() {
    cancelCheck()
    setProxy('')
    setError('')
    setCheckResult(null)
  }

  function handleClose() {
    if (sessionRef.current?.pending) return
    handleReset()
    onClose()
  }

  async function handleCheck() {
    const session = sessionRef.current
    if (!session?.active || session.pending || checkRef.current) return
    const target = proxy.trim()
    if (!target) {
      setError('请输入待测试的代理地址')
      return
    }
    setChecking(true)
    setError('')
    setCheckResult(null)
    const controller = new AbortController()
    checkRef.current = controller
    const isCurrent = () => session.active && checkRef.current === controller && !controller.signal.aborted
    try {
      const res = await request<CheckResult>('/api/proxy/check', {
        method: 'POST',
        body: JSON.stringify({ proxy: target }),
        signal: controller.signal,
      })
      if (isCurrent()) setCheckResult(res)
    } catch (err) {
      if (isCurrent()) setError(err instanceof ApiError ? err.message : '代理检测请求失败')
    } finally {
      if (isCurrent()) {
        checkRef.current = null
        setChecking(false)
      }
    }
  }

  async function handleSubmit() {
    const session = begin()
    if (!session) return
    cancelCheck()
    setError('')
    try {
      await request(`/api/accounts/${accountId}/proxy`, {
        method: 'PUT',
        body: JSON.stringify({ proxy: proxy.trim() }),
      })
      invalidateAccounts(accountId)
      if (!session.active) return
      handleReset()
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
      title="设置代理"
      open={open}
      onClose={handleClose}
    >
      {error && (
        <div className="alert-error" role="alert">
          {error}
        </div>
      )}
      <div className="form-field">
        <label htmlFor="proxy-input">代理地址</label>
        <div className="input-row">
          <input
            id="proxy-input"
            type="text"
            value={proxy}
            disabled={submitting}
            onChange={(e) => {
              cancelCheck()
              setProxy(e.target.value)
              setCheckResult(null)
              setError('')
            }}
            placeholder="http://user:pass@host:port 或 socks5://..."
            autoComplete="off"
          />
          <button
            type="button"
            onClick={() => void handleCheck()}
            disabled={checking || submitting || !proxy.trim()}
          >
            {checking ? '测试中…' : '测试连接'}
          </button>
        </div>
        <p className="hint">留空并保存可清除代理；出于安全考虑不回显当前值。</p>
        {checkResult && (
          <div className={checkResult.ok ? 'alert-info' : 'alert-error'}>
            {checkResult.ok
              ? `✅ 连通正常 (延迟: ${checkResult.latency_ms}ms)`
              : `❌ 连通失败: ${checkResult.message}`}
          </div>
        )}
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
