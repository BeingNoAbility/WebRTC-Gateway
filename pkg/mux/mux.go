package mux

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"webrtc-gateway/pkg/protocol"
)

const (
	DefaultStreamWindow       int64 = 512 * 1024
	MaxStreamInboundBytes     int64 = 2 * 1024 * 1024
	CreditThresholdBytes      int64 = 128 * 1024
	CreditThresholdChunks           = 256
	inboundChunkOverhead      int64 = 64
	DefaultMaxSessionStreams        = 256
	DefaultStreamIdleTimeout        = 150 * time.Second
	DefaultSCTPWriteTimeout         = 30 * time.Second
	SCTPBackpressureHighWater       = 512 * 1024
	SCTPBackpressureLowWater        = 128 * 1024
	acceptQueueSize                 = 256
	frameSendPoolBufSize            = protocol.HeaderSize + protocol.DefaultChunkSize
)

var (
	ErrMuxClosed        = errors.New("mux: closed")
	ErrStreamReset      = errors.New("mux: stream reset")
	ErrStreamClosed     = errors.New("mux: stream closed")
	ErrInvalidLane      = errors.New("mux: invalid data lane")
	ErrSCTPWriteTimeout = errors.New("mux: sctp backpressure write timeout")
)

// DataChannel is the minimal SCTP DataChannel abstraction (satisfied by *webrtc.DataChannel).
type DataChannel interface {
	Send(data []byte) error
	BufferedAmount() uint64
	Close() error
}

// bufferedLowAwareDC is implemented by *webrtc.DataChannel for event-driven SCTP backpressure wakeup.
type bufferedLowAwareDC interface {
	SetBufferedAmountLowThreshold(uint64)
	OnBufferedAmountLow(func())
}

// Config configures a P2PMux V8 multiplexer session.
type Config struct {
	PoolSize            int
	MaxGlobalStreams    int
	MaxSessionStreams   int
	SessionBudget       *MemoryBudget
	GlobalBudget        *MemoryBudget
	GlobalActiveStreams *atomic.Int64
	StreamIdleTimeout   time.Duration
	SCTPWriteTimeout    time.Duration
	RequireAuth         bool // Defense-in-depth: reject data-lane frames before SetAuthenticated(true)
	OnControlFrame      func(f *protocol.Frame)
	OnGoAway            func(code protocol.ErrorCode)
	OnProtocolError     func(err error)
}

type dataLane struct {
	id int

	openMu sync.Mutex // serializes local StreamID allocation + initial HEADERS send on this ordered lane
	sendMu sync.Mutex
	dc     DataChannel
	lowCh  chan struct{}

	mu               sync.Mutex
	lastPeerStreamID uint32
	streams          map[uint32]*Stream
}

func (l *dataLane) signalLow() {
	select {
	case l.lowCh <- struct{}{}:
	default:
	}
}

func (l *dataLane) sendRaw(packet []byte) error {
	l.sendMu.Lock()
	dc := l.dc
	if dc == nil {
		l.sendMu.Unlock()
		return ErrMuxClosed
	}
	err := dc.Send(packet)
	l.sendMu.Unlock()
	return err
}

func (l *dataLane) sendFrame(streamID uint32, frameType, flags uint8, payload []byte) error {
	total := protocol.HeaderSize + len(payload)
	var buf []byte
	var pooled *[]byte
	if total <= frameSendPoolBufSize {
		pooled = sendBufPool.Get().(*[]byte)
		buf = *pooled
		defer sendBufPool.Put(pooled)
	} else {
		buf = make([]byte, total)
	}
	pkt, err := protocol.EncodeInto(buf, streamID, frameType, flags, payload)
	if err != nil {
		return err
	}
	return l.sendRaw(pkt)
}

var sendBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, frameSendPoolBufSize)
		return &b
	},
}

