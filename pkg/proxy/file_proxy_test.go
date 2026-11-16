package proxy

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"webrtc-gateway/pkg/mux"
)

func TestFileProxy_ConstructorsAndValidation(t *testing.T) {
	tempDir := t.TempDir()

	testFile := filepath.Join(tempDir, "sample.iso")
	if err := os.WriteFile(testFile, []byte("fake-iso-content-12345"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// 1. Valid Single File Proxy
	fpFile, err := NewSingleFileProxy(testFile)
	if err != nil {
		t.Fatalf("NewSingleFileProxy failed: %v", err)
	}
	if fpFile.Mode() != FileModeSingleFile {
		t.Errorf("Expected FileModeSingleFile, got %v", fpFile.Mode())
	}
	if fpFile.FileName() != "sample.iso" {
		t.Errorf("Expected FileName sample.iso, got %q", fpFile.FileName())
	}
	if fpFile.FileSize() != int64(len("fake-iso-content-12345")) {
		t.Errorf("Expected FileSize %d, got %d", len("fake-iso-content-12345"), fpFile.FileSize())
	}
	if fpFile.isInline {
		t.Errorf("Expected .iso to not be inline")
	}

	// 2. Reject Dir passed to NewSingleFileProxy
	_, err = NewSingleFileProxy(tempDir)
	if err == nil {
		t.Fatalf("Expected error when passing dir to NewSingleFileProxy, got nil")
	}

	// 3. Reject non-existent file
	_, err = NewSingleFileProxy(filepath.Join(tempDir, "non_existent.bin"))
	if err == nil {
		t.Fatalf("Expected error for non-existent file, got nil")
	}

	// 4. Valid Dir Proxy
	fpDir, err := NewDirProxy(tempDir)
	if err != nil {
		t.Fatalf("NewDirProxy failed: %v", err)
	}
	if fpDir.Mode() != FileModeDirectory {
		t.Errorf("Expected FileModeDirectory, got %v", fpDir.Mode())
	}

	// 5. Reject File passed to NewDirProxy
	_, err = NewDirProxy(testFile)
	if err == nil {
		t.Fatalf("Expected error when passing file to NewDirProxy, got nil")
	}

	// 6. Test media inline vs attachment
	videoFile := filepath.Join(tempDir, "video.mp4")
	_ = os.WriteFile(videoFile, []byte("video-data"), 0644)
	fpVideo, err := NewSingleFileProxy(videoFile)
	if err != nil {
		t.Fatalf("NewSingleFileProxy video failed: %v", err)
	}
	if !fpVideo.isInline {
		t.Errorf("Expected .mp4 to be inline, got false")
	}
}

func TestFileProxy_SingleFile_AttachmentAndRange(t *testing.T) {
	tempDir := t.TempDir()
	archivePath := filepath.Join(tempDir, "archive.zip")
	fullPayload := "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	if err := os.WriteFile(archivePath, []byte(fullPayload), 0644); err != nil {
		t.Fatalf("WriteFile archive.zip failed: %v", err)
	}

	fp, err := NewSingleFileProxy(archivePath)
	if err != nil {
		t.Fatalf("NewSingleFileProxy failed: %v", err)
	}

	// Create memory Mux pair
	tClient, tAgent := newMemoryTransportPair()
	defer tClient.Close()
	defer tAgent.Close()

	clientMux := mux.NewMux(tClient, true)
	agentMux := mux.NewMux(tAgent, false)

	tClient.mu.Lock()
	tClient.onMsg = func(d []byte) { _ = clientMux.HandleIncomingMessage(d) }
	tClient.mu.Unlock()

	tAgent.mu.Lock()
	tAgent.onMsg = func(d []byte) { _ = agentMux.HandleIncomingMessage(d) }
	tAgent.mu.Unlock()

	defer clientMux.Close()
	defer agentMux.Close()

	// 1. Full Download Test (GET /)
	go func() {
		stream, err := agentMux.AcceptStream(context.Background())
		if err != nil {
			return
		}
		fp.HandleStream("sess-1", stream)
	}()

	clientStream, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}

	reqMeta := RequestMetadata{
		Method: "GET",
		Path:   "/",
	}
	reqData, _ := json.Marshal(reqMeta)
	if err := clientStream.WriteHeaders(reqData); err != nil {
		t.Fatalf("WriteHeaders failed: %v", err)
	}
	_ = clientStream.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	respHeadersRaw, err := clientStream.ReadHeaders(ctx)
	cancel()
	if err != nil {
		t.Fatalf("ReadHeaders failed: %v", err)
	}

	var respMeta ResponseMetadata
	_ = json.Unmarshal(respHeadersRaw, &respMeta)
	if respMeta.Status != 200 {
		t.Fatalf("Expected HTTP 200, got %d", respMeta.Status)
	}
	if cd := respMeta.Headers["content-disposition"]; !strings.Contains(cd, "attachment") || !strings.Contains(cd, "archive.zip") {
		t.Errorf("Expected attachment content-disposition, got: %s", cd)
	}

	body, err := io.ReadAll(clientStream)
	if err != nil {
		t.Fatalf("ReadAll body failed: %v", err)
	}
	if string(body) != fullPayload {
		t.Fatalf("Expected payload %q, got %q", fullPayload, string(body))
	}

	// 2. HTTP 206 Partial Content (Range: bytes=10-25)
	go func() {
		stream, err := agentMux.AcceptStream(context.Background())
		if err != nil {
			return
		}
		fp.HandleStream("sess-1", stream)
	}()

	rangeStream, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream for range failed: %v", err)
	}

	rangeReq := RequestMetadata{
		Method: "GET",
		Path:   "/any/sub/path/ignored/in/single/file/mode",
		Headers: map[string]string{
			"range": "bytes=10-25",
		},
	}
	rangeData, _ := json.Marshal(rangeReq)
	_ = rangeStream.WriteHeaders(rangeData)
	_ = rangeStream.Close()

	ctxRange, cancelRange := context.WithTimeout(context.Background(), 3*time.Second)
	rangeRespRaw, err := rangeStream.ReadHeaders(ctxRange)
	cancelRange()
	if err != nil {
		t.Fatalf("ReadHeaders range failed: %v", err)
	}

	var rangeRespMeta ResponseMetadata
	_ = json.Unmarshal(rangeRespRaw, &rangeRespMeta)
	if rangeRespMeta.Status != 206 {
		t.Fatalf("Expected HTTP 206 Partial Content, got %d", rangeRespMeta.Status)
	}
	if cr := rangeRespMeta.Headers["content-range"]; !strings.HasPrefix(cr, "bytes 10-25/") {
		t.Errorf("Expected Content-Range bytes 10-25/..., got: %s", cr)
	}

	rangeBody, err := io.ReadAll(rangeStream)
	if err != nil {
		t.Fatalf("ReadAll range body failed: %v", err)
	}
	expectedSlice := fullPayload[10 : 25+1]
	if string(rangeBody) != expectedSlice {
		t.Fatalf("Expected range slice %q, got %q", expectedSlice, string(rangeBody))
	}

	// 3. Strict Read-Only: Reject POST with 405 Method Not Allowed
	go func() {
		stream, err := agentMux.AcceptStream(context.Background())
		if err != nil {
			return
		}
		fp.HandleStream("sess-1", stream)
	}()

	postStream, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream for post failed: %v", err)
	}
	postReq := RequestMetadata{
		Method: "POST",
		Path:   "/",
	}
	postData, _ := json.Marshal(postReq)
	_ = postStream.WriteHeaders(postData)
	_ = postStream.Close()

	ctxPost, cancelPost := context.WithTimeout(context.Background(), 3*time.Second)
	postRespRaw, err := postStream.ReadHeaders(ctxPost)
	cancelPost()
	if err != nil {
		t.Fatalf("ReadHeaders post failed: %v", err)
	}

	var postRespMeta ResponseMetadata
	_ = json.Unmarshal(postRespRaw, &postRespMeta)
	if postRespMeta.Status != 405 {
		t.Fatalf("Expected HTTP 405 Method Not Allowed, got %d", postRespMeta.Status)
	}
}

