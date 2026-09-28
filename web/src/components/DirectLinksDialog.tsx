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

  const jsonUrl = `${origin}/api/verify-code?email=${encodeURIComponent(email)}${tokenParam}`
  const viewUrl = `${origin}/mail/view?email=${encodeURIComponent(email)}${tokenParam}`
  const rawUrl = `${origin}/mail/raw?email=${encodeURIComponent(email)}${tokenParam}`

  async function handleCopy(url: string, name: string) {
    if (await copyText(url)) {
      show(`${name}已复制到剪贴板`)
    } else {
      showCopyable(url, '复制失败，请手动复制')
    }
  }

  return (
    <Dialog open={open} onClose={onClose} title="对外直出链接 (开箱即用)">
      <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
        <div style={{ fontSize: 13, color: 'var(--text-secondary, #94a3b8)', lineHeight: 1.6 }}>
          针对邮箱 <strong style={{ color: 'var(--text-primary, #f1f5f9)' }}>{email}</strong> 生成可供外部自动化脚本调用的开箱即用直链。
        </div>

        <div className="form-field" style={{ margin: 0 }}>
          <label style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 13 }}>
            <IconKey size={14} />
            <span>外部访问令牌 (Token)</span>
          </label>
          <input
            type="text"
            value={token}
            onChange={(e) => setToken(e.target.value)}
            placeholder="填写在「业务标识」中生成的 Token (可选，会自动存入本机浏览器)"
            style={{ width: '100%', fontSize: 13 }}
          />
          <span style={{ fontSize: 12, color: 'var(--text-muted, #64748b)', marginTop: 4 }}>
            💡 当前浏览器已登录管理员会话时，网页查信直链无需 Token 也可直接点开；给外部脚本或客户使用时请填入 Token。
          </span>
        </div>

        <div style={{ display: 'flex', flexDirection: 'column', gap: 14, marginTop: 4 }}>
          {/* 1. 程序取码 (JSON) */}
          <div className="card" style={{ padding: 12, background: 'var(--bg-subtle, rgba(255,255,255,0.02))', border: '1px solid var(--border-color, #334155)', borderRadius: 8 }}>
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 6 }}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 6, fontWeight: 600, fontSize: 13, color: 'var(--text-primary, #f8fafc)' }}>
                <IconTerminal size={14} style={{ color: '#38bdf8' }} />
                <span>1. 程序一行 GET 取验证码 (JSON)</span>
              </div>
              <button
                type="button"
                className="btn btn-xs btn-primary"
                onClick={() => void handleCopy(jsonUrl, '取码直链')}
                style={{ display: 'inline-flex', alignItems: 'center', gap: 4 }}
              >
                <IconCopy size={11} />
                <span>复制链接</span>
              </button>
            </div>
            <div style={{ fontSize: 12, color: 'var(--text-secondary, #94a3b8)', marginBottom: 6 }}>
              长轮询等待邮件到达并在 0.1 秒内出码，脚本无需携带复杂 Header 头。
            </div>
            <input
              type="text"
              readOnly
              value={jsonUrl}
              onClick={(e) => (e.target as HTMLInputElement).select()}
              style={{ width: '100%', fontSize: 12, fontFamily: 'monospace', background: 'var(--bg-input, #0f172a)', border: '1px solid var(--border-subtle, #1e293b)' }}
            />
          </div>

          {/* 2. 网页查信 (HTML) */}
          <div className="card" style={{ padding: 12, background: 'var(--bg-subtle, rgba(255,255,255,0.02))', border: '1px solid var(--border-color, #334155)', borderRadius: 8 }}>
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 6 }}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 6, fontWeight: 600, fontSize: 13, color: 'var(--text-primary, #f8fafc)' }}>
                <IconGlobe size={14} style={{ color: '#34d399' }} />
                <span>2. 网页可视化查信 (HTML)</span>
              </div>
              <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                <a
                  href={viewUrl}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="btn btn-xs btn-ghost"
                  style={{ display: 'inline-flex', alignItems: 'center', gap: 4, textDecoration: 'none' }}
                >
                  <IconExternalLink size={11} />
                  <span>打开查信页</span>
                </a>
                <button
                  type="button"
                  className="btn btn-xs btn-primary"
                  onClick={() => void handleCopy(viewUrl, '网页查信直链')}
                  style={{ display: 'inline-flex', alignItems: 'center', gap: 4 }}
                >
                  <IconCopy size={11} />
                  <span>复制链接</span>
                </button>
              </div>
            </div>
            <div style={{ fontSize: 12, color: 'var(--text-secondary, #94a3b8)', marginBottom: 6 }}>
              在浏览器中直接查看可视化邮件并醒目展示提取到的验证码，安全沙盒渲染。
            </div>
            <input
              type="text"
              readOnly
              value={viewUrl}
              onClick={(e) => (e.target as HTMLInputElement).select()}
              style={{ width: '100%', fontSize: 12, fontFamily: 'monospace', background: 'var(--bg-input, #0f172a)', border: '1px solid var(--border-subtle, #1e293b)' }}
            />
          </div>

          {/* 3. 纯正文 (Raw) */}
          <div className="card" style={{ padding: 12, background: 'var(--bg-subtle, rgba(255,255,255,0.02))', border: '1px solid var(--border-color, #334155)', borderRadius: 8 }}>
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 6 }}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 6, fontWeight: 600, fontSize: 13, color: 'var(--text-primary, #f8fafc)' }}>
                <IconFileText size={14} style={{ color: '#fbbf24' }} />
                <span>3. 邮件纯正文 (Raw Text)</span>
              </div>
              <button
                type="button"
                className="btn btn-xs btn-primary"
                onClick={() => void handleCopy(rawUrl, '正文纯文本直链')}
                style={{ display: 'inline-flex', alignItems: 'center', gap: 4 }}
              >
                <IconCopy size={11} />
                <span>复制链接</span>
              </button>
            </div>
            <div style={{ fontSize: 12, color: 'var(--text-secondary, #94a3b8)', marginBottom: 6 }}>
              直接输出未经 JSON 转义的邮件纯正文文本，方便爬虫使用正则提取长链接。
            </div>
            <input
              type="text"
              readOnly
              value={rawUrl}
              onClick={(e) => (e.target as HTMLInputElement).select()}
              style={{ width: '100%', fontSize: 12, fontFamily: 'monospace', background: 'var(--bg-input, #0f172a)', border: '1px solid var(--border-subtle, #1e293b)' }}
            />
          </div>
        </div>

        <div style={{ display: 'flex', justifyContent: 'flex-end', marginTop: 8 }}>
          <button type="button" className="btn btn-secondary" onClick={onClose}>
            关闭
          </button>
        </div>
      </div>
    </Dialog>
  )
}
