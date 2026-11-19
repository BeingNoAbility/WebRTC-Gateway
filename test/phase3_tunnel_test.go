package test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
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
	"webrtc-gateway/pkg/proxy"
	"webrtc-gateway/pkg/signaling"
)

// startMockBackendServer creates a mock backend app with Auth, Cookies, Streaming, and WebSockets.
func startMockBackendServer() *httptest.Server {
	mux := http.NewServeMux()

	// 1. Basic JSON API
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "ok",
			"app":    "MockAlistNAS",
		})
	})

	// 2. Login endpoint that sets Cookies (HttpOnly session + client theme)
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name:     "session_token",
			Value:    "secret_token_8899",
			Path:     "/",
			HttpOnly: true,
		})
		http.SetCookie(w, &http.Cookie{
			Name:     "theme",
			Value:    "dark",
			Path:     "/",
			HttpOnly: false,
		})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
	})

	// 3. Protected endpoint requiring session_token cookie
	mux.HandleFunc("/api/profile", func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session_token")
		if err != nil || cookie.Value != "secret_token_8899" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("Unauthorized: missing or invalid session cookie"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"user":  "admin",
			"role":  "superadmin",
			"token": cookie.Value,
		})
	})

	// 4. Streaming download (1MB)
	mux.HandleFunc("/download/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		chunk := bytes.Repeat([]byte("X"), 32*1024) // 32KB
		flusher, ok := w.(http.Flusher)
		for i := 0; i < 32; i++ { // 32 * 32KB = 1MB
			_, _ = w.Write(chunk)
			if ok {
				flusher.Flush()
			}
		}
	})

	// 5. WebSocket Echo endpoint
	wsUpgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	mux.HandleFunc("/ws/echo", func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				break
			}
			if err := conn.WriteMessage(mt, data); err != nil {
				break
			}
		}
	})

	return httptest.NewServer(mux)
}

