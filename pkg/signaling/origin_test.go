package signaling

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

const testStrongSecret = "test-signal-secret-0123456789"

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

func TestSignal_FailClosedStartupAndOriginPolicies(t *testing.T) {
	if _, err := NewSignalServer(ServerOptions{Addr: "127.0.0.1:8888"}); err == nil {
		t.Fatal("expected missing SIGNAL_SECRET without -allow-anonymous-agents to fail")
	}
	if _, err := NewSignalServer(ServerOptions{Addr: "127.0.0.1:8888", SignalSecret: "too-short"}); err == nil {
		t.Fatal("expected <16-byte SIGNAL_SECRET to fail closed")
	}
	if _, err := NewSignalServer(ServerOptions{Addr: "0.0.0.0:8888", SignalSecret: testStrongSecret}); err == nil {
		t.Fatal("expected non-loopback without explicit origins to fail")
	}
	if _, err := NewSignalServer(ServerOptions{Addr: "0.0.0.0:8888", DevMode: true, SignalSecret: testStrongSecret}); err == nil {
		t.Fatal("expected -dev on non-loopback address to fail closed")
	}
	if _, err := NewSignalServer(ServerOptions{
		Addr:          "127.0.0.1:8888",
		DevMode:       true,
		PortalOrigin:  "https://portal.example.com",
		SandboxOrigin: "https://*.sandbox.example.net",
		SignalSecret:  testStrongSecret,
	}); err == nil {
		t.Fatal("expected -dev combined with explicit origins to fail closed")
	}
	if _, err := NewSignalServer(ServerOptions{
		Addr:          "0.0.0.0:8888",
		SignalSecret:  testStrongSecret,
		PortalOrigin:  "https://portal.example.com",
		SandboxOrigin: "https://*.sbxroot.example.com",
	}); err == nil {
		t.Fatal("expected same eTLD+1 wildcard without -allow-same-site-sandbox to fail")
	}
	if _, err := NewSignalServer(ServerOptions{
		Addr:          "0.0.0.0:8888",
		SignalSecret:  testStrongSecret,
		PortalOrigin:  "https://portal.example.com",
		SandboxOrigin: "http://*.sandbox.example.net",
	}); err == nil {
		t.Fatal("expected mismatched portal/sandbox schemes to fail closed")
	}
	// Single-sided TLS configuration must fail closed.
	if _, err := NewSignalServer(ServerOptions{
		Addr:         "127.0.0.1:8888",
		SignalSecret: testStrongSecret,
		TLSCert:      "/tmp/cert.pem",
	}); err == nil {
		t.Fatal("expected single-sided -tls-cert without -tls-key to fail closed")
	}
	// Whitespace-only SIGNAL_SECRET must fail closed.
	if _, err := NewSignalServer(ServerOptions{
		Addr:         "127.0.0.1:8888",
		SignalSecret: "                    ",
	}); err == nil {
		t.Fatal("expected whitespace-only SIGNAL_SECRET to fail closed")
	}
	// Tier 2 fixed sandbox: same eTLD+1 must always fail.
	if _, err := NewSignalServer(ServerOptions{
		Addr:                 "0.0.0.0:8888",
		SignalSecret:         testStrongSecret,
		PortalOrigin:         "https://portal.example.com",
		SandboxOrigin:        "https://app.example.com",
		AllowSameSiteSandbox: true,
	}); err == nil {
		t.Fatal("expected fixed sandbox with same eTLD+1 to fail closed")
	}

	// Tier 2 restricted fixed sandbox with distinct eTLD+1 succeeds and sets RestrictedSandbox=true.
	srvFixed, err := NewSignalServer(ServerOptions{
		Addr:          "0.0.0.0:8888",
		SignalSecret:  testStrongSecret,
		PortalOrigin:  "https://portal.example.com",
		SandboxOrigin: "https://app.example.net",
	})
	if err != nil {
		t.Fatalf("expected Tier 2 fixed distinct cross-site origins to succeed, got %v", err)
	}
	if !srvFixed.Policy.RestrictedSandbox || srvFixed.Policy.Mode != SandboxModeFixed || srvFixed.Policy.IsSameSite {
		t.Fatalf("expected fixed sandbox to be Tier 2 restricted cross-site, got %+v", srvFixed.Policy)
	}
	srvFixed.Shutdown()

	// Tier 2 same-site wildcard with explicit -allow-same-site-sandbox succeeds.
	srvSameSite, err := NewSignalServer(ServerOptions{
		Addr:                 "0.0.0.0:8888",
		SignalSecret:         testStrongSecret,
		PortalOrigin:         "https://portal.example.com",
		SandboxOrigin:        "https://*.sbx.example.com",
		AllowSameSiteSandbox: true,
	})
	if err != nil {
		t.Fatalf("expected Tier 2 explicit same-site wildcard to succeed, got %v", err)
	}
	if !srvSameSite.Policy.RestrictedSandbox || !srvSameSite.Policy.IsSameSite {
		t.Fatalf("expected same-site wildcard to set RestrictedSandbox and IsSameSite, got %+v", srvSameSite.Policy)
	}
	srvSameSite.Shutdown()

	// Tier 1 cross-site wildcard succeeds with RestrictedSandbox=false.
	srvT1, err := NewSignalServer(ServerOptions{
		Addr:          "0.0.0.0:8888",
		SignalSecret:  testStrongSecret,
		PortalOrigin:  "https://portal.example.com",
		SandboxOrigin: "https://*.sandbox.example.net",
	})
	if err != nil {
		t.Fatalf("expected Tier 1 distinct cross-site wildcard origins to succeed, got %v", err)
	}
	if srvT1.Policy.RestrictedSandbox || srvT1.Policy.IsSameSite || srvT1.Policy.Mode != SandboxModeWildcard {
		t.Fatalf("expected Tier 1 full isolation wildcard policy, got %+v", srvT1.Policy)
	}
	srvT1.Shutdown()
}

