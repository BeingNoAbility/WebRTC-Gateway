package test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"webrtc-gateway/pkg/mux"
	"webrtc-gateway/pkg/proxy"
	"webrtc-gateway/pkg/signaling"
)

func ensureLegacyAgentBinary(t *testing.T) string {
	binName := "agent_v2.1.0"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}

	// 1. Check existing binary in test dir
	candidates := []string{
		filepath.Join(".", binName),
		filepath.Join("test", binName),
		filepath.Join("..", "test", binName),
	}
	for _, c := range candidates {
		if abs, err := filepath.Abs(c); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs
			}
		}
	}

	// 2. Build on-the-fly from v2.1.0 commit tag/hash if git is available
	tmpDir, err := os.MkdirTemp("", "agent_v210_build_*")
	if err != nil {
		t.Skipf("Cannot create temp dir to build legacy agent: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(tmpDir)
	})

	repoRoot := ".."
	if _, err := os.Stat("go.mod"); err == nil {
		repoRoot = "."
	} else if _, err := os.Stat(filepath.Join("..", "go.mod")); err == nil {
		repoRoot = ".."
	}

	tarPath := filepath.Join(tmpDir, "v210.tar")
	gitCmd := exec.Command("git", "archive", "-o", tarPath, "cd35815")
	gitCmd.Dir = repoRoot
	if out, err := gitCmd.CombinedOutput(); err != nil {
		t.Skipf("git archive for cd35815 failed: %v, out: %s", err, string(out))
	}

	tarCmd := exec.Command("tar", "-xf", tarPath, "-C", tmpDir)
	if err := tarCmd.Run(); err != nil {
		t.Skipf("tar extraction failed: %v", err)
	}

	targetBin := filepath.Join(tmpDir, binName)
	buildCmd := exec.Command("go", "build", "-o", targetBin, "./cmd/agent")
	buildCmd.Dir = tmpDir
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Skipf("Building legacy agent binary failed: %v, out: %s", err, string(out))
	}

	return targetBin
}

