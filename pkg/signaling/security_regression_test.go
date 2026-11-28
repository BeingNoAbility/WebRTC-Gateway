package signaling

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func addWSUpgradeHeaders(req *http.Request) {
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
}

func TestRegression_CrossOriginWSDoesNotLockoutAgentIP(t *testing.T) {
	srv, err := NewSignalServer(ServerOptions{
		Addr:          "127.0.0.1:8888",
		PortalOrigin:  "https://portal.example.com",
		SandboxOrigin: "https://sandbox.example.net",
		SignalSecret:  testStrongSecret,
		StaticFS:      testDistFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	for i := 0; i < 15; i++ {
		req := httptest.NewRequest(http.MethodGet, "https://portal.example.com/ws/agent/any", nil)
		req.Host = "portal.example.com"
		req.RemoteAddr = "203.0.113.55:12345"
		req.Header.Set("Origin", "https://evil.example.org")
		addWSUpgradeHeaders(req)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for cross-origin /ws/agent probe, got %d", rec.Code)
		}
	}
	if srv.Limiter.IsAuthLocked("203.0.113.55") {
		t.Fatal("cross-origin browser probes locked out victim NAT IP!")
	}
}

func TestRegression_H15c_CrossOriginAndNonUpgradeDoNotBurnUpgradeQuota(t *testing.T) {
	srv, err := NewSignalServer(ServerOptions{
		Addr:          "127.0.0.1:8888",
		PortalOrigin:  "https://portal.example.com",
		SandboxOrigin: "https://*.sandbox.example.net",
		SignalSecret:  testStrongSecret,
		StaticFS:      testDistFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	// 200 cross-origin no-cors fetches + 200 non-upgrade plain HTTP GETs from the same NAT IP
	// MUST NOT exhaust DefaultMaxIPUpgrades (120/min) for legitimate Portal users on that IP.
	for i := 0; i < 200; i++ {
		req := httptest.NewRequest(http.MethodGet, "https://portal.example.com/ws/client/nas", nil)
		req.Host = "portal.example.com"
		req.RemoteAddr = "203.0.113.77:54321"
		req.Header.Set("Origin", "https://attacker.example.org")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 on cross-origin no-cors fetch, got %d", rec.Code)
		}

		plain := httptest.NewRequest(http.MethodGet, "https://portal.example.com/ws/client/nas", nil)
		plain.Host = "portal.example.com"
		plain.RemoteAddr = "203.0.113.77:54322"
		plain.Header.Set("Origin", "https://portal.example.com")
		rec2 := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec2, plain)
		if rec2.Code != http.StatusUpgradeRequired {
			t.Fatalf("expected 426 on non-upgrade GET /ws/client/nas, got %d", rec2.Code)
		}
	}
	if !srv.Limiter.AllowUpgrade("203.0.113.77") {
		t.Fatal("cross-origin / non-upgrade requests exhausted legitimate victim NAT AllowUpgrade quota")
	}
}

func TestRegression_H14_FailedWSUpgradeSetsConnectionCloseOnBody(t *testing.T) {
	srv, err := NewSignalServer(ServerOptions{
		Addr:          "127.0.0.1:8888",
		PortalOrigin:  "https://portal.example.com",
		SandboxOrigin: "https://*.sandbox.example.net",
		SignalSecret:  testStrongSecret,
		StaticFS:      testDistFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "https://portal.example.com/ws/client/nas", strings.NewReader("unexpected-body"))
	req.Host = "portal.example.com"
	req.Header.Set("Origin", "https://portal.example.com")
	addWSUpgradeHeaders(req)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for body-bearing WS upgrade, got %d", rec.Code)
	}
	if rec.Header().Get("Connection") != "close" {
		t.Fatalf("expected Connection: close for body-bearing failed WS upgrade, got %q", rec.Header().Get("Connection"))
	}
}

func TestRegression_H15a_NoGlobalAgentKillSwitchAndAlreadyLockedDoesNotPumpGlobalRing(t *testing.T) {
	lim := NewIPRateLimiter(64, 120)
	// 100 distinct attacker IPs x 10 failures = 1000 failures (> old 600 global kill-switch)
	// MUST NOT lock out a clean Agent IP!
	for i := 0; i < 100; i++ {
		ip := fmt.Sprintf("198.51.100.%d", i+1)
		for f := 0; f < authLockoutThreshold; f++ {
			lim.RecordFailedAuth(ip)
		}
	}
	if lim.IsAuthLocked("203.0.113.200") {
		t.Fatal("distributed auth failures triggered global kill-switch against legitimate Agent IP")
	}

	// Verify AuthLockoutTable (used by Agent VisitorAuthLimiter) ignores already-locked IPs for globalRing.
	tab := NewAuthLockoutTable(AuthLockoutConfig{
		MaxFailures:       3,
		LockoutDuration:   5 * time.Minute,
		MaxPendingKeys:    32,
		MaxLockedKeys:     32,
		GlobalMaxFailures: 10,
		GlobalWindow:      time.Minute,
	})
	now := time.Now()
	for i := 0; i < 50; i++ {
		tab.RecordFailure("198.51.100.1", now)
	}
	if allowed, _ := tab.Allow("203.0.113.201", now); !allowed {
		t.Fatal("single already-locked IP pumped globalRing and DoSed another IP")
	}
}

func TestRegression_H15b_PerNodeSecretResetIsolatedByNodeID(t *testing.T) {
	srv, err := NewSignalServer(ServerOptions{
		Addr: "127.0.0.1:0",
		NodeSecrets: map[string]string{
			"node-a": "secret-for-node-a-0123456789",
			"node-b": "secret-for-node-b-0123456789",
		},
		StaticFS: testDistFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	ip := "198.51.100.42"
	// 9 failures on node-b.
	for i := 0; i < authLockoutThreshold-1; i++ {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/ws/agent/node-b", nil)
		req.Host = "127.0.0.1"
		req.RemoteAddr = ip + ":12345"
		req.Header.Set("Authorization", "Bearer wrong-secret-0123456789")
		addWSUpgradeHeaders(req)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d expected 401, got %d", i, rec.Code)
		}
	}
	// Successful auth on node-a from the SAME IP (resets (ip, node-a) only!).
	srv.Limiter.ResetFailedAuthScoped(ip, "node-a")

	// 10th failure on node-b MUST lock (ip, node-b), proving node-a did not reset node-b!
	req10 := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/ws/agent/node-b", nil)
	req10.Host = "127.0.0.1"
	req10.RemoteAddr = ip + ":12346"
	req10.Header.Set("Authorization", "Bearer wrong-secret-0123456789")
	addWSUpgradeHeaders(req10)
	rec10 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec10, req10)
	if rec10.Code != http.StatusUnauthorized {
		t.Fatalf("10th attempt expected 401, got %d", rec10.Code)
	}
	if !srv.Limiter.IsAuthLockedScoped(ip, "node-b") {
		t.Fatal("node-b was NOT locked out after 10 failures (cross-node reset leaked)")
	}
	if srv.Limiter.IsAuthLockedScoped(ip, "node-a") {
		t.Fatal("node-b lockout leaked onto unrelated node-a")
	}
}

func TestRegression_H13_RegisteredStrictlyPrecedesOfferUnderWaitingClient(t *testing.T) {
	dir := t.TempDir()
	srv, err := NewSignalServer(ServerOptions{
		Addr:          "127.0.0.1:0",
		SignalSecret:  testStrongSecret,
		BoundKeysFile: filepath.Join(dir, "bound.json"),
		StaticFS:      testDistFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Connect waiting client BEFORE agent registers; immediately send Offer upon PeerStatus(online=true).
	clientConn, _, err := websocket.Dial(ctx, wsBase+"/ws/client/race-node", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.CloseNow()
	_, _, err = clientConn.Read(ctx) // JoinedDTO (online=false)
	if err != nil {
		t.Fatal(err)
	}

	offerSent := make(chan struct{})
	go func() {
		defer close(offerSent)
		_, raw, rerr := clientConn.Read(ctx)
		if rerr != nil {
			return
		}
		var ps PeerStatusDTO
		if json.Unmarshal(raw, &ps) == nil && ps.Type == TypePeerStatus && ps.Online {
			offerMsg, _ := json.Marshal(map[string]any{
				"type":  TypeOffer,
				"desc":  sampleOfferDescriptor(),
				"nonce": "0123456789abcdef012345",
			})
			_ = clientConn.Write(ctx, websocket.MessageText, offerMsg)
		}
	}()

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	agConn, _, err := websocket.Dial(ctx, wsBase+"/ws/agent/race-node", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + testStrongSecret}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer agConn.CloseNow()

	_, chRaw, err := agConn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ch ChallengeDTO
	_ = json.Unmarshal(chRaw, &ch)
	sig := ed25519.Sign(priv, RegisterSignPayload("race-node", ch.Nonce))
	reg, _ := json.Marshal(map[string]any{
		"type": TypeRegister,
		"pk":   base64.RawURLEncoding.EncodeToString(pub),
		"sig":  base64.RawURLEncoding.EncodeToString(sig),
	})
	if err := agConn.Write(ctx, websocket.MessageText, reg); err != nil {
		t.Fatal(err)
	}

	// First message MUST be `registered`, second message MUST be `offer` (never inverted!).
	_, firstRaw, err := agConn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var env1 InboundEnvelope
	_ = json.Unmarshal(firstRaw, &env1)
	if env1.Type != TypeRegistered {
		t.Fatalf("H-13 regression: expected first message after register to be %q, got %q (%s)", TypeRegistered, env1.Type, string(firstRaw))
	}

	<-offerSent
	_, secondRaw, err := agConn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var env2 InboundEnvelope
	_ = json.Unmarshal(secondRaw, &env2)
	if env2.Type != TypeOffer {
		t.Fatalf("expected second message to be %q, got %q", TypeOffer, env2.Type)
	}
}

func TestRegression_M18_SessionCloseDeliversStatusCodeBeforeCancel(t *testing.T) {
	srv, err := NewSignalServer(ServerOptions{
		Addr:         "127.0.0.1:0",
		SignalSecret: testStrongSecret,
		StaticFS:     testDistFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	ag1 := registerTestAgent(t, wsBase+"/ws/agent/replace-node", testStrongSecret, pub, priv, "replace-node", false)
	defer ag1.CloseNow()

	// Replace ag1 with a second connection using the same Ed25519 key.
	ag2 := registerTestAgent(t, wsBase+"/ws/agent/replace-node", testStrongSecret, pub, priv, "replace-node", false)
	defer ag2.CloseNow()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	gotStatus := websocket.StatusCode(0)
	for i := 0; i < 4; i++ {
		_, _, err := ag1.Read(ctx)
		if err != nil {
			gotStatus = websocket.CloseStatus(err)
			break
		}
	}
	if gotStatus != websocket.StatusCode(4001) {
		t.Fatalf("M-18 regression: replaced agent expected close status 4001, got %d", gotStatus)
	}
}

func TestRegression_M19_PerIPWaitingCapAndCandidateMinuteLimit(t *testing.T) {
	b := NewSignalBroker()
	defer b.CloseAll()

	for i := 0; i < MaxWaitingPerIPPerNode; i++ {
		s := &Session{ID: fmt.Sprintf("w-%d", i), Role: RoleClient, NodeID: "offline-node", ClientIP: "198.51.100.10"}
		if _, _, _, err := b.JoinClient(s); err != nil {
			t.Fatalf("waiting client %d failed unexpectedly: %v", i, err)
		}
	}
	extra := &Session{ID: "w-overflow", Role: RoleClient, NodeID: "offline-node", ClientIP: "198.51.100.10"}
	if _, _, _, err := b.JoinClient(extra); err != ErrNodeWaitingQueueFull {
		t.Fatalf("M-19 regression: expected per-IP waiting cap ErrNodeWaitingQueueFull, got %v", err)
	}
	// A different IP can still wait on the same offline node.
	otherIP := &Session{ID: "w-other", Role: RoleClient, NodeID: "offline-node", ClientIP: "198.51.100.11"}
	if _, _, _, err := b.JoinClient(otherIP); err != nil {
		t.Fatalf("other IP should still be allowed to wait: %v", err)
	}

	// Candidate per-minute burst limit (60/min).
	sess := &Session{}
	now := time.Now()
	sess.RecordOffer(now)
	for i := 0; i < MaxCandidatesPerMinute; i++ {
		if ok, _ := sess.AllowCandidate(now); !ok {
			t.Fatalf("candidate %d within minute quota rejected", i)
		}
	}
	if ok, pre := sess.AllowCandidate(now); ok || pre {
		t.Fatalf("M-19 regression: 61st candidate in 1 minute must be rate-limited (ok=%v pre=%v)", ok, pre)
	}
	// After 61s, window slides and allows again (up to lifetime cap 128).
	if ok, _ := sess.AllowCandidate(now.Add(61 * time.Second)); !ok {
		t.Fatal("candidate after minute window elapsed should be allowed")
	}
}

func TestRegression_M20_SWRequiresWorkerHeaderAndPreRenderedETag(t *testing.T) {
	srv, err := NewSignalServer(ServerOptions{
		Addr:          "0.0.0.0:8888",
		PortalOrigin:  "https://portal.example.com",
		SandboxOrigin: "https://*.sandbox.example.net",
		SignalSecret:  testStrongSecret,
		StaticFS:      testDistFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	// Plain fetch / <script> read of sw.js MUST fail with 403 and emit CORP: same-origin.
	reqBad := httptest.NewRequest(http.MethodGet, "https://sbx-1.sandbox.example.net/__p2p_gw__/sw.js", nil)
	reqBad.Host = "sbx-1.sandbox.example.net"
	reqBad.Header.Set("Sec-Fetch-Dest", "script")
	recBad := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recBad, reqBad)
	if recBad.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-SW fetch of sw.js, got %d", recBad.Code)
	}
	if recBad.Header().Get("Cross-Origin-Resource-Policy") != "same-origin" {
		t.Fatalf("expected CORP: same-origin on sw.js, got %q", recBad.Header().Get("Cross-Origin-Resource-Policy"))
	}

	// Valid Service Worker fetch succeeds with strong SHA-256 ETag and supports 304 Not Modified.
	reqOK := httptest.NewRequest(http.MethodGet, "https://sbx-1.sandbox.example.net/__p2p_gw__/sw.js", nil)
	reqOK.Host = "sbx-1.sandbox.example.net"
	reqOK.Header.Set("Service-Worker", "script")
	recOK := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recOK, reqOK)
	etag := recOK.Header().Get("ETag")
	if recOK.Code != http.StatusOK || etag == "" {
		t.Fatalf("expected 200 with ETag on sw.js, got code=%d etag=%q", recOK.Code, etag)
	}

	req304 := httptest.NewRequest(http.MethodGet, "https://sbx-1.sandbox.example.net/__p2p_gw__/sw.js", nil)
	req304.Host = "sbx-1.sandbox.example.net"
	req304.Header.Set("Service-Worker", "script")
	req304.Header.Set("If-None-Match", etag)
	rec304 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec304, req304)
	if rec304.Code != http.StatusNotModified {
		t.Fatalf("expected 304 Not Modified for matching ETag, got %d", rec304.Code)
	}
}

func TestRegression_M22_TokenBucketFailClosedOnActiveSaturation(t *testing.T) {
	lim := NewIPRateLimiter(64, 120)
	// Use a small burst so exhausting all 16,384 buckets to actively rate-limited (< 1.0 tokens) is fast.
	lim.maxUpgrPer60s = 1
	for i := 0; i < maxTrackedIPs; i++ {
		ip := fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xff, (i>>8)&0xff, i&0xff)
		if !lim.AllowUpgrade(ip) {
			t.Fatalf("bucket %d should succeed before saturation", i)
		}
	}
	// All 16,384 tracked buckets are now genuinely exhausted (< 1.0 tokens); unseen IP MUST fail closed.
	if lim.AllowUpgrade("198.51.100.254") {
		t.Fatal("M-22 regression: AllowUpgrade evicted an actively rate-limited bucket instead of failing closed")
	}
}

func TestRegression_P0_2_SingleProbeSprayDoesNotLockOutLegitVisitors(t *testing.T) {
	lim := NewIPRateLimiter(64, 120)
	// First throttle a few real attacker IPs (0 tokens left) so they sit at the LRU tail.
	for a := 0; a < 4; a++ {
		attacker := fmt.Sprintf("203.0.113.%d", a+1)
		for j := 0; j < DefaultMaxIPUpgrades+2; j++ {
			_ = lim.AllowUpgrade(attacker)
		}
		if lim.AllowUpgrade(attacker) {
			t.Fatalf("attacker %s should be rate-limited", attacker)
		}
	}
	// Fill remaining slots with 1-probe-per-key IPv6 /64 spray (each still holds 119/120 tokens).
	for i := 0; i < maxTrackedIPs; i++ {
		ip := fmt.Sprintf("2001:db8:%x:%x::1", i>>8, i&0xff)
		if !lim.AllowUpgrade(ip) {
			t.Fatalf("single-probe key %d unexpectedly rejected", i)
		}
	}
	// Unseen legitimate visitor MUST still pass (healthy 119/120 buckets evicted; throttled attackers preserved).
	if !lim.AllowUpgrade("198.51.100.42") {
		t.Fatal("P0-2 regression: single-probe IPv6 spray globally locked out legitimate unseen visitor")
	}
	// Previously throttled attackers must STILL be rate-limited (never evicted by healthy-bucket probe).
	if lim.AllowUpgrade("203.0.113.1") {
		t.Fatal("P0-2 regression: throttled attacker bucket was evicted")
	}
}

func TestRegression_ClientConnFloodDoesNotStarveAgents(t *testing.T) {
	lim := NewIPRateLimiter(64, 120)
	lim.maxGlobalClientConns = 4
	for i := 0; i < 4; i++ {
		if !lim.AcquireConn("198.51.100.1") {
			t.Fatalf("client conn %d should succeed", i)
		}
	}
	if lim.AcquireConn("198.51.100.2") {
		t.Fatal("client pool should be full")
	}
	// Authenticated Agent must still acquire from its isolated pool!
	if !lim.AcquireAgentConn("198.51.100.2") {
		t.Fatal("client connection flood starved authenticated Agent pool")
	}
	lim.ReleaseAgentConn("198.51.100.2")
}

func TestRegression_UnOfferedOnlineClientSurvivesPast2MinLifetime(t *testing.T) {
	b := NewSignalBroker()
	defer b.CloseAll()

	now := time.Now()
	ag := &Session{ID: "ag-1", Role: RoleAgent, NodeID: "nas"}
	cWaitPass := &Session{
		ID:           "c-typing-pass",
		Role:         RoleClient,
		NodeID:       "nas",
		joinedAt:     now.Add(-3 * time.Minute),
		lastActivity: now.Add(-3 * time.Minute),
		hasSentOffer: false,
		outCh:        make(chan []byte, 4),
	}
	cWaitPass.ctx, cWaitPass.cancel = context.WithCancel(context.Background())

	b.mu.Lock()
	b.agents["nas"] = ag
	b.clients[cWaitPass.ID] = cWaitPass
	b.nodeClients["nas"] = map[string]*Session{cWaitPass.ID: cWaitPass}
	b.mu.Unlock()

	b.reapTimedOutClients(now)

	b.mu.Lock()
	_, alive := b.clients[cWaitPass.ID]
	b.mu.Unlock()
	if !alive {
		t.Fatal("online client typing password for 3m was prematurely reaped by 2m post-offer lifetime")
	}
}

func TestRegression_ExplicitDefaultPort443MatchesBrowserHost(t *testing.T) {
	srv, err := NewSignalServer(ServerOptions{
		Addr:          "0.0.0.0:8888",
		SignalSecret:  testStrongSecret,
		PortalOrigin:  "https://portal.example.com:443",
		SandboxOrigin: "https://app.example.net:443",
		StaticFS:      testDistFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "https://portal.example.com/", nil)
	req.Host = "portal.example.com" // Browsers omit :443!
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when PortalOrigin has explicit :443 and browser omits port, got %d", rec.Code)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if strings.Contains(csp, ":443") {
		t.Fatalf("expected canonical omission of :443 in CSP frame-src, got %q", csp)
	}
}

func TestRegression_RollbackOfferPreservesPriorPendingTimer(t *testing.T) {
	s := &Session{}
	t0 := time.Now()
	ok1, prevHas1, prevPend1 := s.RecordOffer(t0)
	if !ok1 || prevHas1 || !prevPend1.IsZero() {
		t.Fatalf("unexpected first RecordOffer: %v %v %v", ok1, prevHas1, prevPend1)
	}
	ok2, prevHas2, prevPend2 := s.RecordOffer(t0.Add(20 * time.Second))
	if !ok2 || !prevHas2 || !prevPend2.Equal(t0) {
		t.Fatalf("unexpected second RecordOffer: %v %v %v", ok2, prevHas2, prevPend2)
	}
	s.RollbackOffer(prevHas2, prevPend2)
	if !s.OfferPendingSince().Equal(t0) {
		t.Fatalf("RollbackOffer cleared earlier pending offer timer: got %v, want %v", s.OfferPendingSince(), t0)
	}
}

func TestRegression_ConflictingTLSAndDuplicateNodeSecretsRejected(t *testing.T) {
	if _, err := NewSignalServer(ServerOptions{
		Addr:         "127.0.0.1:8888",
		SignalSecret: testStrongSecret,
		TLSAuto:      true,
		TLSCert:      "/tmp/a.pem",
		TLSKey:       "/tmp/a.key",
	}); err == nil {
		t.Fatal("expected conflicting -tls-auto + -tls-cert/-tls-key to fail closed")
	}
	if _, err := NewSignalServer(ServerOptions{
		Addr: "127.0.0.1:8888",
		NodeSecrets: map[string]string{
			"NAS": "secret-a-0123456789",
			"nas": "secret-b-0123456789",
		},
	}); err == nil {
		t.Fatal("expected duplicate normalized node_secrets keys to fail closed")
	}
}

func TestRegression_AuthLockoutGlobalRingStops9AttemptEvictionBypass(t *testing.T) {
	tab := NewAuthLockoutTable(AuthLockoutConfig{
		MaxFailures:       10,
		LockoutDuration:   5 * time.Minute,
		MaxPendingKeys:    16,
		MaxLockedKeys:     16,
		GlobalMaxFailures: 30,
		GlobalWindow:      time.Minute,
	})
	now := time.Now()
	for i := 0; i < 4; i++ {
		ip := "2001:db8:" + string(rune('a'+i)) + "::1"
		for j := 0; j < 9; j++ {
			tab.RecordFailure(ip, now)
		}
	}
	if allowed, retry := tab.Allow("2001:db8:ffff::1", now); allowed || retry <= 0 {
		t.Fatalf("expected global sliding window to block distributed 9-attempt brute force, got allowed=%v retry=%v", allowed, retry)
	}
	if allowed, _ := tab.Allow("2001:db8:ffff::1", now.Add(time.Minute+time.Second)); !allowed {
		t.Fatal("expected global sliding window to recover after window expires")
	}
}

func TestRegression_P0_PerNodeFakeNodeIDAndSaturationDoNotDoSLegitAgent(t *testing.T) {
	srv, err := NewSignalServer(ServerOptions{
		PortalOrigin:  "https://portal.example",
		SandboxOrigin: "https://*.sbx.example",
		NodeSecrets: map[string]string{
			"legit-node": "super-secret-legit-token-1234567890",
		},
	})
	if err != nil {
		t.Fatalf("NewSignalServer: %v", err)
	}
	defer srv.Shutdown()
	handler := srv.Handler()

	sendAgentReq := func(nodeID, ip, auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/ws/agent/"+nodeID, nil)
		req.Host = "portal.example"
		req.RemoteAddr = ip + ":54321"
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}

	// 1. Unknown/fake nodeIDs from one IP MUST share a single pure-IP lockout bucket (zero slot amplification).
	for i := 0; i < authLockoutThreshold; i++ {
		rr := sendAgentReq(fmt.Sprintf("nonexistent-%d", i), "203.0.113.10", "Bearer wrong")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("fake node attempt %d: got %d, want 401", i, rr.Code)
		}
	}
	if rr := sendAgentReq("nonexistent-another", "203.0.113.10", "Bearer wrong"); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("expected attacker IP to be locked across all fake nodeIDs, got %d", rr.Code)
	}

	// 2. Even if distributed attackers saturate all maxAuthFailedIPs lockout slots, a clean-IP legitimate
	//    Agent with a valid >= 128-bit Bearer secret MUST NOT be globally locked out (429).
	for i := 0; i < maxAuthFailedIPs+16; i++ {
		ip := fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xff, (i>>8)&0xff, i&0xff)
		for f := 0; f < authLockoutThreshold; f++ {
			srv.Limiter.RecordFailedAuthScoped(ip, "")
		}
	}
	rrLegit := sendAgentReq("legit-node", "198.51.100.99", "Bearer super-secret-legit-token-1234567890")
	// httptest.ResponseRecorder does not implement http.Hijacker so websocket.Accept returns 500/426,
	// proving it passed BOTH auth lockout AND AllowAgentUpgrade without 401 or 429!
	if rrLegit.Code == http.StatusTooManyRequests || rrLegit.Code == http.StatusUnauthorized {
		t.Fatalf("P0-1 regression: legitimate agent blocked under lockout saturation: HTTP %d (%s)", rrLegit.Code, rrLegit.Body.String())
	}
}

