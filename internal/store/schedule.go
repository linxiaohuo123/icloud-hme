/**
 * [INPUT]: 依赖 log, strings, time, icloud-hme/internal/store (Store)
 * [OUTPUT]: 对外提供 ScheduleConfig 类型, GetScheduleConfig, ListScheduleConfigs, SaveScheduleConfig, DeleteScheduleConfig, TryReserveQuota, ReleaseQuota, RemainingQuota, IncrementHourlyQuota, GetSetting, SaveSetting
 * [POS]: internal/store 的定时任务与配额调度持久化领域，支持小时级限流与系统 KV 设置
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"log"
	"strings"
	"time"
)

// ScheduleConfig 账号定时补货配置
type ScheduleConfig struct {
	AccountID        string `json:"account_id"`
	Enabled          bool   `json:"enabled"`
	HourlyQuota      int    `json:"hourly_quota"`
	AliasLabel       string `json:"alias_label"`
	CurrentHourCount int    `json:"current_hour_count"`
	LastHourWindow   int64  `json:"last_hour_window"`
	LastRunAt        string `json:"last_run_at,omitempty"`
	Mode             string `json:"mode,omitempty"`           // "always" | "daily_window" | "duration"
	StartTime        string `json:"start_time,omitempty"`     // "09:00"
	EndTime          string `json:"end_time,omitempty"`       // "18:00"
	DurationHours    int    `json:"duration_hours,omitempty"` // 持续运行时长(小时)
	StartedAt        string `json:"started_at,omitempty"`     // 启动时间 (RFC3339)
}

// ────────────────────────────────────────────────────────────────
// 定时配置 Schedules
// ────────────────────────────────────────────────────────────────

func (s *Store) GetScheduleConfig(accountID string) ScheduleConfig {
	var cfg ScheduleConfig
	var enabledInt int
	query := `SELECT account_id, enabled, hourly_quota, COALESCE(alias_label, 'scheduled'), current_hour_count, last_hour_window, COALESCE(last_run_at, ''), COALESCE(mode, 'always'), COALESCE(start_time, ''), COALESCE(end_time, ''), COALESCE(duration_hours, 0), COALESCE(started_at, '') FROM schedules WHERE account_id = ?`
	err := s.db.QueryRow(query, accountID).Scan(&cfg.AccountID, &enabledInt, &cfg.HourlyQuota, &cfg.AliasLabel, &cfg.CurrentHourCount, &cfg.LastHourWindow, &cfg.LastRunAt, &cfg.Mode, &cfg.StartTime, &cfg.EndTime, &cfg.DurationHours, &cfg.StartedAt)
	if err != nil {
		return ScheduleConfig{
			AccountID:   accountID,
			Enabled:     false,
			HourlyQuota: 5,
			AliasLabel:  "scheduled",
			Mode:        "always",
		}
	}
	cfg.Enabled = (enabledInt == 1)
	if strings.TrimSpace(cfg.AliasLabel) == "" {
		cfg.AliasLabel = "scheduled"
	}
	if strings.TrimSpace(cfg.Mode) == "" {
		cfg.Mode = "always"
	}
	currentHour := time.Now().Unix() / 3600
	if cfg.LastHourWindow != currentHour {
		cfg.CurrentHourCount = 0
	}
	return cfg
}

func (s *Store) ListScheduleConfigs() []ScheduleConfig {
	rows, err := s.db.Query(`SELECT account_id, enabled, hourly_quota, COALESCE(alias_label, 'scheduled'), current_hour_count, last_hour_window, COALESCE(last_run_at, ''), COALESCE(mode, 'always'), COALESCE(start_time, ''), COALESCE(end_time, ''), COALESCE(duration_hours, 0), COALESCE(started_at, '') FROM schedules`)
	if err != nil {
		return []ScheduleConfig{}
	}
	defer rows.Close()

	currentHour := time.Now().Unix() / 3600
	res := make([]ScheduleConfig, 0)
	for rows.Next() {
		var cfg ScheduleConfig
		var enabledInt int
		if err := rows.Scan(&cfg.AccountID, &enabledInt, &cfg.HourlyQuota, &cfg.AliasLabel, &cfg.CurrentHourCount, &cfg.LastHourWindow, &cfg.LastRunAt, &cfg.Mode, &cfg.StartTime, &cfg.EndTime, &cfg.DurationHours, &cfg.StartedAt); err == nil {
			cfg.Enabled = (enabledInt == 1)
			if strings.TrimSpace(cfg.AliasLabel) == "" {
				cfg.AliasLabel = "scheduled"
			}
			if strings.TrimSpace(cfg.Mode) == "" {
				cfg.Mode = "always"
			}
			if cfg.LastHourWindow != currentHour {
				cfg.CurrentHourCount = 0
			}
			res = append(res, cfg)
		}
	}
	// 【BUG-05 修复】迭代中断时记录日志
	if err := rows.Err(); err != nil {
		log.Printf("[Store] ListScheduleConfigs 迭代中断: %v", err)
	}
	return res
}

// SaveScheduleConfig 保存调度配置(线程安全)。
// 与 TryReserveQuota 共用同一把锁，避免"读-改-写"把已扣减的小时计数回退成旧值。
func (s *Store) SaveScheduleConfig(cfg ScheduleConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveScheduleConfigLocked(cfg)
}

// saveScheduleConfigLocked 是 SaveScheduleConfig 的加锁内核，调用方必须持有 s.mu。
func (s *Store) saveScheduleConfigLocked(cfg ScheduleConfig) error {
	if cfg.HourlyQuota <= 0 {
		cfg.HourlyQuota = 5
	}
	if strings.TrimSpace(cfg.AliasLabel) == "" {
		cfg.AliasLabel = "scheduled"
	}
	if strings.TrimSpace(cfg.Mode) == "" {
		cfg.Mode = "always"
	}
	if cfg.Mode == "duration" && cfg.Enabled && strings.TrimSpace(cfg.StartedAt) == "" {
		cfg.StartedAt = time.Now().Format(time.RFC3339)
	}
	enabledInt := 0
	if cfg.Enabled {
		enabledInt = 1
	}
	query := `
	INSERT INTO schedules (account_id, enabled, hourly_quota, alias_label, current_hour_count, last_hour_window, last_run_at, mode, start_time, end_time, duration_hours, started_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(account_id) DO UPDATE SET
		enabled = excluded.enabled,
		hourly_quota = excluded.hourly_quota,
		alias_label = excluded.alias_label,
		current_hour_count = CASE WHEN excluded.last_hour_window > 0 THEN excluded.current_hour_count ELSE schedules.current_hour_count END,
		last_hour_window = CASE WHEN excluded.last_hour_window > 0 THEN excluded.last_hour_window ELSE schedules.last_hour_window END,
		last_run_at = CASE WHEN excluded.last_run_at != '' THEN excluded.last_run_at ELSE schedules.last_run_at END,
		mode = excluded.mode,
		start_time = excluded.start_time,
		end_time = excluded.end_time,
		duration_hours = excluded.duration_hours,
		started_at = CASE WHEN excluded.mode != 'duration' THEN '' WHEN excluded.started_at != '' THEN excluded.started_at ELSE schedules.started_at END;
	`
	_, err := s.db.Exec(query, cfg.AccountID, enabledInt, cfg.HourlyQuota, cfg.AliasLabel, cfg.CurrentHourCount, cfg.LastHourWindow, cfg.LastRunAt, cfg.Mode, cfg.StartTime, cfg.EndTime, cfg.DurationHours, cfg.StartedAt)
	return err
}

// DeleteScheduleConfig 物理删除指定账号的定时调度配置，杜绝幽灵记录残留。
func (s *Store) DeleteScheduleConfig(accountID string) error {
	_, err := s.db.Exec(`DELETE FROM schedules WHERE account_id = ?`, accountID)
	return err
}

// TryReserveQuota 尝试原子预留指定数量的配额（单次/批量/调度统一入口）
func (s *Store) TryReserveQuota(accountID string, count int) (allowed bool, remaining int) {
	if count <= 0 {
		return true, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg := s.GetScheduleConfig(accountID)
	currentHour := time.Now().Unix() / 3600
	if cfg.LastHourWindow != currentHour {
		cfg.LastHourWindow = currentHour
		cfg.CurrentHourCount = 0
	}

	rem := cfg.HourlyQuota - cfg.CurrentHourCount
	if rem < count {
		if rem < 0 {
			rem = 0
		}
		return false, rem
	}

	cfg.CurrentHourCount += count
	cfg.LastRunAt = time.Now().Format(time.RFC3339)
	if err := s.saveScheduleConfigLocked(cfg); err != nil {
		// 落库失败则回滚内存计数，拒绝本次预留，避免重启后配额归零超额出号
		cfg.CurrentHourCount -= count
		return false, cfg.HourlyQuota - cfg.CurrentHourCount
	}
	return true, cfg.HourlyQuota - cfg.CurrentHourCount
}

// ReleaseQuota 当别名创建失败时回滚配额
func (s *Store) ReleaseQuota(accountID string, count int) {
	if count <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg := s.GetScheduleConfig(accountID)
	currentHour := time.Now().Unix() / 3600
	if cfg.LastHourWindow == currentHour {
		cfg.CurrentHourCount -= count
		if cfg.CurrentHourCount < 0 {
			cfg.CurrentHourCount = 0
		}
		_ = s.saveScheduleConfigLocked(cfg)
	}
}

// RemainingQuota 查询指定账号当前小时的剩余创建配额
func (s *Store) RemainingQuota(accountID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg := s.GetScheduleConfig(accountID)
	currentHour := time.Now().Unix() / 3600
	if cfg.LastHourWindow != currentHour {
		return cfg.HourlyQuota
	}
	rem := cfg.HourlyQuota - cfg.CurrentHourCount
	if rem < 0 {
		return 0
	}
	return rem
}

func (s *Store) IncrementHourlyQuota(accountID string) (allowed bool, current int) {
	ok, _ := s.TryReserveQuota(accountID, 1)
	cfg := s.GetScheduleConfig(accountID)
	return ok, cfg.CurrentHourCount
}

// GetSetting 读取系统级 KV 设置,不存在返回空串。
func (s *Store) GetSetting(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var value string
	_ = s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	return value
}

// SaveSetting 写入系统级 KV 设置 (upsert)。
func (s *Store) SaveSetting(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().Format(time.RFC3339),
	)
	return err
}
