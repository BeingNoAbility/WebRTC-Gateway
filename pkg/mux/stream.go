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

const (
	DefaultStreamWindowSize int64  = 2 * 1024 * 1024 // 2 MiB initial per-stream credit window
	MaxStreamInboundBytes   int64  = 4 * 1024 * 1024 // 4 MiB hard cap per stream inbound buffer
	PerChunkMemOverhead     int64  = 64              // Fixed per-chunk accounting overhead (defeats 0B/1B heap amplification)
	MaxInboundQueueLen      int    = 4096            // Max queued chunks per stream (HTTP or WS)
	HeaderAssemblyTimeout          = 5 * time.Second // Max wall time in RECEIVING_HEADERS state
	SendWindowTimeout              = 30 * time.Second
	sctpHighWaterMark       uint64 = 512 * 1024
	sctpLowWaterMark        uint64 = 128 * 1024
)

// Explicit 5-state single-slot inbound FSM (V7).
type streamRxState uint8

const (
	rxIdle streamRxState = iota
	rxReceivingHeaders
	rxOpen
	rxHalfClosed
	rxClosed
)

var (
	ErrMuxClosed       = errors.New("mux: multiplexer closed")
	ErrStreamReset     = errors.New("mux: stream reset")
	ErrStreamClosed    = errors.New("mux: stream closed")
	ErrFlowControlWait = errors.New("mux: send window timed out")
	ErrMemoryExhausted = errors.New("mux: inbound memory budget exhausted")
	ErrTooManyStreams  = errors.New("mux: concurrent stream limit reached")
)

var frameBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, protocol.HeaderSize+protocol.DefaultChunkSize)
		return &b
	},
}

// WSChunk represents one streaming WebSocket frame chunk delivered on a dedicated "p2pws" DC.
type WSChunk struct {
	Op      uint8  // TypeWSText, TypeWSBinary, TypeWSCont, or TypeWSClose
	Fin     bool   // true on final chunk of message (FlagMsgFin)
	Payload []byte // chunk payload (for WS_CLOSE: [u16 code][utf8 reason])
}

// DataChannel abstracts a Pion or in-memory ordered reliable WebRTC DataChannel.
type DataChannel interface {
	Send(data []byte) error
	BufferedAmount() uint64
	Close() error
}

// BufferedAmountLowNotifier is optionally implemented by DataChannels (including pion/webrtc)
// to enable event-driven SCTP backpressure without busy polling.
type BufferedAmountLowNotifier interface {
	SetBufferedAmountLowThreshold(uint64)
	OnBufferedAmountLow(func())
}

// Stream represents one active HTTP or WebSocket stream bound to a dedicated DataChannel slot
// and monotonic per-DC generation (`gen >= 1`, strictly `1` on `p2pws`).
type Stream struct {
	gen               uint32
	isWS              bool
	isLocal           bool
	slot              *dcSlot
	mux               *Mux
	headersHadFin     bool
	respHeadersHadFin bool

	ctx         context.Context
	cancel      context.CancelFunc
	writeCtx    context.Context
	writeCancel context.CancelFunc

	// Write state (strictly serialized by writeMu; Reset() cancels writeCancel WITHOUT taking writeMu!)
	writeMu     sync.Mutex
	writeClosed bool
	sendWindow  int64
	sendWinMu   sync.Mutex
	sendWinCh   chan struct{}

	// Inbound state (all budget reserve/release, rxState & purged checks strictly under inboundMu!)
	inboundMu       sync.Mutex
	rxState         streamRxState
	headerStart     time.Time
	headerChunks    [][]byte
	headerBytes     int
	inbound         [][]byte
	inboundOff      int
	inboundBytes    int64
	reservedBudget  int64
	recvWindowRem   int64
	consumedForCred int64
	readClosed      bool
	purged          bool
	readNotify      chan struct{}
	wsQueue         []WSChunk
	wsNotify        chan struct{}

	respHeadersCh   chan []byte
	respHeadersOnce sync.Once
	reqHeadersRaw   []byte

	resetOnce  sync.Once
	detachOnce sync.Once
	resetErr   atomic.Value // error
	longLived  atomic.Bool
	lastActive atomic.Int64 // unix nanos
}

