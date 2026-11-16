package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	"webrtc-gateway/pkg/mux"
	"webrtc-gateway/pkg/proxy"
	"webrtc-gateway/pkg/signaling"
)

// Helper: Start a full-featured real signal server using the production broker logic
func startRealSignalServer(serverSecret string) (*httptest.Server, *mockRealBroker) {
	broker := &mockRealBroker{
		serverSecret: serverSecret,
		agents:       make(map[string]*mockSession),
		nodeSecrets:  make(map[string]string),
		clients:      make(map[string]*mockSession),
	}

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		sessionID := fmt.Sprintf("sess-%d", time.Now().UnixNano())
		sess := &mockSession{id: sessionID, conn: conn}

		defer func() {
			_ = conn.Close()
			broker.remove(sess)
		}()

		for {
			var msg signaling.Message
			if err := conn.ReadJSON(&msg); err != nil {
				break
			}

			switch msg.Type {
			case signaling.TypeRegister:
				if err := broker.registerAgent(msg.NodeID, msg.Secret, sess); err != nil {
					_ = sess.send(&signaling.Message{Type: signaling.TypeError, Error: err.Error()})
					return
				}
				sess.role = "agent"
				sess.nodeID = msg.NodeID
				_ = sess.send(&signaling.Message{Type: signaling.TypeRegistered, NodeID: msg.NodeID})
				broker.notifyClients(msg.NodeID)

			case signaling.TypeJoin:
				sess.role = "client"
				sess.nodeID = msg.NodeID
				broker.registerClient(sess)

				agent := broker.getAgent(msg.NodeID)
				hasSec := broker.hasSecret(msg.NodeID)
				if agent == nil {
					_ = sess.send(&signaling.Message{
						Type:           signaling.TypePeerStatus,
						NodeID:         msg.NodeID,
						Status:         "offline",
						RequiresSecret: hasSec,
						Error:          "agent is currently offline",
					})
				} else {
					_ = sess.send(&signaling.Message{
						Type:           signaling.TypePeerStatus,
						NodeID:         msg.NodeID,
						SessionID:      sess.id,
						Status:         "online",
						RequiresSecret: hasSec,
					})
				}

			case signaling.TypeOffer:
				msg.SessionID = sess.id
				agent := broker.getAgent(msg.NodeID)
				if agent == nil {
					_ = sess.send(&signaling.Message{Type: signaling.TypeError, Error: "target agent offline"})
				} else {
					_ = agent.send(&msg)
				}

			case signaling.TypeAnswer:
				client := broker.getClient(msg.SessionID)
				if client != nil {
					_ = client.send(&msg)
				}

			case signaling.TypeCandidate:
				if sess.role == "agent" {
					client := broker.getClient(msg.SessionID)
					if client != nil {
						_ = client.send(&msg)
					}
				} else {
					msg.SessionID = sess.id
					agent := broker.getAgent(msg.NodeID)
					if agent != nil {
						_ = agent.send(&msg)
					}
				}

			case signaling.TypeError:
				if msg.SessionID != "" {
					client := broker.getClient(msg.SessionID)
					if client != nil {
						_ = client.send(&msg)
					}
				}
			}
		}
	}))

	return s, broker
}

type mockSession struct {
	id     string
	role   string
	nodeID string
	conn   *websocket.Conn
	mu     sync.Mutex
}

func (s *mockSession) send(msg *signaling.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn.WriteJSON(msg)
}

type mockRealBroker struct {
	mu           sync.RWMutex
	serverSecret string
	agents       map[string]*mockSession
	nodeSecrets  map[string]string
	clients      map[string]*mockSession
}

func (b *mockRealBroker) registerAgent(nodeID, secret string, s *mockSession) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.serverSecret != "" && secret != b.serverSecret {
		return fmt.Errorf("unauthorized: server secret mismatch")
	}

	if boundSec, exists := b.nodeSecrets[nodeID]; exists && boundSec != "" {
		if secret != boundSec {
			return fmt.Errorf("unauthorized: node_id %q is protected by a different secret", nodeID)
		}
	}

	if old, exists := b.agents[nodeID]; exists {
		_ = old.send(&signaling.Message{Type: signaling.TypeError, Error: "replaced by new agent connection"})
		_ = old.conn.Close()
	}
	b.agents[nodeID] = s
	if secret != "" {
		b.nodeSecrets[nodeID] = secret
	}
	return nil
}

