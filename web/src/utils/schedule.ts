/**
 * [INPUT]: api/types 的 ScheduleConfig 与当前毫秒时间
 * [OUTPUT]: isScheduleExpired，统一持续时长到期的界面判定
 * [POS]: web/src/utils 的调度展示规则，与后端 duration 时间闸门保持一致
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */
import type { ScheduleConfig } from '../api/types'

export function isScheduleExpired(cfg: ScheduleConfig | undefined, now: number): boolean {
  if (!cfg || cfg.mode !== 'duration' || !cfg.started_at || !cfg.duration_hours || cfg.duration_hours <= 0) return false
  const started = Date.parse(cfg.started_at)
  return Number.isFinite(started) && now > started + cfg.duration_hours * 3_600_000
}
