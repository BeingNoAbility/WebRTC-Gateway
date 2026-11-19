package test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

func TestE2E_FileServer_SingleFileAndRange(t *testing.T) {
	// 1. Setup Signal Server
	server, _ := startRealSignalServer("")
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	// 2. Prepare a 1MB random payload test file
	tempDir := t.TempDir()
	filePath := filepath.Join(tempDir, "ubuntu-disk.iso")
	payload := make([]byte, 1024*1024) // 1MB
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	expectedHashBytes := sha256.Sum256(payload)
	expectedHash := hex.EncodeToString(expectedHashBytes[:])

	if err := os.WriteFile(filePath, payload, 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	// 3. Launch Agent in Single File Mode
	nodeID := "p2p-iso-download"
	ag := agent.New(nodeID, wsURL, "", "iso-secret-pass", "")
	if err := ag.SetSingleFile(filePath); err != nil {
		t.Fatalf("SetSingleFile failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Start(ctx)
	defer ag.Close()

	time.Sleep(100 * time.Millisecond)

	// 4. Client connects via WebRTC DataChannel using HMAC-SHA256 signature
	clientWS, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Client dial WS failed: %v", err)
	}
	defer clientWS.Close()

	var clientWSMu sync.Mutex
	safeWriteWS := func(m *signaling.Message) error {
		clientWSMu.Lock()
		defer clientWSMu.Unlock()
		return clientWS.WriteJSON(m)
	}

	ansCh := make(chan signaling.Message, 5)
	go func() {
		for {
			var m signaling.Message
			if err := clientWS.ReadJSON(&m); err != nil {
				return
			}
			if m.Type == signaling.TypeAnswer {
				ansCh <- m
			}
		}
	}()

	_ = safeWriteWS(&signaling.Message{Type: signaling.TypeJoin, NodeID: nodeID})

	clientPC, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		t.Fatalf("NewPeerConnection failed: %v", err)
	}
	defer clientPC.Close()

	_, _, allOpenCh, createMux, err := setupClientDualMux(clientPC)
	if err != nil {
		t.Fatalf("setupClientDualMux failed: %v", err)
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

	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer failed: %v", err)
	}
	_ = clientPC.SetLocalDescription(offer)

	futureExp := time.Now().Add(2 * time.Hour).Unix()
	rawShareSign := agent.GenerateShareSignature(nodeID, futureExp, "iso-secret-pass")
	fp := agent.ExtractSDPFingerprint(offer.SDP)
	validSign := agent.ComputeHMAC(rawShareSign, fmt.Sprintf("%s:%d:%s", nodeID, futureExp, fp))

	_ = safeWriteWS(&signaling.Message{
		Type:   signaling.TypeOffer,
		NodeID: nodeID,
		SDP:    &offer,
		Exp:    futureExp,
		Sign:   validSign,
	})

	select {
	case ans := <-ansCh:
		_ = clientPC.SetRemoteDescription(*ans.SDP)
	case <-time.After(5 * time.Second):
		t.Fatalf("Timeout waiting for Answer")
	}

	select {
	case <-allOpenCh:
		t.Logf("WebRTC Dual DataChannels opened for single file download test!")
	case <-time.After(5 * time.Second):
		t.Fatalf("Timeout waiting for DataChannels to open")
	}

	clientMux := createMux()
	defer clientMux.Close()

	// 5. Test Full Download of 1MB ISO via DataChannel
	downloadStream, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream for download failed: %v", err)
	}

	reqMeta := proxy.RequestMetadata{
		Method: "GET",
		Path:   "/",
	}
	reqData, _ := json.Marshal(reqMeta)
	_ = downloadStream.WriteHeaders(reqData)
	_ = downloadStream.Close()

	ctxResp, cancelResp := context.WithTimeout(context.Background(), 5*time.Second)
	respHeadersRaw, err := downloadStream.ReadHeaders(ctxResp)
	cancelResp()
	if err != nil {
		t.Fatalf("ReadHeaders failed: %v", err)
	}

	var respMeta proxy.ResponseMetadata
	_ = json.Unmarshal(respHeadersRaw, &respMeta)
	if respMeta.Status != 200 {
		t.Fatalf("Expected HTTP 200, got: %d (%s)", respMeta.Status, respMeta.Msg)
	}
	if cd := respMeta.Headers["content-disposition"]; !strings.Contains(cd, "attachment") || !strings.Contains(cd, "ubuntu-disk.iso") {
		t.Errorf("Expected attachment Content-Disposition for .iso file, got: %s", cd)
	}

	hasher := sha256.New()
	downloadedBytes, err := io.Copy(hasher, downloadStream)
	if err != nil {
		t.Fatalf("Failed to read download stream: %v", err)
	}
	if downloadedBytes != int64(len(payload)) {
		t.Fatalf("Downloaded %d bytes, expected %d", downloadedBytes, len(payload))
	}
	actualHash := hex.EncodeToString(hasher.Sum(nil))
	if actualHash != expectedHash {
		t.Fatalf("Checksum mismatch! Expected %s, got %s", expectedHash, actualHash)
	}
	t.Logf("Successfully downloaded 1MB single file with exact SHA256 match: %s", actualHash)

	// 6. Test Range 206 Partial Content (bytes 500-1499)
	rangeStream, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream for range failed: %v", err)
	}

	rangeReq := proxy.RequestMetadata{
		Method: "GET",
		Path:   "/ubuntu-disk.iso",
		Headers: map[string]string{
			"range": "bytes=500-1499",
		},
	}
	rangeData, _ := json.Marshal(rangeReq)
	_ = rangeStream.WriteHeaders(rangeData)
	_ = rangeStream.Close()

	ctxRange, cancelRange := context.WithTimeout(context.Background(), 3*time.Second)
	rangeRespRaw, err := rangeStream.ReadHeaders(ctxRange)
	cancelRange()
	if err != nil {
		t.Fatalf("ReadHeaders for range failed: %v", err)
	}

	var rangeRespMeta proxy.ResponseMetadata
	_ = json.Unmarshal(rangeRespRaw, &rangeRespMeta)
	if rangeRespMeta.Status != 206 {
		t.Fatalf("Expected HTTP 206 Partial Content, got: %d", rangeRespMeta.Status)
	}
	if cr := rangeRespMeta.Headers["content-range"]; !strings.HasPrefix(cr, "bytes 500-1499/") {
		t.Errorf("Expected Content-Range bytes 500-1499/..., got: %s", cr)
	}

	sliceBuf, err := io.ReadAll(rangeStream)
	if err != nil {
		t.Fatalf("ReadAll range slice failed: %v", err)
	}
	expectedSliceLen := 1499 - 500 + 1
	if len(sliceBuf) != expectedSliceLen {
		t.Fatalf("Expected slice length %d, got %d", expectedSliceLen, len(sliceBuf))
	}
	for i, b := range sliceBuf {
		if b != payload[500+i] {
			t.Fatalf("Byte mismatch at offset %d: expected %d, got %d", i, payload[500+i], b)
		}
	}
	t.Logf("HTTP 206 Partial Content Range slice verified byte-for-byte over WebRTC!")
}

