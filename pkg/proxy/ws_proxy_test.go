package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"webrtc-gateway/pkg/protocol"
)

func TestProxyV7_WSProxy_NoDeadlockAndBlocks302Escape(t *testing.T) {
	// 302 redirect on /public/redir must not follow into /admin/ws.
	var hitAdminWS atomic.Bool
	wsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/public/redir" {
			http.Redirect(w, r, "/admin/ws", http.StatusFound)
			return
		}
		if r.URL.Path == "/admin/ws" {
			hitAdminWS.Store(true)
			c, err := websocket.Accept(w, r, nil)
			if err == nil {
				_ = c.Close(websocket.StatusNormalClosure, "pwned")
			}
			return
		}
		if r.Header.Get("X-Forwarded-Host") != "" {
			t.Errorf("X-Forwarded-Host not stripped")
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
		_ = c.Close(websocket.StatusNormalClosure, "bye")
	}))
	defer wsSrv.Close()

	pol, _ := NewDestinationPolicy(wsSrv.URL, "")
	wsp, _ := NewWSProxy(WSProxyOptions{Policy: pol})

	// Test 1: 302 redirect must return 302 and never touch /admin/ws.
	cm, sm := newTestMuxPair(t)

	doneRedir := make(chan struct{})
	go func() {
		defer close(doneRedir)
		st, err := sm.AcceptStream(context.Background())
		if err != nil {
			return
		}
		reqHdr, _ := protocol.DecodeRequestHeaders(st.HeadersRaw())
		wsp.HandleStream(st, reqHdr, "198.51.100.1", NewProxyCookieJar(), &CapabilityScope{PathPrefix: "/public"})
	}()

	reqTLV, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindWS, Method: "GET", Target: "/public/redir",
	})
	cs, err := cm.OpenStream(reqTLV, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	respBytes, err := cs.ReadResponseHeaders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := protocol.DecodeResponseHeaders(respBytes)
	if resp.Status != http.StatusFound || hitAdminWS.Load() {
		t.Fatalf("WSProxy followed 302 redirect: status=%d hitAdminWS=%v", resp.Status, hitAdminWS.Load())
	}
	<-doneRedir

	// Test 2: Partial WS frame + backend close exits promptly without deadlock.
	doneClose := make(chan struct{})
	go func() {
		defer close(doneClose)
		st, err := sm.AcceptStream(context.Background())
		if err != nil {
			return
		}
		reqHdr, _ := protocol.DecodeRequestHeaders(st.HeadersRaw())
		wsp.HandleStream(st, reqHdr, "198.51.100.1", NewProxyCookieJar(), nil)
	}()

	reqTLV2, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindWS, Method: "GET", Target: "/ws",
		Headers: []protocol.HeaderField{{Name: "x-forwarded-host", Value: "evil.example.com"}},
	})
	cs2, err := cm.OpenStream(reqTLV2, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cs2.Close()

	rb2, err := cs2.ReadResponseHeaders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resp2, _ := protocol.DecodeResponseHeaders(rb2)
	if resp2.Status != http.StatusSwitchingProtocols {
		t.Fatalf("expected 101, got %d", resp2.Status)
	}
	select {
	case <-doneClose:
	case <-time.After(3 * time.Second):
		t.Fatal("WSProxy hung after backend close")
	}
}

