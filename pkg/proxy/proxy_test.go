package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"webrtc-gateway/pkg/mux"
)

// Mock Transport for testing Mux & HTTPProxy in memory
type memoryTransport struct {
	peer           *memoryTransport
	queue          chan []byte
	bufferedAmount uint64
	lowThreshold   uint64
	onLow          func()
	onMsg          func([]byte)
	closeCh        chan struct{}
	mu             sync.Mutex
}

func newMemoryTransportPair() (*memoryTransport, *memoryTransport) {
	t1 := &memoryTransport{queue: make(chan []byte, 1000), closeCh: make(chan struct{})}
	t2 := &memoryTransport{queue: make(chan []byte, 1000), closeCh: make(chan struct{})}
	t1.peer = t2
	t2.peer = t1

	go t1.worker()
	go t2.worker()
	return t1, t2
}

func (m *memoryTransport) worker() {
	for {
		select {
		case <-m.closeCh:
			return
		case data, ok := <-m.queue:
			if !ok {
				return
			}
			m.mu.Lock()
			cb := m.onMsg
			m.mu.Unlock()
			if cb != nil {
				cb(data)
			}
		}
	}
}

func (m *memoryTransport) Send(data []byte) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	m.peer.queue <- cp
	return nil
}

func (m *memoryTransport) BufferedAmount() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bufferedAmount
}

func (m *memoryTransport) SetBufferedAmountLowThreshold(th uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lowThreshold = th
}

func (m *memoryTransport) OnBufferedAmountLow(f func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onLow = f
}

func (m *memoryTransport) Close() {
	close(m.closeCh)
}

// 1. Boundary tests for ExtractRootDomain
func TestExtractRootDomain_ComprehensiveBoundaries(t *testing.T) {
	testCases := []struct {
		input    string
		expected string
	}{
		// Standard domains
		{"example.com", "example.com"},
		{"www.example.com", "example.com"},
		{"api.sub.example.com", "example.com"},
		{"WWW.EXAMPLE.COM", "example.com"}, // Case insensitivity

		// Two-level ccTLDs (co.uk, com.cn, etc.)
		{"bbc.co.uk", "bbc.co.uk"},
		{"news.bbc.co.uk", "bbc.co.uk"},
		{"google.com.hk", "google.com.hk"},
		{"sub.google.com.hk", "google.com.hk"},
		{"tsinghua.edu.cn", "tsinghua.edu.cn"},
		{"beijing.gov.cn", "beijing.gov.cn"},

		// Localhost and single-label hostnames
		{"localhost", "localhost"},
		{"localhost:8888", "localhost"},
		{"nas", "nas"},
		{"nas:5000", "nas"},
		{"homeserver", "homeserver"},

		// IP addresses (IPv4 & with ports)
		{"127.0.0.1", "127.0.0.1"},
		{"127.0.0.1:8888", "127.0.0.1"},
		{"192.168.1.100", "192.168.1.100"},
		{"192.168.1.100:8080", "192.168.1.100"},
		{"10.0.0.1:443", "10.0.0.1"},

		// Domains with custom ports
		{"www.bilibili.com:8443", "bilibili.com"},
		{"sub.domain.org:9000", "domain.org"},
	}

	for _, tc := range testCases {
		actual := ExtractRootDomain(tc.input)
		if actual != tc.expected {
			t.Errorf("ExtractRootDomain(%q) = %q; expected %q", tc.input, actual, tc.expected)
		}
	}
}

