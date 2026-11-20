package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"webrtc-gateway/pkg/mux"
)

// 1. Comprehensive PSL & eTLD+1 boundary tests covering recent commit c4cbea0
func TestExtractRootDomain_TargetedEdgeCases(t *testing.T) {
	testCases := []struct {
		input    string
		expected string
	}{
		// Standard domains
		{"example.com", "example.com"},
		{"www.example.com", "example.com"},
		{"sub.deep.example.com:8443", "example.com"},
		{"EXAMPLE.COM", "example.com"},
		{"Www.Example.Com:443", "example.com"},

		// Complex ccTLDs & academic institutions
		{"oxford.ac.uk", "oxford.ac.uk"},
		{"cs.oxford.ac.uk", "oxford.ac.uk"},
		{"cs.oxford.ac.uk:8443", "oxford.ac.uk"},
		{"tokyo.ac.jp", "tokyo.ac.jp"},
		{"metro.tokyo.jp", "metro.tokyo.jp"}, // in PSL, tokyo.jp is a geographic public suffix
		{"sydney.edu.au", "sydney.edu.au"},
		{"eng.sydney.edu.au:9000", "sydney.edu.au"},

		// Multi-tenant cloud providers (PSL Private Domains)
		{"my-app.github.io", "my-app.github.io"},
		{"sub.my-app.github.io", "my-app.github.io"},
		{"portal.pages.dev", "portal.pages.dev"},
		{"sub.portal.pages.dev:8443", "portal.pages.dev"},
		{"my-worker.workers.dev", "my-worker.workers.dev"},
		{"my-bucket.s3.amazonaws.com", "my-bucket.s3.amazonaws.com"},

		// Bare public suffixes MUST return empty string to prevent cross-tenant traversal
		{"github.io", ""},
		{"pages.dev", ""},
		{"workers.dev", ""},
		{"s3.amazonaws.com", ""},
		{"co.uk", ""},
		{"ac.uk", ""},
		{"com.cn", ""},
		{"tokyo.jp", ""},

		// IPv4 addresses (with and without ports)
		{"127.0.0.1", "127.0.0.1"},
		{"127.0.0.1:8080", "127.0.0.1"},
		{"192.168.1.100", "192.168.1.100"},
		{"10.0.0.1:443", "10.0.0.1"},

		// IPv6 addresses (bare, bracketed, with ports)
		{"::1", "::1"},
		{"[::1]", "::1"},
		{"[::1]:8080", "::1"},
		{"2001:db8::1", "2001:db8::1"},
		{"[2001:db8::1]", "2001:db8::1"},
		{"[2001:db8::1]:443", "2001:db8::1"},

		// Single-label / Intranet hostnames
		{"localhost", "localhost"},
		{"localhost:3000", "localhost"},
		{"nas", "nas"},
		{"nas:5000", "nas"},
		{"homeserver", "homeserver"},
	}

	for _, tc := range testCases {
		actual := ExtractRootDomain(tc.input)
		if actual != tc.expected {
			t.Errorf("ExtractRootDomain(%q) = %q, expected %q", tc.input, actual, tc.expected)
		}
	}
}

// 2. WebSocket Proxy cross-tenant isolation and bare suffix defense
func TestWSProxy_PSLIsolation_Targeted(t *testing.T) {
	cm := NewSessionCookieManager()

	// Scenario A: Target is a multi-tenant Cloudflare Pages site
	wpPages, err := NewWSProxy("https://my-tenant.pages.dev:8443", cm)
	if err != nil {
		t.Fatalf("NewWSProxy failed: %v", err)
	}
	if wpPages.rootDomain != "my-tenant.pages.dev" {
		t.Fatalf("expected rootDomain 'my-tenant.pages.dev', got %q", wpPages.rootDomain)
	}
	// Exact and subdomains under this tenant MUST BE ALLOWED
	if !wpPages.isDomainAllowed("my-tenant.pages.dev") {
		t.Errorf("expected my-tenant.pages.dev to be allowed")
	}
	if !wpPages.isDomainAllowed("sub.my-tenant.pages.dev") {
		t.Errorf("expected sub.my-tenant.pages.dev to be allowed")
	}
	// Sibling tenant attacker.pages.dev MUST BE FORBIDDEN
	if wpPages.isDomainAllowed("attacker.pages.dev") {
		t.Errorf("CRITICAL SECURITY: attacker.pages.dev allowed under my-tenant.pages.dev target!")
	}
	if wpPages.isDomainAllowed("pages.dev") {
		t.Errorf("CRITICAL SECURITY: parent bare suffix pages.dev allowed under tenant target!")
	}

	// Scenario B: Target is bare public suffix https://pages.dev
	wpBare, err := NewWSProxy("https://pages.dev", cm)
	if err != nil {
		t.Fatalf("NewWSProxy failed: %v", err)
	}
	if wpBare.rootDomain != "" {
		t.Fatalf("expected empty rootDomain for bare suffix pages.dev, got %q", wpBare.rootDomain)
	}
	// Exact target host allowed
	if !wpBare.isDomainAllowed("pages.dev") {
		t.Errorf("expected exact host pages.dev to be allowed")
	}
	// Third-party tenant MUST BE FORBIDDEN
	if wpBare.isDomainAllowed("any-tenant.pages.dev") {
		t.Errorf("CRITICAL SECURITY: any-tenant.pages.dev allowed under bare pages.dev target!")
	}
}

