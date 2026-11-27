package mux

import (
	"bytes"
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"webrtc-gateway/pkg/protocol"
)

type memDC struct {
	mu     sync.Mutex
	closed bool
	ch     chan []byte
	bufAmt atomic.Uint64
}

func newMemDC(onMsg func([]byte)) *memDC {
	d := &memDC{ch: make(chan []byte, 4096)}
	if onMsg != nil {
		go func() {
			for pkt := range d.ch {
				onMsg(pkt)
			}
		}()
	}
	return d
}

func (d *memDC) Send(data []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return ErrMuxClosed
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	d.ch <- cp
	return nil
}

func (d *memDC) BufferedAmount() uint64 { return d.bufAmt.Load() }

func (d *memDC) Close() error {
	d.mu.Lock()
	if !d.closed {
		d.closed = true
		close(d.ch)
	}
	d.mu.Unlock()
	return nil
}

func newConnectedPair(t *testing.T, poolSize int, globalBudget *MemoryBudget) (client, server *Mux) {
	t.Helper()
	var globalActive atomic.Int64
	client = New(Config{PoolSize: poolSize})
	server = New(Config{
		PoolSize:            poolSize,
		GlobalBudget:        globalBudget,
		GlobalActiveStreams: &globalActive,
	})

	cCtrl := newMemDC(server.HandleControlMessage)
	sCtrl := newMemDC(client.HandleControlMessage)
	client.AttachControlDC(cCtrl)
	server.AttachControlDC(sCtrl)

	for i := 0; i < poolSize; i++ {
		idx := i
		cDC := newMemDC(func(b []byte) { server.HandlePoolMessage(idx, b) })
		sDC := newMemDC(func(b []byte) { client.HandlePoolMessage(idx, b) })
		client.AttachPoolDC(idx, cDC)
		server.AttachPoolDC(idx, sDC)
	}
	return client, server
}

func openConnectedWSPair(t *testing.T, client, server *Mux, target string) (cs, ss *Stream, cleanup func()) {
	t.Helper()
	var cliOnMsg func([]byte)
	var cliMu sync.Mutex
	sWSDC := newMemDC(func(b []byte) {
		cliMu.Lock()
		cb := cliOnMsg
		cliMu.Unlock()
		if cb != nil {
			cb(b)
		}
	})
	srvOnMsg, srvOnClose, err := server.AttachDynamicWSDC(sWSDC)
	if err != nil {
		t.Fatal(err)
	}

	wsHdr, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindWS, Method: "GET", Target: target,
	})
	cs, cMsg, cliClose, err := client.OpenWSStream(context.Background(), newMemDC(srvOnMsg), wsHdr)
	if err != nil {
		srvOnClose()
		t.Fatal(err)
	}
	cliMu.Lock()
	cliOnMsg = cMsg
	cliMu.Unlock()

	ss, err = server.AcceptStream(context.Background())
	if err != nil {
		cliClose()
		srvOnClose()
		t.Fatal(err)
	}
	resp101, _ := protocol.EncodeResponseHeaders(&protocol.ResponseHeaders{Status: 101})
	if err := ss.WriteHeaders(resp101, false); err != nil {
		_ = ss.Close()
		cliClose()
		srvOnClose()
		t.Fatal(err)
	}
	if _, err := cs.ReadResponseHeaders(context.Background()); err != nil {
		_ = ss.Close()
		cliClose()
		srvOnClose()
		t.Fatal(err)
	}
	return cs, ss, func() {
		_ = ss.Close()
		cliClose()
		srvOnClose()
	}
}

