/**
 * [INPUT]: 无外部依赖
 * [OUTPUT]: 导出 buildMailCacheKey 规范化邮件缓存键生成器
 * [POS]: web/src/utils 的邮件工具函数，供 InboxTableView 与单元测试消费
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

/** 规范化邮件详情缓存键构造函数 (PR-02)，基于完整 message_ref 或文件夹与 UID 隔离 */
export function buildMailCacheKey(
  accountId: string,
  msg: { message_ref?: string; folder?: string; id?: string; uid_validity?: number; uid?: number },
): string {
  if (msg.message_ref) {
    return `${accountId}:${msg.message_ref}`
  }
  const folder = msg.folder || 'INBOX'
  const uid = msg.uid ?? msg.id
  const uv = msg.uid_validity ?? 0
  return `${accountId}:${folder}:${uv}:${uid}`
}
