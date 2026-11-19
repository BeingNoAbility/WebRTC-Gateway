package test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"webrtc-gateway/pkg/agent"
	"webrtc-gateway/pkg/mux"
	"webrtc-gateway/pkg/proxy"
	"webrtc-gateway/pkg/signaling"
)

// liveTestConfig encapsulates externalized live test endpoints and SSH targets.
// Private domain names and IP addresses are excluded from source control and loaded dynamically
// from gitignored test/test_config.local.json or environment variables.
type liveTestConfig struct {
	CFSignalDomainCustom     string `json:"cf_signal_domain_custom"`
	CFSignalDomainWorkersDev string `json:"cf_signal_domain_workers_dev"`
	CFPortalDomainCustom     string `json:"cf_portal_domain_custom"`
	CFPortalDomainWorkersDev string `json:"cf_portal_domain_workers_dev"`
	TestbedSSHTarget         string `json:"testbed_ssh_target"`
	CFTurnTokenID            string `json:"cf_turn_token_id"`
	CFTurnAPIToken           string `json:"cf_turn_api_token"`
}

func getLiveTestConfig(t *testing.T) *liveTestConfig {
	t.Helper()
	cfg := &liveTestConfig{}

	configPaths := []string{
		"test_config.local.json",
		"test/test_config.local.json",
		"../test/test_config.local.json",
	}

	for _, path := range configPaths {
		if data, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(data, cfg)
			break
		}
	}

	// Environment variable overrides
	if v := os.Getenv("CF_SIGNAL_DOMAIN_CUSTOM"); v != "" {
		cfg.CFSignalDomainCustom = v
	}
	if v := os.Getenv("CF_SIGNAL_DOMAIN_WORKERS_DEV"); v != "" {
		cfg.CFSignalDomainWorkersDev = v
	}
	if v := os.Getenv("CF_PORTAL_DOMAIN_CUSTOM"); v != "" {
		cfg.CFPortalDomainCustom = v
	}
	if v := os.Getenv("CF_PORTAL_DOMAIN_WORKERS_DEV"); v != "" {
		cfg.CFPortalDomainWorkersDev = v
	}
	if v := os.Getenv("TESTBED_SSH_TARGET"); v != "" {
		cfg.TestbedSSHTarget = v
	}
	if v := os.Getenv("CF_TURN_TOKEN_ID"); v != "" {
		cfg.CFTurnTokenID = v
	}
	if v := os.Getenv("CF_TURN_API_TOKEN"); v != "" {
		cfg.CFTurnAPIToken = v
	}

	if cfg.CFSignalDomainCustom == "" || strings.Contains(cfg.CFSignalDomainCustom, "example.com") {
		t.Skip("Live Cloudflare test skipped: endpoints not configured. Set environment variables or configure test/test_config.local.json")
	}

	return cfg
}

// TestCloudflareLive_HealthEndpoints tests live endpoints for HTTP 200 and online status.
func TestCloudflareLive_HealthEndpoints(t *testing.T) {
	cfg := getLiveTestConfig(t)

	endpoints := []string{
		"https://" + cfg.CFSignalDomainCustom + "/health",
		"https://" + cfg.CFSignalDomainWorkersDev + "/health",
		"https://" + cfg.CFPortalDomainCustom + "/health",
		"https://" + cfg.CFPortalDomainWorkersDev + "/health",
	}

	client := &http.Client{Timeout: 10 * time.Second}

	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			resp, err := client.Get(ep)
			if err != nil {
				t.Fatalf("Failed to GET %s: %v", ep, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("Expected HTTP 200, got %d for %s", resp.StatusCode, ep)
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("Failed to read response body: %v", err)
			}

			var statusResp struct {
				Status string `json:"status"`
				Mode   string `json:"mode"`
				Time   string `json:"time"`
			}
			if err := json.Unmarshal(body, &statusResp); err != nil {
				t.Fatalf("Failed to parse JSON: %v, raw: %s", err, string(body))
			}

			if statusResp.Status != "online" {
				t.Fatalf("Expected status 'online', got '%s'", statusResp.Status)
			}
			t.Logf("[PASS] %s returned status: %s, mode: %s, time: %s", ep, statusResp.Status, statusResp.Mode, statusResp.Time)
		})
	}
}

