package mux

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"webrtc-gateway/pkg/protocol"
)

const (
	DefaultMaxGlobalStreams  = 512
	DefaultStreamIdleTimeout = 60 * time.Second
	GoAwayGenThreshold       = uint32(0xFFFF0000)
)

// Config configures a P2PMux V7 DC-Pool Mux.
type Config struct {
	PoolSize            int
	MaxWSConns          int
	MaxGlobalStreams    int
	SessionMemLimit     int64
	GlobalBudget        *MemoryBudget
	GlobalActiveStreams *atomic.Int64
	StreamIdleTimeout   time.Duration
	OnControlFrame      func(f *protocol.Frame)
	OnGoAway            func(code protocol.ErrorCode)
}

// dcSlot represents one ordered reliable DataChannel (either an HTTP pool slot 0..31 or a dynamic "p2pws" DC).
type dcSlot struct {
	mux       *Mux
	isWS      bool
	lowNotify chan struct{}

	sendMu sync.Mutex // serializes wire frames on this DC and protects gen transition ordering

	mu            sync.Mutex
	dc            DataChannel
	active        *Stream
	lastRemoteGen uint32
	localGen      uint32
	closed        bool
}

func (sl *dcSlot) wireLowWatermark(dc DataChannel) {
	if dc == nil {
		return
	}
	if n, ok := dc.(BufferedAmountLowNotifier); ok {
		n.SetBufferedAmountLowThreshold(sctpLowWaterMark)
		n.OnBufferedAmountLow(func() {
			select {
			case sl.lowNotify <- struct{}{}:
			default:
			}
		})
	}
}

func (sl *dcSlot) sendForStream(s *Stream, wire []byte) error {
	sl.sendMu.Lock()
	defer sl.sendMu.Unlock()
	sl.mu.Lock()
	if sl.closed || sl.active != s {
		sl.mu.Unlock()
		return ErrStreamClosed
	}
	dc := sl.dc
	sl.mu.Unlock()
	if dc == nil {
		return ErrMuxClosed
	}
	return dc.Send(wire)
}

func (sl *dcSlot) detachStream(s *Stream) {
	sl.mu.Lock()
	wasActive := sl.active == s
	if wasActive {
		sl.active = nil
	}
	isWS := sl.isWS
	dc := sl.dc
	sl.mu.Unlock()

	if wasActive && !isWS {
		sl.mux.returnIdlePoolSlot(sl)
	}
	if wasActive && isWS && dc != nil {
		_ = dc.Close()
	}
}

func (sl *dcSlot) closeAndResetActive() {
	sl.mu.Lock()
	sl.closed = true
	active := sl.active
	sl.active = nil
	dc := sl.dc
	sl.mu.Unlock()

	if active != nil {
		active.markRemoteReset()
	}
	if dc != nil {
		_ = dc.Close()
	}
}

func isMonotonicRemoteGen(gen, last uint32, isWS bool) bool {
	if gen == 0 {
		return false
	}
	if isWS {
		return gen == 1 && last == 0
	}
	if last == 0 {
		return true
	}
	if last == 0xFFFFFFFF && gen == 1 {
		return true
	}
	return gen > last
}

// Mux manages 1 Control DC + 32 Reusable Pre-Negotiated HTTP Pool DCs + Dynamic Dedicated WS DCs.
type Mux struct {
	cfg           Config
	ctx           context.Context
	cancel        context.CancelFunc
	sessionBudget *MemoryBudget
	globalBudget  *MemoryBudget
	globalActive  *atomic.Int64
	localActive   atomic.Int64
	activeWS      atomic.Int64

	chunkSize atomic.Int32 // strictly clamped downward via SetMaxMessageSize!

	ctrlMu sync.Mutex
	ctrlDC DataChannel

	poolSlots []*dcSlot
	idleSlots chan *dcSlot

	wsMu    sync.Mutex
	wsSlots map[*dcSlot]struct{}

	// acceptCh is NEVER closed (ctx.Done() wakes AcceptStream) -> zero send-on-closed-channel panic!
	acceptCh   chan *Stream
	goAwayOnce sync.Once
	closeOnce  sync.Once
}

