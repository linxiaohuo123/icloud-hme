/**
 * [INPUT]: 依赖 components/inbox/InboxTableView, api/types 的 Alias
 * [OUTPUT]: 对外提供 WorkspaceInboxTab 单账号工作台收件箱视图
 * [POS]: web/src/pages/workspace 的收件箱标签页，封装 InboxTableView 并将父级 aliases 直传消灭冗余 I/O
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import InboxTableView from '../../components/inbox/InboxTableView'
import type { AccountSummary, Alias } from '../../api/types'

export interface WorkspaceInboxTabProps {
  accountId?: string
  accountSummary?: AccountSummary | null
  aliases?: Alias[]
  selectedAlias?: string
  onSelectAlias?: (val: string) => void
  onCopySuccess?: (msg: string) => void
  onCountChange?: (count: number) => void
}

export default function WorkspaceInboxTab({
  accountId = '',
  accountSummary,
  aliases,
  selectedAlias,
  onCopySuccess,
  onCountChange,
}: WorkspaceInboxTabProps) {
  return (
    <InboxTableView
      accountId={accountId}
      accountSummary={accountSummary}
      fixedAccount={true}
      initialAlias={selectedAlias}
      externalAliases={aliases}
      onCopySuccess={onCopySuccess}
      onCountChange={onCountChange}
    />
  )
}
