/**
 * [INPUT]: 无外部依赖，接收 ISO 8601 字符串、秒/毫秒/微秒数字时间戳或数字字符串
 * [OUTPUT]: 对外提供 parseDate, formatDate, formatFullDate, formatRelativeTime, dateTimestamp, isWithinWindow 日期时间归一化与格式化工具函数
 * [POS]: web/src/utils 的时间处理基础设施，被 AccountWorkspace, AliasesPage, AccountsPage, UsedAliasesPage 消费
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

/**
 * 健壮解析各类日期时间输入：
 * 1. ISO 8601 / RFC3339 字符串 (如 "2026-09-10T00:41:59Z")
 * 2. 纯数字或数字字符串时间戳（自动按量级识别秒、毫秒、微秒、纳秒）
 * 3. 容错非标准日期，解析失败统一返回 null
 */
export function parseDate(raw?: string | number | null): Date | null {
  if (raw === undefined || raw === null) return null
  const value = typeof raw === 'number' ? String(raw) : raw.trim()
  if (!value) return null

  // 纯数字时间戳匹配（正负整数或浮点）
  if (/^[+-]?\d+(?:\.\d+)?$/.test(value)) {
    const numeric = Number(value)
    if (Number.isFinite(numeric)) {
      const magnitude = Math.abs(numeric)
      const milliseconds =
        magnitude < 1e11
          ? numeric * 1000 // 秒
          : magnitude < 1e14
            ? numeric // 毫秒
            : magnitude < 1e17
              ? numeric / 1000 // 微秒
              : numeric / 1e6 // 纳秒
      const timestampDate = new Date(milliseconds)
      if (!Number.isNaN(timestampDate.getTime())) return timestampDate
    }
  }

  const normalized = value.includes(' ') && !value.includes('T') ? value.replace(' ', 'T') : value
  let date = new Date(normalized)
  if (Number.isNaN(date.getTime())) {
    date = new Date(value.replace(/-/g, '/'))
  }
  return Number.isNaN(date.getTime()) ? null : date
}

const pad = (n: number) => String(n).padStart(2, '0')

/**
 * 格式化为常用展示时间：YYYY/MM/DD HH:mm (本地时区)
 */
export function formatDate(raw?: string | number | null, fallback = '—'): string {
  const d = parseDate(raw)
  if (!d) return typeof raw === 'string' && raw.trim() ? raw.trim() : fallback
  return `${d.getFullYear()}/${pad(d.getMonth() + 1)}/${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

/**
 * 格式化为完整时间（含秒）：YYYY-MM-DD HH:mm:ss (本地时区，常用于 title / tooltip)
 */
export function formatFullDate(raw?: string | number | null, fallback = '—'): string {
  const d = parseDate(raw)
  if (!d) return typeof raw === 'string' && raw.trim() ? raw.trim() : fallback
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}

/**
 * 格式化为相对/人性化时间：刚刚 / X分钟前 / 今天 HH:mm / 昨天 HH:mm / MM-DD HH:mm / YYYY/MM/DD
 */
export function formatRelativeTime(raw?: string | number | null, fallback = '—'): string {
  const d = parseDate(raw)
  if (!d) return typeof raw === 'string' && raw.trim() ? raw.trim() : fallback

  const now = new Date()
  const diffMs = now.getTime() - d.getTime()

  if (diffMs >= 0 && diffMs < 60_000) {
    return '刚刚'
  }
  if (diffMs >= 0 && diffMs < 3_600_000) {
    return `${Math.floor(diffMs / 60_000)}分钟前`
  }

  const isSameDay =
    d.getFullYear() === now.getFullYear() &&
    d.getMonth() === now.getMonth() &&
    d.getDate() === now.getDate()

  const timeStr = `${pad(d.getHours())}:${pad(d.getMinutes())}`
  if (isSameDay) {
    return `今天 ${timeStr}`
  }

  const yesterday = new Date(now)
  yesterday.setDate(now.getDate() - 1)
  const isYesterday =
    d.getFullYear() === yesterday.getFullYear() &&
    d.getMonth() === yesterday.getMonth() &&
    d.getDate() === yesterday.getDate()

  if (isYesterday) {
    return `昨天 ${timeStr}`
  }

  if (d.getFullYear() === now.getFullYear()) {
    return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${timeStr}`
  }

  return `${d.getFullYear()}/${pad(d.getMonth() + 1)}/${pad(d.getDate())}`
}

/**
 * 提取时间戳毫秒数，用于列表排序；非法日期返回 null
 */
export function dateTimestamp(raw?: string | number | null): number | null {
  return parseDate(raw)?.getTime() ?? null
}

/**
 * 判断时间是否落在距当前的 windowMs 毫秒窗口内（如 24h 活跃判定）；
 * 将 Date.now 隔离在工具函数内，保证调用方渲染纯净
 */
export function isWithinWindow(raw?: string | number | null, windowMs = 0): boolean {
  const ts = parseDate(raw)
  if (!ts) return false
  const diff = Date.now() - ts.getTime()
  return diff >= 0 && diff < windowMs
}
