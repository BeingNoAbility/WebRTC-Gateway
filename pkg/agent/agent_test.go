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

func TestAgent_HMACShareSignature(t *testing.T) {
	nodeID := "test-home"
	secret := "my-secret-key-123"
	ag := New(nodeID, "ws://127.0.0.1:8888/ws", secret, "http://127.0.0.1:8080")
	defer ag.Close()

	// 1. Direct secret token verification
	if !ag.VerifyAuth(nodeID, secret, secret, 0, "") {
		t.Fatalf("Direct secret should be authorized")
	}
	if ag.VerifyAuth(nodeID, secret, "wrong-secret", 0, "") {
		t.Fatalf("Wrong direct secret should be rejected")
	}

	// 2. Valid HMAC signature verification (future expiration)
	futureExp := time.Now().Add(24 * time.Hour).Unix()
	validSign := GenerateShareSignature(nodeID, futureExp, secret)
	if !ag.VerifyAuth(nodeID, secret, "", futureExp, validSign) {
		t.Fatalf("Valid HMAC signature was rejected")
	}

	// 3. Expired HMAC signature verification (past expiration)
	pastExp := time.Now().Add(-2 * time.Hour).Unix()
	expiredSign := GenerateShareSignature(nodeID, pastExp, secret)
	if ag.VerifyAuth(nodeID, secret, "", pastExp, expiredSign) {
		t.Fatalf("Expired HMAC signature should have been rejected")
	}

	// 4. Tampered signature verification
	tamperedSign := validSign[:len(validSign)-2] + "ff"
	if ag.VerifyAuth(nodeID, secret, "", futureExp, tamperedSign) {
		t.Fatalf("Tampered signature should have been rejected")
	}

	// 5. Signature for different nodeID
	otherNodeSign := GenerateShareSignature("other-node", futureExp, secret)
	if ag.VerifyAuth(nodeID, secret, "", futureExp, otherNodeSign) {
		t.Fatalf("Signature for other node should have been rejected")
	}

	// 6. GenerateShareLink formatting
	link := GenerateShareLink("https://gateway.example.com", nodeID, secret, 1*time.Hour)
	if !strings.HasPrefix(link, "https://gateway.example.com/#test-home?exp=") || !strings.Contains(link, "&sign=") {
		t.Fatalf("GenerateShareLink formatted incorrectly: %s", link)
	}
}

func TestAgent_TeardownGraceWindow(t *testing.T) {
	ag := New("test-grace", "ws://127.0.0.1:8888/ws", "", "http://127.0.0.1:8080")
	defer ag.Close()

	sessionID := "test-session-123"
	// Schedule grace teardown
	ag.schedulePendingTeardown(sessionID, sessionID, nil)

	ag.pendingMu.Lock()
	timer, exists := ag.pendingTeardowns[sessionID]
	ag.pendingMu.Unlock()

	if !exists || timer == nil {
		t.Fatalf("Expected pending teardown timer to exist")
	}

	// Cancel grace teardown (e.g. ICE Restart Offer arrived)
	ag.cancelPendingTeardown(sessionID)

	ag.pendingMu.Lock()
	_, existsAfterCancel := ag.pendingTeardowns[sessionID]
	ag.pendingMu.Unlock()

	if existsAfterCancel {
		t.Fatalf("Expected pending teardown timer to be removed after cancel")
	}
}
