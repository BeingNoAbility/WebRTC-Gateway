package mux

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
	"webrtc-gateway/pkg/protocol"
)

var (
	ErrStreamClosed = errors.New("stream closed")
	ErrStreamReset  = errors.New("stream reset by peer")
)

const (
	// maxStreamInboundBytes limits buffered unread data per stream to 32MB to prevent memory exhaustion
	maxStreamInboundBytes = 32 * 1024 * 1024
)

// Stream represents a bidirectional multiplexed stream over P2PMux.
type Stream struct {
	id          uint32
	initialType byte
	mux         *Mux
	ctx         context.Context
	cancel      context.CancelFunc

	mu           sync.Mutex
	cond         *sync.Cond
	readBuf      []byte
	inbound      [][]byte
	inboundBytes int
	headersOnce  sync.Once
	headersCh    chan []byte

	readClosed      bool
	writeClosed     bool
	headersReceived bool
	isReset         bool
	err             error
	lastActive      atomic.Int64
}

func (s *Stream) touch() {
	s.lastActive.Store(time.Now().UnixNano())
}

// LastActive returns the last time data or headers were read or written on this stream.
func (s *Stream) LastActive() time.Time {
	return time.Unix(0, s.lastActive.Load())
}

func newStream(id uint32, m *Mux) *Stream {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Stream{
		id:              id,
		mux:             m,
		ctx:             ctx,
		cancel:          cancel,
		inbound:         make([][]byte, 0, 16),
		headersCh:       make(chan []byte, 1),
		headersReceived: !m.isClient,
	}
	s.touch()
	s.cond = sync.NewCond(&s.mu)
	return s
}

// ID returns the stream ID.
func (s *Stream) ID() uint32 {
	return s.id
}

// InitialType returns the frame type that initiated this stream.
func (s *Stream) InitialType() byte {
	return s.initialType
}

// Context returns the stream context (cancelled on reset/close).
func (s *Stream) Context() context.Context {
	return s.ctx
}

// Read reads payload data from the stream.
func (s *Stream) Read(p []byte) (n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for {
		if len(s.readBuf) > 0 {
			n = copy(p, s.readBuf)
			s.readBuf = s.readBuf[n:]
			s.inboundBytes -= n
			if s.inboundBytes < 0 {
				s.inboundBytes = 0
			}
			s.mux.releaseInbound(n)
			s.touch()
			return n, nil
		}

		if len(s.inbound) > 0 {
			s.readBuf = s.inbound[0]
			s.inbound[0] = nil // Zero out consumed pointer to allow immediate GC
			s.inbound = s.inbound[1:]
			if len(s.inbound) == 0 {
				s.inbound = s.inbound[:0]
			}
			n = copy(p, s.readBuf)
			s.readBuf = s.readBuf[n:]
			s.inboundBytes -= n
			if s.inboundBytes < 0 {
				s.inboundBytes = 0
			}
			s.mux.releaseInbound(n)
			s.touch()
			return n, nil
		}

		if s.isReset {
			return 0, ErrStreamReset
		}

		if s.readClosed {
			if s.err != nil {
				return 0, s.err
			}
			return 0, io.EOF
		}

		s.cond.Wait()
	}
}

// Write writes payload data to the stream, automatically chunking into safe frames.
func (s *Stream) Write(p []byte) (n int, err error) {
	s.mu.Lock()
	if s.writeClosed || s.isReset {
		s.mu.Unlock()
		return 0, ErrStreamClosed
	}
	s.mu.Unlock()
	s.touch()

	total := len(p)
	sent := 0

	for sent < total {
		chunkSize := total - sent
		if chunkSize > protocol.RecommendedChunkSize {
			chunkSize = protocol.RecommendedChunkSize
		}

		chunk := p[sent : sent+chunkSize]
		frame := protocol.NewFrame(s.id, protocol.TypeData, protocol.FlagNone, chunk)
		if err := s.mux.writeFrame(frame); err != nil {
			return sent, err
		}

		sent += chunkSize
	}

	return total, nil
}

// WriteHeaders sends a HEADERS frame on this stream.
func (s *Stream) WriteHeaders(payload []byte) error {
	s.mu.Lock()
	if s.writeClosed || s.isReset {
		s.mu.Unlock()
		return ErrStreamClosed
	}
	s.mu.Unlock()
	s.touch()

	frame := protocol.NewFrame(s.id, protocol.TypeHeaders, protocol.FlagNone, payload)
	return s.mux.writeFrame(frame)
}

// ReadHeaders waits for and returns the HEADERS payload for this stream.
func (s *Stream) ReadHeaders(ctx context.Context) ([]byte, error) {
	// Priority check: if headers already arrived, return them directly without race with s.ctx.Done()
	select {
	case h, ok := <-s.headersCh:
		if ok {
			return h, nil
		}
		return nil, ErrStreamClosed
	default:
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.ctx.Done():
		// Secondary check in case headers arrived concurrently with cancellation
		select {
		case h, ok := <-s.headersCh:
			if ok {
				return h, nil
			}
		default:
		}
		return nil, ErrStreamClosed
	case h, ok := <-s.headersCh:
		if !ok {
			return nil, ErrStreamClosed
		}
		return h, nil
	}
}

// PushInternalHeaders pushes headers back into the channel for subsequent readers.
func (s *Stream) PushInternalHeaders(payload []byte) {
	cp := make([]byte, len(payload))
	copy(cp, payload)
	select {
	case s.headersCh <- cp:
	default:
	}
}

