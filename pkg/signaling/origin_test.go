package signaling

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testDistFS() fstest.MapFS {
	return fstest.MapFS{
		"dist/index.html":                  &fstest.MapFile{Data: []byte("<!doctype html><html><head><title>Portal</title></head><body>ok</body></html>")},
		"dist/favicon.svg":                 &fstest.MapFile{Data: []byte("<svg/>")},
		"dist/__p2p_gw__/portal.js":        &fstest.MapFile{Data: []byte("console.log('portal');")},
		"dist/__p2p_gw__/portal.css":       &fstest.MapFile{Data: []byte("body{margin:0}")},
		"dist/__p2p_gw__/sw.js":            &fstest.MapFile{Data: []byte("console.log('sw');")},
		"dist/__p2p_gw__/bridge.html":      &fstest.MapFile{Data: []byte("<!doctype html><html><head></head><body><script src=\"/__p2p_gw__/assets/bridge.js\"></script></body></html>")},
		"dist/__p2p_gw__/assets/bridge.js": &fstest.MapFile{Data: []byte("console.log('bridge');")},
		"dist/__p2p_gw__/assets/patch.js":  &fstest.MapFile{Data: []byte("console.log('patch');")},
	}
}

func TestSignalV7_FailClosedStartupAndDomainPolicies(t *testing.T) {
	if _, err := NewSignalServer(ServerOptions{Addr: "127.0.0.1:8888"}); err == nil {
		t.Fatal("expected missing SIGNAL_SECRET without -allow-anonymous-agents to fail")
	}
	if _, err := NewSignalServer(ServerOptions{Addr: "0.0.0.0:8888", SignalSecret: "s"}); err == nil {
		t.Fatal("expected non-loopback without domains to fail")
	}
	if _, err := NewSignalServer(ServerOptions{
		Addr:          "0.0.0.0:8888",
		SignalSecret:  "s",
		PortalDomain:  "portal.example.com",
		SandboxDomain: "sbxroot.example.com",
	}); err == nil {
		t.Fatal("expected same eTLD+1 without opt-in flag to fail")
	}
	// Single-sided TLS configuration must fail closed.
	if _, err := NewSignalServer(ServerOptions{
		Addr:         "127.0.0.1:8888",
		SignalSecret: "s",
		TLSCert:      "/tmp/cert.pem",
	}); err == nil {
		t.Fatal("expected single-sided -tls-cert without -tls-key to fail closed")
	}
	// Whitespace-only SIGNAL_SECRET must fail closed.
	if _, err := NewSignalServer(ServerOptions{
		Addr:         "127.0.0.1:8888",
		SignalSecret: "   \t  ",
	}); err == nil {
		t.Fatal("expected whitespace-only SIGNAL_SECRET to fail closed")
	}
	// Tier 1 fixed dual-domain: same eTLD+1 must fail without -allow-same-site-sandbox.
	if _, err := NewSignalServer(ServerOptions{
		Addr:         "0.0.0.0:8888",
		SignalSecret: "s",
		PortalDomain: "portal.example.com",
		SandboxHost:  "app.example.com",
	}); err == nil {
		t.Fatal("expected Tier 1 same eTLD+1 without opt-in flag to fail")
	}
	// Tier 1 fixed dual-domain with distinct eTLD+1 succeeds.
	srvT1, err := NewSignalServer(ServerOptions{
		Addr:         "0.0.0.0:8888",
		SignalSecret: "s",
		PortalDomain: "portal.example.com",
		SandboxHost:  "app.example.net",
	})
	if err != nil {
		t.Fatalf("expected Tier 1 distinct cross-site domains to succeed, got %v", err)
	}
	srvT1.Shutdown()

	// Tier 2 single-root wildcard with explicit -allow-same-site-sandbox succeeds.
	srvT2, err := NewSignalServer(ServerOptions{
		Addr:                 "0.0.0.0:8888",
		SignalSecret:         "s",
		PortalDomain:         "portal.example.com",
		SandboxDomain:        "sbx.example.com",
		AllowSameSiteSandbox: true,
	})
	if err != nil {
		t.Fatalf("expected Tier 2 explicit same-site wildcard to succeed, got %v", err)
	}
	srvT2.Shutdown()

	// Tier 3 distinct eTLD+1 wildcard succeeds.
	srv, err := NewSignalServer(ServerOptions{
		Addr:          "0.0.0.0:8888",
		SignalSecret:  "s",
		PortalDomain:  "portal.example.com",
		SandboxDomain: "sandbox.example.net",
	})
	if err != nil {
		t.Fatalf("expected distinct cross-site domains to succeed, got %v", err)
	}
	srv.Shutdown()
}