func newStream(m *Mux, slot *dcSlot, gen uint32, isWS, isLocal, headersHadFin bool) *Stream {
	ctx, cancel := context.WithCancel(m.ctx)
	wctx, wcancel := context.WithCancel(ctx)
	initRx := rxReceivingHeaders
	var hStart time.Time
	if isLocal {
		initRx = rxIdle // waiting for peer's response HEADERS
	} else {
		hStart = time.Now()
	}
	s := &Stream{
		gen:           gen,
		isWS:          isWS,
		isLocal:       isLocal,
		slot:          slot,
		mux:           m,
		headersHadFin: headersHadFin,
		ctx:           ctx,
		cancel:        cancel,
		writeCtx:      wctx,
		writeCancel:   wcancel,
		sendWindow:    DefaultStreamWindowSize,
		sendWinCh:     make(chan struct{}, 1),
		rxState:       initRx,
		headerStart:   hStart,
		recvWindowRem: DefaultStreamWindowSize,
		readNotify:    make(chan struct{}, 1),
		wsNotify:      make(chan struct{}, 1),
		respHeadersCh: make(chan []byte, 1),
	}
	if headersHadFin {
		s.readClosed = true
	}
	s.touch()
	return s
}

func (s *Stream) IsWS() bool               { return s.isWS }
func (s *Stream) Context() context.Context { return s.ctx }
func (s *Stream) HeadersRaw() []byte       { return s.reqHeadersRaw }

// HasRequestBody reports whether the opening request did NOT terminate at HEADERS.
func (s *Stream) HasRequestBody() bool { return !s.headersHadFin }

func (s *Stream) SetLongLived(v bool) { s.longLived.Store(v) }
func (s *Stream) IsLongLived() bool   { return s.longLived.Load() }

func (s *Stream) touch() { s.lastActive.Store(time.Now().UnixNano()) }

func (s *Stream) LastActivity() time.Time {
	return time.Unix(0, s.lastActive.Load())
}

func (s *Stream) isHeaderAssemblyTimedOut(now time.Time) bool {
	s.inboundMu.Lock()
	timedOut := !s.purged && s.rxState == rxReceivingHeaders && !s.headerStart.IsZero() && now.Sub(s.headerStart) > HeaderAssemblyTimeout
	s.inboundMu.Unlock()
	return timedOut
}

func (s *Stream) getResetErr() error {
	if v := s.resetErr.Load(); v != nil {
		return v.(error)
	}
	return nil
}

func (s *Stream) setResetErr(err error) {
	if err == nil {
		err = ErrStreamReset
	}
	s.resetOnce.Do(func() {
		s.resetErr.Store(err)
	})
}

func (s *Stream) signalRead() {
	select {
	case s.readNotify <- struct{}{}:
	default:
	}
}

func (s *Stream) signalWS() {
	select {
	case s.wsNotify <- struct{}{}:
	default:
	}
}

// purgeInbound atomically and idempotently releases ALL reserved inbound bytes
// (payloads, per-chunk overhead, and header assembly chunks) back to both session
// and global MemoryBudgets.
func (s *Stream) purgeInbound() {
	s.inboundMu.Lock()
	if s.purged {
		s.inboundMu.Unlock()
		return
	}
	s.purged = true
	s.readClosed = true
	s.rxState = rxClosed
	toRelease := s.reservedBudget
	s.reservedBudget = 0
	s.inbound = nil
	s.inboundOff = 0
	s.inboundBytes = 0
	s.headerChunks = nil
	s.headerBytes = 0
	s.wsQueue = nil
	s.inboundMu.Unlock()

	if toRelease > 0 {
		s.mux.releaseMemory(toRelease)
	}
	s.signalRead()
	s.signalWS()
}

