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

func sampleOfferDescriptor() *SessionDescriptor {
	return &SessionDescriptor{
		FP:         strings.Repeat("ab", 32),
		IceUfrag:   "ufrag1234",
		IcePwd:     "icepwd12345678901234567890",
		Setup:      "actpass",
		Mid:        "0",
		SCTPPort:   5000,
		MaxMsgSize: 262144,
		SDPVer:     2,
	}
}

func sampleAnswerDescriptor() *SessionDescriptor {
	return &SessionDescriptor{
		FP:         strings.Repeat("cd", 32),
		IceUfrag:   "ufrag5678",
		IcePwd:     "icepwd09876543210987654321",
		Setup:      "active",
		Mid:        "0",
		SCTPPort:   5000,
		MaxMsgSize: 262144,
		SDPVer:     2,
	}
}

func registerTestAgent(t *testing.T, wsURL string, secret string, pub ed25519.PublicKey, priv ed25519.PrivateKey, nodeID string, requireVisitorAuth bool) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{}
	if secret != "" {
		hdr.Set("Authorization", "Bearer "+secret)
	}
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("dial agent: %v", err)
	}
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read challenge: %v", err)
	}
	var ch ChallengeDTO
	if err := json.Unmarshal(raw, &ch); err != nil || ch.Type != TypeChallenge || ch.Nonce == "" {
		t.Fatalf("unexpected challenge: %s", string(raw))
	}
	sig := ed25519.Sign(priv, RegisterSignPayload(nodeID, ch.Nonce))
	reg, _ := json.Marshal(map[string]any{
		"type":                 TypeRegister,
		"pk":                   base64.RawURLEncoding.EncodeToString(pub),
		"sig":                  base64.RawURLEncoding.EncodeToString(sig),
		"require_visitor_auth": requireVisitorAuth,
	})
	if err := conn.Write(ctx, websocket.MessageText, reg); err != nil {
		t.Fatalf("write register: %v", err)
	}
	_, ackRaw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read registered: %v", err)
	}
	var ack RegisteredDTO
	if err := json.Unmarshal(ackRaw, &ack); err != nil || ack.Type != TypeRegistered {
		t.Fatalf("unexpected register ack: %s", string(ackRaw))
	}
	return conn
}