func New(cfg Config) *Mux {
	if cfg.PoolSize <= 0 {
		cfg.PoolSize = protocol.DefaultPoolSize
	}
	if cfg.MaxWSConns <= 0 {
		cfg.MaxWSConns = protocol.DefaultMaxWSConns
	}
	if cfg.MaxGlobalStreams <= 0 {
		cfg.MaxGlobalStreams = DefaultMaxGlobalStreams
	}
	if cfg.SessionMemLimit <= 0 {
		cfg.SessionMemLimit = DefaultSessionMemoryBudget
	}
	if cfg.StreamIdleTimeout <= 0 {
		cfg.StreamIdleTimeout = DefaultStreamIdleTimeout
	}

	ctx, cancel := context.WithCancel(context.Background())
	m := &Mux{
		cfg:           cfg,
		ctx:           ctx,
		cancel:        cancel,
		sessionBudget: NewMemoryBudget(cfg.SessionMemLimit),
		globalBudget:  cfg.GlobalBudget,
		globalActive:  cfg.GlobalActiveStreams,
		poolSlots:     make([]*dcSlot, cfg.PoolSize),
		idleSlots:     make(chan *dcSlot, cfg.PoolSize),
		wsSlots:       make(map[*dcSlot]struct{}),
		acceptCh:      make(chan *Stream, cfg.PoolSize+cfg.MaxWSConns),
	}
	m.chunkSize.Store(protocol.DefaultChunkSize)
	for i := 0; i < cfg.PoolSize; i++ {
		m.poolSlots[i] = &dcSlot{
			mux:       m,
			lowNotify: make(chan struct{}, 1),
		}
	}
	go m.reaperLoop()
	return m
}

func (m *Mux) SessionMemUsed() int64 { return m.sessionBudget.Used() }

func (m *Mux) effectiveChunkSize() int {
	n := int(m.chunkSize.Load())
	if n < protocol.MinChunkSize {
		return protocol.MinChunkSize
	}
	if n > protocol.DefaultChunkSize {
		return protocol.DefaultChunkSize
	}
	return n
}

// SetMaxMessageSize strictly clamps the frame payload ceiling DOWNWARD based on the negotiated
// SCTP maxMessageSize or peer SETTINGS.
func (m *Mux) SetMaxMessageSize(maxMsg int) {
	if maxMsg <= protocol.HeaderSize {
		return
	}
	candInt := maxMsg - protocol.HeaderSize
	if candInt < protocol.MinChunkSize {
		candInt = protocol.MinChunkSize
	}
	if candInt > protocol.DefaultChunkSize {
		candInt = protocol.DefaultChunkSize
	}
	candidate := int32(candInt) // #nosec G115 -- clamped to [MinChunkSize, DefaultChunkSize]
	for {
		cur := m.chunkSize.Load()
		if candidate >= cur {
			return
		}
		if m.chunkSize.CompareAndSwap(cur, candidate) {
			return
		}
	}
}

func (m *Mux) AttachControlDC(dc DataChannel) {
	m.ctrlMu.Lock()
	m.ctrlDC = dc
	m.ctrlMu.Unlock()
}

func (m *Mux) AttachPoolDC(slotIdx int, dc DataChannel) {
	if slotIdx < 0 || slotIdx >= len(m.poolSlots) {
		return
	}
	sl := m.poolSlots[slotIdx]
	sl.mu.Lock()
	wasNil := sl.dc == nil
	sl.dc = dc
	closed := sl.closed
	sl.mu.Unlock()
	sl.wireLowWatermark(dc)
	if wasNil && !closed {
		m.returnIdlePoolSlot(sl)
	}
}

func (m *Mux) returnIdlePoolSlot(sl *dcSlot) {
	select {
	case m.idleSlots <- sl:
	default:
	}
}