func TestSignalV7_PathCanonicalizationHostAndAirGap(t *testing.T) {
	srv, err := NewSignalServer(ServerOptions{
		Addr:          "0.0.0.0:8888",
		SignalSecret:  "topsecret",
		PortalDomain:  "portal.example.com",
		SandboxDomain: "sandbox.example.net",
		StaticFS:      testDistFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	h := srv.Handler()

	// Single-pass decode: /%73w.js -> /sw.js -> 404.
	req := httptest.NewRequest("GET", "https://portal.example.com/%73w.js", nil)
	req.Host = "portal.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/%%73w.js on portal must return 404, got %d", rec.Code)
	}

	// Double URL-encoded /%2577s/agent/node1 must not route to /ws/agent/node1.
	req = httptest.NewRequest("GET", "https://portal.example.com/%2577s/agent/node1", nil)
	req.Host = "portal.example.com"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("double-encoded /%%2577s/agent/node1 must return 404, got %d", rec.Code)
	}

	req = httptest.NewRequest("GET", "https://portal.example.com/%69ndex.html", nil)
	req.Host = "portal.example.com"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	csp := rec.Header().Get("Content-Security-Policy")
	if rec.Code != http.StatusOK || rec.Header().Get("X-Frame-Options") != "DENY" || !strings.Contains(csp, "connect-src 'self'") || strings.Contains(csp, "unsafe-inline") {
		t.Fatalf("/%%69ndex.html CSP check failed: code=%d csp=%q", rec.Code, csp)
	}

	// Sibling subdomain of portal.example.com must not be implicitly trusted as HostPortal.
	req = httptest.NewRequest("GET", "https://blog.portal.example.com/", nil)
	req.Host = "blog.portal.example.com"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("sibling subdomain of portal must return 421, got %d", rec.Code)
	}

	// Unauthorized Host must return 421.
	req = httptest.NewRequest("GET", "https://evil.example.org/", nil)
	req.Host = "evil.example.org"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("unauthorized host must return 421, got %d", rec.Code)
	}

	// Sandbox host serves /__p2p_gw__/sw.js with Service-Worker-Allowed: /, blocks /ws/*.
	req = httptest.NewRequest("GET", "https://sbx-abc123.sandbox.example.net/__p2p_gw__/sw.js", nil)
	req.Host = "sbx-abc123.sandbox.example.net"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Service-Worker-Allowed") != "/" {
		t.Fatalf("sandbox sw.js failed: code=%d sw-allowed=%q", rec.Code, rec.Header().Get("Service-Worker-Allowed"))
	}

	// bridge.html must inject exact p2p-portal-origins and never allow wildcard frame-ancestors.
	req = httptest.NewRequest("GET", "https://sbx-abc123.sandbox.example.net/__p2p_gw__/bridge.html", nil)
	req.Host = "sbx-abc123.sandbox.example.net"
	req.RemoteAddr = "127.0.0.1:44444"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	bridgeCSP := rec.Header().Get("Content-Security-Policy")
	if rec.Code != http.StatusOK || strings.Contains(bridgeCSP, "*.portal.example.com") || !strings.Contains(bridgeCSP, "frame-ancestors https://portal.example.com;") {
		t.Fatalf("bridge.html CSP must lock exact portal origin, got %q", bridgeCSP)
	}
	if !strings.Contains(rec.Body.String(), `name="p2p-portal-origins"`) {
		t.Fatalf("bridge.html missing injected p2p-portal-origins meta: %s", rec.Body.String())
	}

	req = httptest.NewRequest("GET", "https://sbx-abc123.sandbox.example.net/ws/client/nas", nil)
	req.Host = "sbx-abc123.sandbox.example.net"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("sandbox /ws must return 403, got %d", rec.Code)
	}

	// Plain HTTP absolute-form GET https://... must not spoof IsHTTPS.
	plainReq := httptest.NewRequest("GET", "https://portal.example.com/", nil)
	plainReq.RemoteAddr = "198.51.100.10:12345"
	plainReq.TLS = nil
	if srv.Policy.IsHTTPS(plainReq) {
		t.Fatal("IsHTTPS falsely trusted r.URL.Scheme over plain TCP")
	}

	// Rightmost X-Forwarded-Proto wins.
	xffReq := httptest.NewRequest("GET", "http://portal.example.com/", nil)
	xffReq.RemoteAddr = "127.0.0.1:44444"
	xffReq.Header.Set("X-Forwarded-Proto", "https, http")
	if srv.Policy.IsHTTPS(xffReq) {
		t.Fatal("IsHTTPS trusted leftmost attacker X-Forwarded-Proto instead of rightmost")
	}
}