func TestSignalV7_AgentDisconnectNotifiesClientAndVisitorFloodNeverKillsAgent(t *testing.T) {
	srv, err := NewSignalServer(ServerOptions{
		Addr:          "127.0.0.1:0",
		SignalSecret:  testStrongSecret,
		StaticFS:      testDistFS(),
		MaxIPMessages: 1000,
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
	if _, resp, err := websocket.Dial(ctx, wsBase+"/ws/agent/nas", nil); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on missing SIGNAL_SECRET, err=%v resp=%v", err, resp)
	}

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	agentConn := registerTestAgent(t, wsBase+"/ws/agent/nas", testStrongSecret, pub, priv, "nas", true)
	defer agentConn.CloseNow()

	// Wrong Ed25519 key for same node must be rejected (NODE_OWNERSHIP_CONFLICT).
	pub2, priv2, _ := ed25519.GenerateKey(rand.Reader)
	badConn, _, err := websocket.Dial(ctx, wsBase+"/ws/agent/nas", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + testStrongSecret}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, chRaw, _ := badConn.Read(ctx)
	var ch ChallengeDTO
	_ = json.Unmarshal(chRaw, &ch)
	badReg, _ := json.Marshal(map[string]any{
		"type": TypeRegister,
		"pk":   base64.RawURLEncoding.EncodeToString(pub2),
		"sig":  base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv2, RegisterSignPayload("nas", ch.Nonce))),
	})
	_ = badConn.Write(ctx, websocket.MessageText, badReg)
	_, errMsg, _ := badConn.Read(ctx)
	if !strings.Contains(string(errMsg), "NODE_OWNERSHIP_CONFLICT") {
		t.Fatalf("expected NODE_OWNERSHIP_CONFLICT, got %s", string(errMsg))
	}
	_ = badConn.CloseNow()

	// Connect visitor client and verify JoinedDTO carries authoritative AgentPK.
	clientConn, _, err := websocket.Dial(ctx, wsBase+"/ws/client/nas", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.CloseNow()
	_, joinedRaw, err := clientConn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var joined JoinedDTO
	wantPK := base64.RawURLEncoding.EncodeToString(pub)
	if err := json.Unmarshal(joinedRaw, &joined); err != nil || !joined.Online || !joined.RequireVisitorAuth || joined.AgentPK != wantPK {
		t.Fatalf("unexpected joined msg: %s", string(joinedRaw))
	}

	// Pre-Offer candidate must be rejected.
	preCand, _ := json.Marshal(map[string]any{"type": TypeCandidate, "candidate": "candidate:1 1 udp 1 127.0.0.1 9 typ host"})
	_ = clientConn.Write(ctx, websocket.MessageText, preCand)
	_, preResp, err := clientConn.Read(ctx)
	if err != nil || !strings.Contains(string(preResp), "PRE_OFFER_CANDIDATE") {
		t.Fatalf("expected PRE_OFFER_CANDIDATE, got %s err=%v", string(preResp), err)
	}

	// Visitor flood using valid structured OfferDescriptor must not kill Agent.
	offerMsg, _ := json.Marshal(map[string]any{
		"type":  TypeOffer,
		"desc":  sampleOfferDescriptor(),
		"nonce": "0123456789abcdef012345",
	})
	_ = clientConn.Write(ctx, websocket.MessageText, offerMsg)
	for i := 0; i < 60; i++ {
		_ = clientConn.Write(ctx, websocket.MessageText, preCand)
	}
	time.Sleep(50 * time.Millisecond)
	if srv.Broker.GetAgent("nas") == nil {
		t.Fatal("visitor flood disconnected legitimate Agent")
	}

	// Answer with mismatched PK must be rejected by SignalServer itself (L-10: actually assert PK_MISMATCH!).
	badAns, _ := json.Marshal(map[string]any{
		"type":       TypeAnswer,
		"session_id": joined.SessionID,
		"desc":       sampleAnswerDescriptor(),
		"pk":         base64.RawURLEncoding.EncodeToString(pub2),
		"sig":        "fakesig",
	})
	_ = agentConn.Write(ctx, websocket.MessageText, badAns)
	gotPKMismatch := false
	for i := 0; i < 80; i++ {
		rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, raw, rerr := agentConn.Read(rctx)
		rcancel()
		if rerr != nil {
			t.Fatalf("failed reading agentConn while waiting for PK_MISMATCH: %v", rerr)
		}
		var ed ErrorDTO
		if json.Unmarshal(raw, &ed) == nil && ed.Type == TypeError && ed.Code == "PK_MISMATCH" {
			gotPKMismatch = true
			break
		}
	}
	if !gotPKMismatch {
		t.Fatal("agentConn never received PK_MISMATCH error for mismatched Answer PK")
	}

	// Disconnecting agent must notify client with peer_status offline.
	_ = agentConn.Close(websocket.StatusNormalClosure, "bye")
	gotOffline := false
	for i := 0; i < 10; i++ {
		rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, raw, rerr := clientConn.Read(rctx)
		rcancel()
		if rerr != nil {
			break
		}
		var ps PeerStatusDTO
		if json.Unmarshal(raw, &ps) == nil && ps.Type == TypePeerStatus && !ps.Online {
			gotOffline = true
			break
		}
	}
	if !gotOffline {
		t.Fatal("client never received peer_status offline when Agent disconnected")
	}
}

