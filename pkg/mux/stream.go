package mux

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"webrtc-gateway/pkg/protocol"
)

// WSChunk represents one streaming WebSocket sub-frame (WS_TEXT, WS_BINARY, or WS_CLOSE).
type WSChunk struct {
	Op      uint8
	Fin     bool
	Payload []byte
}

type inboundChunk struct {
	op      uint8 // 0 for HTTP DATA, TypeWSText / TypeWSBinary / TypeWSClose for WS
	fin     bool
	payload []byte
	memCost int64
}

// Stream is a single full-duplex HTTP or WebSocket stream multiplexed over one ordered data lane
// by a strictly monotonic StreamID (never reused).
type Stream struct {
	mux      *Mux
	lane     *dataLane
	streamID uint32
	isClient bool

	ctx    context.Context
	cancel context.CancelFunc

	longLived  atomic.Bool
	kind       atomic.Uint32 // immutable once frozen (protocol.StreamKindHTTP or protocol.StreamKindWS)
	lastActive atomic.Int64  // monotonic offset in nanos from createdAt
	createdAt  time.Time

	// Header assembly state (protected by hdrMu).
	hdrMu        sync.Mutex
	hdrBuf       []byte
	hdrMemCost   int64
	hdrChunks    int
	hdrComplete  bool
	hasReqBody   bool
	hdrReadyCh   chan struct{}
	hdrReadyOnce sync.Once
	inWSActiveOp uint8 // non-zero while inside a multi-chunk WS message (prevents interleaving)
	inWSChunks   int   // chunks accumulated in the current streaming WS message (<= MaxWSMessageChunks)

	// Inbound data/WS queue (protected by inMu).
	inMu                sync.Mutex
	inQueue             []inboundChunk
	inPartial           []byte // leftover bytes from partially-read HTTP DATA chunk
	inNotify            chan struct{}
	recvInFlightPayload int64
	recvInFlightMem     int64
	consumedBytes       int64
	consumedChunks      int
	remoteFin           bool
	inErr               error

	// Outbound send window & SCTP serialization (protected by winMu / writeMu).
	winMu      sync.Mutex
	sendWindow int64
	winNotify  chan struct{}

	writeMu     sync.Mutex
	localFin    atomic.Bool
	writeFailed atomic.Bool
	writeClosed bool
	resetSent   bool

	detachOnce sync.Once
}

func newStream(m *Mux, lane *dataLane, streamID uint32, isClient bool) *Stream {
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now()
	s := &Stream{
		mux:        m,
		lane:       lane,
		streamID:   streamID,
		isClient:   isClient,
		ctx:        ctx,
		cancel:     cancel,
		createdAt:  now,
		hdrReadyCh: make(chan struct{}),
		inNotify:   make(chan struct{}, 1),
		sendWindow: DefaultStreamWindow,
		winNotify:  make(chan struct{}, 1),
	}
	s.touch()
	return s
}

func (s *Stream) StreamID() uint32         { return s.streamID }
func (s *Stream) Context() context.Context { return s.ctx }
func (s *Stream) SetLongLived(v bool)      { s.longLived.Store(v) }

func (s *Stream) touch() {
	s.lastActive.Store(int64(time.Since(s.createdAt)))
}

func (s *Stream) isWSMode() bool {
	return s.kind.Load() == uint32(protocol.StreamKindWS)
}

func (s *Stream) isHTTPMode() bool {
	return s.kind.Load() == uint32(protocol.StreamKindHTTP)
}

func (s *Stream) isHeadersComplete() bool {
	s.hdrMu.Lock()
	ready := s.hdrComplete
	s.hdrMu.Unlock()
	return ready
}

func (s *Stream) isStale(now time.Time, hdrTimeout, idleTimeout time.Duration) bool {
	// Unfinished server-inbound HEADERS are bounded by an un-refreshable hard deadline
	// so partial HEADERS frames can never pin stream slots via keepalive frames.
	if !s.isClient && !s.isHeadersComplete() {
		return now.Sub(s.createdAt) >= hdrTimeout
	}
	if s.longLived.Load() {
		return false
	}
	last := s.createdAt.Add(time.Duration(s.lastActive.Load()))
	return last.Before(now.Add(-idleTimeout))
}

