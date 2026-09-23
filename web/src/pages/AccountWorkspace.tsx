/**
 * [INPUT]: 依赖 api/client 的 request/ApiError, api/types 的各类实体, components 各通用 Dialog 与 ToastProvider, workspace 子组件组
 * [OUTPUT]: 对外提供 AccountWorkspace 单账号专属工作台总装容器
 * [POS]: web/src/pages 的核心页面总装器，将指标、头部、别名流、收件箱与弹窗组合为统一控制台
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useCallback, useEffect, useMemo, useState } from 'react'
import { useParams, useSearchParams } from 'react-router-dom'
import { ApiError, request } from '../api/client'
import type { AccountSummary, Alias } from '../api/types'
import BatchEditAliasDialog from '../components/BatchEditAliasDialog'
import ConfirmDialog from '../components/ConfirmDialog'
import CookieDialog from '../components/CookieDialog'
import CreateAliasDialog from '../components/CreateAliasDialog'
import EditAliasDialog from '../components/EditAliasDialog'
import MailboxDialog from '../components/MailboxDialog'
import { useToast } from '../components/ToastProvider'
import { IconAliases, IconInbox } from '../components/icons'
import { copyText } from '../utils/clipboard'
import WorkspaceAliasesTab from './workspace/WorkspaceAliasesTab'
import WorkspaceHeader from './workspace/WorkspaceHeader'
import WorkspaceInboxTab from './workspace/WorkspaceInboxTab'
import WorkspaceMetrics from './workspace/WorkspaceMetrics'

export default function AccountWorkspace() {
  const { accountId } = useParams<{ accountId: string }>()
  const [searchParams, setSearchParams] = useSearchParams()
  const activeTab = searchParams.get('tab') === 'inbox' ? 'inbox' : 'aliases'

  const [account, setAccount] = useState<AccountSummary | null>(null)
  const [aliases, setAliases] = useState<Alias[]>([])
  const [inboxCount, setInboxCount] = useState<number | null>(null)
  const [accountLoading, setAccountLoading] = useState(true)
  const [aliasLoading, setAliasLoading] = useState(true)
  const [quickCreating, setQuickCreating] = useState(false)

  // 别名筛选状态
  const [selectedAliasForInbox, setSelectedAliasForInbox] = useState(
    searchParams.get('alias') || '',
  )

  // 弹窗状态
  const [cookieOpen, setCookieOpen] = useState(false)
  const [mailboxOpen, setMailboxOpen] = useState(false)
  const [createAliasOpen, setCreateAliasOpen] = useState(false)
  const [confirmDeleteAlias, setConfirmDeleteAlias] = useState<Alias | null>(null)
  const [editingAlias, setEditingAlias] = useState<Alias | null>(null)
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set())
  const [batchEditOpen, setBatchEditOpen] = useState(false)

  const { show, showCopyable } = useToast()

  const loadAccountData = useCallback(
    async (forceRefresh = false) => {
      if (!accountId) return
      const requestedId = accountId
      setAccountLoading(true)
      setAliasLoading(true)
      try {
        // ── 第一阶段: 单账号精准载入 (毫秒级，按需获取) ──
        const found = await request<AccountSummary>(`/api/accounts/${encodeURIComponent(requestedId)}`)
        setAccount((prev) => (accountId !== requestedId ? prev : (found ?? null)))
      } catch (err) {
        if (accountId === requestedId) {
          show(err instanceof ApiError ? err.message : '获取账号信息失败')
        }
      } finally {
        if (accountId === requestedId) {
          setAccountLoading(false)
        }
      }

      if (accountId !== requestedId) return

      // ── 第二阶段: 别名列表 (可能跨洋请求 Apple, 秒级) ──
      const aliasUrl = forceRefresh
        ? `/api/aliases?account_id=${encodeURIComponent(requestedId)}&refresh=true`
        : `/api/aliases?account_id=${encodeURIComponent(requestedId)}`
      try {
        const aliasData = await request<{ aliases?: Alias[] }>(aliasUrl)
        const aliasList = Array.isArray(aliasData?.aliases) ? aliasData.aliases : []
        const activeCount = aliasList.filter((a) => a.active).length
        // 防竞态: 慢请求返回时若用户已切走，丢弃结果，杜绝跨账号数据覆盖
        setAliases((prev) => (accountId !== requestedId ? prev : aliasList))
        setAccount((prev) =>
          prev && prev.id === requestedId
            ? {
                ...prev,
                alias_total: aliasList.length,
                alias_active: activeCount,
              }
            : prev,
        )
        window.dispatchEvent(new CustomEvent('account-updated'))
        if (forceRefresh) {
          show('已与 Apple 服务器完成数据对账')
        }
      } catch (err) {
        show(err instanceof ApiError ? err.message : '获取别名列表失败')
      } finally {
        setAliasLoading(false)
      }
    },
    [accountId, show],
  )

  useEffect(() => {
    void loadAccountData()
  }, [loadAccountData])

  async function handleQuickCreate() {
    if (!accountId) return
    setQuickCreating(true)
    try {
      const created = await request<{ email: string }>(
        `/api/quick-create?account_id=${encodeURIComponent(accountId)}`,
        { method: 'POST' },
      )
      showCopyable('别名已生成', created.email)
      void loadAccountData()
    } catch (err) {
      show(err instanceof ApiError ? err.message : '出号失败')
    } finally {
      setQuickCreating(false)
    }
  }

  async function handleToggleAlias(item: Alias) {
    if (!accountId) return
    try {
      const path = item.active
        ? `/api/aliases/${encodeURIComponent(item.anonymousId)}/deactivate`
        : `/api/aliases/${encodeURIComponent(item.anonymousId)}/reactivate`
      await request(path, {
        method: 'POST',
        body: { account_id: accountId },
      })
      show(item.active ? '别名已停用' : '别名已重新启用')
      void loadAccountData()
    } catch (err) {
      show(err instanceof ApiError ? err.message : '操作失败')
    }
  }

  async function handleDeleteAlias() {
    if (!accountId || !confirmDeleteAlias) return
    try {
      await request(
        `/api/aliases/${encodeURIComponent(confirmDeleteAlias.anonymousId)}`,
        {
          method: 'DELETE',
          body: { account_id: accountId },
        },
      )
      show('别名已彻底删除')
      const deletedId = confirmDeleteAlias.anonymousId
      setConfirmDeleteAlias(null)
      setSelectedIds((prev) => {
        if (!prev.has(deletedId)) return prev
        const next = new Set(prev)
        next.delete(deletedId)
        return next
      })
      void loadAccountData()
    } catch (err) {
      show(err instanceof ApiError ? err.message : '删除别名失败')
    }
  }

  const activeAliasCount = useMemo(() => {
    if (Array.isArray(aliases) && aliases.length > 0) {
      return aliases.filter((a) => a.active).length
    }
    return account?.alias_active ?? 0
  }, [aliases, account?.alias_active])

  if (!account && !accountLoading) {
    return (
      <div className="page-container">
        <div className="card text-center p-8">
          <p className="text-secondary">未找到该账号，请在左侧选择已有账号或创建新账号</p>
        </div>
      </div>
    )
  }

  return (
    <div className="page-container">
      {/* 顶部指标矩阵 */}
      <WorkspaceMetrics
        account={account}
        aliases={aliases}
        activeAliasCount={activeAliasCount}
      />

      {/* 顶部账号卡片与快捷操作 */}
      <WorkspaceHeader
        account={account}
        quickCreating={quickCreating}
        aliasLoading={aliasLoading}
        onQuickCreate={() => void handleQuickCreate()}
        onCreateCustom={() => setCreateAliasOpen(true)}
        onSync={() => void loadAccountData(true)}
        onOpenCookie={() => setCookieOpen(true)}
        onOpenMailbox={() => setMailboxOpen(true)}
        onCopyAccount={(mail) => {
          if (mail) {
            copyText(mail)
            show(`已复制账号邮箱: ${mail}`)
          }
        }}
      />

      {/* 现代悬浮胶囊 Tab 导航 */}
      <div className="workspace-nav-bar">
        <div className="segmented-control">
          <button
            type="button"
            className={`segmented-tab ${activeTab === 'aliases' ? 'active' : ''}`}
            onClick={() => {
              searchParams.set('tab', 'aliases')
              setSearchParams(searchParams)
            }}
          >
            <IconAliases size={15} />
            <span>别名管理</span>
            <span className="tab-pill-count">{aliases.length || account?.alias_total || 0}</span>
          </button>
          <button
            type="button"
            className={`segmented-tab ${activeTab === 'inbox' ? 'active' : ''}`}
            onClick={() => {
              searchParams.set('tab', 'inbox')
              setSearchParams(searchParams)
            }}
          >
            <IconInbox size={15} />
            <span>收件箱与验证码</span>
            {inboxCount !== null && inboxCount > 0 && (
              <span className="tab-pill-count tab-pill-highlight">
                {inboxCount}
              </span>
            )}
          </button>
        </div>

        <div className="workspace-nav-status">
          <span>共管理 <b>{aliases.length || account?.alias_total || 0}</b> 个隐私别名</span>
        </div>
      </div>

      {/* 别名管理 Tab 视图 */}
      {activeTab === 'aliases' && (
        <WorkspaceAliasesTab
          aliases={aliases}
          aliasLoading={aliasLoading}
          totalAliasCount={aliases.length || account?.alias_total || 0}
          activeAliasCount={activeAliasCount}
          selectedIds={selectedIds}
          setSelectedIds={setSelectedIds}
          onEditAlias={(item) => setEditingAlias(item)}
          onToggleAlias={(item) => void handleToggleAlias(item)}
          onDeleteAlias={(item) => setConfirmDeleteAlias(item)}
          onReadMail={(email) => {
            setSelectedAliasForInbox(email)
            searchParams.set('tab', 'inbox')
            searchParams.set('alias', email)
            setSearchParams(searchParams)
          }}
          onOpenBatchEdit={() => setBatchEditOpen(true)}
          onCopySuccess={(msg) => show(msg)}
        />
      )}

      {/* 收件箱 Tab 视图 */}
      {activeTab === 'inbox' && (
        <WorkspaceInboxTab
          accountId={account?.id ?? accountId}
          accountSummary={account}
          aliases={aliases}
          selectedAlias={selectedAliasForInbox}
          onSelectAlias={(val) => {
            setSelectedAliasForInbox(val)
            searchParams.set('alias', val)
            setSearchParams(searchParams)
          }}
          onCountChange={(cnt) => setInboxCount(cnt)}
        />
      )}

      {/* 各管理 Dialog */}
      {account && (
        <>
          <CookieDialog
            open={cookieOpen}
            accountId={account.id}
            onClose={() => setCookieOpen(false)}
            onSaved={() => {
              setCookieOpen(false)
              void loadAccountData()
            }}
          />
          <MailboxDialog
            open={mailboxOpen}
            accountId={account.id}
            current={account.mailbox}
            onClose={() => setMailboxOpen(false)}
            onSaved={() => {
              setMailboxOpen(false)
              void loadAccountData()
            }}
          />
          <CreateAliasDialog
            open={createAliasOpen}
            accountId={account.id}
            onClose={() => setCreateAliasOpen(false)}
            onCreated={() => {
              setCreateAliasOpen(false)
              void loadAccountData()
            }}
          />
          {confirmDeleteAlias && (
            <ConfirmDialog
              open={true}
              title="彻底删除别名"
              message={`确定要删除别名 ${confirmDeleteAlias.email} 吗？此操作不可逆！`}
              onClose={() => setConfirmDeleteAlias(null)}
              onConfirm={() => void handleDeleteAlias()}
            />
          )}
          {editingAlias && (
            <EditAliasDialog
              open={true}
              accountId={account.id}
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
              accountId={account.id}
              selectedIds={Array.from(selectedIds)}
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
        </>
      )}
    </div>
  )
}
