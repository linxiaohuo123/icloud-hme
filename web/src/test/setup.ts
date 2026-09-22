/**
 * [INPUT]: 依赖 @testing-library/jest-dom/vitest, @testing-library/react, vitest, ./server, hooks/useAccounts
 * [OUTPUT]: 测试环境生命周期挂载与全局清理
 * [POS]: web/src/test 的 Vitest 运行环境初始化脚本，配置 MSW 模拟服务器与 React 树清理
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterAll, afterEach, beforeAll } from 'vitest'
import { server } from './server'
import { clearAccountsCache } from '../hooks/useAccounts'

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  cleanup()
  server.resetHandlers()
  clearAccountsCache()
})
afterAll(() => server.close())