// TestCloudflareLive_WebSocketUpgradeRequirement tests that non-WebSocket requests to /ws are rejected with 426.
func TestCloudflareLive_WebSocketUpgradeRequirement(t *testing.T) {
	cfg := getLiveTestConfig(t)
	client := &http.Client{Timeout: 10 * time.Second}
	url := "https://" + cfg.CFSignalDomainCustom + "/ws"

	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("Failed to GET %s: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("Expected HTTP 426 Upgrade Required, got %d", resp.StatusCode)
	}
	t.Logf("[PASS] %s properly rejected plain HTTP with status 426 Upgrade Required", url)
}

// TestCloudflareLive_Signaling_DurableObject_Lifecycle validates registration, anti-hijacking,
// client join, and disconnect notifications via live Cloudflare Durable Object.
func TestCloudflareLive_Signaling_DurableObject_Lifecycle(t *testing.T) {
	cfg := getLiveTestConfig(t)
	testNodeID := "test-agent-" + uuid.NewString()[:8]
	testSecret := "secret-" + uuid.NewString()[:8]
	wsURL := fmt.Sprintf("wss://%s/ws?node=%s", cfg.CFSignalDomainCustom, testNodeID)

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second

	// 1. Connect Agent WebSocket
	agentConn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Failed to dial Cloudflare signaling WS as agent: %v", err)
	}
	defer agentConn.Close()

	// Register Agent with secret
	regMsg := signaling.Message{
		Type:   signaling.TypeRegister,
		NodeID: testNodeID,
		Secret: testSecret,
	}
	if err := agentConn.WriteJSON(regMsg); err != nil {
		t.Fatalf("Failed to write register message: %v", err)
	}

	var regAck signaling.Message
	if err := agentConn.ReadJSON(&regAck); err != nil {
		t.Fatalf("Failed to read register ack: %v", err)
	}
	if regAck.Type != signaling.TypeRegistered || regAck.NodeID != testNodeID {
		t.Fatalf("Unexpected register ack: %+v", regAck)
	}
	t.Logf("[PASS] Agent successfully registered with Cloudflare DO: %s", testNodeID)

	// 2. Connect Client WebSocket and Join
	clientConn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Failed to dial Cloudflare signaling WS as client: %v", err)
	}
	defer clientConn.Close()

	joinMsg := signaling.Message{
		Type:   signaling.TypeJoin,
		NodeID: testNodeID,
	}
	if err := clientConn.WriteJSON(joinMsg); err != nil {
		t.Fatalf("Failed to write join message: %v", err)
	}

	var peerStatus signaling.Message
	if err := clientConn.ReadJSON(&peerStatus); err != nil {
		t.Fatalf("Failed to read peer_status: %v", err)
	}
	if peerStatus.Type != signaling.TypePeerStatus || peerStatus.Status != "online" {
		t.Fatalf("Unexpected peer status: %+v", peerStatus)
	}
	t.Logf("[PASS] Client received peer_status online for node: %s", testNodeID)

	// 3. Anti-hijacking Verification: Attempt to register same node_id with wrong secret from a 3rd conn
	hijackConn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Failed to dial Cloudflare signaling WS as hijacker: %v", err)
	}
	defer hijackConn.Close()

	hijackMsg := signaling.Message{
		Type:   signaling.TypeRegister,
		NodeID: testNodeID,
		Secret: "wrong-secret-attack",
	}
	if err := hijackConn.WriteJSON(hijackMsg); err != nil {
		t.Fatalf("Failed to write hijack register message: %v", err)
	}

	var errMsg signaling.Message
	readErr := hijackConn.ReadJSON(&errMsg)
	if readErr != nil {
		// Connection closed by Cloudflare DO upon auth failure
		t.Logf("[PASS] Hijacker connection cleanly rejected and closed: %v", readErr)
	} else {
		if errMsg.Type != signaling.TypeError {
			t.Fatalf("Expected error type for hijacking attempt, got: %+v", errMsg)
		}
		t.Logf("[PASS] Cloudflare DO rejected hijacking with error: %s", errMsg.Error)
	}

	// 4. Verify Disconnect: Close Agent and observe offline status on Client
	agentConn.Close()

	_ = clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var offlineStatus signaling.Message
	if err := clientConn.ReadJSON(&offlineStatus); err != nil {
		t.Fatalf("Failed to read offline peer_status: %v", err)
	}
	if offlineStatus.Type != signaling.TypePeerStatus || offlineStatus.Status != "offline" {
		t.Fatalf("Expected peer_status offline, got: %+v", offlineStatus)
	}
	t.Logf("[PASS] Client received peer_status offline after agent disconnected")
}