func TestMuxV7_Negative_UnreadBodyCloseNeverLeaksGlobalBudget(t *testing.T) {
	// Client sends GET + 1.5 MiB unread DATA+FIN across 8 streams while handler closes without reading;
	// global and session memory budgets must return strictly to 0.
	globalBudget := NewMemoryBudget(16 * 1024 * 1024)
	client, server := newConnectedPair(t, 8, globalBudget)
	defer client.Close()
	defer server.Close()

	reqHdr, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindHTTP, Method: "GET", Target: "/",
	})
	respHdr, _ := protocol.EncodeResponseHeaders(&protocol.ResponseHeaders{Status: 200})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 8; i++ {
			st, err := server.AcceptStream(context.Background())
			if err != nil {
				return
			}
			// Intentionally do not read request body (simulating GET / 403 / 405).
			time.Sleep(15 * time.Millisecond)
			_ = st.WriteHeaders(respHdr, true)
			_ = st.Close()
		}
	}()

	payload := bytes.Repeat([]byte("A"), 1536*1024) // 1.5 MiB per stream -> 12 MiB total
	for i := 0; i < 8; i++ {
		cs, err := client.OpenHTTPStream(context.Background(), reqHdr, false)
		if err != nil {
			t.Fatalf("open stream %d: %v", i, err)
		}
		if _, err := cs.Write(payload); err != nil {
			t.Fatalf("write stream %d: %v", i, err)
		}
		_ = cs.CloseWrite()
		_, _ = cs.ReadResponseHeaders(context.Background())
		_ = cs.Close()
	}
	wg.Wait()

	if used := globalBudget.Used(); used != 0 {
		t.Fatalf("GlobalMemoryBudget leaked %d bytes after unread closes", used)
	}
	if server.SessionMemUsed() != 0 {
		t.Fatalf("SessionMemUsed leaked %d bytes after unread closes", server.SessionMemUsed())
	}
}

func TestMuxV7_Negative_ConcurrentCloseAndHeadersNeverPanics(t *testing.T) {
	// Concurrent HandlePoolMessage(HEADERS) + Mux.Close() under -race must never panic.
	reqHdr, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindHTTP, Method: "GET", Target: "/race",
	})
	for iter := 0; iter < 100; iter++ {
		m := New(Config{PoolSize: 4})
		dc := newMemDC(nil)
		m.AttachPoolDC(0, dc)
		f := &protocol.Frame{Gen: 1, Type: protocol.TypeHeaders, Flags: protocol.FlagFin, Payload: reqHdr}
		wire, _ := f.Encode()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			m.HandlePoolMessage(0, wire)
		}()
		go func() {
			defer wg.Done()
			m.Close()
		}()
		wg.Wait()
	}
}

func TestMuxV7_FastPostHeadersHadFinPreservesBody(t *testing.T) {
	// Fast POST where HEADERS(FIN=false) + DATA(body, FIN=true) arrive before AcceptStream
	// must report HasRequestBody()==true and deliver the full body.
	globalBudget := NewMemoryBudget(16 * 1024 * 1024)
	client, server := newConnectedPair(t, 4, globalBudget)
	defer client.Close()
	defer server.Close()

	reqHdr, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindHTTP, Method: "POST", Target: "/submit",
	})
	cs, err := client.OpenHTTPStream(context.Background(), reqHdr, false)
	if err != nil {
		t.Fatal(err)
	}
	wantBody := []byte(`{"fast":"post"}`)
	if _, err := cs.Write(wantBody); err != nil {
		t.Fatal(err)
	}
	_ = cs.CloseWrite()

	time.Sleep(30 * time.Millisecond)

	ss, err := server.AcceptStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	if !ss.HasRequestBody() {
		t.Fatal("fast POST reported HasRequestBody()==false")
	}
	gotBody, err := io.ReadAll(ss)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBody, wantBody) {
		t.Fatalf("fast POST body mismatch: got %q want %q", gotBody, wantBody)
	}
}

