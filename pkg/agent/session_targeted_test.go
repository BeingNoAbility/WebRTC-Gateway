package agent

import (
	"sync"
	"testing"

	"github.com/pion/webrtc/v4"
)

// 1. Verifies that Destroy() cleanly resets controlDC and dataDC (commit f79edb4)
func TestAgentSession_DataChannelResetOnDestroy(t *testing.T) {
	sess := newAgentSession("sess-test-1", "cookie-1", "127.0.0.1")

	// Simulate assigned DataChannels
	dummyControlDC := &webrtc.DataChannel{}
	dummyDataDC := &webrtc.DataChannel{}

	sess.mu.Lock()
	sess.controlDC = dummyControlDC
	sess.dataDC = dummyDataDC
	sess.mu.Unlock()

	// Verify they are populated
	sess.mu.Lock()
	if sess.controlDC == nil || sess.dataDC == nil {
		sess.mu.Unlock()
		t.Fatalf("Failed to initialize dummy DataChannels")
	}
	sess.mu.Unlock()

	// Call Destroy()
	sess.Destroy()

	// Verify pointers are reset to nil
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.controlDC != nil {
		t.Errorf("Expected sess.controlDC to be nil after Destroy(), got %v", sess.controlDC)
	}
	if sess.dataDC != nil {
		t.Errorf("Expected sess.dataDC to be nil after Destroy(), got %v", sess.dataDC)
	}
	if !sess.closed {
		t.Errorf("Expected sess.closed to be true after Destroy()")
	}
}

// 2. Concurrency stress test under race detector
func TestAgentSession_ConcurrentDestroyAndAccess(t *testing.T) {
	const iterations = 50
	var wg sync.WaitGroup

	for i := 0; i < iterations; i++ {
		sess := newAgentSession("stress-sess", "cookie-x", "127.0.0.1")
		sess.controlDC = &webrtc.DataChannel{}
		sess.dataDC = &webrtc.DataChannel{}

		wg.Add(3)

		// Goroutine 1: Candidate appending & consumption
		go func() {
			defer wg.Done()
			for k := 0; k < 10; k++ {
				sess.AddCandidate(webrtc.ICECandidateInit{Candidate: "cand-1"})
				_ = sess.DrainCandidates()
			}
		}()

		// Goroutine 2: Reading candidate count & status
		go func() {
			defer wg.Done()
			for k := 0; k < 10; k++ {
				_ = sess.CandidateCount()
			}
		}()

		// Goroutine 3: Destroy
		go func() {
			defer wg.Done()
			sess.Destroy()
		}()

		wg.Wait()
	}
}