func TestSignal_PathCanonicalizationHostAndAirGap(t *testing.T) {
	srv, err := NewSignalServer(ServerOptions{
		Addr:          "0.0.0.0:8888",
		SignalSecret:  testStrongSecret,
		PortalOrigin:  "https://portal.example.com",
		SandboxOrigin: "https://*.sandbox.example.net",
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
	if rec.Header().Get("Strict-Transport-Security") == "" {
		t.Fatal("expected HSTS header for explicit https:// portal origin")
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

	// Sandbox host serves /__p2p_gw__/sw.js ONLY to SW registration fetches with CORP: same-origin + Service-Worker-Allowed: /.
	req = httptest.NewRequest("GET", "https://sbx-abc123.sandbox.example.net/__p2p_gw__/sw.js", nil)
	req.Host = "sbx-abc123.sandbox.example.net"
	req.Header.Set("Service-Worker", "script")
	req.Header.Set("Sec-Fetch-Dest", "serviceworker")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Service-Worker-Allowed") != "/" || rec.Header().Get("Cross-Origin-Resource-Policy") != "same-origin" {
		t.Fatalf("sandbox sw.js failed: code=%d sw-allowed=%q corp=%q", rec.Code, rec.Header().Get("Service-Worker-Allowed"), rec.Header().Get("Cross-Origin-Resource-Policy"))
	}

	// bridge.html must inject exact p2p-portal-origins and never allow wildcard frame-ancestors,
	// regardless of X-Forwarded-Proto header (explicit startup origin is authoritative).
	req = httptest.NewRequest("GET", "http://sbx-abc123.sandbox.example.net/__p2p_gw__/bridge.html", nil)
	req.Host = "sbx-abc123.sandbox.example.net"
	req.RemoteAddr = "127.0.0.1:44444"
	req.Header.Set("X-Forwarded-Proto", "http")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	bridgeCSP := rec.Header().Get("Content-Security-Policy")
	if rec.Code != http.StatusOK || strings.Contains(bridgeCSP, "*.portal.example.com") || !strings.Contains(bridgeCSP, "frame-ancestors https://portal.example.com;") {
		t.Fatalf("bridge.html CSP must lock exact configured portal origin, got %q", bridgeCSP)
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
}

func TestSignal_LocalDevBridgeNeverAllowsWildcardLocalhost(t *testing.T) {
	srv, err := NewSignalServer(ServerOptions{
		Addr:         "127.0.0.1:8888",
		SignalSecret: testStrongSecret,
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
	if !strings.Contains(csp, "http://localhost:8888") || !strings.Contains(csp, "http://127.0.0.1:8888") {
		t.Fatalf("expected exact IPv4 loopback origins in frame-ancestors, got %q", csp)
	}
	if strings.Contains(csp, "[::1]") {
		t.Fatalf("IPv4 127.0.0.1 listener must not cross-trust IPv6 [::1] in frame-ancestors: %q", csp)
	}
}

func TestIPv6OriginNoPanicAndScopedFamily(t *testing.T) {
	srv, err := NewSignalServer(ServerOptions{
		Addr:         "[::1]:8888",
		SignalSecret: testStrongSecret,
		StaticFS:     testDistFS(),
	})
	if err != nil {
		t.Fatalf("IPv6 loopback NewSignalServer failed: %v", err)
	}
	defer srv.Shutdown()

	if !srv.Policy.LocalDevMode {
		t.Fatal("expected [::1]:8888 without origins to run in local dev mode")
	}

	// Portal on [::1]:8888 must be classified as HostPortal and serve index.html without panic.
	req := httptest.NewRequest("GET", "http://[::1]:8888/", nil)
	req.Host = "[::1]:8888"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on [::1]:8888, got %d", rec.Code)
	}

	// IPv4 127.0.0.1 Host on an IPv6 [::1] listener must be rejected (421).
	v4Req := httptest.NewRequest("GET", "http://127.0.0.1:8888/", nil)
	v4Req.Host = "127.0.0.1:8888"
	v4Rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(v4Rec, v4Req)
	if v4Rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("IPv6 [::1] listener must reject 127.0.0.1 Host with 421, got %d", v4Rec.Code)
	}

	// Sandbox bridge.html must include [::1]:8888 and NOT 127.0.0.1:8888.
	brReq := httptest.NewRequest("GET", "http://sbx-test.localhost:8888/__p2p_gw__/bridge.html", nil)
	brReq.Host = "sbx-test.localhost:8888"
	brRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(brRec, brReq)
	if brRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on bridge.html, got %d", brRec.Code)
	}
	csp := brRec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "http://[::1]:8888") || strings.Contains(csp, "127.0.0.1") {
		t.Fatalf("expected IPv6-only loopback origins in bridge.html CSP, got %q", csp)
	}
	if brRec.Header().Get("Cross-Origin-Opener-Policy") != "same-origin" {
		t.Fatalf("expected COOP: same-origin on bridge.html, got %q", brRec.Header().Get("Cross-Origin-Opener-Policy"))
	}

	// Client WS Origin from [::1]:8888 must be accepted; mismatched localhost Origin on [::1] Host rejected.
	wsReq := httptest.NewRequest("GET", "http://[::1]:8888/ws/client/nas", nil)
	wsReq.Host = "[::1]:8888"
	wsReq.Header.Set("Origin", "http://[::1]:8888")
	if !srv.Policy.CheckWSOrigin(wsReq) {
		t.Fatal("expected Origin http://[::1]:8888 to be allowed for WS")
	}
	wsCross := httptest.NewRequest("GET", "http://[::1]:8888/ws/client/nas", nil)
	wsCross.Host = "[::1]:8888"
	wsCross.Header.Set("Origin", "http://localhost:8888")
	if srv.Policy.CheckWSOrigin(wsCross) {
		t.Fatal("expected cross-host Origin http://localhost:8888 on Host [::1]:8888 to be rejected")
	}
}

func TestHostPortCSPInjectionBlocked(t *testing.T) {
	badOrigins := []string{
		"https://portal.example.com:443; script-src 'unsafe-inline'",
		"https://portal.example.com:",
		"https://portal.example.com:abc",
		"https://portal.example.com:0",
		"https://portal.example.com:70000",
		"https://portal.example.com/path",
		"portal.example.com",
		"https://*.portal.example.com",
	}
	for _, o := range badOrigins {
		if _, err := NewSignalServer(ServerOptions{
			Addr:          "0.0.0.0:8888",
			SignalSecret:  testStrongSecret,
			PortalOrigin:  o,
			SandboxOrigin: "https://*.sandbox.example.net",
		}); err == nil {
			t.Fatalf("expected malformed/CSP-injecting PortalOrigin %q to fail closed", o)
		}
	}

	// Malformed Host header at runtime must be rejected with 421 and never reflected into CSP.
	srv, err := NewSignalServer(ServerOptions{
		Addr:          "0.0.0.0:8888",
		SignalSecret:  testStrongSecret,
		PortalOrigin:  "https://portal.example.com",
		SandboxOrigin: "https://*.sandbox.example.net",
		StaticFS:      testDistFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	for _, badHost := range []string{
		"portal.example.com:8443", // Unconfigured non-default port must be rejected
		"portal.example.com:80",   // Wrong default port for https:// origin must be rejected
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
	// and responses MUST emit Vary: Service-Worker, Sec-Fetch-Dest.
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
			SignalSecret:   testStrongSecret,
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

func TestSignal_TLSAndClientIPExtraction(t *testing.T) {
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

	// 5. Duplicate header lines must fail closed to remoteHost.
	reqDup := httptest.NewRequest("GET", "http://localhost/", nil)
	reqDup.RemoteAddr = "127.0.0.1:50002"
	reqDup.Header.Add("Fastly-Client-IP", "203.0.113.1")
	reqDup.Header.Add("Fastly-Client-IP", "198.51.100.2")
	reqDup.Header.Set("X-Forwarded-For", "198.51.100.99")
	if got := ExtractClientIP(reqDup, trusted, "Fastly-Client-IP"); got != "127.0.0.1" {
		t.Fatalf("duplicate client-ip-header did not fail closed: got %q", got)
	}

	// 6. Comma-separated list or malformed value must fail closed to remoteHost.
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
