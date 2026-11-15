package test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// Helper to setup complete E2E environment (Backend, Signal, Agent, Client WebRTC DataChannel)
func setupE2EEnvironment(t *testing.T, handler http.Handler) (
	backend *httptest.Server,
	signalServer *httptest.Server,
	ag *agent.Agent,
	clientMux *mux.Mux,
	cleanup func(),
) {
	backend = httptest.NewServer(handler)
	signalServer, _ = startMockSignalingServer(t)
	wsSignalURL := "ws" + strings.TrimPrefix(signalServer.URL, "http")

	nodeID := fmt.Sprintf("e2e-node-%d", time.Now().UnixNano())
	ag = agent.New(nodeID, wsSignalURL, "", backend.URL)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = ag.Start(ctx) }()

	time.Sleep(120 * time.Millisecond)

	// Setup Client WebRTC
	clientWS, _, err := websocket.DefaultDialer.Dial(wsSignalURL, nil)
	if err != nil {
		t.Fatalf("Client dial signal error: %v", err)
	}

	_ = clientWS.WriteJSON(&signaling.Message{Type: signaling.TypeJoin, NodeID: nodeID})
	var statusMsg signaling.Message
	_ = clientWS.ReadJSON(&statusMsg)

	clientPC, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		t.Fatalf("NewPeerConnection error: %v", err)
	}

	dc, err := clientPC.CreateDataChannel("p2pmux", &webrtc.DataChannelInit{Ordered: boolPtr(true)})
	if err != nil {
		t.Fatalf("CreateDataChannel error: %v", err)
	}

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

	offer, _ := clientPC.CreateOffer(nil)
	_ = clientPC.SetLocalDescription(offer)
	_ = clientWS.WriteJSON(&signaling.Message{Type: signaling.TypeOffer, NodeID: nodeID, SDP: &offer})

	dcOpenCh := make(chan struct{})
	dc.OnOpen(func() { close(dcOpenCh) })

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
	case <-dcOpenCh:
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for WebRTC DataChannel to open")
	}

	clientMux = mux.NewMux(dc, true)
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		_ = clientMux.HandleIncomingMessage(msg.Data)
	})

	cleanup = func() {
		cancel()
		ag.Close()
		clientMux.Close()
		_ = clientPC.Close()
		_ = clientWS.Close()
		signalServer.Close()
		backend.Close()
	}

	return backend, signalServer, ag, clientMux, cleanup
}

// 1. E2E POST Upload with Body & Exact Content-Length
func TestE2E_HTTP_PostUploadWithBodyAndContentLength(t *testing.T) {
	payloadSize := 256 * 1024 // 256KB
	uploadData := make([]byte, payloadSize)
	_, _ = rand.Read(uploadData)
	expectedHash := sha256.Sum256(uploadData)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.ContentLength != int64(payloadSize) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, "Bad Content-Length: got %d, expected %d", r.ContentLength, payloadSize)
			return
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		actualHash := sha256.Sum256(bodyBytes)
		if actualHash != expectedHash {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, "SHA256 mismatch")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":   "created",
			"received": len(bodyBytes),
		})
	})

	_, _, _, clientMux, cleanup := setupE2EEnvironment(t, handler)
	defer cleanup()

	stream, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}
	defer stream.Close()

	reqMeta := proxy.RequestMetadata{
		Method: "POST",
		Path:   "/api/upload",
		Headers: map[string]string{
			"content-type":   "application/octet-stream",
			"content-length": strconv.Itoa(payloadSize),
		},
	}
	metaBytes, _ := json.Marshal(reqMeta)
	if err := stream.WriteHeaders(metaBytes); err != nil {
		t.Fatalf("WriteHeaders failed: %v", err)
	}

	// Stream upload body in chunks
	chunkSize := 32 * 1024
	for i := 0; i < len(uploadData); i += chunkSize {
		end := i + chunkSize
		if end > len(uploadData) {
			end = len(uploadData)
		}
		if _, err := stream.Write(uploadData[i:end]); err != nil {
			t.Fatalf("Stream write chunk failed: %v", err)
		}
	}
	_ = stream.Close() // Send FIN

	// Read response headers
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	respHeadersPayload, err := stream.ReadHeaders(ctx)
	if err != nil {
		t.Fatalf("ReadHeaders failed: %v", err)
	}

	var respMeta proxy.ResponseMetadata
	_ = json.Unmarshal(respHeadersPayload, &respMeta)
	if respMeta.Status != http.StatusCreated {
		t.Fatalf("Expected 201 Created, got %d: %s", respMeta.Status, respMeta.Msg)
	}

	respBody, _ := io.ReadAll(stream)
	if !strings.Contains(string(respBody), `"status":"created"`) {
		t.Errorf("Unexpected response body: %s", string(respBody))
	}
	t.Logf("POST upload of 256KB verified with status 201 Created and exact SHA256 match!")
}

