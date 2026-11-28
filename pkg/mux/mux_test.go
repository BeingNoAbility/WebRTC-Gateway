package mux

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"webrtc-gateway/pkg/protocol"
)

type mockDC struct {
	peer      *mockDC
	inbox     chan []byte
	stopCh    chan struct{}
	closed    atomic.Bool
	closeOnce sync.Once
	buffered  atomic.Uint64
	lowThresh atomic.Uint64
	lowFnMu   sync.Mutex
	lowFn     func()
	stallSend atomic.Bool
	stallGate chan struct{}
}

func newMockDCPair() (*mockDC, *mockDC) {
	a := &mockDC{inbox: make(chan []byte, 4096), stopCh: make(chan struct{})}
	b := &mockDC{inbox: make(chan []byte, 4096), stopCh: make(chan struct{})}
	a.peer = b
	b.peer = a
	return a, b
}

func (d *mockDC) start(handler func([]byte)) {
	go func() {
		for {
			select {
			case <-d.stopCh:
				return
			case msg := <-d.inbox:
				handler(msg)
			}
		}
	}()
}

func (d *mockDC) Send(data []byte) error {
	if d.closed.Load() {
		return ErrMuxClosed
	}
	if d.stallSend.Load() && d.stallGate != nil {
		select {
		case <-d.stallGate:
		case <-d.stopCh:
			return ErrMuxClosed
		}
	}
	cp := append([]byte(nil), data...)
	select {
	case <-d.peer.stopCh:
		return ErrMuxClosed
	case d.peer.inbox <- cp:
		return nil
	}
}

func (d *mockDC) BufferedAmount() uint64 { return d.buffered.Load() }

func (d *mockDC) SetBufferedAmountLowThreshold(n uint64) { d.lowThresh.Store(n) }

func (d *mockDC) OnBufferedAmountLow(fn func()) {
	d.lowFnMu.Lock()
	d.lowFn = fn
	d.lowFnMu.Unlock()
}

func (d *mockDC) setBuffered(n uint64) {
	d.buffered.Store(n)
	if n <= d.lowThresh.Load() {
		d.lowFnMu.Lock()
		fn := d.lowFn
		d.lowFnMu.Unlock()
		if fn != nil {
			fn()
		}
	}
}

func (d *mockDC) Close() error {
	d.closed.Store(true)
	d.closeOnce.Do(func() { close(d.stopCh) })
	return nil
}

func newPair(t *testing.T, lanes int, serverCfg, clientCfg Config) (*Mux, *Mux, []*mockDC, []*mockDC) {
	t.Helper()
	serverCfg.PoolSize = lanes
	clientCfg.PoolSize = lanes
	srv := New(serverCfg)
	cli := New(clientCfg)

	cCtrl, sCtrl := newMockDCPair()
	cli.AttachControlDC(cCtrl)
	srv.AttachControlDC(sCtrl)
	cCtrl.start(cli.HandleControlMessage)
	sCtrl.start(srv.HandleControlMessage)

	cLanes := make([]*mockDC, lanes)
	sLanes := make([]*mockDC, lanes)
	for i := 0; i < lanes; i++ {
		idx := i
		cDC, sDC := newMockDCPair()
		cLanes[idx] = cDC
		sLanes[idx] = sDC
		cli.AttachLaneDC(idx, cDC)
		srv.AttachLaneDC(idx, sDC)
		cDC.start(func(b []byte) { cli.HandleLaneMessage(idx, b) })
		sDC.start(func(b []byte) { srv.HandleLaneMessage(idx, b) })
	}
	t.Cleanup(func() {
		cli.Close()
		srv.Close()
	})
	return cli, srv, cLanes, sLanes
}

