/**
 * [INPUT]: 依赖 api/types 的 FullMessage, components 下 Dialog 与 icons (IconCheck, IconCopy, IconKey, IconExternalLink), utils 下 clipboard/sniffer (extractOTP, buildSniffContext, parseSenderInfo)/date
 * [OUTPUT]: 对外提供 MailDetailDialog 统一邮件详情弹窗组件 (发件人/别名/时间/多模式正文清洗；OTP 与激活链接嗅探展示)
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
    ? extractOTP(buildSniffContext(detail.subject, undefined, detail.body))
    : null
  const code = otpResult?.code
  const magicLink = otpResult?.magicLink

  async function handleCopyCode(otp: string) {
    const ok = await copyText(otp)
    setCopiedCode(true)
    setTimeout(() => setCopiedCode(false), 1600)
    if (onCopySuccess) {
      onCopySuccess(ok ? `验证码 [${otp}] 已复制` : `验证码：${otp}`)
    }
  }

  async function handleCopyAlias(aliasText: string) {
    await copyText(aliasText)
    setCopiedAlias(true)
    setTimeout(() => setCopiedAlias(false), 1600)
    if (onCopySuccess) {
      onCopySuccess(`别名 [${aliasText}] 已复制`)
    }
  }

  return (
    <Dialog
      open={Boolean(detail || loading)}
      title={detail?.subject || (loading ? '读取邮件中…' : '邮件详情')}
      onClose={onClose}
    >
      {loading && <p className="hint" style={{ padding: '24px 0', textAlign: 'center' }}>读取邮件正文中…</p>}

      {detail && sender && (
        <div className="email-detail-box">
          <div className="email-detail-header">
            <div className="email-detail-sender-row">
              <div
                className="sender-avatar"
                style={{
                  background: sender.color,
                  width: 36,
                  height: 36,
                  fontSize: 14,
                }}
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
                      style={{ fontSize: 11, padding: '1px 6px' }}
                      title={`存储文件夹: ${detail.folder}`}
                    >
                      {detail.folder}
                    </span>
                  )}
                  {detail.body_complete === false && (
                    <span
                      className="badge badge-warning"
                      style={{ fontSize: 11, padding: '1px 6px' }}
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
              <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                <IconKey size={18} style={{ color: 'var(--color-success)' }} />
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
            <div className="email-code-banner" style={{ background: 'color-mix(in srgb, var(--color-primary) 10%, transparent)', borderColor: 'color-mix(in srgb, var(--color-primary) 30%, transparent)' }}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                <IconExternalLink size={18} style={{ color: 'var(--color-primary)' }} />
                <span className="email-code-label">检测到激活验证链接</span>
              </div>
              <a
                href={magicLink}
                target="_blank"
                rel="noopener noreferrer"
                className="btn btn-sm btn-primary"
                style={{ display: 'inline-flex', alignItems: 'center', gap: 4 }}
              >
                <span>打开激活链接</span>
                <IconExternalLink size={13} />
              </a>
            </div>
          )}

          {isHtml && (
            <div style={{ display: 'flex', justifyContent: 'flex-end', marginBottom: -6 }}>
              <button
                type="button"
                className="btn btn-xs btn-ghost"
                style={{ minHeight: 24, padding: '2px 8px', fontSize: 12 }}
                onClick={() => setShowRawHtml((v) => !v)}
              >
                {showRawHtml ? '显示清洗文本' : '显示原始源码'}
              </button>
            </div>
          )}

          {detail.body_complete === false && (
            <div className="hint" style={{ fontSize: 12, marginBottom: 8, color: 'var(--color-warning)' }}>
              ⚠️ 当前邮件为 WebMail 摘要预览，未能获取完整正文。
            </div>
          )}

          {/* 纯文本或清洗文本节点安全转义渲染，杜绝 XSS */}
          <div className="email-body-box mail-body">
            {isHtml && !showRawHtml
              ? stripHtml(detail.body) || '无正文内容'
              : detail.body || '无正文内容'}
          </div>

          <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 8 }}>
            <button type="button" className="btn btn-secondary" onClick={onClose}>
              关闭
            </button>
          </div>
        </div>
      )}
    </Dialog>
  )
}