// TestCrossVersion_NMinusOneAgent_Interoperability tests Dimension 8.1:
// N-1 Agent (v2.1.0) connecting to N Signal Server (v2.2.0) and communicating
// with N Client (v2.2.0) over WebRTC P2P DataChannel.
func TestCrossVersion_NMinusOneAgent_Interoperability(t *testing.T) {
	// 1. Locate or ensure v2.1.0 agent binary exists
	agentBinPath := ensureLegacyAgentBinary(t)

	// 2. Start Mock Backend Web Service
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Backend-Version", "v2-interop-ok")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "ok",
			"path":    r.URL.Path,
			"version": "legacy-agent-proxied",
		})
	}))
	defer backend.Close()

	// 3. Start Signal Server (v2.2.0 broker implementation)
	signalServer, _ := startMockSignalingServer(t)
	defer signalServer.Close()
	signalWSURL := "ws" + strings.TrimPrefix(signalServer.URL, "http") + "/ws"

	testNodeID := "interop-v210-node"
	testClusterSecret := "cluster-secret-xyz"
	testVisitorSecret := "visitor-pass-123"

	// 4. Launch legacy Agent (v2.1.0) process
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, agentBinPath,
		"-node-id", testNodeID,
		"-signal", signalWSURL,
		"-signal-secret", testClusterSecret,
		"-access-secret", testVisitorSecret,
		"-target", backend.URL,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start legacy v2.1.0 agent: %v", err)
	}
	defer func() {
		cancel()
		_ = cmd.Wait()
	}()

	// Wait for legacy agent to register with signal server
	time.Sleep(1000 * time.Millisecond)

	// 5. Connect v2.2.0 Client to Signal Server
	dialer := websocket.DefaultDialer
	clientWS, _, err := dialer.Dial(signalWSURL, nil)
	if err != nil {
		t.Fatalf("Client dial failed: %v", err)
	}
	defer clientWS.Close()

	var clientWSMu sync.Mutex
	safeWriteClient := func(m *signaling.Message) error {
		clientWSMu.Lock()
		defer clientWSMu.Unlock()
		return clientWS.WriteJSON(m)
	}

	// Join node
	if err := safeWriteClient(&signaling.Message{
		Type:   signaling.TypeJoin,
		NodeID: testNodeID,
	}); err != nil {
		t.Fatalf("Client join failed: %v", err)
	}

	var statusMsg signaling.Message
	_ = clientWS.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := clientWS.ReadJSON(&statusMsg); err != nil {
		t.Fatalf("Client read PeerStatus failed: %v", err)
	}
	if statusMsg.Status != "online" {
		t.Fatalf("Expected legacy agent to be online, got status: %s", statusMsg.Status)
	}
	sessionID := statusMsg.SessionID
	t.Logf("[PASS] Legacy v2.1.0 Agent registered and reported online to v2.2.0 Signal server: session=%s", sessionID)

	// 6. Create WebRTC PeerConnection on Client
	clientPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection failed: %v", err)
	}
	defer clientPC.Close()

	ordered := true
	dc, err := clientPC.CreateDataChannel("p2pmux", &webrtc.DataChannelInit{
		Ordered: &ordered,
	})
	if err != nil {
		t.Fatalf("CreateDataChannel failed: %v", err)
	}

	clientPC.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			init := c.ToJSON()
			_ = safeWriteClient(&signaling.Message{
				Type:      signaling.TypeCandidate,
				NodeID:    testNodeID,
				SessionID: sessionID,
				Candidate: &init,
			})
		}
	})

	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer failed: %v", err)
	}
	if err := clientPC.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription failed: %v", err)
	}

	// Send Offer with Secret token
	if err := safeWriteClient(&signaling.Message{
		Type:      signaling.TypeOffer,
		NodeID:    testNodeID,
		SessionID: sessionID,
		Secret:    testVisitorSecret,
		SDP:       &offer,
	}); err != nil {
		t.Fatalf("Send Offer failed: %v", err)
	}

	answerReceived := make(chan struct{}, 1)
	go func() {
		for {
			var msg signaling.Message
			if err := clientWS.ReadJSON(&msg); err != nil {
				return
			}
			switch msg.Type {
			case signaling.TypeAnswer:
				if msg.SDP != nil {
					_ = clientPC.SetRemoteDescription(*msg.SDP)
					select {
					case answerReceived <- struct{}{}:
					default:
					}
				}
			case signaling.TypeCandidate:
				if msg.Candidate != nil {
					_ = clientPC.AddICECandidate(*msg.Candidate)
				}
			}
		}
	}()

	select {
	case <-answerReceived:
		t.Log("[PASS] WebRTC Answer successfully received from legacy v2.1.0 Agent")
	case <-time.After(5 * time.Second):
		t.Fatalf("Timed out waiting for WebRTC Answer from legacy agent")
	}

	dcOpened := make(chan struct{}, 1)
	dc.OnOpen(func() {
		select {
		case dcOpened <- struct{}{}:
		default:
		}
	})

	select {
	case <-dcOpened:
		t.Log("[PASS] DataChannel p2pmux connected with legacy v2.1.0 Agent")
	case <-time.After(5 * time.Second):
		t.Fatalf("Timed out waiting for DataChannel to open")
	}

	// 7. Initialize v2.2.0 Mux on Client
	clientMux := mux.NewMux(dc, true)
	defer clientMux.Close()

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		_ = clientMux.HandleIncomingMessage(msg.Data)
	})

	// 8. Test Ping/Pong sequence ID backwards compatibility
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer pingCancel()

	rtt, err := clientMux.Ping(pingCtx)
	if err != nil {
		t.Fatalf("clientMux.Ping() failed with legacy v2.1.0 agent: %v", err)
	}
	t.Logf("[PASS] Ping/Pong RTT with legacy v2.1.0 Agent measured successfully: %v", rtt)

	// 9. Test Full HTTP Request-Response Proxying over P2PMux
	stream, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}
	defer stream.Close()

	reqMeta := proxy.RequestMetadata{
		Method: "GET",
		Path:   "/api/interop-test",
		Headers: map[string]string{
			"User-Agent": "CrossVersionTestClient/2.2.0",
			"Accept":     "application/json",
		},
	}
	metaBytes, _ := json.Marshal(reqMeta)
	if err := stream.WriteHeaders(metaBytes); err != nil {
		t.Fatalf("WriteHeaders failed: %v", err)
	}
	_ = stream.Close()

	readCtx, readCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer readCancel()

	respHeadersBytes, err := stream.ReadHeaders(readCtx)
	if err != nil {
		t.Fatalf("ReadHeaders failed: %v", err)
	}

	var respMeta proxy.ResponseMetadata
	if err := json.Unmarshal(respHeadersBytes, &respMeta); err != nil {
		t.Fatalf("Unmarshal ResponseMetadata failed: %v, raw: %s", err, string(respHeadersBytes))
	}

	if respMeta.Status != 200 {
		t.Fatalf("Expected HTTP 200, got status %d", respMeta.Status)
	}

	bodyBytes, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll stream body failed: %v", err)
	}

	var bodyMap map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &bodyMap); err != nil {
		t.Fatalf("Unmarshal response body failed: %v", err)
	}

	if bodyMap["status"] != "ok" || bodyMap["version"] != "legacy-agent-proxied" {
		t.Fatalf("Unexpected response body: %v", bodyMap)
	}

	t.Logf("[PASS] HTTP request proxied through legacy v2.1.0 Agent to backend: Status %d, Body: %s", respMeta.Status, string(bodyBytes))
}
