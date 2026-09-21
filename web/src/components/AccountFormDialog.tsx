/**
 * [INPUT]: 依赖 api/client 的 request/ApiError，依赖 components/Dialog, components/Select
 * [OUTPUT]: 对外提供 AccountFormDialog 账号创建与编辑对话框组件
 * [POS]: web/src/components 的业务对话框，用于添加和修改账号基础信息
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useCallback, useEffect, useState } from 'react'
import Dialog from './Dialog'
import Select from './Select'
import { request, ApiError } from '../api/client'

interface AccountFormDialogProps {
  open: boolean
  onClose: () => void
  onSaved: () => void
  editing?: {
    id: string
    name: string
    icloudEmail: string
    host: string
  } | null
}

/** 添加账号 / 编辑基本信息对话框 */
export default function AccountFormDialog({
  open,
  onClose,
  onSaved,
  editing,
}: AccountFormDialogProps) {
  const [name, setName] = useState(editing?.name ?? '')
  const [icloudEmail, setIcloudEmail] = useState(editing?.icloudEmail ?? '')
  const [host, setHost] = useState(editing?.host ?? 'icloud.com')
  const [cookies, setCookies] = useState('')
  const [proxy, setProxy] = useState('')
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)

  useEffect(() => {
    if (!open) return
    setName(editing?.name ?? '')
    setIcloudEmail(editing?.icloudEmail ?? '')
    setHost(editing?.host ?? 'icloud.com')
    setCookies('')
    setProxy('')
    setError('')
    setSubmitting(false)
  }, [open, editing])

  function reset() {
    setName('')
    setIcloudEmail('')
    setHost('icloud.com')
    setCookies('')
    setProxy('')
    setError('')
    setSubmitting(false)
  }

  async function handleSubmit() {
    if (submitting) return
    if (!name.trim()) {
      setError('请输入账号名称')
      return
    }
    if (!icloudEmail.trim()) {
      setError('请输入 iCloud 邮箱')
      return
    }
    setSubmitting(true)
    setError('')
    try {
      if (editing) {
        await request(`/api/accounts/${editing.id}`, {
          method: 'PATCH',
          body: JSON.stringify({ name: name.trim(), icloud_email: icloudEmail.trim(), host }),
        })
      } else {
        await request('/api/accounts', {
          method: 'POST',
          body: JSON.stringify({
            name: name.trim(),
            icloud_email: icloudEmail.trim(),
            host,
            proxy: proxy.trim(),
            cookies: cookies.trim(),
          }),
        })
      }
      reset()
      onSaved()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
    } finally {
      setSubmitting(false)
    }
  }

  const handleClose = useCallback(() => {
    reset()
    onClose()
  }, [onClose])

  return (
    <Dialog
      title={editing ? '编辑账号' : '添加账号'}
      open={open}
      onClose={handleClose}
    >
      {error && (
        <div className="alert-error" role="alert">
          {error}
        </div>
      )}
      <div className="form-field">
        <label htmlFor="acc-name">名称</label>
        <input
          id="acc-name"
          value={name}
          onChange={(e) => setName(e.target.value)}
          maxLength={64}
        />
      </div>
      <div className="form-field">
        <label htmlFor="acc-email">iCloud 邮箱</label>
        <input
          id="acc-email"
          type="email"
          value={icloudEmail}
          onChange={(e) => setIcloudEmail(e.target.value)}
          disabled={Boolean(editing)}
        />
      </div>
      <div className="form-field">
        <label htmlFor="acc-host">区域</label>
        <Select
          id="acc-host"
          value={host}
          onChange={setHost}
          disabled={Boolean(editing)}
          options={[
            { value: 'icloud.com', label: '全球区 (icloud.com)' },
            { value: 'icloud.com.cn', label: '中国区 (icloud.com.cn)' },
          ]}
        />
      </div>
      {!editing && (
        <>
          <div className="form-field">
            <label htmlFor="acc-cookies">Cookie（可选，原始文本）</label>
            <textarea
              id="acc-cookies"
              value={cookies}
              onChange={(e) => setCookies(e.target.value)}
              spellCheck={false}
              placeholder="支持全格式智能识别：&#10;1. 键值对: key1=value1; key2=value2 (支持带 Cookie: 前缀)&#10;2. JSON 数组: [{'name':'...', 'value':'...'}] (Chrome 插件导出)&#10;3. JSON 对象: {'key': 'value'}&#10;4. Netscape 格式: 制表符分隔的 .txt 文件内容"
            />
            <p className="hint">支持粘贴 Header 字符串、Chrome 插件 JSON 数组或 Netscape 文本，系统自动识别提纯。</p>
          </div>
          <div className="form-field">
            <label htmlFor="acc-proxy">代理（可选）</label>
            <input
              id="acc-proxy"
              type="text"
              value={proxy}
              onChange={(e) => setProxy(e.target.value)}
              placeholder="http://user:pass@host:port"
            />
          </div>
        </>
      )}
      <div className="form-actions">
        <button type="button" onClick={handleClose}>取消</button>
        <button type="button" className="primary" onClick={() => void handleSubmit()} disabled={submitting}>
          {submitting ? '保存中…' : '保存'}
        </button>
      </div>
    </Dialog>
  )
}
