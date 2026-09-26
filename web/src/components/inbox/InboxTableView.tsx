/**
 * [INPUT]: 依赖 api/client (request, ApiError, getMessageDetail), api/types, components (AsyncState, ConfirmDialog, ToastProvider), hooks/useAccounts (fetchAccountsDeduped), utils (clipboard, date, mail, sniffer: buildSniffContext, extractOTPMemoized, parseSenderInfo), ./InboxFilterBar, ./InboxTableRow, ./MailDetailDialog
 * [OUTPUT]: 对外提供 InboxTableView 收件箱表格与筛选核心组件；支持 externalAliases 直传消灭冗余 I/O、fetchAccountsDeduped 全局缓存共享、INBOX First 首屏优先加载、/api/mailboxes 交互式按需懒加载 (带 pending 队列与 IMAP 避让)、模块级缓存防 Tab 切换重载、Body-on-demand 按需加载单封正文 (零首屏批量正文 I/O)
 * [POS]: web/src/components/inbox 的核心视图容器，统一单账号工作台与全局收件箱大盘的数据流与交互
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { request, ApiError, getMessageDetail } from '../../api/client'
import { fetchAccountsDeduped } from '../../hooks/useAccounts'
import type { AccountSummary, Alias, FullMessage, InboxMessage, InboxResult, MailboxFolder } from '../../api/types'
import AsyncState from '../AsyncState'
import ConfirmDialog from '../ConfirmDialog'
import InboxFilterBar from './InboxFilterBar'
import { useToast } from '../ToastProvider'
import { copyText } from '../../utils/clipboard'
import { dateTimestamp } from '../../utils/date'
import { buildMailCacheKey } from '../../utils/mail'
import { buildSniffContext, extractOTPMemoized, parseSenderInfo } from '../../utils/sniffer'
import InboxTableRow from './InboxTableRow'
import MailDetailDialog from './MailDetailDialog'
import { IconAccounts, IconKey, IconMail, IconRefresh, IconSearch } from '../icons'

export interface InboxTableViewProps {
  accountId?: string
  accountSummary?: AccountSummary | null
  fixedAccount?: boolean
  initialAlias?: string
  externalAliases?: Alias[]
  onCopySuccess?: (msg: string) => void
  showPageHeader?: boolean
  onCountChange?: (count: number) => void
}

// 模块级单例缓存：生命周期超越组件挂载/卸载，保证工作台 Tab 切换 0ms 瞬间恢复
const moduleMessageCache = new Map<string, FullMessage>()
const MAX_MODULE_MSG_CACHE = 1000

function setModuleMessageCache(key: string, msg: FullMessage) {
  if (moduleMessageCache.size >= MAX_MODULE_MSG_CACHE) {
    const it = moduleMessageCache.keys()
    for (let i = 0; i < 200; i++) {
      const k = it.next().value
      if (k) moduleMessageCache.delete(k)
    }
  }
  moduleMessageCache.set(key, msg)
}

// 模块级单例列表快照缓存：支持工作台 Tab 切换、筛选恢复 0ms 瞬间呈现 (后台默默 revalidate)
interface InboxSnapshotEntry {
  result: InboxResult
  cachedAt: number
}
const moduleInboxSnapshotCache = new Map<string, InboxSnapshotEntry>()
const MAX_SNAPSHOT_CACHE = 50

function buildSnapshotKey(accId: string, al: string, fld: string, lmt: number, dys: number, isWebMail: boolean) {
  const trimmed = (fld || 'INBOX').trim()
  const normFolder = isWebMail
    ? 'INBOX'
    : (trimmed.toLowerCase() === 'all'
      ? 'all'
      : (trimmed.toLowerCase() === 'inbox' ? 'INBOX' : trimmed))
  const normDays = isWebMail ? 0 : dys
  return `${accId.trim()}::${al.trim()}::${normFolder}::${lmt}::${normDays}`
}

function getSnapshot(key: string): InboxResult | null {
  const entry = moduleInboxSnapshotCache.get(key)
  if (!entry) return null
  if (Date.now() - entry.cachedAt > 5 * 60 * 1000) {
    moduleInboxSnapshotCache.delete(key)
    return null
  }
  return entry.result
}

function setSnapshot(key: string, result: InboxResult) {
  if (moduleInboxSnapshotCache.size >= MAX_SNAPSHOT_CACHE) {
    const firstKey = moduleInboxSnapshotCache.keys().next().value
    if (firstKey) moduleInboxSnapshotCache.delete(firstKey)
  }
  moduleInboxSnapshotCache.set(key, { result, cachedAt: Date.now() })
}

// 模块级文件夹缓存：带 5 分钟 TTL，消灭重复 /api/mailboxes 网络请求与同账号 IMAP 锁竞争
interface FolderCacheEntry {
  folders: MailboxFolder[]
  cachedAt: number
}
const moduleFolderCache = new Map<string, FolderCacheEntry>()

let moduleSessionGen = 0

// eslint-disable-next-line react-refresh/only-export-components
export function getModuleSessionGen(): number {
  return moduleSessionGen
}

// eslint-disable-next-line react-refresh/only-export-components
export function getModuleMessageCache(key: string): FullMessage | undefined {
  return moduleMessageCache.get(key)
}

if (typeof window !== 'undefined') {
  window.addEventListener('auth-logout', () => {
    moduleSessionGen++
    moduleInboxSnapshotCache.clear()
    moduleFolderCache.clear()
    moduleMessageCache.clear()
  })
}

function getCachedFolders(accountId: string): MailboxFolder[] | null {
  const entry = moduleFolderCache.get(accountId)
  if (!entry) return null
  if (Date.now() - entry.cachedAt > 5 * 60 * 1000) {
    moduleFolderCache.delete(accountId)
    return null
  }
  return entry.folders
}

// eslint-disable-next-line react-refresh/only-export-components
export function clearInboxSnapshotCache(accountId?: string) {
  if (accountId) {
    const acc = accountId.trim()
    moduleFolderCache.delete(acc)
    for (const k of Array.from(moduleInboxSnapshotCache.keys())) {
      if (k.startsWith(acc + '::')) {
        moduleInboxSnapshotCache.delete(k)
      }
    }
    for (const k of Array.from(moduleMessageCache.keys())) {
      if (k.startsWith(acc + ':') || k.startsWith(acc + '::')) {
        moduleMessageCache.delete(k)
      }
    }
  } else {
    moduleSessionGen++
    moduleInboxSnapshotCache.clear()
    moduleFolderCache.clear()
    moduleMessageCache.clear()
  }
}

// eslint-disable-next-line react-refresh/only-export-components
export function dispatchAccountUpdated(accountId?: string) {
  if (typeof window !== 'undefined') {
    window.dispatchEvent(new CustomEvent('account-updated', { detail: { accountId } }))
  }
}

export default function InboxTableView({
  accountId: propAccountId,
  accountSummary,
  fixedAccount = false,
  initialAlias = '',
  externalAliases,
  onCopySuccess,
  showPageHeader = false,
  onCountChange,
}: InboxTableViewProps) {
  const [searchParams, setSearchParams] = useSearchParams()
  const { show } = useToast()

  const initialAccountId = propAccountId || accountSummary?.id || ''
  const initialIsWebMail = Boolean(accountSummary && !accountSummary.has_app_password && !accountSummary.mailbox?.email)
  const initialSnapshotKey = useMemo(() => {
    return initialAccountId ? buildSnapshotKey(initialAccountId, initialAlias, 'INBOX', 20, 7, initialIsWebMail) : ''
  }, [initialAccountId, initialAlias, initialIsWebMail])

  const initialSnapshot = useMemo(() => {
    return (fixedAccount && initialSnapshotKey) ? getSnapshot(initialSnapshotKey) : null
  }, [fixedAccount, initialSnapshotKey])

  const [accounts, setAccounts] = useState<AccountSummary[]>(accountSummary ? [accountSummary] : [])
  const [accountCapabilityReady, setAccountCapabilityReady] = useState(Boolean(accountSummary))
  const [accountId, setAccountId] = useState(initialAccountId)
  const [aliases, setAliases] = useState<Alias[]>(externalAliases ?? [])
  const [folders, setFolders] = useState<MailboxFolder[]>(() => {
    return initialAccountId ? getCachedFolders(initialAccountId) || [] : []
  })

  const [alias, setAlias] = useState(initialAlias)
  const [folder, setFolder] = useState('INBOX')
  const [limit, setLimit] = useState(20)
  const [days, setDays] = useState(7)
  const [autoRefreshInterval, setAutoRefreshInterval] = useState<number>(0)

  // 记录通过 CAPABILITY_UNSUPPORTED 降级或静态推断为仅 WebMail 的账号集合
  const [effectiveWebMailAccounts, setEffectiveWebMailAccounts] = useState<Record<string, boolean>>(() => {
    if (initialAccountId && initialIsWebMail) {
      return { [initialAccountId]: true }
    }
    return {}
  })
  const unsupportedRetryRef = useRef<Record<string, number>>({})

  const currentAccount = useMemo(() => accounts.find((a) => a.id === accountId) || (accountSummary?.id === accountId ? accountSummary : undefined), [accounts, accountId, accountSummary])
  const isWebMailOnly = useMemo(() => {
    if (effectiveWebMailAccounts[accountId]) return true
    if (!currentAccount) return false
    return !currentAccount.has_app_password && !currentAccount.mailbox?.email
  }, [currentAccount, effectiveWebMailAccounts, accountId])

  // 客户端分页
  const [page, setPage] = useState(1)
  const pageSize = 20

  const [result, setResult] = useState<InboxResult | null>(initialSnapshot)
  const [loading, setLoading] = useState(initialSnapshot ? false : true)
  const [isRevalidating, setIsRevalidating] = useState(Boolean(initialSnapshot))
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
  const detailAbortRef = useRef<AbortController | null>(null)
  const detailInFlightRef = useRef<string | null>(null)
  const folderAbortRef = useRef<AbortController | null>(null)
  const folderLoadingAccountRef = useRef<string | null>(null)
  const pendingFolderLoadRef = useRef(false)
  const accountGenRef = useRef(0)
  const queryGenRef = useRef(0)
  const isBusyRef = useRef(false)
  const messageCacheRef = useRef<Map<string, FullMessage>>(new Map())
  const hasAccountsLoadedRef = useRef(Boolean(accountSummary))

  useEffect(() => {
    return () => {
      detailAbortRef.current?.abort()
      detailInFlightRef.current = null
      abortRef.current?.abort()
      folderAbortRef.current?.abort()
      folderLoadingAccountRef.current = null
      pendingFolderLoadRef.current = false
      accountGenRef.current += 1
      isBusyRef.current = false
      if (copiedTimerRef.current) clearTimeout(copiedTimerRef.current)
      if (copiedAliasTimerRef.current) clearTimeout(copiedAliasTimerRef.current)
    }
  }, [])

  // 监听外部 propAccountId 变更（工作台模式），严格杜绝跨账号预取与详情污染
  useEffect(() => {
    if (propAccountId && propAccountId !== accountId) {
      accountGenRef.current += 1
      detailAbortRef.current?.abort()
      detailInFlightRef.current = null
      abortRef.current?.abort()
      folderAbortRef.current?.abort()
      folderLoadingAccountRef.current = null
      pendingFolderLoadRef.current = false
      messageCacheRef.current.clear()
      unsupportedRetryRef.current[propAccountId] = 0
      setAccountId(propAccountId)
      setFolder('INBOX')
      setFolders(getCachedFolders(propAccountId) || [])
    }
  }, [propAccountId, accountId])

  // 监听外部 initialAlias 变更（工作台别名联动）
  useEffect(() => {
    if (initialAlias !== undefined) {
      setAlias(initialAlias)
    }
  }, [initialAlias])

  const loadingRef = useRef(loading)
  useEffect(() => {
    loadingRef.current = loading
  }, [loading])

  // 监听全局登出与账号变更事件，立即使旧任务与在途响应失效，严禁回写陈旧缓存
  useEffect(() => {
    const handleLogout = () => {
      accountGenRef.current += 1
      detailAbortRef.current?.abort()
      detailInFlightRef.current = null
      abortRef.current?.abort()
      folderAbortRef.current?.abort()
      folderLoadingAccountRef.current = null
      pendingFolderLoadRef.current = false
      isBusyRef.current = false
      messageCacheRef.current.clear()
      clearInboxSnapshotCache()
      setLoading(false)
      setResult(null)
      setFolders([])
      setDetail(null)
    }

    const handleAccountUpdated = (e: Event) => {
      const customEvent = e as CustomEvent<{ accountId?: string }>
      const targetId = customEvent.detail?.accountId
      if (!targetId || targetId === accountId) {
        accountGenRef.current += 1
        detailAbortRef.current?.abort()
        detailInFlightRef.current = null
        abortRef.current?.abort()
        folderAbortRef.current?.abort()
        folderLoadingAccountRef.current = null
        pendingFolderLoadRef.current = false
        isBusyRef.current = false
        messageCacheRef.current.clear()
        if (accountId) {
          clearInboxSnapshotCache(accountId)
        }
        setRetryKey((k) => k + 1)
      }
    }

    window.addEventListener('auth-logout', handleLogout)
    window.addEventListener('account-updated', handleAccountUpdated)
    return () => {
      window.removeEventListener('auth-logout', handleLogout)
      window.removeEventListener('account-updated', handleAccountUpdated)
    }
  }, [accountId])

  const prevAccountConfigRef = useRef('')
  useEffect(() => {
    if (!accountSummary) return
    const configSig = `${accountSummary.id}:${accountSummary.has_app_password}:${accountSummary.mailbox?.email || ''}:${accountSummary.mailbox?.imap_host || ''}:${accountSummary.has_proxy}:${accountSummary.status}:${accountSummary.last_validated}`
    if (prevAccountConfigRef.current && prevAccountConfigRef.current !== configSig) {
      accountGenRef.current += 1
      abortRef.current?.abort()
      isBusyRef.current = false
      messageCacheRef.current.clear()
      clearInboxSnapshotCache(accountSummary.id)
      setRetryKey((k) => k + 1)
    }
    prevAccountConfigRef.current = configSig
  }, [accountSummary])

  // 自动刷新轮询定时器：页面在后台时暂停，在途请求未完成（包括后台 revalidate 与正文补全）时跳过打断
  useEffect(() => {
    if (autoRefreshInterval <= 0 || !accountId) return
    const timer = setInterval(() => {
      if (typeof document !== 'undefined' && (document.hidden || document.visibilityState !== 'visible')) return
      if (loadingRef.current || isBusyRef.current) return
      setRetryKey((k) => k + 1)
    }, autoRefreshInterval * 1000)
    return () => clearInterval(timer)
  }, [autoRefreshInterval, accountId])

  // 1. 初始化账号列表（若父级已直传 accountSummary 且 fixedAccount 则 0ms 消费，消灭冗余 I/O）
  useEffect(() => {
    if (fixedAccount && accountSummary) {
      setAccountCapabilityReady(true)
      return
    }
    if (hasAccountsLoadedRef.current) {
      setAccountCapabilityReady(true)
      return
    }
    let cancelled = false
    fetchAccountsDeduped(retryKey > 0)
      .then((data) => {
        if (cancelled) return
        hasAccountsLoadedRef.current = true
        setAccounts(data)
        setAccountCapabilityReady(true)
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
        hasAccountsLoadedRef.current = true
        setAccountCapabilityReady(true)
        setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
        setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [fixedAccount, accountSummary, retryKey, searchParams, setSearchParams])

  // 2. 账号变化时拉取别名列表 (若父级已直传 externalAliases 则 0ms 消费，消灭重复网络请求)
  useEffect(() => {
    if (externalAliases !== undefined) {
      setAliases(externalAliases)
      return
    }
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
  }, [accountId, externalAliases])

  // 3. 惰性拉取文件夹列表 (Mailboxes Lazy Load - PR-MAIL-03)：
  // 仅在用户实际交互文件夹下拉且首屏 Inbox metadata 完成后加载；
  // 带 5 分钟 TTL 模块级缓存、代际保护与 in-flight 并发防护，WebMail 模式严格不发
  const ensureFoldersLoaded = useCallback(() => {
    if (!accountId) return
    if (isWebMailOnly) return

    const cached = getCachedFolders(accountId)
    if (cached) {
      setFolders(cached)
      return
    }

    if (folderLoadingAccountRef.current === accountId) {
      return
    }

    if (loadingRef.current) {
      pendingFolderLoadRef.current = true
      return
    }

    const currentAccountGen = accountGenRef.current
    const requestAccountId = accountId

    folderAbortRef.current?.abort()
    const controller = new AbortController()
    folderAbortRef.current = controller
    folderLoadingAccountRef.current = requestAccountId

    request<{ account_id: string; folders: MailboxFolder[] }>(
      `/api/mailboxes?account_id=${encodeURIComponent(requestAccountId)}`,
      { signal: controller.signal },
    )
      .then((data) => {
        if (currentAccountGen !== accountGenRef.current) return
        const f = data.folders ?? []
        moduleFolderCache.set(requestAccountId, { folders: f, cachedAt: Date.now() })
        setFolders(f)
      })
      .catch((err) => {
        if (currentAccountGen !== accountGenRef.current) return
        if (err instanceof ApiError && err.code === 'ABORTED') return
        setFolders([])
      })
      .finally(() => {
        if (folderLoadingAccountRef.current === requestAccountId) {
          folderLoadingAccountRef.current = null
        }
      })
  }, [accountId, isWebMailOnly])

  // 当首屏 Inbox 渲染完成 (loading 从 true 变为 false) 时，如若用户在此期间已点击文件夹，按序补发 /api/mailboxes
  useEffect(() => {
    if (!loading && pendingFolderLoadRef.current) {
      pendingFolderLoadRef.current = false
      ensureFoldersLoaded()
    }
  }, [loading, ensureFoldersLoaded])

  // 4. 查询收件箱邮件 (带代际保护 accountGenRef、快照秒级恢复与分阶段小批正文补全)
  useEffect(() => {
    if (!accountId) return
    // 首屏 capability 栅栏保护：未完成 capability 解析前暂缓发起带参数查询，防止 WebMail 模式被误传 folder/days 参数 (WEBMAIL-01)
    if (!accountCapabilityReady) return

    isBusyRef.current = true
    const currentAccountGen = accountGenRef.current
    const currentQueryGen = ++queryGenRef.current
    const currentQueryKey = buildSnapshotKey(accountId, alias, folder, limit, days, isWebMailOnly)
    const cachedSnap = fixedAccount ? getSnapshot(currentQueryKey) : null

    if (cachedSnap) {
      setResult(cachedSnap)
      setLoading(false)
      setIsRevalidating(true)
    } else {
      setResult(null)
      setLoading(true)
      setIsRevalidating(false)
    }

    abortRef.current?.abort()
    const controller = new AbortController()
    abortRef.current = controller
    let cancelled = false

    const isStale = () => cancelled || currentAccountGen !== accountGenRef.current || currentQueryGen !== queryGenRef.current

    const params = new URLSearchParams({ account_id: accountId })
    if (alias) params.set('alias', alias)
    if (!isWebMailOnly) {
      params.set('folder', folder || 'INBOX')
      params.set('days', String(days))
    }
    params.set('limit', String(limit))
    if (retryKey > 0) {
      params.set('refresh', 'true')
    }

    request<InboxResult>(`/api/inbox?${params.toString()}`, {
      signal: controller.signal,
    })
      .then((data) => {
        if (isStale()) return
        setError('')
        unsupportedRetryRef.current[accountId] = 0

        // 1. 先用本地/模块缓存中已有的正文快速回填 (零网络成本复用已有缓存)
        let initialMessages = data?.messages || []
        if (data && Array.isArray(data.messages) && data.messages.length > 0) {
          initialMessages = data.messages.map((m) => {
            const key = buildMailCacheKey(accountId, m)
            const cached = moduleMessageCache.get(key) || messageCacheRef.current.get(key)
            if (cached && (cached.body || cached.preview)) {
              return {
                ...m,
                preview: cached.preview || cached.body || m.preview,
                body: cached.body || m.body,
              }
            }
            return m
          })
        }

        // PR-MAIL-04: Body-on-demand
        // /api/inbox 返回 metadata 后直接渲染，不再自动 POST /api/messages，完整正文按需单封读取
        const baseResult = data ? { ...data, messages: initialMessages } : null
        setResult(baseResult)
        if (baseResult) {
          setSnapshot(currentQueryKey, baseResult)
        }
        setLoading(false)
        setIsRevalidating(false)
        isBusyRef.current = false
      })
      .catch((err) => {
        const isAuthError = err instanceof ApiError && (err.status === 401 || err.code === 'AUTH_REQUIRED')
        if (!isAuthError && (isStale() || (err instanceof ApiError && err.code === 'ABORTED'))) return
        if (isAuthError) {
          clearInboxSnapshotCache()
        }
        isBusyRef.current = false
        setIsRevalidating(false)
        setLoading(false)
        // 捕获 CAPABILITY_UNSUPPORTED 错误后：最多允许 1 次退避重试 (剥离 folder/days 并记录 effective webmail capability)，严禁陷入无休止重试循环 (WEBMAIL-03, WEBMAIL-04)
        if (err instanceof ApiError && err.code === 'CAPABILITY_UNSUPPORTED') {
          const retried = unsupportedRetryRef.current[accountId] || 0
          if (retried < 1) {
            unsupportedRetryRef.current[accountId] = retried + 1
            setEffectiveWebMailAccounts((prev) => ({ ...prev, [accountId]: true }))
            return
          }
        }
        setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
        if (!cachedSnap) {
          setResult(null)
        }
      })
      .finally(() => {
        if (!isStale()) {
          setLoading(false)
        }
      })

    return () => {
      cancelled = true
      isBusyRef.current = false
      controller.abort()
    }
  }, [accountId, accountCapabilityReady, isWebMailOnly, alias, folder, limit, days, retryKey, fixedAccount])

  // 查询提交
  function handleSearch() {
    setPage(1)
    if (!fixedAccount) {
      const next: Record<string, string> = { account_id: accountId }
      if (alias) next.alias = alias
      if (!isWebMailOnly) {
        if (folder) next.folder = folder
        next.days = String(days)
      }
      next.limit = String(limit)
      setSearchParams(next, { replace: true })
    }
    setRetryKey((k) => k + 1)
  }

  function handleAccountChange(newAccountId: string) {
    accountGenRef.current += 1
    detailAbortRef.current?.abort()
    detailInFlightRef.current = null
    abortRef.current?.abort()
    folderAbortRef.current?.abort()
    folderLoadingAccountRef.current = null
    pendingFolderLoadRef.current = false
    setAccountId(newAccountId)
    setFolder('INBOX')
    setFolders(newAccountId ? getCachedFolders(newAccountId) || [] : [])
    setAlias('')
    setPage(1)
    setResult(null)
    messageCacheRef.current.clear()
    unsupportedRetryRef.current[newAccountId] = 0
    setSearchParams({ account_id: newAccountId }, { replace: true })
  }

  const openMessage = useCallback(async (message: InboxMessage) => {
    const sessionGen = moduleSessionGen
    const currentGen = accountGenRef.current
    const primaryKey = buildMailCacheKey(accountId, message)
    const cached = moduleMessageCache.get(primaryKey) || messageCacheRef.current.get(primaryKey)
    if (cached) {
      // 边界 1：切换到已缓存邮件时，立即使任何在途未完成详情请求失效并重置状态，防止旧请求晚到覆盖当前视图
      detailAbortRef.current?.abort()
      detailAbortRef.current = null
      detailInFlightRef.current = null
      setDetailLoading(false)
      setDetail(cached)
      return
    }

    const targetRefOrId = message.message_ref || message.id
    if (!targetRefOrId) return

    // 连续点击同一封邮件时防止重复请求 (PR-MAIL-04)
    if (detailInFlightRef.current === targetRefOrId) {
      return
    }

    // 边界 2：切换到另一封未缓存邮件，中止前一请求并建立当前 controller 所有权
    detailAbortRef.current?.abort()
    const controller = new AbortController()
    detailAbortRef.current = controller
    detailInFlightRef.current = targetRefOrId

    setDetailLoading(true)
    try {
      const resp = await getMessageDetail(accountId, targetRefOrId, controller.signal)
      if (
        sessionGen !== moduleSessionGen ||
        currentGen !== accountGenRef.current ||
        detailAbortRef.current !== controller ||
        controller.signal.aborted
      ) {
        return
      }
      // 【PR-02 契约】消费规范响应中的 response.message
      const fullMsg = resp.message
      if (fullMsg.message_ref) {
        const fullKey = buildMailCacheKey(accountId, fullMsg)
        setModuleMessageCache(fullKey, fullMsg)
        messageCacheRef.current.set(fullKey, fullMsg)
      } else {
        setModuleMessageCache(primaryKey, fullMsg)
        messageCacheRef.current.set(primaryKey, fullMsg)
      }
      setDetail(fullMsg)
    } catch (err) {
      if (
        sessionGen === moduleSessionGen &&
        currentGen === accountGenRef.current &&
        detailAbortRef.current === controller &&
        !(err instanceof ApiError && err.code === 'ABORTED')
      ) {
        show(err instanceof ApiError ? err.message : '读取邮件详情失败')
      }
    } finally {
      // 只有拥有当前所有权的 controller 才能清理当前 loading 与 inFlight 状态，严禁旧请求清理新请求
      if (detailAbortRef.current === controller) {
        detailAbortRef.current = null
        detailInFlightRef.current = null
        if (sessionGen === moduleSessionGen && currentGen === accountGenRef.current) {
          setDetailLoading(false)
        }
      }
    }
  }, [accountId, show])

  const deleteMessage = useCallback(async () => {
    if (!deleteFor) return
    setDeleting(true)
    try {
      const targetRefOrId = deleteFor.message_ref || deleteFor.id
      await request(
        `/api/inbox/${encodeURIComponent(targetRefOrId)}?account_id=${encodeURIComponent(accountId)}`,
        { method: 'DELETE' },
      )
      const key = buildMailCacheKey(accountId, deleteFor)
      moduleMessageCache.delete(key)
      messageCacheRef.current.delete(key)
      setDeleteFor(null)
      setDetail(null)
      show('邮件已删除')
      setRetryKey((k) => k + 1)
    } catch (err) {
      show(err instanceof ApiError ? err.message : '删除邮件失败')
    } finally {
      setDeleting(false)
    }
  }, [deleteFor, accountId, show])

  const handleCopyCode = useCallback(async (code: string) => {
    if (copiedTimerRef.current) clearTimeout(copiedTimerRef.current)
    const ok = await copyText(code)
    setCopiedCode(code)
    copiedTimerRef.current = setTimeout(() => {
      setCopiedCode(null)
    }, 1600)
    const notifyMsg = ok ? `验证码 [${code}] 已复制` : `验证码：${code}`
    show(notifyMsg)
    if (onCopySuccess) onCopySuccess(notifyMsg)
  }, [show, onCopySuccess])

  const handleCopyAlias = useCallback(async (aliasText: string) => {
    if (copiedAliasTimerRef.current) clearTimeout(copiedAliasTimerRef.current)
    await copyText(aliasText)
    setCopiedAlias(aliasText)
    copiedAliasTimerRef.current = setTimeout(() => setCopiedAlias(null), 1600)
    const notifyMsg = `别名 [${aliasText}] 已复制`
    show(notifyMsg)
    if (onCopySuccess) onCopySuccess(notifyMsg)
  }, [show, onCopySuccess])

  const handleOpenMessage = useCallback((msg: InboxMessage) => {
    void openMessage(msg)
  }, [openMessage])

  const handleCopyCodeAction = useCallback((c: string) => {
    void handleCopyCode(c)
  }, [handleCopyCode])

  const handleCopyAliasAction = useCallback((a: string) => {
    void handleCopyAlias(a)
  }, [handleCopyAlias])

  const handleDeleteAction = useCallback((msg: InboxMessage) => {
    setDeleteFor(msg)
  }, [])

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
      .map((f) => ({ value: f.name, label: `${f.display_name || f.name} (${f.role})` }))
    return [...base, ...extra]
  }, [folders])

  const rawMessages = useMemo(() => (Array.isArray(result?.messages) ? result.messages : []), [result])

  // 服务端按多 Header 检索并返回权威结果；
  // 数据层一次性计算并附加 OTP 嗅探与发件人解析结果，彻底消灭渲染视图层地毯式大正则计算
  const filteredMessages = useMemo(() => {
    return rawMessages
      .slice()
      .sort((a, b) => (dateTimestamp(b.date) ?? 0) - (dateTimestamp(a.date) ?? 0))
      .map((m) => {
        const primaryKey = buildMailCacheKey(accountId, m)
        const cached = moduleMessageCache.get(primaryKey)
        const body = m.body || cached?.body
        const preview = m.preview || cached?.preview || body || ''
        const otp = extractOTPMemoized(buildSniffContext(m.subject, preview, body))
        const sender = parseSenderInfo(m.from)
        return {
          ...m,
          preview,
          body,
          otp,
          sender,
        }
      })
  }, [rawMessages, accountId])

  useEffect(() => {
    if (onCountChange) {
      onCountChange(filteredMessages.length)
    }
  }, [filteredMessages.length, onCountChange])

  const currentAccountName = currentAccount?.name || currentAccount?.real_email || (accountId || '未选择')
  const totalCount = result?.count ?? filteredMessages.length
  const codesDetectedCount = useMemo(() => {
    return filteredMessages.filter((m) => Boolean(m.otp?.code)).length
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

            {isRevalidating && (
              <span className="card-stat-pill" style={{ opacity: 0.85 }} title="正在后台获取最新邮件">
                <span className="status-dot active" style={{ backgroundColor: 'var(--color-primary, #0071e3)' }} />
                <span>更新中…</span>
              </span>
            )}

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

            {isWebMailOnly && (
              <span className="card-stat-pill" style={{ color: '#e6a23c', borderColor: 'rgba(230,162,60,0.3)' }} title="当前账号未配置 App 专用密码，运行于 WebMail 模式，仅支持默认收件箱拉取，文件夹与天数筛选已禁用">
                <span>WebMail 模式 (仅支持基础收件箱)</span>
              </span>
            )}
          </div>
        </div>

        {/* 统一工具栏 */}
        <InboxFilterBar
          fixedAccount={Boolean(fixedAccount)}
          accountId={accountId}
          accounts={accounts}
          onAccountChange={handleAccountChange}
          alias={alias}
          aliases={aliases}
          onAliasChange={(val) => setAlias(val)}
          folder={folder}
          folderOptions={folderOptions}
          onFolderChange={(val) => {
            setFolder(val)
            ensureFoldersLoaded()
          }}
          onFolderInteract={ensureFoldersLoaded}
          isWebMailOnly={isWebMailOnly}
          limit={limit}
          onLimitChange={(val) => setLimit(val)}
          days={days}
          onDaysChange={(val) => setDays(val)}
          autoRefreshInterval={autoRefreshInterval}
          onAutoRefreshIntervalChange={(val) => setAutoRefreshInterval(val)}
          loading={loading}
          onSearch={handleSearch}
        />

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
                    key={m.message_ref || `${m.folder || 'INBOX'}:${m.id}`}
                    message={m}
                    copiedCode={copiedCode}
                    copiedAlias={copiedAlias}
                    onOpenMessage={handleOpenMessage}
                    onCopyCode={handleCopyCodeAction}
                    onCopyAlias={handleCopyAliasAction}
                    onDelete={handleDeleteAction}
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