// deliverData appends an incoming HTTP DATA frame under strict 5-state FSM, recvWindow,
// queue length, and overhead-aware MemoryBudget control.
func (s *Stream) deliverData(payload []byte, fin bool) error {
	s.touch()
	n := int64(len(payload))

	s.inboundMu.Lock()
	if s.purged {
		s.inboundMu.Unlock()
		return nil
	}
	if s.rxState != rxOpen {
		s.inboundMu.Unlock()
		s.Reset(protocol.ErrCodeProtocolError)
		return errors.New("DATA received in invalid FSM state")
	}
	if s.readClosed {
		s.inboundMu.Unlock()
		s.Reset(protocol.ErrCodeProtocolError)
		return errors.New("DATA received after FIN")
	}
	if n == 0 && !fin {
		s.inboundMu.Unlock()
		s.Reset(protocol.ErrCodeProtocolError)
		return errors.New("0-byte non-FIN DATA frame forbidden")
	}
	if n > 0 {
		if len(s.inbound) >= MaxInboundQueueLen {
			s.inboundMu.Unlock()
			s.Reset(protocol.ErrCodeFlowControlError)
			return ErrFlowControlWait
		}
		cost := n + PerChunkMemOverhead
		if cost > s.recvWindowRem {
			s.inboundMu.Unlock()
			s.Reset(protocol.ErrCodeFlowControlError)
			return ErrFlowControlWait
		}
		if s.inboundBytes+cost > MaxStreamInboundBytes {
			s.inboundMu.Unlock()
			s.Reset(protocol.ErrCodeFlowControlError)
			return ErrMemoryExhausted
		}
		if !s.mux.tryReserveMemory(cost) {
			s.inboundMu.Unlock()
			s.Reset(protocol.ErrCodeRefusedStream)
			return ErrMemoryExhausted
		}
		cp := make([]byte, len(payload))
		copy(cp, payload)
		s.inbound = append(s.inbound, cp)
		s.inboundBytes += cost
		s.reservedBudget += cost
		s.recvWindowRem -= cost
	}
	if fin {
		s.readClosed = true
		s.rxState = rxHalfClosed
	}
	s.inboundMu.Unlock()

	s.signalRead()
	return nil
}

// deliverWSChunk appends a streaming WebSocket sub-frame under strict 5-state FSM,
// recvWindow, queue length, and overhead-aware MemoryBudget control.
func (s *Stream) deliverWSChunk(op uint8, fin bool, payload []byte) error {
	s.touch()
	n := int64(len(payload))
	cost := n + PerChunkMemOverhead

	s.inboundMu.Lock()
	if s.purged {
		s.inboundMu.Unlock()
		return nil
	}
	if s.rxState != rxOpen {
		s.inboundMu.Unlock()
		s.Reset(protocol.ErrCodeProtocolError)
		return errors.New("WS frame received in invalid FSM state")
	}
	if s.readClosed {
		s.inboundMu.Unlock()
		return nil
	}
	if len(s.wsQueue) >= MaxInboundQueueLen {
		s.inboundMu.Unlock()
		s.Reset(protocol.ErrCodeFlowControlError)
		return ErrFlowControlWait
	}
	if op != protocol.TypeWSClose {
		if cost > s.recvWindowRem {
			s.inboundMu.Unlock()
			s.Reset(protocol.ErrCodeFlowControlError)
			return ErrFlowControlWait
		}
	}
	if s.inboundBytes+cost > MaxStreamInboundBytes {
		s.inboundMu.Unlock()
		s.Reset(protocol.ErrCodeFlowControlError)
		return ErrMemoryExhausted
	}
	if !s.mux.tryReserveMemory(cost) {
		s.inboundMu.Unlock()
		s.Reset(protocol.ErrCodeRefusedStream)
		return ErrMemoryExhausted
	}
	cp := make([]byte, len(payload))
	copy(cp, payload)
	s.wsQueue = append(s.wsQueue, WSChunk{Op: op, Fin: fin, Payload: cp})
	s.inboundBytes += cost
	s.reservedBudget += cost
	if op != protocol.TypeWSClose {
		s.recvWindowRem -= cost
	}
	if op == protocol.TypeWSClose {
		s.readClosed = true
		s.rxState = rxHalfClosed
	}
	s.inboundMu.Unlock()

	s.signalWS()
	return nil
}