// 2. E2E Range & 206 Partial Content Media Streaming
func TestE2E_HTTP_RangeAnd206MediaStreaming(t *testing.T) {
	// Virtual 4MB binary media file
	totalSize := 4 * 1024 * 1024
	mediaData := make([]byte, totalSize)
	for i := range mediaData {
		mediaData[i] = byte(i % 251)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "" {
			w.Header().Set("Content-Length", strconv.Itoa(totalSize))
			w.Header().Set("Content-Type", "video/mp4")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(mediaData)
			return
		}

		// Parse Range: bytes=1048576-2097151
		var start, end int
		_, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if end >= totalSize {
			end = totalSize - 1
		}
		length := end - start + 1

		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, totalSize))
		w.Header().Set("Content-Length", strconv.Itoa(length))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(mediaData[start : end+1])
	})

	_, _, _, clientMux, cleanup := setupE2EEnvironment(t, handler)
	defer cleanup()

	// Request slice: bytes=1048576-2097151 (1MB slice)
	start := 1048576
	end := 2097151
	expectedSlice := mediaData[start : end+1]
	expectedHash := hex.EncodeToString(sha256.New().Sum(expectedSlice))

	stream, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}
	defer stream.Close()

	reqMeta := proxy.RequestMetadata{
		Method: "GET",
		Path:   "/video/stream.m4s",
		Headers: map[string]string{
			"Range": fmt.Sprintf("bytes=%d-%d", start, end),
		},
	}
	metaBytes, _ := json.Marshal(reqMeta)
	_ = stream.WriteHeaders(metaBytes)
	_ = stream.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	respHeadersPayload, err := stream.ReadHeaders(ctx)
	if err != nil {
		t.Fatalf("ReadHeaders failed: %v", err)
	}

	var respMeta proxy.ResponseMetadata
	_ = json.Unmarshal(respHeadersPayload, &respMeta)
	if respMeta.Status != http.StatusPartialContent {
		t.Fatalf("Expected 206 Partial Content, got %d", respMeta.Status)
	}

	cr := respMeta.Headers["content-range"]
	expectedCR := fmt.Sprintf("bytes %d-%d/%d", start, end, totalSize)
	if cr != expectedCR {
		t.Errorf("Content-Range mismatch: got %q, expected %q", cr, expectedCR)
	}

	receivedSlice, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if len(receivedSlice) != len(expectedSlice) {
		t.Fatalf("Slice length mismatch: got %d, expected %d", len(receivedSlice), len(expectedSlice))
	}

	actualHash := hex.EncodeToString(sha256.New().Sum(receivedSlice))
	if actualHash != expectedHash {
		t.Fatalf("Media slice hash mismatch")
	}
	t.Logf("206 Partial Content media slice (1MB) verified with exact binary checksum!")
}

