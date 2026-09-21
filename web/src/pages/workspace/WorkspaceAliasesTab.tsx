/**
 * [INPUT]: 依赖 api/types 的 Alias, components/icons 的各类图标, components/Select, utils/clipboard, utils/date
 * [OUTPUT]: 对外提供 WorkspaceAliasesTab 别名表格与批量操作组件 (可配置单页条数)
 * [POS]: web/src/pages/workspace 的别名管理视图，提供检索、多条件过滤、批量修改、分页与行级快捷操作
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useCallback, useMemo, useState } from 'react'
import type { Alias } from '../../api/types'
import Select from '../../components/Select'
import {
  IconChevronDown,
  IconChevronLeft,
  IconChevronRight,
  IconChevronUp,
  IconCopy,
  IconEdit,
  IconInbox,
  IconSearch,
  IconTrash,
} from '../../components/icons'
import { copyText } from '../../utils/clipboard'
import { dateTimestamp, formatDate, formatFullDate } from '../../utils/date'

const PAGE_SIZE_OPTIONS = [
  { value: '20', label: '20 条 / 页' },
  { value: '50', label: '50 条 / 页' },
  { value: '100', label: '100 条 / 页' },
]

interface WorkspaceAliasesTabProps {
  aliases: Alias[]
  aliasLoading: boolean
  totalAliasCount: number
  activeAliasCount: number
  selectedIds: Set<string>
  setSelectedIds: React.Dispatch<React.SetStateAction<Set<string>>>
  onEditAlias: (item: Alias) => void
  onToggleAlias: (item: Alias) => void
  onDeleteAlias: (item: Alias) => void
  onReadMail: (email: string) => void
  onOpenBatchEdit: () => void
  onCopySuccess: (msg: string) => void
}

export default function WorkspaceAliasesTab({
  aliases,
  aliasLoading,
  totalAliasCount,
  activeAliasCount,
  selectedIds,
  setSelectedIds,
  onEditAlias,
  onToggleAlias,
  onDeleteAlias,
  onReadMail,
  onOpenBatchEdit,
  onCopySuccess,
}: WorkspaceAliasesTabProps) {
  const [aliasSearch, setAliasSearch] = useState('')
  const [filterActive, setFilterActive] = useState<'all' | 'active' | 'inactive'>('all')
  const [sortDirection, setSortDirection] = useState<'asc' | 'desc'>('desc')
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState<number>(() => {
    try {
      const saved = Number(localStorage.getItem('icloud_hme_workspace_alias_page_size'))
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
      localStorage.setItem('icloud_hme_workspace_alias_page_size', String(nextSize))
    } catch {
      // ignore
    }
  }

  const filteredAliases = useMemo(() => {
    const q = aliasSearch.trim().toLowerCase()
    const list = Array.isArray(aliases) ? aliases : []
    return list
      .map((item, index) => ({ item, index }))
      .filter(({ item: a }) => {
        const matchFilter =
          filterActive === 'all'
            ? true
            : filterActive === 'active'
              ? a.active
              : !a.active
        const matchQuery =
          !q || a.email.toLowerCase().includes(q) || (a.label && a.label.toLowerCase().includes(q))
        return matchFilter && matchQuery
      })
      .sort((left, right) => {
        const leftTime = dateTimestamp(left.item.createdAt)
        const rightTime = dateTimestamp(right.item.createdAt)
        if (leftTime === null || rightTime === null) {
          if (leftTime === rightTime) return left.index - right.index
          return leftTime === null ? 1 : -1
        }
        if (leftTime === rightTime) return left.index - right.index
        return sortDirection === 'asc' ? leftTime - rightTime : rightTime - leftTime
      })
      .map(({ item }) => item)
  }, [aliases, aliasSearch, filterActive, sortDirection])

  const totalPages = Math.max(1, Math.ceil(filteredAliases.length / pageSize))
  const safePage = Math.min(Math.max(1, page), totalPages)
  const paginatedAliases = useMemo(() => {
    const start = (safePage - 1) * pageSize
    return filteredAliases.slice(start, start + pageSize)
  }, [filteredAliases, safePage, pageSize])

  const pageIds = useMemo(() => paginatedAliases.map((a) => a.anonymousId), [paginatedAliases])
  const isAllPageSelected = pageIds.length > 0 && pageIds.every((id) => selectedIds.has(id))
  const isSomePageSelected = pageIds.some((id) => selectedIds.has(id)) && !isAllPageSelected

  const toggleSelect = useCallback(
    (id: string) => {
      setSelectedIds((prev) => {
        const next = new Set(prev)
        if (next.has(id)) {
          next.delete(id)
        } else {
          next.add(id)
        }
        return next
      })
    },
    [setSelectedIds],
  )

  const toggleSelectPage = useCallback(() => {
    setSelectedIds((prev) => {
      const next = new Set(prev)
      if (isAllPageSelected) {
        pageIds.forEach((id) => next.delete(id))
      } else {
        pageIds.forEach((id) => next.add(id))
      }
      return next
    })
  }, [isAllPageSelected, pageIds, setSelectedIds])

  const selectAllFiltered = useCallback(() => {
    setSelectedIds(new Set(filteredAliases.map((a) => a.anonymousId)))
  }, [filteredAliases, setSelectedIds])

  const clearSelection = useCallback(() => {
    setSelectedIds(new Set())
  }, [setSelectedIds])

  return (
    <div className="card">
      <div className="filter-bar">
        <div className="segmented-filter" role="group" aria-label="别名状态筛选">
          <button
            type="button"
            className={`segmented-filter-btn ${filterActive === 'all' ? 'active' : ''}`}
            onClick={() => {
              setFilterActive('all')
              setPage(1)
            }}
          >
            <span>全部</span>
            <span className="filter-count-badge">{totalAliasCount}</span>
          </button>
          <button
            type="button"
            className={`segmented-filter-btn ${filterActive === 'active' ? 'active' : ''}`}
            onClick={() => {
              setFilterActive('active')
              setPage(1)
            }}
          >
            <span className="filter-dot active" />
            <span>仅活跃</span>
            <span className="filter-count-badge">{activeAliasCount}</span>
          </button>
          <button
            type="button"
            className={`segmented-filter-btn ${filterActive === 'inactive' ? 'active' : ''}`}
            onClick={() => {
              setFilterActive('inactive')
              setPage(1)
            }}
          >
            <span className="filter-dot inactive" />
            <span>仅停用</span>
            <span className="filter-count-badge">{totalAliasCount - activeAliasCount}</span>
          </button>
        </div>
        <div className="filter-item filter-search">
          <IconSearch size={16} />
          <input
            type="text"
            className="input"
            placeholder="搜索别名邮箱或标签备注…"
            value={aliasSearch}
            onChange={(e) => {
              setAliasSearch(e.target.value)
              setPage(1)
            }}
          />
        </div>
      </div>

      {selectedIds.size > 0 && (
        <div className="batch-action-bar">
          <div className="batch-action-left">
            <span className="batch-count-badge">{selectedIds.size}</span>
            <span className="batch-action-title">个别名已选中</span>
            {selectedIds.size < filteredAliases.length && (
              <button
                type="button"
                className="btn btn-xs btn-ghost"
                onClick={selectAllFiltered}
              >
                全选全部匹配 ({filteredAliases.length})
              </button>
            )}
          </div>
          <div className="batch-action-right">
            <button
              type="button"
              className="btn btn-xs btn-primary"
              onClick={onOpenBatchEdit}
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

      <div className="table-responsive">
        <table className="table">
          <thead>
            <tr>
              <th style={{ width: 44, minWidth: 44, textAlign: 'center' }}>
                <input
                  type="checkbox"
                  className="table-checkbox"
                  checked={isAllPageSelected}
                  ref={(el) => {
                    if (el) el.indeterminate = isSomePageSelected
                  }}
                  onChange={toggleSelectPage}
                  title={isAllPageSelected ? '取消全选本页' : '全选本页'}
                />
              </th>
              <th style={{ width: '32%', minWidth: 220 }}>别名邮箱</th>
              <th style={{ width: '26%', minWidth: 150 }}>备注</th>
              <th style={{ width: '12%', minWidth: 90 }}>状态</th>
              <th style={{ width: '16%', minWidth: 130 }} aria-sort={sortDirection === 'asc' ? 'ascending' : 'descending'}>
                <button
                  type="button"
                  className="table-sort-button"
                  onClick={() => {
                    setSortDirection((dir) => (dir === 'asc' ? 'desc' : 'asc'))
                    setPage(1)
                  }}
                  aria-label={`创建时间排序：当前${sortDirection === 'asc' ? '正序' : '倒序'}，点击切换为${sortDirection === 'asc' ? '倒序' : '正序'}`}
                  title="点击切换创建时间排序"
                >
                  <span>创建时间</span>
                  {sortDirection === 'asc' ? <IconChevronUp size={14} /> : <IconChevronDown size={14} />}
                </button>
              </th>
              <th style={{ width: 175, minWidth: 175, textAlign: 'right' }}>操作</th>
            </tr>
          </thead>
          <tbody>
            {aliasLoading && aliases.length === 0 ? (
              Array.from({ length: 5 }).map((_, i) => (
                <tr key={`skel-${i}`}>
                  <td style={{ textAlign: 'center' }}>
                    <div className="skeleton-line" style={{ width: 16, height: 16, borderRadius: 3, margin: '0 auto', background: 'var(--color-border)', opacity: 0.5 }} />
                  </td>
                  <td><div className="skeleton-line" style={{ width: '70%', height: 14, borderRadius: 4, background: 'var(--color-border)', opacity: 0.5, animation: 'pulse 1.5s ease-in-out infinite' }} /></td>
                  <td><div className="skeleton-line" style={{ width: '40%', height: 14, borderRadius: 4, background: 'var(--color-border)', opacity: 0.4, animation: 'pulse 1.5s ease-in-out 0.1s infinite' }} /></td>
                  <td><div className="skeleton-line" style={{ width: '50px', height: 20, borderRadius: 10, background: 'var(--color-border)', opacity: 0.4, animation: 'pulse 1.5s ease-in-out 0.2s infinite' }} /></td>
                  <td><div className="skeleton-line" style={{ width: '60%', height: 14, borderRadius: 4, background: 'var(--color-border)', opacity: 0.3, animation: 'pulse 1.5s ease-in-out 0.3s infinite' }} /></td>
                  <td><div className="skeleton-line" style={{ width: '80px', height: 24, borderRadius: 4, background: 'var(--color-border)', opacity: 0.3, animation: 'pulse 1.5s ease-in-out 0.4s infinite' }} /></td>
                </tr>
              ))
            ) : filteredAliases.length === 0 ? (
              <tr>
                <td colSpan={6} className="text-center text-muted" style={{ padding: '32px' }}>
                  暂无匹配的别名
                </td>
              </tr>
            ) : (
              paginatedAliases.map((item) => (
                <tr
                  key={item.anonymousId}
                  style={selectedIds.has(item.anonymousId) ? { background: 'var(--color-bg-subtle)' } : undefined}
                >
                  <td style={{ textAlign: 'center' }}>
                    <input
                      type="checkbox"
                      className="table-checkbox"
                      checked={selectedIds.has(item.anonymousId)}
                      onChange={() => toggleSelect(item.anonymousId)}
                      title="选择此别名"
                    />
                  </td>
                  <td>
                    <div className="alias-cell">
                      <span className="font-mono font-semibold">{item.email}</span>
                      <button
                        type="button"
                        className="btn-icon"
                        onClick={() => {
                          copyText(item.email)
                          onCopySuccess('别名邮箱已复制')
                        }}
                        title="复制邮箱"
                      >
                        <IconCopy size={13} />
                      </button>
                    </div>
                  </td>
                  <td>
                    <div className="alias-cell">
                      <span className="text-secondary alias-label-text" title={item.label || '无备注'}>
                        {item.label || '—'}
                      </span>
                      <button
                        type="button"
                        className="btn-icon"
                        onClick={() => onEditAlias(item)}
                        title="修改别名备注"
                      >
                        <IconEdit size={13} />
                      </button>
                    </div>
                  </td>
                  <td>
                    <span className={`status-pill ${item.active ? 'active' : 'pending'}`}>
                      <span className="status-dot" />
                      {item.active ? '活跃' : '已停用'}
                    </span>
                  </td>
                  <td className="text-xs text-secondary" title={formatFullDate(item.createdAt)}>
                    {formatDate(item.createdAt)}
                  </td>
                  <td style={{ textAlign: 'right' }}>
                    <div style={{ display: 'inline-flex', gap: '6px', alignItems: 'center', justifyContent: 'flex-end' }}>
                      <button
                        type="button"
                        className="btn btn-xs btn-secondary"
                        onClick={() => onReadMail(item.email)}
                        title="查看发给该别名的邮件"
                      >
                        <IconInbox size={12} /> 读信
                      </button>
                      <button
                        type="button"
                        className="btn btn-xs btn-ghost"
                        onClick={() => onEditAlias(item)}
                        title="修改别名备注"
                      >
                        <IconEdit size={12} /> 备注
                      </button>
                      <button
                        type="button"
                        className="btn btn-xs btn-ghost"
                        onClick={() => onToggleAlias(item)}
                      >
                        {item.active ? '停用' : '启用'}
                      </button>
                      <button
                        type="button"
                        className="btn-icon-danger"
                        onClick={() => onDeleteAlias(item)}
                        title="彻底删除别名"
                      >
                        <IconTrash size={13} />
                      </button>
                    </div>
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>

      {filteredAliases.length > 0 && (
        <div className="pagination-bar">
          <span className="pagination-info">
            共 <b>{filteredAliases.length}</b> 个别名，当前第 <b>{safePage}</b> / <b>{totalPages}</b> 页
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
              disabled={safePage <= 1}
              onClick={() => setPage(safePage - 1)}
            >
              <IconChevronLeft size={13} />
              <span>上一页</span>
            </button>
            <button
              type="button"
              className="pagination-btn"
              disabled={safePage >= totalPages}
              onClick={() => setPage(safePage + 1)}
            >
              <span>下一页</span>
              <IconChevronRight size={13} />
            </button>
          </div>
        </div>
      )}
    </div>
  )
}
