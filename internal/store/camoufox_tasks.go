/**
 * [INPUT]: 依赖 database/sql、time 与 Store
 * [OUTPUT]: 对外提供 CamoufoxTask 及其持久化 CRUD
 * [POS]: Camoufox 登录任务的非敏感生命周期元数据，保证主服务重启后可以回收代理任务
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"fmt"
	"strings"
	"time"
)

// CamoufoxTask 只保存任务定位信息，不保存 Apple 密码、OTP 或 Cookie。
type CamoufoxTask struct {
	AccountID string
	TaskID    string
	BaseURL   string
	CreatedAt time.Time
}

// SaveCamoufoxTask 原子写入一个账号的进行中 Camoufox 任务。
func (s *Store) SaveCamoufoxTask(task CamoufoxTask) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store is nil")
	}
	if strings.TrimSpace(task.AccountID) == "" || strings.TrimSpace(task.TaskID) == "" || strings.TrimSpace(task.BaseURL) == "" {
		return fmt.Errorf("camoufox task metadata is incomplete")
	}
	createdAt := task.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`
		INSERT INTO camoufox_tasks (account_id, task_id, base_url, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET
			task_id = excluded.task_id,
			base_url = excluded.base_url,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at`,
		task.AccountID, task.TaskID, task.BaseURL, createdAt.UTC().Format(time.RFC3339Nano), now)
	return err
}

// DeleteCamoufoxTask 按账号和可选任务 ID 删除任务，避免旧回收操作误删新任务。
func (s *Store) DeleteCamoufoxTask(accountID, taskID string) error {
	if s == nil || s.db == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(taskID) == "" {
		_, err := s.db.Exec(`DELETE FROM camoufox_tasks WHERE account_id = ?`, accountID)
		return err
	}
	_, err := s.db.Exec(`DELETE FROM camoufox_tasks WHERE account_id = ? AND task_id = ?`, accountID, taskID)
	return err
}

// ListCamoufoxTasks 返回所有尚未清理的 Camoufox 任务元数据。
func (s *Store) ListCamoufoxTasks() ([]CamoufoxTask, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT account_id, task_id, base_url, created_at FROM camoufox_tasks ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []CamoufoxTask
	for rows.Next() {
		var task CamoufoxTask
		var createdAt string
		if err := rows.Scan(&task.AccountID, &task.TaskID, &task.BaseURL, &createdAt); err != nil {
			return nil, err
		}
		parsed, err := time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse camoufox task %s created_at: %w", task.TaskID, err)
		}
		task.CreatedAt = parsed
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tasks, nil
}