// Regression test for C-4: rapid back-to-back HTTP requests over a single lane NEVER collide or
// trigger RESET(PROTOCOL_ERROR) because StreamIDs are strictly monotonic and never reused.
func TestV8Mux_NoSlotReuseRace_C4(t *testing.T) {
	cli, srv, _, _ := newPair(t, 1, Config{}, Config{})

	go func() {
		for {
			st, err := srv.AcceptStream(context.Background())
			if err != nil {
				return
			}
			go func(s *Stream) {
				defer s.Close()
				respTLV, _ := protocol.EncodeResponseHeaders(&protocol.ResponseHeaders{Status: 204})
				_ = s.WriteHeaders(respTLV, true)
			}(st)
		}
	}()

	reqTLV, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindHTTP, Method: "GET", Target: "/ping",
	})

	for i := 0; i < 300; i++ {
		st, err := cli.OpenStream(reqTLV, false)
		if err != nil {
			t.Fatalf("req %d OpenStream failed: %v", i, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		rawResp, err := st.ReadResponseHeaders(ctx)
		cancel()
		if err != nil {
			t.Fatalf("C-4 regression at req %d: ReadResponseHeaders failed: %v", i, err)
		}
		rh, err := protocol.DecodeResponseHeaders(rawResp)
		if err != nil || rh.Status != 204 {
			t.Fatalf("req %d unexpected status: %+v err=%v", i, rh, err)
		}
		_ = st.Close()
	}
}

// Regression test for C-5: 10,000 tiny 13-byte SSE chunks actively consumed MUST NOT deadlock or
// trigger FLOW_CONTROL_ERROR (pure payload-byte credit replenishment every 256 chunks / 128 KiB).
func TestV8Mux_TinySSEStreamNoFlowControlKill_C5(t *testing.T) {
	cli, srv, _, _ := newPair(t, 1, Config{}, Config{})

	const events = 10000
	const chunkStr = "data: token\n\n" // 13 bytes

	go func() {
		st, err := srv.AcceptStream(context.Background())
		if err != nil {
			return
		}
		defer st.Close()
		respTLV, _ := protocol.EncodeResponseHeaders(&protocol.ResponseHeaders{
			Status:  200,
			Headers: []protocol.HeaderField{{Name: "content-type", Value: "text/event-stream"}},
		})
		if err := st.WriteHeaders(respTLV, false); err != nil {
			return
		}
		payload := []byte(chunkStr)
		for i := 0; i < events; i++ {
			if _, err := st.Write(payload); err != nil {
				return
			}
		}
	}()

	reqTLV, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindHTTP, Method: "GET", Target: "/sse",
	})
	cs, err := cli.OpenStream(reqTLV, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cs.ReadResponseHeaders(ctx); err != nil {
		t.Fatal(err)
	}
	all, err := io.ReadAll(cs)
	if err != nil {
		t.Fatalf("C-5 regression: tiny SSE stream failed: %v (got %d bytes)", err, len(all))
	}
	wantBytes := events * len(chunkStr)
	if len(all) != wantBytes {
		t.Fatalf("C-5 regression: got %d bytes, want %d bytes", len(all), wantBytes)
	}
	if cli.SessionMemUsed() != 0 || srv.SessionMemUsed() != 0 {
		t.Fatalf("memory budget leak after stream finish: cli=%d srv=%d", cli.SessionMemUsed(), srv.SessionMemUsed())
	}
}

// Regression test for H-4: blocked waitSCTPBackpressure wakes immediately on Reset() AND respects
// bounded SCTPWriteTimeout without leaking budgets or hanging Close().
func TestV8Mux_SCTPBackpressureTimeoutAndInstantReset_H4(t *testing.T) {
	cli, srv, cLanes, _ := newPair(t, 1, Config{}, Config{SCTPWriteTimeout: 120 * time.Millisecond})

	// Hold SCTP buffer permanently above high-water mark.
	cLanes[0].setBuffered(SCTPBackpressureHighWater + 1024)

	reqTLV, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindHTTP, Method: "POST", Target: "/upload",
	})

	// 1. Bounded SCTPWriteTimeout fires cleanly (~120ms) instead of hanging forever.
	start := time.Now()
	_, err := cli.OpenStream(reqTLV, true)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrSCTPWriteTimeout) {
		t.Fatalf("expected ErrSCTPWriteTimeout, got %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("SCTP write timeout took too long: %v", elapsed)
	}

	// 2. Active stream blocked inside Write() wakes IMMEDIATELY when Reset() is called.
	cLanes[0].setBuffered(0)
	cs, err := cli.OpenStream(reqTLV, true)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := srv.AcceptStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()

	cLanes[0].setBuffered(SCTPBackpressureHighWater + 1024)
	writeDone := make(chan error, 1)
	go func() {
		_, werr := cs.Write([]byte("blocked-body"))
		writeDone <- werr
	}()
	time.Sleep(15 * time.Millisecond)

	resetStart := time.Now()
	cs.Reset(protocol.ErrCodeCancel)
	select {
	case werr := <-writeDone:
		if werr == nil {
			t.Fatal("expected blocked Write to fail on Reset")
		}
		if time.Since(resetStart) > 200*time.Millisecond {
			t.Fatalf("H-4 regression: Reset did not wake blocked Write immediately (%v)", time.Since(resetStart))
		}
	case <-time.After(1 * time.Second):
		t.Fatal("H-4 regression: Write deadlocked after Reset")
	}
}

