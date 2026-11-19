package mux

import (
	"context"
	"encoding/binary"
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

	// DefaultStreamWindow is the sliding window size for ghost stream defense and closed stream pruning.
	// 65536 IDs comfortably supports high concurrency (up to 32k active concurrent streams) while preventing ghost stream resurrection.
	DefaultStreamWindow uint32 = 65536

	// DefaultMaxInboundBytes is the global inbound buffer budget for a Mux (128MB).
	DefaultMaxInboundBytes int64 = 128 * 1024 * 1024
	// DefaultInboundHighWaterMark is the threshold at which incoming DATA frames wait for consumer drainage (96MB).
	DefaultInboundHighWaterMark int64 = 96 * 1024 * 1024
	// DefaultInboundLowWaterMark is the threshold at which paused incoming DATA frames resume (64MB).
	DefaultInboundLowWaterMark int64 = 64 * 1024 * 1024
	// DefaultInboundBackpressureTimeout is how long an incoming DATA frame waits for buffer drain before resetting the stream.
	DefaultInboundBackpressureTimeout = 3 * time.Second

	// DefaultMaxOrphanBytesPerStream is the maximum memory buffered for orphan frames of a single stream (64KB).
	DefaultMaxOrphanBytesPerStream = 64 * 1024
	// DefaultMaxGlobalOrphanBytes is the total global orphan frame memory cap (256KB).
	DefaultMaxGlobalOrphanBytes = 256 * 1024
	// DefaultOrphanFrameTTL is the maximum time an orphan frame is held awaiting HEADERS (500ms).
	DefaultOrphanFrameTTL = 500 * time.Millisecond
)

type pendingOrphan struct {
	frame     *protocol.Frame
	arrivedAt time.Time
}

// Transport defines the underlying packet transport interface (e.g. *webrtc.DataChannel).
type Transport interface {
	Send(data []byte) error
	BufferedAmount() uint64
	SetBufferedAmountLowThreshold(th uint64)
	OnBufferedAmountLow(f func())
}

// Mux manages multiplexed logical streams over dual Transports (p2pcontrol + p2pdata) or a single legacy Transport.
type Mux struct {
	controlTransport Transport
	dataTransport    Transport
	isClient         bool
	nextStreamID     uint32
	maxPeerStreamID  uint32
	closedStreams    map[uint32]struct{}

	mu       sync.RWMutex
	streams  map[uint32]*Stream
	acceptCh chan *Stream

	writeMu       sync.Mutex
	controlBpMu   sync.Mutex
	controlBpCond *sync.Cond
	dataBpMu      sync.Mutex
	dataBpCond    *sync.Cond
	closed        bool
	closedCh      chan struct{}

	maxInboundBytes       int64
	inboundHighWaterMark  int64
	inboundLowWaterMark   int64
	inboundBackpressureTo time.Duration

	inboundBytes  atomic.Int64
	inboundBpMu   sync.Mutex
	inboundBpCond *sync.Cond

	// Orphan data frame buffer (DATA arriving before HEADERS on p2pdata)
	orphanMu         sync.Mutex
	pendingOrphans   map[uint32][]*pendingOrphan
	orphanBytesTotal int

	pingSeq      atomic.Uint64
	pingsMu      sync.Mutex
	pendingPings map[uint64]chan struct{}
	closeMu      sync.Once
}

