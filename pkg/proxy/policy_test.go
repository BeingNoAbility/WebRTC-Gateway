package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"

	"webrtc-gateway/pkg/protocol"
)

func TestProxyV7_SingleTargetPolicyAndUpstreamHost(t *testing.T) {
	// 1. Invalid -upstream-host values must fail closed.
	for _, badHost := range []string{
		"http://app.internal.example",
		"https://app.internal.example",
		"app.internal.example/path",
		"app.internal.example?x=1",
		"app.internal.example#frag",
		"user@app.internal.example",
		"app.internal.example:99999",
		"app.internal.example:0",
		"app.internal.example bad",
		"app.internal.example\r\nX-Inject: 1",
	} {
		if _, err := NewDestinationPolicy("http://127.0.0.1:8080", badHost); err == nil {
			t.Fatalf("expected NewDestinationPolicy to reject invalid upstream-host %q", badHost)
		}
	}

	pol, err := NewDestinationPolicy("https://app.example.com:8443/subpath", "")
	if err != nil {
		t.Fatal(err)
	}

	// 2. Cross-origin proxy prefix and path traversal smuggling must fail closed.
	for _, badTarget := range []string{
		"/__p2p_proxy__/https/169.254.169.254/latest/meta-data",
		"/__p2p_proxy__/http/127.0.0.1:8080/admin",
		"/__p2p_proxy__/https/app.example.com:8443/subpath/admin",
		"/..;/admin",
		"/a\\b",
		"/a%5cb",
	} {
		if _, err := pol.Resolve(badTarget, false); err == nil {
			t.Fatalf("expected Resolve to reject unsafe target %q", badTarget)
		}
	}

	// 3. Subpath escape must stay confined and %2F preserved.
	res, err := pol.Resolve("/../admin?k=1", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.URL.Path != "/subpath/admin" || res.URL.RawQuery != "k=1" {
		t.Fatalf("subpath escape or query corruption: %s", res.URL.String())
	}
	escRes, err := pol.Resolve("/items/a%2Fb", false)
	if err != nil || escRes.URL.EscapedPath() != "/subpath/items/a%2Fb" {
		t.Fatalf("expected %%2F preserved under subpath, got %v (%v)", escRes, err)
	}

	// 4. Virtual-host -upstream-host policy: EffectiveHost, EffectiveOrigin, and Location rewriting.
	polVHost, err := NewDestinationPolicy("http://127.0.0.1:8080/app", "App.Internal.Example:80")
	if err != nil {
		t.Fatal(err)
	}
	if polVHost.EffectiveHost() != "app.internal.example" {
		t.Fatalf("unexpected canonical EffectiveHost: %q", polVHost.EffectiveHost())
	}
	if polVHost.EffectiveOrigin() != "http://app.internal.example" {
		t.Fatalf("unexpected EffectiveOrigin: %q", polVHost.EffectiveOrigin())
	}
	if polVHost.TargetOrigin() != "http://127.0.0.1:8080" {
		t.Fatalf("unexpected TargetOrigin: %q", polVHost.TargetOrigin())
	}
	vhostDest, err := polVHost.Resolve("/login", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := polVHost.RewriteLocationHeader("http://app.internal.example/app/next?a=1#top", vhostDest); got != "/app/next?a=1#top" {
		t.Fatalf("expected upstream-host Location rewrite to relative path, got %q", got)
	}
	if got := polVHost.RewriteLocationHeader("http://127.0.0.1:8080/app/home", vhostDest); got != "/app/home" {
		t.Fatalf("expected target-origin Location rewrite to relative path, got %q", got)
	}
	if got := polVHost.RewriteLocationHeader("https://external.example/oauth", vhostDest); got != "https://external.example/oauth" {
		t.Fatalf("external redirect must remain unmodified, got %q", got)
	}

	// 5. End-to-end HTTPProxy + WSProxy + ProxyCookieJar with -upstream-host against real local server.
	var sawCrossOriginHit atomic.Bool
	var wsHost, wsOrigin, wsCookie string
	var wsMu sync.Mutex
	vhostSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/__p2p_proxy__/") {
			sawCrossOriginHit.Store(true)
			w.WriteHeader(200)
			return
		}
		if r.URL.Path == "/vhost-ws" {
			wsMu.Lock()
			wsHost = r.Host
			wsOrigin = r.Header.Get("Origin")
			wsCookie = r.Header.Get("Cookie")
			wsMu.Unlock()
			c, err := websocket.Accept(w, r, nil)
			if err == nil {
				_ = c.Close(websocket.StatusNormalClosure, "ok")
			}
			return
		}
		if r.URL.Path == "/vhost-login" {
			if r.Host != "app.internal.example" ||
				r.Header.Get("Origin") != "http://app.internal.example" ||
				r.Header.Get("Referer") != "http://app.internal.example/start" {
				http.Error(w, "header mismatch: host="+r.Host+" origin="+r.Header.Get("Origin")+" ref="+r.Header.Get("Referer"), 400)
				return
			}
			w.Header().Add("Set-Cookie", "vhost_sid=secret-vhost; Domain=app.internal.example; Path=/; HttpOnly")
			w.Header().Set("Location", "http://app.internal.example/dashboard")
			w.WriteHeader(http.StatusFound)
			return
		}
		if r.URL.Path == "/vhost-check" {
			_, _ = io.WriteString(w, r.Host+"|"+r.Header.Get("Cookie"))
			return
		}
		w.WriteHeader(404)
	}))
	defer vhostSrv.Close()

	polLive, err := NewDestinationPolicy(vhostSrv.URL, "app.internal.example")
	if err != nil {
		t.Fatal(err)
	}
	hpLive, _ := NewHTTPProxy(HTTPProxyOptions{Policy: polLive})
	wpLive, _ := NewWSProxy(WSProxyOptions{Policy: polLive})
	jarLive := NewProxyCookieJar()

	doHTTP := func(target string, hdrs []protocol.HeaderField) (*protocol.ResponseHeaders, string) {
		cm, sm := newTestMuxPair(t)
		go func() {
			st, err := sm.AcceptStream(context.Background())
			if err != nil {
				return
			}
			rh, _ := protocol.DecodeRequestHeaders(st.HeadersRaw())
			hpLive.HandleStream(st, rh, "198.51.100.9", jarLive, nil)
		}()
		tlv, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
			Kind: protocol.StreamKindHTTP, FetchCtx: protocol.FetchCtxSameOriginClient, Method: "GET", Target: target, Headers: hdrs,
		})
		s, err := cm.OpenHTTPStream(context.Background(), tlv, true)
		if err != nil {
			t.Fatal(err)
		}
		rb, err := s.ReadResponseHeaders(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		resp, _ := protocol.DecodeResponseHeaders(rb)
		body, _ := io.ReadAll(s)
		_ = s.Close()
		return resp, string(body)
	}

	// HTTPProxy returns 403 for cross-origin proxy prefix and never dials backend.
	respBad, _ := doHTTP("/__p2p_proxy__/http/169.254.169.254/latest/meta-data", nil)
	if respBad.Status != http.StatusForbidden || sawCrossOriginHit.Load() {
		t.Fatalf("expected 403 blocking /__p2p_proxy__/, got status=%d hit=%v", respBad.Status, sawCrossOriginHit.Load())
	}

	// HTTP virtual-host Host/Origin/Referer + Location rewrite + virtual-host Set-Cookie jar storage.
	respLogin, bodyLogin := doHTTP("/vhost-login", []protocol.HeaderField{
		{Name: "origin", Value: "https://sbx-test.sandbox.example"},
		{Name: "referer", Value: "https://sbx-test.sandbox.example/start"},
	})
	if respLogin.Status != http.StatusFound {
		t.Fatalf("expected 302 from /vhost-login, got %d body=%q", respLogin.Status, bodyLogin)
	}
	var gotLoc string
	for _, h := range respLogin.Headers {
		if h.Name == "location" {
			gotLoc = h.Value
		}
	}
	if gotLoc != "/dashboard" {
		t.Fatalf("expected Location rewritten to /dashboard, got %q", gotLoc)
	}

	// Subsequent HTTP request sends virtual-host HttpOnly cookie automatically.
	respCheck, bodyCheck := doHTTP("/vhost-check", nil)
	if respCheck.Status != http.StatusOK || !strings.Contains(bodyCheck, "app.internal.example|") || !strings.Contains(bodyCheck, "vhost_sid=secret-vhost") {
		t.Fatalf("virtual-host cookie/Host mismatch: status=%d body=%q", respCheck.Status, bodyCheck)
	}

	// Subsequent WS request also sends virtual-host Host, Origin, and HttpOnly cookie.
	cmWS, smWS := newTestMuxPair(t)
	cWSDC, sWSDC := newTestPipePair()
	srvOnMsg, srvOnClose, err := smWS.AttachDynamicWSDC(sWSDC)
	if err != nil {
		t.Fatal(err)
	}
	defer srvOnClose()
	sWSDC.start(srvOnMsg)
	doneWS := make(chan struct{})
	go func() {
		defer close(doneWS)
		st, err := smWS.AcceptStream(context.Background())
		if err != nil {
			return
		}
		rh, _ := protocol.DecodeRequestHeaders(st.HeadersRaw())
		wpLive.HandleStream(st, rh, "198.51.100.9", jarLive, nil)
	}()
	wsTLV, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindWS, FetchCtx: protocol.FetchCtxSameOriginClient, Method: "GET", Target: "/vhost-ws",
		Headers: []protocol.HeaderField{
			{Name: "origin", Value: "https://sbx-test.sandbox.example"},
		},
	})
	csWS, cliOnMsg, cliOnClose, err := cmWS.OpenWSStream(context.Background(), cWSDC, wsTLV)
	if err != nil {
		t.Fatal(err)
	}
	defer cliOnClose()
	cWSDC.start(cliOnMsg)
	rbWS, err := csWS.ReadResponseHeaders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	respWS, _ := protocol.DecodeResponseHeaders(rbWS)
	if respWS.Status != http.StatusSwitchingProtocols {
		t.Fatalf("expected 101 for /vhost-ws, got %d", respWS.Status)
	}
	<-doneWS
	wsMu.Lock()
	if wsHost != "app.internal.example" || wsOrigin != "http://app.internal.example" || !strings.Contains(wsCookie, "vhost_sid=secret-vhost") {
		t.Fatalf("WS -upstream-host mismatch: host=%q origin=%q cookie=%q", wsHost, wsOrigin, wsCookie)
	}
	wsMu.Unlock()

	// 6. CapabilityScope checked after Resolve blocks %2e%2e and query traversal bypasses,
	//    while allowing legitimate /report?page=2.
	pol2, _ := NewDestinationPolicy("http://127.0.0.1:8080", "")
	scope := &CapabilityScope{PathPrefix: "/public", Readonly: true}
	for _, bypass := range []string{"/public/%2e%2e/admin/secrets", "/admin/secrets?x=/../../public"} {
		d, err := pol2.Resolve(bypass, false)
		if err != nil {
			continue
		}
		if scope.AllowsDestination(protocol.StreamKindHTTP, "GET", d) {
			t.Fatalf("capability bypass allowed %q -> %s", bypass, d.URL.Path)
		}
	}
	scopeRep := &CapabilityScope{PathPrefix: "/report", Readonly: true}
	dRep, err := pol2.Resolve("/report?page=2", false)
	if err != nil || !scopeRep.AllowsDestination(protocol.StreamKindHTTP, "GET", dRep) {
		t.Fatalf("legitimate /report?page=2 falsely rejected")
	}
}