func TestRegression_P1_ClientUpgradeFloodNeverStarvesAgents(t *testing.T) {
	srv, err := NewSignalServer(ServerOptions{
		PortalOrigin:  "https://portal.example",
		SandboxOrigin: "https://*.sbx.example",
		SignalSecret:  "cluster-secret-1234567890",
	})
	if err != nil {
		t.Fatalf("NewSignalServer: %v", err)
	}
	defer srv.Shutdown()
	handler := srv.Handler()

	// Case A: Same NAT IP exhausts client upgrade bucket.
	for i := 0; i < DefaultMaxIPUpgrades+5; i++ {
		_ = srv.Limiter.AllowUpgrade("198.51.100.5")
	}
	if srv.Limiter.AllowUpgrade("198.51.100.5") {
		t.Fatal("client upgrade bucket should be exhausted")
	}
	// Case B: Distributed flood saturates all maxTrackedIPs client upgrade buckets.
	for i := 0; i < maxTrackedIPs; i++ {
		ip := fmt.Sprintf("2001:db8:beef:%x::1", i)
		_ = srv.Limiter.AllowUpgrade(ip)
		_ = srv.Limiter.AllowUpgrade(ip)
	}

	// Legitimate Agent behind the SAME NAT IP and from a clean IP must still pass upgrade limiter!
	for _, ip := range []string{"198.51.100.5", "203.0.113.77"} {
		req := httptest.NewRequest(http.MethodGet, "/ws/agent/mynode", nil)
		req.Host = "portal.example"
		req.RemoteAddr = ip + ":50000"
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		req.Header.Set("Authorization", "Bearer cluster-secret-1234567890")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code == http.StatusTooManyRequests || rr.Code == http.StatusUnauthorized {
			t.Fatalf("P1-1 regression: legitimate agent (%s) starved by client upgrade flood: HTTP %d", ip, rr.Code)
		}
	}
}