// AttachDynamicWSDC registers an incoming browser-opened "p2pws" DataChannel and returns
// its message and close handlers. Enforces MaxWSConns cleanly.
func (m *Mux) AttachDynamicWSDC(dc DataChannel) (onMessage func([]byte), onClose func(), err error) {
	if m.ctx.Err() != nil {
		return nil, nil, ErrMuxClosed
	}
	if m.activeWS.Add(1) > int64(m.cfg.MaxWSConns) {
		m.activeWS.Add(-1)
		_ = dc.Close()
		return nil, nil, ErrTooManyStreams
	}
	sl := &dcSlot{
		mux:       m,
		isWS:      true,
		dc:        dc,
		lowNotify: make(chan struct{}, 1),
	}
	sl.wireLowWatermark(dc)

	m.wsMu.Lock()
	if m.ctx.Err() != nil {
		m.wsMu.Unlock()
		m.activeWS.Add(-1)
		_ = dc.Close()
		return nil, nil, ErrMuxClosed
	}
	m.wsSlots[sl] = struct{}{}
	m.wsMu.Unlock()

	var cleanupOnce sync.Once
	onClose = func() {
		cleanupOnce.Do(func() {
			m.wsMu.Lock()
			delete(m.wsSlots, sl)
			m.wsMu.Unlock()
			sl.closeAndResetActive()
			m.activeWS.Add(-1)
		})
	}
	onMessage = func(raw []byte) {
		m.handleSlotMessage(sl, raw)
	}
	return onMessage, onClose, nil
}

// SendControlFrame encodes and sends a Gen==0 control frame on p2pctrl, bounded by
// min(effectiveChunkSize, MaxControlJSONSize).
func (m *Mux) SendControlFrame(frameType uint8, payload []byte) error {
	if !protocol.IsControlFrameType(frameType) {
		return protocol.ErrInvalidFrameType
	}
	maxCtrl := m.effectiveChunkSize()
	if maxCtrl > protocol.MaxControlJSONSize {
		maxCtrl = protocol.MaxControlJSONSize
	}
	if len(payload) > maxCtrl {
		return protocol.ErrInvalidPayloadSize
	}
	f := &protocol.Frame{Gen: 0, Type: frameType, Flags: protocol.FlagNone, Payload: payload}
	wire, err := f.Encode()
	if err != nil {
		return err
	}
	m.ctrlMu.Lock()
	dc := m.ctrlDC
	m.ctrlMu.Unlock()
	if dc == nil {
		return ErrMuxClosed
	}
	return dc.Send(wire)
}

func (m *Mux) SendGoAway(code protocol.ErrorCode) error {
	var err error
	m.goAwayOnce.Do(func() {
		err = m.SendControlFrame(protocol.TypeGoAway, protocol.EncodeGoAwayPayload(code))
	})
	return err
}

func (m *Mux) HandleControlMessage(raw []byte) {
	if m.ctx.Err() != nil {
		return
	}
	f, err := protocol.Decode(raw)
	if err != nil || !protocol.IsControlFrameType(f.Type) {
		return
	}
	switch f.Type {
	case protocol.TypePing:
		_ = m.SendControlFrame(protocol.TypePong, f.Payload)
	case protocol.TypePong:
	case protocol.TypeGoAway:
		if code, derr := protocol.DecodeGoAwayPayload(f.Payload); derr == nil {
			if m.cfg.OnGoAway != nil {
				m.cfg.OnGoAway(code)
			}
		}
	default:
		if m.cfg.OnControlFrame != nil {
			m.cfg.OnControlFrame(f)
		}
	}
}

func (m *Mux) HandlePoolMessage(slotIdx int, raw []byte) {
	if slotIdx < 0 || slotIdx >= len(m.poolSlots) {
		return
	}
	m.handleSlotMessage(m.poolSlots[slotIdx], raw)
}