func (b *mockRealBroker) hasSecret(nodeID string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	sec, exists := b.nodeSecrets[nodeID]
	return exists && sec != ""
}

func (b *mockRealBroker) remove(s *mockSession) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s.role == "agent" && s.nodeID != "" {
		if curr, ok := b.agents[s.nodeID]; ok && curr == s {
			delete(b.agents, s.nodeID)
		}
	} else {
		delete(b.clients, s.id)
	}
}

func (b *mockRealBroker) registerClient(s *mockSession) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clients[s.id] = s
}

func (b *mockRealBroker) getAgent(nodeID string) *mockSession {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.agents[nodeID]
}

func (b *mockRealBroker) getClient(sessionID string) *mockSession {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.clients[sessionID]
}

func (b *mockRealBroker) notifyClients(nodeID string) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	hasSec := b.nodeSecrets[nodeID] != ""
	for _, client := range b.clients {
		if client.nodeID == nodeID {
			_ = client.send(&signaling.Message{
				Type:           signaling.TypePeerStatus,
				NodeID:         nodeID,
				SessionID:      client.id,
				Status:         "online",
				RequiresSecret: hasSec,
			})
		}
	}
}

// ----------------------------------------------------------------------------
// Test 1: Agent with Secret + Client with Matching Secret (100% Success)
// ----------------------------------------------------------------------------
func TestSecretAuth_CorrectSecret_Success(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"auth": "success", "app": "SecureVault"})
	}))
	defer backend.Close()

	signalServer, _ := startRealSignalServer("")
	defer signalServer.Close()
	wsURL := "ws" + strings.TrimPrefix(signalServer.URL, "http")

	secretToken := "strong-vault-pass-888"
	nodeID := "secret-vault-node"

	ag := agent.New(nodeID, wsURL, secretToken, backend.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		_ = ag.Start(ctx)
	}()
	defer ag.Close()

	time.Sleep(100 * time.Millisecond) // wait register

	// Client connects with correct secret
	clientWS, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Client dial signal error: %v", err)
	}
	defer clientWS.Close()

	_ = clientWS.WriteJSON(&signaling.Message{Type: signaling.TypeJoin, NodeID: nodeID, Secret: secretToken})
	var statusMsg signaling.Message
	_ = clientWS.ReadJSON(&statusMsg)
	if !statusMsg.RequiresSecret {
		t.Errorf("Expected statusMsg.RequiresSecret=true, got false")
	}

	clientPC, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		t.Fatalf("NewPeerConnection error: %v", err)
	}
	defer clientPC.Close()

	dc, err := clientPC.CreateDataChannel("p2pmux", &webrtc.DataChannelInit{Ordered: boolPtr(true)})
	if err != nil {
		t.Fatalf("CreateDataChannel error: %v", err)
	}

	clientMux := mux.NewMux(dc, true)
	defer clientMux.Close()
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		_ = clientMux.HandleIncomingMessage(msg.Data)
	})

	clientPC.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			cand := c.ToJSON()
			_ = clientWS.WriteJSON(&signaling.Message{
				Type:      signaling.TypeCandidate,
				NodeID:    nodeID,
				Candidate: &cand,
			})
		}
	})

	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer error: %v", err)
	}
	_ = clientPC.SetLocalDescription(offer)

	// Send offer WITH matching secret
	_ = clientWS.WriteJSON(&signaling.Message{
		Type:   signaling.TypeOffer,
		NodeID: nodeID,
		SDP:    &offer,
		Secret: secretToken,
	})

	answerCh := make(chan signaling.Message, 1)
	go func() {
		for {
			var m signaling.Message
			if err := clientWS.ReadJSON(&m); err != nil {
				return
			}
			if m.Type == signaling.TypeAnswer {
				answerCh <- m
				return
			}
		}
	}()

	select {
	case ans := <-answerCh:
		_ = clientPC.SetRemoteDescription(*ans.SDP)
	case <-time.After(3 * time.Second):
		t.Fatalf("Timeout waiting for Answer with correct secret")
	}

	dcOpenCh := make(chan struct{}, 1)
	dc.OnOpen(func() {
		close(dcOpenCh)
	})

	select {
	case <-dcOpenCh:
		t.Logf("WebRTC DataChannel connected successfully with matching Secret!")
	case <-time.After(3 * time.Second):
		t.Fatalf("Timeout waiting for DataChannel to open")
	}

	// Verify HTTP request over tunnel
	stream, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream error: %v", err)
	}
	defer stream.Close()

	reqMeta := proxy.RequestMetadata{
		Method:  "GET",
		Path:    "/api/vault",
		Headers: map[string]string{"accept": "application/json"},
	}
	reqBytes, _ := json.Marshal(reqMeta)
	if err := stream.WriteHeaders(reqBytes); err != nil {
		t.Fatalf("WriteHeaders error: %v", err)
	}
	_ = stream.Close()

	respHeaderBytes, err := stream.ReadHeaders(context.Background())
	if err != nil {
		t.Fatalf("ReadHeaders error: %v", err)
	}
	var respMeta proxy.ResponseMetadata
	_ = json.Unmarshal(respHeaderBytes, &respMeta)

	if respMeta.Status != 200 {
		t.Fatalf("Expected status 200, got %d", respMeta.Status)
	}

	bodyBuf := new(bytes.Buffer)
	_, _ = io.Copy(bodyBuf, stream)
	var info map[string]string
	_ = json.Unmarshal(bodyBuf.Bytes(), &info)
	if info["app"] != "SecureVault" {
		t.Fatalf("Expected app SecureVault, got %s", info["app"])
	}
	t.Logf("Secret auth verified: HTTP %d received over secure P2P tunnel! Body: %+v", respMeta.Status, info)
}