func TestSignalV7_LocalDevBridgeNeverAllowsWildcardLocalhost(t *testing.T) {
	srv, err := NewSignalServer(ServerOptions{
		Addr:         "127.0.0.1:8888",
		SignalSecret: "s",
		StaticFS:     testDistFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	req := httptest.NewRequest("GET", "http://sbx-victim.localhost:8888/__p2p_gw__/bridge.html", nil)
	req.Host = "sbx-victim.localhost:8888"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	csp := rec.Header().Get("Content-Security-Policy")
	if strings.Contains(csp, "*.localhost") {
		t.Fatalf("local dev bridge.html allowed *.localhost in frame-ancestors: %q", csp)
	}
	if !strings.Contains(csp, "http://localhost:8888") {
		t.Fatalf("expected exact http://localhost:8888 in frame-ancestors, got %q", csp)
	}
}

func TestIPv6OriginNoPanic(t *testing.T) {
	srv, err := NewSignalServer(ServerOptions{
		Addr:         "[::1]:8888",
		SignalSecret: "secret",
		StaticFS:     testDistFS(),
	})
	if err != nil {
		t.Fatalf("IPv6 loopback NewSignalServer failed: %v", err)
	}
	defer srv.Shutdown()

	if !srv.Policy.LocalDevMode {
		t.Fatal("expected [::1]:8888 without domains to run in local dev mode")
	}

	// Portal on [::1]:8888 must be classified as HostPortal and serve index.html without panic.
	req := httptest.NewRequest("GET", "http://[::1]:8888/", nil)
	req.Host = "[::1]:8888"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on [::1]:8888, got %d", rec.Code)
	}

	// Sandbox bridge.html must include [::1]:8888 in exact frame-ancestors without malformed IPv6 brackets.
	brReq := httptest.NewRequest("GET", "http://sbx-test.localhost:8888/__p2p_gw__/bridge.html", nil)
	brReq.Host = "sbx-test.localhost:8888"
	brRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(brRec, brReq)
	if brRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on bridge.html, got %d", brRec.Code)
	}
	csp := brRec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "http://[::1]:8888") {
		t.Fatalf("expected http://[::1]:8888 in bridge.html CSP, got %q", csp)
	}
	if brRec.Header().Get("Cross-Origin-Opener-Policy") != "same-origin" {
		t.Fatalf("expected COOP: same-origin on bridge.html, got %q", brRec.Header().Get("Cross-Origin-Opener-Policy"))
	}

	// Client WS Origin from [::1]:8888 must be accepted.
	wsReq := httptest.NewRequest("GET", "http://[::1]:8888/ws/client/nas", nil)
	wsReq.Host = "[::1]:8888"
	wsReq.Header.Set("Origin", "http://[::1]:8888")
	if !srv.Policy.CheckWSOrigin(wsReq) {
		t.Fatal("expected Origin http://[::1]:8888 to be allowed for WS")
	}
}

