/**
 * [INPUT]: 依赖 api/client, api/types, components 下各 Dialog (含 BatchCreateAliasDialog)、Select 与 ToastProvider, utils/clipboard, utils/date, components/icons (含 IconDownload)
 * [OUTPUT]: 对外提供 AliasesPage 全局别名号池与资产大盘页面组件 (紧凑型资产大盘 + 卡片顶栏内嵌状态胶囊 + 批量生成 + 资产全量导出 + 统一紧凑筛选栏 + 柔性母号徽章 + 极简行级单行操作 + 客户端分页与可配置单页条数)
 * [POS]: web/src/pages 的核心页面，负责跨账号全局别名号池总览、批量生成、资产导出、所属母号溯源、启停、备注编辑与销毁
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useCallback, useEffect, useMemo, useState, type CSSProperties } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { request, ApiError } from '../api/client'
import type { AccountSummary, Alias } from '../api/types'
import AsyncState from '../components/AsyncState'
import EditAliasDialog from '../components/EditAliasDialog'
import BatchEditAliasDialog from '../components/BatchEditAliasDialog'
import BatchCreateAliasDialog from '../components/BatchCreateAliasDialog'
import ConfirmDialog from '../components/ConfirmDialog'
import Select from '../components/Select'
import { useToast } from '../components/ToastProvider'
import { copyText } from '../utils/clipboard'
import { dateTimestamp, formatDate } from '../utils/date'
import {
  IconAccounts,
  IconAliases,
  IconChevronDown,
  IconChevronLeft,
  IconChevronRight,
  IconChevronUp,
  IconCopy,
  IconDownload,
  IconEdit,
  IconInbox,
  IconPlus,
  IconRefresh,
  IconSearch,
  IconTrash,
} from '../components/icons'

type SortDirection = 'asc' | 'desc'

const PAGE_SIZE_OPTIONS = [
  { value: '20', label: '20 条 / 页' },
  { value: '50', label: '50 条 / 页' },
  { value: '100', label: '100 条 / 页' },
]

/** 账号标识名 → 稳定色相 (0-359)，同名永远同色，支持浅色/深色主题自适应 */
function tagHue(name: string): number {
  let hash = 0
  for (let i = 0; i < name.length; i++) {
    hash = (hash * 31 + name.charCodeAt(i)) % 360
  }
  return hash
}

const srOnly: CSSProperties = {
  position: 'absolute',
  width: 1,
  height: 1,
  padding: 0,
  margin: -1,
  overflow: 'hidden',
  clip: 'rect(0, 0, 0, 0)',
  border: 0,
}

