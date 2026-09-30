/**
 * [INPUT]: 依赖 react, api/client (request, getAuthGeneration, registerUnauthorizedHandler, setCSRFToken), api/types (LoginResult)
 * [OUTPUT]: 对外提供 AuthProvider, useAuth
 * [POS]: web/src/auth 的全局认证状态提供者与鉴权上下文
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from 'react'
import { ApiError, request, getAuthGeneration, registerUnauthorizedHandler, setCSRFToken } from '../api/client'
import type { LoginResult } from '../api/types'

type AuthStatus = 'checking' | 'anonymous' | 'authenticated'

interface AuthContextValue {
  status: AuthStatus
  login: (password: string) => Promise<void>
  logout: () => Promise<void>
}

const AuthContext = createContext<AuthContextValue | null>(null)

// eslint-disable-next-line react-refresh/only-export-components
export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext)
  if (!ctx) {
    throw new Error('useAuth 必须在 AuthProvider 内使用')
  }
  return ctx
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<AuthStatus>('checking')

  useEffect(() => {
    const controller = new AbortController()
    const generation = getAuthGeneration()
    request<LoginResult>('/api/auth/session', { signal: controller.signal })
      .then((data) => {
        if (controller.signal.aborted || generation !== getAuthGeneration()) return
        setCSRFToken(data.csrf_token)
        setStatus('authenticated')
      })
      .catch(() => {
        if (controller.signal.aborted || generation !== getAuthGeneration()) return
        setStatus('anonymous')
      })
    return () => {
      controller.abort()
    }
  }, [])


  const logout = useCallback(async () => {
    try {
      await request('/api/auth/logout', { method: 'POST' })
    } catch (err) {
      if (!(err instanceof ApiError && err.status === 401 && err.code === 'AUTH_REQUIRED')) {
        throw err
      }
    }
    setCSRFToken(null)
    setStatus('anonymous')
    if (typeof window !== 'undefined') {
      window.dispatchEvent(new CustomEvent('auth-logout'))
    }
  }, [])

  const handleUnauthorized = useCallback(() => {
    setCSRFToken(null)
    setStatus('anonymous')
    if (typeof window !== 'undefined') {
      window.dispatchEvent(new CustomEvent('auth-logout'))
    }
  }, [])

  useEffect(() => {
    registerUnauthorizedHandler(handleUnauthorized)
    return () => registerUnauthorizedHandler(null)
  }, [handleUnauthorized])
  const login = useCallback(async (password: string) => {
    const data = await request<LoginResult>('/api/auth/login', {
      method: 'POST',
      body: JSON.stringify({ password }),
    })
    setCSRFToken(data.csrf_token)
    setStatus('authenticated')
  }, [])

  const value = useMemo(
    () => ({ status, login, logout }),
    [status, login, logout],
  )

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}
