package main

import (
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	err := broker.RegisterAgent("my-nas", "correct-key-1", s1)
	if err != nil {
		t.Fatalf("Failed to register initial agent: %v", err)
	}

	if !broker.HasSecret("my-nas") {
		t.Errorf("Expected HasSecret to be true for my-nas")
	}

	// 2. Rogue Agent tries to hijack "my-nas" with wrong secret
	err = broker.RegisterAgent("my-nas", "wrong-key", s2Rogue)
	if err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("Expected unauthorized error on hijack attempt, got: %v", err)
	}

	// 3. Rogue Agent tries to register "my-nas" with empty secret
	err = broker.RegisterAgent("my-nas", "", s2Rogue)
	if err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("Expected unauthorized error on empty secret hijack attempt, got: %v", err)
	}

	// 4. Legit Agent re-registers with matching secret -> replacement succeeds
	err = broker.RegisterAgent("my-nas", "correct-key-1", s3Legit)
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
	err := broker.RegisterAgent("node-1", "bad-secret", s1)
	if err == nil || !strings.Contains(err.Error(), "server secret mismatch") {
		t.Errorf("Expected server secret mismatch error, got: %v", err)
	}

	// 2. Agent registers with correct master secret -> accepted
	err = broker.RegisterAgent("node-1", masterSecret, s2)
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
	_ = broker.RegisterAgent("target-device", "pass123", &Session{ID: "agent-1"})

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
}

func TestServerMetrics_Export(t *testing.T) {
	broker := NewSignalBroker("")
	limiter := NewIPRateLimiter(10)

	s1 := &Session{ID: "sess-node-1"}
	_ = broker.RegisterAgent("nas-1", "", s1)

	metrics := exportServerMetrics(broker, limiter)
	if !strings.Contains(metrics, "webrtc_gateway_registered_nodes 1") {
		t.Errorf("Expected registered_nodes 1 in metrics, got:\n%s", metrics)
	}
	if !strings.Contains(metrics, "webrtc_gateway_ratelimit_rejected_total 0") {
		t.Errorf("Expected ratelimit_rejected_total 0 in metrics, got:\n%s", metrics)
	}
}
