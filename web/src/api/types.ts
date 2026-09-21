/**
 * [INPUT]: 无外部依赖，作为整个前端的类型契约
 * [OUTPUT]: 导出 ApiResponse, AccountSummary, Alias, InboxMessage, BusinessTag, APIToken, LeaseRecord, ScheduleConfig, ScheduleLog, ScheduleStatus 等核心接口
 * [POS]: web/src/api 的类型定义中枢，与 internal/server 保持同构映射
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

/** 统一响应包裹 */
export interface ApiResponse<T> {
  success: boolean
  data?: T
  code?: string
  message?: string
}

/** 账号安全摘要(无秘密字段) */
export interface AccountSummary {
  id: string
  name: string
  real_email: string
  icloud_email: string
  host: string
  status: 'active' | 'pending' | 'error' | string
  alias_total: number
  alias_active: number
  has_cookies: boolean
  has_app_password: boolean
  has_proxy: boolean
  mailbox?: MailboxSummary
  last_validated: string
  status_message?: string
  created_at: string
  tags?: string[]
}

export interface MailboxSummary {
  provider: string
  email: string
  imap_host: string
  imap_port: number
}

/** HME 别名(iCloud 返回字段风格为 camelCase) */
export interface Alias {
  email: string
  anonymousId: string
  label: string
  active: boolean
  createdAt?: string
  account_id?: string
  account_name?: string
  accountId?: string
  accountName?: string
}

/** 邮件摘要 */
export interface InboxMessage {
  id: string
  folder?: string
  from: string
  to: string
  subject: string
  date: string
  preview: string
  unread?: boolean
}

export interface FullMessage extends InboxMessage {
  body: string
  content_type: string
}

/** 邮箱文件夹定义 */
export interface MailboxFolder {
  name: string
  role: string
}

/** 批量创建别名结果 */
export interface BatchCreateResult {
  account_id: string
  requested: number
  created: {
    email: string
    label: string
    created_at: string
  }[]
  created_count: number
  skipped_count: number
  remaining_this_hour: number
  message?: string
  last_error?: string
}

/** 收件箱查询结果 */
export interface InboxResult {
  account_id: string
  alias?: string
  folder?: string
  count: number
  messages: InboxMessage[]
  method: 'imap' | 'web_api'
}

/** 登录响应 */
export interface LoginResult {
  csrf_token: string
  expires_at: string
}

/** 业务标识定义 */
export interface BusinessTag {
  id: string
  name: string
  tag: string
  description: string
  status: string
  created_at: string
  last_assigned_at?: string
}

/** 外部 API 访问令牌 */
export interface APIToken {
  id: string
  name: string
  token?: string
  created_at: string
  last_used_at?: string
}

/** 已领用别名审计记录 */
export interface LeaseRecord {
  id: string
  email: string
  account_id: string
  tag: string
  status: string
  allocated_at: string
  completed_at?: string
  token_name?: string
}

/** 已用别名查询响应 */
export interface LeaseListResult {
  records: LeaseRecord[]
  total: number
}

/** 定时调度器配置 */
export interface ScheduleConfig {
  account_id: string
  enabled: boolean
  hourly_quota: number
  alias_label?: string
  current_hour_count: number
  last_run_at?: string
  mode?: 'always' | 'daily_window' | 'duration'
  start_time?: string
  end_time?: string
  duration_hours?: number
  started_at?: string
}

/** 批量获取邮件正文请求 */
export interface BatchMessageReq {
  account_id: string
  messages: { folder: string; id: string }[]
}


/** 调度引擎运行日志 */
export interface ScheduleLog {
  time: string
  message: string
}

/** 调度器实时状态快照 */
export interface ScheduleStatus {
  running: boolean
  last_run_at?: string
  interval_seconds: number
}

/** 通知配置 */
export interface NotifySettings {
  feishu_webhook: string
  bark_url: string
  telegram_token: string
  telegram_chat: string
  event_kinds: Record<string, boolean> | null
  quota_threshold: number
  resend_minutes?: number
}

/** 单渠道测试推送结果 */
export interface NotifyChannelResult {
  channel: string
  ok: boolean
  error?: string
}
