/**
 * [INPUT]: 弹窗是否打开、业务目标 ID、React 生命周期
 * [OUTPUT]: useDialogSession，同步提交锁及异步回调的会话所有权
 * [POS]: 业务弹窗共享的请求生命周期；关闭、切目标或卸载后旧请求不可更新新表单
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */
import { useCallback, useLayoutEffect, useRef, useState } from 'react'

interface DialogSession {
  active: boolean
  pending: boolean
}

export function useDialogSession(open: boolean, targetId: string) {
  const sessionRef = useRef<DialogSession | null>(null)
  const [busy, setBusy] = useState(false)
  useLayoutEffect(() => {
    const session = { active: open, pending: false }
    sessionRef.current = session
    setBusy(false)
    return () => { session.active = false }
  }, [open, targetId])

  const begin = useCallback(() => {
    const session = sessionRef.current
    if (!session?.active || session.pending) return null
    session.pending = true
    setBusy(true)
    return session
  }, [])
  const finish = useCallback((session: DialogSession) => {
    if (!session.active) return
    session.pending = false
    setBusy(false)
  }, [])

  return { busy, sessionRef, begin, finish }
}
