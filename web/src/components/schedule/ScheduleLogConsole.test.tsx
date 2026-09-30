import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ScheduleLogConsole, getLogLevel } from './ScheduleLogConsole'
import type { ScheduleLog } from '../../api/types'

const props = {
  status: { running: false, interval_seconds: 300 }, logFilter: 'all' as const,
  setLogFilter: vi.fn(), triggering: false, onTriggerNow: vi.fn(async () => {}), onRefreshLogs: vi.fn(),
  loading: false, error: null, triggerDisabled: false,
}
const log = (message: string): ScheduleLog => ({ time: '10:00:00', message })

describe('ScheduleLogConsole', () => {
  it('上翻后重新点击当前筛选也立即返回最新，反馈与位置一致', () => {
    render(<ScheduleLogConsole {...props} logs={[log('首批')]} />)
    const body = document.querySelector('.log-feed-body') as HTMLElement
    Object.defineProperties(body, { scrollHeight: { get: () => 1000 }, clientHeight: { get: () => 200 } })
    body.scrollTop = 100
    fireEvent.scroll(body)
    expect(screen.getByRole('button', { name: '查看最新日志' })).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '全部 (1)' }))
    expect(body.scrollTop).toBe(1000)
    expect(screen.queryByRole('button', { name: '查看最新日志' })).not.toBeInTheDocument()
  })
  it('首批日志及大批新增保持跟随，上翻阅读后不打断，可回到最新', () => {
    const view = render(<ScheduleLogConsole {...props} logs={[]} />)
    const body = document.querySelector('.log-feed-body') as HTMLElement
    let height = 1000
    Object.defineProperties(body, { scrollHeight: { get: () => height }, clientHeight: { get: () => 200 } })
    view.rerender(<ScheduleLogConsole {...props} logs={[log('首批')]} />)
    expect(body.scrollTop).toBe(1000)
    height = 2000
    view.rerender(<ScheduleLogConsole {...props} logs={[log('首批'), log('新增一批')]} />)
    expect(body.scrollTop).toBe(2000)
    body.scrollTop = 100
    fireEvent.scroll(body)
    height = 3000
    view.rerender(<ScheduleLogConsole {...props} logs={[log('首批'), log('新增一批'), log('更多')]} />)
    expect(body.scrollTop).toBe(100)
    fireEvent.click(screen.getByRole('button', { name: '查看最新日志' }))
    expect(body.scrollTop).toBe(3000)
  })

  it('日志滚动淘汰首条时复用其余 DOM，重复记录没有 key 冲突', () => {
    const a = log('a'), b = log('b'), c = log('c')
    const view = render(<ScheduleLogConsole {...props} logs={[a, b, b]} />)
    const nodes = document.querySelectorAll('.log-feed-item')
    view.rerender(<ScheduleLogConsole {...props} logs={[b, b, c]} />)
    expect(document.querySelectorAll('.log-feed-item')[0]).toBe(nodes[1])
    expect(document.querySelectorAll('.log-feed-item')[1]).toBe(nodes[2])
  })

  it('读取失败不会显示自动监听，过滤无匹配与全局空日志分开显示', () => {
    const view = render(<ScheduleLogConsole {...props} logs={[]} error="网络错误" />)
    expect(screen.getByText('读取中断')).toBeInTheDocument()
    expect(screen.queryByText('自动监听中')).not.toBeInTheDocument()
    view.rerender(<ScheduleLogConsole {...props} logs={[log('普通信息')]} logFilter="error" />)
    expect(screen.getByText('当前筛选没有匹配日志')).toBeInTheDocument()
  })

  it.each(['上游写操作结果未知', '遭遇瞬态故障', '创建返回空结果'])('异常筛选包含 %s', (message) => {
    expect(getLogLevel(message)).toBe('error')
  })
})
