//go:build !windows

/**
 * [INPUT]: 依赖 errors, fmt, os, path/filepath, golang.org/x/sys/unix
 * [OUTPUT]: Unix/Linux 平台下基于 unix.Flock 的真正 OS 顾问文件锁实现
 * [POS]: internal/store 的 Linux/Unix 进程生命周期与容灾排他锁 (PR-06 Blocker 1)
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type unixFileLock struct {
	file *os.File
	path string
}

func acquireDataDirLock(dataDir string) (dataDirLock, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data directory failed: %w", err)
	}
	lockPath := filepath.Join(dataDir, ".icloud-hme.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock file failed: %w", err)
	}

	// Try non-blocking exclusive flock
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrDatabaseInUse
		}
		return nil, fmt.Errorf("lock file failed: %w", err)
	}

	return &unixFileLock{file: f, path: lockPath}, nil
}

func (l *unixFileLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}
