/**
 * [INPUT]: 依赖 react, api/client 的 request/ApiError, api/types 的 MailLink, components/Dialog, components/icons, utils/clipboard, utils/date, components/ToastProvider 的 useToast
 * [OUTPUT]: 对外提供 DirectLinksDialog 组件，按有效期向服务端签发单别名只读直链 (不暴露令牌) 并一键复制三种链接，支持作废全部已发链接
 * [POS]: web/src/components 的对外直出链接生成弹窗
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useState, useEffect } from 'react'
import Dialog from './Dialog'
import { IconCopy, IconExternalLink, IconTerminal, IconFileText, IconGlobe } from './icons'
import { copyText } from '../utils/clipboard'
import { formatDate } from '../utils/date'
import { request, ApiError } from '../api/client'
import type { MailLink } from '../api/types'
import { useToast } from './ToastProvider'

interface DirectLinksDialogProps {
  open: boolean
  onClose: () => void
  email: string
}

export default function DirectLinksDialog({ open, onClose, email }: DirectLinksDialogProps) {
  const { show, showCopyable } = useToast()
  const [days, setDays] = useState('30')
  const [link, setLink] = useState<MailLink | null>(null)
  const [busy, setBusy] = useState(false)
  const [confirmRevoke, setConfirmRevoke] = useState(false)

  // 旧版把令牌明文存进 localStorage 并拼进链接，这里顺手清掉残留
  useEffect(() => {
    try {
      localStorage.removeItem('icloud_hme_direct_token')
    } catch {
      // ignore
    }
  }, [])

  useEffect(() => {
    setLink(null)
    setConfirmRevoke(false)
  }, [email, open])

  async function handleIssue() {
    setBusy(true)
    try {
      setLink(await request<MailLink>('/api/mail-links', { method: 'POST', body: { email, days: Number(days) } }))
    } catch (err) {
      show(err instanceof ApiError ? err.message : '生成直链失败')
    } finally {
      setBusy(false)
    }
  }

  async function handleRevoke() {
    setBusy(true)
    try {
      await request('/api/mail-links/revoke', { method: 'POST' })
      setLink(null)
      setConfirmRevoke(false)
      show('已作废全部已发直链')
    } catch (err) {
      show(err instanceof ApiError ? err.message : '作废失败')
    } finally {
      setBusy(false)
    }
  }

  const origin = typeof window !== 'undefined' ? window.location.origin : ''
  const query = link?.query ?? ''
  const jsonUrl = `${origin}/mail/code?${query}`
  const viewUrl = `${origin}/mail/view?${query}`
  const rawUrl = `${origin}/mail/raw?${query}`

  async function handleCopy(url: string, name: string) {
    if (await copyText(url)) {
      show(`${name}已复制到剪贴板`)
    } else {
      showCopyable(url, '复制失败，请手动复制')
    }
  }

  const links = [
    {
      key: 'json',
      icon: <IconTerminal size={14} />,
      title: '1. 程序一行 GET 取验证码 (JSON)',
      desc: '长轮询等待邮件到达并在 0.1 秒内出码，脚本无需携带复杂 Header 头；末尾追加 &raw=1 可直接输出 6 位纯数字文本。',
      url: jsonUrl,
      copyLabel: '取码直链',
      openable: false,
    },
    {
      key: 'view',
      icon: <IconGlobe size={14} />,
      title: '2. 网页可视化查信 (HTML)',
      desc: '在浏览器中直接查看可视化邮件并醒目展示提取到的验证码，安全沙盒渲染。',
      url: viewUrl,
      copyLabel: '网页查信直链',
      openable: true,
    },
    {
      key: 'raw',
      icon: <IconFileText size={14} />,
      title: '3. 邮件纯正文 (Raw Text)',
      desc: '直接输出未经 JSON 转义的邮件纯正文文本，方便爬虫使用正则提取长链接。',
      url: rawUrl,
      copyLabel: '正文纯文本直链',
      openable: false,
    },
  ]

  return (
    <Dialog open={open} onClose={onClose} title="对外直出链接 (开箱即用)" size="lg">
      <div className="dialog-stack">
        <p className="dialog-lead">
          针对邮箱 <strong>{email}</strong> 签发只读直链，供外部脚本或客户直接使用。
        </p>

        <div className="form-field">
          <label htmlFor="direct-links-days">有效期 (天)</label>
          <div className="inline-flex-center">
            <input
              id="direct-links-days"
              type="number"
              min={1}
              max={3650}
              value={days}
              onChange={(e) => setDays(e.target.value)}
            />
            <button type="button" className="btn btn-primary" disabled={busy || !days} onClick={() => void handleIssue()}>
              {link ? '重新生成' : '生成链接'}
            </button>
          </div>
          <span className="hint">
            链接只能读取这一个别名的邮件，不包含任何令牌，可直接交给客户；到期自动失效。
            {link && ` 当前链接有效至 ${formatDate(link.expires_at)}。`}
          </span>
        </div>

        {link && links.map((item) => (
          <div key={item.key} className="direct-link-item">
            <div className="direct-link-head">
              <div className="direct-link-title">
                {item.icon}
                <span>{item.title}</span>
              </div>
              <div className="direct-link-actions">
                {item.openable && (
                  <a
                    href={item.url}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="btn btn-xs btn-ghost"
                  >
                    <IconExternalLink size={11} />
                    <span>打开查信页</span>
                  </a>
                )}
                <button
                  type="button"
                  className="btn btn-xs btn-secondary"
                  onClick={() => void handleCopy(item.url, item.copyLabel)}
                >
                  <IconCopy size={11} />
                  <span>复制链接</span>
                </button>
              </div>
            </div>
            <div className="direct-link-desc">{item.desc}</div>
            <input
              type="text"
              readOnly
              className="font-mono"
              aria-label={item.copyLabel}
              value={item.url}
              onClick={(e) => (e.target as HTMLInputElement).select()}
            />
          </div>
        ))}

        <div className="dialog-actions">
          {confirmRevoke ? (
            <>
              <span className="hint">所有已发出的直链 (含其他别名) 将立即失效，确定？</span>
              <button type="button" className="btn btn-danger" disabled={busy} onClick={() => void handleRevoke()}>
                确认作废
              </button>
              <button type="button" className="btn btn-secondary" onClick={() => setConfirmRevoke(false)}>
                取消
              </button>
            </>
          ) : (
            <>
              <button type="button" className="btn btn-ghost" disabled={busy} onClick={() => setConfirmRevoke(true)}>
                作废全部已发直链
              </button>
              <button type="button" className="btn btn-secondary" onClick={onClose}>
                关闭
              </button>
            </>
          )}
        </div>
      </div>
    </Dialog>
  )
}