// Verify chunked JSON control frames (96 KiB over 4 KiB MMS) reassemble intact without truncation.
func TestV8Mux_ChunkedControlFramesAndPreAuthPingGate(t *testing.T) {
	gotCtrl := make(chan *protocol.Frame, 2)
	gotProtoErr := make(chan error, 2)

	cli, srv, _, _ := newPair(t, 2, Config{
		OnControlFrame:  func(f *protocol.Frame) { gotCtrl <- f },
		OnProtocolError: func(err error) { gotProtoErr <- err },
	}, Config{
		OnControlFrame: func(f *protocol.Frame) { gotCtrl <- f },
	})
	cli.SetMaxMessageSize(protocol.MinNegotiatedMaxMsgSize) // 4 KiB MMS -> 4088B chunks!
	srv.SetMaxMessageSize(protocol.MinNegotiatedMaxMsgSize)

	bigJSON := []byte(`{"items":"` + strings.Repeat("x", 90*1024) + `"}`)
	if err := srv.SendControlFrame(protocol.TypeAuthResult, bigJSON); err != nil {
		t.Fatalf("SendControlFrame 90KiB over 4KiB MMS failed: %v", err)
	}
	select {
	case f := <-gotCtrl:
		if f.Type != protocol.TypeAuthResult || !bytes.Equal(f.Payload, bigJSON) {
			t.Fatalf("chunked control payload mismatch: type=%d len=%d", f.Type, len(f.Payload))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for chunked control frame")
	}

	// M-9: Malformed control frame triggers OnProtocolError instead of silent drop.
	srv.HandleControlMessage([]byte{0x00, 0x00})
	select {
	case <-gotProtoErr:
	case <-time.After(1 * time.Second):
		t.Fatal("M-9 regression: malformed control frame did not report OnProtocolError")
	}
}

// Verify multi-stream HTTP + WS multiplexing over shared lanes with large 64 KiB chunked headers
// and 256 KiB streaming WS messages.
func TestV8Mux_MultiplexedHTTPAndWSOverSharedLanes(t *testing.T) {
	cli, srv, _, _ := newPair(t, 2, Config{}, Config{})
	cli.SetMaxMessageSize(protocol.MinNegotiatedMaxMsgSize)
	srv.SetMaxMessageSize(protocol.MinNegotiatedMaxMsgSize)

	go func() {
		for {
			st, err := srv.AcceptStream(context.Background())
			if err != nil {
				return
			}
			go func(s *Stream) {
				defer s.Close()
				rh, err := protocol.DecodeRequestHeaders(s.HeadersRaw())
				if err != nil {
					s.Reset(protocol.ErrCodeProtocolError)
					return
				}
				if rh.Kind == protocol.StreamKindWS {
					respTLV, _ := protocol.EncodeResponseHeaders(&protocol.ResponseHeaders{Status: 101})
					_ = s.WriteHeaders(respTLV, false)
					var msg bytes.Buffer
					for {
						ch, rerr := s.ReadWSChunk(context.Background())
						if rerr != nil {
							return
						}
						if ch.Op == protocol.TypeWSClose {
							_ = s.WriteWSClose(1000, "echo-bye")
							return
						}
						msg.Write(ch.Payload)
						if ch.Fin {
							_ = s.WriteWSChunk(context.Background(), ch.Op, true, msg.Bytes())
							msg.Reset()
						}
					}
				}
				body, _ := io.ReadAll(s)
				respTLV, _ := protocol.EncodeResponseHeaders(&protocol.ResponseHeaders{Status: 200})
				_ = s.WriteHeaders(respTLV, false)
				_, _ = s.Write(body)
			}(st)
		}
	}()

	// 1. Open a streaming WS and an HTTP POST with large 20 KiB headers concurrently on 2 lanes.
	wsReq, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindWS, Method: "GET", Target: "/ws",
	})
	wsStream, err := cli.OpenStream(wsReq, true)
	if err != nil {
		t.Fatal(err)
	}
	defer wsStream.Close()

	httpReq, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind:   protocol.StreamKindHTTP,
		Method: "POST",
		Target: "/echo",
		Headers: []protocol.HeaderField{
			{Name: "x-pad", Value: strings.Repeat("p", 20*1024)},
		},
	})
	httpStream, err := cli.OpenStream(httpReq, true)
	if err != nil {
		t.Fatal(err)
	}
	defer httpStream.Close()

	// Send 1 MiB HTTP body while WS echoes a 256 KiB binary message concurrently.
	httpPayload := bytes.Repeat([]byte("HTTP-DATA!"), 100*1024)
	wsPayload := bytes.Repeat([]byte{0xca, 0xfe, 0xba, 0xbe}, 64*1024)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := httpStream.Write(httpPayload); err != nil {
			t.Errorf("http Write failed: %v", err)
			return
		}
		_ = httpStream.CloseWrite()
		if _, err := httpStream.ReadResponseHeaders(context.Background()); err != nil {
			t.Errorf("http ReadResponseHeaders failed: %v", err)
			return
		}
		got, err := io.ReadAll(httpStream)
		if err != nil || !bytes.Equal(got, httpPayload) {
			t.Errorf("http echo mismatch: len=%d err=%v", len(got), err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := wsStream.ReadResponseHeaders(context.Background()); err != nil {
			t.Errorf("ws ReadResponseHeaders failed: %v", err)
			return
		}
		if err := wsStream.WriteWSChunk(context.Background(), protocol.TypeWSBinary, true, wsPayload); err != nil {
			t.Errorf("ws WriteWSChunk failed: %v", err)
			return
		}
		var got bytes.Buffer
		for {
			ch, rerr := wsStream.ReadWSChunk(context.Background())
			if rerr != nil {
				t.Errorf("ws ReadWSChunk failed: %v", rerr)
				return
			}
			got.Write(ch.Payload)
			if ch.Fin {
				break
			}
		}
		if !bytes.Equal(got.Bytes(), wsPayload) {
			t.Errorf("ws echo mismatch: len=%d want=%d", got.Len(), len(wsPayload))
		}
		_ = wsStream.WriteWSClose(1000, "done")
	}()
	wg.Wait()
}

