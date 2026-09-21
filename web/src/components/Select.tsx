/**
 * [INPUT]: 依赖 react (useState, useRef, useEffect, useLayoutEffect), react-dom (createPortal), ./icons (IconChevronDown, IconCheck)
 * [OUTPUT]: 对外提供 Select 组件及 SelectOption, SelectProps 类型
 * [POS]: web/src/components 的通用自定义下拉组件，替代原生丑陋的 select/option 控件
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useState, useRef, useEffect, useLayoutEffect, useCallback } from 'react'
import { createPortal } from 'react-dom'
import { IconChevronDown, IconCheck } from './icons'

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
  size = 'md',
}: SelectProps) {
  const [open, setOpen] = useState(false)
  const [pos, setPos] = useState<{ top: number; left: number; width: number } | null>(null)
  const containerRef = useRef<HTMLDivElement>(null)
  const triggerRef = useRef<HTMLButtonElement>(null)
  const menuRef = useRef<HTMLDivElement>(null)

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

  const toggleOpen = () => {
    if (disabled) return
    if (open) {
      setOpen(false)
      return
    }
    const nextPos = calcPos()
    if (nextPos) {
      setPos(nextPos)
    }
    setOpen(true)
  }

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

    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        setOpen(false)
        triggerRef.current?.focus()
      }
    }

    const handleScrollOrResize = (e: Event) => {
      // 若滚动事件发生于下拉菜单自身内部，绝不阻断用户浏览长列表选项
      if (menuRef.current && menuRef.current.contains(e.target as Node)) {
        return
      }
      setOpen(false)
    }

    window.addEventListener('mousedown', handlePointerDown)
    window.addEventListener('keydown', handleKeyDown)
    window.addEventListener('scroll', handleScrollOrResize, true)
    window.addEventListener('resize', handleScrollOrResize)

    return () => {
      window.removeEventListener('mousedown', handlePointerDown)
      window.removeEventListener('keydown', handleKeyDown)
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
        type="button"
        className={`ui-select-trigger ${open ? 'open' : ''}`}
        disabled={disabled}
        aria-haspopup="listbox"
        aria-expanded={open}
        onClick={toggleOpen}
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
            className={`ui-select-dropdown ${size === 'sm' ? 'size-sm' : ''}`}
            role="listbox"
            style={{
              position: 'fixed',
              top: pos.top,
              left: pos.left,
              width: pos.width,
            }}
          >
            {options.map((opt) => {
              const isSelected = String(opt.value) === String(value)
              return (
                <div
                  key={String(opt.value)}
                  className={`ui-select-option ${isSelected ? 'selected' : ''} ${
                    opt.disabled ? 'disabled' : ''
                  }`}
                  role="option"
                  aria-selected={isSelected}
                  tabIndex={0}
                  onClick={() => {
                    if (opt.disabled) return
                    onChange(String(opt.value))
                    setOpen(false)
                    triggerRef.current?.focus()
                  }}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' || e.key === ' ') {
                      e.preventDefault()
                      if (opt.disabled) return
                      onChange(String(opt.value))
                      setOpen(false)
                      triggerRef.current?.focus()
                    }
                  }}
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