func TestFileProxy_DirectoryMode_AndPathTraversalDefense(t *testing.T) {
	tempDir := t.TempDir()

	// Setup structure:
	//   tempDir/
	//     index.html
	//     sub/
	//       doc.txt
	_ = os.WriteFile(filepath.Join(tempDir, "index.html"), []byte("<h1>Welcome Home</h1>"), 0644)
	subDir := filepath.Join(tempDir, "sub")
	_ = os.Mkdir(subDir, 0755)
	_ = os.WriteFile(filepath.Join(subDir, "doc.txt"), []byte("Subfolder secret content"), 0644)

	fpDir, err := NewDirProxy(tempDir)
	if err != nil {
		t.Fatalf("NewDirProxy failed: %v", err)
	}

	tClient, tAgent := newMemoryTransportPair()
	defer tClient.Close()
	defer tAgent.Close()

	clientMux := mux.NewMux(tClient, true)
	agentMux := mux.NewMux(tAgent, false)

	tClient.mu.Lock()
	tClient.onMsg = func(d []byte) { _ = clientMux.HandleIncomingMessage(d) }
	tClient.mu.Unlock()

	tAgent.mu.Lock()
	tAgent.onMsg = func(d []byte) { _ = agentMux.HandleIncomingMessage(d) }
	tAgent.mu.Unlock()

	defer clientMux.Close()
	defer agentMux.Close()

	// 1. Fetch index.html on root "/"
	go func() {
		stream, err := agentMux.AcceptStream(context.Background())
		if err != nil {
			return
		}
		fpDir.HandleStream("sess-dir", stream)
	}()

	s1, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}
	r1, _ := json.Marshal(RequestMetadata{Method: "GET", Path: "/"})
	_ = s1.WriteHeaders(r1)
	_ = s1.Close()

	ctx1, cancel1 := context.WithTimeout(context.Background(), 3*time.Second)
	h1Raw, err := s1.ReadHeaders(ctx1)
	cancel1()
	if err != nil {
		t.Fatalf("ReadHeaders failed: %v", err)
	}
	var meta1 ResponseMetadata
	_ = json.Unmarshal(h1Raw, &meta1)
	if meta1.Status != 200 {
		t.Fatalf("Expected 200 for index.html, got %d", meta1.Status)
	}
	body1, _ := io.ReadAll(s1)
	if string(body1) != "<h1>Welcome Home</h1>" {
		t.Fatalf("Expected index.html content, got %q", string(body1))
	}

	// 2. Fetch subdirectory file "/sub/doc.txt"
	go func() {
		stream, err := agentMux.AcceptStream(context.Background())
		if err != nil {
			return
		}
		fpDir.HandleStream("sess-dir", stream)
	}()

	s2, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}
	r2, _ := json.Marshal(RequestMetadata{Method: "GET", Path: "/sub/doc.txt"})
	_ = s2.WriteHeaders(r2)
	_ = s2.Close()

	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	h2Raw, err := s2.ReadHeaders(ctx2)
	cancel2()
	if err != nil {
		t.Fatalf("ReadHeaders failed: %v", err)
	}
	var meta2 ResponseMetadata
	_ = json.Unmarshal(h2Raw, &meta2)
	if meta2.Status != 200 {
		t.Fatalf("Expected 200 for sub/doc.txt, got %d", meta2.Status)
	}
	body2, _ := io.ReadAll(s2)
	if string(body2) != "Subfolder secret content" {
		t.Fatalf("Expected subfolder content, got %q", string(body2))
	}

	// 3. Path Traversal Attack Defense: "/../../etc/passwd" -> must be rejected (404 or 403)
	go func() {
		stream, err := agentMux.AcceptStream(context.Background())
		if err != nil {
			return
		}
		fpDir.HandleStream("sess-dir", stream)
	}()

	s3, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}
	r3, _ := json.Marshal(RequestMetadata{Method: "GET", Path: "/../../etc/passwd"})
	_ = s3.WriteHeaders(r3)
	_ = s3.Close()

	ctx3, cancel3 := context.WithTimeout(context.Background(), 3*time.Second)
	h3Raw, err := s3.ReadHeaders(ctx3)
	cancel3()
	if err != nil {
		t.Fatalf("ReadHeaders failed: %v", err)
	}
	var meta3 ResponseMetadata
	_ = json.Unmarshal(h3Raw, &meta3)
	// path.Clean("/../../etc/passwd") becomes "/etc/passwd", which doesn't exist in tempDir -> 404
	if meta3.Status == 200 {
		t.Fatalf("Path traversal attack must NOT succeed with 200! Got: %d", meta3.Status)
	}
}