func TestMuxV7_LargeWSStreaming2_5MiBNoDeadlock(t *testing.T) {
	// Sending a 2.5 MiB WebSocket message (> 2 MiB initial stream window) over a dedicated "p2pws" DC
	// must stream smoothly without flow-control deadlock.
	globalBudget := NewMemoryBudget(32 * 1024 * 1024)
	client, server := newConnectedPair(t, 2, globalBudget)
	defer client.Close()
	defer server.Close()

	cs, ss, cleanup := openConnectedWSPair(t, client, server, "/ws")
	defer cleanup()

	bigPayload := bytes.Repeat([]byte{0x5a, 0xa5, 0x11, 0x22}, 640*1024) // 2.5 MiB
	errCh := make(chan error, 1)
	go func() {
		errCh <- ss.WriteWSMessage(true, bigPayload)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got bytes.Buffer
	for {
		ch, rerr := cs.ReadWSChunk(ctx)
		if rerr != nil {
			t.Fatalf("ReadWSChunk failed: %v", rerr)
		}
		got.Write(ch.Payload)
		if ch.Fin {
			break
		}
	}
	if werr := <-errCh; werr != nil {
		t.Fatalf("WriteWSMessage failed: %v", werr)
	}
	if !bytes.Equal(got.Bytes(), bigPayload) {
		t.Fatalf("2.5 MiB WS payload mismatch: got %d bytes want %d", got.Len(), len(bigPayload))
	}
}

func TestMuxV7_SetMaxMessageSizeClampsDownOnlyAndRecvWindowEnforced(t *testing.T) {
	// SetMaxMessageSize must only clamp downward.
	m := New(Config{PoolSize: 2})
	defer m.Close()
	m.SetMaxMessageSize(16384)
	if got := m.effectiveChunkSize(); got != 16384-protocol.HeaderSize {
		t.Fatalf("expected 16376, got %d", got)
	}
	m.SetMaxMessageSize(65536) // peer sends larger value -> must not overwrite upward
	if got := m.effectiveChunkSize(); got != 16384-protocol.HeaderSize {
		t.Fatalf("SetMaxMessageSize increased chunk size to %d", got)
	}

	// Sending more than DefaultStreamWindowSize without CREDIT triggers FLOW_CONTROL_ERROR reset.
	dc := newMemDC(nil)
	m.AttachPoolDC(0, dc)
	reqHdr, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindHTTP, Method: "POST", Target: "/flood",
	})
	hdrFrame, _ := (&protocol.Frame{Gen: 1, Type: protocol.TypeHeaders, Flags: 0, Payload: reqHdr}).Encode()
	m.HandlePoolMessage(0, hdrFrame)
	st, err := m.AcceptStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 32760)
	dataFrame, _ := (&protocol.Frame{Gen: 1, Type: protocol.TypeData, Flags: 0, Payload: chunk}).Encode()
	for i := 0; i < 70; i++ {
		m.HandlePoolMessage(0, dataFrame)
	}
	if err := st.getResetErr(); err == nil {
		t.Fatal("exceeding recvWindow did not reset stream")
	}
	if m.SessionMemUsed() != 0 {
		t.Fatalf("expected 0 memory used after flow-control reset, got %d", m.SessionMemUsed())
	}
}

func TestMuxV7_InstantResetUnblocksBlockedWrite(t *testing.T) {
	// Reset() while Write() is blocked waiting for sendWindow must unblock immediately.
	globalBudget := NewMemoryBudget(16 * 1024 * 1024)
	client, server := newConnectedPair(t, 2, globalBudget)
	defer client.Close()
	defer server.Close()

	reqHdr, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindHTTP, Method: "POST", Target: "/stall",
	})
	cs, err := client.OpenHTTPStream(context.Background(), reqHdr, false)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, werr := cs.Write(make([]byte, 3*1024*1024))
		done <- werr
	}()
	time.Sleep(30 * time.Millisecond)
	start := time.Now()
	cs.Reset(protocol.ErrCodeCancel)
	select {
	case <-done:
		if time.Since(start) > 1*time.Second {
			t.Fatalf("Reset took too long (%v)", time.Since(start))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Write remained blocked after Reset()")
	}
}

func TestMuxV7_ReusablePoolExceeds4096StreamsWithZeroTombstoneError(t *testing.T) {
	// Over 4,200 sequential requests on a 4-DC pool must succeed cleanly with zero generation errors.
	globalBudget := NewMemoryBudget(16 * 1024 * 1024)
	client, server := newConnectedPair(t, 4, globalBudget)
	defer client.Close()
	defer server.Close()

	reqHdr, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindHTTP, Method: "GET", Target: "/ping",
	})
	respHdr, _ := protocol.EncodeResponseHeaders(&protocol.ResponseHeaders{Status: 200})

	const total = 4200
	go func() {
		for i := 0; i < total; i++ {
			ss, err := server.AcceptStream(context.Background())
			if err != nil {
				return
			}
			_ = ss.WriteHeaders(respHdr, true)
			_ = ss.Close()
		}
	}()

	for i := 0; i < total; i++ {
		cs, err := client.OpenHTTPStream(context.Background(), reqHdr, true)
		if err != nil {
			t.Fatalf("stream %d open failed: %v", i, err)
		}
		if _, err := cs.ReadResponseHeaders(context.Background()); err != nil {
			t.Fatalf("stream %d read headers failed: %v", i, err)
		}
		_ = cs.Close()
	}
}

