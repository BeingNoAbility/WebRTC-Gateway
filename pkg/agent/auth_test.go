package agent

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"webrtc-gateway/pkg/protocol"
	"webrtc-gateway/pkg/proxy"
)

func TestAgentV7_IdentityPersistenceAndCapabilityRevocationPersistence(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "nas.ed25519.key")
	id1, err := LoadOrCreateIdentity(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := LoadOrCreateIdentity(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if id1.PKHash() != id2.PKHash() || len(id1.PKHash()) != PKHashChars {
		t.Fatalf("persisted identity mismatch: %q vs %q", id1.PKHash(), id2.PKHash())
	}

	p := CapabilityPayload{
		V: CapabilityVersion, JTI: "cap-1", Node: "nas", Exp: time.Now().Add(time.Hour).Unix(),
		PathPrefix: "/docs", Readonly: true, MaxConns: 2,
	}
	tok, err := id1.MintCapability(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := id2.VerifyCapability(tok, "nas", time.Now())
	if err != nil || got.JTI != "cap-1" || !got.Readonly || got.PathPrefix != "/docs" {
		t.Fatalf("capability verification failed: %+v err=%v", got, err)
	}
	if _, err := id2.VerifyCapability(tok, "other-node", time.Now()); err == nil {
		t.Fatal("capability accepted for wrong node")
	}
	if _, err := id2.VerifyCapability(tok, "nas", time.Now().Add(2*time.Hour)); err == nil {
		t.Fatal("expired capability accepted")
	}

	// Revoked capability must remain revoked across Agent restart.
	stateFile := filepath.Join(dir, "nas.state.json")
	cs1, err := NewCapabilityStoreWithFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := cs1.Track(p); err != nil {
		t.Fatal(err)
	}
	if err := cs1.Revoke("cap-1"); err != nil {
		t.Fatal(err)
	}

	cs2, err := NewCapabilityStoreWithFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if cs2.AcquireConn(&p) {
		t.Fatal("revoked capability token resurrected after restart")
	}
}

func TestCapabilityListPagination(t *testing.T) {
	cs := NewCapabilityStore()
	now := time.Now().Unix()
	for i := 0; i < 65; i++ {
		if err := cs.Track(CapabilityPayload{
			V: CapabilityVersion, JTI: fmt.Sprintf("cap-%03d", i), Node: "nas",
			Exp: now + 3600 + int64(i), PathPrefix: "/",
		}); err != nil {
			t.Fatal(err)
		}
	}
	nowTime := time.Now()
	page1, total, hasMore1 := cs.ListPage(nowTime, 0, 0)
	if total != 65 || len(page1) != DefaultCapListLimit || !hasMore1 {
		t.Fatalf("default page mismatch: len=%d total=%d hasMore=%v", len(page1), total, hasMore1)
	}
	page2, total2, hasMore2 := cs.ListPage(nowTime, 50, 20)
	if total2 != 65 || len(page2) != 15 || hasMore2 {
		t.Fatalf("page2 mismatch: len=%d total=%d hasMore=%v", len(page2), total2, hasMore2)
	}
	pageEmpty, _, hasMore3 := cs.ListPage(nowTime, 999, 10)
	if len(pageEmpty) != 0 || hasMore3 {
		t.Fatalf("out-of-range page must be empty, got %d hasMore=%v", len(pageEmpty), hasMore3)
	}
}

func TestAuthLimiterFailClosedWhenSaturated(t *testing.T) {
	lim := NewVisitorAuthLimiter()
	now := time.Now()

	// Fill every slot with an active unexpired lockout.
	for i := 0; i < visitorAuthMaxKeys; i++ {
		ip := fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xff, (i>>8)&0xff, i&0xff)
		for f := 0; f < visitorAuthMaxFailures; f++ {
			lim.RecordFailure(ip, now)
		}
	}

	// Unseen IP must fail closed with positive retryAfter while saturated.
	allowed, retryAfter := lim.Allow("198.51.100.200", now)
	if allowed {
		t.Fatal("VisitorAuthLimiter failed open when saturated with active lockouts")
	}
	if retryAfter <= 0 {
		t.Fatalf("expected positive retryAfter when saturated, got %v", retryAfter)
	}
	if !lim.RecordFailure("198.51.100.201", now) {
		t.Fatal("RecordFailure must return true when saturated")
	}

	// Once the lockout window expires, new IPs must recover cleanly.
	afterWindow := now.Add(visitorAuthWindow + time.Second)
	allowed, _ = lim.Allow("198.51.100.202", afterWindow)
	if !allowed {
		t.Fatal("VisitorAuthLimiter stayed stuck locked after lockout window expired")
	}
}

func TestAgentV7_SPAKE2P_MutualAuthAndNegativeAttacks(t *testing.T) {
	const testAgentPK = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	verifierRec, err := DeriveSPAKE2PVerifier("hunter2-strong-pass", "nas", testAgentPK)
	if err != nil {
		t.Fatal(err)
	}
	vs, err := NewSPAKE2PVerifierSession(verifierRec)
	if err != nil {
		t.Fatal(err)
	}

	ctxBytes, err := BuildSPAKE2PContext("nas", strings.Repeat("aa", 32), strings.Repeat("bb", 32), testAgentPK, "vnonce", "anonce")
	if err != nil {
		t.Fatal(err)
	}
	proverCreds, err := DeriveSPAKE2PProverCreds("hunter2-strong-pass", "nas", testAgentPK)
	if err != nil {
		t.Fatal(err)
	}
	shareP, confirmP, wantConfirmV, proverKey, err := SPAKE2PProverCompute(proverCreds, ctxBytes, vs.ShareV())
	if err != nil {
		t.Fatalf("SPAKE2PProverCompute: %v", err)
	}
	gotConfirmV, verifierKey, err := vs.Finish(ctxBytes, shareP, confirmP)
	if err != nil {
		t.Fatalf("SPAKE2P Verifier Finish failed: %v", err)
	}
	if !bytes.Equal(gotConfirmV, wantConfirmV) || !bytes.Equal(proverKey, verifierKey) {
		t.Fatal("SPAKE2+ mutual confirmation or shared key mismatch")
	}

	// Negative 1: Wrong password fails.
	vs2, _ := NewSPAKE2PVerifierSession(verifierRec)
	wrongCreds, err := DeriveSPAKE2PProverCreds("wrong-password", "nas", testAgentPK)
	if err != nil {
		t.Fatal(err)
	}
	sp2, cp2, _, _, err := SPAKE2PProverCompute(wrongCreds, ctxBytes, vs2.ShareV())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := vs2.Finish(ctxBytes, sp2, cp2); err == nil {
		t.Fatal("SPAKE2+ accepted wrong password")
	}

	// Negative 2: Mismatched DTLS context fails (anti-MITM channel binding).
	vs3, _ := NewSPAKE2PVerifierSession(verifierRec)
	sp3, cp3, _, _, _ := SPAKE2PProverCompute(proverCreds, ctxBytes, vs3.ShareV())
	otherCtx, err := BuildSPAKE2PContext("nas", strings.Repeat("cc", 32), strings.Repeat("bb", 32), testAgentPK, "vnonce", "anonce")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := vs3.Finish(otherCtx, sp3, cp3); err == nil {
		t.Fatal("SPAKE2+ accepted mismatched DTLS fingerprint context")
	}

	// Negative 3: Off-curve / bogus shareP rejected.
	if _, _, err := vs3.Finish(ctxBytes, make([]byte, 65), cp3); err == nil {
		t.Fatal("SPAKE2+ accepted off-curve shareP")
	}
}

func TestRFC9383AppendixCVectorAndV7KDF(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "protocol_v7_golden_vectors.json"))
	if err != nil {
		t.Fatalf("read golden vectors: %v", err)
	}
	var doc struct {
		RFC9383 struct {
			Context      string `json:"context"`
			IDProver     string `json:"id_prover"`
			IDVerifier   string `json:"id_verifier"`
			W0Hex        string `json:"w0_hex"`
			W1Hex        string `json:"w1_hex"`
			LHex         string `json:"L_hex"`
			XHex         string `json:"x_hex"`
			SharePHex    string `json:"share_p_hex"`
			YHex         string `json:"y_hex"`
			ShareVHex    string `json:"share_v_hex"`
			ConfirmPHex  string `json:"confirm_p_hex"`
			ConfirmVHex  string `json:"confirm_v_hex"`
			SharedKeyHex string `json:"shared_key_hex"`
		} `json:"rfc9383_appendix_c"`
		V7KDF struct {
			Password   string `json:"password"`
			NodeID     string `json:"node_id"`
			AgentPKB64 string `json:"agent_pk_b64"`
			W0Hex      string `json:"w0_hex"`
			W1Hex      string `json:"w1_hex"`
		} `json:"v7_spake2p_kdf"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	mustHex := func(s string) []byte {
		b, err := hex.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	var w0, w1, x, y [32]byte
	copy(w0[:], mustHex(doc.RFC9383.W0Hex))
	copy(w1[:], mustHex(doc.RFC9383.W1Hex))
	copy(x[:], mustHex(doc.RFC9383.XHex))
	copy(y[:], mustHex(doc.RFC9383.YHex))
	L := mustHex(doc.RFC9383.LHex)

	vs, err := NewSPAKE2PVerifierSessionWithScalar(
		&SPAKE2PVerifierRecord{W0: w0, L: L},
		y,
		[]byte(doc.RFC9383.IDProver),
		[]byte(doc.RFC9383.IDVerifier),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(vs.ShareV(), mustHex(doc.RFC9383.ShareVHex)) {
		t.Fatalf("RFC 9383 shareV mismatch:\ngot:  %x\nwant: %s", vs.ShareV(), doc.RFC9383.ShareVHex)
	}

	shareP, confirmP, wantConfirmV, proverKey, err := SPAKE2PProverComputeWithScalar(
		SPAKE2PProverCreds{W0: w0, W1: w1},
		x,
		[]byte(doc.RFC9383.Context),
		[]byte(doc.RFC9383.IDProver),
		[]byte(doc.RFC9383.IDVerifier),
		vs.ShareV(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(shareP, mustHex(doc.RFC9383.SharePHex)) {
		t.Fatalf("RFC 9383 shareP mismatch: %x", shareP)
	}
	if !bytes.Equal(confirmP, mustHex(doc.RFC9383.ConfirmPHex)) {
		t.Fatalf("RFC 9383 confirmP mismatch:\ngot:  %x\nwant: %s", confirmP, doc.RFC9383.ConfirmPHex)
	}
	if !bytes.Equal(wantConfirmV, mustHex(doc.RFC9383.ConfirmVHex)) {
		t.Fatalf("RFC 9383 confirmV mismatch:\ngot:  %x\nwant: %s", wantConfirmV, doc.RFC9383.ConfirmVHex)
	}
	if !bytes.Equal(proverKey, mustHex(doc.RFC9383.SharedKeyHex)) {
		t.Fatalf("RFC 9383 sharedKey mismatch:\ngot:  %x\nwant: %s", proverKey, doc.RFC9383.SharedKeyHex)
	}

	gotConfirmV, verifierKey, err := vs.Finish([]byte(doc.RFC9383.Context), shareP, confirmP)
	if err != nil {
		t.Fatalf("RFC 9383 verifier Finish failed: %v", err)
	}
	if !bytes.Equal(gotConfirmV, mustHex(doc.RFC9383.ConfirmVHex)) || !bytes.Equal(verifierKey, mustHex(doc.RFC9383.SharedKeyHex)) {
		t.Fatal("RFC 9383 verifier Finish key mismatch")
	}

	// Verify V7 SPAKE2+ KDF vector parity (shared with TypeScript).
	creds, err := DeriveSPAKE2PProverCreds(doc.V7KDF.Password, doc.V7KDF.NodeID, doc.V7KDF.AgentPKB64)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(creds.W0[:]) != doc.V7KDF.W0Hex || hex.EncodeToString(creds.W1[:]) != doc.V7KDF.W1Hex {
		t.Fatalf("V7 KDF mismatch: w0=%x w1=%x", creds.W0, creds.W1)
	}
}

func TestAgentSecurityRegressions(t *testing.T) {
	id, err := GenerateEphemeralIdentity()
	if err != nil {
		t.Fatal(err)
	}
	pol, err := proxy.NewDestinationPolicy("http://127.0.0.1:8080", "")
	if err != nil {
		t.Fatal(err)
	}
	hp, err := proxy.NewHTTPProxy(proxy.HTTPProxyOptions{Policy: pol})
	if err != nil {
		t.Fatal(err)
	}

	// P1-09: Contradictory AccessSecret + AllowPublic MUST fail closed at startup!
	if _, err := New(Config{
		NodeID:       "nas",
		AccessSecret: "hunter2",
		AllowPublic:  true,
		Identity:     id,
		HTTPProxy:    hp,
	}); err == nil {
		t.Fatal("P1-09 regression: Agent allowed AccessSecret != '' together with AllowPublic == true")
	}

	// P1-11: mDNS Fail-Closed outbound ICE candidate scrubber drops raw host IPs and scrubs TURN/STUN raddr.
	if _, ok := sanitizeOutboundICECandidate("candidate:1 1 udp 2122260223 192.168.1.42 54321 typ host", true); ok {
		t.Fatal("P1-11 regression: raw IPv4 typ host candidate was not dropped when ICEMDNS=true")
	}
	if _, ok := sanitizeOutboundICECandidate("candidate:2 1 udp 2122260223 fd00::1234 54321 typ host", true); ok {
		t.Fatal("P1-11 regression: raw IPv6 typ host candidate was not dropped when ICEMDNS=true")
	}
	if c, ok := sanitizeOutboundICECandidate("candidate:3 1 udp 2122260223 abcdef01-2345.local 54321 typ host", true); !ok || !strings.Contains(c, ".local") {
		t.Fatalf("valid .local mDNS host candidate rejected: ok=%v cand=%q", ok, c)
	}
	scrubbedRelay, ok := sanitizeOutboundICECandidate(
		"candidate:4 1 udp 16777295 203.0.113.10 49152 typ relay raddr 10.244.1.7 rport 51234 generation 0",
		true,
	)
	if !ok || strings.Contains(scrubbedRelay, "10.244.1.7") || strings.Contains(scrubbedRelay, "51234") || !strings.Contains(scrubbedRelay, "raddr 0.0.0.0 rport 0") {
		t.Fatalf("P1-11 regression: TURN relay raddr/rport leaked internal IP: ok=%v cand=%q", ok, scrubbedRelay)
	}

	// P1-12: Global auth failure cap defeats 9-failures/IP distributed LRU eviction bypass.
	lim := NewVisitorAuthLimiter()
	now := time.Now()
	for i := 0; i < 8; i++ {
		ip := fmt.Sprintf("2001:db8:%x::1", i+1)
		for f := 0; f < 9; f++ {
			lim.RecordFailure(ip, now)
		}
	}
	if allowed, _ := lim.Allow("2001:db8:ffff::1", now); allowed {
		t.Fatal("P1-12 regression: global auth failure cap failed to block 9-failures/IP distributed flood")
	}

	// P1-12: Per-IP handshaking cap (MaxHandshakingPerIP = 4) and invalid ClientIP normalization.
	if normalizeClientIP("127.0.0.1, 10.0.0.1") != "" || normalizeClientIP("evil-spoof") != "" {
		t.Fatal("P1-12 regression: unparseable ClientIP accepted")
	}
	ag, err := New(Config{
		NodeID:      "nas",
		AllowPublic: true,
		Identity:    id,
		HTTPProxy:   hp,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()

	fp := strings.Repeat("aa", 32)
	for i := 0; i < MaxHandshakingPerIP; i++ {
		if _, err := ag.createHandshakingSession(fmt.Sprintf("sess-%d", i), "198.51.100.7", fp, "visitor-nonce-123456"); err != nil {
			t.Fatalf("unexpected rejection on handshake %d: %v", i, err)
		}
	}
	if _, err := ag.createHandshakingSession("sess-overflow", "198.51.100.7", fp, "visitor-nonce-123456"); err == nil {
		t.Fatal("P1-12 regression: single IP exceeded MaxHandshakingPerIP")
	}
	// Different IP still allowed.
	if _, err := ag.createHandshakingSession("sess-other-ip", "198.51.100.8", fp, "visitor-nonce-123456"); err != nil {
		t.Fatalf("unrelated IP blocked by another IP's handshake cap: %v", err)
	}

	// P1-13: AUTH_RESULT with 42 KiB of InitialCookies strictly fits inside negotiated max control frame!
	var bigCookies []proxy.CookieDelta
	val := strings.Repeat("v", 1000)
	for i := 0; i < 45; i++ {
		bigCookies = append(bigCookies, proxy.CookieDelta{
			Op: "set", Name: fmt.Sprintf("c%02d", i), Value: val, Path: "/",
		})
	}
	for _, limit := range []int{protocol.MinChunkSize, protocol.DefaultChunkSize, protocol.MaxControlJSONSize} {
		raw := marshalBoundedAuthResult(limit, "owner", "tok123", "", BackendMeta{
			NodeID: "nas", BackendOrigin: "http://127.0.0.1:8080",
		}, bigCookies)
		if len(raw) > limit {
			t.Fatalf("P1-13 regression: AUTH_RESULT (%d bytes) exceeded negotiated limit %d", len(raw), limit)
		}
	}

	// P1-14: Cross-process CapabilityStore merge preserves monotonic revocations & exposes CLI-minted records.
	stateFile := filepath.Join(t.TempDir(), "cap.state.json")
	liveStore, err := NewCapabilityStoreWithFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	cliStore, err := NewCapabilityStoreWithFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour).Unix()
	p1 := CapabilityPayload{V: CapabilityVersion, JTI: "cap-old", Node: "nas", Exp: exp}
	p2 := CapabilityPayload{V: CapabilityVersion, JTI: "cap-cli-new", Node: "nas", Exp: exp}
	if err := liveStore.Track(p1); err != nil {
		t.Fatal(err)
	}
	// Live agent revokes cap-old.
	if err := liveStore.Revoke("cap-old"); err != nil {
		t.Fatal(err)
	}
	// Stale CLI process mints cap-cli-new into the same state file: MUST NOT resurrect cap-old!
	if err := cliStore.Track(p2); err != nil {
		t.Fatal(err)
	}
	// Reload in a fresh store AND check liveStore sees cap-cli-new without losing cap-old revocation!
	if !liveStore.IsRevoked("cap-old") {
		t.Fatal("P1-14 regression: cap-old lost revocation in liveStore")
	}
	items, _, _ := liveStore.ListPage(time.Now(), 0, 50)
	sawNew := false
	for _, it := range items {
		if it.Payload.JTI == "cap-cli-new" {
			sawNew = true
		}
	}
	if !sawNew {
		t.Fatal("P1-14 regression: liveStore did not reload CLI-minted cap-cli-new")
	}
	freshStore, err := NewCapabilityStoreWithFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if !freshStore.IsRevoked("cap-old") {
		t.Fatal("P1-14 regression: CLI -mint-link resurrected revoked cap-old on disk!")
	}

	// P2-09: CAP_RESP list dynamically packs to small negotiated chunk size + MintCapability validation.
	var manyItems []CapabilityRecord
	for i := 0; i < 100; i++ {
		manyItems = append(manyItems, CapabilityRecord{
			Payload: CapabilityPayload{
				V: CapabilityVersion, JTI: fmt.Sprintf("cap-%03d", i), Node: "nas",
				Exp: exp, PathPrefix: "/very/long/path/prefix/" + strings.Repeat("x", 100),
			},
		})
	}
	packed, packedHasMore := fitCapListPage(protocol.MinChunkSize, "req-1", manyItems, 100, 0, 100, false)
	if !packedHasMore || len(packed) == 0 || len(packed) >= len(manyItems) {
		t.Fatalf("P2-09 regression: fitCapListPage failed to pack for 4088B limit: len=%d hasMore=%v", len(packed), packedHasMore)
	}
	if _, err := id.MintCapability(CapabilityPayload{JTI: "ok", Node: "nas", Exp: exp, MaxConns: -1}); err == nil {
		t.Fatal("P2-09 regression: MintCapability accepted negative MaxConns")
	}
	if _, err := id.MintCapability(CapabilityPayload{JTI: "ok", Node: "nas", Exp: exp, PathPrefix: strings.Repeat("a", 600)}); err == nil {
		t.Fatal("P2-09 regression: MintCapability accepted oversized PathPrefix")
	}

	// P2-12: RFC 8841 a=max-message-size:0 -> 262144; 1..4095 -> error!
	mkSDP := func(mms string) string {
		return strings.Join([]string{
			"v=0",
			"o=- 1 2 IN IP4 127.0.0.1",
			"s=-",
			"t=0 0",
			"m=application 9 UDP/DTLS/SCTP webrtc-datachannel",
			"c=IN IP4 0.0.0.0",
			"a=ice-ufrag:ufrag1234",
			"a=ice-pwd:passwordpasswordpassword1234",
			"a=fingerprint:sha-256 " + FormatColonFingerprint(fp),
			"a=setup:actpass",
			"a=mid:0",
			"a=sctp-port:5000",
			"a=max-message-size:" + mms,
			"",
		}, "\r\n")
	}
	d0, err := ExtractSessionDescriptor(mkSDP("0"), 2)
	if err != nil || d0.MaxMsgSize != 262144 {
		t.Fatalf("P2-12 regression: max-message-size:0 expected 262144, got %+v err=%v", d0, err)
	}
	if _, err := ExtractSessionDescriptor(mkSDP("1024"), 2); err == nil {
		t.Fatal("P2-12 regression: max-message-size:1024 below 4096 must fail closed")
	}
}
