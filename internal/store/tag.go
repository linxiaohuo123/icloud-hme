/**
 * [INPUT]: 依赖 time, log, database/sql, icloud-hme/internal/store (Store, NewBusinessTagID)
 * [OUTPUT]: 对外提供 BusinessTag 类型, NewBusinessTagID, ListTags, SaveTag, DeleteTag, UpdateTagLastAssigned
 * [POS]: internal/store 的业务标识/标签持久化领域，支持按标签分组管理已用别名与配额
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"log"
	"time"
)

// BusinessTag 业务标识
type BusinessTag struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Tag            string `json:"tag"`
	Description    string `json:"description"`
	Status         string `json:"status"` // "active" | "disabled"
	CreatedAt      string `json:"created_at"`
	LastAssignedAt string `json:"last_assigned_at,omitempty"`
}

// NewBusinessTagID 生成业务标识主键。
func NewBusinessTagID() string { return newOpaqueID("tag_") }

// ────────────────────────────────────────────────────────────────
// Business Tags
// ────────────────────────────────────────────────────────────────

func (s *Store) ListTags() []BusinessTag {
	rows, err := s.db.Query(`SELECT id, name, tag, description, status, created_at, COALESCE(last_assigned_at, '') FROM business_tags ORDER BY created_at DESC`)
	if err != nil {
		return []BusinessTag{}
	}
	defer rows.Close()

	res := make([]BusinessTag, 0)
	for rows.Next() {
		var t BusinessTag
		if err := rows.Scan(&t.ID, &t.Name, &t.Tag, &t.Description, &t.Status, &t.CreatedAt, &t.LastAssignedAt); err == nil {
			res = append(res, t)
		}
	}
	// 【BUG-05 修复】迭代中断时记录日志,防止底层 IO 错误导致结果静默截断
	if err := rows.Err(); err != nil {
		log.Printf("[Store] ListTags 迭代中断: %v", err)
	}
	return res
}

func (s *Store) SaveTag(tag BusinessTag) error {
	if tag.ID == "" {
		tag.ID = NewBusinessTagID()
	}
	if tag.CreatedAt == "" {
		tag.CreatedAt = time.Now().Format(time.RFC3339)
	}
	if tag.Status == "" {
		tag.Status = "active"
	}
	query := `
	INSERT INTO business_tags (id, name, tag, description, status, created_at, last_assigned_at)
	VALUES (?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		name = excluded.name,
		tag = excluded.tag,
		description = excluded.description,
		status = excluded.status,
		last_assigned_at = CASE WHEN excluded.last_assigned_at != '' THEN excluded.last_assigned_at ELSE business_tags.last_assigned_at END;
	`
	_, err := s.db.Exec(query, tag.ID, tag.Name, tag.Tag, tag.Description, tag.Status, tag.CreatedAt, tag.LastAssignedAt)
	return err
}

// DeleteTag 删除业务标签。返回 true 表示实际删除了记录，false 表示本就不存在。
func (s *Store) DeleteTag(id string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM business_tags WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) UpdateTagLastAssigned(tagStr string) {
	now := time.Now().Format(time.RFC3339)
	_, _ = s.db.Exec(`UPDATE business_tags SET last_assigned_at = ? WHERE LOWER(tag) = LOWER(?)`, now, tagStr)
}