func TestHeaderAssemblyTimeoutAndContinuationFSM(t *testing.T) {
	globalBudget := NewMemoryBudget(16 * 1024 * 1024)
	m := New(Config{PoolSize: 2, GlobalBudget: globalBudget})
	defer m.Close()

	var sentMu sync.Mutex
	var sentFrames []*protocol.Frame
	dc := newMemDC(func(b []byte) {
		if f, err := protocol.Decode(b); err == nil {
			sentMu.Lock()
			sentFrames = append(sentFrames, f)
			sentMu.Unlock()
		}
	})
	m.AttachPoolDC(0, dc)

	waitSentReset := func(gen uint32, code protocol.ErrorCode) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			sentMu.Lock()
			for _, f := range sentFrames {
				if f.Type == protocol.TypeReset && f.Gen == gen {
					gotCode, _ := protocol.DecodeResetPayload(f.Payload)
					if gotCode == code {
						sentMu.Unlock()
						return
					}
				}
			}
			sentMu.Unlock()
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("did not receive expected RESET(gen=%d, code=%v)", gen, code)
	}

	// 1. Orphan CONTINUATION on idle slot must be rejected with RESET(PROTOCOL_ERROR).
	orphanCont, _ := (&protocol.Frame{Gen: 1, Type: protocol.TypeContinuation, Flags: 0, Payload: []byte("abc")}).Encode()
	m.HandlePoolMessage(0, orphanCont)
	waitSentReset(1, protocol.ErrCodeProtocolError)
	if m.SessionMemUsed() != 0 || globalBudget.Used() != 0 {
		t.Fatalf("orphan CONTINUATION leaked budget: sess=%d glob=%d", m.SessionMemUsed(), globalBudget.Used())
	}

	// 2. DATA arriving during RECEIVING_HEADERS must trigger PROTOCOL_ERROR and free headerChunks.
	partialHdr, _ := (&protocol.Frame{Gen: 2, Type: protocol.TypeHeaders, Flags: protocol.FlagMoreHeaders, Payload: []byte{0x01, 0x00}}).Encode()
	m.HandlePoolMessage(0, partialHdr)
	if m.SessionMemUsed() == 0 {
		t.Fatal("expected partial header chunk to charge session budget")
	}
	prematureData, _ := (&protocol.Frame{Gen: 2, Type: protocol.TypeData, Flags: 0, Payload: []byte("x")}).Encode()
	m.HandlePoolMessage(0, prematureData)
	waitSentReset(2, protocol.ErrCodeProtocolError)
	if m.SessionMemUsed() != 0 || globalBudget.Used() != 0 {
		t.Fatalf("DATA during RECEIVING_HEADERS leaked budget: sess=%d glob=%d", m.SessionMemUsed(), globalBudget.Used())
	}

	// 3. Duplicate HEADERS during RECEIVING_HEADERS must trigger PROTOCOL_ERROR and free budget.
	partialHdr3, _ := (&protocol.Frame{Gen: 3, Type: protocol.TypeHeaders, Flags: protocol.FlagMoreHeaders, Payload: []byte{0x01, 0x00}}).Encode()
	m.HandlePoolMessage(0, partialHdr3)
	dupHdr3, _ := (&protocol.Frame{Gen: 3, Type: protocol.TypeHeaders, Flags: 0, Payload: []byte{0x01, 0x00}}).Encode()
	m.HandlePoolMessage(0, dupHdr3)
	waitSentReset(3, protocol.ErrCodeProtocolError)
	if m.SessionMemUsed() != 0 || globalBudget.Used() != 0 {
		t.Fatalf("duplicate HEADERS leaked budget: sess=%d glob=%d", m.SessionMemUsed(), globalBudget.Used())
	}

	// 4. Exceeding MaxContinuationFrames must reject and free 100% of memory budget.
	partialHdr4, _ := (&protocol.Frame{Gen: 4, Type: protocol.TypeHeaders, Flags: protocol.FlagMoreHeaders, Payload: []byte{0x01}}).Encode()
	m.HandlePoolMessage(0, partialHdr4)
	for i := 0; i < protocol.MaxContinuationFrames+2; i++ {
		c, _ := (&protocol.Frame{Gen: 4, Type: protocol.TypeContinuation, Flags: protocol.FlagMoreHeaders, Payload: []byte{0x02}}).Encode()
		m.HandlePoolMessage(0, c)
	}
	waitSentReset(4, protocol.ErrCodeFrameSizeError)
	if m.SessionMemUsed() != 0 || globalBudget.Used() != 0 {
		t.Fatalf("excess CONTINUATION frames leaked budget: sess=%d glob=%d", m.SessionMemUsed(), globalBudget.Used())
	}

	// 5. HeaderAssemblyTimeout (>5s stalled partial HEADERS) must reset and return slot to IDLE.
	partialHdr5, _ := (&protocol.Frame{Gen: 5, Type: protocol.TypeHeaders, Flags: protocol.FlagMoreHeaders, Payload: []byte{0x01, 0x00}}).Encode()
	m.HandlePoolMessage(0, partialHdr5)
	sl := m.poolSlots[0]
	sl.mu.Lock()
	st5 := sl.active
	sl.mu.Unlock()
	if st5 == nil {
		t.Fatal("expected stream 5 to be active in RECEIVING_HEADERS")
	}
	st5.inboundMu.Lock()
	st5.headerStart = time.Now().Add(-(HeaderAssemblyTimeout + time.Second))
	st5.inboundMu.Unlock()

	lateCont, _ := (&protocol.Frame{Gen: 5, Type: protocol.TypeContinuation, Flags: 0, Payload: []byte{0x00}}).Encode()
	m.HandlePoolMessage(0, lateCont)
	waitSentReset(5, protocol.ErrCodeProtocolError)
	if m.SessionMemUsed() != 0 || globalBudget.Used() != 0 {
		t.Fatalf("timed-out header assembly leaked budget: sess=%d glob=%d", m.SessionMemUsed(), globalBudget.Used())
	}
}