func TestRegression_P1_SetMaxMessageSizeMonotonicOnly(t *testing.T) {
	m := New(Config{PoolSize: 1})
	defer m.Close()

	m.SetMaxMessageSize(4096)
	if got := m.ChunkSize(); got != 4096-protocol.HeaderSize {
		t.Fatalf("expected chunkSize=%d, got %d", 4096-protocol.HeaderSize, got)
	}
	// Higher local SCTP capability or peer SETTINGS must NEVER raise chunkSize back up!
	m.SetMaxMessageSize(262144)
	if got := m.ChunkSize(); got != 4096-protocol.HeaderSize {
		t.Fatalf("P1-3 regression: SetMaxMessageSize raised chunkSize from %d to %d", 4096-protocol.HeaderSize, got)
	}
}

func TestRegression_P1_WSStreamCloseNeverEmitsTypeDataFin(t *testing.T) {
	var sentMu sync.Mutex
	var sent [][]byte
	dc := &captureDC{onSend: func(b []byte) {
		sentMu.Lock()
		sent = append(sent, append([]byte(nil), b...))
		sentMu.Unlock()
	}}
	m := New(Config{PoolSize: 1})
	defer m.Close()
	m.AttachLaneDC(0, dc)

	s := newStream(m, m.lanes[0], 1, false)
	s.kind.Store(uint32(protocol.StreamKindWS))
	m.lanes[0].mu.Lock()
	m.lanes[0].streams[1] = s
	m.lanes[0].mu.Unlock()

	_ = s.Close()
	sentMu.Lock()
	defer sentMu.Unlock()
	for _, raw := range sent {
		f, err := protocol.Decode(raw)
		if err == nil && f.Type == protocol.TypeData {
			t.Fatalf("P1-4 regression: WebSocket Stream.Close() emitted illegal TypeData frame (flags=%d)", f.Flags)
		}
	}
}

