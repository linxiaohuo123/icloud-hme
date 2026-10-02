/**
 * [INPUT]: 依赖 api/client、Dialog/Select、useDialogSession 和 invalidateAccounts
 * [OUTPUT]: 对外提供 AccountFormDialog，挂载时初始化草稿，提交锁与异步结果绑定会话
 * [POS]: AccountsPage 按编辑目标 key 挂载，父页面刷新不重置输入或解除在途保存锁
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useState } from 'react'
import Dialog from './Dialog'
import Select from './Select'
import { request, ApiError } from '../api/client'
import { invalidateAccounts } from '../hooks/useAccounts'
import { useDialogSession } from '../hooks/useDialogSession'

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
  const { busy: submitting, sessionRef, begin, finish } = useDialogSession(open, editing?.id ?? 'new')

  function reset() {
    setName('')
    setIcloudEmail('')
    setHost('icloud.com')
    setCookies('')
    setProxy('')
    setError('')
  }

  async function handleSubmit() {
    if (sessionRef.current?.pending) return
    if (!name.trim()) {
      setError('请输入账号名称')
      return
    }
    if (!icloudEmail.trim()) {
      setError('请输入 iCloud 邮箱')
      return
    }
    const session = begin()
    if (!session) return
    setError('')
    try {
      const saved = await request<{ id: string }>(editing ? `/api/accounts/${editing.id}` : '/api/accounts', {
        method: editing ? 'PATCH' : 'POST',
        body: {
          name: name.trim(), icloud_email: icloudEmail.trim(), host,
          ...(!editing ? { proxy: proxy.trim(), cookies: cookies.trim() } : {}),
        },
      })
      invalidateAccounts(saved.id)
      if (!session.active) return
      reset()
      onSaved()
    } catch (err) {
      if (!session.active) return
      setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
    } finally {
      finish(session)
    }
  }

  function handleClose() {
    if (sessionRef.current?.pending) return
    reset()
    onClose()
  }

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
          disabled={submitting}
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
          disabled={Boolean(editing) || submitting}
        />
      </div>
      <div className="form-field">
        <label htmlFor="acc-host">区域</label>
        <Select
          id="acc-host"
          value={host}
          onChange={setHost}
          disabled={Boolean(editing) || submitting}
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
              disabled={submitting}
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
              disabled={submitting}
              onChange={(e) => setProxy(e.target.value)}
              placeholder="http://user:pass@host:port"
            />
          </div>
        </>
      )}
      <div className="form-actions">
        <button type="button" onClick={handleClose} disabled={submitting}>取消</button>
        <button type="button" className="primary" onClick={() => void handleSubmit()} disabled={submitting}>
          {submitting ? '保存中…' : '保存'}
        </button>
      </div>
    </Dialog>
  )
}