// NewDualMux creates a new Mux operating with dedicated control and data transports.
// isClient: true if visitor browser (allocates odd stream IDs), false if agent (allocates even stream IDs).
func NewDualMux(controlTransport, dataTransport Transport, isClient bool) *Mux {
	m := &Mux{
		controlTransport:      controlTransport,
		dataTransport:         dataTransport,
		isClient:              isClient,
		streams:               make(map[uint32]*Stream),
		closedStreams:         make(map[uint32]struct{}),
		acceptCh:              make(chan *Stream, 1024),
		closedCh:              make(chan struct{}),
		maxInboundBytes:       DefaultMaxInboundBytes,
		inboundHighWaterMark:  DefaultInboundHighWaterMark,
		inboundLowWaterMark:   DefaultInboundLowWaterMark,
		inboundBackpressureTo: DefaultInboundBackpressureTimeout,
		pendingPings:          make(map[uint64]chan struct{}),
		pendingOrphans:        make(map[uint32][]*pendingOrphan),
	}

	m.controlBpCond = sync.NewCond(&m.controlBpMu)
	m.dataBpCond = sync.NewCond(&m.dataBpMu)
	m.inboundBpCond = sync.NewCond(&m.inboundBpMu)

	if isClient {
		m.nextStreamID = 1
	} else {
		m.nextStreamID = 2
	}

	if controlTransport != nil {
		controlTransport.SetBufferedAmountLowThreshold(DefaultBufferedLowWaterMark)
		controlTransport.OnBufferedAmountLow(func() {
			m.controlBpMu.Lock()
			m.controlBpCond.Broadcast()
			m.controlBpMu.Unlock()
		})
	}

	if dataTransport != nil {
		dataTransport.SetBufferedAmountLowThreshold(DefaultBufferedLowWaterMark)
		dataTransport.OnBufferedAmountLow(func() {
			m.dataBpMu.Lock()
			m.dataBpCond.Broadcast()
			m.dataBpMu.Unlock()
		})
	}

	go m.reaperLoop()

	return m
}

// NewMux creates a new Mux over a single Transport (convenience wrapper).
func NewMux(transport Transport, isClient bool) *Mux {
	return NewDualMux(transport, nil, isClient)
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
		if _, isClosed := m.closedStreams[frame.StreamID]; isClosed {
			m.mu.Unlock()
			return nil
		}
		if m.maxPeerStreamID > DefaultStreamWindow && frame.StreamID < m.maxPeerStreamID-DefaultStreamWindow {
			m.mu.Unlock()
			return nil
		}

		// If this is an orphan DATA frame on dual-channel mux (arriving on p2pdata before HEADERS on p2pcontrol)
		if m.dataTransport != nil && (frame.Type == protocol.TypeData || frame.Type == protocol.TypeWSData) {
			m.mu.Unlock()
			m.bufferOrphanFrame(frame)
			return nil
		}

		if frame.Type == protocol.TypeRstStream {
			m.mu.Unlock()
			m.dropOrphans(frame.StreamID)
			return nil
		}

		// In single-transport or standard header arrival, initiating frames: TypeHeaders, TypeData (single-transport), TypeWSConnect
		if frame.Type != protocol.TypeHeaders && frame.Type != protocol.TypeData && frame.Type != protocol.TypeWSConnect {
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

		// Flush initiating payload
		if frame.Type == protocol.TypeHeaders {
			s.pushInboundHeaders(frame.Payload)
		} else if frame.Type == protocol.TypeData || frame.Type == protocol.TypeWSConnect {
			fin := (frame.Flags & protocol.FlagFin) != 0
			s.pushInboundData(frame.Payload, fin)
		}

		// Flush any orphan data frames that arrived before HEADERS/WSConnect
		orphans := m.takeOrphanFrames(frame.StreamID)
		for _, of := range orphans {
			fin := (of.frame.Flags & protocol.FlagFin) != 0
			s.pushInboundData(of.frame.Payload, fin)
		}

		return nil
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
		// Reply immediately with Pong, echoing the incoming payload (including sequence number)
		pong := protocol.NewFrame(0, protocol.TypePong, protocol.FlagNone, frame.Payload)
		return m.writeFrame(pong)
	case protocol.TypePong:
		if len(frame.Payload) >= 8 {
			seq := binary.BigEndian.Uint64(frame.Payload[:8])
			m.pingsMu.Lock()
			ch, ok := m.pendingPings[seq]
			m.pingsMu.Unlock()
			if ok {
				select {
				case ch <- struct{}{}:
				default:
				}
				return nil
			}
		}
	}
	return nil
}