func TestWSProxyStreamingCloseCodesAndDefaultOrigin(t *testing.T) {
	// Verify validWSCloseCode preserves RFC 6455 wire-valid codes and blocks wire-forbidden codes.
	for _, valid := range []int{1000, 1001, 1002, 1003, 1007, 1008, 1009, 1010, 1011, 1012, 1013, 1014, 3000, 4999} {
		if got := validWSCloseCode(valid); int(got) != valid {
			t.Fatalf("RFC 6455 valid close code %d was altered to %d", valid, got)
		}
	}
	for _, forbidden := range []int{-1, 999, 1004, 1005, 1006, 1015, 2999, 5000, 70000} {
		if got := validWSCloseCode(forbidden); got != uint16(websocket.StatusNormalClosure) {
			t.Fatalf("RFC 6455 wire-forbidden close code %d leaked as %d", forbidden, got)
		}
	}

	var gotOrigin string
	wsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOrigin = r.Header.Get("Origin")
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		if r.URL.Path == "/close1008" {
			_ = c.Close(websocket.StatusPolicyViolation, "policy-denied")
			return
		}
		// Stream a 96 KiB binary message back to test 16 KiB multi-chunk O(1) streaming.
		payload := bytes.Repeat([]byte{0xde, 0xad, 0xbe, 0xef}, 24*1024)
		_ = c.Write(r.Context(), websocket.MessageBinary, payload)
		_ = c.Close(websocket.StatusNormalClosure, "ok")
	}))
	defer wsSrv.Close()

	pol, _ := NewDestinationPolicy(wsSrv.URL, "")
	wsp, _ := NewWSProxy(WSProxyOptions{Policy: pol})

	openWS := func(target string) (uint16, []byte, uint16, string) {
		cm, sm := newTestMuxPair(t)

		done := make(chan struct{})
		go func() {
			defer close(done)
			st, err := sm.AcceptStream(context.Background())
			if err != nil {
				return
			}
			rh, _ := protocol.DecodeRequestHeaders(st.HeadersRaw())
			wsp.HandleStream(st, rh, "198.51.100.5", NewProxyCookieJar(), nil)
		}()

		// Intentionally omit Origin header; WSProxy must inject EffectiveOrigin for SameOriginClient.
		reqTLV, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
			Kind: protocol.StreamKindWS, FetchCtx: protocol.FetchCtxSameOriginClient, Method: "GET", Target: target,
		})
		cs, err := cm.OpenStream(reqTLV, true)
		if err != nil {
			t.Fatal(err)
		}
		defer cs.Close()

		rb, err := cs.ReadResponseHeaders(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		rh, _ := protocol.DecodeResponseHeaders(rb)

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var buf bytes.Buffer
		var closeCode uint16
		var closeReason string
		for {
			ch, rerr := cs.ReadWSChunk(ctx)
			if rerr != nil {
				break
			}
			if ch.Op == protocol.TypeWSClose {
				closeCode, closeReason, _ = protocol.DecodeWSClosePayload(ch.Payload)
				break
			}
			buf.Write(ch.Payload)
		}
		<-done
		return rh.Status, buf.Bytes(), closeCode, closeReason
	}

	st1, _, cc1, cr1 := openWS("/close1008")
	if st1 != 101 || cc1 != 1008 || cr1 != "policy-denied" {
		t.Fatalf("expected 1008 policy-denied close passthrough, got status=%d code=%d reason=%q", st1, cc1, cr1)
	}
	if gotOrigin != pol.EffectiveOrigin() {
		t.Fatalf("expected default WS Origin %q, got %q", pol.EffectiveOrigin(), gotOrigin)
	}

	st2, data2, cc2, _ := openWS("/stream96k")
	want := bytes.Repeat([]byte{0xde, 0xad, 0xbe, 0xef}, 24*1024)
	if st2 != 101 || cc2 != 1000 || !bytes.Equal(data2, want) {
		t.Fatalf("streaming 96 KiB WS mismatch: status=%d code=%d len=%d", st2, cc2, len(data2))
	}
}

func TestRegression_P1_WSProxyAbruptBackendCloseNeverEmitsTypeDataFin(t *testing.T) {
	wsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// Abruptly close TCP without sending RFC 6455 close frame!
		_ = c.CloseNow()
	}))
	defer wsSrv.Close()

	pol, _ := NewDestinationPolicy(wsSrv.URL, "")
	wsp, _ := NewWSProxy(WSProxyOptions{Policy: pol})
	cm, sm := newTestMuxPair(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		st, err := sm.AcceptStream(context.Background())
		if err != nil {
			return
		}
		rh, _ := protocol.DecodeRequestHeaders(st.TakeHeadersRaw())
		wsp.HandleStream(st, rh, "198.51.100.5", NewProxyCookieJar(), nil)
	}()

	reqTLV, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindWS, Method: "GET", Target: "/abrupt",
	})
	cs, err := cm.OpenStream(reqTLV, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	if _, err := cs.ReadResponseHeaders(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Reading WS chunk must terminate cleanly with an error (reset/EOF) and NEVER hang or panic.
	_, rerr := cs.ReadWSChunk(ctx)
	if rerr == nil {
		t.Fatal("expected error on abrupt backend WS termination")
	}
	<-done
}