// Close closes the write side of the stream (sends FIN).
func (s *Stream) Close() error {
	s.mu.Lock()
	if s.writeClosed {
		s.mu.Unlock()
		return nil
	}
	s.writeClosed = true
	s.mu.Unlock()

	frame := protocol.NewFrame(s.id, protocol.TypeData, protocol.FlagFin, nil)
	err := s.mux.writeFrame(frame)

	s.checkFullClose()
	return err
}

// drainUnreadInboundLocked releases all buffered unread inbound bytes back to the Mux counter.
// Must be called with s.mu held.
func (s *Stream) drainUnreadInboundLocked() {
	unread := s.inboundBytes
	if unread > 0 {
		s.inboundBytes = 0
		s.readBuf = nil
		for i := range s.inbound {
			s.inbound[i] = nil
		}
		s.inbound = nil
		s.mux.releaseInbound(unread)
	}
}

// markResetNoRemove marks stream as reset without calling removeStream (used during Mux.Close).
func (s *Stream) markResetNoRemove() {
	s.mu.Lock()
	if s.isReset {
		s.mu.Unlock()
		return
	}
	s.isReset = true
	s.readClosed = true
	s.writeClosed = true
	s.drainUnreadInboundLocked()
	s.cond.Broadcast()
	s.cancel()
	s.mu.Unlock()
}

// Reset aborts the stream immediately and notifies peer.
func (s *Stream) Reset() error {
	s.mu.Lock()
	if s.isReset {
		s.mu.Unlock()
		return nil
	}
	s.isReset = true
	s.readClosed = true
	s.writeClosed = true
	s.drainUnreadInboundLocked()
	s.cond.Broadcast()
	s.cancel()
	s.mu.Unlock()

	// Wake up any goroutines waiting on mux backpressure
	s.mux.broadcastBackpressure()

	frame := protocol.NewFrame(s.id, protocol.TypeRstStream, protocol.FlagNone, nil)
	_ = s.mux.writeFrame(frame)
	s.mux.removeStream(s.id)
	return nil
}

// pushInboundData handles received DATA frame payload.
func (s *Stream) pushInboundData(payload []byte, fin bool) {
	s.touch()

	payloadLen := len(payload)
	if payloadLen > 0 {
		// Fast check under lock if stream is already reset or closed
		s.mu.Lock()
		if s.readClosed || s.isReset {
			s.mu.Unlock()
			return
		}

		// 1. Single-stream buffer quota check (32MB)
		if s.inboundBytes+payloadLen > maxStreamInboundBytes {
			s.isReset = true
			s.readClosed = true
			s.writeClosed = true
			s.err = errors.New("stream inbound buffer exceeded limit")
			s.cancel()
			s.drainUnreadInboundLocked()
			s.cond.Broadcast()
			s.mu.Unlock()

			s.mux.broadcastBackpressure()

			go func() {
				rstFrame := protocol.NewFrame(s.id, protocol.TypeRstStream, protocol.FlagNone, nil)
				_ = s.mux.writeFrame(rstFrame)
				s.mux.removeStream(s.id)
			}()
			return
		}
		s.mu.Unlock()

		// 2. Global Mux inbound budget check with backpressure (released s.mu so stream can be read)
		if !s.mux.acquireInbound(s.id, payloadLen) {
			s.mu.Lock()
			if !s.isReset {
				s.isReset = true
				s.readClosed = true
				s.writeClosed = true
				s.err = errors.New("mux global inbound buffer budget exceeded limit (backpressure timeout)")
				s.cancel()
				s.drainUnreadInboundLocked()
				s.cond.Broadcast()

				s.mux.broadcastBackpressure()

				go func() {
					rstFrame := protocol.NewFrame(s.id, protocol.TypeRstStream, protocol.FlagNone, nil)
					_ = s.mux.writeFrame(rstFrame)
					s.mux.removeStream(s.id)
				}()
			}
			s.mu.Unlock()
			return
		}

		s.mu.Lock()
		if s.readClosed || s.isReset {
			s.mux.releaseInbound(payloadLen)
			s.mu.Unlock()
			return
		}

		cp := make([]byte, payloadLen)
		copy(cp, payload)
		s.inbound = append(s.inbound, cp)
		s.inboundBytes += payloadLen
	} else {
		s.mu.Lock()
	}

	if fin {
		s.readClosed = true
	}
	s.cond.Broadcast()
	s.mu.Unlock()

	s.checkFullClose()
}

// pushInboundHeaders delivers received HEADERS frame payload.
func (s *Stream) pushInboundHeaders(payload []byte) {
	s.touch()
	s.headersOnce.Do(func() {
		cp := make([]byte, len(payload))
		copy(cp, payload)
		s.mu.Lock()
		s.headersReceived = true
		s.mu.Unlock()
		select {
		case s.headersCh <- cp:
		default:
		}
	})
	s.checkFullClose()
}

// markRemoteReset marks stream as reset by remote peer.
func (s *Stream) markRemoteReset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.isReset = true
	s.readClosed = true
	s.writeClosed = true
	s.headersReceived = true
	s.drainUnreadInboundLocked()
	s.err = ErrStreamReset
	s.cancel()
	s.cond.Broadcast()

	s.mux.broadcastBackpressure()
}

func (s *Stream) checkFullClose() {
	s.mu.Lock()
	bothClosed := s.readClosed && s.writeClosed && s.headersReceived
	s.mu.Unlock()

	if bothClosed {
		s.mux.removeStream(s.id)
	}
}
