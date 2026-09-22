/**
 * [INPUT]: 依赖 api/types (InboxMessage), components/icons (IconCheck, IconCopy, IconKey, IconTrash, IconExternalLink), utils/sniffer (extractOTP, parseSenderInfo, buildSniffContext), utils/date (formatDate, formatFullDate)
 * [OUTPUT]: 对外提供 InboxTableRow 邮件单行数据渲染组件
 * [POS]: web/src/components/inbox 的行级原子展示组件，承载验证码/激活链接高亮、发件人头像、收件别名复制与行级交互
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import type { InboxMessage } from '../../api/types'
import { formatFullDate } from '../../utils/date'
import { buildSniffContext, extractOTP, parseSenderInfo, stripHtml } from '../../utils/sniffer'
import {
  IconCheck,
  IconCopy,
  IconExternalLink,
  IconKey,
  IconTrash,
} from '../icons'

export interface InboxTableRowProps {
  message: InboxMessage
  copiedCode: string | null
  copiedAlias: string | null
  onOpenMessage: (m: InboxMessage) => void
  onCopyCode: (code: string) => void
  onCopyAlias: (alias: string) => void
  onDelete: (m: InboxMessage) => void
}

function formatShortDate(raw: string): string {
  const d = new Date(raw)
  if (Number.isNaN(d.getTime())) return raw
  const now = new Date()
  const isToday = d.toDateString() === now.toDateString()
  const timeStr = `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
  if (isToday) return `今天 ${timeStr}`
  const isThisYear = d.getFullYear() === now.getFullYear()
  const dateStr = `${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
  if (isThisYear) return `${dateStr} ${timeStr}`
  return `${d.getFullYear()}-${dateStr} ${timeStr}`
}

function cleanSnippet(preview?: string): string {
  if (!preview) return ''
  const stripped = preview.includes('<') && preview.includes('>') ? stripHtml(preview) : preview
  const cleaned = stripped.replace(/\s+/g, ' ').trim()
  return cleaned.length > 120 ? cleaned.slice(0, 120) + '…' : cleaned
}

export default function InboxTableRow({
  message: m,
  copiedCode,
  copiedAlias,
  onOpenMessage,
  onCopyCode,
  onCopyAlias,
}: InboxTableRowProps) {
  const otpResult = extractOTP(buildSniffContext(m.subject, m.preview, m.body))
  const code = otpResult?.code
  const magicLink = otpResult?.magicLink
  const sender = parseSenderInfo(m.from)

  return (
    <tr
      className="inbox-row"
      tabIndex={0}
      role="button"
      aria-label={`查看发自 ${sender.name} 的邮件详情`}
      onClick={() => onOpenMessage(m)}
      onKeyDown={(e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault()
          onOpenMessage(m)
        }
      }}
    >
      <td>
        {code ? (
          <button
            type="button"
            className={`badge-code inbox-code-pill${copiedCode === code ? ' copied' : ''}`}
            onClick={(e) => {
              e.stopPropagation()
              onCopyCode(code)
            }}
            title={copiedCode === code ? `已复制验证码: ${code}` : `点击一键复制验证码: ${code}`}
          >
            {copiedCode === code ? <IconCheck size={13} /> : <IconKey size={13} />}
            <span>{code}</span>
            <span className="copy-tag">{copiedCode === code ? '已复制' : '复制'}</span>
          </button>
        ) : magicLink ? (
          <a
            href={magicLink}
            target="_blank"
            rel="noopener noreferrer"
            className="badge-code inbox-link-pill"
            onClick={(e) => e.stopPropagation()}
            title={`点击打开验证链接: ${magicLink}`}
          >
            <IconExternalLink size={13} />
            <span>激活链接</span>
          </a>
        ) : (
          <span className="inbox-no-code text-muted">—</span>
        )}
      </td>
      <td>
        <div className="inbox-content-cell">
          <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginBottom: 2, flexWrap: 'wrap' }}>
            {m.unread && (
              <span className="badge badge-info" style={{ fontSize: 11, padding: '1px 6px', borderRadius: 4 }}>
                未读
              </span>
            )}
            {m.folder && (
              <span
                className={`badge ${m.folder.toLowerCase() === 'junk' ? 'badge-error' : 'badge-neutral'}`}
                style={{ fontSize: 11, padding: '1px 6px', borderRadius: 4 }}
                title={`存储文件夹: ${m.folder}`}
              >
                {m.folder}
              </span>
            )}
            <button
              type="button"
              className="link-button inbox-subject-link"
              onClick={(e) => {
                e.stopPropagation()
                onOpenMessage(m)
              }}
              title={m.subject || '（无主题）'}
            >
              {m.subject || '（无主题）'}
            </button>
          </div>
          {(m.preview || m.body) && (
            <div className="inbox-snippet" title={cleanSnippet(m.preview || m.body)}>
              {cleanSnippet(m.preview || m.body)}
            </div>
          )}
        </div>
      </td>
      <td>
        <div className="sender-cell">
          <div className="sender-avatar" style={{ background: sender.color }}>
            {sender.initial}
          </div>
          <div className="sender-info">
            <span className="sender-name" title={sender.name}>{sender.name}</span>
            {sender.email && (
              <span className="sender-email" title={sender.email}>{sender.email}</span>
            )}
          </div>
        </div>
      </td>
      <td>
        {m.to ? (
          <div className="recipient-chip">
            <span className="recipient-text" title={m.to}>{m.to}</span>
            <button
              type="button"
              className="recipient-copy-btn"
              onClick={(e) => {
                e.stopPropagation()
                onCopyAlias(m.to)
              }}
              title="复制收件别名"
            >
              {copiedAlias === m.to ? (
                <IconCheck size={11} style={{ color: 'var(--color-success)' }} />
              ) : (
                <IconCopy size={11} />
              )}
            </button>
          </div>
        ) : (
          <span className="text-muted text-xs">—</span>
        )}
      </td>
      <td title={formatFullDate(m.date)}>
        <span className="inbox-time-text">{formatShortDate(m.date)}</span>
      </td>
      <td>
        <div className="inbox-row-actions">
          <button
            type="button"
            className="btn-row-action"
            onClick={(e) => {
              e.stopPropagation()
              onOpenMessage(m)
            }}
            title="查看详情"
          >
            详情
          </button>
          {/* 【PR-01 安全止损】物理删信在邮件身份模型与目标 UID 删除支持完善前暂停 */}
          <button
            type="button"
            className="btn-row-action btn-row-action-danger"
            disabled
            style={{ opacity: 0.4, cursor: 'not-allowed' }}
            title="物理删信功能因安全性考量暂不可用，已安全暂停"
            aria-label="删除邮件（已暂停）"
          >
            <IconTrash size={13} />
          </button>
        </div>
      </td>
    </tr>
  )
}
