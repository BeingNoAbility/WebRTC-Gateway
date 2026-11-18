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
	transport       Transport
	isClient        bool
	nextStreamID    uint32
	maxPeerStreamID uint32
	closedStreams   map[uint32]struct{}

	mu       sync.RWMutex
	streams  map[uint32]*Stream
	acceptCh chan *Stream

	writeMu  sync.Mutex
	bpMu     sync.Mutex
	bpCond   *sync.Cond
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
		transport:     transport,
		isClient:      isClient,
		streams:       make(map[uint32]*Stream),
		closedStreams: make(map[uint32]struct{}),
		acceptCh:      make(chan *Stream, 64),
		closedCh:      make(chan struct{}),
		pongCh:        make(chan struct{}, 1),
	}

	if isClient {
		m.nextStreamID = 1
	} else {
		m.nextStreamID = 2
	}

	m.bpCond = sync.NewCond(&m.bpMu)

	// Configure transport backpressure low threshold
	transport.SetBufferedAmountLowThreshold(DefaultBufferedLowWaterMark)
	transport.OnBufferedAmountLow(func() {
		m.bpMu.Lock()
		m.bpCond.Broadcast()
		m.bpMu.Unlock()
	})

	go m.reaperLoop()

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
		// Guard against 32-bit stream ID overflow / wrap-around (F-10)
		base := uint32(2)
		if m.isClient {
			base = 1
		}
		id = base
		for {
			if _, exists := m.streams[id]; !exists {
				break
			}
			id += 2
			if id >= 0xFFFF0000 {
				return nil, errors.New("mux: stream ID space exhausted")
			}
		}
		atomic.StoreUint32(&m.nextStreamID, id+2)
		delete(m.closedStreams, id)
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
			// Validate parity: Client allocates odd IDs, Agent allocates even IDs
			isExpectedPeerID := (m.isClient && frame.StreamID%2 == 0) || (!m.isClient && frame.StreamID%2 == 1)
			if !isExpectedPeerID {
				return nil
			}

			m.mu.Lock()
			if m.closed {
				m.mu.Unlock()
				return ErrMuxClosed
			}

			// Peer wraparound detection: if peer was near the top of the ID space and now sends low IDs (F-10)
			if m.maxPeerStreamID >= 0xFFFF0000 && frame.StreamID < 0x100000 {
				m.maxPeerStreamID = frame.StreamID
				// Reset tombstoned closed streams from previous epoch
				m.closedStreams = make(map[uint32]struct{})
			}

			// Ghost stream protection: drop frame if stream was already closed or is older than the high-water mark window.
			const streamWindow uint32 = 1024
			if _, isClosed := m.closedStreams[frame.StreamID]; isClosed {
				m.mu.Unlock()
				return nil
			}
			if m.maxPeerStreamID > streamWindow && frame.StreamID < m.maxPeerStreamID-streamWindow {
				m.mu.Unlock()
				return nil
			}

			if frame.StreamID > m.maxPeerStreamID {
				m.maxPeerStreamID = frame.StreamID
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

// DefaultStreamIdleTimeout defines how long an inactive non-WebSocket stream may linger before cleanup.
const DefaultStreamIdleTimeout = 5 * time.Minute

func (m *Mux) reaperLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.closedCh:
			return
		case <-ticker.C:
			m.reapIdleStreams()
		}
	}
}

func (m *Mux) reapIdleStreams() {
	now := time.Now()
	var toReap []*Stream

	m.mu.RLock()
	for _, s := range m.streams {
		// Active WebSocket streams can be quiet; do not prematurely reap them
		if s.InitialType() == protocol.TypeWSConnect || s.InitialType() == protocol.TypeWSData {
			continue
		}
		if now.Sub(s.LastActive()) > DefaultStreamIdleTimeout {
			toReap = append(toReap, s)
		}
	}
	m.mu.RUnlock()

	for _, s := range toReap {
		_ = s.Reset()
	}
}

func (m *Mux) getStream(id uint32) *Stream {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.streams[id]
}

const maxPooledFrameSize = protocol.HeaderSize + protocol.RecommendedChunkSize

var frameEncodePool = sync.Pool{
	New: func() any {
		b := make([]byte, maxPooledFrameSize)
		return &b
	},
}

// writeFrame writes an encoded frame to the underlying transport with backpressure flow control.
// High-priority control frames (PING, PONG, RST, HEADERS, stream 0) bypass data buffer backpressure,
// preventing Head-of-Line (HoL) blocking when large data streams saturate the buffer.
func (m *Mux) writeFrame(f *protocol.Frame) error {
	isHighPriority := f.StreamID == 0 ||
		f.Type == protocol.TypePing ||
		f.Type == protocol.TypePong ||
		f.Type == protocol.TypeRstStream ||
		f.Type == protocol.TypeHeaders

	if !isHighPriority {
		m.bpMu.Lock()
		for m.transport.BufferedAmount() > DefaultBufferedHighWaterMark {
			m.mu.RLock()
			closed := m.closed
			m.mu.RUnlock()
			if closed {
				m.bpMu.Unlock()
				return ErrMuxClosed
			}

			if s := m.getStream(f.StreamID); s != nil {
				select {
				case <-s.Context().Done():
					m.bpMu.Unlock()
					return s.Context().Err()
				default:
				}
			}

			m.bpCond.Wait()

			m.mu.RLock()
			closed = m.closed
			m.mu.RUnlock()
			if closed {
				m.bpMu.Unlock()
				return ErrMuxClosed
			}

			if s := m.getStream(f.StreamID); s != nil {
				select {
				case <-s.Context().Done():
					m.bpMu.Unlock()
					return s.Context().Err()
				default:
				}
			}
		}
		m.bpMu.Unlock()
	}

	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	m.mu.RLock()
	closed := m.closed
	m.mu.RUnlock()
	if closed {
		return ErrMuxClosed
	}

	totalLen := protocol.HeaderSize + len(f.Payload)
	var encoded []byte
	var pooledBuf *[]byte
	if totalLen <= maxPooledFrameSize {
		pooledBuf = frameEncodePool.Get().(*[]byte)
		defer frameEncodePool.Put(pooledBuf)
		buf := *pooledBuf
		_ = f.EncodeHeader(buf[:protocol.HeaderSize])
		if len(f.Payload) > 0 {
			copy(buf[protocol.HeaderSize:totalLen], f.Payload)
		}
		encoded = buf[:totalLen]
	} else {
		encoded = f.Encode()
	}

	return m.transport.Send(encoded)
}

func (m *Mux) removeStream(id uint32) {
	m.mu.Lock()
	delete(m.streams, id)
	if m.closedStreams == nil {
		m.closedStreams = make(map[uint32]struct{})
	}
	m.closedStreams[id] = struct{}{}
	// Prune old closed streams older than high-water mark window to prevent unbounded memory growth
	const streamWindow uint32 = 1024
	if m.maxPeerStreamID > streamWindow {
		threshold := m.maxPeerStreamID - streamWindow
		for cid := range m.closedStreams {
			if cid < threshold {
				delete(m.closedStreams, cid)
			}
		}
	}
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

		m.bpMu.Lock()
		m.bpCond.Broadcast()
		m.bpMu.Unlock()

		m.writeMu.Lock()
		m.writeMu.Unlock()
	})
	return nil
}
