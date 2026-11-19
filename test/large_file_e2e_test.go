package test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"webrtc-gateway/pkg/agent"
	"webrtc-gateway/pkg/proxy"
	"webrtc-gateway/pkg/signaling"
)

func TestPhase4LargeStreamingAndBackpressure(t *testing.T) {
	// 1. Build and run Mock Service internally (via httptest.Server)
	mockBackend := startMockBackendServer()
	defer mockBackend.Close()

	// 2. Start Signaling Server
	signalServer, _ := startMockSignalingServer(t)
	defer signalServer.Close()
	wsSignalURL := "ws" + strings.TrimPrefix(signalServer.URL, "http")

	// 3. Start Agent Daemon
	agentNodeID := "nas-large-test"
	ag := agent.New(agentNodeID, wsSignalURL, "", "", mockBackend.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	go func() {
		_ = ag.Start(ctx)
	}()

	time.Sleep(150 * time.Millisecond)

	// 4. Client WebRTC Setup
	clientWS, _, err := websocket.DefaultDialer.Dial(wsSignalURL, nil)
	if err != nil {
		t.Fatalf("Client dial signal error: %v", err)
	}
	defer clientWS.Close()

	var clientWSMu sync.Mutex
	safeWriteWS := func(m *signaling.Message) error {
		clientWSMu.Lock()
		defer clientWSMu.Unlock()
		return clientWS.WriteJSON(m)
	}

	_ = safeWriteWS(&signaling.Message{Type: signaling.TypeJoin, NodeID: agentNodeID})
	var statusMsg signaling.Message
	_ = clientWS.ReadJSON(&statusMsg)

	clientPC, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		t.Fatalf("NewPeerConnection error: %v", err)
	}
	defer clientPC.Close()

	_, _, allOpenCh, createMux, err := setupClientDualMux(clientPC)
	if err != nil {
		t.Fatalf("setupClientDualMux error: %v", err)
	}

	clientPC.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			cand := c.ToJSON()
			_ = safeWriteWS(&signaling.Message{
				Type:      signaling.TypeCandidate,
				NodeID:    agentNodeID,
				Candidate: &cand,
			})
		}
	})

	offer, _ := clientPC.CreateOffer(nil)
	_ = clientPC.SetLocalDescription(offer)
	_ = safeWriteWS(&signaling.Message{Type: signaling.TypeOffer, NodeID: agentNodeID, SDP: &offer})

	go func() {
		for {
			var msg signaling.Message
			if err := clientWS.ReadJSON(&msg); err != nil {
				return
			}
			switch msg.Type {
			case signaling.TypeAnswer:
				_ = clientPC.SetRemoteDescription(*msg.SDP)
			case signaling.TypeCandidate:
				_ = clientPC.AddICECandidate(*msg.Candidate)
			}
		}
	}()

	select {
	case <-allOpenCh:
		t.Log("WebRTC DataChannels connected for streaming and backpressure test!")
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for DataChannels")
	}

	clientMux := createMux()
	defer clientMux.Close()

	// 5. Open stream and request chunked streaming download (1MB)
	stream, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream error: %v", err)
	}
	defer stream.Close()

	reqMeta := proxy.RequestMetadata{
		Method: "GET",
		Path:   "/download/stream", // 1MB mock stream (32 chunks * 32KB)
	}
	raw, _ := json.Marshal(reqMeta)
	_ = stream.WriteHeaders(raw)
	_ = stream.Close()

	respHeaderBytes, err := stream.ReadHeaders(context.Background())
	if err != nil {
		t.Fatalf("ReadHeaders error: %v", err)
	}
	var respMeta proxy.ResponseMetadata
	_ = json.Unmarshal(respHeaderBytes, &respMeta)
	if respMeta.Status != 200 {
		t.Fatalf("download failed with status %d", respMeta.Status)
	}

	startTime := time.Now()
	hasher := sha256.New()
	buf := make([]byte, 32*1024)
	var totalRead int64

	for {
		n, err := stream.Read(buf)
		if n > 0 {
			totalRead += int64(n)
			hasher.Write(buf[:n])
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("stream read error: %v", err)
		}
	}

	elapsed := time.Since(startTime)
	actualHash := hex.EncodeToString(hasher.Sum(nil))

	mb := float64(totalRead) / (1024 * 1024)
	speed := mb / elapsed.Seconds()

	t.Logf("Transferred %.2f MB in %v (Throughput: %.2f MB/s). SHA256: %s", mb, elapsed, speed, actualHash)

	if totalRead == 0 {
		t.Fatal("0 bytes read from stream")
	}
}
