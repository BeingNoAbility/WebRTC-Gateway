package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// TestE2E_ICERestart_SessionContinuityAndWeakNetwork validates that during mobile network
// roaming / Wi-Fi to cellular handover:
// 1. Client initiates in-place ICE Restart (`iceRestart: true`) on existing PeerConnection;
// 2. Agent preserves existing PeerConnection, DataChannel and CookieJar session state;
// 3. Authenticated session cookies (e.g. login tokens) are 100% maintained without disruption;
// 4. DataChannel byte stream multiplexing continues seamlessly post-restart.
func TestE2E_ICERestart_SessionContinuityAndWeakNetwork(t *testing.T) {
	const (
		expectedToken = "auth_session_roaming_token_xyz888"
	)

	// Mock Backend with Cookie-based Authentication
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/login":
			http.SetCookie(w, &http.Cookie{
				Name:  "session_id",
				Value: expectedToken,
				Path:  "/",
			})
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"logged_in":true}`))

		case "/dashboard":
			cookie, err := req.Cookie("session_id")
			if err != nil || cookie.Value != expectedToken {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthorized: session lost"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"authorized","user":"mobile_user"}`))

		default:
			http.NotFound(w, req)
		}
	})

	backend := httptest.NewServer(handler)
	defer backend.Close()

	signalServer, _ := startMockSignalingServer(t)
	defer signalServer.Close()
	wsSignalURL := "ws" + strings.TrimPrefix(signalServer.URL, "http")

	nodeID := fmt.Sprintf("ice-roam-node-%d", time.Now().UnixNano())
	ag := agent.New(nodeID, wsSignalURL, "", "", backend.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ag.Start(ctx) }()
	defer ag.Close()

	time.Sleep(120 * time.Millisecond)

	// Setup Client WebRTC & Signaling
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

	_ = safeWriteWS(&signaling.Message{Type: signaling.TypeJoin, NodeID: nodeID})
	var statusMsg signaling.Message
	_ = clientWS.ReadJSON(&statusMsg)

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

	clientPC.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			cand := c.ToJSON()
			_ = safeWriteWS(&signaling.Message{
				Type:      signaling.TypeCandidate,
				NodeID:    nodeID,
				Candidate: &cand,
			})
		}
	})

	initialOffer, _ := clientPC.CreateOffer(nil)
	_ = clientPC.SetLocalDescription(initialOffer)
	_ = safeWriteWS(&signaling.Message{Type: signaling.TypeOffer, NodeID: nodeID, SDP: &initialOffer})

	dcOpenCh := make(chan struct{})
	dc.OnOpen(func() { close(dcOpenCh) })

	answerCh := make(chan webrtc.SessionDescription, 5)
	go func() {
		for {
			var msg signaling.Message
			if err := clientWS.ReadJSON(&msg); err != nil {
				return
			}
			switch msg.Type {
			case signaling.TypeAnswer:
				if msg.SDP != nil {
					answerCh <- *msg.SDP
				}
			case signaling.TypeCandidate:
				if msg.Candidate != nil {
					_ = clientPC.AddICECandidate(*msg.Candidate)
				}
			}
		}
	}()

	select {
	case ans := <-answerCh:
		_ = clientPC.SetRemoteDescription(ans)
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for initial Answer")
	}

	select {
	case <-dcOpenCh:
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for DataChannel open")
	}

	clientMux := mux.NewMux(dc, true)
	defer clientMux.Close()
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		_ = clientMux.HandleIncomingMessage(msg.Data)
	})

	// 1. Initial State: Login and obtain session cookie
	s1, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream 1 error: %v", err)
	}
	m1, _ := json.Marshal(proxy.RequestMetadata{Method: "GET", Path: "/login"})
	_ = s1.WriteHeaders(m1)
	_ = s1.Close()

	h1, err := s1.ReadHeaders(context.Background())
	if err != nil {
		t.Fatalf("ReadHeaders 1 error: %v", err)
	}
	var resp1 proxy.ResponseMetadata
	_ = json.Unmarshal(h1, &resp1)
	if resp1.Status != http.StatusOK {
		t.Fatalf("Login failed, status=%d", resp1.Status)
	}
	t.Logf("✓ Step 1: Login successful, CookieJar populated")

	// 2. Verify Session Works Before ICE Restart
	s2, _ := clientMux.OpenStream()
	m2, _ := json.Marshal(proxy.RequestMetadata{Method: "GET", Path: "/dashboard"})
	_ = s2.WriteHeaders(m2)
	_ = s2.Close()

	h2, _ := s2.ReadHeaders(context.Background())
	var resp2 proxy.ResponseMetadata
	_ = json.Unmarshal(h2, &resp2)
	if resp2.Status != http.StatusOK {
		t.Fatalf("Pre-restart dashboard query failed, status=%d", resp2.Status)
	}
	t.Logf("✓ Step 2: Dashboard query verified with active session cookie")

	// 3. Trigger ICE Restart (Simulate Wi-Fi to 5G handover)
	t.Logf("⚡ Step 3: Triggering in-place ICE Restart...")
	restartOffer, err := clientPC.CreateOffer(&webrtc.OfferOptions{ICERestart: true})
	if err != nil {
		t.Fatalf("CreateOffer with ICERestart error: %v", err)
	}
	_ = clientPC.SetLocalDescription(restartOffer)

	_ = safeWriteWS(&signaling.Message{
		Type:       signaling.TypeOffer,
		NodeID:     nodeID,
		SDP:        &restartOffer,
		IceRestart: true,
	})

	select {
	case restartAns := <-answerCh:
		if err := clientPC.SetRemoteDescription(restartAns); err != nil {
			t.Fatalf("SetRemoteDescription on ICE Restart error: %v", err)
		}
		t.Logf("✓ Step 3: In-place ICE Restart Answer processed successfully")
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for ICE Restart Answer")
	}

	// Wait briefly for ICE connection state to stabilize
	time.Sleep(100 * time.Millisecond)

	// 4. Post-Restart Verification: Query /dashboard on the SAME existing DataChannel/Mux
	// Must retain the same CookieJar and return 200 OK without re-authenticating
	s3, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream 3 after ICE Restart error: %v", err)
	}
	m3, _ := json.Marshal(proxy.RequestMetadata{Method: "GET", Path: "/dashboard"})
	_ = s3.WriteHeaders(m3)
	_ = s3.Close()

	h3, err := s3.ReadHeaders(context.Background())
	if err != nil {
		t.Fatalf("ReadHeaders 3 after ICE Restart error: %v", err)
	}
	var resp3 proxy.ResponseMetadata
	_ = json.Unmarshal(h3, &resp3)
	if resp3.Status != http.StatusOK {
		t.Fatalf("Post-restart dashboard query failed! Session was lost: status=%d", resp3.Status)
	}

	var body3 bytes.Buffer
	buf := make([]byte, 1024)
	for {
		n, err := s3.Read(buf)
		if n > 0 {
			body3.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}

	if !strings.Contains(body3.String(), "authorized") {
		t.Fatalf("Unexpected response body post-restart: %s", body3.String())
	}

	t.Logf("✓ Step 4: Post-ICE Restart verified! Session CookieJar 100%% maintained: %s", body3.String())
}
