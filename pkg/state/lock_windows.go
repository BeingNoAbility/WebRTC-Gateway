//go:build windows

package state

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"
)

var (
	modKernel32      = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = modKernel32.NewProc("LockFileEx")
	procUnlockFileEx = modKernel32.NewProc("UnlockFileEx")
	localLockMu      sync.Mutex
	localLocks       = map[string]*sync.Mutex{}
)

const lockfileExclusiveLock = 0x00000002

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

// WithFileLock acquires an in-process mutex plus an exclusive OS file lock on lockPath via LockFileEx, executes fn, and unlocks.
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
	defer f.Close()

	var ol syscall.Overlapped
	r1, _, e1 := procLockFileEx.Call(
		uintptr(f.Fd()),
		uintptr(lockfileExclusiveLock),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&ol)),
	)
	if r1 == 0 {
		return fmt.Errorf("state: LockFileEx %q: %w", lockPath, e1)
	}
	defer func() {
		var uol syscall.Overlapped
		_, _, _ = procUnlockFileEx.Call(
			uintptr(f.Fd()),
			0,
			1,
			0,
			uintptr(unsafe.Pointer(&uol)),
		)
	}()
	return fn()
}
