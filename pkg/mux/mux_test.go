package mux

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type mockTransport struct {
	peer           *mockTransport
	queue          chan []byte
	lowThreshold   uint64
	bufferedAmount uint64
	onLow          func()
	mu             sync.Mutex
	onMsg          func([]byte)
	closeCh        chan struct{}
}

func newMockPair() (*mockTransport, *mockTransport) {
	t1 := &mockTransport{queue: make(chan []byte, 10000), closeCh: make(chan struct{})}
	t2 := &mockTransport{queue: make(chan []byte, 10000), closeCh: make(chan struct{})}
	t1.peer = t2
	t2.peer = t1

	go t1.worker()
	go t2.worker()

	return t1, t2
}

func (m *mockTransport) worker() {
	for {
		select {
		case <-m.closeCh:
			return
		case data, ok := <-m.queue:
			if !ok {
				return
			}
			m.mu.Lock()
			cb := m.onMsg
			m.mu.Unlock()
			if cb != nil {
				cb(data)
			}
		}
	}
}

func (m *mockTransport) Send(data []byte) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	m.peer.queue <- cp
	return nil
}

func (m *mockTransport) BufferedAmount() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bufferedAmount
}

func (m *mockTransport) SetBufferedAmountLowThreshold(th uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lowThreshold = th
}

func (m *mockTransport) OnBufferedAmountLow(f func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onLow = f
}

func (m *mockTransport) SetMessageHandler(cb func([]byte)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onMsg = cb
}

func (m *mockTransport) Close() {
	close(m.closeCh)
}

func TestMuxPingPongAndConcurrentStreams(t *testing.T) {
	t1, t2 := newMockPair()
	defer t1.Close()
	defer t2.Close()

	clientMux := NewMux(t1, true)
	agentMux := NewMux(t2, false)

	t1.SetMessageHandler(func(data []byte) {
		_ = clientMux.HandleIncomingMessage(data)
	})
	t2.SetMessageHandler(func(data []byte) {
		_ = agentMux.HandleIncomingMessage(data)
	})

	defer clientMux.Close()
	defer agentMux.Close()

	// 1. Test Ping / Pong
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	rtt, err := clientMux.Ping(ctx)
	if err != nil {
		t.Fatalf("Ping failed: %v", err)
	}
	t.Logf("Ping Pong RTT: %v", rtt)

	// 2. Test Concurrent Streams
	numStreams := 5
	streamSize := 64 * 1024 // 64KB per stream, requiring multi-frame chunking

	var wg sync.WaitGroup
	wg.Add(numStreams)

	// Server accept worker
	go func() {
		for i := 0; i < numStreams; i++ {
			s, err := agentMux.AcceptStream(context.Background())
			if err != nil {
				return
			}
			go func(stream *Stream) {
				defer wg.Done()
				defer stream.Close()

				buf := new(bytes.Buffer)
				_, err := io.Copy(buf, stream)
				if err != nil {
					t.Errorf("server read error: %v", err)
					return
				}

				// Echo back
				_, err = stream.Write(buf.Bytes())
				if err != nil {
					t.Errorf("server write error: %v", err)
				}
			}(s)
		}
	}()

	// Client initiate streams concurrently
	clientWg := sync.WaitGroup{}
	for i := 0; i < numStreams; i++ {
		clientWg.Add(1)
		go func(idx int) {
			defer clientWg.Done()

			s, err := clientMux.OpenStream()
			if err != nil {
				t.Errorf("OpenStream error: %v", err)
				return
			}

			payload := make([]byte, streamSize)
			_, _ = rand.Read(payload)

			go func() {
				_, err := s.Write(payload)
				if err != nil {
					t.Errorf("client write error: %v", err)
				}
				_ = s.Close()
			}()

			received := new(bytes.Buffer)
			_, err = io.Copy(received, s)
			if err != nil {
				t.Errorf("client read error: %v", err)
				return
			}

			if !bytes.Equal(received.Bytes(), payload) {
				t.Errorf("stream %d payload mismatch: sent %d bytes, got %d bytes", idx, len(payload), received.Len())
			}
		}(i)
	}

	clientWg.Wait()
	wg.Wait()
}

