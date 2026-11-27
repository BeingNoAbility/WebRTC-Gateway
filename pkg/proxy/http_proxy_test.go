package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"webrtc-gateway/pkg/protocol"
)

func TestProxyV7_HTTPProxy_FastPostGzipAndStaticOSRootSecurity(t *testing.T) {
	// 1. Fast POST body preservation + spoofed internal header stripping + transparent gzip.
	var gotPostBody string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fast-post" {
			b, _ := io.ReadAll(r.Body)
			gotPostBody = string(b)
			w.Header().Set("X-P2P-Content-Encoding", "zstd") // Spoofed internal header must be stripped
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write(bytes.Repeat([]byte(`{"ok":true}`), 200))
			return
		}
		w.WriteHeader(404)
	}))
	defer backend.Close()

	pol, _ := NewDestinationPolicy(backend.URL, "")
	hp, _ := NewHTTPProxy(HTTPProxyOptions{Policy: pol})
	cm, sm := newTestMuxPair(t)

	// Send fast POST before server calls AcceptStream.
	postTLV, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindHTTP, FetchCtx: protocol.FetchCtxSameOriginClient | protocol.FetchCtxClientDecompressGzip,
		Method: "POST", Target: "/fast-post",
		Headers: []protocol.HeaderField{
			{Name: "content-type", Value: "application/json"},
			{Name: "content-length", Value: "15"},
		},
	})
	cs, err := cm.OpenHTTPStream(context.Background(), postTLV, false)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = cs.Write([]byte(`{"hello":"v7!"}`))
	_ = cs.CloseWrite()
	time.Sleep(30 * time.Millisecond)

	go func() {
		st, err := sm.AcceptStream(context.Background())
		if err != nil {
			return
		}
		rh, _ := protocol.DecodeRequestHeaders(st.HeadersRaw())
		hp.HandleStream(st, rh, "198.51.100.2", nil, nil)
	}()

	rb, err := cs.ReadResponseHeaders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := protocol.DecodeResponseHeaders(rb)
	rawBody, _ := io.ReadAll(cs)
	_ = cs.Close()

	if gotPostBody != `{"hello":"v7!"}` {
		t.Fatalf("fast POST dropped body: got %q", gotPostBody)
	}
	if resp.RespFlags&protocol.RespFlagDecompressGzip == 0 {
		t.Fatalf("expected RespFlagDecompressGzip bit, got respFlags=%#x", resp.RespFlags)
	}
	gzr, err := gzip.NewReader(bytes.NewReader(rawBody))
	if err != nil {
		t.Fatalf("response was not valid gzip: %v", err)
	}
	plain, _ := io.ReadAll(gzr)
	_ = gzr.Close()
	if !strings.HasPrefix(string(plain), `{"ok":true}`) {
		t.Fatalf("unexpected decompressed body: %q", string(plain[:20]))
	}

	// 2. Backend raw TCP EOF -> 502 Bad Gateway.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	polEOF, _ := NewDestinationPolicy("http://"+ln.Addr().String(), "")
	hpEOF, _ := NewHTTPProxy(HTTPProxyOptions{Policy: polEOF})
	go func() {
		st, err := sm.AcceptStream(context.Background())
		if err != nil {
			return
		}
		rh, _ := protocol.DecodeRequestHeaders(st.HeadersRaw())
		hpEOF.HandleStream(st, rh, "198.51.100.2", nil, nil)
	}()
	csEOF, _ := cm.OpenHTTPStream(context.Background(), mustReqTLV("GET", "/crash"), true)
	rbEOF, _ := csEOF.ReadResponseHeaders(context.Background())
	respEOF, _ := protocol.DecodeResponseHeaders(rbEOF)
	_ = csEOF.Close()
	if respEOF.Status != http.StatusBadGateway {
		t.Fatalf("expected 502 on backend EOF, got %d", respEOF.Status)
	}

	// 3. StaticFSHandler through HTTPProxy: extensionless HTML CSP sandbox, multi-hop symlink .env 404, allow-downloads on dir.
	tmpDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmpDir, "report_100%_final.txt"), []byte("percent-ok"), 0600)
	_ = os.WriteFile(filepath.Join(tmpDir, "payload"), []byte("<html><script>alert(1)</script></html>"), 0600)
	_ = os.WriteFile(filepath.Join(tmpDir, ".env"), []byte("TOP_SECRET=1"), 0600)
	_ = os.WriteFile(filepath.Join(tmpDir, "2026:11:26.log"), []byte("colon-ok"), 0600)
	_ = os.Symlink(".env", filepath.Join(tmpDir, "hop2"))
	_ = os.Symlink("hop2", filepath.Join(tmpDir, "hop1"))
	_ = os.Mkdir(filepath.Join(tmpDir, "sub"), 0700)
	_ = os.WriteFile(filepath.Join(tmpDir, "sub", "index.html"), []byte("<h1>Hi</h1>"), 0600)

	sh, err := NewStaticFSHandler(StaticFSOptions{RootDir: tmpDir, HideDotfiles: true, InlineActive: false})
	if err != nil {
		t.Fatal(err)
	}
	defer sh.Close()

	polFS, _ := NewDestinationPolicy("http://static.local", "")
	hpFS, _ := NewHTTPProxy(HTTPProxyOptions{Policy: polFS, CustomTransport: NewInProcessTransport(sh)})

	doStaticReq := func(target string, capScope *CapabilityScope) (uint16, []protocol.HeaderField, string) {
		cm2, sm2 := newTestMuxPair(t)
		go func() {
			st, err := sm2.AcceptStream(context.Background())
			if err != nil {
				return
			}
			rh, _ := protocol.DecodeRequestHeaders(st.HeadersRaw())
			hpFS.HandleStream(st, rh, "198.51.100.2", nil, capScope)
		}()
		s, _ := cm2.OpenHTTPStream(context.Background(), mustReqTLV("GET", target), true)
		rb, err := s.ReadResponseHeaders(context.Background())
		if err != nil {
			t.Fatalf("req %s ReadResponseHeaders: %v", target, err)
		}
		r, _ := protocol.DecodeResponseHeaders(rb)
		body, _ := io.ReadAll(s)
		_ = s.Close()
		return r.Status, r.Headers, string(body)
	}

	// Extensionless HTML must carry Content-Security-Policy: sandbox.
	st, hdrs, _ := doStaticReq("/payload", nil)
	if st != 200 {
		t.Fatalf("expected 200 for /payload, got %d", st)
	}
	hasCSP := false
	for _, h := range hdrs {
		if h.Name == "content-security-policy" && strings.Contains(h.Value, "sandbox") {
			hasCSP = true
		}
	}
	if !hasCSP {
		t.Fatalf("extensionless HTML file served without CSP sandbox: %+v", hdrs)
	}

	// 2-hop symlink hop1 -> hop2 -> .env must return 404.
	st, _, body := doStaticReq("/hop1", nil)
	if st != 404 || strings.Contains(body, "TOP_SECRET") {
		t.Fatalf("2-hop symlink leaked .env: status=%d body=%q", st, body)
	}

	// `%` filename works.
	st, _, body = doStaticReq("/report_100%25_final.txt", nil)
	if st != 200 || body != "percent-ok" {
		t.Fatalf("%% filename failed: status=%d body=%q", st, body)
	}

	// Directory listing includes allow-downloads and `./` prefix for `:` filenames.
	st, hdrs, listHTML := doStaticReq("/", nil)
	if st != 200 || !strings.Contains(listHTML, `href="./2026:11:26.log"`) || strings.Contains(listHTML, ".env") {
		t.Fatalf("unexpected dir listing: %s", listHTML)
	}
	hasAllowDl := false
	for _, h := range hdrs {
		if h.Name == "content-security-policy" && strings.Contains(h.Value, "allow-downloads") {
			hasAllowDl = true
		}
	}
	if !hasAllowDl {
		t.Fatalf("directory listing missing allow-downloads in CSP: %+v", hdrs)
	}
}

