/**
 * [INPUT]: 依赖 react, components/Dialog, components/icons, utils/clipboard, components/ToastProvider 的 useToast
 * [OUTPUT]: 对外提供 DirectLinksDialog 组件，用于展示并一键复制三种对外直出链接
 * [POS]: web/src/components 的对外直出链接生成弹窗
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useState, useEffect } from 'react'
import Dialog from './Dialog'
import { IconCopy, IconExternalLink, IconKey, IconTerminal, IconFileText, IconGlobe } from './icons'
import { copyText } from '../utils/clipboard'
import { useToast } from './ToastProvider'

interface DirectLinksDialogProps {
  open: boolean
  onClose: () => void
  email: string
}

export default function DirectLinksDialog({ open, onClose, email }: DirectLinksDialogProps) {
  const { show, showCopyable } = useToast()
  const [token, setToken] = useState(() => {
    try {
      return localStorage.getItem('icloud_hme_direct_token') || ''
    } catch {
      return ''
    }
  })

  useEffect(() => {
    try {
      localStorage.setItem('icloud_hme_direct_token', token.trim())
    } catch {
      // ignore
    }
  }, [token])

  const origin = typeof window !== 'undefined' ? window.location.origin : ''
  const tokenParam = token.trim() ? `&token=${encodeURIComponent(token.trim())}` : ''

  const jsonUrl = `${origin}/mail/code?email=${encodeURIComponent(email)}${tokenParam}`
  const viewUrl = `${origin}/mail/view?email=${encodeURIComponent(email)}${tokenParam}`
  const rawUrl = `${origin}/mail/raw?email=${encodeURIComponent(email)}${tokenParam}`

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
          针对邮箱 <strong>{email}</strong> 生成可供外部自动化脚本调用的开箱即用直链。
        </p>

        <div className="form-field">
          <label htmlFor="direct-links-token" className="inline-flex-center">
            <IconKey size={14} />
            <span>外部访问令牌 (Token)</span>
          </label>
          <input
            id="direct-links-token"
            type="text"
            value={token}
            onChange={(e) => setToken(e.target.value)}
            placeholder="填写在「业务标识」中生成的 Token (可选，会自动存入本机浏览器)"
          />
          <span className="hint">
            当前浏览器已登录管理员会话时，网页查信直链无需 Token 也可直接点开；给外部脚本或客户使用时请填入 Token。
          </span>
        </div>

        {links.map((link) => (
          <div key={link.key} className="direct-link-item">
            <div className="direct-link-head">
              <div className="direct-link-title">
                {link.icon}
                <span>{link.title}</span>
              </div>
              <div className="direct-link-actions">
                {link.openable && (
                  <a
                    href={link.url}
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
                  onClick={() => void handleCopy(link.url, link.copyLabel)}
                >
                  <IconCopy size={11} />
                  <span>复制链接</span>
                </button>
              </div>
            </div>
            <div className="direct-link-desc">{link.desc}</div>
            <input
              type="text"
              readOnly
              className="font-mono"
              aria-label={link.copyLabel}
              value={link.url}
              onClick={(e) => (e.target as HTMLInputElement).select()}
            />
          </div>
        ))}

        <div className="dialog-actions">
          <button type="button" className="btn btn-secondary" onClick={onClose}>
            关闭
          </button>
        </div>
      </div>
    </Dialog>
  )
}