func TestSignalV7_IPv6PrefixRateLimitAndLockoutPreservation(t *testing.T) {
	lim := NewIPRateLimiter(64, 120)
	// Two different hosts in the same /64 must share the same auth lockout bucket.
	for i := 0; i < authLockoutThreshold; i++ {
		lim.RecordFailedAuth("2001:db8:abcd:1234::1")
	}
	if !lim.IsAuthLocked("2001:db8:abcd:1234::9999") {
		t.Fatal("IPv6 /64 rotation bypassed auth lockout")
	}

	// Filling the table with unlocked single failures must not evict the actively locked IPv6 /64.
	for i := 0; i < maxAuthFailedIPs+50; i++ {
		lim.RecordFailedAuth(RateLimitKey("198.51.100."+string(rune('0'+i%10))) + "_" + string(rune(i)))
	}
	if !lim.IsAuthLocked("2001:db8:abcd:1234::1") {
		t.Fatal("active auth lockout entry was evicted by LRU flood")
	}
}

func TestAuthLimiterFailClosedWhenSaturated(t *testing.T) {
	// Default AuthLockoutTable (AllowEvictLockedOnFull=false, used by Agent SPAKE2+ password auth)
	// MUST fail closed globally when every slot holds an unexpired lockout.
	tab := NewAuthLockoutTable(AuthLockoutConfig{
		MaxFailures:     authLockoutThreshold,
		LockoutDuration: authLockoutDuration,
		MaxPendingKeys:  64,
		MaxLockedKeys:   64,
	})
	now := time.Now()
	for i := 0; i < 64; i++ {
		ip := fmt.Sprintf("10.0.%d.%d", i/256, i%256)
		for f := 0; f < authLockoutThreshold; f++ {
			tab.RecordFailure(ip, now)
		}
	}
	if allowed, _ := tab.Allow("198.51.100.250", now); allowed {
		t.Fatal("default AuthLockoutTable failed open when saturated with active lockouts")
	}
	if !tab.RecordFailure("198.51.100.251", now) {
		t.Fatal("RecordFailure must return true (locked) when saturated in fail-closed mode")
	}
}

func TestSignalV7_BoundKeyPersistenceAndNonEviction(t *testing.T) {
	dir := t.TempDir()
	bkPath := filepath.Join(dir, "bound_keys.json")

	pub1, _, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)

	b1, err := NewSignalBrokerWithFile(bkPath)
	if err != nil {
		t.Fatal(err)
	}
	s1 := &Session{ID: "a1", Role: RoleAgent, NodeID: "persistnode"}
	if err := b1.RegisterAgent(s1, pub1, false, false); err != nil {
		t.Fatal(err)
	}
	b1.CleanupSession(s1)
	b1.CloseAll()

	// Restart broker from disk: pub2 must be rejected because pub1 binding persisted.
	b2, err := NewSignalBrokerWithFile(bkPath)
	if err != nil {
		t.Fatal(err)
	}
	defer b2.CloseAll()
	s2 := &Session{ID: "a2", Role: RoleAgent, NodeID: "persistnode"}
	if err := b2.RegisterAgent(s2, pub2, false, false); err != ErrNodeOwnershipConflict {
		t.Fatalf("expected persisted ErrNodeOwnershipConflict, got %v", err)
	}
}