// 2. Boundary tests for resolveTargetURL
func TestResolveTargetURL_ComprehensiveBoundaries(t *testing.T) {
	cm := NewSessionCookieManager()
	hp, err := NewHTTPProxy("https://www.example.com/base", cm)
	if err != nil {
		t.Fatalf("NewHTTPProxy failed: %v", err)
	}

	// Case 1: Standard subpaths
	u, host, err := hp.resolveTargetURL("/api/v1/info", nil)
	if err != nil || host != "www.example.com" || u.String() != "https://www.example.com/base/api/v1/info" {
		t.Errorf("Standard subpath resolution mismatch: url=%v, host=%v, err=%v", u, host, err)
	}

	// Case 2: Relative path without leading slash
	u, host, err = hp.resolveTargetURL("login", nil)
	if err != nil || u.String() != "https://www.example.com/base/login" {
		t.Errorf("Relative path without leading slash mismatch: url=%v, err=%v", u, err)
	}

	// Case 3: Preserving query parameters and hash
	u, host, err = hp.resolveTargetURL("/search?q=webrtc%20p2p&lang=zh", nil)
	if err != nil || u.RawQuery != "q=webrtc%20p2p&lang=zh" {
		t.Errorf("Query params mismatch: url=%v, query=%q", u, u.RawQuery)
	}

	// Case 4: Sibling subdomain via /--p2p-proxy/api.example.com/endpoint
	u, host, err = hp.resolveTargetURL("/--p2p-proxy/api.example.com/endpoint?foo=bar", nil)
	if err != nil {
		t.Fatalf("Sibling proxy path failed: %v", err)
	}
	if host != "api.example.com" || u.String() != "https://api.example.com/endpoint?foo=bar" {
		t.Errorf("Sibling proxy mismatch: url=%v, host=%v", u, host)
	}

	// Case 5: Allowed media stream path regardless of host
	u, host, err = hp.resolveTargetURL("/--p2p-proxy/xy123x.bilivideo.com/upgcxcode/123.m4s", nil)
	if err != nil {
		t.Fatalf("Media stream path should be allowed, got error: %v", err)
	}
	if host != "xy123x.bilivideo.com" {
		t.Errorf("Media host mismatch: got %v", host)
	}

	// Case 6: Security boundary: completely unrelated 3rd party domain blocked
	_, _, err = hp.resolveTargetURL("/--p2p-proxy/malicious-attacker.com/steal", nil)
	if err == nil {
		t.Errorf("Expected cross-origin restriction error for malicious host, got nil")
	}

	// Case 7: Via x-p2p-dest-host header
	headers := map[string]string{"x-p2p-dest-host": "auth.example.com"}
	u, host, err = hp.resolveTargetURL("/oauth/token", headers)
	if err != nil || host != "auth.example.com" || u.String() != "https://auth.example.com/oauth/token" {
		t.Errorf("x-p2p-dest-host header resolution failed: %v, host=%v", u, host)
	}
}

// 3. Boundary tests for rewriteLocation
func TestRewriteLocation_ComprehensiveBoundaries(t *testing.T) {
	cm := NewSessionCookieManager()
	hp, err := NewHTTPProxy("https://www.example.com", cm)
	if err != nil {
		t.Fatalf("NewHTTPProxy failed: %v", err)
	}

	// Case 1: Absolute redirect to self target
	loc := hp.rewriteLocation("https://www.example.com/dashboard", "www.example.com")
	if loc != "/dashboard" {
		t.Errorf("Self absolute redirect should become relative: got %q, expected /dashboard", loc)
	}

	// Case 2: Absolute redirect to sibling subdomain
	loc = hp.rewriteLocation("https://passport.example.com/login?ret=/dashboard", "www.example.com")
	if loc != "/--p2p-proxy/passport.example.com/login?ret=/dashboard" {
		t.Errorf("Sibling redirect mismatch: got %q", loc)
	}

	// Case 3: Absolute redirect to external 3rd party (e.g. Google OAuth) should be preserved!
	extURL := "https://accounts.google.com/o/oauth2/auth?client_id=123"
	loc = hp.rewriteLocation(extURL, "www.example.com")
	if loc != extURL {
		t.Errorf("External OAuth redirect should be preserved: got %q", loc)
	}

	// Case 4: Relative redirect on root target
	loc = hp.rewriteLocation("/profile/settings", "www.example.com")
	if loc != "/profile/settings" {
		t.Errorf("Relative redirect on root target mismatch: got %q", loc)
	}

	// Case 5: Relative redirect on sibling host (must stay on that sibling host!)
	loc = hp.rewriteLocation("/login/success", "passport.example.com")
	if loc != "/--p2p-proxy/passport.example.com/login/success" {
		t.Errorf("Relative redirect on sibling host mismatch: got %q", loc)
	}
}

