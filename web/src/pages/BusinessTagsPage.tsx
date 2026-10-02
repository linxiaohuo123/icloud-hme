/**
 * [INPUT]: 依赖 react-router-dom 的 Link, api/client 的 request/ApiError, api/types 的 BusinessTag/APIToken/APITokenRecord/CreatedAPIToken, components/AsyncState/ConfirmDialog/Dialog/Select/ToastProvider, utils/clipboard 的 copyText, utils/date 的 formatDate/formatFullDate/formatRelativeTime/isWithinWindow, utils/snippets 的 buildLeaseCommand/buildCurlSnippet/buildPythonSnippet
 * [OUTPUT]: 对外提供 BusinessTagsPage，创建/编辑期间锁定表单并防重复提交，隔离编辑会话，支持权限签发、检索与安全凭据展示
 * [POS]: web/src/pages 的核心页面，负责业务归类增删改查、出号流水入口与 API 令牌生成/作废/轮换
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { ApiError, request } from '../api/client'
import type { APIToken, APITokenRecord, BusinessTag, CreatedAPIToken } from '../api/types'
import AsyncState from '../components/AsyncState'
import ConfirmDialog from '../components/ConfirmDialog'
import Dialog from '../components/Dialog'
import Select from '../components/Select'
import { useToast } from '../components/ToastProvider'
import { useDialogSession } from '../hooks/useDialogSession'
import {
  IconClock,
  IconCopy,
  IconEdit,
  IconExternalLink,
  IconFileText,
  IconKey,
  IconPlus,
  IconRefresh,
  IconSearch,
  IconShield,
  IconTag,
  IconTrash,
} from '../components/icons'
import { copyText } from '../utils/clipboard'
import { dateTimestamp, formatDate, formatFullDate, formatRelativeTime, isWithinWindow } from '../utils/date'
import { buildCurlSnippet, buildLeaseCommand, buildPythonSnippet } from '../utils/snippets'

/** 活跃判定窗口：24 小时内有调用/领用活动视为活跃 */
const ACTIVE_WINDOW_MS = 24 * 60 * 60 * 1000
const PRESET_TAGS = ['tiktok', 'openai', 'discord', 'telegram', 'facebook']

function tagLabel(t?: BusinessTag | null): string {
  if (!t) return ''
  return t.tag || t.name || ''
}

// eslint-disable-next-line react-refresh/only-export-components
export function getTokenStatus(tok: APITokenRecord): 'revoked' | 'expired' | 'needs_rotation' | 'active' {
  if (tok.revoked_at && tok.revoked_at.trim() !== '') {
    return 'revoked'
  }
  if (tok.expires_at && tok.expires_at.trim() !== '') {
    const expTime = new Date(tok.expires_at).getTime()
    if (!isNaN(expTime) && expTime <= Date.now()) {
      return 'expired'
    }
  }
  if (tok.needs_rotation) {
    return 'needs_rotation'
  }
  return 'active'
}

type TokenStatusFilter = 'all' | 'active' | 'needs_rotation' | 'expired' | 'revoked'