func (m *Mux) handleSlotMessage(sl *dcSlot, raw []byte) {
	if m.ctx.Err() != nil {
		return
	}
	f, err := protocol.Decode(raw)
	if err != nil {
		// On strict wire decode failure (e.g., 0B non-final WS_CONT or CONTINUATION),
		// reset active stream on this slot if present.
		sl.mu.Lock()
		active := sl.active
		sl.mu.Unlock()
		if active != nil {
			active.Reset(protocol.ErrCodeProtocolError)
		}
		return
	}
	if sl.isWS {
		if f.Gen != 1 || !protocol.IsWSDataFrameType(f.Type) {
			sl.mu.Lock()
			active := sl.active
			sl.mu.Unlock()
			if active != nil {
				active.Reset(protocol.ErrCodeProtocolError)
			} else if f.Gen != 0 {
				_ = sl.sendDirectReset(f.Gen, protocol.ErrCodeProtocolError)
			}
			return
		}
	} else {
		if !protocol.IsPoolDataFrameType(f.Type) {
			return
		}
	}

	switch f.Type {
	case protocol.TypeHeaders:
		m.handleSlotHeaders(sl, f)
	case protocol.TypeContinuation:
		m.handleSlotContinuation(sl, f)
	case protocol.TypeData:
		sl.mu.Lock()
		active := sl.active
		sl.mu.Unlock()
		if active == nil || active.gen != f.Gen {
			return // Stale frame from prior generation: O(1) drop.
		}
		_ = active.deliverData(f.Payload, f.Flags&protocol.FlagFin != 0)
	case protocol.TypeCredit:
		delta, derr := protocol.DecodeCreditPayload(f.Payload)
		if derr != nil {
			return
		}
		sl.mu.Lock()
		active := sl.active
		sl.mu.Unlock()
		if active == nil || active.gen != f.Gen {
			return
		}
		if err := active.addSendCredit(delta); err != nil {
			active.Reset(protocol.ErrCodeFlowControlError)
		}
	case protocol.TypeReset:
		_, _ = protocol.DecodeResetPayload(f.Payload)
		sl.mu.Lock()
		active := sl.active
		sl.mu.Unlock()
		if active == nil || active.gen != f.Gen {
			return
		}
		active.markRemoteReset()
	case protocol.TypeWSText, protocol.TypeWSBinary, protocol.TypeWSCont, protocol.TypeWSClose:
		sl.mu.Lock()
		active := sl.active
		sl.mu.Unlock()
		if active == nil || active.gen != f.Gen || !active.isWS {
			return
		}
		_ = active.deliverWSChunk(f.Type, f.Flags&protocol.FlagMsgFin != 0, f.Payload)
	}
}

func (m *Mux) handleSlotHeaders(sl *dcSlot, f *protocol.Frame) {
	sl.mu.Lock()
	if sl.closed {
		sl.mu.Unlock()
		return
	}

	if sl.active != nil {
		active := sl.active
		if active.gen == f.Gen && active.isLocal {
			active.inboundMu.Lock()
			state := active.rxState
			if state == rxIdle {
				active.rxState = rxReceivingHeaders
				active.headerStart = time.Now()
				active.respHeadersHadFin = (f.Flags&protocol.FlagFin != 0)
			}
			active.inboundMu.Unlock()
			sl.mu.Unlock()

			if state != rxIdle {
				// Duplicate HEADERS in RECEIVING_HEADERS / OPEN / HALF_CLOSED -> PROTOCOL_ERROR!
				active.Reset(protocol.ErrCodeProtocolError)
				return
			}
			m.appendHeaderChunk(active, f, true)
			return
		}
		if active.gen == f.Gen {
			// Duplicate HEADERS for same remote stream -> PROTOCOL_ERROR!
			sl.mu.Unlock()
			active.Reset(protocol.ErrCodeProtocolError)
			return
		}
		// V7 strict single-slot FSM: NEVER preempt an active stream on a newer Gen!
		canReject := isMonotonicRemoteGen(f.Gen, sl.lastRemoteGen, sl.isWS)
		sl.mu.Unlock()
		if canReject {
			_ = sl.sendDirectReset(f.Gen, protocol.ErrCodeProtocolError)
		}
		return
	}

	// Slot is IDLE: enforce strictly monotonic remote gen (and Gen == 1 on p2pws).
	if !isMonotonicRemoteGen(f.Gen, sl.lastRemoteGen, sl.isWS) {
		sl.mu.Unlock()
		return
	}
	if !m.tryAcquireStreamSlot() {
		sl.mu.Unlock()
		_ = sl.sendDirectReset(f.Gen, protocol.ErrCodeRefusedStream)
		return
	}
	s := newStream(m, sl, f.Gen, sl.isWS, false, f.Flags&protocol.FlagFin != 0)
	sl.lastRemoteGen = f.Gen
	sl.active = s
	sl.mu.Unlock()

	m.appendHeaderChunk(s, f, false)
}