// 3. CreateSafeDialContext dial retry & SSRF defense tests covering commit f79edb4
func TestCreateSafeDialContext_RetryLogic(t *testing.T) {
	// Subtest 1: Transient network drop on target host succeeds on retry
	t.Run("TargetDialRetry_SucceedsOnSecondAttempt", func(t *testing.T) {
		// Reserve a random local port
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("net.Listen failed: %v", err)
		}
		targetAddr := l.Addr().String()
		_ = l.Close() // Close immediately so initial dial fails with connection refused

		targetURL, _ := url.Parse("http://" + targetAddr)
		dialContext := CreateSafeDialContext(targetURL)

		var listenMu sync.Mutex
		var activeListener net.Listener

		// Start listening after 10ms (so attempt 0 fails, attempt 1 succeeds)
		go func() {
			time.Sleep(10 * time.Millisecond)
			nl, err := net.Listen("tcp", targetAddr)
			if err == nil {
				listenMu.Lock()
				activeListener = nl
				listenMu.Unlock()
				go func() {
					for {
						c, err := nl.Accept()
						if err != nil {
							return
						}
						_ = c.Close()
					}
				}()
			}
		}()

		defer func() {
			listenMu.Lock()
			if activeListener != nil {
				_ = activeListener.Close()
			}
			listenMu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		conn, err := dialContext(ctx, "tcp", targetAddr)
		if err != nil {
			t.Fatalf("Expected dial retry to succeed on 2nd attempt, got error: %v", err)
		}
		if conn != nil {
			_ = conn.Close()
		}
	})

	// Subtest 2: Server down completely exhausts 3 attempts and returns error
	t.Run("TargetDialRetry_ExhaustsAttempts", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("net.Listen failed: %v", err)
		}
		deadAddr := l.Addr().String()
		_ = l.Close() // Keep closed

		targetURL, _ := url.Parse("http://" + deadAddr)
		dialContext := CreateSafeDialContext(targetURL)

		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()

		conn, err := dialContext(ctx, "tcp", deadAddr)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("Expected connection refused error after 3 attempts, got nil")
		}
	})

	// Subtest 3: Context cancellation aborts retry sleep immediately
	t.Run("TargetDialRetry_ContextCancelled", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("net.Listen failed: %v", err)
		}
		deadAddr := l.Addr().String()
		_ = l.Close()

		targetURL, _ := url.Parse("http://" + deadAddr)
		dialContext := CreateSafeDialContext(targetURL)

		ctx, cancel := context.WithCancel(context.Background())
		// Cancel almost immediately
		go func() {
			time.Sleep(3 * time.Millisecond)
			cancel()
		}()

		start := time.Now()
		_, err = dialContext(ctx, "tcp", deadAddr)
		elapsed := time.Since(start)

		if err == nil {
			t.Fatalf("Expected error on cancelled context")
		}
		if elapsed > 100*time.Millisecond {
			t.Errorf("Expected early abort on context cancellation, took %v", elapsed)
		}
	})

	// Subtest 4: Non-target SSRF attempts to private networks are blocked
	t.Run("NonTargetSSRF_Blocked", func(t *testing.T) {
		targetURL, _ := url.Parse("https://public-service.com")
		dialContext := CreateSafeDialContext(targetURL)

		ctx := context.Background()

		// Private IPv4 dial attempt
		_, err := dialContext(ctx, "tcp", "10.0.0.1:80")
		if err == nil || !strings.Contains(err.Error(), "SSRF blocked") {
			t.Errorf("Expected SSRF block for 10.0.0.1, got: %v", err)
		}

		// Cloud metadata IP dial attempt
		_, err = dialContext(ctx, "tcp", "169.254.169.254:80")
		if err == nil || !strings.Contains(err.Error(), "SSRF blocked") {
			t.Errorf("Expected SSRF block for 169.254.169.254, got: %v", err)
		}

		// Loopback dial attempt to non-target
		_, err = dialContext(ctx, "tcp", "127.0.0.1:8080")
		if err == nil || !strings.Contains(err.Error(), "SSRF blocked") {
			t.Errorf("Expected SSRF block for loopback 127.0.0.1:8080, got: %v", err)
		}
	})
}

