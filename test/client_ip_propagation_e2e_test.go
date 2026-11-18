package test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"webrtc-gateway/pkg/agent"
	"webrtc-gateway/pkg/mux"
	"webrtc-gateway/pkg/proxy"
	"webrtc-gateway/pkg/signaling"
)

type ipAwareBroker struct {
	mu      sync.RWMutex
	agents  map[string]*testPeerSession
	clients map[string]*testPeerSession
}

func newIPAwareBroker() *ipAwareBroker {
	return &ipAwareBroker{
		agents:  make(map[string]*testPeerSession),
		clients: make(map[string]*testPeerSession),
	}
}

func (b *ipAwareBroker) handleConn(conn *websocket.Conn, remoteIP string) {
	session := &testPeerSession{conn: conn}
	var role, nodeID, sessionID string
	defer func() {
		_ = conn.Close()
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
			break
		}

		switch msg.Type {
		case signaling.TypeRegister:
			role = "agent"
			nodeID = msg.NodeID
			b.mu.Lock()
			b.agents[nodeID] = session
			b.mu.Unlock()
			_ = session.send(&signaling.Message{
				Type:   signaling.TypeRegistered,
				NodeID: nodeID,
			})

		case signaling.TypeJoin:
			role = "client"
			sessionID = "client-sess-" + remoteIP
			b.mu.Lock()
			b.clients[sessionID] = session
			b.mu.Unlock()
			_ = session.send(&signaling.Message{
				Type:      signaling.TypePeerStatus,
				NodeID:    msg.NodeID,
				SessionID: sessionID,
				Status:    "online",
			})

		case signaling.TypeOffer:
			// Enforce authoritative ClientIP assignment, overriding any client spoofing
			msg.SessionID = sessionID
			msg.ClientIP = remoteIP

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

func TestClientIP_EndToEnd_PropagationAndAntiSpoofing(t *testing.T) {
	var receivedXFF, receivedXRealIP string
	var reqMu sync.Mutex
	reqDone := make(chan struct{}, 1)

	// 1. Mock Backend Web Application / WAF
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqMu.Lock()
		receivedXFF = r.Header.Get("X-Forwarded-For")
		receivedXRealIP = r.Header.Get("X-Real-IP")
		reqMu.Unlock()

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("WAF/Backend OK"))

		select {
		case reqDone <- struct{}{}:
		default:
		}
	}))
	defer backend.Close()

	// 2. Signaling Server with IP extraction
	broker := newIPAwareBroker()
	signalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientIP := r.Header.Get("CF-Connecting-IP")
		if clientIP == "" {
			clientIP = "127.0.0.1"
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		broker.handleConn(conn, clientIP)
	}))
	defer signalServer.Close()

	sigWSURL := "ws://" + strings.TrimPrefix(signalServer.URL, "http://")
	nodeID := "test-waf-protected-node"

	// 3. Start Agent
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ag := agent.New(nodeID, sigWSURL, "", "", backend.URL)
	go func() {
		_ = ag.Start(ctx)
	}()
	defer ag.Close()

	time.Sleep(100 * time.Millisecond)

	// 4. Client connects to signaling server with CF-Connecting-IP header (simulating 203.0.113.195)
	dialer := websocket.DefaultDialer
	clientHeaders := make(http.Header)
	authoritativeIP := "203.0.113.195"
	clientHeaders.Set("CF-Connecting-IP", authoritativeIP)

	clientWS, _, err := dialer.Dial(sigWSURL, clientHeaders)
	if err != nil {
		t.Fatalf("Client WS dial failed: %v", err)
	}
	defer clientWS.Close()

	// Join
	if err := clientWS.WriteJSON(&signaling.Message{Type: signaling.TypeJoin, NodeID: nodeID}); err != nil {
		t.Fatalf("Client Join failed: %v", err)
	}

	var statusMsg signaling.Message
	if err := clientWS.ReadJSON(&statusMsg); err != nil {
		t.Fatalf("Client read PeerStatus failed: %v", err)
	}
	sessionID := statusMsg.SessionID

	// 5. Establish WebRTC PeerConnection
	clientPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("Create client PC failed: %v", err)
	}
	defer clientPC.Close()

	dc, err := clientPC.CreateDataChannel("p2pmux", nil)
	if err != nil {
		t.Fatalf("CreateDataChannel failed: %v", err)
	}

	var clientWSMu sync.Mutex
	safeWriteClientWS := func(m *signaling.Message) error {
		clientWSMu.Lock()
		defer clientWSMu.Unlock()
		return clientWS.WriteJSON(m)
	}

	clientPC.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			init := c.ToJSON()
			_ = safeWriteClientWS(&signaling.Message{
				Type:      signaling.TypeCandidate,
				NodeID:    nodeID,
				SessionID: sessionID,
				Candidate: &init,
			})
		}
	})

	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer failed: %v", err)
	}
	_ = clientPC.SetLocalDescription(offer)

	// Client sends Offer attempting to spoof ClientIP to 10.0.0.1!
	if err := safeWriteClientWS(&signaling.Message{
		Type:      signaling.TypeOffer,
		NodeID:    nodeID,
		SessionID: sessionID,
		ClientIP:  "10.0.0.1", // Attacker trying to spoof IP in JSON!
		SDP:       &offer,
	}); err != nil {
		t.Fatalf("Send Offer failed: %v", err)
	}

	// Signaling loop on client side to receive Answer & Candidates
	answerReceived := make(chan struct{}, 1)
	go func() {
		for {
			var sigMsg signaling.Message
			if err := clientWS.ReadJSON(&sigMsg); err != nil {
				return
			}
			switch sigMsg.Type {
			case signaling.TypeAnswer:
				if sigMsg.SDP != nil {
					_ = clientPC.SetRemoteDescription(*sigMsg.SDP)
					select {
					case answerReceived <- struct{}{}:
					default:
					}
				}
			case signaling.TypeCandidate:
				if sigMsg.Candidate != nil {
					_ = clientPC.AddICECandidate(*sigMsg.Candidate)
				}
			}
		}
	}()

	select {
	case <-answerReceived:
	case <-time.After(5 * time.Second):
		t.Fatalf("Timed out waiting for WebRTC Answer")
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
	case <-time.After(5 * time.Second):
		t.Fatalf("Timed out waiting for DataChannel open")
	}

	// 6. Send HTTP request over DataChannel Mux with spoofed headers
	clientMux := mux.NewMux(dc, true)
	defer clientMux.Close()

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		_ = clientMux.HandleIncomingMessage(msg.Data)
	})

	stream, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}
	defer stream.Close()

	// Spoofing attempt inside HTTP headers:
	reqMeta := proxy.RequestMetadata{
		Method: "GET",
		Path:   "/test-waf-check",
		Headers: map[string]string{
			"X-Forwarded-For": "127.0.0.1, 10.0.0.1",
			"X-Real-IP":       "10.0.0.1",
		},
	}
	metaBytes, _ := json.Marshal(reqMeta)
	if err := stream.WriteHeaders(metaBytes); err != nil {
		t.Fatalf("WriteHeaders failed: %v", err)
	}
	_ = stream.Close()

	select {
	case <-reqDone:
	case <-time.After(10 * time.Second):
		t.Fatalf("Timed out waiting for request to reach backend")
	}

	reqMu.Lock()
	defer reqMu.Unlock()

	// 7. Verify that WAF/Backend received the authoritative IP and spoofed headers were completely overwritten!
	if receivedXFF != authoritativeIP {
		t.Errorf("Backend X-Forwarded-For mismatch: expected authoritative %q, got %q", authoritativeIP, receivedXFF)
	}
	if receivedXRealIP != authoritativeIP {
		t.Errorf("Backend X-Real-IP mismatch: expected authoritative %q, got %q", authoritativeIP, receivedXRealIP)
	}
}