// 2. Test Stream.Reset() cancels context, notifies peer, and unblocks reads/writes
func TestMux_StreamResetAndContextCancel(t *testing.T) {
	t1, t2 := newMockPair()
	defer t1.Close()
	defer t2.Close()

	clientMux := NewMux(t1, true)
	agentMux := NewMux(t2, false)

	t1.SetMessageHandler(func(data []byte) { _ = clientMux.HandleIncomingMessage(data) })
	t2.SetMessageHandler(func(data []byte) { _ = agentMux.HandleIncomingMessage(data) })

	defer clientMux.Close()
	defer agentMux.Close()

	streamAccepted := make(chan *Stream, 1)
	go func() {
		s, err := agentMux.AcceptStream(context.Background())
		if err == nil {
			streamAccepted <- s
		}
	}()

	clientStream, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}

	// Send initial data to trigger AcceptStream on agent side
	_, _ = clientStream.Write([]byte("initial trigger"))

	var serverStream *Stream
	select {
	case serverStream = <-streamAccepted:
	case <-time.After(2 * time.Second):
		t.Fatalf("Timeout waiting for server to accept stream")
	}

	// Client resets stream
	if err := clientStream.Reset(); err != nil {
		t.Fatalf("Reset failed: %v", err)
	}

	// 1. Client context must be canceled
	select {
	case <-clientStream.Context().Done():
	default:
		t.Errorf("Client stream context should be canceled after Reset()")
	}

	// 2. Client write should fail
	_, err = clientStream.Write([]byte("after reset"))
	if err != ErrStreamClosed {
		t.Errorf("Write after reset should return ErrStreamClosed, got %v", err)
	}

	// 3. Server stream should be marked reset by peer
	buf := make([]byte, 100)
	// Drain initial trigger first
	_, _ = serverStream.Read(buf)

	// Next read must return ErrStreamReset
	readErrCh := make(chan error, 1)
	go func() {
		_, rErr := serverStream.Read(buf)
		readErrCh <- rErr
	}()

	select {
	case rErr := <-readErrCh:
		if rErr != ErrStreamReset {
			t.Errorf("Expected ErrStreamReset on remote reset, got %v", rErr)
		}
	case <-time.After(2 * time.Second):
		t.Errorf("Timeout waiting for server stream to receive remote reset")
	}
}

// 3. Test Write on closed stream returns ErrStreamClosed
func TestMux_WriteOnClosedStream(t *testing.T) {
	t1, t2 := newMockPair()
	defer t1.Close()
	defer t2.Close()

	clientMux := NewMux(t1, true)
	agentMux := NewMux(t2, false)

	defer clientMux.Close()
	defer agentMux.Close()

	s, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}

	// Close stream
	if err := s.Close(); err != nil {
		t.Fatalf("Close stream failed: %v", err)
	}

	// Write after close must return ErrStreamClosed
	_, err = s.Write([]byte("fail write"))
	if err != ErrStreamClosed {
		t.Errorf("Expected ErrStreamClosed on write after close, got %v", err)
	}
}

// 4. Test Read after remote FIN reads buffered data then returns io.EOF
func TestMux_ReadAfterFin(t *testing.T) {
	t1, t2 := newMockPair()
	defer t1.Close()
	defer t2.Close()

	clientMux := NewMux(t1, true)
	agentMux := NewMux(t2, false)

	t1.SetMessageHandler(func(data []byte) { _ = clientMux.HandleIncomingMessage(data) })
	t2.SetMessageHandler(func(data []byte) { _ = agentMux.HandleIncomingMessage(data) })

	defer clientMux.Close()
	defer agentMux.Close()

	go func() {
		s, err := agentMux.AcceptStream(context.Background())
		if err == nil {
			_, _ = s.Write([]byte("data before fin"))
			_ = s.Close() // sends FIN
		}
	}()

	clientStream, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}
	_, _ = clientStream.Write([]byte("trigger"))

	// Read from client stream
	readBuf := make([]byte, 100)
	n, err := clientStream.Read(readBuf)
	if err != nil && err != io.EOF {
		t.Fatalf("Read failed: %v", err)
	}
	if string(readBuf[:n]) != "data before fin" {
		t.Fatalf("Expected %q, got %q", "data before fin", string(readBuf[:n]))
	}

	// Subsequent read must return io.EOF
	_, err = clientStream.Read(readBuf)
	if err != io.EOF {
		t.Fatalf("Expected io.EOF after FIN, got %v", err)
	}
}

