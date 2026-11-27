package state

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAtomicStoreConcurrentNoLoss(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store := NewAtomicFileStore(path)
	if err := store.CheckWritable(); err != nil {
		t.Fatalf("CheckWritable failed on writable dir: %v", err)
	}

	const workers = 32
	var ver atomic.Uint64
	var wg sync.WaitGroup
	errCh := make(chan error, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v := ver.Add(1)
			data := []byte(fmt.Sprintf(`{"ver":%d}`, v))
			if err := store.Save(v, data); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent AtomicFileStore.Save failed: %v", err)
	}

	finalVer := ver.Add(1)
	finalPayload := []byte(fmt.Sprintf(`{"ver":%d,"revoked":true}`, finalVer))
	if err := store.Save(finalVer, finalPayload); err != nil {
		t.Fatalf("final save failed: %v", err)
	}
	if err := store.Save(finalVer-1, []byte(`{"ver":0,"revoked":false}`)); err != nil {
		t.Fatalf("stale save should cleanly no-op, got: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read final state: %v", err)
	}
	if string(got) != string(finalPayload) {
		t.Fatalf("stale snapshot overwrote newer state: got %s, want %s", string(got), string(finalPayload))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Fatalf("expected only state.json in dir, got %d entries", len(entries))
	}
}

func TestAtomicStoreCheckWritableFailsClosed(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewAtomicFileStore(filepath.Join(blocker, "state.json"))
	if err := store.CheckWritable(); err == nil {
		t.Fatal("expected CheckWritable to fail closed when parent path is not a directory")
	}
}

func TestWithFileLockSerializesCallers(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "state.lock")
	var counter int
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := WithFileLock(lockPath, func() error {
				counter++
				return nil
			}); err != nil {
				t.Errorf("WithFileLock failed: %v", err)
			}
		}()
	}
	wg.Wait()
	if counter != 8 {
		t.Fatalf("expected 8 serialized executions, got %d", counter)
	}
}