// 4. Boundary tests for Cookie Jar & HttpOnly shielding
func TestCookieJar_IsolationAndNonHttpOnly(t *testing.T) {
	cm := NewSessionCookieManager()
	sessionA := "session-user-a"
	sessionB := "session-user-b"

	targetURL, _ := url.Parse("https://app.internal/api")

	// Session A receives cookies
	respHeadersA := http.Header{}
	respHeadersA.Add("Set-Cookie", "session_id=sess_12345; Path=/; HttpOnly; Secure")
	respHeadersA.Add("Set-Cookie", "theme=dark; Path=/")
	respHeadersA.Add("Set-Cookie", "visitor_token=tok_abc; Path=/; SameSite=Lax")
	cm.SaveCookies(sessionA, targetURL, respHeadersA)

	// Verify ExtractNonHttpOnlyCookies (Security check: HttpOnly MUST NOT leak to client)
	rawSetCookies := respHeadersA["Set-Cookie"]
	syncable := ExtractNonHttpOnlyCookies(rawSetCookies)
	if len(syncable) != 2 {
		t.Fatalf("Expected 2 non-HttpOnly cookies, got %d", len(syncable))
	}
	for _, c := range syncable {
		if strings.Contains(strings.ToLower(c), "httponly") {
			t.Errorf("HttpOnly cookie leaked to client: %s", c)
		}
	}

	// Inject into outgoing request for Session A
	reqA, _ := http.NewRequest("GET", "https://app.internal/api/data", nil)
	cm.InjectCookies(sessionA, reqA)
	cookieHeaderA := reqA.Header.Get("Cookie")
	if !strings.Contains(cookieHeaderA, "session_id=sess_12345") || !strings.Contains(cookieHeaderA, "theme=dark") {
		t.Errorf("Cookie header missing expected cookies: %s", cookieHeaderA)
	}

	// Session B must be isolated (zero cookies)
	reqB, _ := http.NewRequest("GET", "https://app.internal/api/data", nil)
	cm.InjectCookies(sessionB, reqB)
	if reqB.Header.Get("Cookie") != "" {
		t.Errorf("Session B was not isolated! Got: %s", reqB.Header.Get("Cookie"))
	}

	// Clean up Session A
	cm.RemoveSession(sessionA)
	reqA2, _ := http.NewRequest("GET", "https://app.internal/api/data", nil)
	cm.InjectCookies(sessionA, reqA2)
	if reqA2.Header.Get("Cookie") != "" {
		t.Errorf("Session A cookies were not removed after RemoveSession! Got: %s", reqA2.Header.Get("Cookie"))
	}
}

// 5. Test Gzip decompression & TCP connection close defense
func TestHTTPProxy_GzipDecompressionAndResourceLeak(t *testing.T) {
	// Mock backend that serves gzipped payload
	rawPayload := "Hello P2P Gateway! Testing transparent Gzip decompression and leak-proof response body closing."
	var gzBuf bytes.Buffer
	zw := gzip.NewWriter(&gzBuf)
	_, _ = zw.Write([]byte(rawPayload))
	_ = zw.Close()

	bodyClosed := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(gzBuf.Bytes())
		bodyClosed = true
	}))
	defer ts.Close()

	cm := NewSessionCookieManager()
	hp, err := NewHTTPProxy(ts.URL, cm)
	if err != nil {
		t.Fatalf("NewHTTPProxy failed: %v", err)
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

	// Agent accept loop
	go func() {
		stream, err := agentMux.AcceptStream(context.Background())
		if err != nil {
			return
		}
		hp.HandleStream("test-session-gz", stream)
	}()

	// Client sends GET request
	clientStream, err := clientMux.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}

	reqMeta := RequestMetadata{
		Method: "GET",
		Path:   "/test-gzip",
		Headers: map[string]string{
			"Accept-Encoding": "gzip",
		},
	}
	reqData, _ := json.Marshal(reqMeta)
	if err := clientStream.WriteHeaders(reqData); err != nil {
		t.Fatalf("WriteHeaders failed: %v", err)
	}
	_ = clientStream.Close()

	// Read response headers
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
		t.Errorf("Expected status 200, got %d", respMeta.Status)
	}
	if enc, exists := respMeta.Headers["content-encoding"]; exists && enc != "" {
		t.Errorf("Content-Encoding header should be stripped after decompression, got %q", enc)
	}

	// Read decompressed body
	bodyReceived := new(bytes.Buffer)
	_, err = io.Copy(bodyReceived, clientStream)
	if err != nil {
		t.Fatalf("Copy body failed: %v", err)
	}

	if bodyReceived.String() != rawPayload {
		t.Errorf("Decompressed payload mismatch:\nExpected: %q\nGot:      %q", rawPayload, bodyReceived.String())
	}

	if !bodyClosed {
		t.Errorf("Backend response handler was not executed")
	}
}

