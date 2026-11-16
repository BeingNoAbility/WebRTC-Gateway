package test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"webrtc-gateway/pkg/agent"
	"webrtc-gateway/pkg/signaling"
)

func TestE2E_WebSocketDualKeepalive(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	var serverReceivedPings int64
	var serverSentPings int64

	type testSession struct {
		conn *websocket.Conn
		mu   sync.Mutex
	}

	testReadTimeout := 250 * time.Millisecond
	testPingPeriod := 50 * time.Millisecond

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		sess := &testSession{conn: conn}

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		defer func() {
			sess.mu.Lock()
			_ = conn.Close()
			sess.conn = nil
			sess.mu.Unlock()
		}()

		// Keepalive: synchronized Pong handler to extend ReadDeadline
		conn.SetPongHandler(func(string) error {
			_ = conn.SetReadDeadline(time.Now().Add(testReadTimeout))
			return nil
		})

		// Keepalive: synchronized Ping handler to reply Pong under mutex
		conn.SetPingHandler(func(appData string) error {
			atomic.AddInt64(&serverReceivedPings, 1)
			sess.mu.Lock()
			defer sess.mu.Unlock()
			if sess.conn == nil {
				return nil
			}
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
			err := conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
			_ = conn.SetReadDeadline(time.Now().Add(testReadTimeout))
			return err
		})

		// Server-side Ping Pump
		go func() {
			ticker := time.NewTicker(testPingPeriod)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					sess.mu.Lock()
					if sess.conn != nil {
						atomic.AddInt64(&serverSentPings, 1)
						_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
						_ = conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(time.Second))
					}
					sess.mu.Unlock()
				}
			}
		}()

		for {
			_ = conn.SetReadDeadline(time.Now().Add(testReadTimeout))
			var msg signaling.Message
			if err := conn.ReadJSON(&msg); err != nil {
				break
			}
			if msg.Type == signaling.TypeRegister {
				sess.mu.Lock()
				_ = conn.WriteJSON(&signaling.Message{
					Type:   signaling.TypeRegistered,
					NodeID: msg.NodeID,
				})
				sess.mu.Unlock()
			}
		}
	}))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

	ag := agent.New("keepalive-e2e-node", wsURL, "secret", "", "http://127.0.0.1:8080")
	ag.SetPingInterval(testPingPeriod)
	ag.SetReadTimeout(testReadTimeout)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = ag.Start(ctx)
	}()

	// Wait 500ms (twice the read deadline of 250ms)
	time.Sleep(500 * time.Millisecond)

	recPings := atomic.LoadInt64(&serverReceivedPings)
	sentPings := atomic.LoadInt64(&serverSentPings)

	if recPings < 3 {
		t.Errorf("Expected server to receive at least 3 pings from agent, got %d", recPings)
	}
	if sentPings < 3 {
		t.Errorf("Expected server to send at least 3 pings to agent, got %d", sentPings)
	}

	cancel()
	ag.Close()
}
