package state

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// CheckWritable verifies at startup that the parent directory of path exists (or can be created)
// and is writable by the current user (Fail-Closed for misconfigured container mounts).
func CheckWritable(path string) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
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

// WriteAtomic writes payload to path via unique temp file -> chmod -> write -> fsync -> close ->
// atomic rename -> best-effort parent directory fsync.
func WriteAtomic(path string, payload []byte, perm os.FileMode) error {
	if path == "" {
		return nil
	}
	if perm == 0 {
		perm = 0600
	}
	dir := filepath.Dir(path)
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

	if err := f.Chmod(perm); err != nil {
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
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("state: rename %q -> %q: %w", tmpPath, path, err)
	}
	cleanup = false

	// Best-effort directory fsync so the directory entry survives sudden power loss.
	if df, derr := os.Open(filepath.Clean(dir)); derr == nil { // #nosec G304 -- parent directory of operator-configured state file
		_ = df.Sync()
		_ = df.Close()
	}
	return nil
}

// LatestFileWriter serializes and coalesces concurrent versioned atomic snapshot writes.
//
// Invariants:
//   - Disk state is strictly monotonic: once version V has been submitted, no older version < V
//     can ever overwrite latestPayload or roll back disk state, even after transient write errors.
//   - Save(ver, payload) returns nil ONLY after a version >= ver has been durably fsynced and renamed.
type LatestFileWriter struct {
	path string

	mu             sync.Mutex
	latestVer      uint64
	latestPayload  []byte
	lastWrittenVer uint64

	writeMu sync.Mutex
}

func NewLatestFileWriter(path string) *LatestFileWriter {
	return &LatestFileWriter{path: path}
}

func (w *LatestFileWriter) Path() string {
	if w == nil {
		return ""
	}
	return w.path
}

func (w *LatestFileWriter) CheckWritable() error {
	if w == nil {
		return nil
	}
	return CheckWritable(w.path)
}

// Save durably writes the highest submitted snapshot >= ver to disk.
// Callers passing ver == 0 perform an unversioned serialized atomic write.
func (w *LatestFileWriter) Save(ver uint64, payload []byte) error {
	if w == nil || w.path == "" {
		return nil
	}
	if ver == 0 {
		w.writeMu.Lock()
		defer w.writeMu.Unlock()
		return WriteAtomic(w.path, payload, 0600)
	}

	w.mu.Lock()
	if ver <= w.lastWrittenVer {
		w.mu.Unlock()
		return nil
	}
	if ver > w.latestVer {
		w.latestVer = ver
		w.latestPayload = payload
	}
	w.mu.Unlock()

	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	w.mu.Lock()
	if ver <= w.lastWrittenVer {
		w.mu.Unlock()
		return nil
	}
	targetVer := w.latestVer
	targetPayload := w.latestPayload
	w.mu.Unlock()

	if err := WriteAtomic(w.path, targetPayload, 0600); err != nil {
		// Retain latestVer/latestPayload intact so subsequent retries never roll back to older versions.
		return err
	}

	w.mu.Lock()
	if targetVer > w.lastWrittenVer {
		w.lastWrittenVer = targetVer
	}
	if w.latestVer == targetVer {
		w.latestPayload = nil
	}
	w.mu.Unlock()
	return nil
}