// Read implements io.Reader for HTTP request/response bodies and automatically issues in-band CREDIT.
func (s *Stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		var creditDelta uint32
		s.inboundMu.Lock()
		if len(s.inbound) > 0 {
			head := s.inbound[0]
			n := copy(p, head[s.inboundOff:])
			s.inboundOff += n
			rel := int64(n)
			if s.inboundOff >= len(head) {
				s.inbound[0] = nil
				s.inbound = s.inbound[1:]
				if len(s.inbound) == 0 {
					s.inbound = nil
				}
				s.inboundOff = 0
				rel += PerChunkMemOverhead
			}
			s.inboundBytes -= rel
			s.reservedBudget -= rel
			s.consumedForCred += rel
			if !s.readClosed && !s.purged && s.consumedForCred >= DefaultStreamWindowSize/4 {
				if s.consumedForCred > 0x7FFFFFFF {
					creditDelta = 0x7FFFFFFF
				} else {
					creditDelta = uint32(s.consumedForCred)
				}
				s.recvWindowRem += s.consumedForCred
				s.consumedForCred = 0
			}
			s.inboundMu.Unlock()

			s.mux.releaseMemory(rel)
			s.touch()
			if creditDelta > 0 {
				s.sendCredit(creditDelta)
			}
			return n, nil
		}
		closed := s.readClosed || s.purged
		s.inboundMu.Unlock()

		if err := s.getResetErr(); err != nil {
			return 0, err
		}
		if closed {
			return 0, io.EOF
		}

		select {
		case <-s.readNotify:
		case <-s.ctx.Done():
			if err := s.getResetErr(); err != nil {
				return 0, err
			}
			return 0, ErrStreamClosed
		}
	}
}

// ReadWSChunk reads the next streaming WebSocket chunk and replenishes flow-control credit
// immediately on consumption, allowing arbitrarily large (16 MiB) WS messages without deadlock.
func (s *Stream) ReadWSChunk(ctx context.Context) (WSChunk, error) {
	for {
		var creditDelta uint32
		s.inboundMu.Lock()
		if len(s.wsQueue) > 0 {
			chunk := s.wsQueue[0]
			s.wsQueue[0] = WSChunk{}
			s.wsQueue = s.wsQueue[1:]
			if len(s.wsQueue) == 0 {
				s.wsQueue = nil
			}
			rel := int64(len(chunk.Payload)) + PerChunkMemOverhead
			s.inboundBytes -= rel
			s.reservedBudget -= rel
			if chunk.Op != protocol.TypeWSClose {
				s.consumedForCred += rel
				if !s.readClosed && !s.purged && s.consumedForCred >= DefaultStreamWindowSize/8 {
					if s.consumedForCred > 0x7FFFFFFF {
						creditDelta = 0x7FFFFFFF
					} else {
						creditDelta = uint32(s.consumedForCred)
					}
					s.recvWindowRem += s.consumedForCred
					s.consumedForCred = 0
				}
			}
			s.inboundMu.Unlock()

			s.mux.releaseMemory(rel)
			s.touch()
			if creditDelta > 0 {
				s.sendCredit(creditDelta)
			}
			return chunk, nil
		}
		closed := s.readClosed || s.purged
		s.inboundMu.Unlock()

		if err := s.getResetErr(); err != nil {
			return WSChunk{}, err
		}
		if closed {
			return WSChunk{}, io.EOF
		}

		select {
		case <-s.wsNotify:
		case <-s.ctx.Done():
			if err := s.getResetErr(); err != nil {
				return WSChunk{}, err
			}
			return WSChunk{}, io.EOF
		case <-ctx.Done():
			return WSChunk{}, ctx.Err()
		}
	}
}

