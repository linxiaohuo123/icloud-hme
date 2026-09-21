/**
 * [INPUT]: 依赖 components/icons 的 IconInfo/IconClose
 * [OUTPUT]: 对外提供 ScheduleMacroHintBar 可折叠动态宏变量说明条
 * [POS]: web/src/components/schedule 的占位符提示条，提供 {date}, {time}, {seq}, {account} 快速复制
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { memo } from 'react'
import { IconClose, IconInfo } from '../icons'

interface ScheduleMacroHintBarProps {
  show: boolean
  onToggle: () => void
  onCopy: (macro: string) => void
}

export const ScheduleMacroHintBar = memo(function ScheduleMacroHintBar({
  show,
  onToggle,
  onCopy,
}: ScheduleMacroHintBarProps) {
  if (!show) return null

  return (
    <div className="schedule-hint-bar-slim">
      <div className="schedule-hint-left">
        <span className="schedule-hint-icon-svg">
          <IconInfo size={14} />
        </span>
        <span className="schedule-hint-label">动态占位符（点击复制）：</span>
        <div className="schedule-macro-chips-inline">
          <button
            type="button"
            className="schedule-macro-pill"
            onClick={() => onCopy('{date}')}
            title="点击复制 {date}"
          >
            <code>&#123;date&#125;</code> 年月日
          </button>
          <button
            type="button"
            className="schedule-macro-pill"
            onClick={() => onCopy('{time}')}
            title="点击复制 {time}"
          >
            <code>&#123;time&#125;</code> 时分
          </button>
          <button
            type="button"
            className="schedule-macro-pill"
            onClick={() => onCopy('{seq}')}
            title="点击复制 {seq}"
          >
            <code>&#123;seq&#125;</code> 批次序号
          </button>
          <button
            type="button"
            className="schedule-macro-pill"
            onClick={() => onCopy('{account}')}
            title="点击复制 {account}"
          >
            <code>&#123;account&#125;</code> 账号名
          </button>
        </div>
        <span className="schedule-hint-sample-inline">
          示例：<code>gpt-&#123;date&#125;</code> → <code>gpt-20260920</code>
        </span>
      </div>
      <button
        type="button"
        className="schedule-hint-dismiss-btn"
        onClick={onToggle}
        title="收起此说明条"
      >
        <IconClose size={13} />
        <span>收起</span>
      </button>
    </div>
  )
})