// 5. Test 32-bit Stream ID wrap-around handling
func TestMux_StreamIDWrapAround(t *testing.T) {
	t1, _ := newMockPair()
	defer t1.Close()

	clientMux := NewMux(t1, true)
	defer clientMux.Close()

	// Artificially advance nextStreamID to wrap-around threshold
	clientMux.nextStreamID = 0xFFFF0000

	// OpenStream immediately intercepts id >= 0xFFFF0000 and wraps to base (1 for client)
	s1, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream wrap-around failed: %v", err)
	}
	if s1.ID() != 1 {
		t.Errorf("Expected wrapped stream ID 1, got %d", s1.ID())
	}

	// Subsequent stream continues monotonically with base + 2 = 3
	s2, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream after wrap-around failed: %v", err)
	}
	if s2.ID() != 3 {
		t.Errorf("Expected next stream ID 3, got %d", s2.ID())
	}
}

// 6. Test Backpressure flow control: writes block when buffer is full and resume on low threshold
func TestMux_BackpressureWaitAndResume(t *testing.T) {
	t1, t2 := newMockPair()
	defer t1.Close()
	defer t2.Close()

	clientMux := NewMux(t1, true)
	defer clientMux.Close()

	// Simulate high buffered amount on transport (> DefaultBufferedHighWaterMark = 256KB)
	t1.mu.Lock()
	t1.bufferedAmount = DefaultBufferedHighWaterMark + 1024
	t1.mu.Unlock()

	s, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}

	writeDone := make(chan error, 1)
	go func() {
		_, err := s.Write([]byte("backpressure payload"))
		writeDone <- err
	}()

	// Ensure write is blocked
	select {
	case <-writeDone:
		t.Fatalf("Write should have blocked due to high water mark backpressure!")
	case <-time.After(100 * time.Millisecond):
		// Expected: write is blocking
	}

	// Drain buffer to low water mark and trigger callback
	t1.mu.Lock()
	t1.bufferedAmount = DefaultBufferedLowWaterMark - 100
	onLow := t1.onLow
	t1.mu.Unlock()

	if onLow != nil {
		onLow()
	}

	// Write should now unblock and complete successfully
	select {
	case err := <-writeDone:
		if err != nil {
			t.Errorf("Write failed after unblock: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Timeout: write did not unblock after low water mark trigger")
	}
}

