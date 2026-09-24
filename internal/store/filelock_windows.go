//go:build windows

/**
 * [INPUT]: 依赖 errors, fmt, os, path/filepath, golang.org/x/sys/windows
 * [OUTPUT]: Windows 平台下基于 LockFileEx 的真正 OS 顾问文件锁实现
 * [POS]: internal/store 的 Windows 进程生命周期与容灾排他锁 (PR-06 Blocker 1)
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

type windowsFileLock struct {
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

	var ov windows.Overlapped
	// Lock 1 byte at offset 0 exclusively, fail immediately if locked
	err = windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&ov,
	)
	if err != nil {
		_ = f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, syscall.Errno(33)) {
			return nil, ErrDatabaseInUse
		}
		return nil, fmt.Errorf("lock file failed: %w", err)
	}

	return &windowsFileLock{file: f, path: lockPath}, nil
}

func (l *windowsFileLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	var ov windows.Overlapped
	_ = windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, &ov)
	err := l.file.Close()
	l.file = nil
	return err
}