export default function AliasesPage() {
  const [accounts, setAccounts] = useState<AccountSummary[]>([])
  const [accountId, setAccountId] = useState('all')
  const [aliases, setAliases] = useState<Alias[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [retryKey, setRetryKey] = useState(0)
  const [search, setSearch] = useState('')
  const [filter, setFilter] = useState<'all' | 'active' | 'inactive'>('all')
  const [sortDirection, setSortDirection] = useState<SortDirection>('desc')
  const [editingAlias, setEditingAlias] = useState<Alias | null>(null)
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set())
  const [batchEditOpen, setBatchEditOpen] = useState(false)
  const [batchCreateOpen, setBatchCreateOpen] = useState(false)
  const [confirm, setConfirm] = useState<{
    type: 'deactivate' | 'reactivate' | 'delete'
    alias: Alias
  } | null>(null)
  const [busy, setBusy] = useState(false)
  const [actionError, setActionError] = useState('')

  // 客户端分页状态
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState<number>(() => {
    try {
      const saved = Number(localStorage.getItem('icloud_hme_alias_page_size'))
      if ([20, 50, 100].includes(saved)) return saved
    } catch {
      // ignore
    }
    return 20
  })

  function handlePageSizeChange(val: string) {
    const nextSize = Number(val) || 20
    setPageSize(nextSize)
    setPage(1)
    try {
      localStorage.setItem('icloud_hme_alias_page_size', String(nextSize))
    } catch {
      // ignore
    }
  }

  const [searchParams, setSearchParams] = useSearchParams()
  const { show, showCopyable } = useToast()

  const queryAccId = searchParams.get('account_id')

  // 加载账号列表 (仅在挂载或重试时拉取)
  useEffect(() => {
    let cancelled = false
    request<AccountSummary[]>('/api/accounts')
      .then((data) => {
        if (cancelled) return
        setAccounts(data)
        const valid = data.find((a) => a.id === queryAccId)
        const target = valid ? valid.id : (!queryAccId || queryAccId === 'all' ? 'all' : (data[0]?.id ?? 'all'))
        setAccountId(target)
        if (data.length === 0) {
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
    // eslint-disable-next-line react-hooks/exhaustive-deps -- 仅在挂载或重试时拉取账号列表，URL 变动由专用同步 Effect 驱动
  }, [retryKey])

  // 监听 URL 外部变动（如前进/后退）并同步 accountId
  useEffect(() => {
    if (accounts.length === 0) return
    const valid = accounts.find((a) => a.id === queryAccId)
    const target = valid ? valid.id : (!queryAccId || queryAccId === 'all' ? 'all' : (accounts[0]?.id ?? 'all'))
    setAccountId(target)
  }, [queryAccId, accounts])

  // 加载别名列表
  useEffect(() => {
    if (accounts.length === 0) {
      setAliases([])
      setLoading(false)
      return
    }
    if (!accountId) return
    setLoading(true)
    let cancelled = false
    const url = accountId === 'all'
      ? '/api/aliases?account_id=all'
      : `/api/aliases?account_id=${encodeURIComponent(accountId)}`
    request<{ account_id: string; count: number; aliases: Alias[] }>(url)
      .then((data) => {
        if (cancelled) return
        setAliases(data.aliases ?? [])
        setError('')
      })
      .catch((err) => {
        if (cancelled) return
        setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [accountId, accounts.length, retryKey])

  // 筛选与排序
  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase()
    return aliases
      .map((alias, index) => ({ alias, index }))
      .filter(({ alias }) => {
        if (filter === 'active' && !alias.active) return false
        if (filter === 'inactive' && alias.active) return false
        if (!q) return true
        const accName = (alias.account_name || alias.accountName || '').toLowerCase()
        return (
          alias.email.toLowerCase().includes(q) ||
          alias.label.toLowerCase().includes(q) ||
          accName.includes(q)
        )
      })
      .sort((left, right) => {
        const leftTime = dateTimestamp(left.alias.createdAt)
        const rightTime = dateTimestamp(right.alias.createdAt)

        // 没有有效时间的记录始终放在末尾，避免倒序时跳到列表顶部。
        if (leftTime === null || rightTime === null) {
          if (leftTime === rightTime) return left.index - right.index
          return leftTime === null ? 1 : -1
        }
        if (leftTime === rightTime) return left.index - right.index
        return sortDirection === 'asc' ? leftTime - rightTime : rightTime - leftTime
      })
      .map(({ alias }) => alias)
  }, [aliases, search, filter, sortDirection])

  // 分页切片
  const totalPages = Math.max(1, Math.ceil(filtered.length / pageSize))
  const pagedAliases = useMemo(() => {
    const start = (page - 1) * pageSize
    return filtered.slice(start, start + pageSize)
  }, [filtered, page, pageSize])

  // 筛选条件变化时复位页码
  useEffect(() => {
    setPage(1)
  }, [accountId, search, filter])

  // 当别名因删除或筛选变少时，确保页码不超出上限
  useEffect(() => {
    if (page > totalPages) {
      setPage(totalPages)
    }
  }, [page, totalPages])

  useEffect(() => {
    setSelectedIds(new Set())
  }, [accountId, search, filter])

  const pagedIds = useMemo(() => pagedAliases.map((a) => a.anonymousId), [pagedAliases])
  const isAllSelected = pagedIds.length > 0 && pagedIds.every((id) => selectedIds.has(id))
  const isSomeSelected = pagedIds.some((id) => selectedIds.has(id)) && !isAllSelected

  const toggleSelect = useCallback((id: string) => {
    setSelectedIds((prev) => {
      const next = new Set(prev)
      if (next.has(id)) {
        next.delete(id)
      } else {
        next.add(id)
      }
      return next
    })
  }, [])

  const toggleSelectAll = useCallback(() => {
    setSelectedIds((prev) => {
      const next = new Set(prev)
      if (isAllSelected) {
        pagedIds.forEach((id) => next.delete(id))
      } else {
        pagedIds.forEach((id) => next.add(id))
      }
      return next
    })
  }, [isAllSelected, pagedIds])

  const clearSelection = useCallback(() => {
    setSelectedIds(new Set())
  }, [])

  function handleRetry() {
    setLoading(true)
    setRetryKey((k) => k + 1)
  }

  async function handleRefreshPool() {
    setLoading(true)
    try {
      const url = accountId === 'all'
        ? '/api/aliases?account_id=all&refresh=true'
        : `/api/aliases?account_id=${encodeURIComponent(accountId)}&refresh=true`
      const data = await request<{ account_id: string; count: number; aliases: Alias[] }>(url)
      setAliases(data.aliases ?? [])
      setError('')
      // 同步刷新母账号统计指标，绝不触发对别名列表的重复拉取
      request<AccountSummary[]>('/api/accounts').then(setAccounts).catch(() => {})
      show('号池已与 Apple 同步最新数据')
    } catch (err) {
      show(err instanceof ApiError ? err.message : '刷新号池失败')
    } finally {
      setLoading(false)
    }
  }

  function handleExport(format: 'csv' | 'json' = 'csv') {
    const a = document.createElement('a')
    a.href = `/api/aliases/export?account_id=${encodeURIComponent(accountId)}&format=${format}`
    a.download = ''
    document.body.appendChild(a)
    a.click()
    document.body.removeChild(a)
    show('已触发别名资产导出')
  }

  const copyEmail = useCallback(
    async (email: string) => {
      if (await copyText(email)) {
        show('邮箱已复制')
      } else {
        showCopyable(email, '复制失败，请手动复制')
      }
    },
    [show, showCopyable],
  )

  async function runAction(type: 'deactivate' | 'reactivate' | 'delete') {
    if (!confirm) return
    setBusy(true)
    setActionError('')
    const { alias } = confirm
    const targetAccId = alias.account_id || alias.accountId || (accountId !== 'all' ? accountId : accounts[0]?.id)
    try {
      if (type === 'delete') {
        const deletedId = alias.anonymousId
        await request(
          `/api/aliases/${encodeURIComponent(deletedId)}`,
          { method: 'DELETE', body: JSON.stringify({ account_id: targetAccId }) },
        )
        show('别名已删除')
        setSelectedIds((prev) => {
          if (!prev.has(deletedId)) return prev
          const next = new Set(prev)
          next.delete(deletedId)
          return next
        })
      } else {
        await request(
          `/api/aliases/${encodeURIComponent(alias.anonymousId)}/${type === 'deactivate' ? 'deactivate' : 'reactivate'}`,
          { method: 'POST', body: JSON.stringify({ account_id: targetAccId }) },
        )
        show(type === 'deactivate' ? '别名已停用' : '别名已激活')
      }
      setConfirm(null)
      setRetryKey((k) => k + 1)
    } catch (err) {
      const msg = err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态'
      setActionError(msg)
      show(msg)
    } finally {
      setBusy(false)
    }
  }

  if (accounts.length === 0 && !loading && !error) {
    return (
      <div className="page-container">
        <p className="empty-state">暂无账号，请先到「账号」页面添加账号</p>
      </div>
    )
  }

  const confirmTitle =
    confirm?.type === 'delete'
      ? '删除别名'
      : confirm?.type === 'deactivate'
        ? '停用别名'
        : '激活别名'

  const confirmLabel =
    confirm?.type === 'delete' ? '确认删除' : confirm?.type === 'deactivate' ? '确认停用' : '确认激活'

  const currentAccount = accounts.find((a) => a.id === accountId)
  const totalAliasesCount = accounts.reduce((sum, a) => sum + (a.alias_total || 0), 0)
  const totalCount = accountId === 'all'
    ? (aliases.length || totalAliasesCount)
    : (aliases.length || currentAccount?.alias_total || 0)

  // 哨兵机制：当 aliases 尚未从服务端返回时，优先利用 accounts 汇总的已统计 active 基数，彻底终结 0% 可用 (0/62) 的假象
  const totalActiveFromAccounts = accountId === 'all'
    ? accounts.reduce((sum, a) => sum + (a.alias_active || 0), 0)
    : (currentAccount?.alias_active || 0)
  const activeCount = aliases.length > 0
    ? aliases.filter((a) => a.active).length
    : totalActiveFromAccounts
  const activePercent = totalCount > 0 ? Math.round((activeCount / totalCount) * 100) : 0
  const hasFilter = Boolean(search.trim() || filter !== 'all' || accountId !== 'all')

  const accountOptions = [
    { value: 'all', label: `全部母账号 (${totalAliasesCount})` },
    ...accounts.map((a) => ({
      value: a.id,
      label: `${a.name || a.real_email} (${a.alias_total ?? 0})`,
    })),
  ]

  return (
    <div className="page-container">
      {/* 顶部标题与主要操作 */}
      <div className="page-header">
        <div>
          <h1 className="page-title">别名号池</h1>
          <p className="page-desc">
            聚合各母号 Hide My Email 别名资产库，支持跨母号溯源、统一检索、批量维护与收件箱直达
          </p>
        </div>
        <div style={{ display: 'flex', gap: 8 }}>
          <button
            type="button"
            className="btn btn-secondary"
            onClick={() => handleExport('csv')}
            disabled={accounts.length === 0}
            title="导出当前母账号或全量别名资产 (CSV 格式，自带 Excel BOM)"
          >
            <IconDownload size={14} />
            <span>导出 CSV</span>
          </button>
          <button
            type="button"
            className="btn btn-primary"
            onClick={() => setBatchCreateOpen(true)}
            disabled={accounts.length === 0}
            title="批量生成别名 (1-5个)"
          >
            <IconPlus size={14} />
            <span>批量生成</span>
          </button>
          <button
            type="button"
            className="btn btn-secondary"
            onClick={handleRefreshPool}
            disabled={loading}
            title="刷新号池数据（穿透缓存向 Apple 同步）"
          >
            <IconRefresh size={14} />
            <span>{loading ? '刷新中…' : '刷新号池'}</span>
          </button>
        </div>
      </div>

      {/* 主数据卡片 */}
      <div className="card">
        <div className="card-header">
          <h2 className="card-title">
            <IconAliases size={16} /> 别名资产库 ({totalCount})
          </h2>
          <div className="card-header-stats">
            <span className="card-stat-pill is-healthy">
              <span className="status-dot" />
              <span>
                {activePercent}% 可用 ({activeCount}/{totalCount})
              </span>
            </span>

            <span className="card-stat-pill">
              <IconAccounts size={13} />
              <span>
                <strong>{accounts.length}</strong> 个母账号
              </span>
            </span>

            {hasFilter && (
              <span className="card-stat-pill is-filter">
                <IconSearch size={12} />
                <span>
                  匹配 <strong>{filtered.length}</strong> 条
                </span>
              </span>
            )}
          </div>
        </div>

        {/* 工具筛选栏 */}
        <div className="filter-bar">
          <div className="filter-item">
            <label htmlFor="alias-account" style={srOnly}>所属账号</label>
            <Select
              id="alias-account"
              aria-label="所属账号"
              size="sm"
              value={accountId}
              onChange={(val) => {
                setAccountId(val)
                setSearchParams(val && val !== 'all' ? { account_id: val } : {}, { replace: true })
              }}
              options={accountOptions}
              style={{ minWidth: 180 }}
            />
          </div>

          <div className="filter-item">
            <label htmlFor="alias-filter" style={srOnly}>状态</label>
            <Select
              id="alias-filter"
              aria-label="状态"
              size="sm"
              value={filter}
              onChange={(val) => setFilter(val as 'all' | 'active' | 'inactive')}
              options={[
                { value: 'all', label: '全部状态' },
                { value: 'active', label: '已启用' },
                { value: 'inactive', label: '已停用' },
              ]}
              style={{ minWidth: 100 }}
            />
          </div>

          <div className="filter-item filter-search">
            <IconSearch size={14} />
            <label htmlFor="alias-search" style={srOnly}>搜索</label>
            <input
              id="alias-search"
              aria-label="搜索"
              type="search"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="按别名邮箱、备注或母号搜索…"
            />
          </div>
        </div>

        {/* 批量操作提示栏 */}
        {selectedIds.size > 0 && (
          <div className="batch-action-bar" style={{ margin: '12px 20px 0' }}>
            <div className="batch-action-left">
              <span className="batch-count-badge">{selectedIds.size}</span>
              <span className="batch-action-title">个别名已选中</span>
            </div>
            <div className="batch-action-right">
              <button
                type="button"
                className="btn btn-xs btn-primary"
                onClick={() => setBatchEditOpen(true)}
              >
                <IconEdit size={12} /> 批量修改备注
              </button>
              <button
                type="button"
                className="btn btn-xs btn-secondary"
                onClick={clearSelection}
              >
                取消选择
              </button>
            </div>
          </div>
        )}

        <AsyncState
          loading={loading && aliases.length === 0}
          error={error}
          empty={filtered.length === 0}
          emptyText={aliases.length === 0 ? '暂无别名' : '没有匹配的别名'}
          onRetry={handleRetry}
        >
          <div className={loading && aliases.length > 0 ? 'table-shell is-refreshing' : 'table-shell'}>
            <div className="table-responsive">
              <table className="table">
              <thead>
                <tr>
                  <th style={{ width: 44, minWidth: 44, textAlign: 'center' }}>
                    <input
                      type="checkbox"
                      className="table-checkbox"
                      checked={isAllSelected}
                      ref={(el) => {
                        if (el) el.indeterminate = isSomeSelected
                      }}
                      onChange={toggleSelectAll}
                      title={isAllSelected ? '取消全选' : '全选当前页'}
                    />
                  </th>
                  <th style={{ width: '28%', minWidth: 220 }}>别名邮箱</th>
                  <th style={{ width: '16%', minWidth: 120 }}>所属母号</th>
                  <th style={{ width: '22%', minWidth: 140 }}>标签 / 备注</th>
                  <th style={{ width: '11%', minWidth: 95 }}>状态</th>
                  <th style={{ width: '15%', minWidth: 130 }} aria-sort={sortDirection === 'asc' ? 'ascending' : 'descending'}>
                    <button
                      type="button"
                      className="table-sort-button"
                      onClick={() => setSortDirection((direction) => (direction === 'asc' ? 'desc' : 'asc'))}
                      aria-label={`创建时间排序：当前${sortDirection === 'asc' ? '正序' : '倒序'}，点击切换为${sortDirection === 'asc' ? '倒序' : '正序'}`}
                      title="点击切换创建时间排序"
                    >
                      <span>创建时间</span>
                      {sortDirection === 'asc' ? <IconChevronUp size={14} /> : <IconChevronDown size={14} />}
                    </button>
                  </th>
                  <th style={{ width: 175, minWidth: 175, textAlign: 'right' }}>快捷操作</th>
                </tr>
              </thead>
              <tbody>
                {pagedAliases.map((alias) => {
                  const aliasAccId = alias.account_id || alias.accountId || (accountId && accountId !== 'all' ? accountId : accounts[0]?.id || '')
                  const aliasAccName = alias.account_name || alias.accountName || accounts.find((a) => a.id === aliasAccId)?.name || aliasAccId
                  return (
                    <tr
                      key={alias.anonymousId}
                      style={selectedIds.has(alias.anonymousId) ? { background: 'var(--color-bg-subtle)' } : undefined}
                    >
                      <td style={{ textAlign: 'center' }}>
                        <input
                          type="checkbox"
                          className="table-checkbox"
                          checked={selectedIds.has(alias.anonymousId)}
                          onChange={() => toggleSelect(alias.anonymousId)}
                          title="选择此别名"
                        />
                      </td>
                      <td>
                        <button
                          type="button"
                          className="link-like font-mono font-semibold"
                          onClick={() => void copyEmail(alias.email)}
                          title="点击复制邮箱"
                        >
                          <span>{alias.email}</span>
                          <IconCopy size={11} className="copy-hint-icon" />
                        </button>
                      </td>
                      <td>
                        <span
                          className="badge badge-tag font-mono"
                          style={{ '--tag-hue': String(tagHue(aliasAccName)) } as CSSProperties}
                          title={`母账号 ID: ${aliasAccId}`}
                        >
                          {aliasAccName || '—'}
                        </span>
                      </td>
                      <td>
                        <div className="alias-cell" style={{ display: 'inline-flex', alignItems: 'center', gap: 4 }}>
                          <span className="alias-label-text" title={alias.label || '无备注'}>{alias.label || '—'}</span>
                          <button
                            type="button"
                            className="btn-icon"
                            onClick={() => setEditingAlias(alias)}
                            title="修改备注"
                            aria-label="修改备注"
                          >
                            <IconEdit size={12} />
                          </button>
                        </div>
                      </td>
                      <td>
                        <span className={`status-pill ${alias.active ? 'active' : 'pending'}`}>
                          <span className="status-dot" />
                          {alias.active ? '已启用' : '已停用'}
                        </span>
                      </td>
                      <td className="text-xs text-secondary">{formatDate(alias.createdAt)}</td>
                      <td style={{ textAlign: 'right' }}>
                        <div className="row-actions" style={{ display: 'inline-flex', alignItems: 'center', gap: 4, justifyContent: 'flex-end', flexWrap: 'nowrap', whiteSpace: 'nowrap' }}>
                          <Link
                            to={aliasAccId ? `/inbox?account_id=${encodeURIComponent(aliasAccId)}&alias=${encodeURIComponent(alias.email)}` : `/inbox?alias=${encodeURIComponent(alias.email)}`}
                            className="btn btn-xs btn-primary-soft"
                            title="查看此别名的收件箱"
                            style={{ display: 'inline-flex', alignItems: 'center', gap: 3, textDecoration: 'none' }}
                          >
                            <IconInbox size={11} />
                            <span>收件箱</span>
                          </Link>
                          {alias.active ? (
                            <button
                              type="button"
                              className="btn btn-xs btn-ghost"
                              disabled={busy}
                              onClick={() => setConfirm({ type: 'deactivate', alias })}
                              title="停用此别名"
                            >
                              <span>停用</span>
                            </button>
                          ) : (
                            <button
                              type="button"
                              className="btn btn-xs btn-ghost"
                              disabled={busy}
                              onClick={() => setConfirm({ type: 'reactivate', alias })}
                              title="激活此别名"
                            >
                              <span>激活</span>
                            </button>
                          )}
                          <button
                            type="button"
                            className="btn btn-xs btn-ghost-danger"
                            disabled={busy}
                            onClick={() => setConfirm({ type: 'delete', alias })}
                            title="删除别名"
                            aria-label="删除"
                          >
                            <IconTrash size={11} />
                            <span>删除</span>
                          </button>
                        </div>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        </div>

        {/* 分页控制栏 */}
        {filtered.length > 0 && (
          <div className="pagination-bar">
            <span className="pagination-info">
              共 <b>{filtered.length}</b> 个别名，当前第 <b>{page}</b> / <b>{totalPages}</b> 页
            </span>
            <div className="pagination-controls">
              <Select
                size="sm"
                aria-label="每页显示条数"
                value={String(pageSize)}
                onChange={handlePageSizeChange}
                options={PAGE_SIZE_OPTIONS}
                style={{ minWidth: 110 }}
              />
              <span className="pagination-divider" />
              <button
                type="button"
                className="pagination-btn"
                disabled={page <= 1}
                onClick={() => setPage((p) => Math.max(1, p - 1))}
              >
                <IconChevronLeft size={13} />
                <span>上一页</span>
              </button>
              <button
                type="button"
                className="pagination-btn"
                disabled={page >= totalPages}
                onClick={() => setPage((p) => Math.min(totalPages, p + 1))}
              >
                <span>下一页</span>
                <IconChevronRight size={13} />
              </button>
            </div>
          </div>
        )}
        </AsyncState>
      </div>

      {confirm && (
        <ConfirmDialog
          title={confirmTitle}
          message={
            confirm.type === 'delete'
              ? `将删除别名 ${confirm.alias.email}。此操作不可恢复，且不会影响 Apple 账号本身。`
              : confirm.type === 'deactivate'
                ? `将停用别名 ${confirm.alias.email}，之后该邮箱将不再接收邮件。`
                : `将重新激活别名 ${confirm.alias.email}。`
          }
          confirmLabel={confirmLabel}
          requireText={
            confirm.type === 'delete' ? confirm.alias.email : undefined
          }
          requireLabel={
            confirm.type === 'delete' ? '输入完整邮箱' : undefined
          }
          open
          busy={busy}
          onClose={() => setConfirm(null)}
          onConfirm={() => void runAction(confirm.type)}
        />
      )}

      {editingAlias && (
        <EditAliasDialog
          open={true}
          accountId={editingAlias.account_id || editingAlias.accountId || (accountId !== 'all' ? accountId : accounts[0]?.id ?? '')}
          alias={editingAlias}
          onClose={() => setEditingAlias(null)}
          onSaved={(newLabel) => {
            setAliases((prev) =>
              prev.map((a) =>
                a.anonymousId === editingAlias.anonymousId ? { ...a, label: newLabel } : a,
              ),
            )
            setEditingAlias(null)
            show('别名备注已更新')
          }}
        />
      )}

      {batchEditOpen && (
        <BatchEditAliasDialog
          open={true}
          accountId={accountId}
          selectedIds={Array.from(selectedIds)}
          aliases={aliases}
          onClose={() => setBatchEditOpen(false)}
          onSaved={(succeededIds, newLabel) => {
            const idSet = new Set(succeededIds)
            setAliases((prev) =>
              prev.map((a) => (idSet.has(a.anonymousId) ? { ...a, label: newLabel } : a)),
            )
            setSelectedIds(new Set())
            setBatchEditOpen(false)
            show(`已成功批量修改 ${succeededIds.length} 个别名的备注`)
          }}
        />
      )}

      {batchCreateOpen && (
        <BatchCreateAliasDialog
          open={true}
          accounts={accounts}
          defaultAccountId={accountId}
          onClose={() => setBatchCreateOpen(false)}
          onSuccess={(res) => {
            show(`已成功生成 ${res.created_count} 个别名`)
            setRetryKey((k) => k + 1)
          }}
        />
      )}

      {actionError && (
        <div className="alert-error" role="alert" style={{ marginTop: 16 }}>
          {actionError}
        </div>
      )}
    </div>
  )
}
