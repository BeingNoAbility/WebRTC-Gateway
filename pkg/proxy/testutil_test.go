package proxy

import (
	"sync"
	"sync/atomic"
	"testing"

	"webrtc-gateway/pkg/mux"
	"webrtc-gateway/pkg/protocol"
)

type testPipe struct {
	peer   *testPipe
	inbox  chan []byte
	stopCh chan struct{}
	closed atomic.Bool
	once   sync.Once
}

func newTestPipePair() (*testPipe, *testPipe) {
	a := &testPipe{inbox: make(chan []byte, 4096), stopCh: make(chan struct{})}
	b := &testPipe{inbox: make(chan []byte, 4096), stopCh: make(chan struct{})}
	a.peer = b
	b.peer = a
	return a, b
}

func (p *testPipe) start(fn func([]byte)) {
	go func() {
		for {
			select {
			case <-p.stopCh:
				return
			case b := <-p.inbox:
				fn(b)
			}
		}
	}()
}

func (p *testPipe) Send(data []byte) error {
	if p.closed.Load() {
		return mux.ErrMuxClosed
	}
	cp := append([]byte(nil), data...)
	select {
	case <-p.peer.stopCh:
		return mux.ErrMuxClosed
	case p.peer.inbox <- cp:
		return nil
	}
}

func (p *testPipe) BufferedAmount() uint64 { return 0 }

func (p *testPipe) Close() error {
	p.closed.Store(true)
	p.once.Do(func() { close(p.stopCh) })
	return nil
}

func newTestMuxPair(t *testing.T) (*mux.Mux, *mux.Mux) {
	t.Helper()
	cm := mux.New(mux.Config{PoolSize: 4})
	sm := mux.New(mux.Config{PoolSize: 4})
	cc, sc := newTestPipePair()
	cm.AttachControlDC(cc)
	sm.AttachControlDC(sc)
	cc.start(cm.HandleControlMessage)
	sc.start(sm.HandleControlMessage)

	for i := 0; i < 4; i++ {
		idx := i
		cDC, sDC := newTestPipePair()
		cm.AttachLaneDC(idx, cDC)
		sm.AttachLaneDC(idx, sDC)
		cDC.start(func(b []byte) { cm.HandleLaneMessage(idx, b) })
		sDC.start(func(b []byte) { sm.HandleLaneMessage(idx, b) })
	}
	t.Cleanup(func() {
		cm.Close()
		sm.Close()
	})
	return cm, sm
}

func mustReqTLV(method, target string) []byte {
	b, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindHTTP, Method: method, Target: target,
	})
	return b
}
