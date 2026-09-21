/**
 * [INPUT]: 依赖 react 的 context/hooks、icons 的矢量图标、utils/clipboard 的 copyText 复制能力
 * [OUTPUT]: 对外提供 ToastProvider 容器组件与 useToast hook (包含 show 与 showCopyable 接口，内置智能去重队列)
 * [POS]: components 的全局反馈层，为所有页面与弹窗提供原子化通知与防重轻提示
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react'
import { IconCheck, IconCopy } from './icons'
import { copyText } from '../utils/clipboard'

interface ToastContextValue {
  show: (message: string) => void
  showCopyable: (value: string, message?: string) => void
}

const ToastContext = createContext<ToastContextValue | null>(null)

interface ToastItem {
  id: number
  message: string
  copyValue?: string
  duration: number
}

const DEFAULT_DURATION = 4000
const COPYABLE_DURATION = 6000
const HOVER_EXIT_DELAY = 1200

export function useToast(): ToastContextValue {
  const ctx = useContext(ToastContext)
  if (!ctx) {
    throw new Error('useToast 必须在 ToastProvider 内使用')
  }
  return ctx
}

function Toast({ toast, onDismiss }: { toast: ToastItem; onDismiss: (id: number) => void }) {
  const timerRef = useRef<number | null>(null)
  const [copyState, setCopyState] = useState<'idle' | 'copied' | 'failed'>('idle')

  const clearTimer = useCallback(() => {
    if (timerRef.current !== null) {
      window.clearTimeout(timerRef.current)
      timerRef.current = null
    }
  }, [])

  const scheduleDismiss = useCallback(
    (delay: number) => {
      clearTimer()
      timerRef.current = window.setTimeout(() => {
        timerRef.current = null
        onDismiss(toast.id)
      }, delay)
    },
    [clearTimer, onDismiss, toast.id],
  )

  useEffect(() => {
    scheduleDismiss(toast.duration)
    return clearTimer
  }, [clearTimer, scheduleDismiss, toast.duration])

  async function handleCopy() {
    if (!toast.copyValue) return
    if (await copyText(toast.copyValue)) {
      setCopyState('copied')
    } else {
      setCopyState('failed')
    }
  }

  function pauseDismiss() {
    clearTimer()
  }

  function resumeDismiss() {
    scheduleDismiss(HOVER_EXIT_DELAY)
  }

  return (
    <div
      className="toast"
      role="status"
      aria-label={toast.copyValue ? `${toast.message}：${toast.copyValue}` : toast.message}
      onMouseEnter={pauseDismiss}
      onMouseLeave={resumeDismiss}
      onFocusCapture={pauseDismiss}
      onBlurCapture={(event) => {
        if (!event.currentTarget.contains(event.relatedTarget as Node | null)) {
          resumeDismiss()
        }
      }}
    >
      <IconCheck size={16} aria-hidden="true" />
      <div className="toast-content">
        <span>{toast.message}{toast.copyValue ? '：' : ''}</span>
        {toast.copyValue && <code className="toast-copy-value">{toast.copyValue}</code>}
      </div>
      {toast.copyValue && (
        <button
          type="button"
          className="toast-copy"
          onClick={() => void handleCopy()}
          title="复制别名邮箱"
          aria-label="复制"
        >
          <IconCopy size={14} />
          {copyState === 'copied' ? '已复制' : copyState === 'failed' ? '复制失败' : '复制'}
        </button>
      )}
    </div>
  )
}

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<ToastItem[]>([])
  const nextId = useRef(1)

  const enqueue = useCallback((message: string, copyValue?: string) => {
    const id = nextId.current++
    setToasts((prev) => {
      // 智能防重: 若当前已有完全相同的通知正在展示，则忽略重复叠加
      if (prev.some((t) => t.message === message && t.copyValue === copyValue)) {
        return prev
      }
      return [
        ...prev,
        {
          id,
          message,
          copyValue,
          duration: copyValue ? COPYABLE_DURATION : DEFAULT_DURATION,
        },
      ]
    })
  }, [])

  const show = useCallback((message: string) => enqueue(message), [enqueue])
  const showCopyable = useCallback(
    (value: string, message = '别名已创建') => enqueue(message, value),
    [enqueue],
  )
  const dismiss = useCallback((id: number) => {
    setToasts((prev) => prev.filter((toast) => toast.id !== id))
  }, [])

  const value = useMemo(() => ({ show, showCopyable }), [show, showCopyable])

  return (
    <ToastContext.Provider value={value}>
      {children}
      <div className="toast-region" aria-live="polite">
        {toasts.map((t) => (
          <Toast key={t.id} toast={t} onDismiss={dismiss} />
        ))}
      </div>
    </ToastContext.Provider>
  )
}