// 7. Test that control and headers frames bypass data buffer backpressure (no HoL blocking)
func TestMux_ControlBypassesBackpressure(t *testing.T) {
	t1, t2 := newMockPair()
	defer t1.Close()
	defer t2.Close()

	clientMux := NewMux(t1, true)
	agentMux := NewMux(t2, false)

	t1.SetMessageHandler(func(data []byte) { _ = clientMux.HandleIncomingMessage(data) })
	t2.SetMessageHandler(func(data []byte) { _ = agentMux.HandleIncomingMessage(data) })

	defer clientMux.Close()
	defer agentMux.Close()

	// Simulate high buffered amount (> 256KB)
	t1.mu.Lock()
	t1.bufferedAmount = DefaultBufferedHighWaterMark + 2048
	t1.mu.Unlock()

	// Stream 1: Data frame should block
	s1, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}

	dataDone := make(chan struct{})
	go func() {
		_, _ = s1.Write([]byte("blocked data payload"))
		close(dataDone)
	}()

	// Confirm data write is blocked
	select {
	case <-dataDone:
		t.Fatalf("Data write should have been blocked!")
	case <-time.After(50 * time.Millisecond):
		// Expected: blocked
	}

	// Stream 2: Send HEADERS frame (must NOT block!)
	s2, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream 2 failed: %v", err)
	}

	headerDone := make(chan error, 1)
	go func() {
		err := s2.WriteHeaders([]byte(`{"test":"fast_header"}`))
		headerDone <- err
	}()

	select {
	case err := <-headerDone:
		if err != nil {
			t.Errorf("WriteHeaders failed: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("WriteHeaders was blocked by backpressure! Head-of-Line blocking detected.")
	}

	// Ping (must NOT block!)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = clientMux.Ping(ctx)
	if err != nil {
		t.Errorf("Ping failed or blocked: %v", err)
	}

	// Now drain buffer so data writer finishes cleanly
	t1.mu.Lock()
	t1.bufferedAmount = DefaultBufferedLowWaterMark - 10
	onLow := t1.onLow
	t1.mu.Unlock()
	if onLow != nil {
		onLow()
	}

	select {
	case <-dataDone:
	case <-time.After(1 * time.Second):
		t.Fatalf("Timeout waiting for data writer to complete after drain")
	}
}

func TestStreamIDWraparoundAndGhostProtection(t *testing.T) {
	t1, t2 := newMockPair()
	defer t1.Close()
	defer t2.Close()

	clientMux := NewMux(t1, true)
	agentMux := NewMux(t2, false)

	t1.SetMessageHandler(func(data []byte) {
		_ = clientMux.HandleIncomingMessage(data)
	})
	t2.SetMessageHandler(func(data []byte) {
		_ = agentMux.HandleIncomingMessage(data)
	})

	// Fast-forward clientMux.nextStreamID close to wraparound limit
	atomic.StoreUint32(&clientMux.nextStreamID, 0xFFFFFFFE)

	// Open stream triggers wraparound handling
	s1, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed at wraparound boundary: %v", err)
	}
	if s1.ID() < 1 || s1.ID() > 10 {
		t.Fatalf("Expected wrapped stream ID to restart near base 1, got %d", s1.ID())
	}

	// Verify agentMux correctly handles wraparound from high ID to low ID without ghost dropping
	agentMux.mu.Lock()
	agentMux.maxPeerStreamID = 0xFFFFFFFD // Simulated near-limit peer stream ID
	agentMux.mu.Unlock()

	// Write headers from wrapped client stream
	err = s1.WriteHeaders([]byte(`{"test":"wraparound"}`))
	if err != nil {
		t.Fatalf("WriteHeaders failed on wrapped stream: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	accepted, err := agentMux.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("agentMux failed to accept wrapped stream (likely dropped by ghost protection): %v", err)
	}
	if accepted.ID() != s1.ID() {
		t.Fatalf("Stream ID mismatch: expected %d, got %d", s1.ID(), accepted.ID())
	}
}

// 9. Test that write goroutines blocked on backpressure immediately unblock and abort when the stream is reset
func TestMux_BackpressureUnblocksOnReset(t *testing.T) {
	t1, t2 := newMockPair()
	defer t1.Close()
	defer t2.Close()

	clientMux := NewMux(t1, true)
	agentMux := NewMux(t2, false)

	t1.SetMessageHandler(func(data []byte) { _ = clientMux.HandleIncomingMessage(data) })
	t2.SetMessageHandler(func(data []byte) { _ = agentMux.HandleIncomingMessage(data) })

	defer clientMux.Close()
	defer agentMux.Close()

	// Simulate high buffered amount (> 256KB) that never drains
	t1.mu.Lock()
	t1.bufferedAmount = DefaultBufferedHighWaterMark + 64*1024
	t1.mu.Unlock()

	s, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}

	writeErrCh := make(chan error, 1)
	go func() {
		_, writeErr := s.Write([]byte("large payload waiting for backpressure drain"))
		writeErrCh <- writeErr
	}()

	// Ensure the write is actually blocked waiting on bpCond
	select {
	case <-writeErrCh:
		t.Fatalf("Write should have been blocked by backpressure!")
	case <-time.After(50 * time.Millisecond):
		// Expected: blocked on m.bpCond.Wait()
	}

	// Reset the stream
	if err := s.Reset(); err != nil {
		t.Fatalf("Reset failed: %v", err)
	}

	// Write goroutine MUST immediately unblock without buffer draining!
	select {
	case err := <-writeErrCh:
		if err == nil {
			t.Errorf("Expected write to fail with error after Reset, got nil")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("Deadlock! Write goroutine hung forever on bpCond.Wait() after Reset()")
	}
}

// 10. Test that write goroutines blocked on backpressure unblock when remote peer sends RST
func TestMux_BackpressureUnblocksOnRemoteReset(t *testing.T) {
	t1, t2 := newMockPair()
	defer t1.Close()
	defer t2.Close()

	clientMux := NewMux(t1, true)
	agentMux := NewMux(t2, false)

	t1.SetMessageHandler(func(data []byte) { _ = clientMux.HandleIncomingMessage(data) })
	t2.SetMessageHandler(func(data []byte) { _ = agentMux.HandleIncomingMessage(data) })

	defer clientMux.Close()
	defer agentMux.Close()

	s, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}

	// Trigger accept
	_, _ = s.Write([]byte("init"))
	srvStream, err := agentMux.AcceptStream(context.Background())
	if err != nil {
		t.Fatalf("AcceptStream failed: %v", err)
	}

	// Simulate high buffered amount on agent side
	t2.mu.Lock()
	t2.bufferedAmount = DefaultBufferedHighWaterMark + 64*1024
	t2.mu.Unlock()

	srvWriteDone := make(chan error, 1)
	go func() {
		_, writeErr := srvStream.Write([]byte("large payload waiting for backpressure"))
		srvWriteDone <- writeErr
	}()

	select {
	case <-srvWriteDone:
		t.Fatalf("Server write should be blocked on backpressure")
	case <-time.After(50 * time.Millisecond):
	}

	// Client sends Reset
	if err := s.Reset(); err != nil {
		t.Fatalf("Client Reset failed: %v", err)
	}

	// Server write goroutine MUST unblock
	select {
	case err := <-srvWriteDone:
		if err == nil {
			t.Errorf("Expected write to fail on remote reset, got nil")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("Deadlock! Server write hung forever on remote Reset()")
	}
}

// 11. Test concurrent Ping/Pong with 8-byte sequence ID (zero crosstalk, zero stale pollution)
func TestMux_ConcurrentPingPong_NoCrosstalk(t *testing.T) {
	t1, t2 := newMockPair()
	defer t1.Close()
	defer t2.Close()

	clientMux := NewMux(t1, true)
	agentMux := NewMux(t2, false)

	t1.SetMessageHandler(func(data []byte) { _ = clientMux.HandleIncomingMessage(data) })
	t2.SetMessageHandler(func(data []byte) { _ = agentMux.HandleIncomingMessage(data) })

	defer clientMux.Close()
	defer agentMux.Close()

	const concurrency = 10
	var wg sync.WaitGroup
	errCh := make(chan error, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			rtt, err := clientMux.Ping(ctx)
			if err != nil {
				errCh <- fmt.Errorf("Ping failed: %w", err)
				return
			}
			if rtt < 0 {
				errCh <- fmt.Errorf("Invalid RTT: %v", rtt)
				return
			}
		}()
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("Concurrent ping error: %v", err)
	}
}