func (s *Stream) signalInbound() {
	select {
	case s.inNotify <- struct{}{}:
	default:
	}
}

func (s *Stream) signalWindow() {
	select {
	case s.winNotify <- struct{}{}:
	default:
	}
}

func (s *Stream) HeadersRaw() []byte {
	s.hdrMu.Lock()
	defer s.hdrMu.Unlock()
	return s.hdrBuf
}

// TakeHeadersRaw returns the assembled header block and immediately releases its buffer and memory budget
// so long-lived streams (WebSocket / SSE) do not pin up to 64 KiB for their entire lifetime.
func (s *Stream) TakeHeadersRaw() []byte {
	s.hdrMu.Lock()
	raw := s.hdrBuf
	cost := s.hdrMemCost
	s.hdrBuf = nil
	s.hdrMemCost = 0
	s.hdrMu.Unlock()
	s.releaseBudget(cost)
	return raw
}

func (s *Stream) HasRequestBody() bool {
	s.hdrMu.Lock()
	defer s.hdrMu.Unlock()
	return s.hasReqBody
}

// ReadResponseHeaders waits for assembled response headers (client helper for tests).
func (s *Stream) ReadResponseHeaders(ctx context.Context) ([]byte, error) {
	select {
	case <-s.hdrReadyCh:
		return s.TakeHeadersRaw(), nil
	case <-s.ctx.Done():
		s.inMu.Lock()
		err := s.inErr
		s.inMu.Unlock()
		if err != nil {
			return nil, err
		}
		return nil, ErrStreamReset
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Stream) onInboundHeaders(flags uint8, payload []byte) {
	s.touch()
	s.hdrMu.Lock()
	if s.hdrComplete {
		s.hdrMu.Unlock()
		s.Reset(protocol.ErrCodeProtocolError)
		return
	}
	s.hdrChunks++
	if s.hdrChunks > protocol.MaxHeaderChunks || len(s.hdrBuf)+len(payload) > protocol.MaxTotalHeadersSize {
		s.hdrMu.Unlock()
		s.Reset(protocol.ErrCodeFrameSizeError)
		return
	}
	memCost := int64(len(payload))
	if memCost > 0 {
		if !s.mux.sessionBudget.TryReserve(memCost) {
			s.hdrMu.Unlock()
			s.Reset(protocol.ErrCodeFlowControlError)
			return
		}
		if s.mux.globalBudget != nil && !s.mux.globalBudget.TryReserve(memCost) {
			s.mux.sessionBudget.Release(memCost)
			s.hdrMu.Unlock()
			s.Reset(protocol.ErrCodeFlowControlError)
			return
		}
		s.hdrMemCost += memCost
	}
	s.hdrBuf = append(s.hdrBuf, payload...)
	if (flags & protocol.FlagMore) != 0 {
		s.hdrMu.Unlock()
		return
	}
	if !s.isClient {
		if len(s.hdrBuf) == 0 || (s.hdrBuf[0] != protocol.StreamKindHTTP && s.hdrBuf[0] != protocol.StreamKindWS) {
			s.hdrMu.Unlock()
			s.Reset(protocol.ErrCodeProtocolError)
			return
		}
		s.kind.Store(uint32(s.hdrBuf[0]))
	}
	s.hdrComplete = true
	fin := (flags & protocol.FlagFin) != 0
	s.hasReqBody = !fin
	s.hdrReadyOnce.Do(func() { close(s.hdrReadyCh) })
	s.hdrMu.Unlock()

	if fin {
		s.inMu.Lock()
		s.remoteFin = true
		s.inMu.Unlock()
		s.signalInbound()
	}

	if !s.isClient {
		s.mux.deliverAcceptedStream(s)
	}
}

func (s *Stream) enqueueInbound(op uint8, fin bool, payload []byte, isHTTPFin bool) {
	s.touch()
	plen := int64(len(payload))
	memCost := plen + inboundChunkOverhead

	s.inMu.Lock()
	if s.remoteFin || s.inErr != nil {
		s.inMu.Unlock()
		return
	}
	// Per-stream bounded memory (payload + 64B chunk metadata) AND chunk count cap so
	// zero-byte WS frames or 1-byte DATA frames on a single stream can never exhaust the
	// 32 MiB session budget or 256 MiB global budget.
	if s.recvInFlightMem+memCost > MaxStreamInboundBytes || len(s.inQueue) >= MaxStreamInboundChunks {
		s.inMu.Unlock()
		s.Reset(protocol.ErrCodeFlowControlError)
		return
	}
	// Local heap budget (payload + 64B chunk struct overhead) — decoupled from wire CREDIT!
	if !s.mux.sessionBudget.TryReserve(memCost) {
		s.inMu.Unlock()
		s.Reset(protocol.ErrCodeFlowControlError)
		return
	}
	if s.mux.globalBudget != nil && !s.mux.globalBudget.TryReserve(memCost) {
		s.mux.sessionBudget.Release(memCost)
		s.inMu.Unlock()
		s.Reset(protocol.ErrCodeFlowControlError)
		return
	}

	var cp []byte
	if len(payload) > 0 {
		cp = make([]byte, len(payload))
		copy(cp, payload)
	}
	if len(cp) > 0 || op != 0 {
		s.inQueue = append(s.inQueue, inboundChunk{
			op:      op,
			fin:     fin,
			payload: cp,
			memCost: memCost,
		})
		s.recvInFlightPayload += plen
		s.recvInFlightMem += memCost
	} else {
		// Pure 0-byte HTTP FIN marker: release budget immediately.
		s.mux.sessionBudget.Release(memCost)
		if s.mux.globalBudget != nil {
			s.mux.globalBudget.Release(memCost)
		}
	}
	if isHTTPFin {
		s.remoteFin = true
	}
	s.inMu.Unlock()
	s.signalInbound()
	if isHTTPFin {
		s.maybeDetachIfBothClosed()
	}
}

func (s *Stream) onInboundData(payload []byte, fin bool) {
	if !s.isHeadersComplete() || !s.isHTTPMode() {
		s.Reset(protocol.ErrCodeProtocolError)
		return
	}
	s.enqueueInbound(0, fin, payload, fin)
}

func (s *Stream) onInboundWSChunk(op uint8, fin bool, payload []byte) {
	s.hdrMu.Lock()
	if !s.hdrComplete || !s.isWSMode() {
		s.hdrMu.Unlock()
		s.Reset(protocol.ErrCodeProtocolError)
		return
	}
	if s.inWSActiveOp != 0 && s.inWSActiveOp != op {
		// Interleaved WS_TEXT and WS_BINARY mid-message is a protocol error!
		s.hdrMu.Unlock()
		s.Reset(protocol.ErrCodeProtocolError)
		return
	}
	s.inWSChunks++
	if s.inWSChunks > protocol.MaxWSMessageChunks {
		s.hdrMu.Unlock()
		s.Reset(protocol.ErrCodeFrameSizeError)
		return
	}
	if fin {
		s.inWSActiveOp = 0
		s.inWSChunks = 0
	} else {
		s.inWSActiveOp = op
	}
	s.hdrMu.Unlock()

	s.enqueueInbound(op, fin, payload, false)
}

func (s *Stream) onInboundWSClose(payload []byte) {
	s.hdrMu.Lock()
	ready := s.hdrComplete && s.isWSMode()
	s.inWSActiveOp = 0
	s.inWSChunks = 0
	s.hdrMu.Unlock()
	if !ready {
		s.Reset(protocol.ErrCodeProtocolError)
		return
	}
	s.enqueueInbound(protocol.TypeWSClose, true, payload, true)
}

func (s *Stream) onInboundCredit(delta uint32) {
	// Server-accepted streams MUST finish HEADERS before accepting CREDIT; otherwise
	// incomplete HEADERS frames could be kept alive indefinitely via CREDIT(1).
	if !s.isClient && !s.isHeadersComplete() {
		s.Reset(protocol.ErrCodeProtocolError)
		return
	}
	s.touch()
	s.winMu.Lock()
	if s.sendWindow+int64(delta) > 0x7FFFFFFF {
		s.winMu.Unlock()
		s.Reset(protocol.ErrCodeFlowControlError)
		return
	}
	s.sendWindow += int64(delta)
	s.winMu.Unlock()
	s.signalWindow()
}

func (s *Stream) onPeerReset(code protocol.ErrorCode) {
	s.abortLocal(ErrStreamReset)
}

func (s *Stream) releaseBudget(memCost int64) {
	if memCost <= 0 {
		return
	}
	s.mux.sessionBudget.Release(memCost)
	if s.mux.globalBudget != nil {
		s.mux.globalBudget.Release(memCost)
	}
}

func (s *Stream) flushCreditLocked() uint32 {
	if s.consumedBytes <= 0 || s.remoteFin || s.inErr != nil {
		return 0
	}
	if s.consumedBytes >= CreditThresholdBytes || s.consumedChunks >= CreditThresholdChunks {
		delta := uint32(s.consumedBytes) // #nosec G115 -- bounded by MaxStreamInboundBytes (2 MiB)
		s.consumedBytes = 0
		s.consumedChunks = 0
		return delta
	}
	return 0
}

func (s *Stream) sendCredit(delta uint32) {
	if delta == 0 || s.ctx.Err() != nil {
		return
	}
	payload, err := protocol.EncodeCreditPayload(delta)
	if err != nil {
		return
	}
	_ = s.lane.sendFrame(s.streamID, protocol.TypeCredit, protocol.FlagNone, payload)
}

// Read implements io.Reader for HTTP request/response bodies with pure payload-byte credit replenishment.
func (s *Stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		s.inMu.Lock()
		if len(s.inPartial) > 0 {
			n := copy(p, s.inPartial)
			s.inPartial = s.inPartial[n:]
			if len(s.inPartial) == 0 {
				s.inPartial = nil
			}
			s.inMu.Unlock()
			s.touch()
			return n, nil
		}
		if len(s.inQueue) > 0 {
			ch := s.inQueue[0]
			s.inQueue[0] = inboundChunk{}
			s.inQueue = s.inQueue[1:]
			if len(s.inQueue) == 0 {
				s.inQueue = nil
			}
			plen := int64(len(ch.payload))
			s.recvInFlightPayload -= plen
			s.recvInFlightMem -= ch.memCost
			s.releaseBudget(ch.memCost)
			s.consumedBytes += plen
			s.consumedChunks++
			creditDelta := s.flushCreditLocked()

			n := copy(p, ch.payload)
			if n < len(ch.payload) {
				s.inPartial = ch.payload[n:]
			}
			s.inMu.Unlock()
			s.sendCredit(creditDelta)
			s.touch()
			return n, nil
		}
		if s.inErr != nil {
			err := s.inErr
			s.inMu.Unlock()
			return 0, err
		}
		if s.remoteFin {
			s.inMu.Unlock()
			return 0, io.EOF
		}
		s.inMu.Unlock()

		select {
		case <-s.inNotify:
		case <-s.ctx.Done():
			s.inMu.Lock()
			err := s.inErr
			remFin := s.remoteFin
			s.inMu.Unlock()
			if err != nil {
				return 0, err
			}
			if remFin {
				return 0, io.EOF
			}
			return 0, ErrStreamClosed
		}
	}
}