// 3. E2E Redirects (302) & Location Header Rewriting
func TestE2E_HTTP_RedirectAndLocationRewrite(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login-redirect":
			// Internal redirect
			w.Header().Set("Location", "/dashboard")
			w.WriteHeader(http.StatusFound)
		case "/external-oauth":
			// External 3rd party redirect
			w.Header().Set("Location", "https://accounts.google.com/o/oauth2/auth")
			w.WriteHeader(http.StatusFound)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	_, _, _, clientMux, cleanup := setupE2EEnvironment(t, handler)
	defer cleanup()

	// 1. Internal redirect
	s1, _ := clientMux.OpenStream()
	reqMeta1 := proxy.RequestMetadata{Method: "GET", Path: "/login-redirect"}
	m1, _ := json.Marshal(reqMeta1)
	_ = s1.WriteHeaders(m1)
	_ = s1.Close()

	hBytes1, _ := s1.ReadHeaders(context.Background())
	var respMeta1 proxy.ResponseMetadata
	_ = json.Unmarshal(hBytes1, &respMeta1)
	if respMeta1.Status != http.StatusFound {
		t.Errorf("Expected 302 Found, got %d", respMeta1.Status)
	}
	if respMeta1.Headers["location"] != "/dashboard" {
		t.Errorf("Location header mismatch: got %q, expected /dashboard", respMeta1.Headers["location"])
	}
	s1.Close()

	// 2. External redirect
	s2, _ := clientMux.OpenStream()
	reqMeta2 := proxy.RequestMetadata{Method: "GET", Path: "/external-oauth"}
	m2, _ := json.Marshal(reqMeta2)
	_ = s2.WriteHeaders(m2)
	_ = s2.Close()

	hBytes2, _ := s2.ReadHeaders(context.Background())
	var respMeta2 proxy.ResponseMetadata
	_ = json.Unmarshal(hBytes2, &respMeta2)
	if respMeta2.Headers["location"] != "https://accounts.google.com/o/oauth2/auth" {
		t.Errorf("External location should remain untouched: got %q", respMeta2.Headers["location"])
	}
	s2.Close()
}

// 4. E2E Empty Body Statuses (204 No Content, 304 Not Modified)
func TestE2E_HTTP_EmptyBodyStatuses_204_304(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/empty-204":
			w.WriteHeader(http.StatusNoContent)
		case "/assets/style.css":
			if r.Header.Get("If-None-Match") == `"v1.2.3"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"v1.2.3"`)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("body: { color: red; }"))
		}
	})

	_, _, _, clientMux, cleanup := setupE2EEnvironment(t, handler)
	defer cleanup()

	// Test 204
	s1, _ := clientMux.OpenStream()
	reqMeta1 := proxy.RequestMetadata{Method: "DELETE", Path: "/api/empty-204"}
	m1, _ := json.Marshal(reqMeta1)
	_ = s1.WriteHeaders(m1)
	_ = s1.Close()

	hBytes1, _ := s1.ReadHeaders(context.Background())
	var respMeta1 proxy.ResponseMetadata
	_ = json.Unmarshal(hBytes1, &respMeta1)
	if respMeta1.Status != http.StatusNoContent {
		t.Errorf("Expected 204 No Content, got %d", respMeta1.Status)
	}
	b1, _ := io.ReadAll(s1)
	if len(b1) != 0 {
		t.Errorf("204 must have 0 body bytes, got %d", len(b1))
	}
	s1.Close()

	// Test 304
	s2, _ := clientMux.OpenStream()
	reqMeta2 := proxy.RequestMetadata{
		Method: "GET",
		Path:   "/assets/style.css",
		Headers: map[string]string{
			"If-None-Match": `"v1.2.3"`,
		},
	}
	m2, _ := json.Marshal(reqMeta2)
	_ = s2.WriteHeaders(m2)
	_ = s2.Close()

	hBytes2, _ := s2.ReadHeaders(context.Background())
	var respMeta2 proxy.ResponseMetadata
	_ = json.Unmarshal(hBytes2, &respMeta2)
	if respMeta2.Status != http.StatusNotModified {
		t.Errorf("Expected 304 Not Modified, got %d", respMeta2.Status)
	}
	b2, _ := io.ReadAll(s2)
	if len(b2) != 0 {
		t.Errorf("304 must have 0 body bytes, got %d", len(b2))
	}
	s2.Close()
}