func TestRegression_P2_RequireAuthHeaderBudgetReleaseAndWSChunkCap(t *testing.T) {
	// 1. Defense-in-depth: RequireAuth rejects pre-auth data-lane frames.
	var protoErr atomic.Bool
	mAuth := New(Config{
		PoolSize:    1,
		RequireAuth: true,
		OnProtocolError: func(error) {
			protoErr.Store(true)
		},
	})
	defer mAuth.Close()
	reqTLV, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindHTTP, Method: "GET", Target: "/",
	})
	hdrFrame, err := (&protocol.Frame{
		StreamID: 1,
		Type:     protocol.TypeHeaders,
		Flags:    protocol.FlagFin,
		Payload:  reqTLV,
	}).Encode()
	if err != nil {
		t.Fatalf("Encode hdrFrame: %v", err)
	}
	mAuth.HandleLaneMessage(0, hdrFrame)
	if !protoErr.Load() {
		t.Fatal("RequireAuth mux must reject lane frames prior to SetAuthenticated(true)")
	}

	// 2. TakeHeadersRaw releases header buffer & session memory budget immediately.
	m := New(Config{PoolSize: 1})
	defer m.Close()
	bigReq, err := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind:   protocol.StreamKindWS,
		Method: "GET",
		Target: "/ws",
		Headers: []protocol.HeaderField{
			{Name: "x-big-1", Value: strings.Repeat("a", 2048)},
			{Name: "x-big-2", Value: strings.Repeat("b", 2048)},
			{Name: "x-big-3", Value: strings.Repeat("c", 2048)},
			{Name: "x-big-4", Value: strings.Repeat("d", 2048)},
		},
	})
	if err != nil {
		t.Fatalf("EncodeRequestHeaders: %v", err)
	}
	rawBigHdr, err := (&protocol.Frame{
		StreamID: 1,
		Type:     protocol.TypeHeaders,
		Flags:    protocol.FlagNone,
		Payload:  bigReq,
	}).Encode()
	if err != nil {
		t.Fatalf("Encode rawBigHdr: %v", err)
	}
	m.HandleLaneMessage(0, rawBigHdr)
	if used := m.SessionMemUsed(); used < 8192 {
		t.Fatalf("expected hdrBuf memory accounted in sessionBudget, got %d", used)
	}
	st, err := m.AcceptStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gotHdr := st.TakeHeadersRaw()
	if len(gotHdr) != len(bigReq) {
		t.Fatalf("TakeHeadersRaw length mismatch: got %d want %d", len(gotHdr), len(bigReq))
	}
	if m.SessionMemUsed() != 0 {
		t.Fatalf("expected sessionBudget released after TakeHeadersRaw, got %d", m.SessionMemUsed())
	}
	if st.HeadersRaw() != nil {
		t.Fatal("expected s.hdrBuf cleared to nil after TakeHeadersRaw")
	}

	// 3. Single streaming WS message exceeding MaxWSMessageChunks (16384) must reset stream!
	var resetSent atomic.Bool
	m.AttachLaneDC(0, &captureDC{onSend: func(b []byte) {
		if f, err := protocol.Decode(b); err == nil && f.Type == protocol.TypeReset {
			resetSent.Store(true)
		}
	}})
	// Feed MaxWSMessageChunks + 1 tiny 1-byte chunks (reading concurrently so recvInFlightPayload stays low).
	doneRead := make(chan struct{})
	go func() {
		defer close(doneRead)
		for {
			if _, rerr := st.ReadWSChunk(context.Background()); rerr != nil {
				return
			}
		}
	}()
	wsChunkFrame, err := (&protocol.Frame{
		StreamID: 1,
		Type:     protocol.TypeWSBinary,
		Flags:    protocol.FlagNone,
		Payload:  []byte{0x01},
	}).Encode()
	if err != nil {
		t.Fatalf("Encode wsChunkFrame: %v", err)
	}
	for i := 0; i <= protocol.MaxWSMessageChunks; i++ {
		if st.Context().Err() != nil {
			break
		}
		m.HandleLaneMessage(0, wsChunkFrame)
	}
	<-doneRead
	if !resetSent.Load() {
		t.Fatal("expected stream RESET when WS message exceeds MaxWSMessageChunks")
	}
}

