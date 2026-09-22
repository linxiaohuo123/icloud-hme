/**
 * [INPUT]: 依赖 components/Select, api/types (AccountSummary, Alias)
 * [OUTPUT]: 对外提供 InboxFilterBar 组件，封装收件箱筛选栏 (账号、别名、文件夹、每页、时间范围、自动刷新)
 * [POS]: web/src/components/inbox 的筛选器组件，为 InboxTableView 提供独立的筛选交互与控制
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

import type { AccountSummary, Alias } from '../../api/types'
import Select from '../Select'

export interface InboxFilterBarProps {
  fixedAccount: boolean
  accountId: string
  accounts: AccountSummary[]
  onAccountChange: (val: string) => void
  alias: string
  aliases: Alias[]
  onAliasChange: (val: string) => void
  folder: string
  folderOptions: { value: string; label: string }[]
  onFolderChange: (val: string) => void
  isWebMailOnly: boolean
  limit: number
  onLimitChange: (val: number) => void
  days: number
  onDaysChange: (val: number) => void
  autoRefreshInterval: number
  onAutoRefreshIntervalChange: (val: number) => void
  loading: boolean
  onSearch: () => void
}

export default function InboxFilterBar({
  fixedAccount,
  accountId,
  accounts,
  onAccountChange,
  alias,
  aliases,
  onAliasChange,
  folder,
  folderOptions,
  onFolderChange,
  isWebMailOnly,
  limit,
  onLimitChange,
  days,
  onDaysChange,
  autoRefreshInterval,
  onAutoRefreshIntervalChange,
  loading,
  onSearch,
}: InboxFilterBarProps) {
  return (
    <div className="inbox-filter-bar">
      {!fixedAccount && (
        <div className="inbox-filter-item">
          <label htmlFor="inbox-account" className="inbox-filter-label">
            账号
          </label>
          <Select
            id="inbox-account"
            aria-label="账号"
            value={accountId}
            onChange={onAccountChange}
            options={accounts.map((a) => ({ value: a.id, label: a.name || a.real_email }))}
            style={{ minWidth: 160 }}
          />
        </div>
      )}

      <div className="inbox-filter-item">
        <label htmlFor="inbox-alias" className="inbox-filter-label">
          别名
        </label>
        <Select
          id="inbox-alias"
          aria-label="别名"
          value={alias}
          onChange={onAliasChange}
          options={[
            { value: '', label: fixedAccount ? '全部别名邮件' : '全部' },
            ...aliases.map((a) => ({
              value: a.email,
              label: a.email,
            })),
          ]}
          style={{ minWidth: 190 }}
        />
      </div>

      <div className="inbox-filter-item">
        <label htmlFor="inbox-folder" className="inbox-filter-label">
          文件夹 {isWebMailOnly && <span style={{ opacity: 0.6, fontSize: '0.85em' }}>(WebMail固定)</span>}
        </label>
        <Select
          id="inbox-folder"
          aria-label="文件夹"
          value={isWebMailOnly ? 'INBOX' : folder}
          onChange={onFolderChange}
          options={isWebMailOnly ? [{ value: 'INBOX', label: '收件箱 (WebMail模式)' }] : folderOptions}
          disabled={isWebMailOnly}
          style={{ minWidth: 200, opacity: isWebMailOnly ? 0.6 : 1 }}
        />
      </div>

      <div className="inbox-filter-item">
        <label htmlFor="inbox-limit" className="inbox-filter-label">
          每页
        </label>
        <Select
          id="inbox-limit"
          aria-label="每页"
          value={limit}
          onChange={(val) => onLimitChange(Number(val))}
          options={[
            { value: 1, label: '1' },
            { value: 20, label: '20' },
            { value: 100, label: '100' },
          ]}
          style={{ minWidth: 72 }}
        />
      </div>

      <div className="inbox-filter-item">
        <label htmlFor="inbox-days" className="inbox-filter-label">
          时间范围 {isWebMailOnly && <span style={{ opacity: 0.6, fontSize: '0.85em' }}>(全量)</span>}
        </label>
        <Select
          id="inbox-days"
          aria-label="时间范围"
          value={days}
          onChange={(val) => onDaysChange(Number(val))}
          options={[
            { value: 1, label: '1 天' },
            { value: 7, label: '7 天' },
            { value: 30, label: '30 天' },
            { value: 90, label: '90 天' },
          ]}
          disabled={isWebMailOnly}
          style={{ minWidth: 90, opacity: isWebMailOnly ? 0.6 : 1 }}
        />
      </div>

      <div className="inbox-filter-item">
        <label htmlFor="inbox-autorefresh" className="inbox-filter-label">
          自动刷新
        </label>
        <Select
          id="inbox-autorefresh"
          aria-label="自动刷新"
          value={autoRefreshInterval}
          onChange={(val) => onAutoRefreshIntervalChange(Number(val))}
          options={[
            { value: 0, label: '关闭' },
            { value: 10, label: '10 秒' },
            { value: 30, label: '30 秒' },
            { value: 60, label: '60 秒' },
          ]}
          style={{ minWidth: 85 }}
        />
      </div>

      <button
        type="button"
        className="btn btn-primary"
        onClick={onSearch}
        disabled={loading}
      >
        查询
      </button>
    </div>
  )
}