func TestHTTPProxyMultiEncodingStrongETagAndContentLength(t *testing.T) {
	var sawGetCL string
	var sawZeroBodyLen int64 = -99
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/multi-enc":
			w.Header().Add("Content-Encoding", "br")
			w.Header().Add("Content-Encoding", "gzip")
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("raw-multi-encoded"))
		case "/no-transform":
			w.Header().Set("Cache-Control", "public, no-transform")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(bytes.Repeat([]byte(`{"a":1}`), 200))
		case "/strong-etag":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `"strong-v7-etag"`)
			_, _ = w.Write(bytes.Repeat([]byte(`{"k":"v"}`), 200))
		case "/empty-gzip":
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(200)
		case "/get-cl":
			sawGetCL = r.Header.Get("Content-Length")
			w.WriteHeader(200)
		case "/post-zero":
			sawZeroBodyLen = r.ContentLength
			b, _ := io.ReadAll(r.Body)
			w.Header().Set("X-Body-Len", fmt.Sprintf("%d", len(b)))
			w.WriteHeader(200)
		}
	}))
	defer backend.Close()

	pol, _ := NewDestinationPolicy(backend.URL, "")
	hp, _ := NewHTTPProxy(HTTPProxyOptions{Policy: pol})

	doReq := func(method, target string, hdrs []protocol.HeaderField, hasBody bool) (*protocol.ResponseHeaders, []byte) {
		cm, sm := newTestMuxPair(t)
		go func() {
			st, err := sm.AcceptStream(context.Background())
			if err != nil {
				return
			}
			rh, _ := protocol.DecodeRequestHeaders(st.HeadersRaw())
			hp.HandleStream(st, rh, "198.51.100.1", nil, nil)
		}()
		tlv, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
			Kind:     protocol.StreamKindHTTP,
			FetchCtx: protocol.FetchCtxSameOriginClient | protocol.FetchCtxClientDecompressGzip,
			Method:   method,
			Target:   target,
			Headers:  hdrs,
		})
		s, err := cm.OpenHTTPStream(context.Background(), tlv, !hasBody)
		if err != nil {
			t.Fatal(err)
		}
		if hasBody {
			_ = s.CloseWrite()
		}
		rb, err := s.ReadResponseHeaders(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		resp, _ := protocol.DecodeResponseHeaders(rb)
		body, _ := io.ReadAll(s)
		_ = s.Close()
		return resp, body
	}

	// 1. Multi Content-Encoding (br + gzip) must not set RespFlagDecompressGzip.
	rMulti, bMulti := doReq("GET", "/multi-enc", nil, false)
	if rMulti.RespFlags&protocol.RespFlagDecompressGzip != 0 || string(bMulti) != "raw-multi-encoded" {
		t.Fatalf("multi Content-Encoding wrongly flagged for gzip decompress: flags=%#x body=%q", rMulti.RespFlags, string(bMulti))
	}

	// 2. Cache-Control: no-transform must not be gzipped.
	rNT, _ := doReq("GET", "/no-transform", nil, false)
	if rNT.RespFlags&protocol.RespFlagDecompressGzip != 0 {
		t.Fatalf("Cache-Control: no-transform was gzipped")
	}

	// 3. Strong ETag must be preserved intact when transparent gateway gzip is enabled.
	rETag, _ := doReq("GET", "/strong-etag", nil, false)
	if rETag.RespFlags&protocol.RespFlagDecompressGzip == 0 {
		t.Fatalf("expected transparent gzip on /strong-etag")
	}
	var gotETag string
	for _, h := range rETag.Headers {
		if h.Name == "etag" {
			gotETag = h.Value
		}
	}
	if gotETag != `"strong-v7-etag"` {
		t.Fatalf("strong ETag was weakened or altered: got %q", gotETag)
	}

	// 4. Empty 0B response with Content-Encoding: gzip must not set RespFlagDecompressGzip.
	rEmpty, _ := doReq("GET", "/empty-gzip", nil, false)
	if rEmpty.RespFlags&protocol.RespFlagDecompressGzip != 0 {
		t.Fatalf("0-byte gzip response must not flag SW decompression")
	}

	// 5. Conflicting duplicate Content-Length -> 400 Bad Request.
	rBadCL, _ := doReq("POST", "/post-zero", []protocol.HeaderField{
		{Name: "content-length", Value: "5"},
		{Name: "content-length", Value: "10"},
	}, false)
	if rBadCL.Status != http.StatusBadRequest {
		t.Fatalf("expected 400 for conflicting Content-Length, got %d", rBadCL.Status)
	}

	// 6. GET with Content-Length strips Content-Length cleanly.
	rGetCL, _ := doReq("GET", "/get-cl", []protocol.HeaderField{{Name: "content-length", Value: "5"}}, false)
	if rGetCL.Status != 200 || sawGetCL != "" {
		t.Fatalf("GET Content-Length was not stripped: status=%d sawCL=%q", rGetCL.Status, sawGetCL)
	}

	// 7. POST with Content-Length: 0 passes nil body cleanly.
	rPost0, _ := doReq("POST", "/post-zero", []protocol.HeaderField{{Name: "content-length", Value: "0"}}, true)
	if rPost0.Status != 200 || sawZeroBodyLen != 0 {
		t.Fatalf("POST Content-Length: 0 mismatch: status=%d cl=%d", rPost0.Status, sawZeroBodyLen)
	}
}