// Mux implements P2PMux V8: 1 chunked control DC (p2pctrl, id=0) + N pre-negotiated ordered
// reliable data lanes (p2p_01..N, default 12) multiplexing arbitrary HTTP & WebSocket streams
// by strictly monotonic StreamID >= 1 (zero slot reuse races, zero DCEP WS negotiation).
type Mux struct {
	cfg           Config
	sessionBudget *MemoryBudget
	globalBudget  *MemoryBudget
	globalStreams *atomic.Int64

	chunkSize     atomic.Int32
	authenticated atomic.Bool
	nextStreamID  atomic.Uint32
	activeStreams atomic.Int32

	ctrlSendMu sync.Mutex
	ctrlDC     DataChannel

	ctrlAsmMu     sync.Mutex
	ctrlAsmType   uint8
	ctrlAsmBuf    []byte
	ctrlAsmChunks int

	lanes []*dataLane

	acceptCh  chan *Stream
	closedCh  chan struct{}
	closed    atomic.Bool
	closeOnce sync.Once
}

func New(cfg Config) *Mux {
	if cfg.PoolSize <= 0 {
		cfg.PoolSize = protocol.DefaultPoolSize
	}
	if cfg.PoolSize > protocol.MaxPoolSize {
		cfg.PoolSize = protocol.MaxPoolSize
	}
	if cfg.MaxSessionStreams <= 0 {
		cfg.MaxSessionStreams = DefaultMaxSessionStreams
	}
	if cfg.StreamIdleTimeout <= 0 {
		cfg.StreamIdleTimeout = DefaultStreamIdleTimeout
	}
	if cfg.SCTPWriteTimeout <= 0 {
		cfg.SCTPWriteTimeout = DefaultSCTPWriteTimeout
	}
	sessBudget := cfg.SessionBudget
	if sessBudget == nil {
		sessBudget = NewMemoryBudget(DefaultSessionMemoryBudget)
	}

	m := &Mux{
		cfg:           cfg,
		sessionBudget: sessBudget,
		globalBudget:  cfg.GlobalBudget,
		globalStreams: cfg.GlobalActiveStreams,
		lanes:         make([]*dataLane, cfg.PoolSize),
		acceptCh:      make(chan *Stream, acceptQueueSize),
		closedCh:      make(chan struct{}),
	}
	m.chunkSize.Store(protocol.DefaultChunkSize)
	for i := 0; i < cfg.PoolSize; i++ {
		m.lanes[i] = &dataLane{
			id:      i,
			lowCh:   make(chan struct{}, 1),
			streams: make(map[uint32]*Stream),
		}
	}
	go m.idleReaperLoop()
	return m
}

func (m *Mux) SetAuthenticated(ok bool) { m.authenticated.Store(ok) }
func (m *Mux) Done() <-chan struct{}    { return m.closedCh }
func (m *Mux) SessionMemUsed() int64    { return m.sessionBudget.Used() }
func (m *Mux) ActiveStreamCount() int   { return int(m.activeStreams.Load()) }

// SetMaxMessageSize monotonically narrows the maximum wire frame payload size from negotiated
// SCTP MMS or peer SETTINGS (never increases chunkSize above an already-narrowed limit).
func (m *Mux) SetMaxMessageSize(mms int) {
	if mms < protocol.MinNegotiatedMaxMsgSize {
		mms = protocol.MinNegotiatedMaxMsgSize
	}
	cs := mms - protocol.HeaderSize
	if cs > protocol.DefaultChunkSize {
		cs = protocol.DefaultChunkSize
	}
	if cs < protocol.MinChunkSize {
		cs = protocol.MinChunkSize
	}
	next := int32(cs) // #nosec G115 -- clamped to [MinChunkSize, DefaultChunkSize]
	for {
		cur := m.chunkSize.Load()
		if next >= cur {
			return
		}
		if m.chunkSize.CompareAndSwap(cur, next) {
			return
		}
	}
}

func (m *Mux) ChunkSize() int {
	cs := int(m.chunkSize.Load())
	if cs < protocol.MinChunkSize {
		return protocol.MinChunkSize
	}
	return cs
}

func (m *Mux) AttachControlDC(dc DataChannel) {
	m.ctrlSendMu.Lock()
	m.ctrlDC = dc
	m.ctrlSendMu.Unlock()
}

func (m *Mux) AttachLaneDC(laneIdx int, dc DataChannel) {
	if laneIdx < 0 || laneIdx >= len(m.lanes) {
		return
	}
	lane := m.lanes[laneIdx]
	if lowDC, ok := dc.(bufferedLowAwareDC); ok {
		lowDC.SetBufferedAmountLowThreshold(SCTPBackpressureLowWater)
		lowDC.OnBufferedAmountLow(func() {
			lane.signalLow()
		})
	}
	lane.sendMu.Lock()
	lane.dc = dc
	lane.sendMu.Unlock()
}