func TestClientPostOfferLifetimeReaped(t *testing.T) {
	b := NewSignalBroker()
	defer b.CloseAll()

	now := time.Now()
	// Client 1: post-offer idle beyond DefaultPostOfferIdleTimeout (45s) -> must be reaped.
	cIdle := &Session{
		ID:           "c-idle",
		Role:         RoleClient,
		NodeID:       "nas",
		joinedAt:     now.Add(-50 * time.Second),
		lastActivity: now.Add(-(DefaultPostOfferIdleTimeout + 5*time.Second)),
		hasSentOffer: true,
		outCh:        make(chan []byte, 4),
	}
	cIdle.ctx, cIdle.cancel = context.WithCancel(context.Background())

	// Client 2: active candidates, but total lifetime exceeded DefaultMaxClientLifetime (2m) -> must be reaped.
	cOld := &Session{
		ID:           "c-old",
		Role:         RoleClient,
		NodeID:       "nas",
		joinedAt:     now.Add(-(DefaultMaxClientLifetime + 5*time.Second)),
		lastActivity: now,
		hasSentOffer: true,
		outCh:        make(chan []byte, 4),
	}
	cOld.ctx, cOld.cancel = context.WithCancel(context.Background())

	// Client 3: fresh post-offer client -> must survive.
	cFresh := &Session{
		ID:           "c-fresh",
		Role:         RoleClient,
		NodeID:       "nas",
		joinedAt:     now.Add(-10 * time.Second),
		lastActivity: now.Add(-2 * time.Second),
		hasSentOffer: true,
		outCh:        make(chan []byte, 4),
	}
	cFresh.ctx, cFresh.cancel = context.WithCancel(context.Background())

	b.mu.Lock()
	b.clients[cIdle.ID] = cIdle
	b.clients[cOld.ID] = cOld
	b.clients[cFresh.ID] = cFresh
	b.nodeClients["nas"] = map[string]*Session{
		cIdle.ID:  cIdle,
		cOld.ID:   cOld,
		cFresh.ID: cFresh,
	}
	b.mu.Unlock()

	b.reapTimedOutClients(now)

	b.mu.Lock()
	_, idleExists := b.clients[cIdle.ID]
	_, oldExists := b.clients[cOld.ID]
	_, freshExists := b.clients[cFresh.ID]
	b.mu.Unlock()

	if idleExists {
		t.Fatal("post-offer idle client was not reaped")
	}
	if oldExists {
		t.Fatal("client exceeding MaxClientLifetime was not reaped")
	}
	if !freshExists {
		t.Fatal("fresh active client was wrongly reaped")
	}
}

func TestBrokerCloseAllBoundedTimeout(t *testing.T) {
	srv, err := NewSignalServer(ServerOptions{
		Addr:         "127.0.0.1:0",
		SignalSecret: testStrongSecret,
		StaticFS:     testDistFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Open several client connections and leave them unread so close handshakes won't be acked quickly.
	var conns []*websocket.Conn
	for i := 0; i < 8; i++ {
		c, _, err := websocket.Dial(ctx, wsBase+"/ws/client/nas", nil)
		if err != nil {
			t.Fatalf("dial client %d: %v", i, err)
		}
		conns = append(conns, c)
	}
	defer func() {
		for _, c := range conns {
			_ = c.CloseNow()
		}
	}()

	start := time.Now()
	srv.Shutdown()
	elapsed := time.Since(start)
	if elapsed > 3500*time.Millisecond {
		t.Fatalf("Broker.CloseAll exceeded bounded 2s fan-out timeout: took %v", elapsed)
	}
}

func TestSharedSecretDisablesForceRekeyByDefault(t *testing.T) {
	srv, err := NewSignalServer(ServerOptions{
		Addr:         "127.0.0.1:0",
		SignalSecret: "shared-cluster-secret",
		StaticFS:     testDistFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")

	pub1, priv1, _ := ed25519.GenerateKey(rand.Reader)
	ag1 := registerTestAgent(t, wsBase+"/ws/agent/nas", "shared-cluster-secret", pub1, priv1, "nas", true)
	_ = ag1.CloseNow()

	// Another node holding the shared secret trying force_rekey must fail by default.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pub2, priv2, _ := ed25519.GenerateKey(rand.Reader)
	conn2, _, err := websocket.Dial(ctx, wsBase+"/ws/agent/nas", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer shared-cluster-secret"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.CloseNow()
	_, chRaw, _ := conn2.Read(ctx)
	var ch ChallengeDTO
	_ = json.Unmarshal(chRaw, &ch)
	reg, _ := json.Marshal(map[string]any{
		"type":        TypeRegister,
		"pk":          base64.RawURLEncoding.EncodeToString(pub2),
		"sig":         base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv2, RegisterSignPayload("nas", ch.Nonce))),
		"force_rekey": true,
	})
	_ = conn2.Write(ctx, websocket.MessageText, reg)
	_, respRaw, _ := conn2.Read(ctx)
	if !strings.Contains(string(respRaw), "NODE_OWNERSHIP_CONFLICT") {
		t.Fatalf("expected force_rekey under shared SIGNAL_SECRET to fail closed by default, got %s", string(respRaw))
	}
}