func TestZeroByteAndOneByteWSContBudgetEnforced(t *testing.T) {
	globalBudget := NewMemoryBudget(16 * 1024 * 1024)
	m := New(Config{PoolSize: 2, GlobalBudget: globalBudget})
	defer m.Close()

	wsDC := newMemDC(nil)
	onMsg, onClose, err := m.AttachDynamicWSDC(wsDC)
	if err != nil {
		t.Fatal(err)
	}
	defer onClose()

	wsReq, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindWS, Method: "GET", Target: "/ws",
	})
	hdrFrame, _ := (&protocol.Frame{Gen: 1, Type: protocol.TypeHeaders, Flags: 0, Payload: wsReq}).Encode()
	onMsg(hdrFrame)

	st, err := m.AcceptStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 1. 0B non-final WS_CONT raw frame must reset active stream (PROTOCOL_ERROR).
	bad0B := []byte{0, 0, 0, 1, protocol.TypeWSCont, 0x00, 0x00, 0x00}
	onMsg(bad0B)
	if st.getResetErr() == nil {
		t.Fatal("0B non-final WS_CONT did not reset active WS stream")
	}
	if m.SessionMemUsed() != 0 || globalBudget.Used() != 0 {
		t.Fatalf("0B WS_CONT reset leaked budget: sess=%d glob=%d", m.SessionMemUsed(), globalBudget.Used())
	}

	// 2. Flood of 1-byte WS_CONT chunks without reading must hit MaxInboundQueueLen=4096
	//    (charging 65B per chunk) -> FLOW_CONTROL_ERROR reset and 0 leaked budget.
	wsDC2 := newMemDC(nil)
	onMsg2, onClose2, err := m.AttachDynamicWSDC(wsDC2)
	if err != nil {
		t.Fatal(err)
	}
	defer onClose2()

	onMsg2(hdrFrame)
	st2, err := m.AcceptStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()

	first1B, _ := (&protocol.Frame{Gen: 1, Type: protocol.TypeWSBinary, Flags: 0, Payload: []byte{'a'}}).Encode()
	onMsg2(first1B)
	if m.SessionMemUsed() < int64(1+PerChunkMemOverhead) {
		t.Fatalf("expected >= %d bytes charged for 1B chunk, got %d", 1+PerChunkMemOverhead, m.SessionMemUsed())
	}

	cont1B, _ := (&protocol.Frame{Gen: 1, Type: protocol.TypeWSCont, Flags: 0, Payload: []byte{'b'}}).Encode()
	for i := 0; i < MaxInboundQueueLen+10; i++ {
		onMsg2(cont1B)
		if st2.getResetErr() != nil {
			break
		}
	}
	if st2.getResetErr() == nil {
		t.Fatal("1B WS_CONT queue flood did not trigger FLOW_CONTROL_ERROR reset")
	}
	if m.SessionMemUsed() != 0 || globalBudget.Used() != 0 {
		t.Fatalf("1B WS_CONT flood reset leaked budget: sess=%d glob=%d", m.SessionMemUsed(), globalBudget.Used())
	}
}

