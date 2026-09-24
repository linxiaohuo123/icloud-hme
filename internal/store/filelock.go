/**
 * [INPUT]: 依赖 errors, io
 * [OUTPUT]: 对外提供 ErrDatabaseInUse 哨兵错误与 dataDirLock 接口契约
 * [POS]: internal/store 的进程级目录独占排他锁抽象 (PR-06 Blocker 1)
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"errors"
	"io"
)

var (
	// ErrDatabaseInUse 数据库正被其他运行中的 icloud-hme 进程持有
	ErrDatabaseInUse = errors.New("database is in use; stop icloud-hme before restore")
)

type dataDirLock interface {
	io.Closer
}