// 12. Test global inbound memory budget, backpressure waiting/resume, and zero-leak on stream reset (Task 7 / B 项)
func TestMux_GlobalInboundMemoryBudget_Backpressure(t *testing.T) {
	t1, t2 := newMockPair()
	defer t1.Close()
	defer t2.Close()

	clientMux := NewMux(t1, true)
	agentMux := NewMux(t2, false)

	// Configure tight inbound limits on agent: max 10KB, high 7KB, low 3KB, backpressure timeout 100ms
	agentMux.SetInboundLimits(10000, 7000, 3000, 100*time.Millisecond)

	t1.SetMessageHandler(func(data []byte) { _ = clientMux.HandleIncomingMessage(data) })
	t2.SetMessageHandler(func(data []byte) { _ = agentMux.HandleIncomingMessage(data) })

	defer clientMux.Close()
	defer agentMux.Close()

	// 1. Open stream 1 and write 5000 bytes (below high watermark of 7000)
	s1, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}

	data5k := make([]byte, 5000)
	_, _ = rand.Read(data5k)
	if _, err := s1.Write(data5k); err != nil {
		t.Fatalf("Write 5k failed: %v", err)
	}

	serverS1, err := agentMux.AcceptStream(context.Background())
	if err != nil {
		t.Fatalf("AcceptStream failed: %v", err)
	}

	// Verify agent inboundBytes is tracked deterministically
	deadline := time.Now().Add(time.Second)
	for agentMux.InboundBytes() < 5000 && time.Now().Before(deadline) {
		time.Sleep(1 * time.Millisecond)
	}
	if inBytes := agentMux.InboundBytes(); inBytes < 5000 {
		t.Fatalf("Expected inboundBytes >= 5000, got %d", inBytes)
	}

	// 2. Read 3000 bytes on server side -> drops below 3000 threshold
	buf := make([]byte, 3000)
	if _, err := io.ReadFull(serverS1, buf); err != nil {
		t.Fatalf("ReadFull failed: %v", err)
	}

	deadline = time.Now().Add(time.Second)
	for agentMux.InboundBytes() > 2000 && time.Now().Before(deadline) {
		time.Sleep(1 * time.Millisecond)
	}
	if inBytes := agentMux.InboundBytes(); inBytes > 2000 {
		t.Fatalf("Expected inboundBytes <= 2000 after read, got %d", inBytes)
	}

	// 3. Open stream 2 and write data exceeding hard maximum (12000 bytes) without reading
	s2, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream 2 failed: %v", err)
	}

	data12k := make([]byte, 12000)
	_, _ = rand.Read(data12k)
	_, _ = s2.Write(data12k)

	serverS2, err := agentMux.AcceptStream(context.Background())
	if err != nil {
		t.Fatalf("AcceptStream 2 failed: %v", err)
	}

	// Wait for backpressure timeout / hard breach to trigger stream reset
	select {
	case <-serverS2.Context().Done():
		// Stream was properly reset due to budget breach
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("Expected server stream 2 to be reset due to inbound budget breach")
	}

	// 4. Close/Reset stream 1 and verify complete cleanup (zero ghost memory leak)
	_ = s1.Reset()
	_ = serverS1.Reset()
	_ = s2.Reset()

	deadline = time.Now().Add(time.Second)
	for agentMux.InboundBytes() != 0 && time.Now().Before(deadline) {
		time.Sleep(1 * time.Millisecond)
	}
	if remaining := agentMux.InboundBytes(); remaining != 0 {
		t.Fatalf("Expected 0 remaining inboundBytes after all streams reset, got %d (memory counter leak!)", remaining)
	}
}