func TestE2E_FileServer_DirectoryMode(t *testing.T) {
	// 1. Setup Signal Server
	server, _ := startRealSignalServer("")
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	// 2. Setup directory tree
	tempDir := t.TempDir()
	indexHTML := "<html><body><h1>WebRTC Static Docs</h1></body></html>"
	_ = os.WriteFile(filepath.Join(tempDir, "index.html"), []byte(indexHTML), 0644)
	subDir := filepath.Join(tempDir, "assets")
	_ = os.Mkdir(subDir, 0755)
	_ = os.WriteFile(filepath.Join(subDir, "style.css"), []byte("body { background: black; }"), 0644)

	// 3. Launch Agent in Directory Mode (Public / No secret)
	nodeID := "p2p-docs-preview"
	ag := agent.New(nodeID, wsURL, "", "", "")
	if err := ag.SetDirectory(tempDir); err != nil {
		t.Fatalf("SetDirectory failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Start(ctx)
	defer ag.Close()

	time.Sleep(100 * time.Millisecond)

	// 4. Client connects via WebRTC DataChannel (public access)
	clientWS, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Client dial WS failed: %v", err)
	}
	defer clientWS.Close()

	var clientWSMu sync.Mutex
	safeWriteWS := func(m *signaling.Message) error {
		clientWSMu.Lock()
		defer clientWSMu.Unlock()
		return clientWS.WriteJSON(m)
	}

	ansCh := make(chan signaling.Message, 5)
	go func() {
		for {
			var m signaling.Message
			if err := clientWS.ReadJSON(&m); err != nil {
				return
			}
			if m.Type == signaling.TypeAnswer {
				ansCh <- m
			}
		}
	}()

	_ = safeWriteWS(&signaling.Message{Type: signaling.TypeJoin, NodeID: nodeID})

	clientPC, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		t.Fatalf("NewPeerConnection failed: %v", err)
	}
	defer clientPC.Close()

	_, _, allOpenCh, createMux, err := setupClientDualMux(clientPC)
	if err != nil {
		t.Fatalf("setupClientDualMux failed: %v", err)
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

	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer failed: %v", err)
	}
	_ = clientPC.SetLocalDescription(offer)

	_ = safeWriteWS(&signaling.Message{
		Type:   signaling.TypeOffer,
		NodeID: nodeID,
		SDP:    &offer,
	})

	select {
	case ans := <-ansCh:
		_ = clientPC.SetRemoteDescription(*ans.SDP)
	case <-time.After(5 * time.Second):
		t.Fatalf("Timeout waiting for Answer")
	}

	select {
	case <-allOpenCh:
		t.Logf("Dual DataChannels opened for directory server test!")
	case <-time.After(5 * time.Second):
		t.Fatalf("Timeout waiting for DataChannels to open")
	}

	clientMux := createMux()
	defer clientMux.Close()

	// 5. Test fetching index.html via "/"
	s1, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}
	reqData, _ := json.Marshal(proxy.RequestMetadata{Method: "GET", Path: "/"})
	_ = s1.WriteHeaders(reqData)
	_ = s1.Close()

	ctx1, cancel1 := context.WithTimeout(context.Background(), 3*time.Second)
	h1Raw, err := s1.ReadHeaders(ctx1)
	cancel1()
	if err != nil {
		t.Fatalf("ReadHeaders failed: %v", err)
	}
	var meta1 proxy.ResponseMetadata
	_ = json.Unmarshal(h1Raw, &meta1)
	if meta1.Status != 200 {
		t.Fatalf("Expected 200 for index.html, got: %d", meta1.Status)
	}
	body1, _ := io.ReadAll(s1)
	if string(body1) != indexHTML {
		t.Fatalf("Expected index.html content %q, got %q", indexHTML, string(body1))
	}
	t.Logf("Directory mode index.html served correctly!")

	// 6. Test fetching "/assets/style.css"
	s2, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}
	reqData2, _ := json.Marshal(proxy.RequestMetadata{Method: "GET", Path: "/assets/style.css"})
	_ = s2.WriteHeaders(reqData2)
	_ = s2.Close()

	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	h2Raw, err := s2.ReadHeaders(ctx2)
	cancel2()
	if err != nil {
		t.Fatalf("ReadHeaders failed: %v", err)
	}
	var meta2 proxy.ResponseMetadata
	_ = json.Unmarshal(h2Raw, &meta2)
	if meta2.Status != 200 {
		t.Fatalf("Expected 200 for style.css, got: %d", meta2.Status)
	}
	body2, _ := io.ReadAll(s2)
	if string(body2) != "body { background: black; }" {
		t.Fatalf("Expected css content, got %q", string(body2))
	}
	t.Logf("Subdirectory asset /assets/style.css served correctly!")
}

