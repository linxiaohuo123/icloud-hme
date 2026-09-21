/**
 * [INPUT]: 依赖 components/inbox/InboxTableView, api/types 的 Alias
 * [OUTPUT]: 对外提供 WorkspaceInboxTab 单账号工作台收件箱视图
 * [POS]: web/src/pages/workspace 的收件箱标签页，封装 InboxTableView 为当前账号提供锁定专属邮件与验证码工作台
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import InboxTableView from '../../components/inbox/InboxTableView'
import type { Alias } from '../../api/types'

export interface WorkspaceInboxTabProps {
  accountId?: string
  aliases?: Alias[]
  selectedAlias?: string
  onSelectAlias?: (val: string) => void
  onCopySuccess?: (msg: string) => void
  onCountChange?: (count: number) => void
}

export default function WorkspaceInboxTab({
  accountId = '',
  selectedAlias,
  onCopySuccess,
  onCountChange,
}: WorkspaceInboxTabProps) {
  return (
    <InboxTableView
      accountId={accountId}
      fixedAccount={true}
      initialAlias={selectedAlias}
      onCopySuccess={onCopySuccess}
      onCountChange={onCountChange}
    />
  )
}