// SendControlFrame sends a control-plane message on p2pctrl (id=0).
// JSON control types automatically chunk across ChunkSize using FlagMore so large AUTH_RESULT /
// CAP_RESP payloads never require truncation even at 4 KiB SCTP MMS.
func (m *Mux) SendControlFrame(frameType uint8, payload []byte) error {
	if m.closed.Load() {
		return ErrMuxClosed
	}
	if !protocol.IsControlFrameType(frameType) {
		return protocol.ErrInvalidFrameType
	}
	if protocol.IsControlJSONFrameType(frameType) && len(payload) > protocol.MaxControlJSONSize {
		return protocol.ErrInvalidPayloadSize
	}

	m.ctrlSendMu.Lock()
	defer m.ctrlSendMu.Unlock()
	if m.ctrlDC == nil || m.closed.Load() {
		return ErrMuxClosed
	}

	cs := m.ChunkSize()
	if !protocol.IsControlJSONFrameType(frameType) || len(payload) <= cs {
		pkt, err := (&protocol.Frame{StreamID: 0, Type: frameType, Flags: protocol.FlagNone, Payload: payload}).Encode()
		if err != nil {
			return err
		}
		return m.ctrlDC.Send(pkt)
	}

	for off := 0; off < len(payload); off += cs {
		end := off + cs
		flags := protocol.FlagMore
		if end >= len(payload) {
			end = len(payload)
			flags = protocol.FlagNone
		}
		pkt, err := (&protocol.Frame{StreamID: 0, Type: frameType, Flags: flags, Payload: payload[off:end]}).Encode()
		if err != nil {
			return err
		}
		if err := m.ctrlDC.Send(pkt); err != nil {
			return err
		}
	}
	return nil
}

func (m *Mux) SendPing(seq uint64) error {
	return m.SendControlFrame(protocol.TypePing, protocol.EncodePingPayload(seq))
}

func (m *Mux) SendGoAway(code protocol.ErrorCode) error {
	return m.SendControlFrame(protocol.TypeGoAway, protocol.EncodeGoAwayPayload(code))
}

func (m *Mux) reportProtocolError(err error) {
	if m.cfg.OnProtocolError != nil {
		m.cfg.OnProtocolError(err)
	}
}

