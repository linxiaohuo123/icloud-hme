/**
 * [INPUT]: 依赖 react-router-dom 的 Link, api/client 的 request/ApiError, api/types 的 BusinessTag/APIToken, components/AsyncState/ConfirmDialog/Dialog/ToastProvider, utils/clipboard 的 copyText, utils/date 的 formatDate/formatFullDate/formatRelativeTime/isWithinWindow, utils/snippets 的 buildLeaseCommand/buildCurlSnippet/buildPythonSnippet
 * [OUTPUT]: 对外提供 BusinessTagsPage 业务标识与 API 令牌管理组件 (Bento 指标卡 + 双栏 CRUD + 自动化接入指南)
 * [POS]: web/src/pages 的核心页面，负责业务归类增删改查、出号流水入口与 API 令牌生成/作废
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useCallback, useEffect, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { ApiError, request } from '../api/client'
import type { APIToken, APITokenRecord, BusinessTag, CreatedAPIToken } from '../api/types'
import AsyncState from '../components/AsyncState'
import ConfirmDialog from '../components/ConfirmDialog'
import Dialog from '../components/Dialog'
import { useToast } from '../components/ToastProvider'
import {
  IconCheck,
  IconChevronDown,
  IconClock,
  IconCopy,
  IconEdit,
  IconExternalLink,
  IconFileText,
  IconKey,
  IconPlus,
  IconRefresh,
  IconTag,
  IconTrash,
} from '../components/icons'
import { copyText } from '../utils/clipboard'
import { dateTimestamp, formatDate, formatFullDate, formatRelativeTime, isWithinWindow } from '../utils/date'
import { buildCurlSnippet, buildLeaseCommand, buildPythonSnippet } from '../utils/snippets'

/** 活跃判定窗口：24 小时内有调用/领用活动视为活跃 */
const ACTIVE_WINDOW_MS = 24 * 60 * 60 * 1000