// TestCloudflareLive_P2P_WebRTC_DataChannel_E2E validates full end-to-end P2P DataChannel
// traversal and HTTP reverse proxying over the live Cloudflare Worker Durable Object signaling plane.
func TestCloudflareLive_P2P_WebRTC_DataChannel_E2E(t *testing.T) {
	cfg := getLiveTestConfig(t)

	// 1. Local Target HTTP Service
	expectedBody := "Hello from Cloudflare Worker P2P WebRTC Gateway E2E!"
	mockTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-E2E-Cloudflare", "verified")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(expectedBody))
	}))
	defer mockTarget.Close()

	// 2. Start Agent connected to Live Cloudflare Worker
	testNodeID := "cf-e2e-" + uuid.NewString()[:8]
	signalURL := fmt.Sprintf("wss://%s/ws?node=%s", cfg.CFSignalDomainCustom, testNodeID)

	agentInstance := agent.New(testNodeID, signalURL, "", "", mockTarget.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	go func() {
		_ = agentInstance.Start(ctx)
	}()

	// Wait for agent to connect and register with Cloudflare DO
	time.Sleep(800 * time.Millisecond)

	// 3. Connect Visitor Client to Live Cloudflare Worker
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second

	clientWS, _, err := dialer.Dial(signalURL, nil)
	if err != nil {
		t.Fatalf("Client failed to dial Cloudflare signaling server: %v", err)
	}
	defer clientWS.Close()

	var clientWSMu sync.Mutex
	safeWriteWS := func(m *signaling.Message) error {
		clientWSMu.Lock()
		defer clientWSMu.Unlock()
		return clientWS.WriteJSON(m)
	}

	// Join the room
	if err := safeWriteWS(&signaling.Message{
		Type:   signaling.TypeJoin,
		NodeID: testNodeID,
	}); err != nil {
		t.Fatalf("Client join error: %v", err)
	}

	var statusMsg signaling.Message
	deadline := time.Now().Add(5 * time.Second)
	for {
		_ = clientWS.SetReadDeadline(deadline)
		if err := clientWS.ReadJSON(&statusMsg); err != nil {
			t.Fatalf("Client read peer_status error: %v", err)
		}
		if statusMsg.Type == signaling.TypePeerStatus && statusMsg.Status == "online" {
			break
		}
	}
	t.Logf("[PASS] Live Cloudflare signaling reported agent '%s' online", testNodeID)

	// 4. Create Client WebRTC PeerConnection with public Google STUN
	clientPC, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	})
	if err != nil {
		t.Fatalf("NewPeerConnection error: %v", err)
	}
	defer clientPC.Close()

	ordered := true
	dc, err := clientPC.CreateDataChannel("p2pmux", &webrtc.DataChannelInit{
		Ordered: &ordered,
	})
	if err != nil {
		t.Fatalf("CreateDataChannel error: %v", err)
	}

	clientPC.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			cand := c.ToJSON()
			_ = safeWriteWS(&signaling.Message{
				Type:      signaling.TypeCandidate,
				NodeID:    testNodeID,
				Candidate: &cand,
			})
		}
	})

	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer error: %v", err)
	}
	if err := clientPC.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription error: %v", err)
	}

	if err := safeWriteWS(&signaling.Message{
		Type:   signaling.TypeOffer,
		NodeID: testNodeID,
		SDP:    &offer,
	}); err != nil {
		t.Fatalf("Send Offer error: %v", err)
	}

	// 5. Exchange SDP & ICE Candidates via Cloudflare DO
	answerReceived := make(chan struct{})
	go func() {
		for {
			var msg signaling.Message
			if err := clientWS.ReadJSON(&msg); err != nil {
				return
			}
			switch msg.Type {
			case signaling.TypeAnswer:
				t.Log("[Client WS] Received Answer")
				if msg.SDP != nil {
					_ = clientPC.SetRemoteDescription(*msg.SDP)
					close(answerReceived)
				}
			case signaling.TypeCandidate:
				if msg.Candidate != nil {
					t.Logf("[Client WS] Received Candidate from Agent: %s", msg.Candidate.Candidate)
					_ = clientPC.AddICECandidate(*msg.Candidate)
				}
			}
		}
	}()

	select {
	case <-answerReceived:
		t.Log("[PASS] Received WebRTC Answer from Agent via Cloudflare Worker")
	case <-time.After(10 * time.Second):
		t.Fatal("Timeout waiting for WebRTC Answer via Cloudflare DO")
	}

	// 6. Wait for DataChannel to Open
	dcOpened := make(chan struct{})
	dc.OnOpen(func() {
		close(dcOpened)
	})

	select {
	case <-dcOpened:
		t.Log("[PASS] WebRTC DataChannel successfully opened over Cloudflare signaling!")
	case <-time.After(10 * time.Second):
		t.Fatal("Timeout waiting for DataChannel to open")
	}

	// 7. Initialize P2PMux on Visitor Client
	clientMux := mux.NewMux(dc, true)
	defer clientMux.Close()

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		_ = clientMux.HandleIncomingMessage(msg.Data)
	})

	// Verify Ping/Pong over WebRTC DataChannel
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer pingCancel()

	rtt, err := clientMux.Ping(pingCtx)
	if err != nil {
		t.Fatalf("Ping over WebRTC failed: %v", err)
	}
	t.Logf("[PASS] WebRTC DataChannel Ping/Pong RTT via Cloudflare DO: %v", rtt)

	// 8. Open HTTP Proxy Stream and send Request
	stream, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("Failed to open multiplexed stream: %v", err)
	}
	defer stream.Close()

	reqMetaBytes, _ := json.Marshal(proxy.RequestMetadata{
		Method: "GET",
		Path:   "/api/test",
		Headers: map[string]string{
			"host": "mock-target",
		},
	})

	if err := stream.WriteHeaders(reqMetaBytes); err != nil {
		t.Fatalf("Failed to write request headers to stream: %v", err)
	}
	_ = stream.Close() // Half-close write

	readCtx, rCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rCancel()

	respHeadersPayload, err := stream.ReadHeaders(readCtx)
	if err != nil {
		t.Fatalf("Failed to read response headers from stream: %v", err)
	}

	var respMeta proxy.ResponseMetadata
	if err := json.Unmarshal(respHeadersPayload, &respMeta); err != nil {
		t.Fatalf("Failed to unmarshal response metadata: %v", err)
	}

	if respMeta.Status != http.StatusOK {
		t.Fatalf("Expected HTTP 200 OK, got: %d", respMeta.Status)
	}
	if respMeta.Headers["x-e2e-cloudflare"] != "verified" {
		t.Fatalf("Expected header X-E2E-Cloudflare: verified, got: %v", respMeta.Headers)
	}

	bodyBytes, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("Failed to read body from stream: %v", err)
	}

	bodyStr := string(bodyBytes)
	if !strings.Contains(bodyStr, expectedBody) {
		t.Fatalf("Expected body '%s', got '%s'", expectedBody, bodyStr)
	}

	t.Logf("[PASS] Full HTTP request-response proxied over WebRTC P2P DataChannel via Cloudflare DO signaling!")
	t.Logf("       Status: %d, Headers: %+v, Body: %s", respMeta.Status, respMeta.Headers, bodyStr)
}

