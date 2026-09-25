/**
 * [INPUT]: 依赖 api/client 的 request/ApiError，依赖 api/types 的 AccountSummary，依赖 react 的 useState/useEffect/useCallback
 * [OUTPUT]: 对外提供 useAccounts hook 与 fetchAccountsDeduped 共享拉取函数, clearAccountsCache, invalidateAccounts, getAccountsCacheState
 * [POS]: web/src/hooks 的全局账号数据流中枢，具备 generation 保护、AbortController 取消、登出隔离与 SWR 缓存
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useCallback, useEffect, useState } from 'react'
import { request } from '../api/client'
import type { AccountSummary } from '../api/types'

// 缓存有效期 30 秒 (SWR 策略)
const CACHE_TTL = 30_000

let cacheData: AccountSummary[] = []
let hasCache = false
let cacheTime = 0
let currentGeneration = 0
let inflight: {
  promise: Promise<AccountSummary[]>
  controller: AbortController
  generation: number
} | null = null

const listeners = new Set<(data: AccountSummary[]) => void>()

function notifyListeners(data: AccountSummary[]) {
  cacheData = data
  hasCache = true
  cacheTime = Date.now()
  listeners.forEach((fn) => fn(data))
}

/** 清理全局账号缓存（测试重置、登出或手动强制失效时调用） */
export function clearAccountsCache() {
  currentGeneration++
  if (inflight) {
    inflight.controller.abort()
    inflight = null
  }
  cacheData = []
  hasCache = false
  cacheTime = 0
  listeners.forEach((fn) => fn([]))
}

/** 供测试与调试观察内部缓存状态 */
export function getAccountsCacheState() {
  return {
    cacheData,
    hasCache,
    cacheTime,
    currentGeneration,
    hasInflight: inflight !== null,
  }
}

// 统一监听 auth-logout，隔离登出后在途请求与缓存
if (typeof window !== 'undefined') {
  window.addEventListener('auth-logout', () => {
    clearAccountsCache()
  })
}

/**
 * 统一账号缓存失效入口 (仅在真实 mutation 成功后调用)
 * 职责:
 * 1. 让 accounts cache 失效;
 * 2. abort stale accounts GET 并递增 generation;
 * 3. dispatch account-updated 事件, detail 附带 accountId。
 */
export function invalidateAccounts(accountId?: string) {
  currentGeneration++
  if (inflight) {
    inflight.controller.abort()
    inflight = null
  }
  hasCache = false
  cacheTime = 0
  if (typeof window !== 'undefined') {
    window.dispatchEvent(
      new CustomEvent('account-updated', { detail: { accountId } }),
    )
  }
}

/**
 * fetchAccountsDeduped 执行带去重的全量账号拉取。
 * 并发调用共享同一个网络 Promise，避免瞬态多组件连环请求。
 * force=true 强制使旧在途请求失效，递增 generation 并重新向服务器拉取。
 */
export async function fetchAccountsDeduped(force = false): Promise<AccountSummary[]> {
  const now = Date.now()
  // 命中有效 TTL 缓存 (包含合法的空账号列表 [])
  if (!force && hasCache && now - cacheTime < CACHE_TTL) {
    return cacheData
  }

  // 并发请求复用 (dedupe)
  if (!force && inflight) {
    return inflight.promise
  }

  // force refresh: 必须显式使旧在途请求失效
  if (force && inflight) {
    inflight.controller.abort()
    inflight = null
  }

  currentGeneration++
  const requestGen = currentGeneration
  const controller = new AbortController()

  const promise = request<AccountSummary[]>('/api/accounts', { signal: controller.signal })
    .then((data) => {
      // 必须通过 generation 与 abort 状态双重校验
      if (requestGen !== currentGeneration || controller.signal.aborted) {
        // 请求已被淘汰，绝不能更新 cache，绝不能 notifyListeners
        return cacheData
      }
      const list = Array.isArray(data) ? data : []
      notifyListeners(list)
      return list
    })
    .catch((err) => {
      // 若请求已被 abort 或已被新请求 supersede，不向外抛出未捕获错误
      if (controller.signal.aborted || requestGen !== currentGeneration) {
        return cacheData
      }
      throw err
    })
    .finally(() => {
      if (inflight?.generation === requestGen) {
        inflight = null
      }
    })

  inflight = {
    promise,
    controller,
    generation: requestGen,
  }

  return promise
}

export function useAccounts() {
  const [accounts, setAccounts] = useState<AccountSummary[]>(cacheData)
  const [loading, setLoading] = useState(!hasCache)
  const [error, setError] = useState<string | null>(null)

  const refresh = useCallback(async (force = true) => {
    setLoading(true)
    try {
      const data = await fetchAccountsDeduped(force)
      setAccounts(data)
      setError(null)
      return data
    } catch (err) {
      if (err instanceof Error && err.name === 'ApiError' && (err as { code?: string }).code === 'ABORTED') {
        return cacheData
      }
      const msg = err instanceof Error ? err.message : '获取账号列表失败'
      setError(msg)
      throw err
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    // 挂载时订阅全局数据更新
    const handler = (data: AccountSummary[]) => {
      setAccounts(data)
      setLoading(false)
    }
    listeners.add(handler)

    // 挂载时若未缓存或已过期，静默刷新
    if (!hasCache || Date.now() - cacheTime >= CACHE_TTL) {
      void refresh(false)
    } else {
      setAccounts(cacheData)
      setLoading(false)
    }

    // 监听全局 account-updated 事件（如新建、修改、切换别名后立即刷新）
    const onAccountUpdated = () => {
      void refresh(true)
    }
    window.addEventListener('account-updated', onAccountUpdated)

    return () => {
      listeners.delete(handler)
      window.removeEventListener('account-updated', onAccountUpdated)
    }
  }, [refresh])

  return { accounts, loading, error, refresh }
}