func (m *Mux) handleSlotContinuation(sl *dcSlot, f *protocol.Frame) {
	sl.mu.Lock()
	active := sl.active
	lastGen := sl.lastRemoteGen
	sl.mu.Unlock()

	if active == nil || active.gen != f.Gen {
		if isMonotonicRemoteGen(f.Gen, lastGen, sl.isWS) {
			_ = sl.sendDirectReset(f.Gen, protocol.ErrCodeProtocolError)
		}
		return
	}

	active.inboundMu.Lock()
	state := active.rxState
	active.inboundMu.Unlock()
	if state != rxReceivingHeaders {
		// Orphan CONTINUATION outside RECEIVING_HEADERS -> PROTOCOL_ERROR!
		active.Reset(protocol.ErrCodeProtocolError)
		return
	}

	m.appendHeaderChunk(active, f, active.isLocal)
}

func (m *Mux) appendHeaderChunk(s *Stream, f *protocol.Frame, isResponse bool) {
	s.touch()
	now := time.Now()
	cost := int64(len(f.Payload)) + PerChunkMemOverhead

	s.inboundMu.Lock()
	if s.purged || s.rxState != rxReceivingHeaders {
		s.inboundMu.Unlock()
		return
	}
	if !s.headerStart.IsZero() && now.Sub(s.headerStart) > HeaderAssemblyTimeout {
		s.inboundMu.Unlock()
		s.Reset(protocol.ErrCodeProtocolError)
		return
	}
	// Max 1 HEADERS + MaxContinuationFrames (16) = 17 chunks total; total payload <= 64 KiB.
	if s.headerBytes+len(f.Payload) > protocol.MaxTotalHeadersSize || len(s.headerChunks) >= 1+protocol.MaxContinuationFrames {
		s.inboundMu.Unlock()
		s.Reset(protocol.ErrCodeFrameSizeError)
		return
	}
	if s.inboundBytes+cost > MaxStreamInboundBytes {
		s.inboundMu.Unlock()
		s.Reset(protocol.ErrCodeFrameSizeError)
		return
	}
	if !m.tryReserveMemory(cost) {
		s.inboundMu.Unlock()
		s.Reset(protocol.ErrCodeRefusedStream)
		return
	}
	cp := make([]byte, len(f.Payload))
	copy(cp, f.Payload)
	s.headerChunks = append(s.headerChunks, cp)
	s.headerBytes += len(cp)
	s.inboundBytes += cost
	s.reservedBudget += cost

	if f.Flags&protocol.FlagMoreHeaders != 0 {
		s.inboundMu.Unlock()
		return
	}

	assembled := make([]byte, 0, s.headerBytes)
	for _, ch := range s.headerChunks {
		assembled = append(assembled, ch...)
	}
	headerReserved := int64(s.headerBytes) + int64(len(s.headerChunks))*PerChunkMemOverhead
	s.headerChunks = nil
	s.headerBytes = 0
	// Release temporary chunk assembly budget once assembled into a single compact slice;
	// reserve only the assembled size + 64B until stream closes if server-side request headers.
	releaseNow := headerReserved
	if !isResponse {
		s.reqHeadersRaw = assembled
		keep := int64(len(assembled)) + PerChunkMemOverhead
		if keep < headerReserved {
			releaseNow = headerReserved - keep
		} else {
			releaseNow = 0
		}
	}
	s.inboundBytes -= releaseNow
	s.reservedBudget -= releaseNow

	if isResponse {
		if s.respHeadersHadFin {
			s.readClosed = true
			s.rxState = rxHalfClosed
		} else {
			s.rxState = rxOpen
		}
	} else {
		if s.headersHadFin {
			s.readClosed = true
			s.rxState = rxHalfClosed
		} else {
			s.rxState = rxOpen
		}
	}
	s.inboundMu.Unlock()

	if releaseNow > 0 {
		m.releaseMemory(releaseNow)
	}

	if isResponse {
		s.deliverResponseHeaders(assembled)
		if s.respHeadersHadFin {
			s.signalRead()
		}
		return
	}

	// Non-blocking delivery to acceptCh so DC receive goroutine NEVER stalls!
	select {
	case m.acceptCh <- s:
	default:
		s.Reset(protocol.ErrCodeRefusedStream)
	}
}

