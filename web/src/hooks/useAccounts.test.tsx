import { beforeEach, describe, expect, it } from 'vitest'
import { waitFor } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { server } from '../test/server'
import {
  clearAccountsCache,
  fetchAccountsDeduped,
  getAccountsCacheState,
} from './useAccounts'
import type { AccountSummary } from '../api/types'

function createDeferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

const mockAccountA: AccountSummary = {
  id: 'acc_a',
  name: 'Account A',
  real_email: 'a@example.com',
  icloud_email: 'a@icloud.com',
  host: 'imap.mail.me.com',
  status: 'active',
  alias_total: 10,
  alias_active: 8,
  has_cookies: true,
  has_app_password: true,
  has_proxy: false,
  last_validated: new Date().toISOString(),
  created_at: new Date().toISOString(),
}

const mockAccountB: AccountSummary = {
  id: 'acc_b',
  name: 'Account B',
  real_email: 'b@example.com',
  icloud_email: 'b@icloud.com',
  host: 'imap.mail.me.com',
  status: 'active',
  alias_total: 20,
  alias_active: 15,
  has_cookies: true,
  has_app_password: true,
  has_proxy: false,
  last_validated: new Date().toISOString(),
  created_at: new Date().toISOString(),
}

describe('useAccounts Global Cache Lifecycle', () => {
  beforeEach(() => {
    clearAccountsCache()
    server.resetHandlers()
  })

  it('TestAccountsCache_LogoutRejectsLateResponse: session A 请求在途时登出，晚返回的响应绝不写入缓存', async () => {
    const deferredA = createDeferred<AccountSummary[]>()

    server.use(
      http.get('/api/accounts', async () => {
        const data = await deferredA.promise
        return HttpResponse.json({ success: true, data })
      }),
    )

    // 1. Session A 启动拉取并挂起
    const fetchPromise = fetchAccountsDeduped(false)
    expect(getAccountsCacheState().hasInflight).toBe(true)

    // 2. 模拟用户登出 (派发 auth-logout)
    window.dispatchEvent(new CustomEvent('auth-logout'))

    const stateAfterLogout = getAccountsCacheState()
    expect(stateAfterLogout.cacheData).toEqual([])
    expect(stateAfterLogout.hasCache).toBe(false)
    expect(stateAfterLogout.hasInflight).toBe(false)

    // 3. 此时 Session A 的晚返回响应到达
    deferredA.resolve([mockAccountA])
    await fetchPromise

    // 4. 验证: 缓存必须仍然为空，晚返回数据不得污染新环境
    const stateAfterLateResponse = getAccountsCacheState()
    expect(stateAfterLateResponse.cacheData).toEqual([])
    expect(stateAfterLateResponse.hasCache).toBe(false)

    // 5. 新 session 再次 fetch 必须重新发起网络请求并拿到最新数据
    server.use(
      http.get('/api/accounts', () => {
        return HttpResponse.json({ success: true, data: [mockAccountB] })
      }),
    )
    const newSessionData = await fetchAccountsDeduped(false)
    expect(newSessionData).toEqual([mockAccountB])
    expect(getAccountsCacheState().cacheData).toEqual([mockAccountB])
    expect(getAccountsCacheState().hasCache).toBe(true)
  })

  it('TestAccountsCache_ForceRefreshSupersedesInflightRequest: force refresh 真正使旧在途 GET 失效，晚返回不可覆盖新数据', async () => {
    const deferred1 = createDeferred<AccountSummary[]>()
    const deferred2 = createDeferred<AccountSummary[]>()

    let requestCount = 0
    server.use(
      http.get('/api/accounts', async () => {
        requestCount++
        if (requestCount === 1) {
          const data = await deferred1.promise
          return HttpResponse.json({ success: true, data })
        }
        const data = await deferred2.promise
        return HttpResponse.json({ success: true, data })
      }),
    )

    // 1. GET #1 启动并挂起 (模拟旧数据)
    const p1 = fetchAccountsDeduped(false)
    await waitFor(() => expect(requestCount).toBe(1))

    // 2. 触发 mutation 后的 force refresh (GET #2)
    const p2 = fetchAccountsDeduped(true)
    await waitFor(() => expect(requestCount).toBe(2))

    // 3. GET #2 先完成返回新数据 [mockAccountB]
    deferred2.resolve([mockAccountB])
    const res2 = await p2
    expect(res2).toEqual([mockAccountB])
    expect(getAccountsCacheState().cacheData).toEqual([mockAccountB])

    // 4. 再放行 GET #1 返回旧数据 [mockAccountA]
    deferred1.resolve([mockAccountA])
    await p1

    // 5. 验证: 最终 cache 必须仍为 new 数据，old response 绝不能覆盖 new
    expect(getAccountsCacheState().cacheData).toEqual([mockAccountB])
  })

  it('TestAccountsCache_EmptyResultIsCached: 空账号列表合法进入 TTL 缓存，避免重复请求', async () => {
    let networkCalls = 0
    server.use(
      http.get('/api/accounts', () => {
        networkCalls++
        return HttpResponse.json({ success: true, data: [] })
      }),
    )

    // 第一次拉取: 访问网络
    const first = await fetchAccountsDeduped(false)
    expect(first).toEqual([])
    expect(networkCalls).toBe(1)
    expect(getAccountsCacheState().hasCache).toBe(true)
    expect(getAccountsCacheState().cacheData).toEqual([])

    // 第二次拉取 (未超时且非 force): 应当直接命中缓存，不发网络请求
    const second = await fetchAccountsDeduped(false)
    expect(second).toEqual([])
    expect(networkCalls).toBe(1)
  })
})