// 6. Test Range request bypasses Gzip to allow proper 206 binary streaming
func TestHTTPProxy_RangeRequestBypassesGzip(t *testing.T) {
	acceptEncodingReceived := ""
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acceptEncodingReceived = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Range", "bytes 0-100/1000")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(make([]byte, 101))
	}))
	defer ts.Close()

	cm := NewSessionCookieManager()
	hp, err := NewHTTPProxy(ts.URL, cm)
	if err != nil {
		t.Fatalf("NewHTTPProxy failed: %v", err)
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

	go func() {
		stream, _ := agentMux.AcceptStream(context.Background())
		if stream != nil {
			hp.HandleStream("sess-range", stream)
		}
	}()

	clientStream, _ := clientMux.OpenStream()
	reqMeta := RequestMetadata{
		Method: "GET",
		Path:   "/video.mp4",
		Headers: map[string]string{
			"Range": "bytes=0-100",
		},
	}
	reqData, _ := json.Marshal(reqMeta)
	_ = clientStream.WriteHeaders(reqData)
	_ = clientStream.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	respHeadersBytes, err := clientStream.ReadHeaders(ctx)
	if err != nil {
		t.Fatalf("ReadHeaders failed: %v", err)
	}

	var respMeta ResponseMetadata
	_ = json.Unmarshal(respHeadersBytes, &respMeta)
	if respMeta.Status != http.StatusPartialContent {
		t.Errorf("Expected status 206 Partial Content, got %d", respMeta.Status)
	}

	if acceptEncodingReceived != "" {
		t.Errorf("Accept-Encoding should be stripped for Range requests, got: %q", acceptEncodingReceived)
	}
}

// 7. Test Outbound Proxy configuration propagation
func TestHTTPProxy_OutboundProxyConfiguration(t *testing.T) {
	cm := NewSessionCookieManager()
	hp, err := NewHTTPProxy("https://www.example.com", cm)
	if err != nil {
		t.Fatalf("NewHTTPProxy failed: %v", err)
	}

	// 1. Explicit SOCKS5 proxy
	hp.SetProxy("socks5://127.0.0.1:10808")
	dummyReq, _ := http.NewRequest("GET", "https://www.example.com", nil)
	proxyURL, err := hp.transport.Proxy(dummyReq)
	if err != nil || proxyURL == nil || proxyURL.String() != "socks5://127.0.0.1:10808" {
		t.Errorf("SOCKS5 proxy not configured properly: %v", proxyURL)
	}

	// 2. Explicit HTTP proxy
	hp.SetProxy("http://proxy.corp.internal:8080")
	proxyURL, err = hp.transport.Proxy(dummyReq)
	if err != nil || proxyURL == nil || proxyURL.String() != "http://proxy.corp.internal:8080" {
		t.Errorf("HTTP proxy not configured properly: %v", proxyURL)
	}

	// 3. Reset to empty (should fallback to ProxyFromEnvironment)
	hp.SetProxy("")
	if hp.transport.Proxy == nil {
		t.Errorf("Proxy func should not be nil when reset")
	}
}