// Ping measures RTT over the DataChannel with an 8-byte sequence ID to prevent crosstalk.
func (m *Mux) Ping(ctx context.Context) (time.Duration, error) {
	seq := m.pingSeq.Add(1)
	ch := make(chan struct{}, 1)

	m.pingsMu.Lock()
	m.pendingPings[seq] = ch
	m.pingsMu.Unlock()

	defer func() {
		m.pingsMu.Lock()
		delete(m.pendingPings, seq)
		m.pingsMu.Unlock()
	}()

	payload := make([]byte, 8)
	binary.BigEndian.PutUint64(payload, seq)

	start := time.Now()
	pingFrame := protocol.NewFrame(0, protocol.TypePing, protocol.FlagNone, payload)
	if err := m.writeFrame(pingFrame); err != nil {
		return 0, err
	}

	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(5 * time.Second):
		return 0, ErrPingTimeout
	case <-ch:
		return time.Since(start), nil
	}
}

// DefaultStreamIdleTimeout defines how long an inactive non-WebSocket stream may linger before cleanup.
const DefaultStreamIdleTimeout = 5 * time.Minute

func (m *Mux) reaperLoop() {
	idleTicker := time.NewTicker(30 * time.Second)
	defer idleTicker.Stop()

	orphanTicker := time.NewTicker(100 * time.Millisecond)
	defer orphanTicker.Stop()

	for {
		select {
		case <-m.closedCh:
			return
		case <-idleTicker.C:
			m.reapIdleStreams()
		case <-orphanTicker.C:
			m.orphanMu.Lock()
			hasOrphans := len(m.pendingOrphans) > 0
			m.orphanMu.Unlock()
			if hasOrphans {
				m.reapExpiredOrphans()
			}
		}
	}
}

func (m *Mux) reapExpiredOrphans() {
	now := time.Now()
	var expiredStreamIDs []uint32

	m.orphanMu.Lock()
	for streamID, orphans := range m.pendingOrphans {
		if len(orphans) > 0 && now.Sub(orphans[0].arrivedAt) > DefaultOrphanFrameTTL {
			expiredStreamIDs = append(expiredStreamIDs, streamID)
		}
	}
	for _, sid := range expiredStreamIDs {
		orphans := m.pendingOrphans[sid]
		delete(m.pendingOrphans, sid)
		for _, o := range orphans {
			m.orphanBytesTotal -= len(o.frame.Payload)
		}
	}
	if m.orphanBytesTotal < 0 {
		m.orphanBytesTotal = 0
	}
	m.orphanMu.Unlock()

	for _, sid := range expiredStreamIDs {
		m.mu.Lock()
		if m.closedStreams == nil {
			m.closedStreams = make(map[uint32]struct{})
		}
		m.closedStreams[sid] = struct{}{}
		m.mu.Unlock()

		rst := protocol.NewFrame(sid, protocol.TypeRstStream, protocol.FlagNone, nil)
		_ = m.writeFrame(rst)
	}
}

// bufferOrphanFrame stores an orphan DATA/WS_DATA frame awaiting HEADERS on dual-channel mux.
func (m *Mux) bufferOrphanFrame(f *protocol.Frame) {
	m.orphanMu.Lock()
	defer m.orphanMu.Unlock()

	frameLen := len(f.Payload)
	// Check global cap (256KB)
	if m.orphanBytesTotal+frameLen > DefaultMaxGlobalOrphanBytes {
		return
	}

	streamOrphans := m.pendingOrphans[f.StreamID]
	streamBytes := 0
	for _, o := range streamOrphans {
		streamBytes += len(o.frame.Payload)
	}
	// Check per-stream cap (64KB)
	if streamBytes+frameLen > DefaultMaxOrphanBytesPerStream {
		return
	}

	m.pendingOrphans[f.StreamID] = append(streamOrphans, &pendingOrphan{
		frame:     f,
		arrivedAt: time.Now(),
	})
	m.orphanBytesTotal += frameLen
}

func (m *Mux) takeOrphanFrames(streamID uint32) []*pendingOrphan {
	m.orphanMu.Lock()
	defer m.orphanMu.Unlock()

	orphans, ok := m.pendingOrphans[streamID]
	if !ok {
		return nil
	}
	delete(m.pendingOrphans, streamID)
	for _, o := range orphans {
		m.orphanBytesTotal -= len(o.frame.Payload)
	}
	if m.orphanBytesTotal < 0 {
		m.orphanBytesTotal = 0
	}
	return orphans
}