// ReadResponseHeaders waits for the peer's TLV response headers (used by client/Go E2E streams).
func (s *Stream) ReadResponseHeaders(ctx context.Context) ([]byte, error) {
	select {
	case raw := <-s.respHeadersCh:
		return raw, nil
	case <-s.ctx.Done():
		if err := s.getResetErr(); err != nil {
			return nil, err
		}
		return nil, ErrStreamClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Stream) deliverResponseHeaders(raw []byte) {
	cp := make([]byte, len(raw))
	copy(cp, raw)
	s.respHeadersOnce.Do(func() {
		select {
		case s.respHeadersCh <- cp:
		default:
		}
	})
}

func (s *Stream) addSendCredit(delta uint32) error {
	s.sendWinMu.Lock()
	if s.sendWindow+int64(delta) > 0x7FFFFFFF {
		s.sendWinMu.Unlock()
		return errors.New("send window overflow")
	}
	s.sendWindow += int64(delta)
	s.sendWinMu.Unlock()

	select {
	case s.sendWinCh <- struct{}{}:
	default:
	}
	return nil
}

// consumeSendWindow reserves payloadGrant + PerChunkMemOverhead from sendWindow so both ends
// stay 1-to-1 conserved under overhead-aware flow control.
func (s *Stream) consumeSendWindow(ctx context.Context, want int64, allowZeroPayload bool) (int64, error) {
	if want < 0 {
		return 0, nil
	}
	if want == 0 && !allowZeroPayload {
		return 0, nil
	}
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	minNeeded := PerChunkMemOverhead
	if !allowZeroPayload {
		minNeeded = PerChunkMemOverhead + 1
	}

	for {
		if err := s.writeCtx.Err(); err != nil {
			if rerr := s.getResetErr(); rerr != nil {
				return 0, rerr
			}
			return 0, ErrStreamClosed
		}
		if ctx != nil && ctx.Err() != nil {
			return 0, ctx.Err()
		}
		s.sendWinMu.Lock()
		if s.sendWindow >= minNeeded {
			availPayload := s.sendWindow - PerChunkMemOverhead
			take := want
			if take > availPayload {
				take = availPayload
			}
			s.sendWindow -= take + PerChunkMemOverhead
			s.sendWinMu.Unlock()
			return take, nil
		}
		s.sendWinMu.Unlock()

		if timer == nil {
			timer = time.NewTimer(SendWindowTimeout)
		}
		var ctxDone <-chan struct{}
		if ctx != nil {
			ctxDone = ctx.Done()
		}
		select {
		case <-s.sendWinCh:
		case <-s.writeCtx.Done():
			if rerr := s.getResetErr(); rerr != nil {
				return 0, rerr
			}
			return 0, ErrStreamClosed
		case <-ctxDone:
			return 0, ctx.Err()
		case <-timer.C:
			s.Reset(protocol.ErrCodeFlowControlError)
			return 0, ErrFlowControlWait
		}
	}
}

func (s *Stream) waitSCTPBackpressure(ctx context.Context) error {
	var fallback *time.Timer
	defer func() {
		if fallback != nil {
			fallback.Stop()
		}
	}()

	for {
		s.slot.mu.Lock()
		closed := s.slot.closed || s.slot.active != s
		dc := s.slot.dc
		s.slot.mu.Unlock()
		if closed {
			return ErrStreamClosed
		}
		if dc == nil {
			return ErrMuxClosed
		}
		if dc.BufferedAmount() < sctpHighWaterMark {
			return nil
		}

		if fallback == nil {
			fallback = time.NewTimer(50 * time.Millisecond)
		} else {
			fallback.Reset(50 * time.Millisecond)
		}
		var ctxDone <-chan struct{}
		if ctx != nil {
			ctxDone = ctx.Done()
		}
		select {
		case <-s.slot.lowNotify:
			if !fallback.Stop() {
				select {
				case <-fallback.C:
				default:
				}
			}
		case <-fallback.C:
		case <-s.writeCtx.Done():
			return ErrStreamClosed
		case <-ctxDone:
			return ctx.Err()
		}
	}
}

func (s *Stream) sendRawFrame(ctx context.Context, frameType, flags uint8, payload []byte) error {
	if err := s.waitSCTPBackpressure(ctx); err != nil {
		return err
	}
	bufPtr := frameBufPool.Get().(*[]byte)
	buf := *bufPtr
	if len(buf) < protocol.HeaderSize+len(payload) {
		buf = make([]byte, protocol.HeaderSize+len(payload))
		*bufPtr = buf
	}
	wire, err := protocol.EncodeInto(buf, s.gen, frameType, flags, payload)
	if err != nil {
		frameBufPool.Put(bufPtr)
		return err
	}
	err = s.slot.sendForStream(s, wire)
	frameBufPool.Put(bufPtr)
	return err
}

func (s *Stream) sendControlDirect(frameType uint8, payload []byte) error {
	f := &protocol.Frame{Gen: s.gen, Type: frameType, Flags: protocol.FlagNone, Payload: payload}
	wire, err := f.Encode()
	if err != nil {
		return err
	}
	return s.slot.sendForStream(s, wire)
}

func (s *Stream) sendCredit(delta uint32) {
	payload, err := protocol.EncodeCreditPayload(delta)
	if err != nil {
		return
	}
	_ = s.sendControlDirect(protocol.TypeCredit, payload)
}

// WriteHeaders transmits a V7 TLV header block (fragmented via CONTINUATION if > chunkSize).
func (s *Stream) WriteHeaders(tlvBytes []byte, fin bool) error {
	if len(tlvBytes) > protocol.MaxTotalHeadersSize {
		return protocol.ErrTLVTooLarge
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.writeClosed {
		return ErrStreamClosed
	}

	chunkSize := s.mux.effectiveChunkSize()
	if len(tlvBytes) <= chunkSize {
		flags := protocol.FlagNone
		if fin {
			flags |= protocol.FlagFin
		}
		if err := s.sendRawFrame(s.writeCtx, protocol.TypeHeaders, flags, tlvBytes); err != nil {
			return err
		}
		if fin {
			s.writeClosed = true
		}
		s.touch()
		return nil
	}

	chunks := (len(tlvBytes) + chunkSize - 1) / chunkSize
	if chunks-1 > protocol.MaxContinuationFrames {
		return protocol.ErrTLVTooLarge
	}

	off := 0
	for i := 0; i < chunks; i++ {
		end := off + chunkSize
		if end > len(tlvBytes) {
			end = len(tlvBytes)
		}
		isFirst := i == 0
		isLast := i == chunks-1
		var fType, flags uint8
		if isFirst {
			fType = protocol.TypeHeaders
			if fin {
				flags |= protocol.FlagFin
			}
			if !isLast {
				flags |= protocol.FlagMoreHeaders
			}
		} else {
			fType = protocol.TypeContinuation
			if !isLast {
				flags |= protocol.FlagMoreHeaders
			}
		}
		if err := s.sendRawFrame(s.writeCtx, fType, flags, tlvBytes[off:end]); err != nil {
			return err
		}
		off = end
	}
	if fin {
		s.writeClosed = true
	}
	s.touch()
	return nil
}

// Write transmits HTTP body bytes paced by overhead-aware sendWindow and effectiveChunkSize.
func (s *Stream) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.writeClosed {
		return 0, ErrStreamClosed
	}

	written := 0
	for written < len(p) {
		chunkMax := s.mux.effectiveChunkSize()
		rem := len(p) - written
		want := int64(rem)
		if want > int64(chunkMax) {
			want = int64(chunkMax)
		}
		grant, err := s.consumeSendWindow(s.writeCtx, want, false)
		if err != nil {
			return written, err
		}
		end := written + int(grant)
		if err := s.sendRawFrame(s.writeCtx, protocol.TypeData, protocol.FlagNone, p[written:end]); err != nil {
			return written, err
		}
		written = end
		s.touch()
	}
	return written, nil
}

// WriteWSChunk streams a single WebSocket sub-frame chunk (or splits oversized slices
// cleanly across <= effectiveChunkSize frames) with O(1) memory and context cancellation.
func (s *Stream) WriteWSChunk(ctx context.Context, opcode uint8, msgFin bool, payload []byte) error {
	if opcode != protocol.TypeWSText && opcode != protocol.TypeWSBinary && opcode != protocol.TypeWSCont {
		return protocol.ErrInvalidFrameType
	}
	if opcode == protocol.TypeWSCont && !msgFin && len(payload) == 0 {
		return protocol.ErrInvalidPayloadSize
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.writeClosed {
		return ErrStreamClosed
	}

	if len(payload) == 0 {
		if _, err := s.consumeSendWindow(ctx, 0, true); err != nil {
			return err
		}
		flags := protocol.FlagNone
		if msgFin {
			flags = protocol.FlagMsgFin
		}
		if err := s.sendRawFrame(ctx, opcode, flags, nil); err != nil {
			return err
		}
		s.touch()
		return nil
	}

	off := 0
	first := true
	for off < len(payload) {
		chunkMax := s.mux.effectiveChunkSize()
		want := int64(len(payload) - off)
		if want > int64(chunkMax) {
			want = int64(chunkMax)
		}
		grant, err := s.consumeSendWindow(ctx, want, false)
		if err != nil {
			return err
		}
		end := off + int(grant)
		op := protocol.TypeWSCont
		if first {
			op = opcode
			first = false
		}
		flags := protocol.FlagNone
		if msgFin && end == len(payload) {
			flags = protocol.FlagMsgFin
		}
		if err := s.sendRawFrame(ctx, op, flags, payload[off:end]); err != nil {
			return err
		}
		off = end
		s.touch()
	}
	return nil
}

// WriteWSMessage streams a complete WebSocket text or binary message in <= chunkSize chunks.
func (s *Stream) WriteWSMessage(isBinary bool, payload []byte) error {
	op := protocol.TypeWSText
	if isBinary {
		op = protocol.TypeWSBinary
	}
	return s.WriteWSChunk(s.writeCtx, op, true, payload)
}

// WriteWSClose sends a bounded RFC 6455 WS_CLOSE frame cleanly on the dedicated WS DC.
func (s *Stream) WriteWSClose(code uint16, reason string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.writeClosed {
		return nil
	}
	s.writeClosed = true
	payload := protocol.EncodeWSClosePayload(code, reason)
	return s.sendRawFrame(s.writeCtx, protocol.TypeWSClose, protocol.FlagNone, payload)
}

// CloseWrite half-closes the outbound HTTP direction (sending DATA+FIN) without purging inbound.
func (s *Stream) CloseWrite() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.writeClosed {
		return nil
	}
	s.writeClosed = true
	if s.isWS {
		return nil
	}
	return s.sendRawFrame(s.writeCtx, protocol.TypeData, protocol.FlagFin, nil)
}

// Close finishes outbound writes, unconditionally purges all unread inbound memory back to
// GlobalMemoryBudget, and detaches the stream from its slot.
// If the peer has not yet finished sending (readClosed == false), records ErrStreamClosed
// BEFORE purging so any concurrent upload reader (e.g. http.Transport) receives an error
// instead of a spurious clean io.EOF that would commit a truncated chunked request body!
func (s *Stream) Close() error {
	err := s.CloseWrite()
	s.inboundMu.Lock()
	incompleteInbound := !s.readClosed
	s.inboundMu.Unlock()
	if incompleteInbound {
		s.setResetErr(ErrStreamClosed)
	}
	s.purgeInbound()
	s.cancel()
	s.detach()
	return err
}

// Reset aborts the stream immediately WITHOUT blocking on writeMu,
// emits an in-band RESET frame atomically with slot detachment (preventing post-RESET DATA),
// and unconditionally purges all inbound memory.
func (s *Stream) Reset(code protocol.ErrorCode) {
	s.setResetErr(ErrStreamReset)
	s.writeCancel()
	s.cancel()
	s.purgeInbound()
	var wire []byte
	f := &protocol.Frame{
		Gen:     s.gen,
		Type:    protocol.TypeReset,
		Flags:   protocol.FlagNone,
		Payload: protocol.EncodeResetPayload(code),
	}
	wire, _ = f.Encode()
	s.detachOnce.Do(func() {
		s.slot.sendResetAndDetach(s, wire)
		s.mux.releaseStreamSlot()
	})
}

// markRemoteReset handles an incoming RESET frame from the peer.
func (s *Stream) markRemoteReset() {
	s.setResetErr(ErrStreamReset)
	s.writeCancel()
	s.cancel()
	s.purgeInbound()
	s.detach()
}

func (s *Stream) detach() {
	s.detachOnce.Do(func() {
		s.slot.detachStream(s)
		s.mux.releaseStreamSlot()
	})
}
