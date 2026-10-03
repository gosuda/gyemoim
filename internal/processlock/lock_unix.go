//go:build linux || darwin

// Package processlock keeps one Gyemoim process active per data directory.
package processlock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Lock owns the open lock file and its advisory process lock.
type Lock struct {
	file *os.File
}

// Acquire opens the persistent lock file and obtains a non-blocking exclusive lock.
func Acquire(dataDir string) (*Lock, error) {
	path := filepath.Join(dataDir, "gyemoim.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open process lock %q: %w", path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("set process lock permissions on %q: %w", path, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("another Gyemoim process is already using data directory %q", dataDir)
		}
		return nil, fmt.Errorf("lock data directory %q: %w", dataDir, err)
	}
	return &Lock{file: file}, nil
}

// Release relinquishes the lock and closes the file. The lock file itself stays in place.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return fmt.Errorf("unlock Gyemoim data directory: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close Gyemoim process lock: %w", closeErr)
	}
	return nil
}
