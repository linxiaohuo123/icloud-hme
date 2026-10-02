/**
 * [INPUT]: 依赖 api/client 的 request/ApiError, api/types 的各类实体, components 各通用 Dialog 与 ToastProvider, workspace 子组件组
 * [OUTPUT]: 对外提供 AccountWorkspace 单账号专属工作台总装容器，启停跨标签保持处理中状态并等待列表同步完成，删除同步在途锁与确认弹窗 busy，复制反馈遵循实际结果
 * [POS]: web/src/pages 的核心页面总装器，按账号隔离异步请求与局部状态，URL 作为收件箱筛选真相源
 * 单条及批量元数据保存同步本地 label/note；创建弹窗自行使账号缓存失效。
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useParams, useSearchParams } from 'react-router-dom'
import { ApiError, request } from '../api/client'
import type { AccountSummary, Alias } from '../api/types'
import { invalidateAccounts } from '../hooks/useAccounts'
import BatchEditAliasDialog from '../components/BatchEditAliasDialog'
import ConfirmDialog from '../components/ConfirmDialog'
import CookieDialog from '../components/CookieDialog'
import CreateAliasDialog from '../components/CreateAliasDialog'
import EditAliasDialog from '../components/EditAliasDialog'
import ICloudLoginDialog from '../components/ICloudLoginDialog'
import MailboxDialog from '../components/MailboxDialog'
import { useToast } from '../components/ToastProvider'
import { IconAliases, IconInbox } from '../components/icons'
import { copyText } from '../utils/clipboard'
import WorkspaceAliasesTab from './workspace/WorkspaceAliasesTab'
import WorkspaceHeader from './workspace/WorkspaceHeader'
import WorkspaceInboxTab from './workspace/WorkspaceInboxTab'

export default function AccountWorkspace() {
  const { accountId } = useParams<{ accountId: string }>()
  return <AccountWorkspaceContent key={accountId} accountId={accountId} />
}

function AccountWorkspaceContent({ accountId }: { accountId: string | undefined }) {
  const [searchParams, setSearchParams] = useSearchParams()
  const activeTab = searchParams.get('tab') === 'inbox' ? 'inbox' : 'aliases'

  const [account, setAccount] = useState<AccountSummary | null>(null)
  const [aliases, setAliases] = useState<Alias[]>([])
  const [inboxCount, setInboxCount] = useState<number | null>(null)
  const [accountLoading, setAccountLoading] = useState(true)
  const [aliasLoading, setAliasLoading] = useState(true)
  const [quickCreating, setQuickCreating] = useState(false)
  const [togglingAliasId, setTogglingAliasId] = useState<string | null>(null)

  // 跨账号竞态保护与请求取消
  const workspaceGenRef = useRef(0)
  const accountAbortRef = useRef<AbortController | null>(null)
  const aliasAbortRef = useRef<AbortController | null>(null)
  const currentAccountIdRef = useRef<string | undefined>(accountId)

  // 别名筛选状态
  const selectedAliasForInbox = searchParams.get('alias') || ''

  // 弹窗状态
  const [loginOpen, setLoginOpen] = useState(false)
  const [cookieOpen, setCookieOpen] = useState(false)
  const [mailboxOpen, setMailboxOpen] = useState(false)
  const [createAliasOpen, setCreateAliasOpen] = useState(false)
  const [confirmDeleteAlias, setConfirmDeleteAlias] = useState<Alias | null>(null)
  const deleteInFlightRef = useRef(false)
  const [deletingAlias, setDeletingAlias] = useState(false)
  const [editingAlias, setEditingAlias] = useState<Alias | null>(null)
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set())
  const [batchEditOpen, setBatchEditOpen] = useState(false)

  const { show, showCopyable } = useToast()

  // 保持 currentAccountIdRef 实时同步
  useEffect(() => {
    currentAccountIdRef.current = accountId
  }, [accountId])

  // 卸载时取消所有在途请求
  useEffect(() => {
    return () => {
      currentAccountIdRef.current = undefined
      accountAbortRef.current?.abort()
      aliasAbortRef.current?.abort()
    }
  }, [])

  const loadAccountData = useCallback(
    async (forceRefresh = false) => {
      if (!accountId || currentAccountIdRef.current !== accountId) return
      const requestedId = accountId

      // 递增全局代数，作废此前所有未决请求
      workspaceGenRef.current++
      const gen = workspaceGenRef.current

      accountAbortRef.current?.abort()
      aliasAbortRef.current?.abort()

      const accountCtrl = new AbortController()
      const aliasCtrl = new AbortController()
      accountAbortRef.current = accountCtrl
      aliasAbortRef.current = aliasCtrl

      setAccountLoading(true)
      setAliasLoading(true)

      try {
        // ── 第一阶段: 单账号精准载入 (按需获取) ──
        const found = await request<AccountSummary>(
          `/api/accounts/${encodeURIComponent(requestedId)}`,
          { signal: accountCtrl.signal },
        )
        if (
          gen === workspaceGenRef.current &&
          currentAccountIdRef.current === requestedId &&
          !accountCtrl.signal.aborted
        ) {
          setAccount(found ?? null)
        }
      } catch (err) {
        if (
          gen === workspaceGenRef.current &&
          currentAccountIdRef.current === requestedId &&
          !accountCtrl.signal.aborted
        ) {
          show(err instanceof ApiError ? err.message : '获取账号信息失败')
        }
      } finally {
        if (
          gen === workspaceGenRef.current &&
          currentAccountIdRef.current === requestedId
        ) {
          setAccountLoading(false)
        }
      }

      // 如果在此期间路由已切走或 generation 已被 supersede，中断第二阶段
      if (
        gen !== workspaceGenRef.current ||
        currentAccountIdRef.current !== requestedId ||
        accountCtrl.signal.aborted
      ) {
        return
      }

      // ── 第二阶段: 别名列表 (可能跨洋请求 Apple, 秒级) ──
      const aliasUrl = forceRefresh
        ? `/api/aliases?account_id=${encodeURIComponent(requestedId)}&refresh=true`
        : `/api/aliases?account_id=${encodeURIComponent(requestedId)}`
      try {
        const aliasData = await request<{ aliases?: Alias[] }>(aliasUrl, {
          signal: aliasCtrl.signal,
        })
        if (
          gen === workspaceGenRef.current &&
          currentAccountIdRef.current === requestedId &&
          !aliasCtrl.signal.aborted
        ) {
          const aliasList = Array.isArray(aliasData?.aliases) ? aliasData.aliases : []
          const activeCount = aliasList.filter((a) => a.active).length
          setAliases(aliasList)
          setAccount((prev) =>
            prev && prev.id === requestedId
              ? {
                  ...prev,
                  alias_total: aliasList.length,
                  alias_active: activeCount,
                }
              : prev,
          )
          // 普通 GET 绝不广播 account-updated
          if (forceRefresh) {
            show('已与 Apple 服务器完成数据对账')
          }
        }
      } catch (err) {
        if (
          gen === workspaceGenRef.current &&
          currentAccountIdRef.current === requestedId &&
          !aliasCtrl.signal.aborted
        ) {
          show(err instanceof ApiError ? err.message : '获取别名列表失败')
        }
      } finally {
        if (
          gen === workspaceGenRef.current &&
          currentAccountIdRef.current === requestedId
        ) {
          setAliasLoading(false)
        }
      }
    },
    [accountId, show],
  )

  useEffect(() => {
    // 切换账号时重置局部数据并拉取
    setAccount(null)
    setAliases([])
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
      showCopyable(created.email, '别名已生成')
      invalidateAccounts(accountId)
      void loadAccountData()
    } catch (err) {
      show(err instanceof ApiError ? err.message : '出号失败')
    } finally {
      setQuickCreating(false)
    }
  }

  async function handleToggleAlias(item: Alias) {
    if (!accountId || togglingAliasId) return
    setTogglingAliasId(item.anonymousId)
    try {
      const path = item.active
        ? `/api/aliases/${encodeURIComponent(item.anonymousId)}/deactivate`
        : `/api/aliases/${encodeURIComponent(item.anonymousId)}/reactivate`
      await request(path, {
        method: 'POST',
        body: { account_id: accountId },
      })
      show(item.active ? '别名已停用' : '别名已重新启用')
      invalidateAccounts(accountId)
      await loadAccountData()
    } catch (err) {
      show(err instanceof ApiError ? err.message : '操作失败')
    } finally {
      setTogglingAliasId(null)
    }
  }

  async function handleDeleteAlias() {
    if (!accountId || !confirmDeleteAlias || deleteInFlightRef.current) return
    deleteInFlightRef.current = true
    setDeletingAlias(true)
    try {
      await request(
        `/api/aliases/${encodeURIComponent(confirmDeleteAlias.anonymousId)}`,
        {
          method: 'DELETE',
          body: { account_id: accountId },
        },
      )
      show('别名已彻底删除')
      invalidateAccounts(accountId)
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
    } finally {
      deleteInFlightRef.current = false
      setDeletingAlias(false)
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
      {/* 顶部一体化旗舰工作台卡片 */}
      <WorkspaceHeader
        account={account}
        aliases={aliases}
        activeAliasCount={activeAliasCount}
        quickCreating={quickCreating}
        aliasLoading={aliasLoading}
        onQuickCreate={() => void handleQuickCreate()}
        onCreateCustom={() => setCreateAliasOpen(true)}
        onSync={() => void loadAccountData(true)}
        onOpenLogin={() => setLoginOpen(true)}
        onOpenCookie={() => setCookieOpen(true)}
        onOpenMailbox={() => setMailboxOpen(true)}
        onCopyAccount={async (mail) => {
          const ok = await copyText(mail)
          show(ok ? `已复制账号邮箱: ${mail}` : '复制失败，请手动复制账号邮箱')
          return ok
        }}
      />

      {/* 现代悬浮胶囊 Tab 导航 */}
      <div className="workspace-nav-bar">
        <div className="segmented-control">
          <button
            type="button"
            className={`segmented-tab ${activeTab === 'aliases' ? 'active' : ''}`}
            aria-pressed={activeTab === 'aliases'}
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
            aria-pressed={activeTab === 'inbox'}
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
      </div>

      {/* 别名管理 Tab 视图 */}
      {activeTab === 'aliases' && (
        <WorkspaceAliasesTab
          aliases={aliases}
          aliasLoading={aliasLoading}
          totalAliasCount={aliases.length || account?.alias_total || 0}
          activeAliasCount={activeAliasCount}
          togglingAliasId={togglingAliasId}
          deletingAlias={deletingAlias}
          selectedIds={selectedIds}
          setSelectedIds={setSelectedIds}
          onEditAlias={(item) => setEditingAlias(item)}
          onToggleAlias={handleToggleAlias}
          onDeleteAlias={(item) => { if (!deleteInFlightRef.current) setConfirmDeleteAlias(item) }}
          onReadMail={(email) => {
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
            onChanged={() => {
              invalidateAccounts(account.id)
              setAliases([])
              void loadAccountData()
            }}
            onSaved={() => {
              setCookieOpen(false)
              invalidateAccounts(account.id)
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
              invalidateAccounts(account.id)
              void loadAccountData()
            }}
          />
          <CreateAliasDialog
            open={createAliasOpen}
            accountId={account.id}
            onClose={() => setCreateAliasOpen(false)}
            onCreated={(email, auditRecorded) => {
              setCreateAliasOpen(false)
              void loadAccountData()
              if (!auditRecorded) show(`别名 ${email} 已创建，但出号流水写入失败，请勿重复创建`)
            }}
          />
          {confirmDeleteAlias && (
            <ConfirmDialog
              open={true}
              title="彻底删除别名"
              busy={deletingAlias}
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
              onSaved={(newLabel, newNote) => {
                setAliases((prev) =>
                  prev.map((a) =>
                    a.anonymousId === editingAlias.anonymousId ? { ...a, label: newLabel, note: newNote } : a,
                  ),
                )
                setEditingAlias(null)
                show('别名备注已更新')
              }}
            />
          )}
          {loginOpen && (
            <ICloudLoginDialog
              open={true}
              accountId={account.id}
              accountEmail={account.icloud_email || account.real_email}
              accountName={account.name}
              onClose={() => setLoginOpen(false)}
              onSaved={() => {
                setLoginOpen(false)
                show('iCloud 登录成功，会话凭据已更新')
                invalidateAccounts(account.id)
                void loadAccountData(true)
              }}
            />
          )}
          {batchEditOpen && (
            <BatchEditAliasDialog
              open={true}
              accountId={account.id}
              selectedIds={Array.from(selectedIds)}
              onClose={() => setBatchEditOpen(false)}
              onSaved={(succeededIds, newLabel, warning, newNote) => {
                const idSet = new Set(succeededIds)
                setAliases((prev) =>
                  prev.map((a) => (idSet.has(a.anonymousId) ? { ...a, label: newLabel, note: newNote } : a)),
                )
                setSelectedIds((prev) => new Set([...prev].filter((id) => !idSet.has(id))))
                setBatchEditOpen(false)
                show(`已成功批量修改 ${succeededIds.length} 个别名的备注${warning ? `；${warning}` : ''}`)
              }}
            />
          )}
        </>
      )}
    </div>
  )
}