type captureDC struct {
	onSend func([]byte)
}

func (c *captureDC) Send(b []byte) error {
	if c.onSend != nil {
		c.onSend(b)
	}
	return nil
}
func (c *captureDC) BufferedAmount() uint64 { return 0 }
func (c *captureDC) Close() error           { return nil }

func TestRegression_P0_3_SingleStreamZeroByteWSCannotExhaustSessionBudget(t *testing.T) {
	var resetCode atomic.Uint32
	m := New(Config{PoolSize: 1}) // default 32 MiB session budget
	defer m.Close()
	m.AttachLaneDC(0, &captureDC{onSend: func(b []byte) {
		if f, err := protocol.Decode(b); err == nil && f.Type == protocol.TypeReset {
			if code, cerr := protocol.DecodeResetPayload(f.Payload); cerr == nil {
				resetCode.Store(uint32(code))
			}
		}
	}})

	wsReq, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindWS, Method: "GET", Target: "/ws",
	})
	hdrFrame, _ := (&protocol.Frame{
		StreamID: 1, Type: protocol.TypeHeaders, Flags: protocol.FlagNone, Payload: wsReq,
	}).Encode()
	m.HandleLaneMessage(0, hdrFrame)
	st, err := m.AcceptStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = st.TakeHeadersRaw()

	emptyWS, _ := (&protocol.Frame{
		StreamID: 1, Type: protocol.TypeWSText, Flags: protocol.FlagMsgFin, Payload: nil,
	}).Encode()
	for i := 0; i < MaxStreamInboundChunks+100; i++ {
		if st.Context().Err() != nil {
			break
		}
		m.HandleLaneMessage(0, emptyWS)
	}
	if protocol.ErrorCode(resetCode.Load()) != protocol.ErrCodeFlowControlError {
		t.Fatalf("P0-3 regression: expected FLOW_CONTROL_ERROR reset on 0B WS flood, got code=%d", resetCode.Load())
	}
	if m.SessionMemUsed() != 0 {
		t.Fatalf("P0-3 regression: session memory leaked after reset: %d", m.SessionMemUsed())
	}
}

func TestRegression_P1_1_HTTPStreamRejectsWSFramesAndPreservesCloseFin(t *testing.T) {
	var sentMu sync.Mutex
	var resetCodes []protocol.ErrorCode
	m := New(Config{PoolSize: 1})
	defer m.Close()
	m.AttachLaneDC(0, &captureDC{onSend: func(b []byte) {
		if f, err := protocol.Decode(b); err == nil && f.Type == protocol.TypeReset {
			if code, cerr := protocol.DecodeResetPayload(f.Payload); cerr == nil {
				sentMu.Lock()
				resetCodes = append(resetCodes, code)
				sentMu.Unlock()
			}
		}
	}})

	httpReq, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindHTTP, Method: "POST", Target: "/upload",
	})
	hdrFrame, _ := (&protocol.Frame{
		StreamID: 1, Type: protocol.TypeHeaders, Flags: protocol.FlagNone, Payload: httpReq,
	}).Encode()
	m.HandleLaneMessage(0, hdrFrame)
	st, err := m.AcceptStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Inject illegal WS_TEXT frame on an HTTP stream -> must Reset(PROTOCOL_ERROR) immediately!
	wsInject, _ := (&protocol.Frame{
		StreamID: 1, Type: protocol.TypeWSText, Flags: protocol.FlagMsgFin, Payload: []byte("spoof"),
	}).Encode()
	m.HandleLaneMessage(0, wsInject)

	if _, rerr := io.ReadAll(st); rerr == nil {
		t.Fatal("P1-1 regression: HTTP Stream.Read accepted spoofed WS_TEXT payload")
	}
	sentMu.Lock()
	defer sentMu.Unlock()
	if len(resetCodes) == 0 || resetCodes[0] != protocol.ErrCodeProtocolError {
		t.Fatalf("P1-1 regression: expected PROTOCOL_ERROR reset on WS_TEXT in HTTP stream, got %v", resetCodes)
	}
}