func TestPhase3FullHTTPAndWSTunnel(t *testing.T) {
	// Step 1: Start Mock Backend
	backend := startMockBackendServer()
	defer backend.Close()

	// Step 2: Start Signaling Server
	signalServer, _ := startMockSignalingServer(t)
	defer signalServer.Close()
	wsSignalURL := "ws" + strings.TrimPrefix(signalServer.URL, "http")

	// Step 3: Start Agent Daemon targeting Mock Backend
	agentNodeID := "nas-node-1"
	ag := agent.New(agentNodeID, wsSignalURL, "", "", backend.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	go func() {
		_ = ag.Start(ctx)
	}()

	time.Sleep(150 * time.Millisecond) // Wait for agent registration

	// Step 4: Connect Simulated Browser Client & Establish WebRTC P2P DataChannel
	clientWS, _, err := websocket.DefaultDialer.Dial(wsSignalURL, nil)
	if err != nil {
		t.Fatalf("Client dial signal error: %v", err)
	}
	defer clientWS.Close()

	var wsMu sync.Mutex
	safeWriteJSON := func(v interface{}) error {
		wsMu.Lock()
		defer wsMu.Unlock()
		return clientWS.WriteJSON(v)
	}

	_ = safeWriteJSON(&signaling.Message{Type: signaling.TypeJoin, NodeID: agentNodeID})
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
			_ = safeWriteJSON(&signaling.Message{
				Type:      signaling.TypeCandidate,
				NodeID:    agentNodeID,
				Candidate: &cand,
			})
		}
	})

	offer, _ := clientPC.CreateOffer(nil)
	_ = clientPC.SetLocalDescription(offer)
	_ = safeWriteJSON(&signaling.Message{Type: signaling.TypeOffer, NodeID: agentNodeID, SDP: &offer})

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
		t.Log("WebRTC Dual DataChannels open!")
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for DataChannels")
	}

	clientMux := createMux()
	defer clientMux.Close()

	// ----------------------------------------------------
	// Verification 1: Basic HTTP GET Request (/api/info)
	// ----------------------------------------------------
	t.Run("HTTP_GET_API_Info", func(t *testing.T) {
		stream, err := clientMux.OpenStream()
		if err != nil {
			t.Fatalf("OpenStream error: %v", err)
		}
		defer stream.Close()

		reqMeta := proxy.RequestMetadata{
			Method:  "GET",
			Path:    "/api/info",
			Headers: map[string]string{"accept": "application/json"},
		}
		reqBytes, _ := json.Marshal(reqMeta)
		if err := stream.WriteHeaders(reqBytes); err != nil {
			t.Fatalf("WriteHeaders error: %v", err)
		}
		_ = stream.Close() // No request body

		// Read response headers
		respHeaderBytes, err := stream.ReadHeaders(context.Background())
		if err != nil {
			t.Fatalf("ReadHeaders error: %v", err)
		}
		var respMeta proxy.ResponseMetadata
		_ = json.Unmarshal(respHeaderBytes, &respMeta)

		if respMeta.Status != 200 {
			t.Fatalf("expected status 200, got %d", respMeta.Status)
		}

		bodyBuf := new(bytes.Buffer)
		_, _ = io.Copy(bodyBuf, stream)

		var info map[string]string
		if err := json.Unmarshal(bodyBuf.Bytes(), &info); err != nil {
			t.Fatalf("unmarshal body error: %v", err)
		}
		if info["app"] != "MockAlistNAS" {
			t.Fatalf("expected app MockAlistNAS, got %s", info["app"])
		}
		t.Logf("HTTP GET /api/info verified: %+v", info)
	})

	// ----------------------------------------------------
	// Verification 2: Login & Cookie Persistence
	// ----------------------------------------------------
	t.Run("HTTP_Login_And_CookieJar_Persistence", func(t *testing.T) {
		// 1. Perform Login POST
		stream1, err := clientMux.OpenStream()
		if err != nil {
			t.Fatalf("OpenStream error: %v", err)
		}
		reqMeta := proxy.RequestMetadata{
			Method: "POST",
			Path:   "/login",
		}
		reqBytes, _ := json.Marshal(reqMeta)
		_ = stream1.WriteHeaders(reqBytes)
		_ = stream1.Close()

		respHeaderBytes, err := stream1.ReadHeaders(context.Background())
		if err != nil {
			t.Fatalf("ReadHeaders error: %v", err)
		}
		var respMeta proxy.ResponseMetadata
		_ = json.Unmarshal(respHeaderBytes, &respMeta)
		if respMeta.Status != 200 {
			t.Fatalf("login failed: status %d", respMeta.Status)
		}

		// Verify client-syncable non-HttpOnly cookie (theme=dark) was extracted
		foundTheme := false
		for _, c := range respMeta.Cookies {
			if strings.Contains(c, "theme=dark") {
				foundTheme = true
				break
			}
		}
		if !foundTheme {
			t.Fatalf("expected non-HttpOnly theme cookie in respMeta.Cookies, got: %+v", respMeta.Cookies)
		}
		stream1.Close()

		// 2. Access Protected /api/profile WITHOUT sending any cookies in request
		// The Go Agent's SessionCookieJar MUST automatically attach the session_token!
		stream2, err := clientMux.OpenStream()
		if err != nil {
			t.Fatalf("OpenStream error: %v", err)
		}
		reqMeta2 := proxy.RequestMetadata{
			Method:  "GET",
			Path:    "/api/profile",
			Headers: map[string]string{"accept": "application/json"},
		}
		reqBytes2, _ := json.Marshal(reqMeta2)
		_ = stream2.WriteHeaders(reqBytes2)
		_ = stream2.Close()

		ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
		respHeaderBytes2, err := stream2.ReadHeaders(ctx2)
		cancel2()
		if err != nil {
			t.Fatalf("ReadHeaders error: %v", err)
		}
		var respMeta2 proxy.ResponseMetadata
		_ = json.Unmarshal(respHeaderBytes2, &respMeta2)

		if respMeta2.Status != 200 {
			t.Fatalf("Protected access failed: status %d (Session Cookie was not preserved!)", respMeta2.Status)
		}

		bodyBuf := new(bytes.Buffer)
		_, _ = io.Copy(bodyBuf, stream2)
		var profile map[string]string
		_ = json.Unmarshal(bodyBuf.Bytes(), &profile)

		if profile["user"] != "admin" || profile["token"] != "secret_token_8899" {
			t.Fatalf("unexpected profile data: %+v", profile)
		}
		t.Logf("CookieJar Persistence verified: successfully accessed protected API: %+v", profile)
		stream2.Close()
	})

	// ----------------------------------------------------
	// Verification 3: Streaming Download (1MB Chunked)
	// ----------------------------------------------------
	t.Run("HTTP_Streaming_1MB_Download", func(t *testing.T) {
		stream, err := clientMux.OpenStream()
		if err != nil {
			t.Fatalf("OpenStream error: %v", err)
		}
		defer stream.Close()

		reqMeta := proxy.RequestMetadata{
			Method: "GET",
			Path:   "/download/stream",
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
			t.Fatalf("download failed: status %d", respMeta.Status)
		}

		// Count streamed bytes
		var totalBytes int64
		buf := make([]byte, 16*1024)
		for {
			n, err := stream.Read(buf)
			if n > 0 {
				totalBytes += int64(n)
			}
			if err != nil {
				if err == io.EOF {
					break
				}
				t.Fatalf("stream read error: %v", err)
			}
		}

		expectedBytes := int64(32 * 1024 * 32) // 1,048,576 bytes
		if totalBytes != expectedBytes {
			t.Fatalf("expected %d streamed bytes, got %d", expectedBytes, totalBytes)
		}
		t.Logf("Streaming download verified: %d bytes (1MB) received smoothly", totalBytes)
	})

	// ----------------------------------------------------
	// Verification 4: WebSocket Proxy Tunnel (/ws/echo)
	// ----------------------------------------------------
	t.Run("WebSocket_Echo_Tunnel", func(t *testing.T) {
		stream, err := clientMux.OpenStream()
		if err != nil {
			t.Fatalf("OpenStream error: %v", err)
		}
		defer stream.Close()

		// Send WS handshake
		wsMeta := proxy.WSConnectMetadata{
			Path:      "/ws/echo",
			Protocols: []string{"chat"},
		}
		raw, _ := json.Marshal(wsMeta)
		if err := stream.WriteHeaders(raw); err != nil {
			t.Fatalf("WriteHeaders error: %v", err)
		}

		// Wait for 101 Switching Protocols
		respHeaderBytes, err := stream.ReadHeaders(context.Background())
		if err != nil {
			t.Fatalf("ReadHeaders error: %v", err)
		}
		var respMeta proxy.ResponseMetadata
		_ = json.Unmarshal(respHeaderBytes, &respMeta)
		if respMeta.Status != http.StatusSwitchingProtocols {
			t.Fatalf("expected status 101 Switching Protocols, got %d (%s)", respMeta.Status, respMeta.Msg)
		}
		t.Log("WebSocket 101 Switching Protocols confirmed over P2P stream!")

		// Send WebSocket Text Message: [Opcode 0x01] + [Length 4 bytes] + [Payload]
		testMsg := "Hello P2P WebSocket Tunnel over WebRTC!"
		packet := make([]byte, 5+len(testMsg))
		packet[0] = 0x01 // Text
		binary.BigEndian.PutUint32(packet[1:5], uint32(len(testMsg)))
		copy(packet[5:], testMsg)

		if _, err := stream.Write(packet); err != nil {
			t.Fatalf("stream write WS packet error: %v", err)
		}

		// Read Echo back
		hdr := make([]byte, 5)
		if _, err := io.ReadFull(stream, hdr); err != nil {
			t.Fatalf("read WS header error: %v", err)
		}
		opcode := hdr[0]
		length := binary.BigEndian.Uint32(hdr[1:5])
		payload := make([]byte, length)
		if _, err := io.ReadFull(stream, payload); err != nil {
			t.Fatalf("read WS payload error: %v", err)
		}

		if opcode != 0x01 {
			t.Fatalf("expected text opcode 0x01, got %d", opcode)
		}
		receivedText := string(payload)
		if receivedText != testMsg {
			t.Fatalf("WS echo mismatch: expected %q, got %q", testMsg, receivedText)
		}
		t.Logf("WebSocket Echo tunnel verified: %q", receivedText)
	})
}