func TestE2E_FileServer_SpecialCharactersAndMediaMIME(t *testing.T) {
	// 1. Setup Signal Server
	server, _ := startRealSignalServer("")
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	// 2. Prepare directory with anime video and pdf
	tempDir := t.TempDir()
	specialAnimeName := "[ANi] FX 戰士久留美 - 01 [1080P][Baha][WEB-DL][AAC AVC][CHT].mp4"
	animePath := filepath.Join(tempDir, specialAnimeName)
	animePayload := []byte("anime-mp4-video-stream-payload-data")
	if err := os.WriteFile(animePath, animePayload, 0644); err != nil {
		t.Fatalf("Write anime file failed: %v", err)
	}

	pdfName := "user_guide.pdf"
	pdfPath := filepath.Join(tempDir, pdfName)
	pdfPayload := []byte("%PDF-1.7 mock document stream")
	if err := os.WriteFile(pdfPath, pdfPayload, 0644); err != nil {
		t.Fatalf("Write pdf file failed: %v", err)
	}

	// 3. Launch Agent in Directory Mode
	nodeID := "p2p-anime-vault"
	ag := agent.New(nodeID, wsURL, "", "", "")
	if err := ag.SetDirectory(tempDir); err != nil {
		t.Fatalf("SetDirectory failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Start(ctx)
	defer ag.Close()

	time.Sleep(100 * time.Millisecond)

	// 4. Client connects via WebRTC DataChannel
	clientWS, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Client dial WS failed: %v", err)
	}
	defer clientWS.Close()

	var clientWSMu sync.Mutex
	safeWriteWS := func(m *signaling.Message) error {
		clientWSMu.Lock()
		defer clientWSMu.Unlock()
		return clientWS.WriteJSON(m)
	}

	ansCh := make(chan signaling.Message, 5)
	go func() {
		for {
			var m signaling.Message
			if err := clientWS.ReadJSON(&m); err != nil {
				return
			}
			if m.Type == signaling.TypeAnswer {
				ansCh <- m
			}
		}
	}()

	_ = safeWriteWS(&signaling.Message{Type: signaling.TypeJoin, NodeID: nodeID})

	clientPC, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		t.Fatalf("NewPeerConnection failed: %v", err)
	}
	defer clientPC.Close()

	_, _, allOpenCh, createMux, err := setupClientDualMux(clientPC)
	if err != nil {
		t.Fatalf("setupClientDualMux failed: %v", err)
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

	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer failed: %v", err)
	}
	_ = clientPC.SetLocalDescription(offer)

	_ = safeWriteWS(&signaling.Message{
		Type:   signaling.TypeOffer,
		NodeID: nodeID,
		SDP:    &offer,
	})

	select {
	case ans := <-ansCh:
		_ = clientPC.SetRemoteDescription(*ans.SDP)
	case <-time.After(5 * time.Second):
		t.Fatalf("Timeout waiting for Answer")
	}

	select {
	case <-allOpenCh:
		t.Logf("Dual DataChannels opened for anime special char test!")
	case <-time.After(5 * time.Second):
		t.Fatalf("Timeout waiting for DataChannels to open")
	}

	clientMux := createMux()
	defer clientMux.Close()

	// 5. Test fetching special anime filename via percent-encoded path
	s1, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}
	encodedPath := "/%5BANi%5D%20FX%20%E6%88%B0%E5%A3%AB%E4%B9%85%E7%95%99%E7%BE%8E%20-%2001%20%5B1080P%5D%5BBaha%5D%5BWEB-DL%5D%5BAAC%20AVC%5D%5BCHT%5D.mp4"
	reqData, _ := json.Marshal(proxy.RequestMetadata{Method: "GET", Path: encodedPath})
	_ = s1.WriteHeaders(reqData)
	_ = s1.Close()

	ctx1, cancel1 := context.WithTimeout(context.Background(), 3*time.Second)
	h1Raw, err := s1.ReadHeaders(ctx1)
	cancel1()
	if err != nil {
		t.Fatalf("ReadHeaders failed: %v", err)
	}
	var meta1 proxy.ResponseMetadata
	_ = json.Unmarshal(h1Raw, &meta1)
	if meta1.Status != 200 {
		t.Fatalf("Expected 200 for anime episode, got: %d", meta1.Status)
	}
	if !strings.HasPrefix(meta1.Headers["content-type"], "video/mp4") {
		t.Errorf("Expected content-type video/mp4, got %q", meta1.Headers["content-type"])
	}
	if meta1.Headers["x-p2p-mode"] != "dir" {
		t.Errorf("Expected x-p2p-mode dir, got %q", meta1.Headers["x-p2p-mode"])
	}
	if meta1.Headers["x-p2p-file-name"] != specialAnimeName {
		t.Errorf("Expected x-p2p-file-name %q, got %q", specialAnimeName, meta1.Headers["x-p2p-file-name"])
	}
	body1, _ := io.ReadAll(s1)
	if string(body1) != string(animePayload) {
		t.Fatalf("Anime payload mismatch!")
	}
	t.Logf("✓ Special anime filename with Chinese, spaces, brackets fetched with 200 OK and video/mp4 MIME!")

	// 6. Test fetching PDF file
	s2, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}
	reqData2, _ := json.Marshal(proxy.RequestMetadata{Method: "GET", Path: "/" + pdfName})
	_ = s2.WriteHeaders(reqData2)
	_ = s2.Close()

	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	h2Raw, err := s2.ReadHeaders(ctx2)
	cancel2()
	if err != nil {
		t.Fatalf("ReadHeaders failed: %v", err)
	}
	var meta2 proxy.ResponseMetadata
	_ = json.Unmarshal(h2Raw, &meta2)
	if meta2.Status != 200 {
		t.Fatalf("Expected 200 for pdf, got: %d", meta2.Status)
	}
	if meta2.Headers["content-type"] != "application/pdf" {
		t.Errorf("Expected content-type application/pdf, got %q", meta2.Headers["content-type"])
	}
	if meta2.Headers["x-p2p-file-name"] != pdfName {
		t.Errorf("Expected x-p2p-file-name %q, got %q", pdfName, meta2.Headers["x-p2p-file-name"])
	}
	body2, _ := io.ReadAll(s2)
	if string(body2) != string(pdfPayload) {
		t.Fatalf("PDF payload mismatch!")
	}
	t.Logf("✓ PDF document fetched with 200 OK and application/pdf MIME!")

	// 7. Verify Path Traversal defense over real WebRTC DataChannel
	s3, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}
	reqData3, _ := json.Marshal(proxy.RequestMetadata{Method: "GET", Path: "/%2e%2e/%2e%2e/etc/passwd"})
	_ = s3.WriteHeaders(reqData3)
	_ = s3.Close()

	ctx3, cancel3 := context.WithTimeout(context.Background(), 3*time.Second)
	h3Raw, err := s3.ReadHeaders(ctx3)
	cancel3()
	if err != nil {
		t.Fatalf("ReadHeaders failed: %v", err)
	}
	var meta3 proxy.ResponseMetadata
	_ = json.Unmarshal(h3Raw, &meta3)
	if meta3.Status == 200 {
		t.Fatalf("Path traversal attack must NOT return 200 OK! Got: %d", meta3.Status)
	}
	t.Logf("✓ Path traversal attack over WebRTC DataChannel correctly rejected with status %d!", meta3.Status)
}
