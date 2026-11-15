package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"webrtc-gateway/pkg/signaling"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func TestAgent_ConfigAndLifecycle(t *testing.T) {
	ag := New("test-agent-node", "ws://127.0.0.1:8888/ws", "pass123", "http://127.0.0.1:8080")
	if ag.NodeID != "test-agent-node" || ag.Secret != "pass123" || ag.Target != "http://127.0.0.1:8080" {
		t.Fatalf("Agent initialization fields mismatch")
	}

	// Test Proxy setter
	ag.SetProxy("socks5://127.0.0.1:1080")
	if ag.ProxyURL != "socks5://127.0.0.1:1080" {
		t.Errorf("ProxyURL not set properly")
	}

	// Test Insecure TLS setter
	ag.SetInsecureTLS(true)
	if !ag.InsecureTLS {
		t.Errorf("InsecureTLS not set to true")
	}

	// Test Close on idle agent
	ag.Close()
}

func TestAgent_RegistrationAndReconnection(t *testing.T) {
	registeredCh := make(chan string, 1)

	// Mock signaling server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		var msg signaling.Message
		if err := conn.ReadJSON(&msg); err == nil {
			if msg.Type == signaling.TypeRegister {
				registeredCh <- msg.NodeID
				_ = conn.WriteJSON(&signaling.Message{
					Type:   signaling.TypeRegistered,
					NodeID: msg.NodeID,
				})
			}
		}

		// Keep connection open briefly
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	wsURL := "ws://" + u.Host + "/ws"

	ag := New("agent-mock-node", wsURL, "secret-tok", "http://127.0.0.1:8080")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = ag.Start(ctx)
	}()

	select {
	case nodeID := <-registeredCh:
		if nodeID != "agent-mock-node" {
			t.Errorf("Registered nodeID mismatch: %q", nodeID)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Timeout waiting for agent registration")
	}

	cancel()
	ag.Close()
}

func TestAgent_RejectUnauthorizedOffer(t *testing.T) {
	offerRejectedCh := make(chan string, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		for {
			var msg signaling.Message
			if err := conn.ReadJSON(&msg); err != nil {
				break
			}

			if msg.Type == signaling.TypeRegister {
				_ = conn.WriteJSON(&signaling.Message{
					Type:   signaling.TypeRegistered,
					NodeID: msg.NodeID,
				})

				// Send Offer with wrong secret
				_ = conn.WriteJSON(&signaling.Message{
					Type:      signaling.TypeOffer,
					SessionID: "client-sess-hacker",
					NodeID:    msg.NodeID,
					Secret:    "WRONG_SECRET",
				})
			} else if msg.Type == signaling.TypeError {
				if strings.Contains(msg.Error, "unauthorized") {
					offerRejectedCh <- msg.Error
					break
				}
			}
		}
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	wsURL := "ws://" + u.Host + "/ws"

	ag := New("protected-node", wsURL, "SUPER_SECRET_KEY", "http://127.0.0.1:8080")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = ag.Start(ctx)
	}()

	select {
	case errMsg := <-offerRejectedCh:
		if !strings.Contains(errMsg, "unauthorized") {
			t.Errorf("Expected unauthorized error, got: %s", errMsg)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Timeout waiting for agent to reject unauthorized offer")
	}

	cancel()
	ag.Close()
}
