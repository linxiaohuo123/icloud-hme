import { describe, expect, it } from 'vitest'
import { dateTimestamp, formatDate, formatFullDate, formatRelativeTime, parseDate } from './date'

describe('date utility', () => {
  it('正确解析并格式化 ISO 字符串', () => {
    const iso = '2026-09-10T00:41:59.081Z'
    const d = parseDate(iso)
    expect(d).not.toBeNull()
    expect(dateTimestamp(iso)).toBe(new Date(iso).getTime())
    expect(formatDate(iso)).toMatch(/^\d{4}\/\d{2}\/\d{2} \d{2}:\d{2}$/)
    expect(formatFullDate(iso)).toMatch(/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$/)
  })

  it('正确解析并格式化 13 位毫秒时间戳 (数字与字符串)', () => {
    const msNum = 1789000919081
    const msStr = '1789000919081'
    expect(parseDate(msNum)?.getTime()).toBe(msNum)
    expect(parseDate(msStr)?.getTime()).toBe(msNum)
    expect(dateTimestamp(msStr)).toBe(msNum)
    expect(formatDate(msStr)).toMatch(/^\d{4}\/\d{2}\/\d{2} \d{2}:\d{2}$/)
  })

  it('正确解析 10 位秒级时间戳', () => {
    const sec = 1789000919
    expect(parseDate(sec)?.getTime()).toBe(sec * 1000)
    expect(parseDate(String(sec))?.getTime()).toBe(sec * 1000)
  })

  it('正确解析浮点格式时间戳与空格分隔时间', () => {
    const floatStr = '1789000919081.0'
    expect(parseDate(floatStr)?.getTime()).toBe(1789000919081)

    const spaceStr = '2026-09-10 08:41:59'
    expect(parseDate(spaceStr)).not.toBeNull()
    expect(formatDate(spaceStr)).toBe('2026/09/10 08:41')
  })

  it('正确格式化人性化相对时间', () => {
    const now = Date.now()
    expect(formatRelativeTime(now - 10_000)).toBe('刚刚')
    expect(formatRelativeTime(now - 15 * 60_000)).toBe('15分钟前')
    expect(formatRelativeTime(null)).toBe('—')
  })

  it('处理空值与非法值', () => {
    expect(parseDate(null)).toBeNull()
    expect(parseDate(undefined)).toBeNull()
    expect(parseDate('')).toBeNull()
    expect(formatDate(null)).toBe('—')
    expect(formatDate(undefined, '从未')).toBe('从未')
    expect(dateTimestamp('invalid')).toBeNull()
  })
})
