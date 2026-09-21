/**
 * [INPUT]: 依赖 React 基础能力，接收 title, open, onClose, children
 * [OUTPUT]: 对外提供可访问的通用 Dialog 弹窗组件
 * [POS]: web/src/components 的弹窗基础容器，被所有业务 Dialog 消费
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useEffect, useRef, type ReactNode } from 'react'

// 模块级弹窗栈:嵌套弹窗(如详情上叠删除确认)时只有最顶层响应 Escape,避免一次按键全关
const dialogStack: symbol[] = []

interface DialogProps {
  title: string
  open: boolean
  onClose: () => void
  children: ReactNode
}

/** 可访问 Dialog:Escape 关闭、焦点圈定、关闭后回到触发按钮 */
export default function Dialog({ title, open, onClose, children }: DialogProps) {
  const ref = useRef<HTMLDivElement>(null)
  const lastFocused = useRef<Element | null>(null)
  const onCloseRef = useRef(onClose)
  useEffect(() => {
    onCloseRef.current = onClose
  }, [onClose])

  // 1. 初次打开时捕获焦点，避免输入过程中焦点被抢占回首个输入框
  useEffect(() => {
    if (!open) return
    lastFocused.current = document.activeElement
    const node = ref.current
    if (node && !node.contains(document.activeElement)) {
      const first = node.querySelector<HTMLElement>(
        'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])',
      )
      first?.focus()
    }
  }, [open])

  // 2. 键盘 Tab 循环焦点与 Escape 监听；锁定背景滚动
  useEffect(() => {
    if (!open) return

    const dialogId = Symbol('dialog')
    dialogStack.push(dialogId)

    const originalOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'

    const node = ref.current
    const focusables = () =>
      node
        ? Array.from(
            node.querySelectorAll<HTMLElement>(
              'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])',
            ),
          ).filter((el) => !el.hasAttribute('disabled'))
        : []

    const handleKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        if (dialogStack[dialogStack.length - 1] !== dialogId) return
        onCloseRef.current()
        return
      }
      if (e.key !== 'Tab') return
      const items = focusables()
      if (items.length === 0) return
      const first = items[0]
      const last = items[items.length - 1]
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault()
        last.focus()
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault()
        first.focus()
      }
    }

    document.addEventListener('keydown', handleKey)
    return () => {
      const idx = dialogStack.indexOf(dialogId)
      if (idx >= 0) dialogStack.splice(idx, 1)
      document.removeEventListener('keydown', handleKey)
      document.body.style.overflow = originalOverflow
      if (lastFocused.current instanceof HTMLElement) {
        lastFocused.current.focus()
      }
    }
  }, [open])

  if (!open) return null

  return (
    <div
      className="dialog-backdrop"
      role="presentation"
      onClick={(e) => {
        if (e.target === e.currentTarget) onCloseRef.current()
      }}
    >
      <div
        ref={ref}
        className="dialog"
        role="dialog"
        aria-modal="true"
        aria-label={title}
      >
        <h3>{title}</h3>
        {children}
      </div>
    </div>
  )
}