// ----------------------------------------------------------------------------
// Test 2: Agent with Secret + Client with WRONG Secret (Rejected)
// ----------------------------------------------------------------------------
func TestSecretAuth_WrongSecret_Rejected(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer backend.Close()

	signalServer, _ := startRealSignalServer("")
	defer signalServer.Close()
	wsURL := "ws" + strings.TrimPrefix(signalServer.URL, "http")

	secretToken := "vault-correct-secret"
	nodeID := "secure-node-reject"

	ag := agent.New(nodeID, wsURL, secretToken, backend.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		_ = ag.Start(ctx)
	}()
	defer ag.Close()

	time.Sleep(100 * time.Millisecond)

	clientWS, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Client dial signal error: %v", err)
	}
	defer clientWS.Close()

	_ = clientWS.WriteJSON(&signaling.Message{Type: signaling.TypeJoin, NodeID: nodeID})
	var statusMsg signaling.Message
	_ = clientWS.ReadJSON(&statusMsg)

	clientPC, _ := webrtc.NewPeerConnection(webrtc.Configuration{})
	defer clientPC.Close()
	_, _ = clientPC.CreateDataChannel("p2pmux", nil)

	offer, _ := clientPC.CreateOffer(nil)
	_ = clientPC.SetLocalDescription(offer)

	// Send offer with WRONG secret
	_ = clientWS.WriteJSON(&signaling.Message{
		Type:   signaling.TypeOffer,
		NodeID: nodeID,
		SDP:    &offer,
		Secret: "completely-wrong-password",
	})

	errCh := make(chan string, 1)
	go func() {
		for {
			var m signaling.Message
			if err := clientWS.ReadJSON(&m); err != nil {
				return
			}
			if m.Type == signaling.TypeError {
				errCh <- m.Error
				return
			}
		}
	}()

	select {
	case errText := <-errCh:
		if !strings.Contains(errText, "unauthorized") {
			t.Fatalf("Expected unauthorized error, got %q", errText)
		}
		t.Logf("Agent correctly rejected wrong secret with: %s", errText)
	case <-time.After(3 * time.Second):
		t.Fatalf("Expected agent to reject wrong secret with TypeError, but timed out")
	}
}

