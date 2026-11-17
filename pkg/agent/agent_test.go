package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"webrtc-gateway/pkg/proxy"
	"webrtc-gateway/pkg/signaling"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func TestAgent_ConfigAndLifecycle(t *testing.T) {
	ag := New("test-agent-node", "ws://127.0.0.1:8888/ws", "sig-pass", "access-pass", "http://127.0.0.1:8080")
	if ag.NodeID != "test-agent-node" || ag.SignalSecret != "sig-pass" || ag.AccessSecret != "access-pass" || ag.Target != "http://127.0.0.1:8080" {
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

	ag := New("agent-mock-node", wsURL, "secret-tok", "", "http://127.0.0.1:8080")

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

	ag := New("protected-node", wsURL, "", "SUPER_SECRET_KEY", "http://127.0.0.1:8080")

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
	ag := New(nodeID, "ws://127.0.0.1:8888/ws", "", secret, "http://127.0.0.1:8080")
	defer ag.Close()

	// 1. Direct secret token verification
	if !ag.VerifyAuth(nodeID, secret, secret, 0, "", "") {
		t.Fatalf("Direct secret should be authorized")
	}
	if ag.VerifyAuth(nodeID, secret, "wrong-secret", 0, "", "") {
		t.Fatalf("Wrong direct secret should be rejected")
	}

	// 2. Valid HMAC signature verification (future expiration, legacy share link)
	futureExp := time.Now().Add(24 * time.Hour).Unix()
	validSign := GenerateShareSignature(nodeID, futureExp, secret)
	if !ag.VerifyAuth(nodeID, secret, "", futureExp, validSign, "") {
		t.Fatalf("Valid HMAC signature was rejected")
	}

	// 3. Expired HMAC signature verification (past expiration)
	pastExp := time.Now().Add(-2 * time.Hour).Unix()
	expiredSign := GenerateShareSignature(nodeID, pastExp, secret)
	if ag.VerifyAuth(nodeID, secret, "", pastExp, expiredSign, "") {
		t.Fatalf("Expired HMAC signature should have been rejected")
	}

	// 4. Tampered signature verification
	tamperedSign := validSign[:len(validSign)-2] + "ff"
	if ag.VerifyAuth(nodeID, secret, "", futureExp, tamperedSign, "") {
		t.Fatalf("Tampered signature should have been rejected")
	}

	// 5. Signature for different nodeID
	otherNodeSign := GenerateShareSignature("other-node", futureExp, secret)
	if ag.VerifyAuth(nodeID, secret, "", futureExp, otherNodeSign, "") {
		t.Fatalf("Signature for other node should have been rejected")
	}

	// 6. F-04: SDP DTLS Certificate Fingerprint HMAC Proof & Replay Defense
	dtlsFingerprint := "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99"
	authKey := ComputeHMAC(secret, "node-auth:"+nodeID)
	fpSign := ComputeHMAC(authKey, fmt.Sprintf("%s:%d:%s", nodeID, futureExp, dtlsFingerprint))

	// Legitimate client with matching DTLS fingerprint
	if !ag.VerifyAuth(nodeID, secret, "", futureExp, fpSign, dtlsFingerprint) {
		t.Fatalf("Valid SDP fingerprint HMAC signature was rejected")
	}

	// Attacker attempts to replay the signature with a different DTLS fingerprint
	attackerFingerprint := "11:22:33:44:55:66:77:88:99:00:AA:BB:CC:DD:EE:FF:11:22:33:44:55:66:77:88:99:00:AA:BB:CC:DD:EE:FF"
	if ag.VerifyAuth(nodeID, secret, "", futureExp, fpSign, attackerFingerprint) {
		t.Fatalf("Replay attack with mismatched DTLS fingerprint must be rejected!")
	}

	// 6. GenerateShareLink formatting
	link := GenerateShareLink("https://gateway.example.com", nodeID, secret, 1*time.Hour)
	if !strings.HasPrefix(link, "https://gateway.example.com/#test-home?exp=") || !strings.Contains(link, "&sign=") {
		t.Fatalf("GenerateShareLink formatted incorrectly: %s", link)
	}
}

func TestAgent_TeardownGraceWindow(t *testing.T) {
	ag := New("test-grace", "ws://127.0.0.1:8888/ws", "", "", "http://127.0.0.1:8080")
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

func TestAgent_ConcurrentStreamsCapacity(t *testing.T) {
	ag := New("test-capacity-node", "ws://127.0.0.1:8888/ws", "", "", "http://127.0.0.1:8080")
	defer ag.Close()

	ag.SetMaxConcurrentStreams(2)
	if ag.MaxConcurrentStreams() != 2 {
		t.Fatalf("Expected MaxConcurrentStreams to be 2, got %d", ag.MaxConcurrentStreams())
	}

	// Manipulate activeStreams to verify threshold accounting
	atomic.StoreInt64(&ag.activeStreams, 2)

	if ag.ActiveStreams() != 2 {
		t.Errorf("Expected 2 active streams, got %d", ag.ActiveStreams())
	}

	metrics := ag.ExportPrometheusMetrics()
	if !strings.Contains(metrics, `webrtc_gateway_max_streams{node_id="test-capacity-node"} 2`) {
		t.Errorf("Prometheus metrics missing max_streams 2:\n%s", metrics)
	}
	if !strings.Contains(metrics, `webrtc_gateway_active_streams{node_id="test-capacity-node"} 2`) {
		t.Errorf("Prometheus metrics missing active_streams 2:\n%s", metrics)
	}
}

func TestAgent_FileAndDirectoryMode(t *testing.T) {
	tempDir := t.TempDir()
	sampleFile := filepath.Join(tempDir, "shared_document.pdf")
	_ = os.WriteFile(sampleFile, []byte("%PDF-1.4 sample pdf content"), 0644)

	// 1. Single file mode
	agFile := New("test-file-node", "ws://127.0.0.1:8888/ws", "sig-secret", "visitor-secret", "")
	defer agFile.Close()

	if err := agFile.SetSingleFile(sampleFile); err != nil {
		t.Fatalf("SetSingleFile failed: %v", err)
	}
	if agFile.FileProxy() == nil {
		t.Fatalf("Expected FileProxy to be initialized")
	}
	if agFile.FileProxy().Mode() != proxy.FileModeSingleFile {
		t.Errorf("Expected FileModeSingleFile, got %v", agFile.FileProxy().Mode())
	}
	if agFile.FileProxy().FileName() != "shared_document.pdf" {
		t.Errorf("Expected FileName shared_document.pdf, got %q", agFile.FileProxy().FileName())
	}

	// 2. Directory mode
	agDir := New("test-dir-node", "ws://127.0.0.1:8888/ws", "sig-secret", "", "")
	defer agDir.Close()

	if err := agDir.SetDirectory(tempDir); err != nil {
		t.Fatalf("SetDirectory failed: %v", err)
	}
	if agDir.FileProxy() == nil {
		t.Fatalf("Expected FileProxy to be initialized")
	}
	if agDir.FileProxy().Mode() != proxy.FileModeDirectory {
		t.Errorf("Expected FileModeDirectory, got %v", agDir.FileProxy().Mode())
	}
}

func TestAgent_KeepalivePingPump(t *testing.T) {
	pingsReceived := int64(0)
	serverClosed := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		defer close(serverClosed)

		conn.SetPingHandler(func(appData string) error {
			atomic.AddInt64(&pingsReceived, 1)
			_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
			return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
		})

		// Server enforces a 200ms read deadline.
		// If Agent doesn't send Pings, the connection will drop after 200ms.
		// If Agent sends Pings every 40ms, the read deadline is refreshed and connection stays alive!
		for {
			_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			var msg signaling.Message
			if err := conn.ReadJSON(&msg); err != nil {
				break
			}
			if msg.Type == signaling.TypeRegister {
				_ = conn.WriteJSON(&signaling.Message{
					Type:   signaling.TypeRegistered,
					NodeID: msg.NodeID,
				})
			}
		}
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	wsURL := "ws://" + u.Host + "/ws"

	ag := New("agent-keepalive-node", wsURL, "secret-tok", "", "http://127.0.0.1:8080")
	ag.SetPingInterval(40 * time.Millisecond)
	ag.SetReadTimeout(200 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = ag.Start(ctx)
	}()

	// Wait 350ms (well past the 200ms read deadline)
	time.Sleep(350 * time.Millisecond)

	select {
	case <-serverClosed:
		t.Fatalf("Server connection closed prematurely; Agent keepalive ping pump failed!")
	default:
		// Connection is still open, as expected!
	}

	count := atomic.LoadInt64(&pingsReceived)
	if count < 3 {
		t.Errorf("Expected at least 3 pings received in 350ms, got %d", count)
	}

	cancel()
	ag.Close()
}

func TestAgent_SessionClientIP_LifecycleAndCleanup(t *testing.T) {
	ag := New("agent-ip-lifecycle", "ws://127.0.0.1:8888/ws", "", "", "http://127.0.0.1:8080")
	defer ag.Close()

	// 1. Initial lookup should be empty
	if ip := ag.getClientIP("sess-test-1"); ip != "" {
		t.Fatalf("Expected empty IP for non-existent session, got: %q", ip)
	}

	// 2. Prepare client PeerConnection to generate real SDP offer
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("Failed to create PeerConnection: %v", err)
	}
	defer pc.Close()

	_, err = pc.CreateDataChannel("p2pmux", nil)
	if err != nil {
		t.Fatalf("Failed to create DataChannel: %v", err)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("Failed to create Offer: %v", err)
	}

	// 3. Process offer with client IP
	ag.handleOffer("sess-test-1", "198.51.100.99", &offer, "", false)

	if ip := ag.getClientIP("sess-test-1"); ip != "198.51.100.99" {
		t.Fatalf("Expected clientIP 198.51.100.99, got: %q", ip)
	}

	// 3b. Test in-place ICE Restart roaming IP update (e.g. client roams from Wi-Fi to 5G)
	restartOffer, err := pc.CreateOffer(&webrtc.OfferOptions{ICERestart: true})
	if err != nil {
		t.Fatalf("Failed to create ICE restart offer: %v", err)
	}
	ag.handleOffer("sess-test-1", "198.51.100.150", &restartOffer, "", true)
	if ip := ag.getClientIP("sess-test-1"); ip != "198.51.100.150" {
		t.Fatalf("Expected updated clientIP 198.51.100.150 after ICE Restart, got: %q", ip)
	}

	// 4. Teardown session and verify IP is cleaned up (anti-leak)
	ag.teardownSession("sess-test-1", "sess-test-1", nil)

	if ip := ag.getClientIP("sess-test-1"); ip != "" {
		t.Fatalf("Expected clientIP to be deleted after teardown, got: %q", ip)
	}

	// 5. Add another session and verify Close() cleans everything up
	ag.handleOffer("sess-test-2", "203.0.113.88", &offer, "", false)
	if ip := ag.getClientIP("sess-test-2"); ip != "203.0.113.88" {
		t.Fatalf("Expected clientIP 203.0.113.88, got: %q", ip)
	}

	ag.Close()
	if ip := ag.getClientIP("sess-test-2"); ip != "" {
		t.Fatalf("Expected all clientIPs to be cleaned up after Close(), got: %q", ip)
	}
}
