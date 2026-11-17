package main

import (
	"context"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"webrtc-gateway/pkg/signaling"
)

func TestSignalBroker_AgentRegistrationAndAntiHijack(t *testing.T) {
	broker := NewSignalBroker("")

	s1 := &Session{ID: "agent-sess-1", Role: "agent"}
	s2Rogue := &Session{ID: "agent-sess-2-rogue", Role: "agent"}
	s3Legit := &Session{ID: "agent-sess-3-legit", Role: "agent"}

	// 1. Legitimate Agent registers node "my-nas" with secret "correct-key-1"
	err := broker.RegisterAgent("my-nas", "correct-key-1", nil, s1)
	if err != nil {
		t.Fatalf("Failed to register initial agent: %v", err)
	}

	if !broker.HasSecret("my-nas") {
		t.Errorf("Expected HasSecret to be true for my-nas")
	}

	// 2. Rogue Agent tries to hijack "my-nas" with wrong secret
	err = broker.RegisterAgent("my-nas", "wrong-key", nil, s2Rogue)
	if err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("Expected unauthorized error on hijack attempt, got: %v", err)
	}

	// 3. Rogue Agent tries to register "my-nas" with empty secret
	err = broker.RegisterAgent("my-nas", "", nil, s2Rogue)
	if err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("Expected unauthorized error on empty secret hijack attempt, got: %v", err)
	}

	// 4. Legit Agent re-registers with matching secret -> replacement succeeds
	err = broker.RegisterAgent("my-nas", "correct-key-1", nil, s3Legit)
	if err != nil {
		t.Fatalf("Legitimate re-registration should succeed, got: %v", err)
	}

	currentAgent := broker.GetAgent("my-nas")
	if currentAgent != s3Legit {
		t.Errorf("Agent was not updated to s3Legit")
	}
}

func TestSignalBroker_ServerMasterSecret(t *testing.T) {
	// Server started with master key
	masterSecret := "cluster-wide-preshared-master-key"
	broker := NewSignalBroker(masterSecret)

	s1 := &Session{ID: "agent-sess-1"}
	s2 := &Session{ID: "agent-sess-2"}

	// 1. Agent registers with wrong master secret -> rejected
	err := broker.RegisterAgent("node-1", "bad-secret", nil, s1)
	if err == nil || !strings.Contains(err.Error(), "server secret mismatch") {
		t.Errorf("Expected server secret mismatch error, got: %v", err)
	}

	// 2. Agent registers with correct master secret -> accepted
	err = broker.RegisterAgent("node-1", masterSecret, nil, s2)
	if err != nil {
		t.Fatalf("Expected registration to succeed with master secret, got: %v", err)
	}

	if broker.GetAgent("node-1") != s2 {
		t.Errorf("Agent node-1 not registered properly")
	}
}

func TestSignalBroker_ClientNotificationAndState(t *testing.T) {
	// Start an in-memory WebSocket test server using gorilla
	broker := NewSignalBroker("")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		session := &Session{ID: "client-test-sess", Conn: conn, NodeID: "target-device"}
		broker.RegisterClient(session)
		defer broker.RemoveClient(session.ID)

		// Wait until closed
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				break
			}
		}
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	u.Scheme = "ws"

	clientWS, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer clientWS.Close()

	// Broker notifies waiting clients when agent comes online
	time.Sleep(50 * time.Millisecond)
	requiresAuth := true
	_ = broker.RegisterAgent("target-device", "pass123", &requiresAuth, &Session{ID: "agent-1"})

	broker.NotifyWaitingClients("target-device")

	_ = clientWS.SetReadDeadline(time.Now().Add(2 * time.Second))
	var msg signaling.Message
	if err := clientWS.ReadJSON(&msg); err != nil {
		t.Fatalf("Client failed to receive online notification: %v", err)
	}

	if msg.Type != signaling.TypePeerStatus || msg.Status != "online" || !msg.RequiresSecret {
		t.Errorf("Notification mismatch: %+v", msg)
	}
}