// ReadWSChunk reads the next streaming WebSocket sub-frame (TypeWSText, TypeWSBinary, or TypeWSClose).
func (s *Stream) ReadWSChunk(ctx context.Context) (WSChunk, error) {
	for {
		s.inMu.Lock()
		if len(s.inQueue) > 0 {
			ch := s.inQueue[0]
			s.inQueue[0] = inboundChunk{}
			s.inQueue = s.inQueue[1:]
			if len(s.inQueue) == 0 {
				s.inQueue = nil
			}
			plen := int64(len(ch.payload))
			s.recvInFlightPayload -= plen
			s.recvInFlightMem -= ch.memCost
			s.releaseBudget(ch.memCost)
			var creditDelta uint32
			if ch.op == protocol.TypeWSText || ch.op == protocol.TypeWSBinary {
				s.consumedBytes += plen
				s.consumedChunks++
				creditDelta = s.flushCreditLocked()
			}
			s.inMu.Unlock()
			s.sendCredit(creditDelta)
			s.touch()
			return WSChunk{Op: ch.op, Fin: ch.fin, Payload: ch.payload}, nil
		}
		if s.inErr != nil {
			err := s.inErr
			s.inMu.Unlock()
			return WSChunk{}, err
		}
		if s.remoteFin {
			s.inMu.Unlock()
			return WSChunk{}, io.EOF
		}
		s.inMu.Unlock()

		select {
		case <-s.inNotify:
		case <-s.ctx.Done():
			return WSChunk{}, ErrStreamClosed
		case <-ctx.Done():
			return WSChunk{}, ctx.Err()
		}
	}
}

