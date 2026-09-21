/**
 * [INPUT]: 依赖 react-router-dom 的 Link/useSearchParams, api/client 的 request/ApiError, api/types 的 LeaseRecord/LeaseListResult/BusinessTag, components/AsyncState, components/Select, components/ToastProvider 的 show/showCopyable, components/icons, utils/clipboard 的 copyText, utils/date 的 formatDate/formatFullDate/formatRelativeTime/parseDate
 * [OUTPUT]: 对外提供 UsedAliasesPage 已用别名流水审计组件 (卡片顶栏内嵌状态胶囊 + 状态中文映射 + 业务徽章着色 + 骨架屏/错误重试 + 服务端 SQLite 分页与可配置单页条数 + URL 业务标识筛选)
 * [POS]: web/src/pages 的核心页面，负责业务出号审计溯源与一键直跳收件箱
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useCallback, useEffect, useRef, useState, type CSSProperties } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { ApiError, request } from '../api/client'
import type { BusinessTag, LeaseListResult, LeaseRecord } from '../api/types'
import AsyncState from '../components/AsyncState'
import Select from '../components/Select'
import { useToast } from '../components/ToastProvider'
import {
  IconClock,
  IconCopy,
  IconChevronLeft,
  IconChevronRight,
  IconFileText,
  IconInbox,
  IconSearch,
  IconTag,
} from '../components/icons'
import { copyText } from '../utils/clipboard'
import { formatDate, formatFullDate, formatRelativeTime, parseDate } from '../utils/date'

/** 领用状态 → 中文文案 + 语义色；未收录的状态一律走中性徽章，绝不误报绿色 */
const STATUS_META: Record<string, { label: string; pill: string }> = {
  completed: { label: '已完成', pill: 'active' },
  allocated: { label: '已分配', pill: 'pending' },
  used: { label: '已使用', pill: 'pending' },
  revoked: { label: '已废弃', pill: 'error' },
  expired: { label: '已过期', pill: 'error' },
}

/** 业务标识名 → 稳定色相 (0-359)，同名永远同色，两个主题共用 */
function tagHue(tag: string): number {
  let hash = 0
  for (let i = 0; i < tag.length; i++) {
    hash = (hash * 31 + tag.charCodeAt(i)) % 360
  }
  return hash
}

/** 活跃判定窗口：24 小时内有出号视为近期活跃 */
const ACTIVE_WINDOW_MS = 24 * 60 * 60 * 1000

const PAGE_SIZE_OPTIONS = [
  { value: '20', label: '20 条 / 页' },
  { value: '50', label: '50 条 / 页' },
  { value: '100', label: '100 条 / 页' },
]

