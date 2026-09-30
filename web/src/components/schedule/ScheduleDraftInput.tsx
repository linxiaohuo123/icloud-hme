/**
 * [INPUT]: React input 属性、服务端字段值、规范化与异步保存回调
 * [OUTPUT]: ScheduleDraftInput，聚焦保留草稿、失焦保存、结束后回显服务端值
 * [POS]: schedule 配额、模板和时间字段共用的草稿输入，失败不遗留伪已保存状态
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */
import { useRef, useState, type InputHTMLAttributes } from 'react'

interface Props extends Omit<InputHTMLAttributes<HTMLInputElement>, 'value' | 'onChange' | 'onBlur' | 'onFocus'> {
  value: string
  normalize: (raw: string) => string
  onSave: (value: string) => Promise<boolean>
}

export function ScheduleDraftInput({ value, normalize, onSave, disabled, ...props }: Props) {
  const [draft, setDraft] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const pendingRef = useRef(false)
  return <input
    {...props}
    value={draft ?? value}
    disabled={disabled || saving}
    aria-busy={saving}
    onFocus={() => setDraft(value)}
    onChange={(event) => setDraft(event.target.value)}
    onKeyDown={(event) => {
      if (event.key === 'Enter') event.currentTarget.blur()
      props.onKeyDown?.(event)
    }}
    onBlur={async (event) => {
      if (pendingRef.current) return
      const next = normalize(event.target.value)
      if (next === value) { setDraft(null); return }
      pendingRef.current = true
      setDraft(next)
      setSaving(true)
      try { await onSave(next) } finally {
        pendingRef.current = false
        setDraft(null)
        setSaving(false)
      }
    }}
  />
}