func TestSlotNoPreemptionStrictFSM(t *testing.T) {
	globalBudget := NewMemoryBudget(16 * 1024 * 1024)
	m := New(Config{PoolSize: 1, GlobalBudget: globalBudget})
	defer m.Close()

	var sentMu sync.Mutex
	var resets []uint32
	dc := newMemDC(func(b []byte) {
		if f, err := protocol.Decode(b); err == nil && f.Type == protocol.TypeReset {
			sentMu.Lock()
			resets = append(resets, f.Gen)
			sentMu.Unlock()
		}
	})
	m.AttachPoolDC(0, dc)

	req1, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindHTTP, Method: "POST", Target: "/active",
	})
	hdr1, _ := (&protocol.Frame{Gen: 1, Type: protocol.TypeHeaders, Flags: 0, Payload: req1}).Encode()
	m.HandlePoolMessage(0, hdr1)

	st1, err := m.AcceptStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer st1.Close()

	// Malicious peer tries to preempt active Gen=1 with Gen=2 HEADERS on the same slot.
	hdr2, _ := (&protocol.Frame{Gen: 2, Type: protocol.TypeHeaders, Flags: protocol.FlagFin, Payload: req1}).Encode()
	m.HandlePoolMessage(0, hdr2)

	// Gen=1 must stay alive and receive its body cleanly.
	if st1.getResetErr() != nil {
		t.Fatalf("active Gen=1 stream was preempted by Gen=2 HEADERS: %v", st1.getResetErr())
	}
	data1, _ := (&protocol.Frame{Gen: 1, Type: protocol.TypeData, Flags: protocol.FlagFin, Payload: []byte("still-alive")}).Encode()
	m.HandlePoolMessage(0, data1)

	buf := make([]byte, 32)
	n, err := st1.Read(buf)
	if err != nil || string(buf[:n]) != "still-alive" {
		t.Fatalf("Gen=1 stream read failed after preemption attempt: n=%d data=%q err=%v", n, string(buf[:n]), err)
	}

	// Gen=2 must have received a RESET.
	deadline := time.Now().Add(time.Second)
	foundGen2Reset := false
	for time.Now().Before(deadline) {
		sentMu.Lock()
		for _, g := range resets {
			if g == 2 {
				foundGen2Reset = true
			}
		}
		sentMu.Unlock()
		if foundGen2Reset {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !foundGen2Reset {
		t.Fatal("expected RESET(Gen=2, PROTOCOL_ERROR) on preemption attempt")
	}
}

func TestOpenWSStreamEnforcesMaxWSConns(t *testing.T) {
	m := New(Config{PoolSize: 1, MaxWSConns: 2})
	defer m.Close()

	wsReq, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindWS, Method: "GET", Target: "/ws",
	})
	_, _, close1, err := m.OpenWSStream(context.Background(), newMemDC(nil), wsReq)
	if err != nil {
		t.Fatalf("ws 1 failed: %v", err)
	}
	_, _, close2, err := m.OpenWSStream(context.Background(), newMemDC(nil), wsReq)
	if err != nil {
		t.Fatalf("ws 2 failed: %v", err)
	}
	if _, _, _, err := m.OpenWSStream(context.Background(), newMemDC(nil), wsReq); err != ErrTooManyStreams {
		t.Fatalf("expected ErrTooManyStreams on 3rd WS stream, got %v", err)
	}
	close1()
	_, _, close3, err := m.OpenWSStream(context.Background(), newMemDC(nil), wsReq)
	if err != nil {
		t.Fatalf("ws 3 after closing ws 1 failed: %v", err)
	}
	close2()
	close3()
}

func TestWriteWSChunkStreaming(t *testing.T) {
	globalBudget := NewMemoryBudget(16 * 1024 * 1024)
	client, server := newConnectedPair(t, 1, globalBudget)
	defer client.Close()
	defer server.Close()

	cs, ss, cleanup := openConnectedWSPair(t, client, server, "/stream")
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ss.WriteWSChunk(ctx, protocol.TypeWSBinary, false, []byte("part1-")); err != nil {
		t.Fatal(err)
	}
	if err := ss.WriteWSChunk(ctx, protocol.TypeWSCont, true, []byte("part2")); err != nil {
		t.Fatal(err)
	}

	ch1, err := cs.ReadWSChunk(ctx)
	if err != nil || ch1.Op != protocol.TypeWSBinary || ch1.Fin || !bytes.Equal(ch1.Payload, []byte("part1-")) {
		t.Fatalf("chunk 1 mismatch: %+v err=%v", ch1, err)
	}
	ch2, err := cs.ReadWSChunk(ctx)
	if err != nil || ch2.Op != protocol.TypeWSCont || !ch2.Fin || !bytes.Equal(ch2.Payload, []byte("part2")) {
		t.Fatalf("chunk 2 mismatch: %+v err=%v", ch2, err)
	}
}