func TestFetchContextFailClosed(t *testing.T) {
	pol, err := NewDestinationPolicy("https://192.0.2.50:8443/subapp", "app.internal.example")
	if err != nil {
		t.Fatal(err)
	}
	jar := NewProxyCookieJar()
	dest, err := pol.Resolve("/docs/report", false)
	if err != nil {
		t.Fatal(err)
	}
	cookieURL := pol.CookieURL(dest.URL)
	jar.SaveResponseCookies(cookieURL, []string{
		"strict_sid=secret; Path=/; HttpOnly; Secure; SameSite=Strict",
		"lax_sid=laxval; Path=/; HttpOnly; Secure; SameSite=Lax",
	})

	// 1. Fail-Closed: FetchCtx=0 (SameOriginClient unset) treats request as cross-site,
	//    blocks SameSite=Strict/Lax on POST, and never rewrites external Origin/Referer.
	csHdrs := pol.SanitizeRequestHeaders(
		[]protocol.HeaderField{
			{Name: "origin", Value: "https://evil.example"},
			{Name: "referer", Value: "https://evil.example/csrf"},
		},
		0, "POST", dest, "198.51.100.1", jar, false, false,
	)
	if csHdrs.Get("Origin") == pol.EffectiveOrigin() {
		t.Fatalf("cross-site Origin rewritten to EffectiveOrigin: %q", csHdrs.Get("Origin"))
	}
	if csHdrs.Get("Referer") != "" {
		t.Fatalf("cross-site Referer leaked/rewritten: %q", csHdrs.Get("Referer"))
	}
	if strings.Contains(csHdrs.Get("Cookie"), "strict_sid") || strings.Contains(csHdrs.Get("Cookie"), "lax_sid") {
		t.Fatalf("SameSite cookies leaked when FetchCtx=0: %q", csHdrs.Get("Cookie"))
	}

	// 2. Origin: null must never be rewritten to EffectiveOrigin even when SameOriginClient is set.
	nullOriginHdrs := pol.SanitizeRequestHeaders(
		[]protocol.HeaderField{{Name: "origin", Value: "null"}},
		protocol.FetchCtxSameOriginClient, "POST", dest, "198.51.100.1", jar, false, false,
	)
	if nullOriginHdrs.Get("Origin") != "null" {
		t.Fatalf("Origin: null was rewritten to %q", nullOriginHdrs.Get("Origin"))
	}

	// 3. FetchCtxCredentialsOmit must omit Cookie header completely.
	omitHdrs := pol.SanitizeRequestHeaders(
		nil,
		protocol.FetchCtxSameOriginClient|protocol.FetchCtxCredentialsOmit,
		"GET", dest, "198.51.100.1", jar, false, false,
	)
	if omitHdrs.Get("Cookie") != "" {
		t.Fatalf("FetchCtxCredentialsOmit still attached Cookie: %q", omitHdrs.Get("Cookie"))
	}

	// 4. Readonly capability strips X-HTTP-Method-Override / X-Method-Override.
	roHdrs := pol.SanitizeRequestHeaders(
		[]protocol.HeaderField{
			{Name: "x-http-method-override", Value: "DELETE"},
			{Name: "x-method-override", Value: "POST"},
		},
		protocol.FetchCtxSameOriginClient, "GET", dest, "198.51.100.1", nil, false, true,
	)
	if roHdrs.Get("X-Http-Method-Override") != "" || roHdrs.Get("X-Method-Override") != "" {
		t.Fatalf("readonly mode failed to strip method override headers: %+v", roHdrs)
	}

	// 5. TargetSubpath + CapabilityScope.PathPrefix composition.
	scope := &CapabilityScope{PathPrefix: "/docs", Readonly: true}
	if !scope.AllowsDestination(protocol.StreamKindHTTP, "GET", dest) {
		t.Fatalf("TargetSubpath (/subapp) + PathPrefix (/docs) falsely rejected %s", dest.URL.Path)
	}
	adminDest, _ := pol.Resolve("/admin", false)
	if scope.AllowsDestination(protocol.StreamKindHTTP, "GET", adminDest) {
		t.Fatalf("TargetSubpath + PathPrefix allowed /admin: %s", adminDest.URL.Path)
	}

	// 6. RewriteLocationHeader collapses leading `//` on same-origin target to block open redirect.
	if got := pol.RewriteLocationHeader("https://app.internal.example//evil.example/steal?x=1", dest); got != "/evil.example/steal?x=1" {
		t.Fatalf("open redirect via same-origin leading // not normalized: got %q", got)
	}

	// 7. HTTPS target with -upstream-host defaults TLS ServerName (SNI) to UpstreamHostName.
	if sni := pol.DefaultTLSServerName(""); sni != "app.internal.example" {
		t.Fatalf("expected default TLS SNI app.internal.example, got %q", sni)
	}
	if sni := pol.DefaultTLSServerName("custom.sni.example"); sni != "custom.sni.example" {
		t.Fatalf("explicit SNI not honored: got %q", sni)
	}
}
