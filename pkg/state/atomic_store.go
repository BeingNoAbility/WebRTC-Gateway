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
// Concurrent versioned Save() calls coalesce onto the newest snapshot while still
// guaranteeing every caller waits until a version >= its own is durably flushed.
type AtomicFileStore struct {
	path string

	mu             sync.Mutex
	cond           *sync.Cond
	writing        bool
	lastWrittenVer uint64
	lastErr        error
	hasPending     bool
	pendingVer     uint64
	pendingPayload []byte
}

func NewAtomicFileStore(path string) *AtomicFileStore {
	s := &AtomicFileStore{path: path}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *AtomicFileStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// CheckWritable verifies at startup that the target directory exists (or can be created)
// and is writable by the current user (Fail-Closed for misconfigured container mounts).
func (s *AtomicFileStore) CheckWritable() error {
	if s == nil || s.path == "" {
		return nil
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("state: create dir %q: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".state-probe-*.tmp")
	if err != nil {
		return fmt.Errorf("state: directory %q is not writable: %w", dir, err)
	}
	probePath := f.Name()
	_ = f.Close()
	_ = os.Remove(probePath)
	return nil
}

// Save atomically writes payload to s.path using a unique temp file + fsync + rename + dir fsync.
// If ver > 0 and ver <= lastWrittenVer, Save is a no-op and returns nil.
// Concurrent callers with ver > 0 coalesce into the highest pending version while preserving
// durability: Save returns nil only once a version >= ver has been fsynced and renamed.
func (s *AtomicFileStore) Save(ver uint64, payload []byte) error {
	if s == nil || s.path == "" {
		return nil
	}

	s.mu.Lock()
	if ver == 0 {
		for s.writing {
			s.cond.Wait()
		}
		s.writing = true
		s.mu.Unlock()

		err := s.writeAtomic(payload)

		s.mu.Lock()
		s.writing = false
		s.cond.Broadcast()
		s.mu.Unlock()
		return err
	}

	if ver <= s.lastWrittenVer {
		s.mu.Unlock()
		return nil
	}
	if !s.hasPending || ver > s.pendingVer {
		s.hasPending = true
		s.pendingVer = ver
		s.pendingPayload = payload
	}

	for {
		if ver <= s.lastWrittenVer {
			s.mu.Unlock()
			return nil
		}
		if s.writing {
			s.cond.Wait()
			if ver <= s.lastWrittenVer {
				s.mu.Unlock()
				return nil
			}
			// If our batch finished with an error and nothing newer superseded it, report that error.
			if !s.writing && !s.hasPending && s.lastErr != nil {
				err := s.lastErr
				s.mu.Unlock()
				return err
			}
			continue
		}
		if !s.hasPending {
			err := s.lastErr
			s.mu.Unlock()
			return err
		}

		writeVer := s.pendingVer
		writePayload := s.pendingPayload
		s.hasPending = false
		s.pendingVer = 0
		s.pendingPayload = nil
		s.writing = true
		s.mu.Unlock()

		err := s.writeAtomic(writePayload)

		s.mu.Lock()
		s.writing = false
		s.lastErr = err
		if err == nil && writeVer > s.lastWrittenVer {
			s.lastWrittenVer = writeVer
		}
		s.cond.Broadcast()
		if err != nil {
			s.mu.Unlock()
			return err
		}
		if ver <= s.lastWrittenVer {
			s.mu.Unlock()
			return nil
		}
	}
}

func (s *AtomicFileStore) writeAtomic(payload []byte) error {
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
	return nil
}