func TestSelfSignedCert_ValidityAndSAN(t *testing.T) {
	cert, err := generateSelfSignedCert()
	if err != nil {
		t.Fatalf("generateSelfSignedCert failed: %v", err)
	}

	if len(cert.Certificate) == 0 {
		t.Fatalf("No certificate bytes returned")
	}

	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("Failed to parse generated x509 cert: %v", err)
	}

	// 1. Time validity
	now := time.Now()
	if now.Before(x509Cert.NotBefore) || now.After(x509Cert.NotAfter) {
		t.Errorf("Certificate validity window invalid: notBefore=%v, notAfter=%v", x509Cert.NotBefore, x509Cert.NotAfter)
	}

	// 2. DNS SAN
	hasLocalhost := false
	for _, dns := range x509Cert.DNSNames {
		if dns == "localhost" {
			hasLocalhost = true
			break
		}
	}
	if !hasLocalhost {
		t.Errorf("Certificate DNSNames missing 'localhost': %v", x509Cert.DNSNames)
	}

	// 3. IP SAN (Must include 127.0.0.1 and ::1)
	hasIPv4Loopback := false
	hasIPv6Loopback := false
	for _, ip := range x509Cert.IPAddresses {
		if ip.String() == "127.0.0.1" {
			hasIPv4Loopback = true
		}
		if ip.String() == "::1" {
			hasIPv6Loopback = true
		}
	}
	if !hasIPv4Loopback {
		t.Errorf("Certificate IPAddresses missing 127.0.0.1: %v", x509Cert.IPAddresses)
	}
	if !hasIPv6Loopback {
		t.Errorf("Certificate IPAddresses missing ::1: %v", x509Cert.IPAddresses)
	}
}

func TestIPRateLimiter_ConnectionAndRateLimits(t *testing.T) {
	limiter := NewIPRateLimiter(3) // Max 3 conns per IP

	ip := "192.168.1.100"

	// 1. Connection limit test
	if !limiter.AcquireConn(ip) {
		t.Errorf("First connection should be allowed")
	}
	if !limiter.AcquireConn(ip) {
		t.Errorf("Second connection should be allowed")
	}
	if !limiter.AcquireConn(ip) {
		t.Errorf("Third connection should be allowed")
	}
	// 4th connection should be rejected
	if limiter.AcquireConn(ip) {
		t.Errorf("Fourth connection should be rejected (limit=3)")
	}

	if limiter.Rejections() != 1 {
		t.Errorf("Expected 1 rejection, got %d", limiter.Rejections())
	}

	// Release one connection
	limiter.ReleaseConn(ip)
	if !limiter.AcquireConn(ip) {
		t.Errorf("Should allow connection after releasing one")
	}

	// 2. Message rate limit test
	for i := 0; i < 120; i++ {
		if !limiter.AllowMessage(ip) {
			t.Fatalf("Message %d should be allowed within threshold", i)
		}
	}
	// 121st message in 10s should be throttled
	if limiter.AllowMessage(ip) {
		t.Errorf("Message above 120 should be throttled")
	}

	// 3. Failed auth lockout test
	attackerIP := "10.0.0.50"
	for i := 0; i < 9; i++ {
		if limiter.RecordFailedAuth(attackerIP) {
			t.Fatalf("Attempt %d should not lock out yet", i)
		}
	}
	// 10th failure should lock out
	if !limiter.RecordFailedAuth(attackerIP) {
		t.Errorf("10th failure should trigger lockout")
	}
	if !limiter.IsLocked(attackerIP) {
		t.Errorf("IsLocked should return true after 10 failures")
	}
	limiter.ResetFailedAuth(attackerIP)
	if limiter.IsLocked(attackerIP) {
		t.Errorf("IsLocked should return false after ResetFailedAuth")
	}
}

func TestServerMetrics_Export(t *testing.T) {
	broker := NewSignalBroker("")
	limiter := NewIPRateLimiter(10)

	s1 := &Session{ID: "sess-node-1"}
	_ = broker.RegisterAgent("nas-1", "", nil, s1)

	metrics := exportServerMetrics(broker, limiter)
	if !strings.Contains(metrics, "webrtc_gateway_registered_nodes 1") {
		t.Errorf("Expected registered_nodes 1 in metrics, got:\n%s", metrics)
	}
	if !strings.Contains(metrics, "webrtc_gateway_ratelimit_rejected_total 0") {
		t.Errorf("Expected ratelimit_rejected_total 0 in metrics, got:\n%s", metrics)
	}
}