func TestRegression_P2_AssetCacheControlRequiresContentHash(t *testing.T) {
	if got := assetCacheControl("/__p2p_gw__/portal.0123456789.js"); !strings.Contains(got, "immutable") {
		t.Fatalf("expected hashed asset immutable, got %q", got)
	}
	for _, unhashed := range []string{"/__p2p_gw__/portal.min.js", "/__p2p_gw__/app.v1.2.js", "/__p2p_gw__/portal.js"} {
		if got := assetCacheControl(unhashed); strings.Contains(got, "immutable") {
			t.Fatalf("unhashed asset %q must not be cached immutable, got %q", unhashed, got)
		}
	}
}

func FuzzCanonicalizeHTTPPathAndHost(f *testing.F) {
	f.Add("/%2577s/agent/nas", "portal.example.com")
	f.Add("/__p2p_gw__/assets/bridge.js", "sbx-abc123.sandbox.example.net")
	f.Add("/%2f..%5csecret", "portal.example.com:443;script-src")
	dp, _ := NewDomainPolicy("0.0.0.0:8888", "https://portal.example.com", "https://*.sandbox.example.net", false, false, false, false)
	f.Fuzz(func(t *testing.T, p, host string) {
		_, _ = CanonicalizeHTTPPath(p)
		_ = dp.ClassifyHost(host)
		_, _ = NormalizeAndValidateNodeID(host)
	})
}
