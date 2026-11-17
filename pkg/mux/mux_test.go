package mux

import (
	"bytes"
	"context"
	"crypto/rand"
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
