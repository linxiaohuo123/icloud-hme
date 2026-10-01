/**
 * [INPUT]: 依赖 react、react-dom (createPortal)、utils/focus 与 ./icons
 * [OUTPUT]: 对外提供带命名触发按钮的 Select 组件及类型，支持键盘选择、Tab 返回表单、Escape/外部滚动/resize 关闭与焦点恢复
 * [POS]: web/src/components 的通用自定义下拉组件，替代原生丑陋的 select/option 控件
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useState, useRef, useEffect, useLayoutEffect, useCallback, useId, type KeyboardEvent } from 'react'
import { createPortal } from 'react-dom'
import { IconChevronDown, IconCheck } from './icons'
import { getFocusableElements } from '../utils/focus'

export interface SelectOption {
  value: string | number
  label: React.ReactNode
  disabled?: boolean
}

export interface SelectProps {
  id?: string
  name?: string
  value: string | number
  onChange: (value: string) => void
  options: SelectOption[]
  placeholder?: string
  disabled?: boolean
  className?: string
  style?: React.CSSProperties
  'aria-label'?: string
  triggerLabel?: string
  size?: 'sm' | 'md'
}

export default function Select({
  id,
  name,
  value,
  onChange,
  options,
  placeholder = '请选择',
  disabled = false,
  className = '',
  style,
  'aria-label': ariaLabel,
  triggerLabel,
  size = 'md',
}: SelectProps) {
  const [open, setOpen] = useState(false)
  const [activeIndex, setActiveIndex] = useState(-1)
  const [pos, setPos] = useState<{ top: number; left: number; width: number } | null>(null)
  const containerRef = useRef<HTMLDivElement>(null)
  const triggerRef = useRef<HTMLButtonElement>(null)
  const menuRef = useRef<HTMLDivElement>(null)
  const optionRefs = useRef<(HTMLDivElement | null)[]>([])
  const menuId = useId()

  const selectedOption = options.find((o) => String(o.value) === String(value))
  const displayLabel = selectedOption ? selectedOption.label : placeholder

  // 计算下拉浮层位置(支持自适应向上翻转与宽度对齐)
  const calcPos = useCallback(() => {
    if (!triggerRef.current) return null
    const rect = triggerRef.current.getBoundingClientRect()
    const itemHeight = size === 'sm' ? 28 : 34
    const menuHeight = Math.min(options.length * itemHeight + 8, 260)
    const spaceBelow = window.innerHeight - rect.bottom
    const openUpward = spaceBelow < menuHeight + 8 && rect.top > menuHeight + 8
    const width = Math.max(Math.round(rect.width), 140)
    const left = Math.min(Math.round(rect.left), Math.max(8, window.innerWidth - width - 8))
    const top = Math.round(openUpward ? rect.top - menuHeight - 4 : rect.bottom + 4)

    return { top, left, width }
  }, [options.length, size])

  const openMenu = (edge?: 'first' | 'last') => {
    if (disabled) return
    const enabledIndexes = options.flatMap((option, index) => option.disabled ? [] : [index])
    const selectedIndex = options.findIndex((option) => !option.disabled && String(option.value) === String(value))
    setActiveIndex(edge === 'last' ? (enabledIndexes.at(-1) ?? -1)
      : edge === 'first' ? (enabledIndexes[0] ?? -1)
        : selectedIndex >= 0 ? selectedIndex : (enabledIndexes[0] ?? -1))
    const nextPos = calcPos()
    if (nextPos) {
      setPos(nextPos)
    }
    setOpen(true)
  }

  const toggleOpen = () => {
    if (open) setOpen(false)
    else openMenu()
  }

  const chooseOption = (index: number) => {
    const option = options[index]
    if (disabled || !option || option.disabled) return
    onChange(String(option.value))
    setOpen(false)
    triggerRef.current?.focus()
  }

  const handleMenuKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    const enabledIndexes = options.flatMap((option, index) => option.disabled ? [] : [index])
    if (['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) {
      event.preventDefault()
      event.stopPropagation()
      if (!enabledIndexes.length) return
      const current = enabledIndexes.indexOf(activeIndex)
      if (event.key === 'Home') setActiveIndex(enabledIndexes[0])
      else if (event.key === 'End') setActiveIndex(enabledIndexes[enabledIndexes.length - 1])
      else {
        const next = (current + (event.key === 'ArrowDown' ? 1 : -1) + enabledIndexes.length) % enabledIndexes.length
        setActiveIndex(enabledIndexes[next])
      }
    } else if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault()
      event.stopPropagation()
      chooseOption(activeIndex)
    }
  }

  useEffect(() => {
    if (!open) return
    const activeOption = optionRefs.current[activeIndex]
    if (activeOption) {
      activeOption.focus()
      activeOption.scrollIntoView?.({ block: 'nearest' })
    } else {
      menuRef.current?.focus()
    }
  }, [open, activeIndex])

  useLayoutEffect(() => {
    if (open) {
      const p = calcPos()
      if (p) {
        setPos(p)
      }
    }
  }, [open, calcPos])

  useEffect(() => {
    if (!open) return

    const handlePointerDown = (e: MouseEvent) => {
      const target = e.target as Node
      if (
        containerRef.current?.contains(target) ||
        menuRef.current?.contains(target)
      ) {
        return
      }
      setOpen(false)
    }

    const handleKeyDown = (e: globalThis.KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.preventDefault()
        e.stopPropagation()
        setOpen(false)
        triggerRef.current?.focus()
      } else if (e.key === 'Tab' && menuRef.current?.contains(e.target as Node)) {
        // Portal 浮层不在表单的 DOM 顺序中，按触发器的位置继续导航。
        e.preventDefault()
        setOpen(false)
        const trigger = triggerRef.current
        if (!trigger) return
        const root = trigger.closest<HTMLElement>('[role="dialog"][aria-modal="true"]') ?? document.body
        const items = getFocusableElements(root)
        const index = items.indexOf(trigger)
        const next = (index + (e.shiftKey ? -1 : 1) + items.length) % items.length
        items[next]?.focus()
      }
    }

    const handleScrollOrResize = (e: Event) => {
      // 若滚动事件发生于下拉菜单自身内部，绝不阻断用户浏览长列表选项
      if (e.target instanceof Node && menuRef.current?.contains(e.target)) {
        return
      }
      if (menuRef.current?.contains(document.activeElement)) {
        triggerRef.current?.focus()
      }
      setOpen(false)
    }

    window.addEventListener('mousedown', handlePointerDown)
    window.addEventListener('keydown', handleKeyDown, true)
    window.addEventListener('scroll', handleScrollOrResize, true)
    window.addEventListener('resize', handleScrollOrResize)

    return () => {
      window.removeEventListener('mousedown', handlePointerDown)
      window.removeEventListener('keydown', handleKeyDown, true)
      window.removeEventListener('scroll', handleScrollOrResize, true)
      window.removeEventListener('resize', handleScrollOrResize)
    }
  }, [open])

  return (
    <div
      ref={containerRef}
      className={`ui-select ${size === 'sm' ? 'ui-select-sm' : ''} ${className}`}
      style={style}
    >
      {/* 隐藏原生 select：满足 DOM 无障碍与端到端测试 query */}
      <select
        id={id}
        name={name}
        aria-label={ariaLabel}
        aria-hidden={triggerLabel ? true : undefined}
        value={value}
        disabled={disabled}
        tabIndex={-1}
        onChange={(e) => onChange(e.target.value)}
        style={{
          position: 'absolute',
          width: '1px',
          height: '1px',
          padding: 0,
          margin: '-1px',
          overflow: 'hidden',
          clip: 'rect(0, 0, 0, 0)',
          whiteSpace: 'nowrap',
          border: 0,
          opacity: 0,
          pointerEvents: 'none',
        }}
      >
        {options.map((opt) => (
          <option key={String(opt.value)} value={opt.value} disabled={opt.disabled}>
            {typeof opt.label === 'string' ? opt.label : String(opt.value)}
          </option>
        ))}
      </select>

      {/* 现代自定义触发器 */}
      <button
        ref={triggerRef}
        id={`${menuId}-trigger`}
        type="button"
        className={`ui-select-trigger ${open ? 'open' : ''}`}
        disabled={disabled}
        aria-haspopup="listbox"
        aria-label={triggerLabel}
        aria-expanded={open}
        aria-controls={open ? menuId : undefined}
        onClick={toggleOpen}
        onKeyDown={(event) => {
          if (['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) {
            event.preventDefault()
            openMenu(event.key === 'Home' ? 'first' : event.key === 'End' ? 'last' : undefined)
          }
        }}
      >
        <span className="ui-select-label">{displayLabel}</span>
        <span className="ui-select-arrow">
          <IconChevronDown size={size === 'sm' ? 12 : 14} />
        </span>
      </button>

      {/* 浮动菜单 Portal */}
      {open &&
        pos &&
        createPortal(
          <div
            ref={menuRef}
            id={menuId}
            className={`ui-select-dropdown ${size === 'sm' ? 'size-sm' : ''}`}
            role="listbox"
            aria-labelledby={`${menuId}-trigger`}
            tabIndex={-1}
            onKeyDown={handleMenuKeyDown}
            style={{
              position: 'fixed',
              top: pos.top,
              left: pos.left,
              width: pos.width,
            }}
          >
            {options.map((opt, index) => {
              const isSelected = String(opt.value) === String(value)
              return (
                <div
                  key={String(opt.value)}
                  ref={(node) => { optionRefs.current[index] = node }}
                  className={`ui-select-option ${isSelected ? 'selected' : ''} ${
                    opt.disabled ? 'disabled' : ''
                  }`}
                  role="option"
                  aria-selected={isSelected}
                  aria-disabled={opt.disabled || undefined}
                  tabIndex={-1}
                  onFocus={() => setActiveIndex(index)}
                  onClick={() => chooseOption(index)}
                  onKeyDown={handleMenuKeyDown}
                >
                  <span className="ui-select-option-label">{opt.label}</span>
                  {isSelected && (
                    <span className="ui-select-option-check">
                      <IconCheck size={14} />
                    </span>
                  )}
                </div>
              )
            })}
          </div>,
          document.body,
        )}
    </div>
  )
}