export default function UsedAliasesPage() {
  // 支持从「业务标识」页 ?tag=xxx 深链直达对应业务流水，筛选变化同步回 URL
  const [searchParams, setSearchParams] = useSearchParams()
  const [leases, setLeases] = useState<LeaseRecord[]>([])
  const [total, setTotal] = useState(0)
  const [tags, setTags] = useState<BusinessTag[]>([])
  const [tagsError, setTagsError] = useState(false)
  const [selectedTag, setSelectedTag] = useState(() => searchParams.get('tag') ?? '')
  const [search, setSearch] = useState('')
  // loading 仅用于首屏/无数据时的骨架屏；翻页与筛选保留旧表格，用 refreshing 做半透明遮罩
  const [loading, setLoading] = useState(true)
  const [refreshing, setRefreshing] = useState(false)
  const [error, setError] = useState('')
  const [retryKey, setRetryKey] = useState(0)
  const { show, showCopyable } = useToast()

  // 全量口径指标 (不带筛选的 total 与最近一条领用)，供顶部指标卡使用
  const [grandTotal, setGrandTotal] = useState<number | null>(null)
  const [latestAllocatedAt, setLatestAllocatedAt] = useState<string | null>(null)
  const [recentActive, setRecentActive] = useState(false)

  // 服务端分页状态
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState<number>(() => {
    try {
      const saved = Number(localStorage.getItem('icloud_hme_used_alias_page_size'))
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
      localStorage.setItem('icloud_hme_used_alias_page_size', String(nextSize))
    } catch {
      // ignore
    }
  }

  const leasesRef = useRef<LeaseRecord[]>([])
  useEffect(() => {
    leasesRef.current = leases
  }, [leases])
  const seqRef = useRef(0)

  // 挂载后 URL 的 tag 参数变化时 (如同页二次跳转/浏览器后退)，在渲染期同步筛选状态
  const urlTag = searchParams.get('tag') ?? ''
  const [prevUrlTag, setPrevUrlTag] = useState(urlTag)
  if (prevUrlTag !== urlTag) {
    setPrevUrlTag(urlTag)
    setSelectedTag(urlTag)
    // 换了筛选条件后原页码可能超出总页数,必须回到第 1 页
    setPage(1)
  }

  // 1. 初始化加载标签列表；失败降级为"仅全部业务标识"，不阻塞流水列表
  useEffect(() => {
    let cancelled = false
    request<BusinessTag[]>('/api/tags')
      .then((tagData) => {
        if (cancelled) return
        setTags(Array.isArray(tagData) ? tagData : [])
        setTagsError(false)
      })
      .catch(() => {
        if (cancelled) return
        setTagsError(true)
      })
    return () => {
      cancelled = true
    }
  }, [])

  // 2. 全量口径指标：一次 limit=1 请求同时取回未筛选总数与最近一条领用
  useEffect(() => {
    let cancelled = false
    request<LeaseListResult>('/api/leases?limit=1')
      .then((res) => {
        if (cancelled) return
        setGrandTotal(typeof res?.total === 'number' ? res.total : 0)
        const latest = res?.records?.[0]?.allocated_at ?? null
        setLatestAllocatedAt(latest)
        const ts = parseDate(latest)
        setRecentActive(ts !== null && Date.now() - ts.getTime() < ACTIVE_WINDOW_MS)
      })
      .catch(() => {
        // 指标卡显示 '—'，不打扰主列表
      })
    return () => {
      cancelled = true
    }
  }, [retryKey])

  // 3. 根据分页、标签与搜索条件，向后端 SQLite 发起检索（150ms 防抖 + 请求序号防竞态）
  useEffect(() => {
    const seq = ++seqRef.current
    const controller = new AbortController()
    const timer = setTimeout(() => {
      if (seq !== seqRef.current) return
      setError('')
      if (leasesRef.current.length === 0) {
        setLoading(true)
      } else {
        setRefreshing(true)
      }

      const params = new URLSearchParams()
      if (search.trim()) params.set('alias', search.trim())
      if (selectedTag) params.set('tag', selectedTag)
      params.set('limit', String(pageSize))
      params.set('offset', String((page - 1) * pageSize))

      request<LeaseListResult>(`/api/leases?${params.toString()}`, { signal: controller.signal })
        .then((res) => {
          if (seq !== seqRef.current) return
          setLeases(Array.isArray(res?.records) ? res.records : [])
          setTotal(typeof res?.total === 'number' ? res.total : 0)
          setLoading(false)
          setRefreshing(false)
        })
        .catch((err) => {
          if (controller.signal.aborted || seq !== seqRef.current) return
          setError(err instanceof ApiError ? err.message : '加载审计流水失败，请检查服务状态')
          setLeases([])
          setTotal(0)
          setLoading(false)
          setRefreshing(false)
        })
    }, 150)

    return () => {
      clearTimeout(timer)
      controller.abort()
    }
  }, [page, pageSize, selectedTag, search, retryKey])

  const totalPages = Math.max(1, Math.ceil(total / pageSize))
  const safePage = Math.min(Math.max(1, page), totalPages)

  const copyEmail = useCallback(
    async (email: string) => {
      if (await copyText(email)) {
        show('已复制别名邮箱')
      } else {
        showCopyable(email, '复制失败，请手动复制')
      }
    },
    [show, showCopyable],
  )

  function handleRetry() {
    setRetryKey((k) => k + 1)
  }

  const hasFilter = Boolean(selectedTag || search.trim())

  return (
    <div className="page-container">
      <div className="page-header">
        <div>
          <h1 className="page-title">出号记录</h1>
          <p className="page-desc">
            追踪每个业务标识领取的 HME 别名流水，支持一键直达对应账号收件箱读取验证码
          </p>
        </div>
      </div>

      <div className="card">
        <div className="card-header">
          <h2 className="card-title">
            <IconFileText size={16} />
            <span>累计出号流水</span>
            <span className="card-title-count">(<strong>{grandTotal ?? total}</strong>)</span>
          </h2>
          <div className="card-header-stats">
            <span className={`card-stat-pill ${recentActive ? 'is-healthy' : ''}`}>
              <IconClock size={12} />
              <span>
                {latestAllocatedAt ? `最近出号: ${formatRelativeTime(latestAllocatedAt)}` : '暂无近期出号'}
              </span>
            </span>

            <span className="card-stat-pill">
              <IconTag size={12} />
              <span>
                <strong>{tags.length}</strong> 个业务标识
              </span>
              {tagsError && (
                <span className="card-stat-dim" style={{ color: 'var(--color-warning)' }}>
                  (加载失败)
                </span>
              )}
            </span>

            {hasFilter && (
              <span className="card-stat-pill is-filter">
                <IconSearch size={12} />
                <span>
                  匹配 <strong>{total}</strong> 条
                </span>
              </span>
            )}
          </div>
        </div>
        <div className="filter-bar">
          <div className="filter-item">
            <Select
              aria-label="按业务标识筛选"
              value={selectedTag}
              onChange={(next) => {
                setSelectedTag(next)
                setPage(1)
                setSearchParams(next ? { tag: next } : {}, { replace: true })
              }}
              options={[
                { value: '', label: '全部业务标识' },
                ...tags.map((t) => ({
                  value: t.tag || t.name,
                  label: t.tag || t.name,
                })),
              ]}
              style={{ minWidth: 180 }}
            />
          </div>
          <div className="filter-item filter-search">
            <IconSearch size={16} />
            <input
              type="text"
              className="input"
              placeholder="搜索别名邮箱 / 账号 ID…"
              aria-label="搜索别名邮箱或账号 ID"
              value={search}
              onChange={(e) => {
                setSearch(e.target.value)
                setPage(1)
              }}
            />
          </div>
        </div>

        <AsyncState
          loading={loading}
          error={error}
          empty={leases.length === 0}
          emptyText={hasFilter ? '没有匹配的领用流水，试试调整筛选条件' : '暂无别名领用流水'}
          emptyAction={
            !hasFilter ? (
              <Link className="btn btn-sm btn-soft-primary" to="/accounts">
                前往账号页生成别名
              </Link>
            ) : undefined
          }
          onRetry={handleRetry}
        >
          <div className={refreshing ? 'table-shell is-refreshing' : 'table-shell'}>
            <div className="table-responsive">
              <table className="table">
                <thead>
                  <tr>
                    <th style={{ width: '18%', minWidth: 140 }}>领用时间</th>
                    <th style={{ width: '16%', minWidth: 110 }}>业务标识</th>
                    <th style={{ width: '28%', minWidth: 220 }}>别名邮箱</th>
                    <th style={{ width: '16%', minWidth: 110 }}>所属账号</th>
                    <th style={{ width: '12%', minWidth: 90 }}>状态</th>
                    <th style={{ width: 100, minWidth: 100, textAlign: 'right' }}>快捷操作</th>
                  </tr>
                </thead>
                <tbody>
                  {leases.map((r) => {
                    const statusMeta = STATUS_META[(r.status || '').toLowerCase()]
                    return (
                      <tr key={r.id}>
                        <td
                          className="text-xs text-secondary"
                          title={formatFullDate(r.allocated_at)}
                        >
                          {formatDate(r.allocated_at)}
                        </td>
                        <td>
                          <span
                            className="badge badge-tag"
                            style={{ '--tag-hue': String(tagHue(r.tag || '默认')) } as CSSProperties}
                          >
                            {r.tag || '默认'}
                          </span>
                        </td>
                        <td>
                          <button
                            type="button"
                            className="link-like font-mono font-semibold"
                            onClick={() => void copyEmail(r.email)}
                            title="点击复制邮箱"
                          >
                            <span>{r.email}</span>
                            <IconCopy size={11} className="copy-hint-icon" />
                          </button>
                        </td>
                        <td className="text-xs text-secondary font-mono">{r.account_id}</td>
                        <td>
                          {statusMeta ? (
                            <span className={`status-pill ${statusMeta.pill}`}>
                              <span className="status-dot" />
                              {statusMeta.label}
                            </span>
                          ) : (
                            <span className="badge badge-neutral">{r.status || '已分配'}</span>
                          )}
                        </td>
                        <td style={{ textAlign: 'right' }}>
                          <Link
                            to={`/workspace/${encodeURIComponent(r.account_id)}?tab=inbox&alias=${encodeURIComponent(r.email)}`}
                            className="btn btn-xs btn-primary-soft"
                            title="前往收件箱查看验证码"
                            style={{ display: 'inline-flex', alignItems: 'center', gap: 4, textDecoration: 'none' }}
                          >
                            <IconInbox size={12} />
                            <span>收件箱</span>
                          </Link>
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>

            {/* 分页控制栏 (无数据时隐藏) */}
            {total > 0 && (
              <div className="pagination-bar">
                <span className="pagination-info">
                  共 <b>{total}</b> 条流水，当前第 <b>{safePage}</b> / <b>{totalPages}</b> 页
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
        </AsyncState>
      </div>
    </div>
  )
}