func TestHostPortCSPInjectionBlocked(t *testing.T) {
	badDomains := []string{
		"portal.example.com:443; script-src 'unsafe-inline'",
		"portal.example.com:",
		"portal.example.com:abc",
		"portal.example.com:0",
		"portal.example.com:70000",
		"portal.example.com;evil",
	}
	for _, d := range badDomains {
		if _, err := NewSignalServer(ServerOptions{
			Addr:          "0.0.0.0:8888",
			SignalSecret:  "s",
			PortalDomain:  d,
			SandboxDomain: "sandbox.example.net",
		}); err == nil {
			t.Fatalf("expected malformed/CSP-injecting PortalDomain %q to fail closed", d)
		}
	}

	// Malformed Host header at runtime must be rejected with 421 and never reflected into CSP.
	srv, err := NewSignalServer(ServerOptions{
		Addr:          "0.0.0.0:8888",
		SignalSecret:  "s",
		PortalDomain:  "portal.example.com",
		SandboxDomain: "sandbox.example.net",
		StaticFS:      testDistFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	for _, badHost := range []string{
		"portal.example.com:8443", // Unconfigured non-default port must be rejected
		"portal.example.com:",
		"sbx-1.sandbox.example.net:443;script-src",
	} {
		req := httptest.NewRequest("GET", "https://portal.example.com/", nil)
		req.Host = badHost
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusMisdirectedRequest {
			t.Fatalf("expected 421 for Host %q, got %d", badHost, rec.Code)
		}
	}

	// Service-Worker: script and Sec-Fetch-Dest: serviceworker must be rejected (403) on bridge.js and patch.js,
	// and responses MUST emit Vary: Service-Worker, Sec-Fetch-Dest so immutable caches cannot bypass the gate!
	for _, p := range []string{"/__p2p_gw__/assets/bridge.js", "/__p2p_gw__/assets/patch.js"} {
		req := httptest.NewRequest("GET", "https://sbx-1.sandbox.example.net"+p, nil)
		req.Host = "sbx-1.sandbox.example.net"
		req.Header.Set("Service-Worker", "script")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for Service-Worker: script on %s, got %d", p, rec.Code)
		}
		if !strings.Contains(rec.Header().Get("Vary"), "Service-Worker") {
			t.Fatalf("expected Vary: Service-Worker on %s, got %q", p, rec.Header().Get("Vary"))
		}

		req2 := httptest.NewRequest("GET", "https://sbx-1.sandbox.example.net"+p, nil)
		req2.Host = "sbx-1.sandbox.example.net"
		req2.Header.Set("Sec-Fetch-Dest", "serviceworker")
		rec2 := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec2, req2)
		if rec2.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for Sec-Fetch-Dest: serviceworker on %s, got %d", p, rec2.Code)
		}

		reqOK := httptest.NewRequest("GET", "https://sbx-1.sandbox.example.net"+p, nil)
		reqOK.Host = "sbx-1.sandbox.example.net"
		recOK := httptest.NewRecorder()
		srv.Handler().ServeHTTP(recOK, reqOK)
		if recOK.Code != http.StatusOK || !strings.Contains(recOK.Header().Get("Vary"), "Service-Worker") {
			t.Fatalf("expected 200 with Vary: Service-Worker on %s, got %d vary=%q", p, recOK.Code, recOK.Header().Get("Vary"))
		}
	}
}

func TestTrustedProxiesRejectMalformedAnd4In6(t *testing.T) {
	badInputs := []string{
		"not-an-ip",
		"127.0.0.1,invalid-cidr/24",
		"::ffff:192.0.2.0/64", // Must fail closed (never silently expand to 0.0.0.0/0)
		"::ffff:10.0.0.0/95",
		"10.0.0.1/99",
	}
	for _, raw := range badInputs {
		if _, err := ParseTrustedProxies(raw); err == nil {
			t.Fatalf("expected ParseTrustedProxies(%q) to fail closed, but got nil error", raw)
		}
		if _, err := NewSignalServer(ServerOptions{
			Addr:           "127.0.0.1:8888",
			SignalSecret:   "s",
			TrustedProxies: raw,
		}); err == nil {
			t.Fatalf("expected NewSignalServer to fail closed on TrustedProxies=%q", raw)
		}
	}

	// Valid IPv4-mapped IPv6 /120 maps cleanly to IPv4 /24.
	prefixes, err := ParseTrustedProxies("::ffff:192.0.2.0/120,127.0.0.1")
	if err != nil {
		t.Fatalf("expected valid ::ffff:192.0.2.0/120 to succeed, got %v", err)
	}
	if len(prefixes) != 2 || prefixes[0].Bits() != 24 {
		t.Fatalf("expected mapped IPv4 /24 prefix, got %v", prefixes)
	}

	// Absent custom -client-ip-header must never fall through to X-Forwarded-For.
	req := httptest.NewRequest("GET", "http://localhost/", nil)
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("X-Forwarded-For", "198.51.100.88")
	if got := ExtractClientIP(req, prefixes, "CF-Connecting-IP"); got != "127.0.0.1" {
		t.Fatalf("absent CF-Connecting-IP fell through to X-Forwarded-For: got %q", got)
	}
}