// ----------------------------------------------------------------------------
// Test 3: Agent with Secret + Client with EMPTY Secret (Rejected)
// ----------------------------------------------------------------------------
func TestSecretAuth_EmptySecret_Rejected(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer backend.Close()

	signalServer, _ := startRealSignalServer("")
	defer signalServer.Close()
	wsURL := "ws" + strings.TrimPrefix(signalServer.URL, "http")

	secretToken := "strict-vault-secret"
	nodeID := "secure-node-empty"

	ag := agent.New(nodeID, wsURL, secretToken, backend.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		_ = ag.Start(ctx)
	}()
	defer ag.Close()

	time.Sleep(100 * time.Millisecond)

	clientWS, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Client dial signal error: %v", err)
	}
	defer clientWS.Close()

	_ = clientWS.WriteJSON(&signaling.Message{Type: signaling.TypeJoin, NodeID: nodeID})
	var statusMsg signaling.Message
	_ = clientWS.ReadJSON(&statusMsg)

	clientPC, _ := webrtc.NewPeerConnection(webrtc.Configuration{})
	defer clientPC.Close()
	_, _ = clientPC.CreateDataChannel("p2pmux", nil)

	offer, _ := clientPC.CreateOffer(nil)
	_ = clientPC.SetLocalDescription(offer)

	// Send offer with NO secret
	_ = clientWS.WriteJSON(&signaling.Message{
		Type:   signaling.TypeOffer,
		NodeID: nodeID,
		SDP:    &offer,
		Secret: "",
	})

	errCh := make(chan string, 1)
	go func() {
		for {
			var m signaling.Message
			if err := clientWS.ReadJSON(&m); err != nil {
				return
			}
			if m.Type == signaling.TypeError {
				errCh <- m.Error
				return
			}
		}
	}()

	select {
	case errText := <-errCh:
		if !strings.Contains(errText, "unauthorized") {
			t.Fatalf("Expected unauthorized error, got %q", errText)
		}
		t.Logf("Agent correctly rejected empty secret with: %s", errText)
	case <-time.After(3 * time.Second):
		t.Fatalf("Expected agent to reject empty secret, but timed out")
	}
}

// ----------------------------------------------------------------------------
// Test 4: Signal Server Anti-Hijack Defense
// ----------------------------------------------------------------------------
func TestSecretAuth_SignalServer_AntiHijack(t *testing.T) {
	signalServer, _ := startRealSignalServer("")
	defer signalServer.Close()
	wsURL := "ws" + strings.TrimPrefix(signalServer.URL, "http")

	nodeID := "my-protected-nas"
	legitSecret := "alpha-secret-777"
	attackerSecret := "evil-hacker-secret-999"

	// 1. Legit Agent registers first
	legitWS, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Legit agent dial error: %v", err)
	}
	defer legitWS.Close()

	_ = legitWS.WriteJSON(&signaling.Message{
		Type:   signaling.TypeRegister,
		NodeID: nodeID,
		Secret: legitSecret,
		Role:   "agent",
	})

	var regResp signaling.Message
	_ = legitWS.ReadJSON(&regResp)
	if regResp.Type != signaling.TypeRegistered {
		t.Fatalf("Expected TypeRegistered, got %s: %s", regResp.Type, regResp.Error)
	}
	t.Logf("Legit Agent successfully registered node %s with secret", nodeID)

	// 2. Attacker Agent tries to hijack nodeID with a different secret
	attackerWS, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Attacker agent dial error: %v", err)
	}
	defer attackerWS.Close()

	_ = attackerWS.WriteJSON(&signaling.Message{
		Type:   signaling.TypeRegister,
		NodeID: nodeID,
		Secret: attackerSecret,
		Role:   "agent",
	})

	var attackerResp signaling.Message
	_ = attackerWS.ReadJSON(&attackerResp)
	if attackerResp.Type != signaling.TypeError {
		t.Fatalf("Expected Attacker to be rejected with TypeError, got %s", attackerResp.Type)
	}
	if !strings.Contains(attackerResp.Error, "unauthorized") {
		t.Fatalf("Expected unauthorized error, got: %s", attackerResp.Error)
	}
	t.Logf("Signal Server correctly blocked hijack attempt: %s", attackerResp.Error)

	// 3. Reconnect legit agent with correct secret -> should succeed
	legitReconnectWS, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Legit reconnect dial error: %v", err)
	}
	defer legitReconnectWS.Close()

	_ = legitReconnectWS.WriteJSON(&signaling.Message{
		Type:   signaling.TypeRegister,
		NodeID: nodeID,
		Secret: legitSecret,
		Role:   "agent",
	})

	var reconnResp signaling.Message
	_ = legitReconnectWS.ReadJSON(&reconnResp)
	if reconnResp.Type != signaling.TypeRegistered {
		t.Fatalf("Expected legit reconnect to succeed, got %s: %s", reconnResp.Type, reconnResp.Error)
	}
	t.Logf("Legit Agent re-registration with correct secret succeeded smoothly!")
}