// 5. E2E High Concurrency: 20 Simultaneous HTTP Streams over Single WebRTC DataChannel
func TestE2E_HighConcurrency_20_Parallel_Requests(t *testing.T) {
	concurrency := 20

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idStr := r.URL.Query().Get("id")
		id, _ := strconv.Atoi(idStr)

		// Staggered response to simulate asynchronous real backend processing
		sleepMs := time.Duration(10+(id%5)*10) * time.Millisecond
		time.Sleep(sleepMs)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     id,
			"sqr":    id * id,
			"random": math.Sin(float64(id)),
		})
	})

	_, _, _, clientMux, cleanup := setupE2EEnvironment(t, handler)
	defer cleanup()

	var wg sync.WaitGroup
	errCh := make(chan error, concurrency)

	startTime := time.Now()
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(taskID int) {
			defer wg.Done()

			stream, err := clientMux.OpenStream()
			if err != nil {
				errCh <- fmt.Errorf("task %d OpenStream error: %w", taskID, err)
				return
			}
			defer stream.Close()

			reqMeta := proxy.RequestMetadata{
				Method: "GET",
				Path:   fmt.Sprintf("/api/calc?id=%d", taskID),
			}
			metaBytes, _ := json.Marshal(reqMeta)
			if err := stream.WriteHeaders(metaBytes); err != nil {
				errCh <- fmt.Errorf("task %d WriteHeaders error: %w", taskID, err)
				return
			}
			_ = stream.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			respHeadersPayload, err := stream.ReadHeaders(ctx)
			if err != nil {
				errCh <- fmt.Errorf("task %d ReadHeaders error: %w", taskID, err)
				return
			}

			var respMeta proxy.ResponseMetadata
			_ = json.Unmarshal(respHeadersPayload, &respMeta)
			if respMeta.Status != http.StatusOK {
				errCh <- fmt.Errorf("task %d bad status: %d", taskID, respMeta.Status)
				return
			}

			bodyBytes, err := io.ReadAll(stream)
			if err != nil {
				errCh <- fmt.Errorf("task %d ReadAll error: %w", taskID, err)
				return
			}

			var result struct {
				ID  int `json:"id"`
				Sqr int `json:"sqr"`
			}
			if err := json.Unmarshal(bodyBytes, &result); err != nil {
				errCh <- fmt.Errorf("task %d Unmarshal error: %w", taskID, err)
				return
			}

			if result.ID != taskID || result.Sqr != taskID*taskID {
				errCh <- fmt.Errorf("task %d payload mismatch: got id=%d, sqr=%d", taskID, result.ID, result.Sqr)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("Concurrent request failure: %v", err)
	}
	t.Logf("20 concurrent HTTP requests over single DataChannel completed in %v with 100%% integrity!", time.Since(startTime))
}

// 6. E2E Stream Cancellation Midway (User Abort / Reset)
func TestE2E_StreamCancellation_Midway(t *testing.T) {
	backendCancelled := make(chan struct{})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		flusher, ok := w.(http.Flusher)
		chunk := bytes.Repeat([]byte("A"), 16*1024)

		for i := 0; i < 100; i++ {
			select {
			case <-r.Context().Done():
				// Confirms the backend received cancellation when stream was reset!
				close(backendCancelled)
				return
			default:
				_, _ = w.Write(chunk)
				if ok {
					flusher.Flush()
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
	})

	_, _, _, clientMux, cleanup := setupE2EEnvironment(t, handler)
	defer cleanup()

	stream, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}

	reqMeta := proxy.RequestMetadata{Method: "GET", Path: "/large/stream"}
	m, _ := json.Marshal(reqMeta)
	_ = stream.WriteHeaders(m)
	_ = stream.Close()

	// Read first chunk
	buf := make([]byte, 16*1024)
	_, _ = stream.Read(buf)

	// Client cancels stream immediately (simulating page navigation or user abort)
	_ = stream.Reset()

	// Verify backend context receives cancellation within 2 seconds
	select {
	case <-backendCancelled:
		t.Log("Backend HTTP request context was cleanly canceled on stream reset!")
	case <-time.After(2 * time.Second):
		t.Errorf("Backend context was not canceled after client stream.Reset()")
	}
}