func TestSignalV7_TLSAndClientIPExtraction(t *testing.T) {
	trusted, err := ParseTrustedProxies("127.0.0.1,::ffff:127.0.0.1")
	if err != nil {
		t.Fatalf("ParseTrustedProxies failed: %v", err)
	}

	// 1. Default mode (clientIPHeader == ""): no implicit X-Real-IP trust even when XFF is absent.
	reqNoXFF := httptest.NewRequest("GET", "http://localhost/", nil)
	reqNoXFF.RemoteAddr = "127.0.0.1:50000"
	reqNoXFF.Header.Set("X-Real-IP", "198.51.100.99")
	if ip := ExtractClientIP(reqNoXFF, trusted, ""); ip != "127.0.0.1" {
		t.Fatalf("implicit X-Real-IP must NOT be trusted without -client-ip-header: got %q", ip)
	}

	// 2. Blank XFF must fail closed to remoteHost without falling through.
	req := httptest.NewRequest("GET", "http://localhost/", nil)
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("X-Forwarded-For", " , 127.0.0.1")
	req.Header.Set("X-Real-IP", "198.51.100.99")
	if ip := ExtractClientIP(req, trusted, ""); ip != "127.0.0.1" {
		t.Fatalf("blank XFF fell through to X-Real-IP: got %q", ip)
	}

	// 3. Generic single-value headers: Fastly, Cloudflare, CloudFront (IP:port & IPv6:port), explicit X-Real-IP.
	for _, tc := range []struct {
		header string
		value  string
		want   string
	}{
		{"CF-Connecting-IP", "203.0.113.10", "203.0.113.10"},
		{"Fastly-Client-IP", "::ffff:203.0.113.11", "203.0.113.11"},
		{"CloudFront-Viewer-Address", "198.51.100.42:54321", "198.51.100.42"},
		{"CloudFront-Viewer-Address", "[2001:db8::9]:443", "2001:db8::9"},
		{"X-Real-IP", "198.51.100.77", "198.51.100.77"},
	} {
		r := httptest.NewRequest("GET", "http://localhost/", nil)
		r.RemoteAddr = "127.0.0.1:50001"
		r.Header.Set(tc.header, tc.value)
		if got := ExtractClientIP(r, trusted, tc.header); got != tc.want {
			t.Fatalf("ExtractClientIP(%s=%q) = %q, want %q", tc.header, tc.value, got, tc.want)
		}
	}

	// 4. Untrusted immediate peer spoofing -client-ip-header must be ignored.
	reqUntrusted := httptest.NewRequest("GET", "http://localhost/", nil)
	reqUntrusted.RemoteAddr = "192.0.2.55:44321"
	reqUntrusted.Header.Set("Fastly-Client-IP", "203.0.113.9")
	if got := ExtractClientIP(reqUntrusted, trusted, "Fastly-Client-IP"); got != "192.0.2.55" {
		t.Fatalf("untrusted peer spoofed client-ip-header: got %q", got)
	}

	// 5. Duplicate header lines must fail closed to remoteHost (never pick first or fall through to XFF).
	reqDup := httptest.NewRequest("GET", "http://localhost/", nil)
	reqDup.RemoteAddr = "127.0.0.1:50002"
	reqDup.Header.Add("Fastly-Client-IP", "203.0.113.1")
	reqDup.Header.Add("Fastly-Client-IP", "198.51.100.2")
	reqDup.Header.Set("X-Forwarded-For", "198.51.100.99")
	if got := ExtractClientIP(reqDup, trusted, "Fastly-Client-IP"); got != "127.0.0.1" {
		t.Fatalf("duplicate client-ip-header did not fail closed: got %q", got)
	}

	// 6. Comma-separated list or malformed value must fail closed to remoteHost (no XFF fallthrough).
	for _, badVal := range []string{"203.0.113.1, 198.51.100.2", "not-an-ip", "   "} {
		r := httptest.NewRequest("GET", "http://localhost/", nil)
		r.RemoteAddr = "127.0.0.1:50003"
		r.Header.Set("True-Client-IP", badVal)
		r.Header.Set("X-Forwarded-For", "198.51.100.88")
		if got := ExtractClientIP(r, trusted, "True-Client-IP"); got != "127.0.0.1" {
			t.Fatalf("malformed client-ip-header %q did not fail closed: got %q", badVal, got)
		}
	}

	// 7. Startup validation rejects multi-header lists, X-Forwarded-For, Forwarded, and invalid tokens.
	for _, badHdr := range []string{
		"CF-Connecting-IP,X-Real-IP",
		"X-Forwarded-For",
		"Forwarded",
		"Host",
		"Bad Header",
		"X-Bad\r\nInject",
	} {
		if _, err := NewSignalServer(ServerOptions{
			Addr:                 "127.0.0.1:0",
			AllowAnonymousAgents: true,
			ClientIPHeader:       badHdr,
		}); err == nil {
			t.Fatalf("expected NewSignalServer to reject invalid client-ip-header %q", badHdr)
		}
	}
}