func TestGetClientIP_TrustedProxiesAndHeaderSpoofing(t *testing.T) {
	defaultLimiter := NewIPRateLimiter(64) // uses DefaultTrustedProxies()

	// 1. Untrusted direct public connection: should IGNORE forged proxy headers
	reqUntrusted := httptest.NewRequest("GET", "http://example.com/ws", nil)
	reqUntrusted.RemoteAddr = "203.0.113.5:12345"
	reqUntrusted.Header.Set("X-Forwarded-For", "1.1.1.1")
	reqUntrusted.Header.Set("CF-Connecting-IP", "2.2.2.2")
	reqUntrusted.Header.Set("X-Real-IP", "3.3.3.3")

	got := defaultLimiter.GetClientIP(reqUntrusted)
	if got != "203.0.113.5" {
		t.Errorf("Expected direct untrusted remote IP 203.0.113.5, got spoofed %q", got)
	}

	// 2. Trusted proxy (loopback): CF-Connecting-IP takes highest priority
	reqLoopback := httptest.NewRequest("GET", "http://example.com/ws", nil)
	reqLoopback.RemoteAddr = "127.0.0.1:54321"
	reqLoopback.Header.Set("CF-Connecting-IP", "203.0.113.10")
	reqLoopback.Header.Set("X-Forwarded-For", "198.51.100.20, 10.0.0.1")
	reqLoopback.Header.Set("X-Real-IP", "198.51.100.30")

	got = defaultLimiter.GetClientIP(reqLoopback)
	if got != "203.0.113.10" {
		t.Errorf("Expected CF-Connecting-IP 203.0.113.10, got %q", got)
	}

	// 3. Trusted proxy (Docker private IP 172.17.0.1): X-Forwarded-For first IP
	reqDocker := httptest.NewRequest("GET", "http://example.com/ws", nil)
	reqDocker.RemoteAddr = "172.17.0.1:45678"
	reqDocker.Header.Set("X-Forwarded-For", "198.51.100.25, 172.17.0.1")

	got = defaultLimiter.GetClientIP(reqDocker)
	if got != "198.51.100.25" {
		t.Errorf("Expected first XFF IP 198.51.100.25, got %q", got)
	}

	// 4. IPv4-mapped IPv6 connection (::ffff:172.17.0.1) from container host
	reqMapped := httptest.NewRequest("GET", "http://example.com/ws", nil)
	reqMapped.RemoteAddr = "[::ffff:172.17.0.1]:45678"
	reqMapped.Header.Set("X-Forwarded-For", "198.51.100.35")

	got = defaultLimiter.GetClientIP(reqMapped)
	if got != "198.51.100.35" {
		t.Errorf("Expected unmapped IPv4 proxy matching and XFF IP 198.51.100.35, got %q", got)
	}

	// 5. Custom trusted proxy list: parse custom CIDR
	customPrefixes, err := ParseTrustedProxies("203.0.113.0/24, 198.51.100.5")
	if err != nil {
		t.Fatalf("Failed to parse custom trusted proxies: %v", err)
	}
	customLimiter := NewIPRateLimiter(64, customPrefixes)

	reqCustom := httptest.NewRequest("GET", "http://example.com/ws", nil)
	reqCustom.RemoteAddr = "203.0.113.99:9999"
	reqCustom.Header.Set("X-Forwarded-For", "8.8.8.8")

	got = customLimiter.GetClientIP(reqCustom)
	if got != "8.8.8.8" {
		t.Errorf("Expected custom proxy match and XFF IP 8.8.8.8, got %q", got)
	}

	// 6. Explicitly disabled trusted proxies: "none"
	nonePrefixes, err := ParseTrustedProxies("none")
	if err != nil {
		t.Fatalf("Failed to parse 'none' trusted proxies: %v", err)
	}
	noneLimiter := NewIPRateLimiter(64, nonePrefixes)

	reqNone := httptest.NewRequest("GET", "http://example.com/ws", nil)
	reqNone.RemoteAddr = "127.0.0.1:9999"
	reqNone.Header.Set("X-Forwarded-For", "8.8.8.8")

	got = noneLimiter.GetClientIP(reqNone)
	if got != "127.0.0.1" {
		t.Errorf("Expected 'none' trusted proxies to reject loopback XFF and return socket IP 127.0.0.1, got %q", got)
	}
}

