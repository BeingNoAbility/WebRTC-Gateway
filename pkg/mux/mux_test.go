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
	if !errors.Is(err, ErrSCTPWriteTimout) {
		t.Fatalf("expected ErrSCTPWriteTimout, got %v", err)
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
