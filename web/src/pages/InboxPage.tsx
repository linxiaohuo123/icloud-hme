/**
 * [INPUT]: 依赖 components/inbox/InboxTableView
 * [OUTPUT]: 对外提供 InboxPage 全局收件箱摘要页面
 * [POS]: web/src/pages 的全局收件箱入口，挂载 InboxTableView 统一内核提供跨账号邮件巡检与验证码大盘
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import InboxTableView from '../components/inbox/InboxTableView'

export default function InboxPage() {
  return <InboxTableView fixedAccount={false} showPageHeader={true} />
}
