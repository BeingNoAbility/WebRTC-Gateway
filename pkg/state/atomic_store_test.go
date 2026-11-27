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

	// Explicitly verify a stale older version cannot overwrite the latest persisted state.
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

	// Ensure zero stray temp files left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Fatalf("expected only state.json in dir, got %d entries", len(entries))
	}
}
