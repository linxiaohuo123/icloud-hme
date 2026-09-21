/**
 * [INPUT]: 依赖 api/client (request, ApiError), api/types, components (AsyncState, ConfirmDialog, Select, ToastProvider), utils (clipboard, date, sniffer: extractVerifyCode, parseSenderInfo, buildSniffContext), ./InboxTableRow, ./MailDetailDialog
 * [OUTPUT]: 对外提供 InboxTableView 收件箱表格与筛选核心组件；正文预取 POST /api/messages 使用 uid 并消费 data.messages
 * [POS]: web/src/components/inbox 的核心视图容器，统一单账号工作台与全局收件箱大盘的数据流与交互
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useEffect, useMemo, useRef, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { request, ApiError } from '../../api/client'
import type { AccountSummary, Alias, FullMessage, InboxMessage, InboxResult, MailboxFolder } from '../../api/types'
import AsyncState from '../AsyncState'
import ConfirmDialog from '../ConfirmDialog'
import Select from '../Select'
import { useToast } from '../ToastProvider'
import { copyText } from '../../utils/clipboard'
import { dateTimestamp } from '../../utils/date'
import { buildSniffContext, extractVerifyCode, parseSenderInfo } from '../../utils/sniffer'
import InboxTableRow from './InboxTableRow'
import MailDetailDialog from './MailDetailDialog'
import {
  IconAccounts,
  IconKey,
  IconMail,
  IconRefresh,
  IconSearch,
} from '../icons'

export interface InboxTableViewProps {
  accountId?: string
  fixedAccount?: boolean
  initialAlias?: string
  onCopySuccess?: (msg: string) => void
  showPageHeader?: boolean
  onCountChange?: (count: number) => void
}

export default function InboxTableView({
  accountId: propAccountId,
  fixedAccount = false,
  initialAlias = '',
  onCopySuccess,
  showPageHeader = false,
  onCountChange,
}: InboxTableViewProps) {
  const [searchParams, setSearchParams] = useSearchParams()
  const { show } = useToast()

  const [accounts, setAccounts] = useState<AccountSummary[]>([])
  const [accountId, setAccountId] = useState(propAccountId || '')
  const [aliases, setAliases] = useState<Alias[]>([])
  const [folders, setFolders] = useState<MailboxFolder[]>([])

  const [alias, setAlias] = useState(initialAlias)
  const [folder, setFolder] = useState('all')
  const [limit, setLimit] = useState(20)
  const [days, setDays] = useState(7)

  // 客户端分页
  const [page, setPage] = useState(1)
  const pageSize = 20

  const [result, setResult] = useState<InboxResult | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [retryKey, setRetryKey] = useState(0)

  // 邮件详情弹窗
  const [detail, setDetail] = useState<FullMessage | null>(null)
  const [detailLoading, setDetailLoading] = useState(false)

  // 删除确认
  const [deleteFor, setDeleteFor] = useState<InboxMessage | null>(null)
  const [deleting, setDeleting] = useState(false)

  // 验证码与别名复制反馈
  const [copiedCode, setCopiedCode] = useState<string | null>(null)
  const [copiedAlias, setCopiedAlias] = useState<string | null>(null)
  const copiedTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const copiedAliasTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  const abortRef = useRef<AbortController | null>(null)
  const messageCacheRef = useRef<Map<string, FullMessage>>(new Map())
  const hasAccountsLoadedRef = useRef(false)

  useEffect(() => {
    return () => {
      if (copiedTimerRef.current) clearTimeout(copiedTimerRef.current)
      if (copiedAliasTimerRef.current) clearTimeout(copiedAliasTimerRef.current)
    }
  }, [])

  // 监听外部 propAccountId 变更（工作台模式）
  useEffect(() => {
    if (propAccountId) {
      setAccountId(propAccountId)
    }
  }, [propAccountId])

  // 监听外部 initialAlias 变更（工作台别名联动）
  useEffect(() => {
    if (initialAlias !== undefined) {
      setAlias(initialAlias)
    }
  }, [initialAlias])

  // 1. 初始化账号列表（仅在非固定账号模式，或账号为空时拉取一次，防止 searchParams 诱发无限重拉）
  useEffect(() => {
    if (hasAccountsLoadedRef.current) return
    let cancelled = false
    request<AccountSummary[]>('/api/accounts')
      .then((data) => {
        if (cancelled) return
        hasAccountsLoadedRef.current = true
        setAccounts(data)
        if (!fixedAccount) {
          const queryId = searchParams.get('account_id')
          const valid = data.find((a) => a.id === queryId)
          const target = valid ? valid.id : data[0]?.id ?? ''
          setAccountId(target)
          if (target) {
            const next: Record<string, string> = { account_id: target }
            const qAlias = searchParams.get('alias')
            if (qAlias) {
              setAlias(qAlias)
              next.alias = qAlias
            }
            const qFolder = searchParams.get('folder')
            if (qFolder) {
              setFolder(qFolder)
              next.folder = qFolder
            }
            const qLimit = searchParams.get('limit')
            if (qLimit) {
              setLimit(Number(qLimit) || 20)
              next.limit = qLimit
            }
            const qDays = searchParams.get('days')
            if (qDays) {
              setDays(Number(qDays) || 7)
              next.days = qDays
            }
            setSearchParams(next, { replace: true })
          }
        }
        if (data.length === 0 && !fixedAccount) {
          setLoading(false)
        }
      })
      .catch((err) => {
        if (cancelled) return
        setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
        setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [fixedAccount, retryKey, searchParams, setSearchParams])

  // 2. 账号变化时拉取别名列表
  useEffect(() => {
    if (!accountId) return
    let cancelled = false
    request<{ account_id: string; count: number; aliases: Alias[] }>(
      `/api/aliases?account_id=${encodeURIComponent(accountId)}`,
    )
      .then((data) => {
        if (cancelled) return
        setAliases(data.aliases ?? [])
      })
      .catch(() => {
        if (cancelled) return
        setAliases([])
      })
    return () => {
      cancelled = true
    }
  }, [accountId])

  // 3. 账号变化时拉取文件夹列表 (供 INBOX/Junk 筛选)
  useEffect(() => {
    if (!accountId) return
    let cancelled = false
    request<{ account_id: string; folders: MailboxFolder[] }>(
      `/api/mailboxes?account_id=${encodeURIComponent(accountId)}`,
    )
      .then((data) => {
        if (cancelled) return
        setFolders(data.folders ?? [])
      })
      .catch(() => {
        if (cancelled) return
        setFolders([])
      })
    return () => {
      cancelled = true
    }
  }, [accountId])

  // 4. 查询收件箱邮件
  useEffect(() => {
    if (!accountId) return
    setLoading(true)
    abortRef.current?.abort()
    const controller = new AbortController()
    abortRef.current = controller
    let cancelled = false

    const params = new URLSearchParams({ account_id: accountId })
    if (alias) params.set('alias', alias)
    if (folder) params.set('folder', folder)
    params.set('limit', String(limit))
    params.set('days', String(days))

    request<InboxResult>(`/api/inbox?${params.toString()}`, {
      signal: controller.signal,
    })
      .then((data) => {
        if (cancelled) return
        setResult(data)
        setError('')
        // 静默预取前 20 封邮件正文注入内存缓存
        if (data && Array.isArray(data.messages) && data.messages.length > 0) {
          const targets = data.messages.slice(0, 20).map((m) => ({
            folder: m.folder || 'INBOX',
            uid: m.id,
          }))
          request<{ messages?: FullMessage[] }>('/api/messages', {
            method: 'POST',
            body: {
              account_id: accountId,
              messages: targets,
            },
          })
            .then((batch) => {
              const list = Array.isArray(batch?.messages) ? batch.messages : []
              list.forEach((fm) => {
                if (fm?.id) {
                  messageCacheRef.current.set(fm.id, fm)
                }
              })
            })
            .catch(() => {})
        }
      })
      .catch((err) => {
        if (cancelled || (err instanceof ApiError && err.code === 'ABORTED')) return
        setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
        setResult(null)
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })

    return () => {
      cancelled = true
      controller.abort()
    }
  }, [accountId, alias, folder, limit, days, retryKey])

  // 查询提交
  function handleSearch() {
    setPage(1)
    if (!fixedAccount) {
      const next: Record<string, string> = { account_id: accountId }
      if (alias) next.alias = alias
      if (folder && folder !== 'all') next.folder = folder
      next.limit = String(limit)
      next.days = String(days)
      setSearchParams(next, { replace: true })
    }
    setRetryKey((k) => k + 1)
  }

  function handleAccountChange(newAccountId: string) {
    setAccountId(newAccountId)
    setAlias('')
    setPage(1)
    messageCacheRef.current.clear()
    setSearchParams({ account_id: newAccountId }, { replace: true })
  }

  async function openMessage(message: InboxMessage) {
    const cached = messageCacheRef.current.get(message.id)
    if (cached) {
      setDetail(cached)
      return
    }
    setDetailLoading(true)
    try {
      const data = await request<FullMessage>(
        `/api/inbox/${encodeURIComponent(message.id)}?account_id=${encodeURIComponent(accountId)}`,
      )
      messageCacheRef.current.set(message.id, data)
      setDetail(data)
    } catch (err) {
      show(err instanceof ApiError ? err.message : '读取邮件详情失败')
    } finally {
      setDetailLoading(false)
    }
  }

  async function deleteMessage() {
    if (!deleteFor) return
    setDeleting(true)
    try {
      await request(
        `/api/inbox/${encodeURIComponent(deleteFor.id)}?account_id=${encodeURIComponent(accountId)}`,
        { method: 'DELETE' },
      )
      messageCacheRef.current.delete(deleteFor.id)
      setDeleteFor(null)
      setDetail(null)
      show('邮件已删除')
      setRetryKey((k) => k + 1)
    } catch (err) {
      show(err instanceof ApiError ? err.message : '删除邮件失败')
    } finally {
      setDeleting(false)
    }
  }

  async function handleCopyCode(code: string) {
    if (copiedTimerRef.current) clearTimeout(copiedTimerRef.current)
    const ok = await copyText(code)
    setCopiedCode(code)
    copiedTimerRef.current = setTimeout(() => {
      setCopiedCode(null)
    }, 1600)
    const notifyMsg = ok ? `验证码 [${code}] 已复制` : `验证码：${code}`
    show(notifyMsg)
    if (onCopySuccess) onCopySuccess(notifyMsg)
  }

  async function handleCopyAlias(aliasText: string) {
    if (copiedAliasTimerRef.current) clearTimeout(copiedAliasTimerRef.current)
    await copyText(aliasText)
    setCopiedAlias(aliasText)
    copiedAliasTimerRef.current = setTimeout(() => setCopiedAlias(null), 1600)
    const notifyMsg = `别名 [${aliasText}] 已复制`
    show(notifyMsg)
    if (onCopySuccess) onCopySuccess(notifyMsg)
  }

  const folderOptions = useMemo(() => {
    const base = [
      { value: 'all', label: '全部 (收件箱+垃圾箱)' },
      { value: 'INBOX', label: '收件箱 (INBOX)' },
      { value: 'Junk', label: '垃圾箱 (Junk)' },
    ]
    if (!folders.length) return base
    const known = new Set(['all', 'inbox', 'junk'])
    const extra = folders
      .filter((f) => !known.has(f.name.toLowerCase()) && !known.has(f.role.toLowerCase()))
      .map((f) => ({ value: f.name, label: `${f.name} (${f.role})` }))
    return [...base, ...extra]
  }, [folders])

  const rawMessages = useMemo(() => (Array.isArray(result?.messages) ? result.messages : []), [result])

  // 工作台模式下支持别名包含过滤
  const filteredMessages = useMemo(() => {
    let list = rawMessages
    if (fixedAccount && alias) {
      const q = alias.toLowerCase()
      list = list.filter((m) => (m.to || '').toLowerCase().includes(q))
    }
    return list.slice().sort((a, b) => (dateTimestamp(b.date) ?? 0) - (dateTimestamp(a.date) ?? 0))
  }, [rawMessages, fixedAccount, alias])

  useEffect(() => {
    if (onCountChange) {
      onCountChange(filteredMessages.length)
    }
  }, [filteredMessages.length, onCountChange])

  const currentAccount = accounts.find((a) => a.id === accountId)
  const currentAccountName = currentAccount?.name || currentAccount?.real_email || (accountId || '未选择')
  const totalCount = result?.count ?? filteredMessages.length
  const codesDetectedCount = useMemo(() => {
    return filteredMessages.filter((m) => Boolean(extractVerifyCode(buildSniffContext(m.subject, m.preview)))).length
  }, [filteredMessages])

  const totalPages = Math.max(1, Math.ceil(filteredMessages.length / pageSize))
  useEffect(() => {
    if (page > totalPages) setPage(totalPages)
  }, [page, totalPages])

  const pagedMessages = useMemo(() => {
    const start = (page - 1) * pageSize
    return filteredMessages.slice(start, start + pageSize)
  }, [filteredMessages, page, pageSize])

  const methodText = (result?.method || 'imap').toLowerCase() === 'web_api' ? 'Web API' : 'IMAP'

  if (!fixedAccount && accounts.length === 0 && !loading && !error) {
    return (
      <div className="page-container">
        <p className="empty-state">暂无账号，请先到「账号」页面添加账号</p>
      </div>
    )
  }

  return (
    <div className={showPageHeader ? 'page-container' : undefined}>
      {showPageHeader && (
        <div className="page-header">
          <div>
            <h1 className="page-title">收件箱摘要</h1>
            <p className="page-desc">查看发往各母号隐私别名的邮件与验证码（支持纯文本清洗与验证码一键复制）</p>
          </div>
          <div>
            <button
              type="button"
              className="btn btn-secondary"
              onClick={handleSearch}
              disabled={loading}
              title="重新获取最新邮件"
            >
              <IconRefresh size={14} />
              <span>{loading ? '刷新中…' : '刷新邮件'}</span>
            </button>
          </div>
        </div>
      )}

      <div className="card">
        {/* 卡片顶栏：统计胶囊 */}
        <div className="card-header">
          <h2 className="card-title">
            <IconMail size={16} />
            <span>邮件收件箱</span>
            <span className="card-title-count">(<strong>{totalCount}</strong>)</span>
          </h2>
          <div className="card-header-stats">
            {codesDetectedCount > 0 ? (
              <span className="card-stat-pill is-healthy">
                <IconKey size={12} />
                <span>
                  探测到 <strong>{codesDetectedCount}</strong> 项验证码
                </span>
              </span>
            ) : (
              <span className="card-stat-pill">
                <IconKey size={12} />
                <span>{loading && !result ? '读取中…' : '未发现验证码'}</span>
              </span>
            )}

            <span className="card-stat-pill">
              <span className="status-dot active" />
              <span>接收: {methodText}</span>
            </span>

            {currentAccount && (
              <span className="card-stat-pill" title={`母账号: ${currentAccountName}`}>
                <IconAccounts size={12} />
                <span>
                  母号: <strong>{currentAccountName}</strong>
                </span>
              </span>
            )}

            {alias && (
              <span className="card-stat-pill is-filter" title={`当前筛选别名: ${alias}`}>
                <IconSearch size={12} />
                <span>
                  别名: <strong>{alias}</strong>
                </span>
              </span>
            )}
          </div>
        </div>

        {/* 统一工具栏 */}
        <div className="inbox-filter-bar">
          {!fixedAccount && (
            <div className="inbox-filter-item">
              <label htmlFor="inbox-account" className="inbox-filter-label">
                账号
              </label>
              <Select
                id="inbox-account"
                aria-label="账号"
                value={accountId}
                onChange={handleAccountChange}
                options={accounts.map((a) => ({ value: a.id, label: a.name || a.real_email }))}
                style={{ minWidth: 160 }}
              />
            </div>
          )}

          <div className="inbox-filter-item">
            <label htmlFor="inbox-alias" className="inbox-filter-label">
              别名
            </label>
            <Select
              id="inbox-alias"
              aria-label="别名"
              value={alias}
              onChange={(val) => setAlias(val)}
              options={[
                { value: '', label: fixedAccount ? '全部别名邮件' : '全部' },
                ...aliases.map((a) => ({
                  value: a.email,
                  label: a.email,
                })),
              ]}
              style={{ minWidth: 190 }}
            />
          </div>

          <div className="inbox-filter-item">
            <label htmlFor="inbox-folder" className="inbox-filter-label">
              文件夹
            </label>
            <Select
              id="inbox-folder"
              aria-label="文件夹"
              value={folder}
              onChange={(val) => setFolder(val)}
              options={folderOptions}
              style={{ minWidth: 200 }}
            />
          </div>

          <div className="inbox-filter-item">
            <label htmlFor="inbox-limit" className="inbox-filter-label">
              每页
            </label>
            <Select
              id="inbox-limit"
              aria-label="每页"
              value={limit}
              onChange={(val) => setLimit(Number(val))}
              options={[
                { value: 1, label: '1' },
                { value: 20, label: '20' },
                { value: 100, label: '100' },
              ]}
              style={{ minWidth: 72 }}
            />
          </div>

          <div className="inbox-filter-item">
            <label htmlFor="inbox-days" className="inbox-filter-label">
              时间范围
            </label>
            <Select
              id="inbox-days"
              aria-label="时间范围"
              value={days}
              onChange={(val) => setDays(Number(val))}
              options={[
                { value: 1, label: '1 天' },
                { value: 7, label: '7 天' },
                { value: 30, label: '30 天' },
                { value: 90, label: '90 天' },
              ]}
              style={{ minWidth: 90 }}
            />
          </div>

          <button
            type="button"
            className="btn btn-primary"
            onClick={handleSearch}
            disabled={loading}
          >
            查询
          </button>
        </div>

        {/* 邮件列表数据体 */}
        <AsyncState
          loading={loading && !result}
          error={error}
          onRetry={() => setRetryKey((k) => k + 1)}
          empty={!loading && filteredMessages.length === 0}
          emptyText={alias ? `未查找到别名 [${alias}] 的邮件` : '收件箱暂无邮件'}
        >
          <div className="table-responsive">
            <table className="table inbox-table">
              <thead>
                <tr>
                  <th style={{ width: 145 }}>验证码</th>
                  <th style={{ minWidth: 280 }}>邮件内容</th>
                  <th style={{ width: 180 }}>发件人</th>
                  <th style={{ width: 260 }}>收件地址</th>
                  <th style={{ width: 110 }}>时间</th>
                  <th style={{ width: 110, textAlign: 'center' }}>操作</th>
                </tr>
              </thead>
              <tbody>
                {pagedMessages.map((m) => (
                  <InboxTableRow
                    key={m.id}
                    message={m}
                    copiedCode={copiedCode}
                    copiedAlias={copiedAlias}
                    onOpenMessage={(msg) => void openMessage(msg)}
                    onCopyCode={(c) => void handleCopyCode(c)}
                    onCopyAlias={(a) => void handleCopyAlias(a)}
                    onDelete={(msg) => setDeleteFor(msg)}
                  />
                ))}
              </tbody>
            </table>
          </div>

          {/* 分页控制器 */}
          {totalPages > 1 && (
            <div className="table-pagination">
              <span className="pagination-info">
                第 <strong>{page}</strong> / {totalPages} 页 (共 {filteredMessages.length} 封)
              </span>
              <div className="pagination-buttons">
                <button
                  type="button"
                  className="pagination-btn"
                  disabled={page <= 1}
                  onClick={() => setPage((p) => Math.max(1, p - 1))}
                >
                  上一页
                </button>
                <button
                  type="button"
                  className="pagination-btn"
                  disabled={page >= totalPages}
                  onClick={() => setPage((p) => Math.min(totalPages, p + 1))}
                >
                  下一页
                </button>
              </div>
            </div>
          )}
        </AsyncState>
      </div>

      {/* 邮件详情弹窗 */}
      <MailDetailDialog
        detail={detail}
        loading={detailLoading}
        onClose={() => setDetail(null)}
        onCopySuccess={(msg) => {
          show(msg)
          onCopySuccess?.(msg)
        }}
      />

      {/* 删除邮件确认弹窗 */}
      <ConfirmDialog
        open={Boolean(deleteFor)}
        title="删除邮件"
        message={deleteFor ? `确定从 iCloud IMAP 邮箱中永久删除来自「${parseSenderInfo(deleteFor.from).name}」的邮件吗？此操作不可撤销。` : ''}
        confirmLabel="确认删除"
        busy={deleting}
        onConfirm={() => void deleteMessage()}
        onClose={() => setDeleteFor(null)}
      />
    </div>
  )
}