func TestSignalBroker_VisitorAuthDecoupled(t *testing.T) {
	broker := NewSignalBroker("cluster-master-secret")

	// Case 1: Agent registers with cluster secret, but visitor auth is FALSE (public service)
	pubVisitorAuth := false
	sPub := &Session{ID: "sess-pub"}
	err := broker.RegisterAgent("public-blog", "cluster-master-secret", &pubVisitorAuth, sPub)
	if err != nil {
		t.Fatalf("Failed to register public agent: %v", err)
	}

	if broker.RequiresVisitorAuth("public-blog") {
		t.Errorf("Expected RequiresVisitorAuth to be false for public node")
	}

	// Case 2: Agent registers with cluster secret, and visitor auth is TRUE (private service)
	privVisitorAuth := true
	sPriv := &Session{ID: "sess-priv"}
	err = broker.RegisterAgent("private-nas", "cluster-master-secret", &privVisitorAuth, sPriv)
	if err != nil {
		t.Fatalf("Failed to register private agent: %v", err)
	}

	if !broker.RequiresVisitorAuth("private-nas") {
		t.Errorf("Expected RequiresVisitorAuth to be true for private node")
	}
}

func TestServerConfig_FileParsing(t *testing.T) {
	yamlContent := `
addr: ":9999"
signal_secret: "yaml-cluster-secret"
max_ip_conns: 128
trusted_proxies: "10.0.0.0/8"
`
	tmpYAML, err := os.CreateTemp("", "server-test-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpYAML.Name())
	_, _ = tmpYAML.WriteString(yamlContent)
	_ = tmpYAML.Close()

	cfg, err := LoadServerConfigFile(tmpYAML.Name())
	if err != nil {
		t.Fatalf("Failed to load server config YAML: %v", err)
	}

	if cfg.Addr != ":9999" {
		t.Errorf("Expected addr :9999, got %q", cfg.Addr)
	}
	if cfg.SignalSecret != "yaml-cluster-secret" {
		t.Errorf("Expected signal_secret yaml-cluster-secret, got %q", cfg.SignalSecret)
	}
	if cfg.MaxIPConns != 128 {
		t.Errorf("Expected max_ip_conns 128, got %d", cfg.MaxIPConns)
	}
}

func TestSignalBroker_WebSocketKeepalivePingPong(t *testing.T) {
	// 1. Temporarily configure short keepalive intervals for fast testing
	origPing := defaultPingPeriod
	origRead := defaultReadTimeout
	defer func() {
		SetKeepaliveIntervalsForTest(origPing, origRead)
	}()
	SetKeepaliveIntervalsForTest(40*time.Millisecond, 150*time.Millisecond)

	broker := NewSignalBroker("")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		sessionID := "test-keepalive-sess"
		session := &Session{ID: sessionID, Conn: conn}

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		defer func() {
			cancel()
			session.mu.Lock()
			_ = conn.Close()
			session.Conn = nil
			session.mu.Unlock()
			broker.RemoveAgent("keepalive-node", session)
		}()

		conn.SetPongHandler(func(string) error {
			_ = conn.SetReadDeadline(time.Now().Add(defaultReadTimeout))
			return nil
		})

		conn.SetPingHandler(func(appData string) error {
			session.mu.Lock()
			defer session.mu.Unlock()
			if session.Conn == nil {
				return nil
			}
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			err := conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(5*time.Second))
			_ = conn.SetReadDeadline(time.Now().Add(defaultReadTimeout))
			return err
		})

		go func() {
			ticker := time.NewTicker(defaultPingPeriod)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := session.Ping(); err != nil {
						return
					}
				}
			}
		}()

		for {
			_ = conn.SetReadDeadline(time.Now().Add(defaultReadTimeout))
			var msg signaling.Message
			if err := conn.ReadJSON(&msg); err != nil {
				break
			}
			if msg.Type == signaling.TypeRegister {
				_ = broker.RegisterAgent(msg.NodeID, msg.Secret, msg.RequireVisitorAuth, session)
			}
		}
	}))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

	// Client connects
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial error: %v", err)
	}
	defer ws.Close()

	// Register
	regMsg := signaling.Message{Type: signaling.TypeRegister, NodeID: "keepalive-node"}
	if err := ws.WriteJSON(regMsg); err != nil {
		t.Fatalf("WriteJSON error: %v", err)
	}

	// Verify agent registered
	time.Sleep(20 * time.Millisecond)
	if broker.GetAgent("keepalive-node") == nil {
		t.Fatalf("Agent should be registered")
	}

	// Client receives Pings and sends Pongs back
	pingsReceived := 0
	ws.SetPingHandler(func(appData string) error {
		pingsReceived++
		return ws.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
	})

	// Start reading to allow control frame handler to process pings
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			var m signaling.Message
			if err := ws.ReadJSON(&m); err != nil {
				break
			}
		}
	}()

	// Wait 350ms (more than 2x the 150ms read deadline)
	time.Sleep(350 * time.Millisecond)

	if broker.GetAgent("keepalive-node") == nil {
		t.Errorf("Agent was prematurely disconnected; keepalive ping/pong failed to maintain read deadline!")
	}
	if pingsReceived < 3 {
		t.Errorf("Expected at least 3 pings received in 350ms with 40ms interval, got %d", pingsReceived)
	}
}