// waitSCTPBackpressure waits for lane SCTP buffer to drain below watermark with a strict bounded
// timeout (DefaultSCTPWriteTimeout = 30s) and immediate wake on s.ctx.Done() (fixes H-4 & M-8).
func (s *Stream) waitSCTPBackpressure(ctx context.Context) error {
	s.lane.sendMu.Lock()
	dc := s.lane.dc
	s.lane.sendMu.Unlock()
	if dc == nil {
		return ErrMuxClosed
	}
	if dc.BufferedAmount() < SCTPBackpressureHighWater {
		return nil
	}

	deadline := time.NewTimer(s.mux.cfg.SCTPWriteTimeout)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()

	for {
		s.lane.sendMu.Lock()
		curDC := s.lane.dc
		s.lane.sendMu.Unlock()
		if curDC == nil {
			return ErrMuxClosed
		}
		if curDC.BufferedAmount() <= SCTPBackpressureLowWater {
			return nil
		}
		select {
		case <-s.lane.lowCh:
		case <-poll.C:
		case <-s.ctx.Done():
			return ErrStreamClosed
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return ErrSCTPWriteTimeout
		}
	}
}

func (s *Stream) acquireSendWindow(ctx context.Context, want int) (int, error) {
	if want <= 0 {
		return 0, nil
	}
	for {
		s.winMu.Lock()
		avail := s.sendWindow
		if avail > 0 {
			grant := int64(want)
			if grant > avail {
				grant = avail
			}
			s.sendWindow -= grant
			s.winMu.Unlock()
			return int(grant), nil
		}
		s.winMu.Unlock()

		deadline := time.NewTimer(s.mux.cfg.SCTPWriteTimeout)
		select {
		case <-s.winNotify:
			deadline.Stop()
		case <-s.ctx.Done():
			deadline.Stop()
			return 0, ErrStreamClosed
		case <-ctx.Done():
			deadline.Stop()
			return 0, ctx.Err()
		case <-deadline.C:
			return 0, ErrSCTPWriteTimeout
		}
	}
}