export default function BusinessTagsPage() {
  const [tags, setTags] = useState<BusinessTag[]>([])
  const [tokens, setTokens] = useState<APIToken[]>([])
  const [tagsLoading, setTagsLoading] = useState(true)
  const [tokensLoading, setTokensLoading] = useState(true)
  const [tagsError, setTagsError] = useState('')
  const [tokensError, setTokensError] = useState('')

  // 页面工作台视图切换
  const [viewTab, setViewTab] = useState<'all' | 'tags' | 'tokens' | 'docs'>('all')

  // 表格内部即时检索
  const [tagSearch, setTagSearch] = useState('')
  const [tokenSearch, setTokenSearch] = useState('')
  const [tokenStatusFilter, setTokenStatusFilter] = useState<TokenStatusFilter>('all')

  // 新建业务标识
  const [newTagName, setNewTagName] = useState('')
  const [newTagDesc, setNewTagDesc] = useState('')
  const [savingTag, setSavingTag] = useState(false)
  const creatingTagRef = useRef(false)

  // 新建 API 令牌 (补齐 scopes 与 expires_in_days 权威契约)
  const [newTokenName, setNewTokenName] = useState('')
  const [newTokenScope, setNewTokenScope] = useState<'allocate,verify' | 'admin'>('allocate,verify')
  const [newTokenExpires, setNewTokenExpires] = useState<string>('')
  const [savingToken, setSavingToken] = useState(false)
  const creatingTokenRef = useRef(false)

  // 编辑业务标识
  const [editingTag, setEditingTag] = useState<BusinessTag | null>(null)
  const [editTagValue, setEditTagValue] = useState('')
  const [editDescValue, setEditDescValue] = useState('')
  const editSession = useDialogSession(Boolean(editingTag), editingTag?.id ?? '')
  const savingEdit = editSession.busy

  // 删除 / 作废确认
  const [deleteTagTarget, setDeleteTagTarget] = useState<BusinessTag | null>(null)
  const [deleteTokenTarget, setDeleteTokenTarget] = useState<APIToken | null>(null)
  const [purgeTokenTarget, setPurgeTokenTarget] = useState<APIToken | null>(null)
  const [deletingTag, setDeletingTag] = useState(false)
  const [deletingToken, setDeletingToken] = useState(false)
  const [purgingToken, setPurgingToken] = useState(false)
  const [rotatingTokenId, setRotatingTokenId] = useState<string | null>(null)

  // 一次性明文密钥揭示
  const [createdTokenVal, setCreatedTokenVal] = useState('')
  const [docTag, setDocTag] = useState('')
  const [docTokenId, setDocTokenId] = useState<string>('')
  const [activeLang, setActiveLang] = useState<'curl' | 'python'>('curl')

  const tokenBannerRef = useRef<HTMLDivElement>(null)
  const { show, showCopyable } = useToast()

  const loadTags = useCallback(async () => {
    setTagsLoading(true)
    setTagsError('')
    try {
      const data = await request<BusinessTag[]>('/api/tags')
      setTags(Array.isArray(data) ? data : [])
    } catch (err) {
      setTagsError(err instanceof ApiError ? err.message : '加载业务标识失败')
    } finally {
      setTagsLoading(false)
    }
  }, [])

  const loadTokens = useCallback(async () => {
    setTokensLoading(true)
    setTokensError('')
    try {
      const data = await request<APIToken[]>('/api/tokens')
      setTokens(Array.isArray(data) ? data : [])
    } catch (err) {
      setTokensError(err instanceof ApiError ? err.message : '加载外部令牌失败')
    } finally {
      setTokensLoading(false)
    }
  }, [])

  // 定时器触发加载：避免 effect 同步体里直接 setState（级联渲染）
  useEffect(() => {
    const timer = setTimeout(() => {
      void loadTags()
      void loadTokens()
    }, 0)
    return () => clearTimeout(timer)
  }, [loadTags, loadTokens])

  // 令牌仅显示一次，生成后主动滚动到横幅确保用户看到
  useEffect(() => {
    if (createdTokenVal) {
      tokenBannerRef.current?.scrollIntoView({ behavior: 'smooth', block: 'center' })
    }
  }, [createdTokenVal])

  // 文档卡业务标识：所选 tag 被删除后自动回退到第一条（派生值，不用 effect 同步）
  const docTagValid = tags.some((t) => tagLabel(t) === docTag)
  const activeDocTag = docTagValid
    ? docTag
    : tags.length > 0
      ? tagLabel(tags[0])
      : 'tiktok'

  // 代码示例令牌联动逻辑：若指定了已有令牌或新生成令牌则联动带入
  const effectiveTokenForSnippet = useMemo(() => {
    if (docTokenId === '__placeholder__') return '<YOUR_API_TOKEN>'
    if (createdTokenVal && (!docTokenId || docTokenId === '__created__')) {
      return createdTokenVal
    }
    if (docTokenId && docTokenId !== '__placeholder__') {
      const found = tokens.find((t) => t.id === docTokenId)
      if (found) {
        return found.token_prefix ? `${found.token_prefix}****` : found.id
      }
    }
    return createdTokenVal || '<YOUR_API_TOKEN>'
  }, [createdTokenVal, docTokenId, tokens])

  const curlSnippet = buildCurlSnippet(window.location.origin, activeDocTag, effectiveTokenForSnippet)
  const pythonSnippet = buildPythonSnippet(window.location.origin, activeDocTag, effectiveTokenForSnippet)

  const lastActivity = [
    ...tags.map((t) => t.last_assigned_at || ''),
    ...tokens.map((t) => t.last_used_at || ''),
  ]
    .filter(Boolean)
    .sort((a, b) => (dateTimestamp(a) ?? 0) - (dateTimestamp(b) ?? 0))
    .pop()
  // 与出号记录页同一口径：24 小时内有调用视为活跃
  const recentActive = isWithinWindow(lastActivity, ACTIVE_WINDOW_MS)

  // 顶部 Bento 状态统计
  const activeAssignedTagsCount = useMemo(() => tags.filter((t) => Boolean(t.last_assigned_at)).length, [tags])
  const activeTokensCount = useMemo(() => tokens.filter((tok) => getTokenStatus(tok) === 'active').length, [tokens])
  const needsRotationCount = useMemo(() => tokens.filter((tok) => tok.needs_rotation).length, [tokens])

  // 过滤后的列表
  const filteredTags = useMemo(() => {
    const q = tagSearch.trim().toLowerCase()
    if (!q) return tags
    return tags.filter(
      (t) => tagLabel(t).toLowerCase().includes(q) || (t.description || '').toLowerCase().includes(q),
    )
  }, [tags, tagSearch])

  // 快捷预设场景推荐（已存在的标识自动过滤剔除）
  const unaddedPresets = useMemo(() => {
    const existing = new Set(tags.map((t) => tagLabel(t).toLowerCase().trim()))
    return PRESET_TAGS.filter((p) => !existing.has(p)).slice(0, 4)
  }, [tags])

  const filteredTokens = useMemo(() => {
    const q = tokenSearch.trim().toLowerCase()
    return tokens.filter((tok) => {
      const status = getTokenStatus(tok)
      if (tokenStatusFilter !== 'all' && status !== tokenStatusFilter) return false
      if (!q) return true
      return (
        tok.name.toLowerCase().includes(q) ||
        tok.id.toLowerCase().includes(q) ||
        (tok.token_prefix || '').toLowerCase().includes(q)
      )
    })
  }, [tokens, tokenSearch, tokenStatusFilter])

  async function handleCreateTag(e: React.FormEvent) {
    e.preventDefault()
    if (creatingTagRef.current || !newTagName.trim()) return
    creatingTagRef.current = true
    setSavingTag(true)
    try {
      await request('/api/tags', {
        method: 'POST',
        body: {
          tag: newTagName.trim(),
          name: newTagName.trim(),
          description: newTagDesc.trim(),
        },
      })
      show(`业务标识 [${newTagName}] 已创建`)
      setNewTagName('')
      setNewTagDesc('')
      await loadTags()
    } catch (err) {
      show(err instanceof ApiError ? err.message : '创建业务标识失败')
    } finally {
      creatingTagRef.current = false
      setSavingTag(false)
    }
  }

  function openEditDialog(t: BusinessTag) {
    if (editSession.sessionRef.current?.pending) return
    setEditingTag(t)
    setEditTagValue(tagLabel(t))
    setEditDescValue(t.description || '')
  }

  function closeEditDialog() {
    if (!editSession.sessionRef.current?.pending) setEditingTag(null)
  }

  async function handleSaveEdit(e: React.FormEvent) {
    e.preventDefault()
    if (!editingTag || !editTagValue.trim()) return
    const session = editSession.begin()
    if (!session) return
    const targetID = editingTag.id
    const payload = { tag: editTagValue.trim(), name: editTagValue.trim(), description: editDescValue.trim() }
    try {
      await request(`/api/tags/${encodeURIComponent(targetID)}`, {
        method: 'PATCH',
        body: payload,
      })
      if (!session.active) return
      show(`业务标识 [${payload.tag}] 已更新`)
      setEditingTag(null)
      await loadTags()
    } catch (err) {
      if (session.active) show(err instanceof ApiError ? err.message : '更新业务标识失败')
    } finally {
      editSession.finish(session)
    }
  }

  async function confirmDeleteTag() {
    if (!deleteTagTarget || deletingTag) return
    setDeletingTag(true)
    try {
      await request(`/api/tags/${encodeURIComponent(deleteTagTarget.id)}`, { method: 'DELETE' })
      show(`业务标识 [${tagLabel(deleteTagTarget)}] 已删除`)
      setDeleteTagTarget(null)
      await loadTags()
    } catch (err) {
      show(err instanceof ApiError ? err.message : '删除失败')
    } finally {
      setDeletingTag(false)
    }
  }

  async function handleCreateToken(e: React.FormEvent) {
    e.preventDefault()
    if (creatingTokenRef.current || !newTokenName.trim()) return
    creatingTokenRef.current = true
    setSavingToken(true)
    try {
      const payload: { name: string; scopes: string; expires_in_days?: number } = {
        name: newTokenName.trim(),
        scopes: newTokenScope || 'allocate,verify',
      }
      if (newTokenExpires) {
        const days = Number(newTokenExpires)
        if (!isNaN(days) && days > 0) {
          payload.expires_in_days = days
        }
      }

      const created = await request<CreatedAPIToken>('/api/tokens', {
        method: 'POST',
        body: payload,
      })
      if (created.token) {
        setCreatedTokenVal(created.token)
        setDocTokenId('__created__')
      }
      show(`令牌 [${newTokenName}] 已生成`)
      setNewTokenName('')
      setNewTokenExpires('')
      await loadTokens()
    } catch (err) {
      show(err instanceof ApiError ? err.message : '生成令牌失败')
    } finally {
      creatingTokenRef.current = false
      setSavingToken(false)
    }
  }

  async function handleRotateToken(tok: APITokenRecord) {
    if (rotatingTokenId) return
    setRotatingTokenId(tok.id)
    try {
      const res = await request<CreatedAPIToken>(`/api/tokens/${encodeURIComponent(tok.id)}/rotate`, {
        method: 'POST',
      })
      if (res.token) {
        setCreatedTokenVal(res.token)
        setDocTokenId('__created__')
      }
      show(`令牌 [${tok.name}] 已轮换，新令牌已生成`)
      await loadTokens()
    } catch (err) {
      show(err instanceof ApiError ? err.message : '轮换令牌失败')
    } finally {
      setRotatingTokenId(null)
    }
  }

  async function confirmDeleteToken() {
    if (!deleteTokenTarget || deletingToken) return
    setDeletingToken(true)
    try {
      await request(`/api/tokens/${encodeURIComponent(deleteTokenTarget.id)}`, { method: 'DELETE' })
      show(`令牌 [${deleteTokenTarget.name}] 已作废`)
      setDeleteTokenTarget(null)
      await loadTokens()
    } catch (err) {
      show(err instanceof ApiError ? err.message : '删除令牌失败')
    } finally {
      setDeletingToken(false)
    }
  }

  async function confirmPurgeToken() {
    if (!purgeTokenTarget || purgingToken) return
    setPurgingToken(true)
    try {
      await request(`/api/tokens/${encodeURIComponent(purgeTokenTarget.id)}?purge=true`, { method: 'DELETE' })
      show(`令牌 [${purgeTokenTarget.name}] 记录已彻底删除`)
      setPurgeTokenTarget(null)
      await loadTokens()
    } catch (err) {
      show(err instanceof ApiError ? err.message : '彻底删除令牌失败')
    } finally {
      setPurgingToken(false)
    }
  }

  const handleCopyPrefix = async (prefixText: string) => {
    const ok = await copyText(prefixText)
    if (ok) {
      show('凭据标识已复制')
    } else {
      showCopyable(prefixText, '请手动复制凭据标识')
    }
  }

  return (
    <div className="page-container">
      <div className="page-header">
        <div>
          <h1 className="page-title">业务标识与 API 令牌</h1>
          <p className="page-desc">
            管理外部自动化脚本的 API 访问令牌与业务分组，实现出号流水的按线隔离与审计追踪
          </p>
        </div>
      </div>

      {/* 顶部指标统计 (Bento 结构，与全站控制台一致) */}
      <div className="stat-grid">
        <div className="stat-card stat-card-blue">
          <div className="stat-card-header">
            <div className="stat-card-title-group">
              <div className="stat-icon">
                <IconTag size={16} />
              </div>
              <span className="stat-label">业务标识</span>
            </div>
            <span className="stat-badge stat-badge-blue">
              {tags.length > 0 ? `● ${activeAssignedTagsCount} 条已出号` : '● 待添加'}
            </span>
          </div>
          <div className="stat-value">{tags.length}</div>
          <div className="stat-subtext">划分项目与渠道流水</div>
        </div>

        <div className="stat-card stat-card-purple">
          <div className="stat-card-header">
            <div className="stat-card-title-group">
              <div className="stat-icon">
                <IconKey size={16} />
              </div>
              <span className="stat-label">API 访问令牌</span>
            </div>
            <span className={`stat-badge ${needsRotationCount > 0 ? 'stat-badge-amber' : activeTokensCount > 0 ? 'stat-badge-green' : 'stat-badge-amber'}`}>
              {needsRotationCount > 0
                ? `● ${needsRotationCount} 个待轮换`
                : activeTokensCount > 0
                  ? `● ${activeTokensCount} 个正常`
                  : '● 暂无令牌'}
            </span>
          </div>
          <div className="stat-value">{tokens.length}</div>
          <div className="stat-subtext">外部注册机鉴权凭据</div>
        </div>

        <div className="stat-card stat-card-green">
          <div className="stat-card-header">
            <div className="stat-card-title-group">
              <div className="stat-icon">
                <IconClock size={16} />
              </div>
              <span className="stat-label">最近调用活动</span>
            </div>
            <span className={`stat-badge ${recentActive ? 'stat-badge-green' : 'stat-badge-amber'}`}>
              {recentActive ? '● 24h 内有调用' : lastActivity ? '● 近期无调用' : '● 未接入'}
            </span>
          </div>
          <div className="stat-value" title={formatFullDate(lastActivity, '暂无调用记录')}>
            {lastActivity ? formatRelativeTime(lastActivity) : '暂无调用'}
          </div>
          <div className="stat-subtext">
            {lastActivity ? formatFullDate(lastActivity) : '外部注册机尚未接入'}
          </div>
        </div>

        <div
          role="button"
          tabIndex={0}
          className="stat-card stat-card-amber"
          onClick={() => {
            setViewTab('docs')
            document.querySelector('.api-docs-card')?.scrollIntoView({ behavior: 'smooth' })
          }}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault()
              setViewTab('docs')
              document.querySelector('.api-docs-card')?.scrollIntoView({ behavior: 'smooth' })
            }
          }}
          title="点击查看自动化接入示例"
        >
          <div className="stat-card-header">
            <div className="stat-card-title-group">
              <div className="stat-icon">
                <IconExternalLink size={16} />
              </div>
              <span className="stat-label">快速领号接口</span>
            </div>
            <span className="stat-badge stat-badge-green">● 就绪</span>
          </div>
          <div className="stat-value">已就绪</div>
          <div className="stat-subtext">POST /api/external/v2/allocate · 点击直达接入示例 ↓</div>
        </div>
      </div>

      {/* 一次性高危令牌凭据展示横幅 */}
      {createdTokenVal && (
        <div className="alert-banner alert-success token-created-banner" ref={tokenBannerRef}>
          <div className="alert-title">
            <IconShield size={16} className="text-success" />
            <span>新生成的外部 API 访问令牌（仅显示一次，请妥善保存）：</span>
          </div>
          <div className="token-reveal-row">
            <code>{createdTokenVal}</code>
            <button
              type="button"
              className="btn btn-sm btn-primary"
              onClick={async () => {
                const ok = await copyText(createdTokenVal)
                show(ok ? '令牌已复制到剪贴板' : '复制失败，请手动复制令牌')
              }}
            >
              <IconCopy size={13} /> 复制令牌
            </button>
            <button
              type="button"
              className="btn btn-sm btn-primary"
              onClick={async () => {
                const ok = await copyText(buildLeaseCommand(window.location.origin, activeDocTag, createdTokenVal))
                show(ok ? '完整接入命令已复制' : '复制失败，请手动复制接入命令')
              }}
            >
              <IconCopy size={13} /> 复制完整接入命令
            </button>
            <button
              type="button"
              className="btn btn-sm btn-ghost"
              onClick={() => setCreatedTokenVal('')}
            >
              关闭
            </button>
          </div>
          <div className="token-reveal-tip text-2xs text-secondary">
            注意：出于最小暴露安全原则，明文令牌不会在服务端持久保存，关闭或刷新后将无法找回。
          </div>
        </div>
      )}

      {/* 分段视图控制器：提供全景/独立聚焦切换 */}
      <div className="view-segmented-bar">
        <div className="segmented-filter" role="tablist" aria-label="页面视图切换">
          <button
            type="button"
            role="tab"
            aria-selected={viewTab === 'all'}
            className={`segmented-filter-btn ${viewTab === 'all' ? 'active' : ''}`}
            onClick={() => setViewTab('all')}
          >
            全部总览
          </button>
          <button
            type="button"
            role="tab"
            aria-selected={viewTab === 'tags'}
            className={`segmented-filter-btn ${viewTab === 'tags' ? 'active' : ''}`}
            onClick={() => setViewTab('tags')}
          >
            <IconTag size={13} />
            业务标识
            <span className="filter-count-badge">{tags.length}</span>
          </button>
          <button
            type="button"
            role="tab"
            aria-selected={viewTab === 'tokens'}
            className={`segmented-filter-btn ${viewTab === 'tokens' ? 'active' : ''}`}
            onClick={() => setViewTab('tokens')}
          >
            <IconKey size={13} />
            API 访问令牌
            <span className="filter-count-badge">{tokens.length}</span>
          </button>
          <button
            type="button"
            role="tab"
            aria-selected={viewTab === 'docs'}
            className={`segmented-filter-btn ${viewTab === 'docs' ? 'active' : ''}`}
            onClick={() => setViewTab('docs')}
          >
            <IconExternalLink size={13} />
            自动化接入指南
          </button>
        </div>
      </div>

      <div className={`tags-tokens-layout ${viewTab === 'all' ? 'grid-2-col' : 'single-col'}`}>
        {/* 左侧/独立：业务标识管理 */}
        {(viewTab === 'all' || viewTab === 'tags') && (
          <div className="card">
            <div className="card-header">
              <h2 className="card-title">
                <IconTag size={16} /> 业务标识 ({tags.length})
              </h2>
              <div className="card-header-stats">
                <span className="card-stat-pill">
                  <IconFileText size={12} />
                  <span>已分配 <strong>{activeAssignedTagsCount}</strong> 组</span>
                </span>
              </div>
            </div>

            {/* 新建业务标识输入栏 (双行黄金排布：Row 1 名称与描述，Row 2 快捷预设与添加按钮) */}
            <form onSubmit={(e) => void handleCreateTag(e)} className="form-inline-compact tag-creation-form">
              <div className="tag-creation-row1">
                <input
                  type="text"
                  className="input tag-name-input"
                  placeholder="标识名称 (例: tiktok)"
                  value={newTagName}
                  disabled={savingTag}
                  onChange={(e) => setNewTagName(e.target.value)}
                  required
                />
                <input
                  type="text"
                  className="input tag-desc-input"
                  placeholder="描述备注"
                  value={newTagDesc}
                  disabled={savingTag}
                  onChange={(e) => setNewTagDesc(e.target.value)}
                />
              </div>

              <div className="tag-creation-row2">
                <div className="tag-quick-presets">
                  {!tagsLoading && unaddedPresets.length > 0 && (
                    <>
                      <span className="tag-quick-presets-label">快速预设:</span>
                      {unaddedPresets.map((preset) => (
                        <button
                          key={preset}
                          type="button"
                          className="tag-quick-chip"
                          disabled={savingTag}
                          onClick={() => {
                            setNewTagName(preset)
                            setNewTagDesc(`${preset} 自动化出号任务`)
                          }}
                          title={`点击填入 ${preset}`}
                        >
                          <IconPlus size={10} />
                          <span>{preset}</span>
                        </button>
                      ))}
                    </>
                  )}
                </div>
                <button type="submit" className="btn btn-primary tag-submit-btn" disabled={savingTag}>
                  <IconPlus size={14} /> 添加标识
                </button>
              </div>
            </form>

            {/* 列表轻量搜索栏 */}
            {tags.length > 5 && (
              <div className="card-filter-toolbar">
                <div className="search-input-wrapper">
                  <IconSearch size={13} className="search-input-icon text-muted" />
                  <input
                    type="search"
                    className="input input-sm search-input-field"
                    placeholder="按标识名称或备注筛选..."
                    value={tagSearch}
                    onChange={(e) => setTagSearch(e.target.value)}
                  />
                </div>
              </div>
            )}

            <AsyncState
              loading={tagsLoading}
              error={tagsError}
              empty={tags.length === 0}
              emptyText="暂无业务标识，请在上方添加"
              onRetry={() => void loadTags()}
            >
              <div className="table-responsive">
                <table className="table">
                  <thead>
                    <tr>
                      <th>标识</th>
                      <th>描述</th>
                      <th>最近领用</th>
                      <th>操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    {filteredTags.length === 0 && tags.length > 0 ? (
                      <tr>
                        <td colSpan={4} className="text-center text-secondary py-4">
                          未找到匹配「{tagSearch}」的业务标识
                        </td>
                      </tr>
                    ) : (
                      filteredTags.map((t) => (
                        <tr key={t.id || tagLabel(t)}>
                          <td className="tag-name-cell">
                            <span className="tag-badge-pill" title={tagLabel(t)}>
                              {tagLabel(t)}
                            </span>
                          </td>
                          <td className="text-secondary">{t.description || '—'}</td>
                          <td
                            className="text-xs text-secondary"
                            title={formatFullDate(t.last_assigned_at, '从未领用')}
                          >
                            {formatRelativeTime(t.last_assigned_at, '从未领用')}
                          </td>
                          <td>
                            <div className="row-actions">
                              <Link
                                to={`/used?tag=${encodeURIComponent(tagLabel(t))}`}
                                title="查看出号记录"
                              >
                                <IconFileText size={14} /> 出号
                              </Link>
                              <button type="button" onClick={() => openEditDialog(t)} title="编辑标识">
                                <IconEdit size={14} /> 编辑
                              </button>
                              <button
                                type="button"
                                className="danger"
                                onClick={() => setDeleteTagTarget(t)}
                                title="删除标识"
                              >
                                <IconTrash size={14} /> 删除
                              </button>
                            </div>
                          </td>
                        </tr>
                      ))
                    )}
                  </tbody>
                </table>
              </div>
            </AsyncState>

            {/* 卡片底栏收口：轻量提示与直达流水，拒绝大块死灰 */}
            <div className="card-footer-strip">
              <div className="card-footer-tip">
                <span className="status-dot status-dot-blue" />
                <span>外部请求传入 <code>tag</code> 即可自动按业务线隔离出号流水</span>
              </div>
              <Link to="/used" className="card-footer-link">
                <span>出号记录流水</span>
                <IconExternalLink size={12} />
              </Link>
            </div>
          </div>
        )}

        {/* 右侧/独立：外部令牌管理 */}
        {(viewTab === 'all' || viewTab === 'tokens') && (
          <div className="card">
            <div className="card-header">
              <h2 className="card-title">
                <IconKey size={16} /> API 访问令牌 ({tokens.length})
              </h2>
              <div className="card-header-stats">
                {needsRotationCount > 0 && (
                  <span className="card-stat-pill is-warning">
                    <span className="status-ribbon-dot" />
                    <span>{needsRotationCount} 个待轮换</span>
                  </span>
                )}
                <span className="card-stat-pill">
                  <span>有效 <strong>{activeTokensCount}</strong> 个</span>
                </span>
              </div>
            </div>

            {/* 新建令牌输入栏 (双行黄金排布：Row 1 令牌备注，Row 2 权限、有效期与生成按钮) */}
            <form onSubmit={(e) => void handleCreateToken(e)} className="form-inline-compact token-creation-form">
              <div className="token-creation-row1">
                <input
                  type="text"
                  className="input token-name-input"
                  placeholder="令牌备注 (例: 注册机-01)"
                  value={newTokenName}
                  disabled={savingToken}
                  onChange={(e) => setNewTokenName(e.target.value)}
                  required
                />
              </div>
              <div className="token-creation-row2">
                <Select
                  aria-label="令牌权限"
                  size="sm"
                  value={newTokenScope}
                  disabled={savingToken}
                  onChange={(val) => setNewTokenScope(val as 'allocate,verify' | 'admin')}
                  options={[
                    { value: 'allocate,verify', label: '出号与取码 (推荐)' },
                    { value: 'admin', label: '全站管理员 (高危)' },
                  ]}
                />
                <Select
                  aria-label="令牌有效期"
                  size="sm"
                  value={newTokenExpires}
                  disabled={savingToken}
                  onChange={(val) => setNewTokenExpires(val)}
                  options={[
                    { value: '', label: '永久有效' },
                    { value: '7', label: '有效期 7 天' },
                    { value: '30', label: '有效期 30 天' },
                    { value: '90', label: '有效期 90 天' },
                    { value: '180', label: '有效期 180 天' },
                  ]}
                />
                <button type="submit" className="btn btn-primary token-submit-btn" disabled={savingToken}>
                  <IconPlus size={14} /> 生成令牌
                </button>
              </div>
            </form>

            {/* 令牌表格过滤栏 (统一使用 filter-bar 与 Select 下拉) */}
            {tokens.length > 5 && (
              <div className="filter-bar card-filter-toolbar">
                <div className="search-input-wrapper">
                  <IconSearch size={13} className="search-input-icon text-muted" />
                  <input
                    type="search"
                    className="input input-sm search-input-field"
                    placeholder="按备注或前缀筛选..."
                    value={tokenSearch}
                    onChange={(e) => setTokenSearch(e.target.value)}
                  />
                </div>
                <div className="filter-item">
                  <Select
                    aria-label="令牌状态筛选"
                    size="sm"
                    value={tokenStatusFilter}
                    onChange={(val) => setTokenStatusFilter(val as TokenStatusFilter)}
                    options={[
                      { value: 'all', label: '全部状态' },
                      { value: 'active', label: '正常' },
                      ...(needsRotationCount > 0 ? [{ value: 'needs_rotation', label: '待轮换' }] : []),
                      { value: 'expired', label: '已过期' },
                      { value: 'revoked', label: '已作废' },
                    ]}
                    style={{ minWidth: 110 }}
                  />
                </div>
              </div>
            )}

            <AsyncState
              loading={tokensLoading}
              error={tokensError}
              empty={tokens.length === 0}
              emptyText="暂无外部令牌，点击上方按钮生成"
              onRetry={() => void loadTokens()}
            >
              <div className="table-responsive">
                <table className="table">
                  <thead>
                    <tr>
                      <th>令牌标识</th>
                      <th>权限范围</th>
                      <th>状态</th>
                      <th>最后调用时间</th>
                      <th>操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    {filteredTokens.length === 0 && tokens.length > 0 ? (
                      <tr>
                        <td colSpan={5} className="text-center text-secondary py-4">
                          未找到匹配的外部令牌
                        </td>
                      </tr>
                    ) : (
                      filteredTokens.map((tok) => {
                        const status = getTokenStatus(tok)
                        const prefixText = tok.token_prefix ? `${tok.token_prefix}****` : tok.id
                        return (
                          <tr key={tok.id}>
                            <td>
                              <div className="token-cell-name">{tok.name}</div>
                              <div className="token-cell-prefix-row">
                                <span className="token-cell-prefix">{prefixText}</span>
                                <button
                                  type="button"
                                  className="token-prefix-copy-btn"
                                  onClick={() => void handleCopyPrefix(prefixText)}
                                  title="复制前缀标识"
                                >
                                  <IconCopy size={11} />
                                </button>
                              </div>
                            </td>
                            <td>
                              {tok.scopes === 'admin' ? (
                                <span className="badge badge-warning font-mono" title="全局管理员作用域 (高危)">
                                  admin
                                </span>
                              ) : (
                                <span className="badge badge-primary-soft font-mono" title="仅限出号与关联取码 (安全推荐)">
                                  {tok.scopes || 'allocate,verify'}
                                </span>
                              )}
                            </td>
                            <td>
                              {status === 'revoked' && (
                                <span className="badge badge-error">已作废</span>
                              )}
                              {status === 'expired' && (
                                <span className="badge badge-error">已过期</span>
                              )}
                              {status === 'needs_rotation' && (
                                <span className="badge badge-warning" title="历史遗留高危令牌，建议立即轮换">
                                  待轮换
                                </span>
                              )}
                              {status === 'active' && (
                                <span className="badge badge-active">正常</span>
                              )}
                            </td>
                            <td className="token-cell-meta" title={formatFullDate(tok.last_used_at, '从未调用')}>
                              <div>{formatDate(tok.last_used_at, '从未调用')}</div>
                              {tok.expires_at ? (
                                <div className="token-cell-expiry text-2xs text-secondary font-mono" title={`到期时间: ${formatFullDate(tok.expires_at)}`}>
                                  到期: {formatDate(tok.expires_at)}
                                </div>
                              ) : (
                                <div className="token-cell-expiry text-2xs text-tertiary">永久有效</div>
                              )}
                            </td>
                            <td>
                              <div className="row-actions">
                                {status !== 'revoked' && status !== 'expired' && (
                                  <>
                                    <button
                                      type="button"
                                      className="btn btn-xs btn-secondary row-rotate-btn"
                                      onClick={() => void handleRotateToken(tok)}
                                      disabled={rotatingTokenId === tok.id}
                                      title="轮换令牌"
                                    >
                                      <IconRefresh size={12} /> {rotatingTokenId === tok.id ? '轮换中…' : '轮换'}
                                    </button>
                                    <button
                                      type="button"
                                      className="danger"
                                      onClick={() => setDeleteTokenTarget(tok)}
                                      title="作废令牌"
                                    >
                                      <IconTrash size={14} /> 作废
                                    </button>
                                  </>
                                )}
                                {(status === 'revoked' || status === 'expired') && (
                                  <button
                                    type="button"
                                    className="danger"
                                    onClick={() => setPurgeTokenTarget(tok)}
                                    title="删除已作废令牌记录"
                                  >
                                    <IconTrash size={14} /> 删除
                                  </button>
                                )}
                              </div>
                            </td>
                          </tr>
                        )
                      })
                    )}
                  </tbody>
                </table>
              </div>
            </AsyncState>

            {/* 卡片底栏收口：轻量提示与直达接入示例 */}
            <div className="card-footer-strip">
              <div className="card-footer-tip">
                <span className="status-dot status-dot-green" />
                <span>外部注册机通过 HTTP 请求携带 API 访问令牌鉴权</span>
              </div>
              <button
                type="button"
                className="card-footer-link card-footer-action-btn"
                onClick={() => {
                  document.querySelector('.api-docs-card')?.scrollIntoView({ behavior: 'smooth' })
                }}
              >
                <span>接入指南代码</span>
                <IconExternalLink size={12} />
              </button>
            </div>
          </div>
        )}
      </div>

      {/* 底部：外部调用文档（随所选业务标识与令牌动态生成） */}
      {(viewTab === 'all' || viewTab === 'docs') && (
        <div className="card api-docs-card">
          <div className="card-header">
            <h2 className="card-title">
              <IconExternalLink size={16} /> 自动化注册机接入指南
            </h2>
            <div className="segmented-filter" role="group" aria-label="示例语言">
              <button
                type="button"
                className={`segmented-filter-btn ${activeLang === 'curl' ? 'active' : ''}`}
                aria-pressed={activeLang === 'curl'}
                onClick={() => setActiveLang('curl')}
              >
                cURL (标准协议)
              </button>
              <button
                type="button"
                className={`segmented-filter-btn ${activeLang === 'python' ? 'active' : ''}`}
                aria-pressed={activeLang === 'python'}
                onClick={() => setActiveLang('python')}
              >
                Python (完整流水线)
              </button>
            </div>
          </div>
          <div className="api-docs-body">
            <div className="api-docs-toolbar">
              <p className="api-snippet-desc">
                外部自动化脚本通过 HTTP 携带 API 令牌调用以下接口，即可从预热号池分配别名并提取验证码。支持动态传入任意 <code>tag</code> 参数；在上方预先登记可统一管理业务线说明：
              </p>

              <div className="filter-bar api-docs-filter-bar">
                <div className="filter-item">
                  <span className="filter-item-label">业务标识:</span>
                  <Select
                    aria-label="业务标识"
                    size="sm"
                    value={activeDocTag}
                    onChange={(val) => setDocTag(val)}
                    options={
                      tags.length === 0
                        ? [{ value: 'tiktok', label: '默认示例 (tiktok)' }]
                        : tags.map((t) => ({ value: tagLabel(t), label: tagLabel(t) }))
                    }
                    style={{ minWidth: 160 }}
                  />
                </div>

                {tokens.length > 0 && (
                  <div className="filter-item">
                    <span className="filter-item-label">示范令牌:</span>
                    <Select
                      aria-label="示范令牌"
                      size="sm"
                      value={docTokenId || (createdTokenVal ? '__created__' : '__placeholder__')}
                      onChange={(val) => setDocTokenId(val)}
                      options={[
                        ...(createdTokenVal
                          ? [
                              {
                                value: '__created__',
                                label: `刚刚生成 (${createdTokenVal.slice(0, 10)}…)`,
                              },
                            ]
                          : []),
                        ...tokens
                          .filter((t) => getTokenStatus(t) === 'active' || getTokenStatus(t) === 'needs_rotation')
                          .map((t) => ({
                            value: t.id,
                            label: `${t.name} (${t.token_prefix ? `${t.token_prefix}****` : t.id.slice(0, 8)})`,
                          })),
                        { value: '__placeholder__', label: '占位符 (<YOUR_API_TOKEN>)' },
                      ]}
                      style={{ minWidth: 200 }}
                    />
                  </div>
                )}
              </div>
            </div>

            <pre className="code-box">
              <code>{activeLang === 'curl' ? curlSnippet : pythonSnippet}</code>
            </pre>

            <div className="api-docs-copy-row">
              <button
                type="button"
                className="btn btn-sm btn-secondary"
                onClick={async () => {
                  const content = activeLang === 'curl' ? curlSnippet : pythonSnippet
                  const ok = await copyText(content)
                  show(
                    ok
                      ? activeLang === 'curl'
                        ? 'cURL 示例命令已复制'
                        : 'Python 自动化脚本已复制'
                      : '复制失败，请手动复制接入示例',
                  )
                }}
              >
                <IconCopy size={13} /> {activeLang === 'curl' ? '复制 cURL 命令' : '复制 Python 脚本'}
              </button>
            </div>
          </div>
        </div>
      )}

      {editingTag && (
        <Dialog
          title="编辑业务标识"
          open
          onClose={closeEditDialog}
        >
          <form onSubmit={(e) => void handleSaveEdit(e)}>
            <div className="form-field">
              <label htmlFor="edit-tag-name">标识名称</label>
              <input
                id="edit-tag-name"
                type="text"
                className="input"
                value={editTagValue}
                disabled={savingEdit}
                onChange={(e) => setEditTagValue(e.target.value)}
                required
                autoComplete="off"
              />
              <p className="hint">标识名称是外部脚本的调用参数（?tag=…）。已有母号引用时，请先调整母号标签；修改后需同步更新脚本配置</p>
            </div>
            <div className="form-field">
              <label htmlFor="edit-tag-desc">描述备注</label>
              <input
                id="edit-tag-desc"
                type="text"
                className="input"
                value={editDescValue}
                disabled={savingEdit}
                onChange={(e) => setEditDescValue(e.target.value)}
                autoComplete="off"
              />
            </div>
            <div className="form-actions">
              <button type="button" onClick={closeEditDialog} disabled={savingEdit}>
                取消
              </button>
              <button type="submit" className="primary" disabled={savingEdit || !editTagValue.trim()}>
                {savingEdit ? '保存中…' : '保存'}
              </button>
            </div>
          </form>
        </Dialog>
      )}

      {deleteTagTarget && (
        <ConfirmDialog
          title="删除业务标识"
          message={`确定删除业务标识 [${tagLabel(deleteTagTarget)}] 吗？删除后外部脚本将无法按此标识领号，历史出号流水不受影响。`}
          open={!!deleteTagTarget}
          onClose={() => setDeleteTagTarget(null)}
          onConfirm={() => void confirmDeleteTag()}
          busy={deletingTag}
        />
      )}
      {deleteTokenTarget && (
        <ConfirmDialog
          title="作废访问令牌"
          message={`确定作废令牌 [${deleteTokenTarget.name}] 吗？作废后使用该令牌的外部自动化脚本将立即失去调用权限。`}
          open={!!deleteTokenTarget}
          onClose={() => setDeleteTokenTarget(null)}
          onConfirm={() => void confirmDeleteToken()}
          busy={deletingToken}
        />
      )}
      {purgeTokenTarget && (
        <ConfirmDialog
          title="彻底删除令牌记录"
          confirmLabel="彻底删除"
          message={`确定彻底删除已作废令牌 [${purgeTokenTarget.name}] 的历史记录吗？删除后此记录将从列表中永久清除，不可恢复。`}
          open={!!purgeTokenTarget}
          onClose={() => setPurgeTokenTarget(null)}
          onConfirm={() => void confirmPurgeToken()}
          busy={purgingToken}
        />
      )}
    </div>
  )
}