// 7. E2E DataChannel & Session Reconnection Resilience
func TestE2E_DataChannel_ReconnectionResilience(t *testing.T) {
	requestCount := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"count":  requestCount,
		})
	})

	backend := httptest.NewServer(handler)
	defer backend.Close()

	signalServer, _ := startMockSignalingServer(t)
	defer signalServer.Close()
	wsSignalURL := "ws" + strings.TrimPrefix(signalServer.URL, "http")

	nodeID := fmt.Sprintf("reconn-node-%d", time.Now().UnixNano())
	ag := agent.New(nodeID, wsSignalURL, "", backend.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ag.Start(ctx) }()
	defer ag.Close()

	time.Sleep(120 * time.Millisecond)

	// Helper to perform a single WebRTC connection, send GET, and verify 200 OK
	connectAndQuery := func(sessionTag string) error {
		clientWS, _, err := websocket.DefaultDialer.Dial(wsSignalURL, nil)
		if err != nil {
			return fmt.Errorf("dial error: %w", err)
		}
		defer clientWS.Close()

		_ = clientWS.WriteJSON(&signaling.Message{Type: signaling.TypeJoin, NodeID: nodeID})
		var statusMsg signaling.Message
		_ = clientWS.ReadJSON(&statusMsg)

		clientPC, err := webrtc.NewPeerConnection(webrtc.Configuration{
			ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
		})
		if err != nil {
			return fmt.Errorf("pc error: %w", err)
		}
		defer clientPC.Close()

		dc, err := clientPC.CreateDataChannel("p2pmux", &webrtc.DataChannelInit{Ordered: boolPtr(true)})
		if err != nil {
			return fmt.Errorf("dc error: %w", err)
		}

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

		offer, _ := clientPC.CreateOffer(nil)
		_ = clientPC.SetLocalDescription(offer)
		_ = clientWS.WriteJSON(&signaling.Message{Type: signaling.TypeOffer, NodeID: nodeID, SDP: &offer})

		dcOpenCh := make(chan struct{})
		dc.OnOpen(func() { close(dcOpenCh) })

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
		case <-dcOpenCh:
		case <-time.After(5 * time.Second):
			return fmt.Errorf("timeout waiting for DC open in %s", sessionTag)
		}

		clientMux := mux.NewMux(dc, true)
		defer clientMux.Close()
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			_ = clientMux.HandleIncomingMessage(msg.Data)
		})

		stream, err := clientMux.OpenStream()
		if err != nil {
			return fmt.Errorf("open stream error: %w", err)
		}
		defer stream.Close()

		reqMeta := proxy.RequestMetadata{Method: "GET", Path: "/api/check"}
		metaBytes, _ := json.Marshal(reqMeta)
		_ = stream.WriteHeaders(metaBytes)
		_ = stream.Close()

		readCtx, rCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer rCancel()
		respH, err := stream.ReadHeaders(readCtx)
		if err != nil {
			return fmt.Errorf("read headers error: %w", err)
		}

		var respMeta proxy.ResponseMetadata
		_ = json.Unmarshal(respH, &respMeta)
		if respMeta.Status != http.StatusOK {
			return fmt.Errorf("status was %d, expected 200", respMeta.Status)
		}

		body, _ := io.ReadAll(stream)
		if !strings.Contains(string(body), `"status":"ok"`) {
			return fmt.Errorf("body mismatch: %s", string(body))
		}
		return nil
	}

	// Session 1: First connection
	if err := connectAndQuery("Session-1"); err != nil {
		t.Fatalf("Session 1 failed: %v", err)
	}
	t.Log("Session 1 connected and completed request successfully!")

	// Brief pause to allow Session 1 to tear down
	time.Sleep(100 * time.Millisecond)

	// Session 2: Reconnection to the same running Agent daemon
	if err := connectAndQuery("Session-2"); err != nil {
		t.Fatalf("Session 2 reconnection failed: %v", err)
	}
	t.Log("Session 2 reconnected and completed request successfully!")

	if requestCount != 2 {
		t.Errorf("Expected 2 completed requests across 2 sessions, got %d", requestCount)
	}
}
