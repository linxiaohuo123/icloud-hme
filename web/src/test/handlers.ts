/**
 * [INPUT]: 依赖 msw 的 http/HttpResponse
 * [OUTPUT]: 对外提供 handlers 测试拦截预设（含 auth/accounts/aliases 兜底）
 * [POS]: web/src/test 的基础 MSW 路由配置
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import { http, HttpResponse } from 'msw'

export const handlers = [
  http.post('/api/auth/login', () =>
    HttpResponse.json(
      { success: false, code: 'INVALID_CREDENTIALS', message: '管理员密码错误' },
      { status: 401 },
    ),
  ),
  http.get('/api/auth/session', () =>
    HttpResponse.json(
      { success: false, code: 'AUTH_REQUIRED', message: '请先登录' },
      { status: 401 },
    ),
  ),
  http.get('/api/accounts', () =>
    HttpResponse.json({ success: true, data: [] }),
  ),
  http.get('/api/accounts/:id', ({ params }) =>
    HttpResponse.json({
      success: true,
      data: {
        id: params.id,
        name: '测试账号',
        status: 'active',
        real_email: 'test@example.com',
        icloud_email: 'test@icloud.com',
        alias_total: 0,
        alias_active: 0,
      },
    }),
  ),
  http.get('/api/aliases', () =>
    HttpResponse.json({ success: true, data: { aliases: [], count: 0 } }),
  ),
  http.get('/api/mailboxes', () =>
    HttpResponse.json({ success: true, data: { folders: [] } }),
  ),
  http.post('/api/messages', () =>
    HttpResponse.json({ success: true, data: { messages: [], count: 0 } }),
  ),
]