// WriteHeaders sends request or response TLV headers, automatically chunking with FlagMore.
func (s *Stream) WriteHeaders(raw []byte, fin bool) error {
	if len(raw) == 0 || len(raw) > protocol.MaxTotalHeadersSize {
		return protocol.ErrInvalidPayloadSize
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.writeClosed || s.localFin.Load() || s.ctx.Err() != nil {
		return ErrStreamClosed
	}

	cs := s.mux.ChunkSize()
	for off := 0; off < len(raw); off += cs {
		if err := s.waitSCTPBackpressure(s.ctx); err != nil {
			s.writeFailed.Store(true)
			return err
		}
		end := off + cs
		flags := protocol.FlagMore
		if end >= len(raw) {
			end = len(raw)
			flags = protocol.FlagNone
			if fin {
				flags |= protocol.FlagFin
			}
		}
		if err := s.lane.sendFrame(s.streamID, protocol.TypeHeaders, flags, raw[off:end]); err != nil {
			s.writeFailed.Store(true)
			return err
		}
	}
	s.touch()
	if fin {
		s.localFin.Store(true)
		s.maybeDetachIfBothClosed()
	}
	return nil
}

// Write implements io.Writer for HTTP bodies using pure payload-byte flow control.
func (s *Stream) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.writeClosed || s.localFin.Load() || s.ctx.Err() != nil {
		return 0, ErrStreamClosed
	}

	written := 0
	for written < len(p) {
		if err := s.waitSCTPBackpressure(s.ctx); err != nil {
			s.writeFailed.Store(true)
			return written, err
		}
		cs := s.mux.ChunkSize()
		rem := len(p) - written
		if rem > cs {
			rem = cs
		}
		grant, err := s.acquireSendWindow(s.ctx, rem)
		if err != nil {
			s.writeFailed.Store(true)
			return written, err
		}
		if err := s.lane.sendFrame(s.streamID, protocol.TypeData, protocol.FlagNone, p[written:written+grant]); err != nil {
			s.writeFailed.Store(true)
			return written, err
		}
		written += grant
		s.touch()
	}
	return written, nil
}