func (sl *dcSlot) sendDirectReset(gen uint32, code protocol.ErrorCode) error {
	f := &protocol.Frame{Gen: gen, Type: protocol.TypeReset, Flags: protocol.FlagNone, Payload: protocol.EncodeResetPayload(code)}
	wire, err := f.Encode()
	if err != nil {
		return err
	}
	sl.sendMu.Lock()
	defer sl.sendMu.Unlock()
	sl.mu.Lock()
	dc := sl.dc
	sl.mu.Unlock()
	if dc == nil {
		return ErrMuxClosed
	}
	return dc.Send(wire)
}

// AcceptStream waits for the next peer-initiated HTTP or WS stream.
func (m *Mux) AcceptStream(ctx context.Context) (*Stream, error) {
	select {
	case s := <-m.acceptCh:
		return s, nil
	case <-m.ctx.Done():
		return nil, ErrMuxClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// OpenHTTPStream acquires an idle HTTP pool DC, starts a new generation, and sends HEADERS.
func (m *Mux) OpenHTTPStream(ctx context.Context, headerTLV []byte, fin bool) (*Stream, error) {
	if len(headerTLV) > protocol.MaxTotalHeadersSize {
		return nil, protocol.ErrTLVTooLarge
	}
	var sl *dcSlot
	select {
	case sl = <-m.idleSlots:
	case <-m.ctx.Done():
		return nil, ErrMuxClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	if !m.tryAcquireStreamSlot() {
		m.returnIdlePoolSlot(sl)
		return nil, ErrTooManyStreams
	}

	sl.mu.Lock()
	if sl.closed || sl.dc == nil {
		sl.mu.Unlock()
		m.releaseStreamSlot()
		return nil, ErrMuxClosed
	}
	sl.localGen++
	if sl.localGen == 0 {
		sl.localGen = 1
	}
	gen := sl.localGen
	s := newStream(m, sl, gen, false, true, false)
	sl.active = s
	sl.mu.Unlock()

	if gen >= GoAwayGenThreshold {
		go func() { _ = m.SendGoAway(protocol.ErrCodeGoingAway) }()
	}

	if err := s.WriteHeaders(headerTLV, fin); err != nil {
		s.Reset(protocol.ErrCodeInternalError)
		return nil, err
	}
	return s, nil
}

// OpenWSStream binds a client-opened dedicated WS DataChannel and sends its upgrade HEADERS.
// Enforces MaxWSConns identically to AttachDynamicWSDC.
func (m *Mux) OpenWSStream(ctx context.Context, wsDC DataChannel, headerTLV []byte) (*Stream, func([]byte), func(), error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, nil, nil, ctx.Err()
	}
	if m.ctx.Err() != nil {
		return nil, nil, nil, ErrMuxClosed
	}
	if len(headerTLV) > protocol.MaxTotalHeadersSize {
		return nil, nil, nil, protocol.ErrTLVTooLarge
	}
	if m.activeWS.Add(1) > int64(m.cfg.MaxWSConns) {
		m.activeWS.Add(-1)
		return nil, nil, nil, ErrTooManyStreams
	}
	if !m.tryAcquireStreamSlot() {
		m.activeWS.Add(-1)
		return nil, nil, nil, ErrTooManyStreams
	}
	sl := &dcSlot{
		mux:       m,
		isWS:      true,
		dc:        wsDC,
		localGen:  1,
		lowNotify: make(chan struct{}, 1),
	}
	sl.wireLowWatermark(wsDC)
	s := newStream(m, sl, 1, true, true, false)
	sl.active = s

	m.wsMu.Lock()
	if m.ctx.Err() != nil {
		m.wsMu.Unlock()
		m.releaseStreamSlot()
		m.activeWS.Add(-1)
		return nil, nil, nil, ErrMuxClosed
	}
	m.wsSlots[sl] = struct{}{}
	m.wsMu.Unlock()

	var once sync.Once
	onClose := func() {
		once.Do(func() {
			m.wsMu.Lock()
			delete(m.wsSlots, sl)
			m.wsMu.Unlock()
			sl.closeAndResetActive()
			m.activeWS.Add(-1)
		})
	}
	onMsg := func(raw []byte) {
		m.handleSlotMessage(sl, raw)
	}
	if err := s.WriteHeaders(headerTLV, false); err != nil {
		s.Reset(protocol.ErrCodeInternalError)
		onClose()
		return nil, nil, nil, err
	}
	return s, onMsg, onClose, nil
}

func (m *Mux) tryReserveMemory(n int64) bool {
	if !m.sessionBudget.TryReserve(n) {
		return false
	}
	if m.globalBudget != nil && !m.globalBudget.TryReserve(n) {
		m.sessionBudget.Release(n)
		return false
	}
	return true
}

func (m *Mux) releaseMemory(n int64) {
	m.sessionBudget.Release(n)
	if m.globalBudget != nil {
		m.globalBudget.Release(n)
	}
}

func (m *Mux) tryAcquireStreamSlot() bool {
	if m.globalActive != nil {
		if m.globalActive.Add(1) > int64(m.cfg.MaxGlobalStreams) {
			m.globalActive.Add(-1)
			return false
		}
	}
	m.localActive.Add(1)
	return true
}

func (m *Mux) releaseStreamSlot() {
	m.localActive.Add(-1)
	if m.globalActive != nil {
		m.globalActive.Add(-1)
	}
}

func (m *Mux) reaperLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-ticker.C:
			timeout := m.cfg.StreamIdleTimeout
			for _, sl := range m.poolSlots {
				sl.mu.Lock()
				s := sl.active
				sl.mu.Unlock()
				if s == nil {
					continue
				}
				if s.isHeaderAssemblyTimedOut(now) {
					s.Reset(protocol.ErrCodeProtocolError)
				} else if !s.IsLongLived() && now.Sub(s.LastActivity()) > timeout {
					s.Reset(protocol.ErrCodeCancel)
				}
			}
			m.wsMu.Lock()
			var wsList []*Stream
			for sl := range m.wsSlots {
				sl.mu.Lock()
				if sl.active != nil {
					wsList = append(wsList, sl.active)
				}
				sl.mu.Unlock()
			}
			m.wsMu.Unlock()
			for _, s := range wsList {
				if s.isHeaderAssemblyTimedOut(now) {
					s.Reset(protocol.ErrCodeProtocolError)
				} else if !s.IsLongLived() && now.Sub(s.LastActivity()) > timeout {
					s.Reset(protocol.ErrCodeCancel)
				}
			}
		}
	}
}

// Close tears down all active streams and releases 100% of MemoryBudget cleanly.
func (m *Mux) Close() {
	m.closeOnce.Do(func() {
		m.cancel()
		for _, sl := range m.poolSlots {
			sl.closeAndResetActive()
		}
		m.wsMu.Lock()
		wsList := make([]*dcSlot, 0, len(m.wsSlots))
		for sl := range m.wsSlots {
			wsList = append(wsList, sl)
		}
		m.wsSlots = make(map[*dcSlot]struct{})
		m.wsMu.Unlock()
		for _, sl := range wsList {
			sl.closeAndResetActive()
		}
		m.ctrlMu.Lock()
		ctrl := m.ctrlDC
		m.ctrlDC = nil
		m.ctrlMu.Unlock()
		if ctrl != nil {
			_ = ctrl.Close()
		}
	})
}