// 3. Test upload body length header restoration (RFC 7230 / Nginx 413 mitigation via x-p2p-body-length)
func TestHTTPProxy_UploadBodyLengthRestoration(t *testing.T) {
	var mu sync.Mutex
	var receivedContentLength int64
	var receivedHeaderCL string
	var receivedChunked bool
	var receivedP2PLengthHeader string
	var receivedBody []byte

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		receivedContentLength = r.ContentLength
		receivedHeaderCL = r.Header.Get("Content-Length")
		for _, te := range r.TransferEncoding {
			if strings.EqualFold(te, "chunked") {
				receivedChunked = true
			}
		}
		receivedP2PLengthHeader = r.Header.Get("x-p2p-body-length")
		receivedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	cm := NewSessionCookieManager()
	hp, err := NewHTTPProxy(backend.URL, cm)
	if err != nil {
		t.Fatalf("NewHTTPProxy failed: %v", err)
	}

	tClient, tAgent := newMemoryTransportPair()
	defer tClient.Close()
	defer tAgent.Close()

	clientMux := mux.NewMux(tClient, true)
	agentMux := mux.NewMux(tAgent, false)
	defer clientMux.Close()
	defer agentMux.Close()

	tClient.mu.Lock()
	tClient.onMsg = func(d []byte) { _ = clientMux.HandleIncomingMessage(d) }
	tClient.mu.Unlock()

	tAgent.mu.Lock()
	tAgent.onMsg = func(d []byte) { _ = agentMux.HandleIncomingMessage(d) }
	tAgent.mu.Unlock()

	go func() {
		stream, err := agentMux.AcceptStream(context.Background())
		if err != nil {
			return
		}
		hp.HandleStream("test-session-upload", "127.0.0.1", stream)
	}()

	clientStream, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}

	payload := []byte("upload-test-payload-1234567890")
	expectedLen := int64(len(payload))

	reqMeta := RequestMetadata{
		Method: "POST",
		Path:   "/upload",
		Headers: map[string]string{
			"x-p2p-body-length": fmt.Sprintf("%d", expectedLen),
			// Notice: content-length is intentionally omitted to simulate SW fetch streaming
		},
	}
	reqData, _ := json.Marshal(reqMeta)
	if err := clientStream.WriteHeaders(reqData); err != nil {
		t.Fatalf("WriteHeaders failed: %v", err)
	}

	if _, err := clientStream.Write(payload); err != nil {
		t.Fatalf("Write body failed: %v", err)
	}
	_ = clientStream.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	respHeadersBytes, err := clientStream.ReadHeaders(ctx)
	if err != nil {
		t.Fatalf("ReadHeaders failed: %v", err)
	}

	var respMeta ResponseMetadata
	if err := json.Unmarshal(respHeadersBytes, &respMeta); err != nil {
		t.Fatalf("Unmarshal respMeta failed: %v", err)
	}
	if respMeta.Status != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", respMeta.Status)
	}

	mu.Lock()
	defer mu.Unlock()

	if receivedContentLength != expectedLen {
		t.Errorf("Expected r.ContentLength=%d, got %d", expectedLen, receivedContentLength)
	}
	if receivedHeaderCL != fmt.Sprintf("%d", expectedLen) {
		t.Errorf("Expected Content-Length header=%d, got %q", expectedLen, receivedHeaderCL)
	}
	if receivedChunked {
		t.Errorf("Expected non-chunked transfer encoding, but chunked was received")
	}
	if receivedP2PLengthHeader != "" {
		t.Errorf("Expected x-p2p-body-length header to be stripped, got %q", receivedP2PLengthHeader)
	}
	if string(receivedBody) != string(payload) {
		t.Errorf("Payload mismatch: expected %q, got %q", string(payload), string(receivedBody))
	}
}