function tagLabel(t: BusinessTag): string {
  return t.tag || t.name
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

export default function BusinessTagsPage() {
  const [tags, setTags] = useState<BusinessTag[]>([])
  const [tokens, setTokens] = useState<APIToken[]>([])
  const [tagsLoading, setTagsLoading] = useState(true)
  const [tokensLoading, setTokensLoading] = useState(true)
  const [tagsError, setTagsError] = useState('')
  const [tokensError, setTokensError] = useState('')

  const [newTagName, setNewTagName] = useState('')
  const [newTagDesc, setNewTagDesc] = useState('')
  const [newTokenName, setNewTokenName] = useState('')
  const [savingTag, setSavingTag] = useState(false)
  const [savingToken, setSavingToken] = useState(false)

  const [editingTag, setEditingTag] = useState<BusinessTag | null>(null)
  const [editTagValue, setEditTagValue] = useState('')
  const [editDescValue, setEditDescValue] = useState('')
  const [savingEdit, setSavingEdit] = useState(false)

  const [deleteTagTarget, setDeleteTagTarget] = useState<BusinessTag | null>(null)
  const [deleteTokenTarget, setDeleteTokenTarget] = useState<APIToken | null>(null)
  const [deletingTag, setDeletingTag] = useState(false)
  const [deletingToken, setDeletingToken] = useState(false)

  const [createdTokenVal, setCreatedTokenVal] = useState('')
  const [docTag, setDocTag] = useState('')
  const [activeLang, setActiveLang] = useState<'curl' | 'python'>('curl')
  const [tagDropdownOpen, setTagDropdownOpen] = useState(false)
  const tokenBannerRef = useRef<HTMLDivElement>(null)
  const tagDropdownRef = useRef<HTMLDivElement>(null)
  const { show } = useToast()

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

  // 业务标识下拉框点击外部与按 Esc 自动收起
  useEffect(() => {
    if (!tagDropdownOpen) return
    const handleClickOutside = (e: MouseEvent) => {
      if (tagDropdownRef.current && !tagDropdownRef.current.contains(e.target as Node)) {
        setTagDropdownOpen(false)
      }
    }
    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setTagDropdownOpen(false)
    }
    document.addEventListener('mousedown', handleClickOutside)
    document.addEventListener('keydown', handleKeyDown)
    return () => {
      document.removeEventListener('mousedown', handleClickOutside)
      document.removeEventListener('keydown', handleKeyDown)
    }
  }, [tagDropdownOpen])

  // 文档卡业务标识：所选 tag 被删除后自动回退到第一条（派生值，不用 effect 同步）
  const docTagValid = tags.some((t) => tagLabel(t) === docTag)
  const activeDocTag = docTagValid
    ? docTag
    : tags.length > 0
      ? tagLabel(tags[0])
      : 'tiktok'

  const tokenPlaceholder = createdTokenVal || '<YOUR_API_TOKEN>'

  const curlSnippet = buildCurlSnippet(window.location.origin, activeDocTag, tokenPlaceholder)
  const pythonSnippet = buildPythonSnippet(window.location.origin, activeDocTag, tokenPlaceholder)
  const lastActivity = [
    ...tags.map((t) => t.last_assigned_at || ''),
    ...tokens.map((t) => t.last_used_at || ''),
  ]
    .filter(Boolean)
    .sort((a, b) => (dateTimestamp(a) ?? 0) - (dateTimestamp(b) ?? 0))
    .pop()
  // 与出号记录页同一口径：24 小时内有调用视为活跃
  const recentActive = isWithinWindow(lastActivity, ACTIVE_WINDOW_MS)

  async function handleCreateTag(e: React.FormEvent) {
    e.preventDefault()
    if (!newTagName.trim()) return
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
      setSavingTag(false)
    }
  }

  function openEditDialog(t: BusinessTag) {
    setEditingTag(t)
    setEditTagValue(tagLabel(t))
    setEditDescValue(t.description || '')
  }

  async function handleSaveEdit(e: React.FormEvent) {
    e.preventDefault()
    if (!editingTag || !editTagValue.trim()) return
    setSavingEdit(true)
    try {
      // 后端 PATCH 是整对象 upsert，必须全量回传以免未提交字段被清空
      await request(`/api/tags/${encodeURIComponent(editingTag.id)}`, {
        method: 'PATCH',
        body: {
          ...editingTag,
          tag: editTagValue.trim(),
          name: editTagValue.trim(),
          description: editDescValue.trim(),
        },
      })
      show(`业务标识 [${editTagValue.trim()}] 已更新`)
      setEditingTag(null)
      await loadTags()
    } catch (err) {
      show(err instanceof ApiError ? err.message : '更新业务标识失败')
    } finally {
      setSavingEdit(false)
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

  const [rotatingTokenId, setRotatingTokenId] = useState<string | null>(null)

  async function handleCreateToken(e: React.FormEvent) {
    e.preventDefault()
    if (!newTokenName.trim()) return
    setSavingToken(true)
    try {
      const created = await request<CreatedAPIToken>('/api/tokens', {
        method: 'POST',
        body: { name: newTokenName.trim() },
      })
      if (created.token) {
        setCreatedTokenVal(created.token)
      }
      show(`令牌 [${newTokenName}] 已生成`)
      setNewTokenName('')
      await loadTokens()
    } catch (err) {
      show(err instanceof ApiError ? err.message : '生成令牌失败')
    } finally {
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

      {/* 顶部指标统计 (Bento 结构，与账号工作台一致) */}
      <div className="stat-grid">
        <div className="stat-card stat-card-blue">
          <div className="stat-card-header">
            <div className="stat-card-title-group">
              <div className="stat-icon stat-icon-blue">
                <IconTag size={16} />
              </div>
              <span className="stat-label">业务标识</span>
            </div>
          </div>
          <div className="stat-value">{tags.length}</div>
          <div className="stat-subtext">划分项目与渠道流水</div>
        </div>

        <div className="stat-card stat-card-purple">
          <div className="stat-card-header">
            <div className="stat-card-title-group">
              <div className="stat-icon stat-icon-purple">
                <IconKey size={16} />
              </div>
              <span className="stat-label">API 访问令牌</span>
            </div>
          </div>
          <div className="stat-value">{tokens.length}</div>
          <div className="stat-subtext">外部注册机鉴权凭据</div>
        </div>

        <div className="stat-card stat-card-green">
          <div className="stat-card-header">
            <div className="stat-card-title-group">
              <div className="stat-icon stat-icon-green">
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
          style={{ cursor: 'pointer' }}
          onClick={() => {
            document.querySelector('.api-docs-card')?.scrollIntoView({ behavior: 'smooth' })
          }}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault()
              document.querySelector('.api-docs-card')?.scrollIntoView({ behavior: 'smooth' })
            }
          }}
          title="点击查看自动化接入示例"
        >
          <div className="stat-card-header">
            <div className="stat-card-title-group">
              <div className="stat-icon stat-icon-amber">
                <IconExternalLink size={16} />
              </div>
              <span className="stat-label">快速领号接口</span>
            </div>
            <span className="stat-badge stat-badge-green">● 就绪</span>
          </div>
          <div className="stat-value">已就绪</div>
          <div className="stat-subtext">POST /api/quick-create · 点击直达接入示例 ↓</div>
        </div>
      </div>

      {createdTokenVal && (
        <div className="alert-banner alert-success" ref={tokenBannerRef}>
          <div className="alert-title">新生成的外部 API 访问令牌（仅显示一次，请妥善保存）：</div>
          <div className="token-reveal-row">
            <code>{createdTokenVal}</code>
            <button
              type="button"
              className="btn btn-sm btn-primary"
              onClick={() => {
                copyText(createdTokenVal)
                show('令牌已复制到剪贴板')
              }}
            >
              <IconCopy size={13} /> 复制令牌
            </button>
            <button
              type="button"
              className="btn btn-sm btn-primary"
              onClick={() => {
                copyText(buildLeaseCommand(window.location.origin, activeDocTag, createdTokenVal))
                show('完整接入命令已复制')
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
        </div>
      )}

      <div className="grid-2-col">
        {/* 左侧：业务标识列表与新建 */}
        <div className="card">
          <div className="card-header">
            <h2 className="card-title">
              <IconTag size={16} /> 业务标识 ({tags.length})
            </h2>
          </div>
          <form onSubmit={(e) => void handleCreateTag(e)} className="form-inline-compact">
            <input
              type="text"
              className="input"
              placeholder="标识名称 (例: tiktok)"
              value={newTagName}
              onChange={(e) => setNewTagName(e.target.value)}
              required
            />
            <input
              type="text"
              className="input"
              placeholder="描述备注"
              value={newTagDesc}
              onChange={(e) => setNewTagDesc(e.target.value)}
            />
            <button type="submit" className="btn btn-primary" disabled={savingTag}>
              <IconPlus size={14} /> 添加标识
            </button>
          </form>

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
                  {tags.map((t) => (
                    <tr key={t.id || tagLabel(t)}>
                      <td className="tag-name-cell">
                        <span className="badge badge-active" title={tagLabel(t)}>
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
                  ))}
                </tbody>
              </table>
            </div>
          </AsyncState>
        </div>

        {/* 右侧：外部令牌管理 */}
        <div className="card">
          <div className="card-header">
            <h2 className="card-title">
              <IconKey size={16} /> API 访问令牌 ({tokens.length})
            </h2>
          </div>
          <form onSubmit={(e) => void handleCreateToken(e)} className="form-inline-compact">
            <input
              type="text"
              className="input"
              placeholder="令牌备注 (例: 注册机-01)"
              value={newTokenName}
              onChange={(e) => setNewTokenName(e.target.value)}
              required
            />
            <button type="submit" className="btn btn-primary" disabled={savingToken}>
              <IconPlus size={14} /> 生成令牌
            </button>
          </form>

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
                  {tokens.map((tok) => {
                    const status = getTokenStatus(tok)
                    return (
                      <tr key={tok.id}>
                        <td>
                          <div style={{ fontWeight: 600, color: 'var(--color-text)' }}>{tok.name}</div>
                          <div style={{ fontSize: '11px', color: 'var(--color-text-secondary)', fontFamily: 'var(--font-mono)' }}>
                            {tok.token_prefix ? `${tok.token_prefix}****` : tok.id}
                          </div>
                        </td>
                        <td>
                          <span style={{ fontSize: '12px', color: 'var(--color-text-secondary)' }}>
                            {tok.scopes || 'allocate,verify'}
                          </span>
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
                        <td style={{ fontSize: '12px', color: 'var(--color-text-secondary)' }} title={formatFullDate(tok.last_used_at, '从未调用')}>
                          {formatDate(tok.last_used_at, '从未调用')}
                        </td>
                        <td>
                          <div className="row-actions">
                            {status !== 'revoked' && status !== 'expired' && (
                              <>
                                <button
                                  type="button"
                                  className="btn btn-xs btn-secondary"
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
                            {status === 'revoked' && (
                              <span style={{ fontSize: '12px', color: 'var(--color-text-tertiary)' }}>已作废</span>
                            )}
                            {status === 'expired' && (
                              <span style={{ fontSize: '12px', color: 'var(--color-text-tertiary)' }}>已过期</span>
                            )}
                          </div>
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          </AsyncState>
        </div>
      </div>

      {/* 底部：外部调用文档（随所选业务标识动态生成） */}
      <div className="card api-docs-card">
        <div className="card-header" style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: '12px' }}>
          <h2 className="card-title">
            <IconExternalLink size={16} /> 自动化注册机接入指南
          </h2>
          <div style={{ display: 'flex', gap: '6px' }}>
            <button
              type="button"
              className={`btn btn-sm ${activeLang === 'curl' ? 'btn-primary' : 'btn-secondary'}`}
              onClick={() => setActiveLang('curl')}
            >
              cURL (标准协议)
            </button>
            <button
              type="button"
              className={`btn btn-sm ${activeLang === 'python' ? 'btn-primary' : 'btn-secondary'}`}
              onClick={() => setActiveLang('python')}
            >
              Python (完整流水线)
            </button>
          </div>
        </div>
        <div style={{ padding: '20px' }}>
          <div className="api-docs-toolbar">
            <div className="api-snippet-desc">
              外部自动化脚本通过 HTTP 携带 API 令牌调用以下接口，即可从预热号池分配别名并提取验证码。支持动态传入任意 <code>tag</code> 参数；在上方预先登记可统一管理业务线说明：
            </div>
            <div className="custom-select-container" ref={tagDropdownRef}>
              <span className="custom-select-label">业务标识</span>
              <button
                type="button"
                className={`custom-select-trigger ${tagDropdownOpen ? 'open' : ''}`}
                onClick={() => setTagDropdownOpen((prev) => !prev)}
                aria-haspopup="listbox"
                aria-expanded={tagDropdownOpen}
              >
                <div style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
                  <IconTag size={13} style={{ color: 'var(--color-primary)' }} />
                  <span>
                    {tags.length === 0 && activeDocTag === 'tiktok'
                      ? '默认示例 (tiktok)'
                      : activeDocTag}
                  </span>
                </div>
                <IconChevronDown
                  size={14}
                  style={{
                    transition: 'transform 0.2s',
                    transform: tagDropdownOpen ? 'rotate(180deg)' : 'none',
                    color: 'var(--color-text-tertiary)',
                  }}
                />
              </button>

              {tagDropdownOpen && (
                <div className="custom-select-dropdown" role="listbox">
                  {tags.length === 0 ? (
                    <>
                      <div
                        className="custom-select-option selected"
                        role="option"
                        tabIndex={0}
                        aria-selected="true"
                        onClick={() => {
                          setDocTag('tiktok')
                          setTagDropdownOpen(false)
                        }}
                        onKeyDown={(e) => {
                          if (e.key === 'Enter' || e.key === ' ') {
                            e.preventDefault()
                            setDocTag('tiktok')
                            setTagDropdownOpen(false)
                          }
                        }}
                      >
                        <div style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
                          <IconTag size={13} />
                          <span>默认示例 (tiktok)</span>
                        </div>
                        <IconCheck size={13} />
                      </div>
                      <div className="custom-select-footer">
                        在上方添加业务标识后可在此切换
                      </div>
                    </>
                  ) : (
                    tags.map((t) => {
                      const label = tagLabel(t)
                      const isSelected = label === activeDocTag
                      return (
                        <div
                          key={t.id || label}
                          className={`custom-select-option ${isSelected ? 'selected' : ''}`}
                          role="option"
                          tabIndex={0}
                          aria-selected={isSelected}
                          onClick={() => {
                            setDocTag(label)
                            setTagDropdownOpen(false)
                          }}
                          onKeyDown={(e) => {
                            if (e.key === 'Enter' || e.key === ' ') {
                              e.preventDefault()
                              setDocTag(label)
                              setTagDropdownOpen(false)
                            }
                          }}
                        >
                          <div style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
                            <IconTag size={13} />
                            <span>{label}</span>
                          </div>
                          {isSelected && <IconCheck size={13} />}
                        </div>
                      )
                    })
                  )}
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
              onClick={() => {
                copyText(activeLang === 'curl' ? curlSnippet : pythonSnippet)
                show(activeLang === 'curl' ? 'cURL 示例命令已复制' : 'Python 自动化脚本已复制')
              }}
            >
              <IconCopy size={13} /> {activeLang === 'curl' ? '复制 cURL 命令' : '复制 Python 脚本'}
            </button>
          </div>
        </div>
      </div>

      {editingTag && (
        <Dialog
          title="编辑业务标识"
          open
          onClose={() => setEditingTag(null)}
        >
          <form onSubmit={(e) => void handleSaveEdit(e)}>
            <div className="form-field">
              <label htmlFor="edit-tag-name">标识名称</label>
              <input
                id="edit-tag-name"
                type="text"
                className="input"
                value={editTagValue}
                onChange={(e) => setEditTagValue(e.target.value)}
                required
                autoComplete="off"
              />
              <p className="hint">标识名称是外部脚本的调用参数（?tag=…），修改后需同步更新脚本配置</p>
            </div>
            <div className="form-field">
              <label htmlFor="edit-tag-desc">描述备注</label>
              <input
                id="edit-tag-desc"
                type="text"
                className="input"
                value={editDescValue}
                onChange={(e) => setEditDescValue(e.target.value)}
                autoComplete="off"
              />
            </div>
            <div className="form-actions">
              <button type="button" onClick={() => setEditingTag(null)}>
                取消
              </button>
              <button type="submit" className="danger" disabled={savingEdit || !editTagValue.trim()}>
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
    </div>
  )
}