func TestSignalServer_HealthCheckEndpoints(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	for _, endpoint := range []string{"/healthz", "/health"} {
		resp, err := http.Get(server.URL + endpoint)
		if err != nil {
			t.Fatalf("Failed to GET %s: %v", endpoint, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected status 200 on %s, got %d", endpoint, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

func TestSignalServer_RunHealthCheckProbe(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// 1. Plain HTTP probe
	httpServer := httptest.NewServer(mux)
	defer httpServer.Close()

	httpCode := runHealthCheck(httpServer.Listener.Addr().String(), false, "")
	if httpCode != 0 {
		t.Errorf("Expected runHealthCheck on HTTP server to return 0, got %d", httpCode)
	}

	// 2. HTTPS (Self-signed) probe
	tlsServer := httptest.NewTLSServer(mux)
	defer tlsServer.Close()

	tlsCode := runHealthCheck(tlsServer.Listener.Addr().String(), true, "")
	if tlsCode != 0 {
		t.Errorf("Expected runHealthCheck on HTTPS server to return 0, got %d", tlsCode)
	}

	// 3. Custom URL probe
	customCode := runHealthCheck("", false, httpServer.URL+"/healthz")
	if customCode != 0 {
		t.Errorf("Expected runHealthCheck with custom URL to return 0, got %d", customCode)
	}

	// 4. Failing probe (unreachable port)
	failCode := runHealthCheck("127.0.0.1:59998", false, "")
	if failCode != 1 {
		t.Errorf("Expected runHealthCheck on closed port to return 1, got %d", failCode)
	}
}

func TestSignalServer_Offer_ClientIP_InjectionAndAntiSpoofing(t *testing.T) {
	broker := NewSignalBroker("")
	limiter := NewIPRateLimiter(64, nil)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientIP := limiter.GetClientIP(r)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		sessionID := "test-sess-" + clientIP
		session := &Session{
			ID:       sessionID,
			RemoteIP: clientIP,
			Conn:     conn,
		}
		defer conn.Close()

		for {
			var msg signaling.Message
			if err := conn.ReadJSON(&msg); err != nil {
				break
			}
			switch msg.Type {
			case signaling.TypeRegister:
				session.Role = "agent"
				session.NodeID = msg.NodeID
				_ = broker.RegisterAgent(msg.NodeID, "", nil, session)
			case signaling.TypeOffer:
				msg.SessionID = session.ID
				msg.ClientIP = session.RemoteIP
				agent := broker.GetAgent(msg.NodeID)
				if agent != nil {
					_ = agent.Send(&msg)
				}
			}
		}
	})

	server := httptest.NewServer(handler)
	defer server.Close()

	u, _ := url.Parse(server.URL)
	u.Scheme = "ws"

	// 1. Connect agent
	agentWS, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		t.Fatalf("Agent dial failed: %v", err)
	}
	defer agentWS.Close()
	_ = agentWS.WriteJSON(&signaling.Message{Type: signaling.TypeRegister, NodeID: "test-node"})

	// Give registration a moment to be recorded in broker
	time.Sleep(50 * time.Millisecond)

	// 2. Connect client
	clientWS, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		t.Fatalf("Client dial failed: %v", err)
	}
	defer clientWS.Close()

	// 3. Client sends Offer with spoofed ClientIP
	_ = clientWS.WriteJSON(&signaling.Message{
		Type:      signaling.TypeOffer,
		NodeID:    "test-node",
		ClientIP:  "198.51.100.22", // Attacker trying to spoof IP!
		SessionID: "spoofed-session",
	})

	// 4. Agent receives Offer
	var forwarded signaling.Message
	_ = agentWS.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := agentWS.ReadJSON(&forwarded); err != nil {
		t.Fatalf("Agent failed to receive offer: %v", err)
	}

	if forwarded.ClientIP == "198.51.100.22" {
		t.Errorf("Security risk! Agent received spoofed ClientIP")
	}
	if forwarded.ClientIP != "127.0.0.1" {
		t.Errorf("Expected ClientIP to be 127.0.0.1, got %q", forwarded.ClientIP)
	}
}