func (m *Mux) dropOrphans(streamID uint32) {
	_ = m.takeOrphanFrames(streamID)
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

func (m *Mux) broadcastBackpressure() {
	if m.controlBpCond != nil {
		m.controlBpMu.Lock()
		m.controlBpCond.Broadcast()
		m.controlBpMu.Unlock()
	}
	if m.dataBpCond != nil {
		m.dataBpMu.Lock()
		m.dataBpCond.Broadcast()
		m.dataBpMu.Unlock()
	}
}

// writeFrame writes an encoded frame to the appropriate transport (control vs data) with backpressure flow control.
// Control frames (PING, PONG, RST, HEADERS, stream 0) route to controlTransport and bypass data backpressure,
// preventing Head-of-Line (HoL) blocking when large data streams saturate the buffer.
func (m *Mux) writeFrame(f *protocol.Frame) error {
	isBulkData := f.Type == protocol.TypeData || f.Type == protocol.TypeWSData

	var targetTransport Transport
	var bpMu *sync.Mutex
	var bpCond *sync.Cond

	if isBulkData && m.dataTransport != nil {
		targetTransport = m.dataTransport
		bpMu = &m.dataBpMu
		bpCond = m.dataBpCond
	} else if isBulkData {
		targetTransport = m.controlTransport
		bpMu = &m.controlBpMu
		bpCond = m.controlBpCond
	} else {
		// Control frame: routed over controlTransport, bypasses backpressure
		targetTransport = m.controlTransport
		bpMu = nil
		bpCond = nil
	}

	if targetTransport == nil {
		return ErrMuxClosed
	}

	if bpCond != nil {
		bpMu.Lock()
		for targetTransport.BufferedAmount() > DefaultBufferedHighWaterMark {
			m.mu.RLock()
			closed := m.closed
			m.mu.RUnlock()
			if closed {
				bpMu.Unlock()
				return ErrMuxClosed
			}

			s := m.getStream(f.StreamID)
			if s == nil {
				bpMu.Unlock()
				return ErrStreamClosed
			}
			select {
			case <-s.Context().Done():
				bpMu.Unlock()
				return s.Context().Err()
			default:
			}

			bpCond.Wait()

			m.mu.RLock()
			closed = m.closed
			m.mu.RUnlock()
			if closed {
				bpMu.Unlock()
				return ErrMuxClosed
			}

			s = m.getStream(f.StreamID)
			if s == nil {
				bpMu.Unlock()
				return ErrStreamClosed
			}
			select {
			case <-s.Context().Done():
				bpMu.Unlock()
				return s.Context().Err()
			default:
			}
		}
		bpMu.Unlock()
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

	return targetTransport.Send(encoded)
}

func (m *Mux) removeStream(id uint32) {
	m.mu.Lock()
	delete(m.streams, id)
	if m.closedStreams == nil {
		m.closedStreams = make(map[uint32]struct{})
	}
	m.closedStreams[id] = struct{}{}
	// Prune old closed streams older than high-water mark window to prevent unbounded memory growth
	if m.maxPeerStreamID > DefaultStreamWindow {
		threshold := m.maxPeerStreamID - DefaultStreamWindow
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

		if m.controlBpCond != nil {
			m.controlBpMu.Lock()
			m.controlBpCond.Broadcast()
			m.controlBpMu.Unlock()
		}

		if m.dataBpCond != nil {
			m.dataBpMu.Lock()
			m.dataBpCond.Broadcast()
			m.dataBpMu.Unlock()
		}

		m.inboundBpMu.Lock()
		m.inboundBpCond.Broadcast()
		m.inboundBpMu.Unlock()

		m.orphanMu.Lock()
		m.pendingOrphans = make(map[uint32][]*pendingOrphan)
		m.orphanBytesTotal = 0
		m.orphanMu.Unlock()

		m.pingsMu.Lock()
		for seq, ch := range m.pendingPings {
			close(ch)
			delete(m.pendingPings, seq)
		}
		m.pingsMu.Unlock()

		m.writeMu.Lock()
		m.writeMu.Unlock()
	})
	return nil
}

// SetInboundLimits configures custom inbound memory budget, high/low watermarks, and backpressure timeout.
func (m *Mux) SetInboundLimits(max, high, low int64, timeout time.Duration) {
	m.inboundBpMu.Lock()
	defer m.inboundBpMu.Unlock()
	m.maxInboundBytes = max
	m.inboundHighWaterMark = high
	m.inboundLowWaterMark = low
	m.inboundBackpressureTo = timeout
	m.inboundBpCond.Broadcast()
}

// InboundBytes returns the current total unread inbound buffered payload bytes across all streams.
func (m *Mux) InboundBytes() int64 {
	return m.inboundBytes.Load()
}

// acquireInbound attempts to account for incoming payload bytes against the Mux global budget.
// If the buffer is above high-water mark, it waits on inboundBpCond for up to inboundBackpressureTo.
// If the wait times out or the budget exceeds maxInboundBytes, it returns false.
func (m *Mux) acquireInbound(streamID uint32, n int) bool {
	if n <= 0 {
		return true
	}

	delta := int64(n)
	m.inboundBpMu.Lock()
	high := m.inboundHighWaterMark
	if high <= 0 {
		high = DefaultInboundHighWaterMark
	}
	max := m.maxInboundBytes
	if max <= 0 {
		max = DefaultMaxInboundBytes
	}
	timeout := m.inboundBackpressureTo
	if timeout <= 0 {
		timeout = DefaultInboundBackpressureTimeout
	}
	low := m.inboundLowWaterMark
	if low <= 0 {
		low = DefaultInboundLowWaterMark
	}

	curr := m.inboundBytes.Add(delta)
	if curr <= high {
		m.inboundBpMu.Unlock()
		return true
	}

	// Immediate hard ceiling breach
	if curr > max {
		m.inboundBytes.Add(-delta)
		m.inboundBpMu.Unlock()
		return false
	}

	// Between high watermark and max: pause incoming frame consumption to backpressure SCTP socket
	if m.inboundBytes.Load() > low {
		timer := time.AfterFunc(timeout, func() {
			m.inboundBpMu.Lock()
			m.inboundBpCond.Broadcast()
			m.inboundBpMu.Unlock()
		})
		deadline := time.Now().Add(timeout)
		for m.inboundBytes.Load() > low && time.Now().Before(deadline) {
			m.mu.RLock()
			closed := m.closed
			m.mu.RUnlock()
			if closed {
				timer.Stop()
				m.inboundBytes.Add(-delta)
				m.inboundBpMu.Unlock()
				return false
			}
			s := m.getStream(streamID)
			if s == nil {
				timer.Stop()
				m.inboundBytes.Add(-delta)
				m.inboundBpMu.Unlock()
				return false
			}
			m.inboundBpCond.Wait()
		}
		timer.Stop()

		// Backpressure timeout: if backlog is still above low watermark after timeout expires,
		// slow client / stalled consumer failed to drain -> fuse and return false to trigger stream reset
		if m.inboundBytes.Load() > low {
			m.inboundBytes.Add(-delta)
			m.inboundBpMu.Unlock()
			return false
		}
	}

	if m.inboundBytes.Load() > max {
		m.inboundBytes.Add(-delta)
		m.inboundBpMu.Unlock()
		return false
	}

	m.inboundBpMu.Unlock()
	return true
}

// releaseInbound returns consumed or discarded bytes back to the Mux budget and signals blocked frames.
func (m *Mux) releaseInbound(n int) {
	if n <= 0 {
		return
	}
	newVal := m.inboundBytes.Add(-int64(n))
	if newVal < 0 {
		m.inboundBytes.Store(0)
		newVal = 0
	}
	m.inboundBpMu.Lock()
	low := m.inboundLowWaterMark
	if low <= 0 {
		low = DefaultInboundLowWaterMark
	}
	if newVal <= low {
		m.inboundBpCond.Broadcast()
	}
	m.inboundBpMu.Unlock()
}