// HandleControlMessage processes inbound frames on p2pctrl (id=0).
func (m *Mux) HandleControlMessage(raw []byte) {
	if m.closed.Load() {
		return
	}
	f, err := protocol.Decode(raw)
	if err != nil || f.StreamID != 0 || !protocol.IsControlFrameType(f.Type) {
		m.reportProtocolError(protocol.ErrInvalidFrameType)
		return
	}

	switch f.Type {
	case protocol.TypePing:
		// M-9: Drop unauthenticated PING reflection to prevent pre-auth amplification.
		if !m.authenticated.Load() {
			return
		}
		_ = m.SendControlFrame(protocol.TypePong, f.Payload)
		return
	case protocol.TypePong:
		return
	case protocol.TypeGoAway:
		code, derr := protocol.DecodeGoAwayPayload(f.Payload)
		if derr != nil {
			m.reportProtocolError(derr)
			return
		}
		if m.cfg.OnGoAway != nil {
			m.cfg.OnGoAway(code)
		}
		return
	}

	// Chunked JSON control message reassembly (supports FlagMore up to MaxControlJSONSize).
	m.ctrlAsmMu.Lock()
	hasMore := (f.Flags & protocol.FlagMore) != 0
	if m.ctrlAsmType == 0 {
		if !hasMore {
			m.ctrlAsmMu.Unlock()
			if len(f.Payload) > protocol.MaxControlJSONSize {
				m.reportProtocolError(protocol.ErrInvalidPayloadSize)
				return
			}
			if m.cfg.OnControlFrame != nil {
				m.cfg.OnControlFrame(f)
			}
			return
		}
		m.ctrlAsmType = f.Type
		m.ctrlAsmChunks = 1
		m.ctrlAsmBuf = append(m.ctrlAsmBuf[:0], f.Payload...)
		if len(m.ctrlAsmBuf) > protocol.MaxControlJSONSize {
			m.ctrlAsmType = 0
			m.ctrlAsmBuf = nil
			m.ctrlAsmChunks = 0
			m.ctrlAsmMu.Unlock()
			m.reportProtocolError(protocol.ErrInvalidPayloadSize)
			return
		}
		m.ctrlAsmMu.Unlock()
		return
	}

	if f.Type != m.ctrlAsmType {
		m.ctrlAsmType = 0
		m.ctrlAsmBuf = nil
		m.ctrlAsmChunks = 0
		m.ctrlAsmMu.Unlock()
		m.reportProtocolError(protocol.ErrInvalidFrameType)
		return
	}
	m.ctrlAsmChunks++
	if m.ctrlAsmChunks > protocol.MaxControlChunks || len(m.ctrlAsmBuf)+len(f.Payload) > protocol.MaxControlJSONSize {
		m.ctrlAsmType = 0
		m.ctrlAsmBuf = nil
		m.ctrlAsmChunks = 0
		m.ctrlAsmMu.Unlock()
		m.reportProtocolError(protocol.ErrInvalidPayloadSize)
		return
	}
	m.ctrlAsmBuf = append(m.ctrlAsmBuf, f.Payload...)
	if hasMore {
		m.ctrlAsmMu.Unlock()
		return
	}
	assembled := append([]byte(nil), m.ctrlAsmBuf...)
	msgType := m.ctrlAsmType
	m.ctrlAsmType = 0
	m.ctrlAsmBuf = nil
	m.ctrlAsmChunks = 0
	m.ctrlAsmMu.Unlock()

	if m.cfg.OnControlFrame != nil {
		m.cfg.OnControlFrame(&protocol.Frame{
			StreamID: 0,
			Type:     msgType,
			Flags:    protocol.FlagNone,
			Payload:  assembled,
		})
	}
}

// HandleLaneMessage dispatches an inbound frame on ordered data lane laneIdx.
func (m *Mux) HandleLaneMessage(laneIdx int, raw []byte) {
	if m.closed.Load() || laneIdx < 0 || laneIdx >= len(m.lanes) {
		return
	}
	if m.cfg.RequireAuth && !m.authenticated.Load() {
		m.reportProtocolError(errors.New("mux: data-lane frame before authentication"))
		return
	}
	f, err := protocol.Decode(raw)
	if err != nil || f.StreamID == 0 || !protocol.IsStreamFrameType(f.Type) {
		m.reportProtocolError(protocol.ErrInvalidFrameType)
		return
	}

	lane := m.lanes[laneIdx]
	switch f.Type {
	case protocol.TypeHeaders:
		m.handleLaneHeaders(lane, f)
	case protocol.TypeData:
		m.handleLaneData(lane, f)
	case protocol.TypeWSText, protocol.TypeWSBinary:
		m.handleLaneWSChunk(lane, f)
	case protocol.TypeWSClose:
		m.handleLaneWSClose(lane, f)
	case protocol.TypeCredit:
		m.handleLaneCredit(lane, f)
	case protocol.TypeReset:
		m.handleLaneReset(lane, f)
	}
}

func (m *Mux) tryAcquireStreamSlot() bool {
	if m.activeStreams.Add(1) > int32(m.cfg.MaxSessionStreams) { // #nosec G115 -- MaxSessionStreams bounded
		m.activeStreams.Add(-1)
		return false
	}
	if m.globalStreams != nil && m.cfg.MaxGlobalStreams > 0 {
		if m.globalStreams.Add(1) > int64(m.cfg.MaxGlobalStreams) {
			m.globalStreams.Add(-1)
			m.activeStreams.Add(-1)
			return false
		}
	}
	return true
}

func (m *Mux) releaseStreamSlot() {
	m.activeStreams.Add(-1)
	if m.globalStreams != nil && m.cfg.MaxGlobalStreams > 0 {
		m.globalStreams.Add(-1)
	}
}

