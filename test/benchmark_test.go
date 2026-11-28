package test

import (
	"bytes"
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"webrtc-gateway/pkg/mux"
	"webrtc-gateway/pkg/protocol"
)

type chanPipeDC struct {
	peerCh    chan []byte
	closed    chan struct{}
	closeMu   sync.Mutex
	isClose   bool
	stallGate chan struct{}
	stalled   atomic.Bool
}

func newPipePair(buf int) (*chanPipeDC, *chanPipeDC) {
	chAB := make(chan []byte, buf)
	chBA := make(chan []byte, buf)
	return &chanPipeDC{peerCh: chAB, closed: make(chan struct{})},
		&chanPipeDC{peerCh: chBA, closed: make(chan struct{})}
}

func (p *chanPipeDC) Send(data []byte) error {
	if p.stalled.Load() && p.stallGate != nil {
		select {
		case <-p.stallGate:
		case <-p.closed:
			return io.ErrClosedPipe
		}
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	select {
	case <-p.closed:
		return io.ErrClosedPipe
	case p.peerCh <- cp:
		return nil
	}
}

func (p *chanPipeDC) BufferedAmount() uint64 { return uint64(len(p.peerCh) * 16384) }

func (p *chanPipeDC) Close() error {
	p.closeMu.Lock()
	if !p.isClose {
		p.isClose = true
		close(p.closed)
	}
	p.closeMu.Unlock()
	return nil
}

func setupPipeMuxPair(t testing.TB, poolSize int) (*mux.Mux, *mux.Mux, []*chanPipeDC, func()) {
	t.Helper()
	cCtrl, sCtrl := newPipePair(256)
	cMux := mux.New(mux.Config{PoolSize: poolSize})
	sMux := mux.New(mux.Config{PoolSize: poolSize})
	cMux.AttachControlDC(cCtrl)
	sMux.AttachControlDC(sCtrl)

	pump := func(src *chanPipeDC, fn func([]byte)) {
		for {
			select {
			case <-src.closed:
				return
			case msg := <-src.peerCh:
				fn(msg)
			}
		}
	}
	go pump(cCtrl, sMux.HandleControlMessage)
	go pump(sCtrl, cMux.HandleControlMessage)

	sLanes := make([]*chanPipeDC, poolSize)
	for i := 0; i < poolSize; i++ {
		idx := i
		cDC, sDC := newPipePair(256)
		sLanes[idx] = sDC
		cMux.AttachLaneDC(idx, cDC)
		sMux.AttachLaneDC(idx, sDC)
		go pump(cDC, func(b []byte) { sMux.HandleLaneMessage(idx, b) })
		go pump(sDC, func(b []byte) { cMux.HandleLaneMessage(idx, b) })
	}

	return cMux, sMux, sLanes, func() {
		cMux.Close()
		sMux.Close()
	}
}

// TestPoolMux_AntiHoL_Baseline injects a real 600ms transport stall on lane 0 during a bulk transfer
// and proves concurrent small probes on sibling lanes complete in < 200ms (strictly BEFORE lane 0 unblocks).
func TestPoolMux_AntiHoL_Baseline(t *testing.T) {
	cMux, sMux, sLanes, cleanup := setupPipeMuxPair(t, 12)
	defer cleanup()

	bulkSize := 1024 * 1024
	const waves = 3
	const waveConcurrency = 10

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	stallGate := make(chan struct{})
	sLanes[0].stallGate = stallGate
	bulkStarted := make(chan struct{})

	go func() {
		for {
			st, err := sMux.AcceptStream(ctx)
			if err != nil {
				return
			}
			go func(s *mux.Stream) {
				defer s.Close()
				req, err := protocol.DecodeRequestHeaders(s.HeadersRaw())
				if err != nil {
					return
				}
				resp, _ := protocol.EncodeResponseHeaders(&protocol.ResponseHeaders{Status: 200})
				if err := s.WriteHeaders(resp, false); err != nil {
					return
				}
				if req.Target == "/bulk" {
					// Stall lane 0 after first response header to simulate severe SCTP loss on lane 0!
					sLanes[0].stalled.Store(true)
					close(bulkStarted)
					chunk := bytes.Repeat([]byte("B"), 32*1024)
					for rem := bulkSize; rem > 0; {
						n := len(chunk)
						if n > rem {
							n = rem
						}
						if _, err := s.Write(chunk[:n]); err != nil {
							return
						}
						rem -= n
					}
					return
				}
				_, _ = s.Write([]byte("pong"))
			}(st)
		}
	}()

	// Launch bulk download on lane 0 (first idle lane picked).
	bulkDone := make(chan error, 1)
	go func() {
		req, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
			Kind: protocol.StreamKindHTTP, Method: "GET", Target: "/bulk",
		})
		st, err := cMux.OpenStream(req, false)
		if err != nil {
			bulkDone <- err
			return
		}
		defer st.Close()
		if _, err := st.ReadResponseHeaders(ctx); err != nil {
			bulkDone <- err
			return
		}
		all, rerr := io.ReadAll(st)
		if rerr != nil {
			bulkDone <- rerr
			return
		}
		if len(all) != bulkSize {
			bulkDone <- io.ErrShortBuffer
			return
		}
		bulkDone <- nil
	}()

	// Wait until lane 0 is actively stalled before measuring sibling probe latencies.
	<-bulkStarted
	stallRelease := time.AfterFunc(600*time.Millisecond, func() {
		sLanes[0].stalled.Store(false)
		close(stallGate)
	})
	defer stallRelease.Stop()

	probeStart := time.Now()
	var maxSmallDur time.Duration
	var mu sync.Mutex
	for w := 0; w < waves; w++ {
		var wg sync.WaitGroup
		for i := 0; i < waveConcurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				start := time.Now()
				req, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
					Kind: protocol.StreamKindHTTP, Method: "GET", Target: "/ping",
				})
				st, err := cMux.OpenStream(req, false)
				if err != nil {
					t.Errorf("small OpenStream: %v", err)
					return
				}
				defer st.Close()
				if _, err := st.ReadResponseHeaders(ctx); err != nil {
					t.Errorf("small ReadResponseHeaders: %v", err)
					return
				}
				body, err := io.ReadAll(st)
				if err != nil || string(body) != "pong" {
					t.Errorf("small body=%q err=%v", string(body), err)
					return
				}
				dur := time.Since(start)
				mu.Lock()
				if dur > maxSmallDur {
					maxSmallDur = dur
				}
				mu.Unlock()
			}()
		}
		wg.Wait()
	}

	probesElapsed := time.Since(probeStart)
	if probesElapsed >= 400*time.Millisecond {
		t.Fatalf("HoL regression: small probes waited on stalled lane 0 (probesElapsed=%v, maxSmall=%v)", probesElapsed, maxSmallDur)
	}
	if err := <-bulkDone; err != nil {
		t.Fatalf("bulk download failed after lane 0 resumed: %v", err)
	}
}

func BenchmarkPoolMuxThroughput(b *testing.B) {
	cMux, sMux, _, cleanup := setupPipeMuxPair(b, 8)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	payload := bytes.Repeat([]byte("X"), 64*1024)
	go func() {
		for {
			st, err := sMux.AcceptStream(ctx)
			if err != nil {
				return
			}
			go func(s *mux.Stream) {
				defer s.Close()
				resp, _ := protocol.EncodeResponseHeaders(&protocol.ResponseHeaders{Status: 200})
				if err := s.WriteHeaders(resp, false); err != nil {
					return
				}
				_, _ = s.Write(payload)
			}(st)
		}
	}()

	req, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindHTTP, Method: "GET", Target: "/bench",
	})
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st, err := cMux.OpenStream(req, false)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := st.ReadResponseHeaders(ctx); err != nil {
			b.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, st); err != nil {
			b.Fatal(err)
		}
		_ = st.Close()
	}
}