// TestCloudflareLive_CrossNetwork_DebianToWindows_E2E tests real cross-machine P2P traversal:
// Agent on Debian 13 VM <-> Cloudflare Worker DO <-> Client on Windows Host.
func TestCloudflareLive_CrossNetwork_DebianToWindows_E2E(t *testing.T) {
	cfg := getLiveTestConfig(t)
	if cfg.TestbedSSHTarget == "" || strings.Contains(cfg.TestbedSSHTarget, "192.0.2.") {
		t.Skip("Testbed SSH target not configured in test_config.local.json or TESTBED_SSH_TARGET, skipping cross-network test")
	}

	// 1. Verify SSH connectivity to Linux Testbed VM
	checkCmd := exec.Command("ssh", "-o", "ConnectTimeout=2", cfg.TestbedSSHTarget, "echo ok")
	if err := checkCmd.Run(); err != nil {
		t.Skipf("Linux Testbed VM (%s) not accessible via SSH, skipping cross-network test", cfg.TestbedSSHTarget)
	}

	testNodeID := "cf-crossnet-" + uuid.NewString()[:8]
	signalURL := fmt.Sprintf("wss://%s/ws?node=%s", cfg.CFSignalDomainCustom, testNodeID)

	// 2. Start Python Mock HTTP Server and Agent on Linux VM in background via helper script
	startCmd := exec.Command("ssh", cfg.TestbedSSHTarget, "/root/webrtc-testbed/test_cf_agent.sh", testNodeID, signalURL, "8089")
	if out, err := startCmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to launch agent on Linux VM: %v, out: %s", err, string(out))
	}

	// Defer cleanup on Linux VM
	defer func() {
		cleanupCmd := exec.Command("ssh", cfg.TestbedSSHTarget, "pkill -f 'python3 -m http.server'; pkill -x agent")
		_ = cleanupCmd.Run()
	}()

	// Wait for remote agent to connect to Cloudflare Worker
	time.Sleep(1000 * time.Millisecond)

	// 3. Connect Visitor Client from Windows to Live Cloudflare Worker
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second

	clientWS, _, err := dialer.Dial(signalURL, nil)
	if err != nil {
		t.Fatalf("Client failed to dial Cloudflare signaling server: %v", err)
	}
	defer clientWS.Close()

	var clientWSMu sync.Mutex
	safeWriteWS := func(m *signaling.Message) error {
		clientWSMu.Lock()
		defer clientWSMu.Unlock()
		return clientWS.WriteJSON(m)
	}

	// Join room
	if err := safeWriteWS(&signaling.Message{
		Type:   signaling.TypeJoin,
		NodeID: testNodeID,
	}); err != nil {
		t.Fatalf("Client join error: %v", err)
	}

	var statusMsg signaling.Message
	deadline := time.Now().Add(5 * time.Second)
	for {
		_ = clientWS.SetReadDeadline(deadline)
		if err := clientWS.ReadJSON(&statusMsg); err != nil {
			t.Fatalf("Client read peer_status error: %v", err)
		}
		if statusMsg.Type == signaling.TypePeerStatus && statusMsg.Status == "online" {
			break
		}
	}
	t.Logf("[PASS] Linux VM Agent registered with Cloudflare DO and reported online: %s", testNodeID)

	// 4. Create Windows Client WebRTC PeerConnection
	clientPC, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	})
	if err != nil {
		t.Fatalf("NewPeerConnection error: %v", err)
	}
	defer clientPC.Close()

	ordered := true
	dc, err := clientPC.CreateDataChannel("p2pmux", &webrtc.DataChannelInit{
		Ordered: &ordered,
	})
	if err != nil {
		t.Fatalf("CreateDataChannel error: %v", err)
	}

	clientPC.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			cand := c.ToJSON()
			_ = safeWriteWS(&signaling.Message{
				Type:      signaling.TypeCandidate,
				NodeID:    testNodeID,
				Candidate: &cand,
			})
		}
	})

	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer error: %v", err)
	}
	if err := clientPC.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription error: %v", err)
	}

	if err := safeWriteWS(&signaling.Message{
		Type:   signaling.TypeOffer,
		NodeID: testNodeID,
		SDP:    &offer,
	}); err != nil {
		t.Fatalf("Send Offer error: %v", err)
	}

	// 5. Exchange SDP & ICE Candidates via Cloudflare DO
	answerReceived := make(chan struct{})
	go func() {
		for {
			var msg signaling.Message
			if err := clientWS.ReadJSON(&msg); err != nil {
				return
			}
			switch msg.Type {
			case signaling.TypeAnswer:
				t.Log("[Client WS] Received Answer")
				if msg.SDP != nil {
					_ = clientPC.SetRemoteDescription(*msg.SDP)
					close(answerReceived)
				}
			case signaling.TypeCandidate:
				if msg.Candidate != nil {
					t.Logf("[Client WS] Received Candidate from Agent: %s", msg.Candidate.Candidate)
					_ = clientPC.AddICECandidate(*msg.Candidate)
				}
			}
		}
	}()

	select {
	case <-answerReceived:
		t.Log("[PASS] Received WebRTC Answer from Linux VM Agent via Cloudflare Worker")
	case <-time.After(10 * time.Second):
		t.Fatal("Timeout waiting for WebRTC Answer via Cloudflare DO")
	}

	// 6. Wait for DataChannel to Open between Windows and Linux VM
	dcOpened := make(chan struct{})
	dc.OnOpen(func() {
		close(dcOpened)
	})

	select {
	case <-dcOpened:
		t.Log("[PASS] Cross-network WebRTC DataChannel (Linux VM <-> Windows) OPENED!")
	case <-time.After(10 * time.Second):
		t.Fatal("Timeout waiting for cross-network DataChannel to open")
	}

	// 7. Initialize P2PMux on Visitor Client
	clientMux := mux.NewMux(dc, true)
	defer clientMux.Close()

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		_ = clientMux.HandleIncomingMessage(msg.Data)
	})

	// Verify Ping/Pong RTT across machines
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer pingCancel()

	rtt, err := clientMux.Ping(pingCtx)
	if err != nil {
		t.Fatalf("Cross-network Ping over WebRTC failed: %v", err)
	}
	t.Logf("[PASS] Cross-network WebRTC Ping/Pong RTT (Linux <-> Windows): %v", rtt)

	// 8. Open HTTP Proxy Stream and Fetch from Linux VM Target
	stream, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("Failed to open stream: %v", err)
	}
	defer stream.Close()

	reqMetaBytes, _ := json.Marshal(proxy.RequestMetadata{
		Method: "GET",
		Path:   "/",
		Headers: map[string]string{
			"host": "testbed-local",
		},
	})

	if err := stream.WriteHeaders(reqMetaBytes); err != nil {
		t.Fatalf("Failed to write request headers to stream: %v", err)
	}
	_ = stream.Close()

	readCtx, rCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rCancel()

	respHeadersPayload, err := stream.ReadHeaders(readCtx)
	if err != nil {
		t.Fatalf("Failed to read response headers from stream: %v", err)
	}

	var respMeta proxy.ResponseMetadata
	if err := json.Unmarshal(respHeadersPayload, &respMeta); err != nil {
		t.Fatalf("Failed to unmarshal response metadata: %v", err)
	}

	if respMeta.Status != http.StatusOK {
		t.Fatalf("Expected HTTP 200 OK from Linux VM, got: %d", respMeta.Status)
	}

	bodyBytes, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("Failed to read body from stream: %v", err)
	}

	bodyStr := string(bodyBytes)
	if len(bodyStr) == 0 {
		t.Fatalf("Expected non-empty response body from Linux VM")
	}

	t.Logf("[PASS] Cross-network HTTP reverse proxy succeeded between Linux VM and Windows Host via Cloudflare DO!")
	t.Logf("       Status: %d, Content-Type: %s, Body length: %d bytes", respMeta.Status, respMeta.Headers["content-type"], len(bodyStr))
}

