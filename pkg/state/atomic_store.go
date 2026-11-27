package state

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// AtomicFileStore serializes crash-safe atomic JSON/binary state persistence to disk.
// Stale snapshots with a version <= lastWrittenVer are skipped so out-of-order concurrent
// callers can never roll back newer revocations or key bindings.
type AtomicFileStore struct {
	path           string
	mu             sync.Mutex
	lastWrittenVer uint64
}

func NewAtomicFileStore(path string) *AtomicFileStore {
	return &AtomicFileStore{path: path}
}

func (s *AtomicFileStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Save atomically writes payload to s.path using a unique temp file + fsync + rename + dir fsync.
// If ver > 0 and ver <= lastWrittenVer, Save is a no-op and returns nil.
func (s *AtomicFileStore) Save(ver uint64, payload []byte) error {
	if s == nil || s.path == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if ver > 0 && ver <= s.lastWrittenVer {
		return nil
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("state: create dir %q: %w", dir, err)
	}

	f, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("state: create temp file in %q: %w", dir, err)
	}
	tmpPath := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = f.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	if err := f.Chmod(0600); err != nil {
		return fmt.Errorf("state: chmod temp file: %w", err)
	}
	if _, err := f.Write(payload); err != nil {
		return fmt.Errorf("state: write temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("state: fsync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("state: close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("state: rename %q -> %q: %w", tmpPath, s.path, err)
	}
	cleanup = false

	// Best-effort directory fsync so the directory entry survives sudden power loss.
	if df, derr := os.Open(filepath.Clean(dir)); derr == nil { // #nosec G304 -- parent directory of operator-configured state file
		_ = df.Sync()
		_ = df.Close()
	}

	if ver > s.lastWrittenVer {
		s.lastWrittenVer = ver
	}
	return nil
}
