/**
 * [INPUT]: 依赖 database/sql, errors, time 与 Store 的 SQLite 连接
 * [OUTPUT]: 对外提供 RevokeSession, IsSessionRevoked
 * [POS]: internal/store 的管理员会话撤销持久化，防止重启后恢复已退出会话
 */

package store

import (
	"database/sql"
	"errors"
	"time"
)

// RevokeSession persists a session hash before it is removed from memory.
func (s *Store) RevokeSession(hash [32]byte, expiresAt time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM revoked_sessions WHERE expires_at <= ?`, time.Now().Unix()); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO revoked_sessions (session_hash, expires_at) VALUES (?, ?)
		ON CONFLICT(session_hash) DO UPDATE SET expires_at = excluded.expires_at`, hash[:], expiresAt.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// IsSessionRevoked is checked before a signed session is restored after restart.
func (s *Store) IsSessionRevoked(hash [32]byte) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM revoked_sessions WHERE session_hash = ?`, hash[:]).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