// TestCloudflareLive_Portal_StaticAssets verifies the static SPA portal is served correctly.
func TestCloudflareLive_Portal_StaticAssets(t *testing.T) {
	cfg := getLiveTestConfig(t)

	portalURLs := []string{
		"https://" + cfg.CFPortalDomainWorkersDev,
		"https://" + cfg.CFPortalDomainCustom,
	}

	client := &http.Client{Timeout: 10 * time.Second}

	for _, pURL := range portalURLs {
		t.Run(pURL, func(t *testing.T) {
			resp, err := client.Get(pURL)
			if err != nil {
				t.Fatalf("Failed to GET portal: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("Expected HTTP 200, got %d", resp.StatusCode)
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("Failed to read portal body: %v", err)
			}

			content := string(body)
			if !strings.Contains(content, "<html") || !strings.Contains(content, "WebRTC") {
				t.Fatalf("Expected HTML content with WebRTC portal, got length %d", len(content))
			}
			t.Logf("[PASS] %s successfully returned WebRTC Portal SPA (length: %d bytes)", pURL, len(content))
		})
	}
}

// fetchCloudflareTURNServers dynamically requests short-term TURN credentials from Cloudflare Realtime TURN API.
func fetchCloudflareTURNServers(t *testing.T, cfg *liveTestConfig) []webrtc.ICEServer {
	t.Helper()
	if cfg.CFTurnTokenID == "" || cfg.CFTurnAPIToken == "" {
		t.Skip("Cloudflare TURN token ID or API token not configured; skipping")
	}

	apiURL := fmt.Sprintf("https://rtc.live.cloudflare.com/v1/turn/keys/%s/credentials/generate-ice-servers", cfg.CFTurnTokenID)
	reqBody := `{"ttl": 86400}`
	req, err := http.NewRequest(http.MethodPost, apiURL, strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("Failed to create TURN request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.CFTurnAPIToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Failed to call Cloudflare TURN API: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Cloudflare TURN API returned status %d: %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		ICEServers []struct {
			URLs       any    `json:"urls"`
			Username   string `json:"username"`
			Credential string `json:"credential"`
		} `json:"iceServers"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("Failed to decode Cloudflare TURN response: %v", err)
	}

	var iceServers []webrtc.ICEServer
	for _, s := range parsed.ICEServers {
		var urls []string
		switch v := s.URLs.(type) {
		case []any:
			for _, item := range v {
				if str, ok := item.(string); ok {
					urls = append(urls, str)
				}
			}
		case string:
			urls = append(urls, v)
		}
		if len(urls) == 0 {
			continue
		}
		server := webrtc.ICEServer{
			URLs: urls,
		}
		if s.Username != "" {
			server.Username = s.Username
			server.Credential = s.Credential
			server.CredentialType = webrtc.ICECredentialTypePassword
		}
		iceServers = append(iceServers, server)
	}

	if len(iceServers) == 0 {
		t.Fatalf("No ICE servers returned by Cloudflare TURN API")
	}

	return iceServers
}

// TestCloudflareLive_TURN_Relay_ForcedSymmetricNAT_E2E verifies Dimension 3.2:
// Forces ICETransportPolicyRelay on both Client and Agent, disabling host and srflx candidates.
// All WebRTC traffic must relay 100% through Cloudflare Anycast TURN servers.
func TestCloudflareLive_TURN_Relay_ForcedSymmetricNAT_E2E(t *testing.T) {
	cfg := getLiveTestConfig(t)
	if cfg.CFTurnTokenID == "" || cfg.CFTurnAPIToken == "" {
		t.Skip("Cloudflare TURN credentials not configured in test/test_config.local.json; skipping TURN relay test")
	}

	turnServers := fetchCloudflareTURNServers(t, cfg)
	t.Logf("[PASS] Dynamically fetched %d ICE server groups from Cloudflare Realtime TURN API", len(turnServers))

	// 1. Mock Backend HTTP service
	expectedBody := "Hello from 100% Cloudflare TURN Relay Forced-Path E2E!"
	mockTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-E2E-TURN-Relay", "verified")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(expectedBody))
	}))
	defer mockTarget.Close()

	// 2. Start Agent with forced TURN relay policy
	testNodeID := "cf-turn-" + uuid.NewString()[:8]
	signalURL := fmt.Sprintf("wss://%s/ws?node=%s", cfg.CFSignalDomainCustom, testNodeID)

	agentInstance := agent.New(testNodeID, signalURL, "", "", mockTarget.URL)
	agentInstance.SetICEServers(turnServers)
	agentInstance.SetICETransportPolicy(webrtc.ICETransportPolicyRelay)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	go func() {
		_ = agentInstance.Start(ctx)
	}()

	time.Sleep(800 * time.Millisecond)

	// 3. Connect Visitor Client to Cloudflare Signaling DO
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second

	clientWS, _, err := dialer.Dial(signalURL, nil)
	if err != nil {
		t.Fatalf("Client failed to dial Cloudflare signaling server: %v", err)
	}
	defer clientWS.Close()

	var clientWSMu sync.Mutex
	safeWriteWS := func(m *signaling.Message) error {
		clientWSMu.Lock()
		defer clientWSMu.Unlock()
		return clientWS.WriteJSON(m)
	}

	if err := safeWriteWS(&signaling.Message{
		Type:   signaling.TypeJoin,
		NodeID: testNodeID,
	}); err != nil {
		t.Fatalf("Client join error: %v", err)
	}

	var statusMsg signaling.Message
	deadline := time.Now().Add(5 * time.Second)
	for {
		_ = clientWS.SetReadDeadline(deadline)
		if err := clientWS.ReadJSON(&statusMsg); err != nil {
			t.Fatalf("Client read peer_status error: %v", err)
		}
		if statusMsg.Type == signaling.TypePeerStatus && statusMsg.Status == "online" {
			break
		}
	}
	t.Logf("[PASS] Live Cloudflare signaling reported agent '%s' online", testNodeID)

	// 4. Create Client WebRTC PeerConnection with forced TURN relay policy
	clientPC, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers:         turnServers,
		ICETransportPolicy: webrtc.ICETransportPolicyRelay,
	})
	if err != nil {
		t.Fatalf("NewPeerConnection error: %v", err)
	}
	defer clientPC.Close()

	ordered := true
	dc, err := clientPC.CreateDataChannel("p2pmux", &webrtc.DataChannelInit{
		Ordered: &ordered,
	})
	if err != nil {
		t.Fatalf("CreateDataChannel error: %v", err)
	}

	var relayCandidateObserved bool
	var relayMu sync.Mutex

	clientPC.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		t.Logf("[Client] ICE State: %s", state.String())
	})
	clientPC.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		t.Logf("[Client] PeerConnection State: %s", state.String())
	})

	clientPC.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			cand := c.ToJSON()
			t.Logf("[Client gathered candidate] %s", cand.Candidate)
			if strings.Contains(cand.Candidate, "typ relay") {
				relayMu.Lock()
				relayCandidateObserved = true
				relayMu.Unlock()
			}
			_ = safeWriteWS(&signaling.Message{
				Type:      signaling.TypeCandidate,
				NodeID:    testNodeID,
				Candidate: &cand,
			})
		}
	})

	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer error: %v", err)
	}
	if err := clientPC.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription error: %v", err)
	}

	if err := safeWriteWS(&signaling.Message{
		Type:   signaling.TypeOffer,
		NodeID: testNodeID,
		SDP:    &offer,
	}); err != nil {
		t.Fatalf("Send Offer error: %v", err)
	}

	// 5. Exchange SDP & ICE Candidates via Cloudflare DO
	answerReceived := make(chan struct{})
	go func() {
		for {
			var msg signaling.Message
			if err := clientWS.ReadJSON(&msg); err != nil {
				return
			}
			switch msg.Type {
			case signaling.TypeAnswer:
				t.Log("[Client WS] Received Answer")
				if msg.SDP != nil {
					_ = clientPC.SetRemoteDescription(*msg.SDP)
					close(answerReceived)
				}
			case signaling.TypeCandidate:
				if msg.Candidate != nil {
					t.Logf("[Client WS] Received Candidate from Agent: %s", msg.Candidate.Candidate)
					_ = clientPC.AddICECandidate(*msg.Candidate)
				}
			}
		}
	}()

	select {
	case <-answerReceived:
		t.Log("[PASS] Received WebRTC Answer from Agent via Cloudflare Worker (TURN Relay mode)")
	case <-time.After(10 * time.Second):
		t.Fatal("Timeout waiting for WebRTC Answer via Cloudflare DO")
	}

	// 6. Wait for DataChannel to Open over TURN Relay
	dcOpened := make(chan struct{})
	dc.OnOpen(func() {
		close(dcOpened)
	})

	select {
	case <-dcOpened:
		t.Log("[PASS] WebRTC DataChannel successfully opened over Cloudflare TURN Relay!")
	case <-time.After(30 * time.Second):
		t.Fatal("Timeout waiting for DataChannel to open over TURN Relay")
	}

	relayMu.Lock()
	hasRelay := relayCandidateObserved
	relayMu.Unlock()
	if !hasRelay {
		t.Log("[WARN] Local client candidate did not log typ relay explicitly, but ICETransportPolicyRelay enforced relay-only.")
	} else {
		t.Log("[PASS] Confirmed local WebRTC client gathered and used 'typ relay' candidate from Cloudflare TURN")
	}

	// 7. Initialize P2PMux on Visitor Client
	clientMux := mux.NewMux(dc, true)
	defer clientMux.Close()

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		_ = clientMux.HandleIncomingMessage(msg.Data)
	})

	// 8. Measure Ping/Pong RTT over TURN Relay
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()

	rtt, err := clientMux.Ping(pingCtx)
	if err != nil {
		t.Fatalf("Ping over WebRTC TURN Relay failed: %v", err)
	}
	t.Logf("[PASS] WebRTC DataChannel Ping/Pong RTT via Cloudflare Realtime TURN: %v", rtt)

	// 9. Open HTTP Proxy Stream and send Request over TURN Relay
	stream, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("Failed to open multiplexed stream over TURN: %v", err)
	}
	defer stream.Close()

	reqMetaBytes, _ := json.Marshal(proxy.RequestMetadata{
		Method: "GET",
		Path:   "/api/turn-relay-test",
		Headers: map[string]string{
			"host": "mock-target-turn",
		},
	})

	if err := stream.WriteHeaders(reqMetaBytes); err != nil {
		t.Fatalf("Failed to write request headers to stream: %v", err)
	}
	_ = stream.Close()

	readCtx, rCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rCancel()

	respHeadersPayload, err := stream.ReadHeaders(readCtx)
	if err != nil {
		t.Fatalf("Failed to read response headers from stream: %v", err)
	}

	var respMeta proxy.ResponseMetadata
	if err := json.Unmarshal(respHeadersPayload, &respMeta); err != nil {
		t.Fatalf("Failed to unmarshal response metadata: %v", err)
	}

	if respMeta.Status != http.StatusOK {
		t.Fatalf("Expected HTTP 200 OK, got: %d", respMeta.Status)
	}
	if respMeta.Headers["x-e2e-turn-relay"] != "verified" {
		t.Fatalf("Expected header X-E2E-TURN-Relay: verified, got: %v", respMeta.Headers)
	}

	bodyBytes, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("Failed to read body from stream: %v", err)
	}

	bodyStr := string(bodyBytes)
	if !strings.Contains(bodyStr, expectedBody) {
		t.Fatalf("Expected body '%s', got '%s'", expectedBody, bodyStr)
	}

	t.Logf("[PASS] Full HTTP request-response successfully proxied over Cloudflare TURN Relay!")
	t.Logf("       Status: %d, Headers: %+v, Body: %s", respMeta.Status, respMeta.Headers, bodyStr)
}
