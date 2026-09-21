/**
 * [INPUT]: 依赖 msw/node 的 setupServer 与本地 handlers
 * [OUTPUT]: 对外提供 server 测试拦截器实例
 * [POS]: web/src/test 的测试服务器实例入口
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { setupServer } from 'msw/node'
import { handlers } from './handlers'

export const server = setupServer(...handlers)
