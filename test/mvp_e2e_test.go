package test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"webrtc-gateway/pkg/agent"
	"webrtc-gateway/pkg/signaling"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func startMockSignalingServer(t *testing.T) (*httptest.Server, *signalingBroker) {
	broker := newBroker()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		broker.handleConn(conn)
	}))
	return server, broker
}

type testPeerSession struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

func (s *testPeerSession) send(msg *signaling.Message) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.conn.WriteJSON(msg)
}

type signalingBroker struct {
	mu      sync.RWMutex
	agents  map[string]*testPeerSession
	clients map[string]*testPeerSession
}

func newBroker() *signalingBroker {
	return &signalingBroker{
		agents:  make(map[string]*testPeerSession),
		clients: make(map[string]*testPeerSession),
	}
}

func (b *signalingBroker) handleConn(conn *websocket.Conn) {
	session := &testPeerSession{conn: conn}
	var role, nodeID, sessionID string
	defer func() {
		conn.Close()
		b.mu.Lock()
		if role == "agent" {
			delete(b.agents, nodeID)
		} else {
			delete(b.clients, sessionID)
		}
		b.mu.Unlock()
	}()

	for {
		var msg signaling.Message
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}

		switch msg.Type {
		case signaling.TypeRegister:
			role = "agent"
			nodeID = msg.NodeID
			b.mu.Lock()
			b.agents[nodeID] = session
			b.mu.Unlock()
			_ = session.send(&signaling.Message{Type: signaling.TypeRegistered, NodeID: nodeID})

		case signaling.TypeJoin:
			role = "client"
			nodeID = msg.NodeID
			sessionID = "client-session-1"
			b.mu.Lock()
			b.clients[sessionID] = session
			_, hasAgent := b.agents[nodeID]
			b.mu.Unlock()

			status := "offline"
			if hasAgent {
				status = "online"
			}
			_ = session.send(&signaling.Message{
				Type:      signaling.TypePeerStatus,
				NodeID:    nodeID,
				SessionID: sessionID,
				Status:    status,
			})

		case signaling.TypeOffer:
			msg.SessionID = sessionID
			b.mu.RLock()
			agentSess := b.agents[msg.NodeID]
			b.mu.RUnlock()
			if agentSess != nil {
				_ = agentSess.send(&msg)
			}

		case signaling.TypeAnswer:
			b.mu.RLock()
			clientSess := b.clients[msg.SessionID]
			b.mu.RUnlock()
			if clientSess != nil {
				_ = clientSess.send(&msg)
			}

		case signaling.TypeCandidate:
			b.mu.RLock()
			if role == "agent" {
				if clientSess := b.clients[msg.SessionID]; clientSess != nil {
					_ = clientSess.send(&msg)
				}
			} else {
				msg.SessionID = sessionID
				if agentSess := b.agents[msg.NodeID]; agentSess != nil {
					_ = agentSess.send(&msg)
				}
			}
			b.mu.RUnlock()
		}
	}
}

func TestPhase2E2EWebRTCP2PMux(t *testing.T) {
	// 1. Start Signaling Server
	signalServer, _ := startMockSignalingServer(t)
	defer signalServer.Close()

	wsURL := "ws" + strings.TrimPrefix(signalServer.URL, "http")

	// 2. Start Agent in background
	agentNodeID := "test-agent-node"
	agentInstance := agent.New(agentNodeID, wsURL, "", "", "http://127.0.0.1:8080")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() {
		_ = agentInstance.Start(ctx)
	}()

	time.Sleep(150 * time.Millisecond) // Wait for agent registration

	// 3. Connect simulated Visitor Browser
	clientWS, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Client failed to dial signaling server: %v", err)
	}
	defer clientWS.Close()

	var clientWSMu sync.Mutex
	safeWriteWS := func(m *signaling.Message) error {
		clientWSMu.Lock()
		defer clientWSMu.Unlock()
		return clientWS.WriteJSON(m)
	}

	// Send Join
	if err := safeWriteWS(&signaling.Message{
		Type:   signaling.TypeJoin,
		NodeID: agentNodeID,
	}); err != nil {
		t.Fatalf("Client join error: %v", err)
	}

	var statusMsg signaling.Message
	if err := clientWS.ReadJSON(&statusMsg); err != nil {
		t.Fatalf("Client read peer_status error: %v", err)
	}
	if statusMsg.Status != "online" {
		t.Fatalf("expected peer status online, got %s", statusMsg.Status)
	}

	// 4. Create Client WebRTC PeerConnection
	clientPC, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
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

	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer error: %v", err)
	}
	if err := clientPC.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription error: %v", err)
	}

	// Send Offer
	if err := safeWriteWS(&signaling.Message{
		Type:   signaling.TypeOffer,
		NodeID: agentNodeID,
		SDP:    &offer,
	}); err != nil {
		t.Fatalf("Send Offer error: %v", err)
	}

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

	// 5. Wait for WebRTC P2P DataChannels to Open
	select {
	case <-allOpenCh:
		t.Log("WebRTC Dual DataChannels [p2pcontrol + p2pdata] successfully opened via P2P!")
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for DataChannels to open")
	}

	// 6. Test P2PMux over WebRTC DataChannel
	clientMux := createMux()
	defer clientMux.Close()

	// Test Ping / Pong over WebRTC
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer pingCancel()

	rtt, err := clientMux.Ping(pingCtx)
	if err != nil {
		t.Fatalf("Ping over WebRTC failed: %v", err)
	}
	t.Logf("Ping Pong over WebRTC RTT: %v", rtt)

	// Test Concurrent Streams over WebRTC
	numStreams := 3
	payloadSize := 64 * 1024 // 64KB per stream
	var wg sync.WaitGroup

	for i := 0; i < numStreams; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			stream, err := clientMux.OpenStream()
			if err != nil {
				t.Errorf("stream %d OpenStream error: %v", idx, err)
				return
			}
			defer stream.Close()

			if err := stream.WriteHeaders([]byte(`{"method":"ECHO","path":"/echo"}`)); err != nil {
				t.Errorf("stream %d WriteHeaders error: %v", idx, err)
				return
			}

			payload := make([]byte, payloadSize)
			_, _ = rand.Read(payload)

			go func() {
				_, writeErr := stream.Write(payload)
				if writeErr != nil {
					t.Errorf("stream %d write error: %v", idx, writeErr)
				}
				_ = stream.Close()
			}()

			received := new(bytes.Buffer)
			_, readErr := io.Copy(received, stream)
			if readErr != nil {
				t.Errorf("stream %d read error: %v", idx, readErr)
				return
			}

			if !bytes.Equal(received.Bytes(), payload) {
				t.Errorf("stream %d payload mismatch: sent %d bytes, got %d bytes", idx, len(payload), received.Len())
			} else {
				t.Logf("stream %d echo verified! (%d bytes matched)", idx, received.Len())
			}
		}(i)
	}

	wg.Wait()
}

func boolPtr(b bool) *bool {
	return &b
}
