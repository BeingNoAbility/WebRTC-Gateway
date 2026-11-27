//go:build unix

package state

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

var (
	localLockMu sync.Mutex
	localLocks  = map[string]*sync.Mutex{}
)

func getLocalLock(cleanPath string) *sync.Mutex {
	localLockMu.Lock()
	m, ok := localLocks[cleanPath]
	if !ok {
		m = &sync.Mutex{}
		localLocks[cleanPath] = m
	}
	localLockMu.Unlock()
	return m
}

// WithFileLock acquires an in-process mutex plus an advisory exclusive OS file lock on lockPath
// (kernel-released automatically on process crash), executes fn, and releases both locks.
func WithFileLock(lockPath string, fn func() error) error {
	if lockPath == "" {
		return fn()
	}
	clean := filepath.Clean(lockPath)
	lm := getLocalLock(clean)
	lm.Lock()
	defer lm.Unlock()

	dir := filepath.Dir(clean)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("state: create lock dir %q: %w", dir, err)
	}
	f, err := os.OpenFile(clean, os.O_CREATE|os.O_RDWR, 0600) // #nosec G304 -- operator-configured state lock file
	if err != nil {
		return fmt.Errorf("state: open lock file %q: %w", lockPath, err)
	}
	defer func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("state: flock %q: %w", lockPath, err)
	}
	return fn()
}
