package test

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"webrtc-gateway/pkg/agent"
	"webrtc-gateway/pkg/signaling"
)

// TestTLSSignalingAndAgentConnect verifies that the agent can connect to a TLS (WSS) signaling endpoint,
// both with and without skipping certificate verification for self-signed certificates.
func TestTLSSignalingAndAgentConnect(t *testing.T) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	regCh := make(chan string, 1)

	// Create a TLS test server with auto-generated self-signed cert
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Logf("WS upgrade error: %v", err)
				return
			}
			defer conn.Close()

			var msg signaling.Message
			if err := conn.ReadJSON(&msg); err == nil {
				if msg.Type == signaling.TypeRegister {
					regCh <- msg.NodeID
					_ = conn.WriteJSON(&signaling.Message{
						Type:   signaling.TypeRegistered,
						NodeID: msg.NodeID,
					})
				}
			}
		}
	}))
	defer server.Close()

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("Failed to parse test server URL: %v", err)
	}

	wssURL := "wss://" + u.Host + "/ws"
	t.Logf("Test TLS Signaling URL: %s", wssURL)

	// 1. Verify standard dialer fails on self-signed cert without insecure flag
	dialerStrict := *websocket.DefaultDialer
	_, _, err = dialerStrict.Dial(wssURL, nil)
	if err == nil {
		t.Errorf("Expected TLS certificate verification failure on self-signed cert, but succeeded")
	} else {
		t.Logf("Strict TLS correctly rejected self-signed cert: %v", err)
	}

	// 2. Verify agent with InsecureTLS=true connects over WSS successfully
	ag := agent.New("tls-test-node", wssURL, "", "http://127.0.0.1:8080")
	ag.SetInsecureTLS(true)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() {
		_ = ag.Start(ctx)
	}()

	select {
	case nodeID := <-regCh:
		if nodeID != "tls-test-node" {
			t.Fatalf("Expected registered nodeID tls-test-node, got %s", nodeID)
		}
		t.Logf("Agent successfully connected and registered over WSS (TLS) to node %s!", nodeID)
	case <-time.After(3 * time.Second):
		t.Fatalf("Timeout waiting for agent to connect over WSS")
	}

	// Clean up
	ag.Close()

	// 3. Test HTTP client accessing HTTPS portal
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{Transport: tr}
	resp, err := client.Get(server.URL + "/health")
	if err != nil {
		t.Fatalf("Failed to GET HTTPS server: %v", err)
	}
	defer resp.Body.Close()
	t.Logf("HTTPS GET response status: %d", resp.StatusCode)
}