func (m *Mux) handleLaneHeaders(lane *dataLane, f *protocol.Frame) {
	lane.mu.Lock()
	st, exists := lane.streams[f.StreamID]
	if exists {
		// Either multi-frame continuation of an assembling header block, OR response headers on a client-opened stream.
		lane.mu.Unlock()
		st.onInboundHeaders(f.Flags, f.Payload)
		return
	}
	// New peer-initiated stream: StreamID MUST strictly advance on this ordered lane (eliminates C-4!).
	if f.StreamID <= lane.lastPeerStreamID {
		lane.mu.Unlock()
		_ = lane.sendFrame(f.StreamID, protocol.TypeReset, protocol.FlagNone, protocol.EncodeResetPayload(protocol.ErrCodeProtocolError))
		return
	}
	if !m.tryAcquireStreamSlot() {
		lane.mu.Unlock()
		_ = lane.sendFrame(f.StreamID, protocol.TypeReset, protocol.FlagNone, protocol.EncodeResetPayload(protocol.ErrCodeRefusedStream))
		return
	}
	lane.lastPeerStreamID = f.StreamID
	st = newStream(m, lane, f.StreamID, false)
	lane.streams[f.StreamID] = st
	lane.mu.Unlock()

	st.onInboundHeaders(f.Flags, f.Payload)
}

func (m *Mux) lookupStream(lane *dataLane, streamID uint32) (*Stream, bool) {
	lane.mu.Lock()
	st := lane.streams[streamID]
	alreadyClosed := st == nil && streamID <= lane.lastPeerStreamID
	lane.mu.Unlock()
	return st, alreadyClosed
}

func (m *Mux) handleLaneData(lane *dataLane, f *protocol.Frame) {
	st, alreadyClosed := m.lookupStream(lane, f.StreamID)
	if st == nil {
		if !alreadyClosed {
			_ = lane.sendFrame(f.StreamID, protocol.TypeReset, protocol.FlagNone, protocol.EncodeResetPayload(protocol.ErrCodeProtocolError))
		}
		return
	}
	st.onInboundData(f.Payload, (f.Flags&protocol.FlagFin) != 0)
}

func (m *Mux) handleLaneWSChunk(lane *dataLane, f *protocol.Frame) {
	st, alreadyClosed := m.lookupStream(lane, f.StreamID)
	if st == nil {
		if !alreadyClosed {
			_ = lane.sendFrame(f.StreamID, protocol.TypeReset, protocol.FlagNone, protocol.EncodeResetPayload(protocol.ErrCodeProtocolError))
		}
		return
	}
	st.onInboundWSChunk(f.Type, (f.Flags&protocol.FlagMsgFin) != 0, f.Payload)
}

func (m *Mux) handleLaneWSClose(lane *dataLane, f *protocol.Frame) {
	st, alreadyClosed := m.lookupStream(lane, f.StreamID)
	if st == nil {
		if !alreadyClosed {
			_ = lane.sendFrame(f.StreamID, protocol.TypeReset, protocol.FlagNone, protocol.EncodeResetPayload(protocol.ErrCodeProtocolError))
		}
		return
	}
	st.onInboundWSClose(f.Payload)
}

func (m *Mux) handleLaneCredit(lane *dataLane, f *protocol.Frame) {
	st, _ := m.lookupStream(lane, f.StreamID)
	if st == nil {
		return
	}
	delta := binary.BigEndian.Uint32(f.Payload)
	st.onInboundCredit(delta)
}

func (m *Mux) handleLaneReset(lane *dataLane, f *protocol.Frame) {
	st, _ := m.lookupStream(lane, f.StreamID)
	if st == nil {
		return
	}
	code := protocol.ErrorCode(binary.BigEndian.Uint32(f.Payload))
	st.onPeerReset(code)
}

func (m *Mux) HandleLaneClose(laneIdx int) {
	if laneIdx < 0 || laneIdx >= len(m.lanes) {
		return
	}
	lane := m.lanes[laneIdx]
	lane.sendMu.Lock()
	lane.dc = nil
	lane.sendMu.Unlock()
	lane.signalLow()

	lane.mu.Lock()
	victims := make([]*Stream, 0, len(lane.streams))
	for _, st := range lane.streams {
		victims = append(victims, st)
	}
	lane.streams = make(map[uint32]*Stream)
	lane.mu.Unlock()

	for _, st := range victims {
		st.abortLocal(ErrMuxClosed)
	}
}

