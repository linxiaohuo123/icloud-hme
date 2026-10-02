/**
 * [INPUT]: 依赖 api/types 的 FullMessage, components 下 Dialog 与 icons (IconCheck, IconCopy, IconKey, IconExternalLink), utils 下 clipboard/sniffer (extractOTP, buildSniffContext, parseSenderInfo)/date
 * [OUTPUT]: 对外提供 MailDetailDialog 统一邮件详情弹窗组件 (发件人/别名/时间/多模式正文清洗；OTP 与激活链接嗅探展示；按真实复制结果反馈)
 * [POS]: web/src/components/inbox 的详情展示层，供工作台 Tab 与全局收件箱双入口消费
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useEffect, useState } from 'react'
import type { FullMessage } from '../../api/types'
import Dialog from '../Dialog'
import { IconCheck, IconCopy, IconExternalLink, IconKey } from '../icons'
import { copyText } from '../../utils/clipboard'
import { formatDate, formatFullDate, formatRelativeTime } from '../../utils/date'
import { buildSniffContext, extractOTP, parseSenderInfo, stripHtml } from '../../utils/sniffer'

export interface MailDetailDialogProps {
  detail: FullMessage | null
  loading?: boolean
  onClose: () => void
  onCopySuccess?: (msg: string) => void
}

export default function MailDetailDialog({
  detail,
  loading = false,
  onClose,
  onCopySuccess,
}: MailDetailDialogProps) {
  const [copiedCode, setCopiedCode] = useState(false)
  const [copiedAlias, setCopiedAlias] = useState(false)
  const [showRawHtml, setShowRawHtml] = useState(false)

  useEffect(() => {
    setShowRawHtml(false)
    setCopiedCode(false)
    setCopiedAlias(false)
  }, [detail?.id])

  if (!detail && !loading) return null

  const isHtml = detail?.content_type?.toLowerCase().includes('html') ?? false
  const sender = detail ? parseSenderInfo(detail.from) : null
  const otpResult = detail
    ? extractOTP(buildSniffContext(detail.subject, detail.preview, detail.body))
    : null
  const code = otpResult?.code
  const magicLink = otpResult?.magicLink

  async function handleCopyCode(otp: string) {
    const ok = await copyText(otp)
    setCopiedCode(ok)
    if (ok) setTimeout(() => setCopiedCode(false), 1600)
    if (onCopySuccess) {
      onCopySuccess(ok ? `验证码 [${otp}] 已复制` : `复制失败，请手动复制验证码：${otp}`)
    }
  }

  async function handleCopyAlias(aliasText: string) {
    const ok = await copyText(aliasText)
    setCopiedAlias(ok)
    if (ok) setTimeout(() => setCopiedAlias(false), 1600)
    if (onCopySuccess) {
      onCopySuccess(ok ? `别名 [${aliasText}] 已复制` : '复制失败，请手动复制收件别名')
    }
  }

  return (
    <Dialog
      open={Boolean(detail || loading)}
      title={detail?.subject || (loading ? '读取邮件中…' : '邮件详情')}
      onClose={onClose}
      size="lg"
    >
      {loading && <p className="empty-state">读取邮件正文中…</p>}

      {detail && sender && (
        <div className="email-detail-box">
          <div className="email-detail-header">
            <div className="email-detail-sender-row">
              <div
                className="sender-avatar"
                aria-hidden="true"
              >
                {sender.initial}
              </div>
              <div className="email-detail-sender-info">
                <div className="email-detail-sender-name">
                  <span>{sender.name}</span>
                  {sender.email && (
                    <span className="email-detail-sender-addr">
                      &lt;{sender.email}&gt;
                    </span>
                  )}
                  {detail.folder && (
                    <span
                      className={`badge ${detail.folder.toLowerCase() === 'junk' ? 'badge-error' : 'badge-neutral'}`}
                      title={`存储文件夹: ${detail.folder}`}
                    >
                      {detail.folder}
                    </span>
                  )}
                  {detail.body_complete === false && (
                    <span
                      className="badge badge-warning"
                      title="当前仅提供 WebMail 摘要预览，非完整邮件正文"
                    >
                      正文预览
                    </span>
                  )}
                </div>
                <div className="email-detail-recipient-line">
                  <span className="email-meta-label">收件地址：</span>
                  {detail.to ? (
                    <span className="recipient-chip">
                      <span className="recipient-text" title={detail.to}>{detail.to}</span>
                      <button
                        type="button"
                        className="recipient-copy-btn"
                        onClick={() => void handleCopyAlias(detail.to)}
                        title="复制收件别名"
                      >
                        {copiedAlias ? (
                          <IconCheck size={11} style={{ color: 'var(--color-success)' }} />
                        ) : (
                          <IconCopy size={11} />
                        )}
                      </button>
                    </span>
                  ) : (
                    <span>—</span>
                  )}
                </div>
              </div>
              <div className="email-detail-time">
                <span className="time-relative">{formatRelativeTime(detail.date)}</span>
                <span className="time-full">{formatFullDate(detail.date) || formatDate(detail.date)}</span>
              </div>
            </div>
          </div>

          {code && (
            <div className="email-code-banner">
              <div className="email-code-banner-main">
                <IconKey size={18} className="text-success" />
                <span className="email-code-label">提取到验证码:</span>
                <span className="email-code-value">{code}</span>
              </div>
              <button
                type="button"
                className={`btn btn-sm ${copiedCode ? 'btn-success' : 'btn-primary'}`}
                onClick={() => void handleCopyCode(code)}
              >
                {copiedCode ? <IconCheck size={14} /> : <IconCopy size={14} />}
                <span>{copiedCode ? '已复制' : '复制验证码'}</span>
              </button>
            </div>
          )}

          {!code && magicLink && (
            <div className="email-code-banner is-link">
              <div className="email-code-banner-main">
                <IconExternalLink size={18} className="text-primary" />
                <span className="email-code-label">检测到激活验证链接</span>
              </div>
              <a
                href={magicLink}
                target="_blank"
                rel="noopener noreferrer"
                className="btn btn-sm btn-primary"
              >
                <span>打开激活链接</span>
                <IconExternalLink size={13} />
              </a>
            </div>
          )}

          {isHtml && (
            <div className="email-body-toolbar">
              <button
                type="button"
                className="btn btn-xs btn-ghost"
                onClick={() => setShowRawHtml((v) => !v)}
              >
                {showRawHtml ? '显示清洗文本' : '显示原始源码'}
              </button>
            </div>
          )}

          {detail.body_complete === false && (
            <div className="email-body-note" role="note">
              当前邮件为 WebMail 摘要预览，未能获取完整正文。
            </div>
          )}

          {/* 纯文本或清洗文本节点安全转义渲染，杜绝 XSS */}
          <div className="email-body-box mail-body">
            {isHtml && !showRawHtml
              ? stripHtml(detail.body) || '无正文内容'
              : detail.body || '无正文内容'}
          </div>

          <div className="dialog-actions">
            <button type="button" className="btn btn-secondary" onClick={onClose}>
              关闭
            </button>
          </div>
        </div>
      )}
    </Dialog>
  )
}
