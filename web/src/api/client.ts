/**
 * [INPUT]: 依赖 api/types 的 ApiResponse 契约
 * [OUTPUT]: 导出 request 请求函数、ApiError、CSRF 与 401 统一处理器
 * [POS]: web/src/api 的通信中枢，所有前端请求的唯一出口
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import type { ApiResponse, MessageDetailResponse } from './types'

/** CSRF token,仅存 React 内存状态 */
let csrfToken: string | null = null

/** 全局 401 回调(由 AuthProvider 注册,避免循环 import) */
let unauthorizedHandler: (() => void) | null = null

/** 注册全局 401 回调 */
export function registerUnauthorizedHandler(handler: (() => void) | null): void {
  unauthorizedHandler = handler
}

/** 设置内存 CSRF token(由 AuthProvider 管理) */
export function setCSRFToken(token: string | null): void {
  csrfToken = token
}

/** ApiError 携带 HTTP 状态与稳定错误码 */
export class ApiError extends Error {
  readonly status: number
  readonly code: string

  constructor(status: number, code: string, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
  }
}

interface RequestOptions extends Omit<RequestInit, 'body'> {
  body?: unknown
  signal?: AbortSignal
}

/**
 * 唯一的 fetch 入口。
 *
 * 统一设置 Accept、JSON Content-Type 与 credentials: same-origin;
 * 非 GET/HEAD/OPTIONS 自动携带 X-CSRF-Token;401 触发 onUnauthorized 回调。
 */
export async function request<T>(
  path: string,
  init?: RequestOptions,
  onUnauthorized?: () => void,
): Promise<T> {
  const headers = new Headers(init?.headers)
  headers.set('Accept', 'application/json')
  headers.set('Content-Type', 'application/json')

  const method = (init?.method ?? 'GET').toUpperCase()
  if (method !== 'GET' && method !== 'HEAD' && method !== 'OPTIONS' && csrfToken) {
    headers.set('X-CSRF-Token', csrfToken)
  }

  let body: BodyInit | undefined
  if (init?.body !== undefined) {
    body = typeof init.body === 'string' ? init.body : JSON.stringify(init.body)
  }

  let resp: Response
  try {
    resp = await fetch(path, {
      ...init,
      method,
      headers,
      body,
      credentials: 'same-origin',
    })
  } catch {
    if (init?.signal?.aborted) {
      throw new ApiError(0, 'ABORTED', '请求已中止')
    }
    throw new ApiError(0, 'NETWORK_ERROR', '网络连接失败，请检查服务状态')
  }

  let payload: ApiResponse<T>
  try {
    payload = (await resp.json()) as ApiResponse<T>
  } catch {
    throw new ApiError(resp.status, 'INVALID_RESPONSE', '网络连接失败，请检查服务状态')
  }

  // 只有当服务端明确返回 AUTH_REQUIRED 且不是管理登录接口时，才判定为管理台会话过期
  if (resp.status === 401 && payload.code === 'AUTH_REQUIRED' && path !== '/api/auth/login') {
    onUnauthorized?.()
    unauthorizedHandler?.()
  }

  if (!resp.ok || payload.success === false) {
    throw new ApiError(
      resp.status,
      payload.code ?? 'INTERNAL_ERROR',
      payload.message ?? '请求失败',
    )
  }
  return payload.data as T
}

/** 规范化邮件详情获取适配器 (PR-02)，校验服务端 MessageDetailResponse 并提取 .message */
export async function getMessageDetail(
  accountId: string,
  messageRefOrId: string,
  signal?: AbortSignal,
): Promise<MessageDetailResponse> {
  const resp = await request<MessageDetailResponse>(
    `/api/inbox/${encodeURIComponent(messageRefOrId)}?account_id=${encodeURIComponent(accountId)}`,
    { signal },
  )
  if (!resp || typeof resp !== 'object' || !resp.message || typeof resp.message !== 'object') {
    throw new ApiError(500, 'INVALID_CONTRACT', '邮件详情响应契约异常: 缺失 message 字段')
  }
  return resp
}