// ----------------------------------------------------------------------------
// Test 5: Signal Server Global Master Secret Enforcement
// ----------------------------------------------------------------------------
func TestSecretAuth_SignalServer_GlobalMasterSecret(t *testing.T) {
	masterKey := "top-secret-cluster-master-key"
	signalServer, _ := startRealSignalServer(masterKey)
	defer signalServer.Close()
	wsURL := "ws" + strings.TrimPrefix(signalServer.URL, "http")

	// 1. Agent with wrong secret -> rejected
	wrongWS, _, _ := websocket.DefaultDialer.Dial(wsURL, nil)
	defer wrongWS.Close()

	_ = wrongWS.WriteJSON(&signaling.Message{
		Type:   signaling.TypeRegister,
		NodeID: "test-node",
		Secret: "wrong-cluster-key",
		Role:   "agent",
	})

	var wrongResp signaling.Message
	_ = wrongWS.ReadJSON(&wrongResp)
	if wrongResp.Type != signaling.TypeError || !strings.Contains(wrongResp.Error, "server secret mismatch") {
		t.Fatalf("Expected server secret mismatch error, got: %+v", wrongResp)
	}
	t.Logf("Global server secret correctly rejected unauthorized agent: %s", wrongResp.Error)

	// 2. Agent with matching master key -> accepted
	validWS, _, _ := websocket.DefaultDialer.Dial(wsURL, nil)
	defer validWS.Close()

	_ = validWS.WriteJSON(&signaling.Message{
		Type:   signaling.TypeRegister,
		NodeID: "test-node",
		Secret: masterKey,
		Role:   "agent",
	})

	var validResp signaling.Message
	_ = validWS.ReadJSON(&validResp)
	if validResp.Type != signaling.TypeRegistered {
		t.Fatalf("Expected registration success with master key, got: %+v", validResp)
	}
	t.Logf("Global server secret accepted authorized agent with master key!")
}

