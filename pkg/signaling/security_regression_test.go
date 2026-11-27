package signaling

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRegression_CrossOriginWSDoesNotLockoutAgentIP(t *testing.T) {
	srv, err := NewSignalServer(ServerOptions{
		Addr:         "127.0.0.1:8888",
		PortalDomain: "portal.example.com",
		SandboxHost:  "sandbox.example.net",
		SignalSecret: "top-secret",
		StaticFS:     testDistFS(),
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
		joinedAt:     now.Add(-3 * time.Minute), // > 2m maxLife, < 5m DefaultOnlineUnOfferTimeout
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
		Addr:         "0.0.0.0:8888",
		SignalSecret: "s",
		PortalDomain: "portal.example.com:443",
		SandboxHost:  "app.example.net:443",
		StaticFS:     testDistFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	req := httptest.NewRequest(http.MethodGet, "https://portal.example.com/", nil)
	req.Host = "portal.example.com" // Browsers omit :443!
	req.RemoteAddr = "127.0.0.1:44444"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when PortalDomain has explicit :443 and browser omits port, got %d", rec.Code)
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
		SignalSecret: "s",
		TLSAuto:      true,
		TLSCert:      "/tmp/a.pem",
		TLSKey:       "/tmp/a.key",
	}); err == nil {
		t.Fatal("expected conflicting -tls-auto + -tls-cert/-tls-key to fail closed")
	}
	if _, err := NewSignalServer(ServerOptions{
		Addr: "127.0.0.1:8888",
		NodeSecrets: map[string]string{
			"NAS": "secret-a",
			"nas": "secret-b",
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
	// Rotate IPs doing only 9 failures each (never triggering per-IP lockout).
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

func FuzzCanonicalizeHTTPPathAndHost(f *testing.F) {
	f.Add("/%2577s/agent/nas", "portal.example.com")
	f.Add("/__p2p_gw__/assets/bridge.js", "sbx-abc123.sandbox.example.net")
	f.Add("/%2f..%5csecret", "portal.example.com:443;script-src")
	dp, _ := NewDomainPolicy("0.0.0.0:8888", "portal.example.com", "sandbox.example.net", "", false, false, nil)
	f.Fuzz(func(t *testing.T, p, host string) {
		_, _ = CanonicalizeHTTPPath(p)
		_ = dp.ClassifyHost(host)
		_, _ = NormalizeAndValidateNodeID(host)
	})
}