// WriteWSChunk sends one streaming WebSocket sub-frame (TypeWSText or TypeWSBinary) with pure
// payload-byte flow control and bounded SCTP backpressure.
func (s *Stream) WriteWSChunk(ctx context.Context, op uint8, fin bool, payload []byte) error {
	if (op != protocol.TypeWSText && op != protocol.TypeWSBinary) || !s.isWSMode() {
		return protocol.ErrInvalidFrameType
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.writeClosed || s.localFin.Load() || s.ctx.Err() != nil {
		return ErrStreamClosed
	}

	if len(payload) == 0 {
		if !fin {
			return protocol.ErrInvalidPayloadSize
		}
		if err := s.waitSCTPBackpressure(ctx); err != nil {
			return err
		}
		s.touch()
		return s.lane.sendFrame(s.streamID, op, protocol.FlagMsgFin, nil)
	}

	written := 0
	for written < len(payload) {
		if err := s.waitSCTPBackpressure(ctx); err != nil {
			return err
		}
		cs := s.mux.ChunkSize()
		rem := len(payload) - written
		if rem > cs {
			rem = cs
		}
		grant, err := s.acquireSendWindow(ctx, rem)
		if err != nil {
			return err
		}
		flags := protocol.FlagNone
		if fin && written+grant == len(payload) {
			flags = protocol.FlagMsgFin
		}
		if err := s.lane.sendFrame(s.streamID, op, flags, payload[written:written+grant]); err != nil {
			return err
		}
		written += grant
		s.touch()
	}
	return nil
}

// WriteWSClose sends an RFC 6455 WS_CLOSE sub-frame on this stream.
func (s *Stream) WriteWSClose(code uint16, reason string) error {
	if !s.isWSMode() {
		return protocol.ErrInvalidFrameType
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.writeClosed || s.localFin.Load() || s.ctx.Err() != nil {
		return ErrStreamClosed
	}
	s.localFin.Store(true)
	s.touch()
	err := s.lane.sendFrame(s.streamID, protocol.TypeWSClose, protocol.FlagNone, protocol.EncodeWSClosePayload(code, reason))
	s.maybeDetachIfBothClosed()
	return err
}

// CloseWrite sends a clean HTTP FIN half-close without tearing down inbound reading.
func (s *Stream) CloseWrite() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.writeClosed || s.localFin.Load() {
		return nil
	}
	s.localFin.Store(true)
	if err := s.waitSCTPBackpressure(s.ctx); err != nil {
		s.writeFailed.Store(true)
		return err
	}
	err := s.lane.sendFrame(s.streamID, protocol.TypeData, protocol.FlagFin, nil)
	if err != nil {
		s.writeFailed.Store(true)
	}
	s.maybeDetachIfBothClosed()
	return err
}

// Close finishes the stream cleanly: for healthy sequential HTTP streams sends FIN if not yet sent,
// purges unread inbound buffers, cancels stream context, and detaches from the lane.
// WebSocket streams NEVER emit HTTP TypeData(FlagFin). If another goroutine currently holds writeMu
// (e.g. blocked in Write() on SCTP backpressure/send window) or a prior write already failed, Close()
// aborts immediately via Reset(ErrCodeCancel) instead of stalling for up to 2x SCTPWriteTimeout.
func (s *Stream) Close() error {
	if s.isWSMode() || s.localFin.Load() || s.ctx.Err() != nil {
		s.cancel()
		s.purgeInbound(ErrStreamClosed)
		s.detach()
		s.writeMu.Lock()
		s.localFin.Store(true)
		s.writeClosed = true
		s.writeMu.Unlock()
		return nil
	}

	if s.writeFailed.Load() || !s.writeMu.TryLock() {
		s.Reset(protocol.ErrCodeCancel)
		return nil
	}
	needFin := !s.writeClosed && !s.localFin.Load() && !s.resetSent && !s.isWSMode() && s.ctx.Err() == nil && !s.writeFailed.Load()
	s.localFin.Store(true)
	s.writeClosed = true
	if needFin {
		if err := s.waitSCTPBackpressure(s.ctx); err == nil {
			_ = s.lane.sendFrame(s.streamID, protocol.TypeData, protocol.FlagFin, nil)
		}
	}
	s.writeMu.Unlock()

	s.cancel()
	s.purgeInbound(ErrStreamClosed)
	s.detach()
	return nil
}

// Reset abruptly aborts the stream and notifies the peer via RESET(code).
// Cancels s.ctx BEFORE acquiring s.writeMu so any blocked Write()/waitSCTPBackpressure wakes immediately (H-4)!
func (s *Stream) Reset(code protocol.ErrorCode) {
	s.cancel()
	s.purgeInbound(ErrStreamReset)
	s.detach()

	s.writeMu.Lock()
	alreadySent := s.resetSent
	s.resetSent = true
	s.writeClosed = true
	s.localFin.Store(true)
	s.writeMu.Unlock()

	if !alreadySent {
		_ = s.lane.sendFrame(s.streamID, protocol.TypeReset, protocol.FlagNone, protocol.EncodeResetPayload(code))
	}
}

func (s *Stream) abortLocal(err error) {
	s.cancel()
	s.purgeInbound(err)
	s.detach()

	s.writeMu.Lock()
	s.writeClosed = true
	s.localFin.Store(true)
	s.resetSent = true
	s.writeMu.Unlock()
}

func (s *Stream) purgeInbound(err error) {
	s.hdrMu.Lock()
	hdrCost := s.hdrMemCost
	s.hdrBuf = nil
	s.hdrMemCost = 0
	s.hdrMu.Unlock()
	s.releaseBudget(hdrCost)

	s.inMu.Lock()
	if s.inErr == nil {
		s.inErr = err
	}
	queued := s.inQueue
	s.inQueue = nil
	s.inPartial = nil
	s.recvInFlightPayload = 0
	s.recvInFlightMem = 0
	s.consumedBytes = 0
	s.consumedChunks = 0
	s.inMu.Unlock()

	for _, ch := range queued {
		s.releaseBudget(ch.memCost)
	}
	s.signalInbound()
	s.signalWindow()
}

func (s *Stream) maybeDetachIfBothClosed() {
	s.inMu.Lock()
	remFin := s.remoteFin
	s.inMu.Unlock()
	if remFin && s.localFin.Load() {
		s.detach()
	}
}

func (s *Stream) detach() {
	s.detachOnce.Do(func() {
		s.lane.mu.Lock()
		if cur, ok := s.lane.streams[s.streamID]; ok && cur == s {
			delete(s.lane.streams, s.streamID)
		}
		s.lane.mu.Unlock()
		s.mux.releaseStreamSlot()
	})
}