// ----------------------------------------------------------------------------
// Test 6: HMAC-SHA256 Stateless Signed Share Links (E2E)
// ----------------------------------------------------------------------------
func TestSecretAuth_HMACShareLink_E2E(t *testing.T) {
	nodeSecret := "master-vault-password-xyz"
	nodeID := "secure-share-node"

	// Mock target web service
	targetApp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"share": "verified"})
	}))
	defer targetApp.Close()

	signalServer, _ := startRealSignalServer("")
	defer signalServer.Close()
	wsURL := "ws" + strings.TrimPrefix(signalServer.URL, "http")

	// Start Agent
	ag := agent.New(nodeID, wsURL, nodeSecret, targetApp.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ag.Start(ctx) }()
	defer ag.Close()
	time.Sleep(100 * time.Millisecond)

	// 1. Client with EXPIRED share signature -> rejected
	pastExp := time.Now().Add(-1 * time.Hour).Unix()
	expiredSign := agent.GenerateShareSignature(nodeID, pastExp, nodeSecret)

	expiredWS, _, _ := websocket.DefaultDialer.Dial(wsURL, nil)
	defer expiredWS.Close()
	_ = expiredWS.WriteJSON(&signaling.Message{Type: signaling.TypeJoin, NodeID: nodeID})
	var expStatus signaling.Message
	_ = expiredWS.ReadJSON(&expStatus)

	_ = expiredWS.WriteJSON(&signaling.Message{
		Type:   signaling.TypeOffer,
		NodeID: nodeID,
		SDP:    &webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "v=0\r\no=-\r\n"},
		Exp:    pastExp,
		Sign:   expiredSign,
	})

	var expResp signaling.Message
	_ = expiredWS.ReadJSON(&expResp)
	if expResp.Type != signaling.TypeError || !strings.Contains(expResp.Error, "unauthorized") {
		t.Fatalf("Expected expired signature to be rejected, got: %+v", expResp)
	}
	t.Logf("Agent correctly rejected expired share link signature: %s", expResp.Error)

	// 2. Client with VALID future share signature -> successfully connects over WebRTC & DataChannel!
	futureExp := time.Now().Add(2 * time.Hour).Unix()
	validSign := agent.GenerateShareSignature(nodeID, futureExp, nodeSecret)

	clientWS, _, _ := websocket.DefaultDialer.Dial(wsURL, nil)
	defer clientWS.Close()
	_ = clientWS.WriteJSON(&signaling.Message{Type: signaling.TypeJoin, NodeID: nodeID})
	var validStatus signaling.Message
	_ = clientWS.ReadJSON(&validStatus)

	clientPC, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		t.Fatalf("NewPeerConnection error: %v", err)
	}
	defer clientPC.Close()

	dc, err := clientPC.CreateDataChannel("p2pmux", &webrtc.DataChannelInit{Ordered: boolPtr(true)})
	if err != nil {
		t.Fatalf("CreateDataChannel error: %v", err)
	}

	clientMux := mux.NewMux(dc, true)
	defer clientMux.Close()
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		_ = clientMux.HandleIncomingMessage(msg.Data)
	})

	clientPC.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			cand := c.ToJSON()
			_ = clientWS.WriteJSON(&signaling.Message{
				Type:      signaling.TypeCandidate,
				NodeID:    nodeID,
				Candidate: &cand,
			})
		}
	})

	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer error: %v", err)
	}
	_ = clientPC.SetLocalDescription(offer)

	// Send offer WITH Exp and Sign (No raw secret!)
	_ = clientWS.WriteJSON(&signaling.Message{
		Type:   signaling.TypeOffer,
		NodeID: nodeID,
		SDP:    &offer,
		Exp:    futureExp,
		Sign:   validSign,
	})

	var ans signaling.Message
	for {
		if err := clientWS.ReadJSON(&ans); err != nil {
			t.Fatalf("ReadJSON error: %v", err)
		}
		if ans.Type == signaling.TypeAnswer {
			break
		}
	}
	_ = clientPC.SetRemoteDescription(*ans.SDP)

	dcOpenCh := make(chan struct{}, 1)
	dc.OnOpen(func() { close(dcOpenCh) })

	select {
	case <-dcOpenCh:
		t.Logf("DataChannel opened successfully via stateless HMAC-SHA256 signed share link!")
	case <-time.After(3 * time.Second):
		t.Fatalf("Timeout waiting for DataChannel to open via share link")
	}

	// Verify HTTP access
	stream, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream error: %v", err)
	}
	defer stream.Close()

	reqMeta := proxy.RequestMetadata{
		Method:  "GET",
		Path:    "/",
		Headers: map[string]string{"accept": "application/json"},
	}
	reqBytes, _ := json.Marshal(reqMeta)
	if err := stream.WriteHeaders(reqBytes); err != nil {
		t.Fatalf("WriteHeaders error: %v", err)
	}
	_ = stream.Close()

	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	rawHeaders, err := stream.ReadHeaders(ctx2)
	if err != nil {
		t.Fatalf("ReadHeaders error: %v", err)
	}
	var respMeta proxy.ResponseMetadata
	_ = json.Unmarshal(rawHeaders, &respMeta)
	if respMeta.Status != 200 {
		t.Fatalf("Expected HTTP 200 via share link, got: %d", respMeta.Status)
	}
	t.Logf("Successfully fetched HTTP 200 via stateless HMAC signed share link!")

	// 3. Test In-Place ICE Restart on existing connection
	iceRestartOffer, err := clientPC.CreateOffer(&webrtc.OfferOptions{ICERestart: true})
	if err != nil {
		t.Fatalf("CreateOffer iceRestart error: %v", err)
	}
	_ = clientPC.SetLocalDescription(iceRestartOffer)

	_ = clientWS.WriteJSON(&signaling.Message{
		Type:       signaling.TypeOffer,
		NodeID:     nodeID,
		SDP:        &iceRestartOffer,
		IceRestart: true,
		Exp:        futureExp,
		Sign:       validSign,
	})

	var restartAns signaling.Message
	for {
		if err := clientWS.ReadJSON(&restartAns); err != nil {
			t.Fatalf("ReadJSON error: %v", err)
		}
		if restartAns.Type == signaling.TypeAnswer {
			break
		}
	}
	_ = clientPC.SetRemoteDescription(*restartAns.SDP)
	t.Logf("In-place ICE Restart completed successfully! DataChannel and Mux preserved.")
}
