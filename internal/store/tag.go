/**
 * [INPUT]: 依赖 time, database/sql, encoding/json, errors, strings
 * [OUTPUT]: 对外提供 BusinessTag 类型, NewBusinessTagID, ListTags, CreateTag, UpdateTag, DeleteTag, UpdateTagLastAssigned
 * [POS]: internal/store 的业务标识/标签持久化领域，支持按标签分组管理已用别名与配额
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
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

func (s *Store) ListTags() ([]BusinessTag, error) {
	rows, err := s.db.Query(`SELECT id, name, tag, description, status, created_at, COALESCE(last_assigned_at, '') FROM business_tags ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := make([]BusinessTag, 0)
	for rows.Next() {
		var t BusinessTag
		if err := rows.Scan(&t.ID, &t.Name, &t.Tag, &t.Description, &t.Status, &t.CreatedAt, &t.LastAssignedAt); err != nil {
			return nil, err
		}
		res = append(res, t)
	}
	return res, rows.Err()
}

var (
	ErrTagExists   = errors.New("业务标识已存在")
	ErrTagNotFound = errors.New("业务标识不存在")
	ErrTagInUse    = errors.New("业务标识仍被母号引用，请先调整母号标签后再修改标识")
)

// CreateTag inserts only; an existing ID must never overwrite another tag.
func (s *Store) CreateTag(tag BusinessTag) error {
	if tag.ID == "" {
		tag.ID = NewBusinessTagID()
	}
	if tag.CreatedAt == "" {
		tag.CreatedAt = time.Now().Format(time.RFC3339)
	}
	if tag.Status == "" {
		tag.Status = "active"
	}
	tag.Tag = strings.TrimSpace(tag.Tag)
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.beginTagMutation()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkTagUnique(tx, tag.Tag, ""); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO business_tags (id, name, tag, description, status, created_at, last_assigned_at)
        VALUES (?, ?, ?, ?, ?, ?, ?)`, tag.ID, tag.Name, tag.Tag, tag.Description, tag.Status, tag.CreatedAt, tag.LastAssignedAt)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateTag reads and patches in one transaction, preserving assignment history.
// A referenced business key is immutable until its account references are removed.
func (s *Store) UpdateTag(id string, update func(*BusinessTag)) (BusinessTag, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.beginTagMutation()
	if err != nil {
		return BusinessTag{}, err
	}
	defer tx.Rollback()
	var tag BusinessTag
	err = tx.QueryRow(`SELECT id, name, tag, description, status, created_at, COALESCE(last_assigned_at, '')
        FROM business_tags WHERE id = ?`, id).Scan(&tag.ID, &tag.Name, &tag.Tag, &tag.Description, &tag.Status, &tag.CreatedAt, &tag.LastAssignedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return BusinessTag{}, ErrTagNotFound
	}
	if err != nil {
		return BusinessTag{}, err
	}
	oldKey := tag.Tag
	update(&tag)
	tag.Tag = strings.TrimSpace(tag.Tag)
	if err := checkTagUnique(tx, tag.Tag, id); err != nil {
		return BusinessTag{}, err
	}
	if !strings.EqualFold(oldKey, tag.Tag) {
		if err := checkTagUnreferenced(tx, oldKey); err != nil {
			return BusinessTag{}, err
		}
	}
	_, err = tx.Exec(`UPDATE business_tags SET name = ?, tag = ?, description = ?, status = ? WHERE id = ?`,
		tag.Name, tag.Tag, tag.Description, tag.Status, id)
	if err != nil {
		return BusinessTag{}, err
	}
	if err := tx.Commit(); err != nil {
		return BusinessTag{}, err
	}
	return tag, nil
}

// Take SQLite's write lock before reading. Account/token writers use other
// connections; a deferred read transaction cannot safely upgrade after their writes.
func (s *Store) beginTagMutation() (*sql.Tx, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE business_tags SET tag = tag WHERE 0`); err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func checkTagUnique(tx *sql.Tx, key, excludeID string) error {
	rows, err := tx.Query(`SELECT id, tag FROM business_tags`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, existing string
		if err := rows.Scan(&id, &existing); err != nil {
			return err
		}
		if id != excludeID && strings.EqualFold(strings.TrimSpace(existing), key) {
			return ErrTagExists
		}
	}
	return rows.Err()
}

func checkTagUnreferenced(tx *sql.Tx, key string) error {
	rows, err := tx.Query(`SELECT tags FROM accounts`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var raw sql.NullString
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		if !raw.Valid || strings.TrimSpace(raw.String) == "" {
			continue
		}
		var tags []string
		if err := json.Unmarshal([]byte(raw.String), &tags); err != nil {
			return errors.New("母号标签数据损坏，无法确认业务标识引用")
		}
		for _, tag := range tags {
			if strings.EqualFold(strings.TrimSpace(tag), strings.TrimSpace(key)) {
				return ErrTagInUse
			}
		}
	}
	return rows.Err()
}

// DeleteTag 删除业务标签。返回 true 表示实际删除了记录，false 表示本就不存在。
func (s *Store) DeleteTag(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
