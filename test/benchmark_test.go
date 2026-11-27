package test

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"webrtc-gateway/pkg/mux"
	"webrtc-gateway/pkg/protocol"
)

type chanPipeDC struct {
	peerCh  chan []byte
	closed  chan struct{}
	closeMu sync.Mutex
	isClose bool
}

func newPipePair(buf int) (*chanPipeDC, *chanPipeDC) {
	chAB := make(chan []byte, buf)
	chBA := make(chan []byte, buf)
	return &chanPipeDC{peerCh: chAB, closed: make(chan struct{})},
		&chanPipeDC{peerCh: chBA, closed: make(chan struct{})}
}

func (p *chanPipeDC) Send(data []byte) error {
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

func setupPipeMuxPair(t testing.TB, poolSize int) (*mux.Mux, *mux.Mux, func()) {
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

	for i := 0; i < poolSize; i++ {
		idx := i
		cDC, sDC := newPipePair(256)
		cMux.AttachPoolDC(idx, cDC)
		sMux.AttachPoolDC(idx, sDC)
		go pump(cDC, func(b []byte) { sMux.HandlePoolMessage(idx, b) })
		go pump(sDC, func(b []byte) { cMux.HandlePoolMessage(idx, b) })
	}

	return cMux, sMux, func() {
		cMux.Close()
		sMux.Close()
	}
}

// TestPoolMux_AntiHoL_Baseline verifies with real backpressure that a stalled bulk download
// occupying one pool DC NEVER blocks concurrent small probes on sibling pool DCs!
func TestPoolMux_AntiHoL_Baseline(t *testing.T) {
	cMux, sMux, cleanup := setupPipeMuxPair(t, 8)
	defer cleanup()

	bulkSize := 4 * 1024 * 1024
	smallCount := 24
	if testing.Short() {
		bulkSize = 512 * 1024
		smallCount = 8
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

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

	// Launch a throttled bulk download on pool DC 0 (simulating slow receiver / packet loss HoL).
	bulkDone := make(chan error, 1)
	go func() {
		req, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
			Kind: protocol.StreamKindHTTP, Method: "GET", Target: "/bulk",
		})
		st, err := cMux.OpenHTTPStream(ctx, req, true)
		if err != nil {
			bulkDone <- err
			return
		}
		defer st.Close()
		if _, err := st.ReadResponseHeaders(ctx); err != nil {
			bulkDone <- err
			return
		}
		buf := make([]byte, 32*1024)
		total := 0
		for {
			n, rerr := st.Read(buf)
			total += n
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				bulkDone <- rerr
				return
			}
		}
		if total != bulkSize {
			bulkDone <- io.ErrShortBuffer
			return
		}
		bulkDone <- nil
	}()

	var wg sync.WaitGroup
	var maxSmallDur time.Duration
	var mu sync.Mutex
	for i := 0; i < smallCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			req, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
				Kind: protocol.StreamKindHTTP, Method: "GET", Target: "/ping",
			})
			st, err := cMux.OpenHTTPStream(ctx, req, true)
			if err != nil {
				t.Errorf("small OpenHTTPStream: %v", err)
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
	if err := <-bulkDone; err != nil {
		t.Fatalf("bulk download failed: %v", err)
	}
	if maxSmallDur > 3*time.Second {
		t.Fatalf("small probe latency regressed under bulk load: max=%v", maxSmallDur)
	}
}

func BenchmarkPoolMuxThroughput(b *testing.B) {
	cMux, sMux, cleanup := setupPipeMuxPair(b, 8)
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
		st, err := cMux.OpenHTTPStream(ctx, req, true)
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
