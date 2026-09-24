/**
 * [INPUT]: 依赖 encoding/json, fmt, log, os, path/filepath, time
 * [OUTPUT]: 对外提供 Store.migrateLegacyJSON 方法，负责 tags/tokens/leases/schedules 遗留 JSON 文件的事务级自动无损迁移 (Fail-Closed)
 * [POS]: internal/store 的遗留兼容迁移层
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// migrateLegacyJSON 把遗留 JSON 存储迁移进 SQLite。
//
// migrateLegacyJSON 把遗留 JSON 存储迁移进 SQLite (Fail-Closed: 任何错误立即中断并返回 error)。
//
// 【数据安全红线】迁移必须无损且可重试：
//   - 任一字段都不允许丢弃(列清单必须与当前 schema 对齐)；
//   - 解析失败或逐条写入失败时，绝不归档源文件，保留现场供下次启动重试或人工修复；
//   - 归档文件名带时间戳，避免后续启动的清理动作覆盖掉人工备份。
func (s *Store) migrateLegacyJSON() error {
	if err := s.migrateTagsFile(); err != nil {
		return err
	}
	if err := s.migrateTokensFile(); err != nil {
		return err
	}
	if err := s.migrateLeasesFile(); err != nil {
		return err
	}
	if err := s.migrateSchedulesFile(); err != nil {
		return err
	}
	return nil
}

func (s *Store) migrateTagsFile() error {
	file := filepath.Join(s.dataDir, "tags.json")
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 文件不存在: 无需迁移
		}
		return fmt.Errorf("read %s failed: %w", file, err)
	}
	var tags map[string]*BusinessTag
	if err := json.Unmarshal(data, &tags); err != nil {
		s.keepLegacyFile(file, "解析失败", err)
		return fmt.Errorf("parse %s failed: %w", file, err)
	}
	if len(tags) == 0 {
		s.archiveLegacyFile(file, true)
		return nil
	}
	if err := s.insertLegacyTags(tags); err != nil {
		s.keepLegacyFile(file, "迁移失败", err)
		return fmt.Errorf("migrate %s into db failed: %w", file, err)
	}
	s.archiveLegacyFile(file, false)
	return nil
}

func (s *Store) insertLegacyTags(tags map[string]*BusinessTag) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO business_tags (id, name, tag, description, status, created_at, last_assigned_at) VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, t := range tags {
		if t == nil {
			continue
		}
		if _, err := stmt.Exec(t.ID, t.Name, t.Tag, t.Description, t.Status, t.CreatedAt, t.LastAssignedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) migrateTokensFile() error {
	file := filepath.Join(s.dataDir, "tokens.json")
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s failed: %w", file, err)
	}
	var tokens map[string]*APIToken
	if err := json.Unmarshal(data, &tokens); err != nil {
		s.keepLegacyFile(file, "解析失败", err)
		return fmt.Errorf("parse %s failed: %w", file, err)
	}
	if len(tokens) == 0 {
		s.archiveLegacyFile(file, true)
		return nil
	}
	if err := s.insertLegacyTokens(tokens); err != nil {
		s.keepLegacyFile(file, "迁移失败", err)
		return fmt.Errorf("migrate %s into db failed: %w", file, err)
	}
	s.archiveLegacyFile(file, false)
	return nil
}

func (s *Store) insertLegacyTokens(tokens map[string]*APIToken) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO api_tokens (id, name, token, created_at, last_used_at, scopes) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, tok := range tokens {
		if tok == nil {
			continue
		}
		// 历史令牌保持原有(管理员级)能力，避免升级后打断既有外部接入
		scopes := tok.Scopes
		if scopes == "" {
			scopes = ScopeAdmin
		}
		if _, err := stmt.Exec(tok.ID, tok.Name, tok.Token, tok.CreatedAt, tok.LastUsedAt, scopes); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) migrateLeasesFile() error {
	file := filepath.Join(s.dataDir, "leases.json")
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s failed: %w", file, err)
	}
	var leases []*LeaseRecord
	if err := json.Unmarshal(data, &leases); err != nil {
		s.keepLegacyFile(file, "解析失败", err)
		return fmt.Errorf("parse %s failed: %w", file, err)
	}
	if len(leases) == 0 {
		s.archiveLegacyFile(file, true)
		return nil
	}
	if err := s.insertLegacyLeases(leases); err != nil {
		s.keepLegacyFile(file, "迁移失败", err)
		return fmt.Errorf("migrate %s into db failed: %w", file, err)
	}
	s.archiveLegacyFile(file, false)
	return nil
}

func (s *Store) insertLegacyLeases(leases []*LeaseRecord) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// token_name 必须一并迁移，否则历史流水的业务归属与审计链会断裂
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO lease_records (id, email, account_id, tag, status, allocated_at, completed_at, token_name) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, l := range leases {
		if l == nil {
			continue
		}
		if _, err := stmt.Exec(l.ID, l.Email, l.AccountID, l.Tag, l.Status, l.AllocatedAt, l.CompletedAt, l.TokenName); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) migrateSchedulesFile() error {
	file := filepath.Join(s.dataDir, "schedules.json")
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s failed: %w", file, err)
	}
	var schedules map[string]*ScheduleConfig
	if err := json.Unmarshal(data, &schedules); err != nil {
		s.keepLegacyFile(file, "解析失败", err)
		return fmt.Errorf("parse %s failed: %w", file, err)
	}
	if len(schedules) == 0 {
		s.archiveLegacyFile(file, true)
		return nil
	}
	if err := s.insertLegacySchedules(schedules); err != nil {
		s.keepLegacyFile(file, "迁移失败", err)
		return fmt.Errorf("migrate %s into db failed: %w", file, err)
	}
	s.archiveLegacyFile(file, false)
	return nil
}

func (s *Store) insertLegacySchedules(schedules map[string]*ScheduleConfig) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// 运行模式/时间窗口/持续时长必须一并迁移:只写 6 列会让 daily_window 退化成
	// always，把"每天 9-18 点补货"变成 7x24 全天发号，直接放大 Apple 风控暴露。
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO schedules
		(account_id, enabled, hourly_quota, alias_label, current_hour_count, last_hour_window,
		 last_run_at, mode, start_time, end_time, duration_hours, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, sc := range schedules {
		if sc == nil {
			continue
		}
		enabledInt := 0
		if sc.Enabled {
			enabledInt = 1
		}
		label := sc.AliasLabel
		if label == "" {
			label = "scheduled"
		}
		mode := sc.Mode
		if mode == "" {
			mode = "always"
		}
		quota := sc.HourlyQuota
		if quota <= 0 {
			quota = 5
		}
		if _, err := stmt.Exec(
			sc.AccountID, enabledInt, quota, label, sc.CurrentHourCount, sc.LastHourWindow,
			sc.LastRunAt, mode, sc.StartTime, sc.EndTime, sc.DurationHours, sc.StartedAt,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// keepLegacyFile 在迁移失败时保留源文件并告警，绝不归档。
func (s *Store) keepLegacyFile(file, reason string, err error) {
	log.Printf("[Store][MIGRATE] %s %s，已保留原文件以便下次启动重试或人工修复: %v", file, reason, err)
}

// archiveLegacyFile 归档已成功迁移(或确认为空)的遗留文件。
// empty 为 true 时用固定后缀(无内容可丢)；否则附加时间戳，避免覆盖历史备份。
func (s *Store) archiveLegacyFile(file string, empty bool) {
	dst := file + "." + time.Now().Format("20060102T150405") + ".migrated"
	if empty {
		dst = file + ".migrated"
	}
	_ = os.Remove(dst)
	if err := os.Rename(file, dst); err != nil {
		log.Printf("[Store][MIGRATE] 归档 %s 失败: %v", file, err)
		return
	}
	_ = os.Remove(file + ".bak")
	_ = os.Remove(file + ".tmp")
}
