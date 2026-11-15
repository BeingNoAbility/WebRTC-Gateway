package mux

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"webrtc-gateway/pkg/protocol"
)

var (
	ErrMuxClosed    = errors.New("multiplexer closed")
	ErrPingTimeout  = errors.New("ping timeout")
	ErrStreamExists = errors.New("stream already exists")
)

const (
	// DefaultBufferedHighWaterMark is the threshold above which writes block until buffer drains.
	DefaultBufferedHighWaterMark uint64 = 256 * 1024
	// DefaultBufferedLowWaterMark is the threshold at which blocked writes resume.
	DefaultBufferedLowWaterMark uint64 = 64 * 1024
)

// Transport defines the underlying packet transport interface (e.g. *webrtc.DataChannel).
type Transport interface {
	Send(data []byte) error
	BufferedAmount() uint64
	SetBufferedAmountLowThreshold(th uint64)
	OnBufferedAmountLow(f func())
}

// Mux manages multiplexed logical streams over a single Transport.
type Mux struct {
	transport    Transport
	isClient     bool
	nextStreamID uint32

	mu       sync.RWMutex
	streams  map[uint32]*Stream
	acceptCh chan *Stream

	writeMu  sync.Mutex
	bufCond  *sync.Cond
	closed   bool
	closedCh chan struct{}

	pingMu  sync.Mutex
	pongCh  chan struct{}
	closeMu sync.Once
}

// NewMux creates a new Mux over the given Transport.
// isClient: true if visitor browser (allocates odd stream IDs), false if agent (allocates even stream IDs).
func NewMux(transport Transport, isClient bool) *Mux {
	m := &Mux{
		transport: transport,
		isClient:  isClient,
		streams:   make(map[uint32]*Stream),
		acceptCh:  make(chan *Stream, 64),
		closedCh:  make(chan struct{}),
		pongCh:    make(chan struct{}, 1),
	}

	if isClient {
		m.nextStreamID = 1
	} else {
		m.nextStreamID = 2
	}

	m.bufCond = sync.NewCond(&m.writeMu)

	// Configure transport backpressure low threshold
	transport.SetBufferedAmountLowThreshold(DefaultBufferedLowWaterMark)
	transport.OnBufferedAmountLow(func() {
		m.writeMu.Lock()
		m.bufCond.Broadcast()
		m.writeMu.Unlock()
	})

	return m
}

// OpenStream initiates a new outbound stream.
func (m *Mux) OpenStream() (*Stream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrMuxClosed
	}

	id := atomic.AddUint32(&m.nextStreamID, 2) - 2
	if id >= 0xFFFF0000 {
		// Guard against 32-bit stream ID overflow / wrap-around
		base := uint32(2)
		if m.isClient {
			base = 1
		}
		atomic.StoreUint32(&m.nextStreamID, base+2)
		id = base
		for {
			if _, exists := m.streams[id]; !exists {
				break
			}
			id += 2
		}
	}

	s := newStream(id, m)
	m.streams[id] = s
	return s, nil
}

// AcceptStream waits for and returns the next inbound stream opened by the peer.
func (m *Mux) AcceptStream(ctx context.Context) (*Stream, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.closedCh:
		return nil, ErrMuxClosed
	case s, ok := <-m.acceptCh:
		if !ok {
			return nil, ErrMuxClosed
		}
		return s, nil
	}
}

// HandleIncomingMessage parses and dispatches a raw binary packet received from the Transport.
func (m *Mux) HandleIncomingMessage(data []byte) error {
	frame, err := protocol.DecodeFrameBytes(data)
	if err != nil {
		return fmt.Errorf("decode frame error: %w", err)
	}

	// Stream ID 0 is reserved for connection control
	if frame.StreamID == 0 {
		return m.handleControlFrame(frame)
	}

	m.mu.RLock()
	s, exists := m.streams[frame.StreamID]
	m.mu.RUnlock()

	if !exists {
		// New incoming stream initiated by peer
		if frame.Type == protocol.TypeHeaders || frame.Type == protocol.TypeData || frame.Type == protocol.TypeWSConnect {
			m.mu.Lock()
			if m.closed {
				m.mu.Unlock()
				return ErrMuxClosed
			}
			s = newStream(frame.StreamID, m)
			s.initialType = frame.Type
			m.streams[frame.StreamID] = s

			select {
			case m.acceptCh <- s:
				m.mu.Unlock()
			default:
				// Buffer full, reset stream
				delete(m.streams, frame.StreamID)
				m.mu.Unlock()
				s.Reset()
				return nil
			}
		} else {
			// Frame for unknown/already closed stream, ignore or RST
			return nil
		}
	}

	switch frame.Type {
	case protocol.TypeHeaders:
		s.pushInboundHeaders(frame.Payload)
	case protocol.TypeData:
		fin := (frame.Flags & protocol.FlagFin) != 0
		s.pushInboundData(frame.Payload, fin)
	case protocol.TypeRstStream:
		s.markRemoteReset()
		m.removeStream(frame.StreamID)
	case protocol.TypeWSData, protocol.TypeWSConnect, protocol.TypeWSClose:
		// WebSocket payload frames can be read as standard DATA on the stream
		fin := (frame.Flags & protocol.FlagFin) != 0
		s.pushInboundData(frame.Payload, fin)
	}

	return nil
}

func (m *Mux) handleControlFrame(frame *protocol.Frame) error {
	switch frame.Type {
	case protocol.TypePing:
		// Reply immediately with Pong
		pong := protocol.NewFrame(0, protocol.TypePong, protocol.FlagNone, frame.Payload)
		return m.writeFrame(pong)
	case protocol.TypePong:
		select {
		case m.pongCh <- struct{}{}:
		default:
		}
	}
	return nil
}

// Ping measures RTT over the DataChannel.
func (m *Mux) Ping(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	pingFrame := protocol.NewFrame(0, protocol.TypePing, protocol.FlagNone, nil)
	if err := m.writeFrame(pingFrame); err != nil {
		return 0, err
	}

	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(5 * time.Second):
		return 0, ErrPingTimeout
	case <-m.pongCh:
		return time.Since(start), nil
	}
}

// writeFrame writes an encoded frame to the underlying transport with backpressure flow control.
func (m *Mux) writeFrame(f *protocol.Frame) error {
	encoded := f.Encode()

	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	if m.closed {
		return ErrMuxClosed
	}

	// Backpressure: pause if transport buffer exceeds threshold
	for m.transport.BufferedAmount() > DefaultBufferedHighWaterMark {
		m.bufCond.Wait()
		if m.closed {
			return ErrMuxClosed
		}
	}

	return m.transport.Send(encoded)
}

func (m *Mux) removeStream(id uint32) {
	m.mu.Lock()
	delete(m.streams, id)
	m.mu.Unlock()
}

// Close closes the multiplexer and all active streams.
func (m *Mux) Close() error {
	m.closeMu.Do(func() {
		m.mu.Lock()
		m.closed = true
		close(m.closedCh)

		activeStreams := make([]*Stream, 0, len(m.streams))
		for _, s := range m.streams {
			activeStreams = append(activeStreams, s)
		}
		m.streams = make(map[uint32]*Stream)
		close(m.acceptCh)
		m.mu.Unlock()

		for _, s := range activeStreams {
			s.markResetNoRemove()
		}

		m.writeMu.Lock()
		m.bufCond.Broadcast()
		m.writeMu.Unlock()
	})
	return nil
}
