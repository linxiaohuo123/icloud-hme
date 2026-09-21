/**
 * [INPUT]: 依赖 api/client 的 request/ApiError，依赖 api/types 的 AccountSummary，依赖 react 的 useState/useEffect/useCallback
 * [OUTPUT]: 对外提供 useAccounts hook 与 fetchAccountsDeduped 共享拉取函数
 * [POS]: web/src/hooks 的全局账号数据流中枢，基于 SWR 与请求去重消灭全站 7 处冗余轮询
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { useCallback, useEffect, useState } from 'react'
import { request } from '../api/client'
import type { AccountSummary } from '../api/types'

// 缓存有效期 30 秒 (SWR 策略)
const CACHE_TTL = 30_000

let cacheData: AccountSummary[] = []
let cacheTime = 0
let inflightPromise: Promise<AccountSummary[]> | null = null
const listeners = new Set<(data: AccountSummary[]) => void>()

function notifyListeners(data: AccountSummary[]) {
  cacheData = data
  cacheTime = Date.now()
  listeners.forEach((fn) => fn(data))
}

/** 清理全局账号缓存（测试重置或手动强制失效时调用） */
export function clearAccountsCache() {
  cacheData = []
  cacheTime = 0
  inflightPromise = null
}

/**
 * fetchAccountsDeduped 执行带去重的全量账号拉取。
 * 并发调用共享同一个网络 Promise，避免瞬态多组件连环请求。
 */
export async function fetchAccountsDeduped(force = false): Promise<AccountSummary[]> {
  const now = Date.now()
  if (!force && cacheData.length > 0 && now - cacheTime < CACHE_TTL) {
    return cacheData
  }

  if (inflightPromise) {
    return inflightPromise
  }

  inflightPromise = request<AccountSummary[]>('/api/accounts')
    .then((data) => {
      const list = Array.isArray(data) ? data : []
      notifyListeners(list)
      return list
    })
    .finally(() => {
      inflightPromise = null
    })

  return inflightPromise
}

export function useAccounts() {
  const [accounts, setAccounts] = useState<AccountSummary[]>(cacheData)
  const [loading, setLoading] = useState(cacheData.length === 0)
  const [error, setError] = useState<string | null>(null)

  const refresh = useCallback(async (force = true) => {
    setLoading(true)
    try {
      const data = await fetchAccountsDeduped(force)
      setAccounts(data)
      setError(null)
      return data
    } catch (err) {
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

    // 挂载时若缓存已过期或为空，静默刷新
    if (cacheData.length === 0 || Date.now() - cacheTime >= CACHE_TTL) {
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