func TestMux_InboundBackpressure_TimeoutBetweenWatermarkAndMax(t *testing.T) {
	t1, t2 := newMockPair()
	defer t1.Close()
	defer t2.Close()

	clientMux := NewMux(t1, true)
	agentMux := NewMux(t2, false)

	// Low: 2000, High: 4000, Max: 10000, Timeout: 50ms
	agentMux.SetInboundLimits(10000, 4000, 2000, 50*time.Millisecond)

	t1.SetMessageHandler(func(data []byte) { _ = clientMux.HandleIncomingMessage(data) })
	t2.SetMessageHandler(func(data []byte) { _ = agentMux.HandleIncomingMessage(data) })

	defer clientMux.Close()
	defer agentMux.Close()

	// 1. Open stream 1 and write 3000 bytes (puts load > low of 2000)
	s1, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream 1 failed: %v", err)
	}
	defer s1.Reset()

	data3k := make([]byte, 3000)
	_, _ = rand.Read(data3k)
	if _, err := s1.Write(data3k); err != nil {
		t.Fatalf("Write 3k failed: %v", err)
	}

	serverS1, err := agentMux.AcceptStream(context.Background())
	if err != nil {
		t.Fatalf("AcceptStream 1 failed: %v", err)
	}
	defer serverS1.Reset()

	deadline := time.Now().Add(time.Second)
	for agentMux.InboundBytes() < 3000 && time.Now().Before(deadline) {
		time.Sleep(1 * time.Millisecond)
	}

	// 2. Open stream 2 and write 4000 bytes. Total inbound = 7000 bytes (between high 4000 and max 10000)
	// Server does not read stream 1, so load remains > low(2000).
	// Backpressure wait will block for 50ms and then timeout, triggering stream 2 reset!
	s2, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream 2 failed: %v", err)
	}
	defer s2.Reset()

	data4k := make([]byte, 4000)
	_, _ = rand.Read(data4k)
	_, _ = s2.Write(data4k)

	serverS2, err := agentMux.AcceptStream(context.Background())
	if err != nil {
		t.Fatalf("AcceptStream 2 failed: %v", err)
	}
	defer serverS2.Reset()

	select {
	case <-serverS2.Context().Done():
		// PASS: stream 2 was properly reset due to backpressure timeout
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("Expected stream 2 to be reset by backpressure timeout when backlog remained between high and max")
	}

	// Verify quota rollback: delta (4000) was rolled back, so inboundBytes is back to stream 1's 3000
	deadline = time.Now().Add(time.Second)
	for agentMux.InboundBytes() != 3000 && time.Now().Before(deadline) {
		time.Sleep(1 * time.Millisecond)
	}
	if inBytes := agentMux.InboundBytes(); inBytes != 3000 {
		t.Fatalf("Expected inboundBytes to rollback to 3000 after stream 2 timed out, got %d", inBytes)
	}
}