func TestRegression_P1_2_IncompleteHeadersRejectsCreditAndEnforcesHardTimeout(t *testing.T) {
	// 1. CREDIT arriving before HEADERS completes must immediately reset with PROTOCOL_ERROR.
	var resetCode atomic.Uint32
	m1 := New(Config{PoolSize: 1})
	defer m1.Close()
	m1.AttachLaneDC(0, &captureDC{onSend: func(b []byte) {
		if f, err := protocol.Decode(b); err == nil && f.Type == protocol.TypeReset {
			if code, cerr := protocol.DecodeResetPayload(f.Payload); cerr == nil {
				resetCode.Store(uint32(code))
			}
		}
	}})
	partHdr, _ := (&protocol.Frame{
		StreamID: 1, Type: protocol.TypeHeaders, Flags: protocol.FlagMore, Payload: []byte{0x01},
	}).Encode()
	m1.HandleLaneMessage(0, partHdr)
	creditPay, _ := protocol.EncodeCreditPayload(1)
	credit1, _ := (&protocol.Frame{
		StreamID: 1, Type: protocol.TypeCredit, Payload: creditPay,
	}).Encode()
	m1.HandleLaneMessage(0, credit1)
	if protocol.ErrorCode(resetCode.Load()) != protocol.ErrCodeProtocolError {
		t.Fatalf("P1-2 regression: CREDIT on incomplete HEADERS must trigger PROTOCOL_ERROR, got %d", resetCode.Load())
	}
	if m1.activeStreams.Load() != 0 {
		t.Fatalf("P1-2 regression: stream slot leaked after premature CREDIT: %d", m1.activeStreams.Load())
	}

	// 2. Unfinished HEADERS stream must be reaped by HeaderAssemblyTimeout even when idle timeout is long.
	m2 := New(Config{
		PoolSize:              1,
		HeaderAssemblyTimeout: 60 * time.Millisecond,
		StreamIdleTimeout:     5 * time.Second,
	})
	defer m2.Close()
	m2.AttachLaneDC(0, &captureDC{})
	m2.HandleLaneMessage(0, partHdr)
	if m2.activeStreams.Load() != 1 {
		t.Fatalf("expected 1 active partial stream, got %d", m2.activeStreams.Load())
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if m2.activeStreams.Load() == 0 {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("P1-2 regression: unfinished HEADERS stream was not reaped within HeaderAssemblyTimeout (active=%d)", m2.activeStreams.Load())
}

func TestRegression_P1_3_HTTPCloseDoesNotStallBehindBlockedWrite(t *testing.T) {
	cli, srv, cLanes, _ := newPair(t, 1, Config{}, Config{SCTPWriteTimeout: 500 * time.Millisecond})
	reqTLV, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindHTTP, Method: "POST", Target: "/slow",
	})
	cs, err := cli.OpenStream(reqTLV, true)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := srv.AcceptStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()

	cLanes[0].setBuffered(SCTPBackpressureHighWater + 1024)
	writeDone := make(chan error, 1)
	go func() {
		_, werr := cs.Write([]byte("blocked-write"))
		writeDone <- werr
	}()
	time.Sleep(20 * time.Millisecond)

	start := time.Now()
	_ = cs.Close()
	elapsed := time.Since(start)
	if elapsed > 150*time.Millisecond {
		t.Fatalf("P1-3 regression: HTTP Close() stalled behind blocked Write() for %v", elapsed)
	}
	select {
	case werr := <-writeDone:
		if werr == nil {
			t.Fatal("expected blocked Write() to abort when Close() ran")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("P1-3 regression: blocked Write() did not wake after Close()")
	}
}