func (m *Mux) deliverAcceptedStream(st *Stream) {
	select {
	case m.acceptCh <- st:
	case <-m.closedCh:
		st.abortLocal(ErrMuxClosed)
	default:
		st.Reset(protocol.ErrCodeRefusedStream)
	}
}

// AcceptStream waits for the next peer-opened stream whose headers have finished assembling.
func (m *Mux) AcceptStream(ctx context.Context) (*Stream, error) {
	select {
	case st := <-m.acceptCh:
		return st, nil
	case <-m.closedCh:
		return nil, ErrMuxClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// OpenStream opens a client-initiated HTTP or WebSocket stream on the least-loaded data lane
// using a strictly monotonic StreamID (zero slot blocking, zero HoL across N lanes).
func (m *Mux) OpenStream(headersRaw []byte, hasBody bool) (*Stream, error) {
	if m.closed.Load() {
		return nil, ErrMuxClosed
	}
	if len(headersRaw) == 0 || len(headersRaw) > protocol.MaxTotalHeadersSize {
		return nil, protocol.ErrInvalidPayloadSize
	}
	if !m.tryAcquireStreamSlot() {
		return nil, protocol.ErrInvalidPayloadSize
	}

	// Pick the least-loaded lane with an attached DC (prefers idle lanes & lowest SCTP backlog -> zero HoL).
	var chosen *dataLane
	bestLoad := int(^uint(0) >> 1)
	var bestBuf uint64 = ^uint64(0)
	for _, lane := range m.lanes {
		lane.sendMu.Lock()
		dc := lane.dc
		lane.sendMu.Unlock()
		if dc == nil {
			continue
		}
		bufAmt := dc.BufferedAmount()
		lane.mu.Lock()
		load := len(lane.streams)
		lane.mu.Unlock()
		if load < bestLoad || (load == bestLoad && bufAmt < bestBuf) {
			bestLoad = load
			bestBuf = bufAmt
			chosen = lane
			if load == 0 && bufAmt == 0 {
				break
			}
		}
	}
	if chosen == nil {
		m.releaseStreamSlot()
		return nil, ErrMuxClosed
	}

	// Serialize local StreamID allocation + initial HEADERS emission per ordered lane so concurrent
	// OpenStream calls on the same lane ALWAYS arrive at the peer in strictly increasing StreamID order!
	chosen.openMu.Lock()
	streamID := m.nextStreamID.Add(1)
	st := newStream(m, chosen, streamID, true)
	chosen.mu.Lock()
	chosen.streams[streamID] = st
	chosen.mu.Unlock()

	err := st.WriteHeaders(headersRaw, !hasBody)
	chosen.openMu.Unlock()
	if err != nil {
		st.abortLocal(err)
		return nil, err
	}
	return st, nil
}

func (m *Mux) idleReaperLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.closedCh:
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-m.cfg.StreamIdleTimeout)
			var stale []*Stream
			for _, lane := range m.lanes {
				lane.mu.Lock()
				for _, st := range lane.streams {
					if st.isIdleBefore(cutoff) {
						stale = append(stale, st)
					}
				}
				lane.mu.Unlock()
			}
			for _, st := range stale {
				st.Reset(protocol.ErrCodeCancel)
			}
		}
	}
}

// Close shuts down the Mux, aborts all active streams immediately (releasing all memory budgets),
// and closes attached DataChannels.
func (m *Mux) Close() {
	m.closeOnce.Do(func() {
		m.closed.Store(true)
		close(m.closedCh)

		m.ctrlSendMu.Lock()
		cdc := m.ctrlDC
		m.ctrlDC = nil
		m.ctrlSendMu.Unlock()
		if cdc != nil {
			_ = cdc.Close()
		}

		for i, lane := range m.lanes {
			lane.sendMu.Lock()
			dc := lane.dc
			lane.dc = nil
			lane.sendMu.Unlock()
			lane.signalLow()
			if dc != nil {
				_ = dc.Close()
			}
			m.HandleLaneClose(i)
		}

		// Drain any unaccepted streams sitting in acceptCh.
		for {
			select {
			case st := <-m.acceptCh:
				st.abortLocal(ErrMuxClosed)
			default:
				return
			}
		}
	})
}