func TestSignalServer_HelpAndVersionFlags(t *testing.T) {
	// 1. Test -h
	cmd := exec.Command("go", "run", ".", "-h")
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("Expected signal -h to exit 0, got err: %v\nOutput: %s", err, string(out))
	}
	output := string(out)
	if !strings.Contains(output, "WebRTC Gateway Signal") || !strings.Contains(output, "TLS & HTTPS Security Options") || !strings.Contains(output, "-trusted-proxies") {
		t.Errorf("Unexpected help output for signal -h:\n%s", output)
	}

	// 2. Test help subcommand
	cmd = exec.Command("go", "run", ".", "help")
	cmd.Dir = "."
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Errorf("Expected signal help to exit 0, got err: %v\nOutput: %s", err, string(out))
	}
	if !strings.Contains(string(out), "Server & Network Options") {
		t.Errorf("Unexpected help output for signal help:\n%s", string(out))
	}

	// 3. Test -v and --version
	for _, flagArg := range []string{"-v", "--version"} {
		cmd = exec.Command("go", "run", ".", flagArg)
		cmd.Dir = "."
		out, err = cmd.CombinedOutput()
		if err != nil {
			t.Errorf("Expected signal %s to exit 0, got err: %v\nOutput: %s", flagArg, err, string(out))
		}
		if !strings.Contains(string(out), "WebRTC Gateway Signal v") {
			t.Errorf("Expected version string for %s, got:\n%s", flagArg, string(out))
		}
	}
}

func TestIPRateLimiter_JanitorPrunesExpired(t *testing.T) {
	limiter := NewIPRateLimiter(64)
	defer limiter.Close()

	// 1. Setup IP records
	activeIP := "192.168.1.10"
	expiredIP := "192.168.1.20"

	// Add active entry
	limiter.AllowMessage(activeIP)
	limiter.RecordFailedAuth(activeIP)

	// Simulate expired entries for expiredIP
	limiter.mu.Lock()
	limiter.msgTimes[expiredIP] = []time.Time{
		time.Now().Add(-15 * time.Second),
		time.Now().Add(-12 * time.Second),
	}
	limiter.failedAuths[expiredIP] = []time.Time{
		time.Now().Add(-2 * time.Minute),
		time.Now().Add(-70 * time.Second),
	}
	limiter.mu.Unlock()

	// Verify before prune: both activeIP and expiredIP exist in maps
	limiter.mu.Lock()
	if _, exists := limiter.msgTimes[expiredIP]; !exists {
		t.Fatalf("expiredIP should exist in msgTimes before prune")
	}
	if _, exists := limiter.failedAuths[expiredIP]; !exists {
		t.Fatalf("expiredIP should exist in failedAuths before prune")
	}
	limiter.mu.Unlock()

	// 2. Trigger pruneExpired
	limiter.pruneExpired()

	// 3. Verify after prune: expiredIP is pruned and map key is deleted (anti-memory-leak)
	limiter.mu.Lock()
	defer limiter.mu.Unlock()

	if _, exists := limiter.msgTimes[expiredIP]; exists {
		t.Errorf("expiredIP should be deleted from msgTimes after prune")
	}
	if _, exists := limiter.failedAuths[expiredIP]; exists {
		t.Errorf("expiredIP should be deleted from failedAuths after prune")
	}

	// Active IP must be preserved
	if times, exists := limiter.msgTimes[activeIP]; !exists || len(times) == 0 {
		t.Errorf("activeIP must be retained in msgTimes")
	}
	if times, exists := limiter.failedAuths[activeIP]; !exists || len(times) == 0 {
		t.Errorf("activeIP must be retained in failedAuths")
	}
}
